package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Capability 能力标识：不同能力可以接不同的服务商。
//
// 典型组合：出图/视频/TTS 用智谱（CogView/CogVideoX/CogTTS），
// 而视觉复核用 DeepSeek 的 deepseek-flash（它支持图片输入）。
type Capability string

const (
	CapText      Capability = "text"
	CapVision    Capability = "vision"
	CapImage     Capability = "image"
	CapVideo     Capability = "video"
	CapTTS       Capability = "tts"
	CapTranslate Capability = "translate"
	CapImageEdit Capability = "image_edit" // 多参考图生成（Seedream/Qwen-Image-Edit 等）
	CapMusic     Capability = "music"      // 纯音乐生成（MiniMax Music 等）
)

// Endpoint 一个能力的接入点。
type Endpoint struct {
	BaseURL   string
	APIKeyEnv string
	// APIKey 直接给定的密钥（本地层）。env 名存在但未配到时作为兜底。
	APIKey string
	Model  string
}

// Key 解析该端点的密钥：环境变量优先，其次直接给定的密钥。
func (e Endpoint) Key() string {
	if e.APIKeyEnv != "" {
		if v := os.Getenv(e.APIKeyEnv); v != "" {
			return v
		}
	}
	return strings.TrimSpace(e.APIKey)
}

// HasKey 该端点是否具备真实调用条件。
func (e Endpoint) HasKey() bool { return e.Key() != "" }

// For 派生一个面向指定能力的客户端。
//
// 关键设计：**共享同一个花费账本**。视觉复核若开独立账本，
// 预算熔断和成本报告就会漏掉它，等于给自己留了个不受控的付费出口。
func (c *Client) For(cap Capability, ep Endpoint) *Client {
	nc := &Client{
		cfg:       c.cfg,
		http:      c.http,
		shared:    c.shared, // 共享可变状态：usage/预留/批次上下文必须全局一致
		ledger:    c.ledger, // 共用账本：预算与成本报告必须看到全部花费
		prices:    c.prices,
		budget:    c.budget,
		warnRatio: c.warnRatio,
	}
	if ep.BaseURL != "" {
		nc.cfg.BaseURL = strings.TrimRight(ep.BaseURL, "/")
	}
	if ep.APIKeyEnv != "" {
		nc.cfg.APIKeyEnv = ep.APIKeyEnv
	}
	if ep.APIKey != "" {
		nc.cfg.APIKey = ep.APIKey
	}
	if ep.Model != "" {
		nc.cfg.Model = ep.Model
	}
	// 该能力没有可用密钥时退回离线模式，避免拿别人的密钥去请求另一个服务
	if !nc.cfg.HasKey() {
		nc.cfg.Mock = true
	}
	return nc
}

// ImageRef 一张待送检的图片：可以给本地路径，也可以直接给已编码的 data URL
// （内存帧不必先落盘再读回，省一次 I/O）。
type ImageRef struct {
	Path    string
	DataURL string
}

// VisionJSON 便捷包装：从本地文件路径送检（兼容旧签名）。
func (c *Client) VisionJSON(system, prompt string, imagePaths []string, out any) error {
	refs := make([]ImageRef, 0, len(imagePaths))
	for _, p := range imagePaths {
		refs = append(refs, ImageRef{Path: p})
	}
	return c.VisionJSONEx(system, prompt, refs, "original", out)
}

// EndpointInfo 返回客户端当前面向的接入点信息（用于日志与排障）。
func (c *Client) EndpointInfo() string {
	hasKey := "无密钥"
	if c.cfg.HasKey() {
		hasKey = "已配置密钥"
	}
	return fmt.Sprintf("%s | 模型 %s | %s", c.cfg.BaseURL, c.cfg.Model, hasKey)
}

// MockFor 判断某能力是否处于离线模式（该能力的密钥是否就绪）。
func (c *Client) MockFor(cap Capability, ep Endpoint) bool {
	if c.cfg.Mock && ep.APIKeyEnv == "" {
		return true
	}
	if ep.APIKeyEnv == "" {
		return !c.cfg.HasKey()
	}
	return !ep.HasKey()
}

