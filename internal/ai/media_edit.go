package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// 多参考图生成（角色一致性的终局手段）
//
// 单张参考帧 + 图生图只能锁"一张脸"；两人同框时第二个人没有图像级约束。
// 业界解法是把多张角色参考帧一起喂给支持多图参考的图像编辑模型：
//   - Seedream 4.0（火山方舟 Ark）：image 字段接受 URL/base64 数组，组合一致性
//   - Qwen-Image-Edit（阿里百炼 DashScope）：message content 交替 image/text
//
// 端点按 base_url 关键字路由，两家 schema 不同分别组装；
// 解析对响应结构做宽容处理（正则兜底），供应商微调字段也不至于直接失败。
// ---------------------------------------------------------------------------

type arkImageResp struct {
	Data []struct {
		URL string `json:"url"`
	} `json:"data"`
	Error *apiError `json:"error"`
}

type dashScopeImageResp struct {
	Output struct {
		Choices []struct {
			Message struct {
				Content []struct {
					Image string `json:"image"`
				} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	} `json:"output"`
}

var reAnyImageURL = regexp.MustCompile(`"(?:url|image)"\s*:\s*"(https?://[^"]+)"`)

// ImageEditSupported 当前端点是否配置了受支持的多参考图服务。
func ImageEditSupported(baseURL string) bool {
	low := strings.ToLower(baseURL)
	return strings.Contains(low, "volces.com") || strings.Contains(low, "ark.") ||
		strings.Contains(low, "dashscope")
}

// ImageEditMulti 多参考图生成：refs 为参考图（URL 或 data URL），返回图片 URL 与实际模型。
//
// 路由规则：
//   - base_url 含 volces.com/ark → 火山方舟 Seedream（POST /api/v3/images/generations）
//   - base_url 含 dashscope     → 阿里百炼 Qwen-Image-Edit（POST /api/v1/services/aigc/multimodal-generation/generation）
//
// 调用方负责在失败时降级回单参考帧/纯文生图路径——该能力是增强，不是依赖。
func (c *Client) ImageEditMulti(prompt string, refs []string, opt ImageOptions) (string, string, error) {
	if c.cfg.Mock {
		return "mock://image", opt.Model, nil
	}
	if len(refs) == 0 {
		return "", "", fmt.Errorf("多参考图生成需要至少一张参考图")
	}
	if len(refs) > 4 {
		refs = refs[:4] // Seedream 上限 10、Qwen-Edit 2509 为 3-4；取保守值
	}
	model := opt.Model
	if model == "" {
		model = c.cfg.Model
	}
	low := strings.ToLower(c.cfg.BaseURL)
	// 预算预留包住整次编辑调用（与出图同价口径）
	if err := c.reserve("images.edit", c.estImageCost()); err != nil {
		return "", model, err
	}
	var data []byte
	var err error
	if strings.Contains(low, "dashscope") {
		data, err = c.imageEditDashScope(prompt, refs, model)
	} else {
		data, err = c.imageEditArk(prompt, refs, opt.Size, model)
	}
	c.settle(c.estImageCost())
	if err != nil {
		return "", model, err
	}
	if url := parseEditImageURL(data); url != "" {
		c.chargeImage()
		return url, model, nil
	}
	return "", model, fmt.Errorf("多参考图生成返回空结果: %s", truncateForLog(data))
}

// imageEditArk 火山方舟 Seedream：image 字段为数组，size 用档位词（1K/2K/4K）。
func (c *Client) imageEditArk(prompt string, refs []string, sizeHint, model string) ([]byte, error) {
	size := "2K"
	if s := parseEditSize(sizeHint); s != "" {
		size = s
	}
	body := mustJSON(map[string]any{
		"model":           model,
		"prompt":          prompt,
		"image":           refs,
		"response_format": "url",
		"size":            size,
		"watermark":       false,
	})
	return c.post("/api/v3/images/generations", body)
}

// imageEditDashScope 百炼 Qwen-Image-Edit：content 交替 image/text 的多模态消息。
func (c *Client) imageEditDashScope(prompt string, refs []string, model string) ([]byte, error) {
	content := make([]map[string]string, 0, len(refs)+1)
	for _, r := range refs {
		content = append(content, map[string]string{"image": r})
	}
	content = append(content, map[string]string{"text": prompt})
	body := mustJSON(map[string]any{
		"model": model,
		"input": map[string]any{
			"messages": []map[string]any{
				{"role": "user", "content": content},
			},
		},
		"parameters": map[string]any{"watermark": false},
	})
	return c.post("/api/v1/services/aigc/multimodal-generation/generation", body)
}

// parseEditImageURL 宽容解析：先按两家已知结构取，失败则正则兜底。
func parseEditImageURL(data []byte) string {
	var ark arkImageResp
	if json.Unmarshal(data, &ark) == nil && len(ark.Data) > 0 && ark.Data[0].URL != "" {
		return ark.Data[0].URL
	}
	var ds dashScopeImageResp
	if json.Unmarshal(data, &ds) == nil && len(ds.Output.Choices) > 0 {
		for _, c := range ds.Output.Choices[0].Message.Content {
			if c.Image != "" {
				return c.Image
			}
		}
	}
	if m := reAnyImageURL.FindSubmatch(data); len(m) >= 2 {
		return string(m[1])
	}
	return ""
}

// parseEditSize 从分辨率字符串推导 Seedream 档位（1920x1080 → 2K）。
// Ark 的 size 支持档位词或具体 "宽x高"，档位词最稳。
func parseEditSize(hint string) string {
	switch {
	case strings.Contains(hint, "3840"), strings.Contains(hint, "2160"):
		return "4K"
	case strings.Contains(hint, "1920"), strings.Contains(hint, "1080"), strings.Contains(hint, "2048"):
		return "2K"
	default:
		return "1K"
	}
}