// chatRequestV 视觉请求（content 为块数组）。
type chatRequestVision struct {
	Model       string          `json:"model"`
	Messages    []visionMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
	MaxTokens   int             `json:"max_tokens"`
}

type visionMessage struct {
	Role    string          `json:"role"`
	Content []visionContent `json:"content"`
}

type visionContent struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *visionImage `json:"image_url,omitempty"`
}

type visionImage struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// VisionJSONEx 视觉复核的完整实现：支持独立的视觉端点/模型、图片细节级别、
// 多图批量送检，并把花费记入共享账本。
//
// 与各家视觉接口的兼容性（已按官方文档核对）：
//   - OpenAI 兼容：POST {base_url}/chat/completions，content 为块数组
//   - 图片只能出现在 user 消息中（system 带图会 400）
//   - base64 用 data URL：data:image/png;base64,...
//   - DeepSeek：模型名 deepseek-flash（v4-pro 不支持视觉）；单请求最多 600 图、
//     总 64MiB，单图自动缩放到约 1300×1300 且每图最多 1024 token
func (c *Client) VisionJSONEx(system, prompt string, images []ImageRef, detail string, out any) error {
	if c.cfg.Mock {
		return mockVision(prompt, out)
	}
	if len(images) == 0 {
		return fmt.Errorf("视觉复核需要至少一张图片")
	}
	if err := c.guardBudget("vision"); err != nil {
		return err
	}
	if detail == "" {
		detail = "auto"
	}
	content := []visionContent{{Type: "text", Text: prompt}}
	for _, img := range images {
		url := img.DataURL
		if url == "" {
			var err error
			url, err = c.imageRef(img.Path)
			if err != nil {
				return fmt.Errorf("读取复核图片失败 (%s): %w", shortName(img.Path), err)
			}
		}
		content = append(content, visionContent{Type: "image_url", ImageURL: &visionImage{URL: url, Detail: detail}})
	}
	body, err := json.Marshal(chatRequestVision{
		Model: c.cfg.Model,
		Messages: []visionMessage{
			// system 只放文字指令：带图会返回 400
			{Role: "system", Content: []visionContent{{Type: "text", Text: system}}},
			{Role: "user", Content: content},
		},
		Temperature: 0.1, // 复核要稳定判断，不要创意
		MaxTokens:   4096,
	})
	if err != nil {
		return err
	}
	data, err := c.post("/chat/completions", body)
	if err != nil {
		return err
	}
	var resp chatResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("解析视觉响应失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("视觉模型返回空结果（图片可能被内容审核拦截）")
	}
	c.recordUsage("vision", resp.Usage)
	c.recordTokens(CostVision, c.priceOf(CostVision), resp.Usage)
	raw, err := ExtractJSON(resp.Choices[0].Message.Content)
	if err != nil {
		return fmt.Errorf("视觉复核输出无法解析为 JSON: %w", err)
	}
	return json.Unmarshal(raw, out)
}

// Ping 轻量连通性探测：用一张 1x1 图片验证该端点/模型是否真的支持视觉。
//
// 为什么需要：选错模型（例如把不支持视觉的模型配到 vision）会在真正复核时
// 才报错，那时渲染已经花完钱了。开工前探一次，成本可忽略。
func (c *Client) Ping() error {
	if c.cfg.Mock {
		return fmt.Errorf("离线模式：未配置该能力的密钥（环境变量 %s）", c.cfg.APIKeyEnv)
	}
	// 1x1 透明 PNG
	const px = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	body, err := json.Marshal(chatRequestVision{
		Model: c.cfg.Model,
		Messages: []visionMessage{{Role: "user", Content: []visionContent{
			{Type: "text", Text: "reply with the single word: ok"},
			{Type: "image_url", ImageURL: &visionImage{URL: px, Detail: "low"}},
		}}},
		Temperature: 0,
		MaxTokens:   16,
	})
	if err != nil {
		return err
	}
	if _, err := c.post("/chat/completions", body); err != nil {
		return fmt.Errorf("视觉能力探测失败（端点 %s / 模型 %s）：%w", c.cfg.BaseURL, c.cfg.Model, err)
	}
	return nil
}

func shortName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
