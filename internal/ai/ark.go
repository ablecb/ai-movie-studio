package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 火山方舟（Ark）适配层：出图走 Seedream、出视频走 Seedance。
//
// 端点判定：base_url 含 volces.com / ark. 即走本文件；其余（智谱等）走
// media2.go 里的原生路径。两家的差异（均按官方文档核对）：
//
//	出图（Seedream，POST /api/v3/images/generations）
//	  - 去水印字段是 watermark（官方默认 true："AI 生成"角标必须显式写 false 关掉）
//	  - 参考图是 image（单图字符串 / 多图数组；没有 ref_strength 概念）
//	  - 没有 quality 参数；size 支持档位词（1K/1.5K/2K/4K）或 "宽x高"
//	    （总像素 ∈ [921600, 16777216]，宽高比 ∈ [1/16, 16]）
//
//	出视频（Seedance，POST /api/v3/contents/generations/tasks，异步任务 + 轮询）
//	  - 参数是**顶层 JSON 字段**（ratio/resolution/duration/watermark/camera_fixed）；
//	    把 --ratio 之类写进提示词文本是另一套 CLI/节点接口，不能用于 REST API
//	  - content 数组：首帧 = 无 role 的 image_url；尾帧 = role=last_frame；
//	    参考图 = role=reference_image（与首/尾帧**不可混用**）
//	  - 任务状态 queued/running/succeeded/failed，结果在 content.video_url
// ---------------------------------------------------------------------------

// arkMinPixels Seedream 自定义宽高的总像素下限（低于它服务端 400）。
const arkMinPixels = 921600

// arkMaxSide 单边像素上限（4K 档）。
const arkMaxSide = 4096

// arkPollInterval 任务轮询间隔（包级变量：测试里可缩短）。
var arkPollInterval = 10 * time.Second

// isArkEndpoint base_url 是否指向火山方舟。
func isArkEndpoint(baseURL string) bool {
	low := strings.ToLower(baseURL)
	return strings.Contains(low, "volces.com") || strings.Contains(low, "ark.")
}

// arkAPIPath 拼出发送给方舟的完整路径。
//
// 方舟文档里的 base_url 是 https://ark.cn-beijing.volces.com/api/v3（与智谱
// base_url 含 /api/paas/v4 同构）。这里对两种写法都兼容：base 已含 /api/v3 用
// 相对路径，否则补上——漏了前缀会打到网关根路径，只得到一个"空响应体"404，
// 排查成本极高（真实踩过）。
func arkAPIPath(baseURL, path string) string {
	if strings.Contains(strings.ToLower(baseURL), "/api/v3") {
		return path
	}
	return "/api/v3" + path
}

// arkTaskResp Seedance 任务提交/查询的响应（同一结构：提交只回 id/status）。
type arkTaskResp struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // queued | running | succeeded | failed
	Content struct {
		VideoURL     string `json:"video_url"`
		LastFrameURL string `json:"last_frame_url"`
	} `json:"content"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// 出图：Seedream
// ---------------------------------------------------------------------------

// arkImageBody Seedream 出图请求体。
func arkImageBody(prompt string, opt ImageOptions, refImage, model string) []byte {
	body := map[string]any{
		"model":  model,
		"prompt": prompt,
		// 显式要 URL（默认也是 url；写清楚便于排障）
		"response_format": "url",
		// 去水印必须显式写 false：官方默认 true（右下角"AI 生成"角标）
		"watermark": opt.Watermark,
	}
	if opt.Size != "" {
		body["size"] = arkImageSize(opt.Size)
	}
	if refImage != "" {
		body["image"] = refImage
	}
	data, _ := json.Marshal(body)
	return data
}

// arkImageSize 把请求尺寸翻译成 Seedream 合法尺寸：
// 档位词原样返回；"宽x高" 满足官方约束（总像素下限 + 单边上限 + 合法比例）时原样使用，
// 否则等比放大到下限之上——1248x704（< 92 万像素）这类 glm-image 专用尺寸
// 在 Seedream 上会 400，必须先翻译再发。
func arkImageSize(size string) string {
	s := strings.TrimSpace(size)
	if s == "" {
		return ""
	}
	switch strings.ToUpper(s) {
	case "1K", "1.5K", "2K", "4K":
		return strings.ToUpper(s)
	}
	w, h, ok := arkParseSize(s)
	if !ok {
		return s // 认不出来的形态原样发给服务端，让它给出明确报错
	}
	for w*h < arkMinPixels || w > arkMaxSide || h > arkMaxSide {
		if w*h < arkMinPixels {
			w, h = w*2, h*2
			continue
		}
		w, h = w/2, h/2 // 超过单边上限：等比缩回
	}
	w -= w % 2 // 偶数对齐（奇数边在部分档位上会 400）
	h -= h % 2
	return fmt.Sprintf("%dx%d", w, h)
}

// arkParseSize 解析 "宽x高"（大小写不敏感，允许两边空格）。
func arkParseSize(size string) (int, int, bool) {
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(size)), func(r rune) bool {
		return r == 'x' || r == '*' || r == '×'
	})
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// ---------------------------------------------------------------------------
// 出视频：Seedance
// ---------------------------------------------------------------------------

//	isSeedance2x Seedance 2.x 系模型（任务类型与参数集与 1.x 不同：
//	generate_audio、omni_reference_task_type、first_frame role、ratio=adaptive 约束）。
func isSeedance2x(model string) bool {
	return strings.Contains(strings.ToLower(model), "seedance-2")
}

// arkVideoTaskBody Seedance 视频任务请求体（POST /api/v3/contents/generations/tasks）。
//
// Seedance 的任务类型由 content 里 role 的组合决定（官方二分）：
//   - 首帧/首尾帧任务：role = first_frame（2.x 显式标，1.x 是"无 role = 首帧"）+ last_frame
//   - 全模态参考任务：role = reference_image（与首/尾帧**不可混用**）
func arkVideoTaskBody(prompt, imageRef string, opt VideoOptions, model string) []byte {
	content := []map[string]any{{"type": "text", "text": prompt}}
	switch {
	case imageRef == "" && len(opt.RefImages) > 0:
		// 参考图生视频（全模态参考路径）：只发参考图，不发首/尾帧——
		// 官方约束：reference_image 与 first/last frame 不可混用
		for _, r := range opt.RefImages {
			content = append(content, arkImageItem(r, "reference_image"))
		}
	case imageRef != "":
		firstRole := ""
		if isSeedance2x(model) {
			// 2.x 官方语义：首帧必须显式标 role=first_frame（1.x 才是"无 role = 首帧"）
			firstRole = "first_frame"
		}
		content = append(content, arkImageItem(imageRef, firstRole))
		if opt.EndImage != "" {
			content = append(content, arkImageItem(opt.EndImage, "last_frame"))
		}
	}
	body := map[string]any{
		"model":   model,
		"content": content,
		// 默认带"AI 生成"水印：交付物必须显式关掉
		"watermark": opt.Watermark,
	}
	if opt.Duration > 0 {
		body["duration"] = opt.Duration
	}
	if res := arkVideoResolution(opt.Size); res != "" {
		body["resolution"] = res
	}
	if ratio := arkVideoRatio(opt.AspectRatio, opt.Size); ratio != "" {
		body["ratio"] = ratio
	}
	// 2.x 首帧/首尾帧任务官方约束：ratio 必须为 adaptive（模型自动跟随首帧画幅），
	// 显式画幅会触发 InvalidParameter.TaskTypeConstraint 异步报错
	if imageRef != "" && isSeedance2x(model) {
		body["ratio"] = "adaptive"
	}
	// 多模态参考任务显式声明任务类型为 reference：不写的话模型按提示词意图自判，
	// 可能误判成编辑/延长（那两类对 ratio/duration 有硬约束，会异步报错）；
	// 显式声明还会让服务端把参数校验前置成同步报错，不白等一轮任务排队。
	// 1.x 没有该字段（未知字段 400），只在 2.x 上发。
	if imageRef == "" && len(opt.RefImages) > 0 && isSeedance2x(model) {
		body["omni_reference_task_type"] = "reference"
	}
	// r2v（全模态参考）任务不支持 camera_fixed：方舟同步报 400
	// "the specified parameter camera_fixed is not supported for model
	// doubao-seedance-2-0 in r2v, must be empty"——该任务类型下运镜意图
	// 只能靠提示词的 Camera 行表达；首帧/首尾帧任务保留该开关。
	isR2V := imageRef == "" && len(opt.RefImages) > 0
	if opt.CameraFixed && !(isR2V && isSeedance2x(model)) {
		// 运镜意图服从分镜：定点镜必须真的钉住机位（Seedance 有该开关）
		body["camera_fixed"] = true
	}
	// generate_audio 只有 2.x 系种子模型认（1.x 收到未知字段会 400）；
	// 本项目自己做后期声音，默认关掉以免为用不上的音轨付费
	if isSeedance2x(model) {
		body["generate_audio"] = opt.WithAudio
	}
	data, _ := json.Marshal(body)
	return data
}

// arkImageItem 组装一个 image_url content 项；role 为空表示首帧（官方语义）。
func arkImageItem(url, role string) map[string]any {
	item := map[string]any{
		"type":      "image_url",
		"image_url": map[string]string{"url": url},
	}
	if role != "" {
		item["role"] = role
	}
	return item
}

// arkVideoResolution 从请求分辨率推导 Seedance 的 resolution 档位（480p/720p/1080p）。
func arkVideoResolution(size string) string {
	_, h, ok := arkParseSize(size)
	if !ok {
		return ""
	}
	switch {
	case h >= 1080:
		return "1080p"
	case h >= 720:
		return "720p"
	default:
		return "480p"
	}
}

// arkVideoRatios Seedance 支持的画幅枚举。
var arkVideoRatios = map[string]bool{
	"16:9": true, "4:3": true, "1:1": true, "3:4": true, "9:16": true, "21:9": true,
	"adaptive": true, "keep_ratio": true,
}

// arkVideoRatio 推导画幅：显式 AspectRatio（vidu 路径会带）优先，
// 否则从生成尺寸换算（1920x1080 → 16:9）；都认不出就不传——
// 图生视频的默认 adaptive 会跟随首帧比例，恰好是本管线的交付画幅。
func arkVideoRatio(aspectRatio, size string) string {
	if ar := strings.TrimSpace(strings.ToLower(aspectRatio)); ar != "" && arkVideoRatios[ar] {
		return ar
	}
	w, h, ok := arkParseSize(size)
	if !ok {
		return ""
	}
	r := float64(w) / float64(h)
	for _, c := range []struct {
		name string
		v    float64
	}{
		{"21:9", 21.0 / 9}, {"16:9", 16.0 / 9}, {"4:3", 4.0 / 3},
		{"1:1", 1}, {"3:4", 0.75}, {"9:16", 9.0 / 16},
	} {
		if math.Abs(r-c.v)/c.v <= 0.03 {
			return c.name
		}
	}
	return ""
}

// arkModelHint 给"模型不存在/未开通"的方舟报错补一条可执行提示。
//
// 真实踩过的坑：API Key 格式合法但账号未在方舟控制台开通模型时，服务端把
// "未开通/无权限"和"模型名写错"合并成同一句 InvalidEndpointOrModel.NotFound——
// 光看英文报错会以为是代码或模型名的问题，其实是控制台里少了一步开通。
func arkModelHint(err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	// 兼容两种呈现：错误码（InvalidEndpointOrModel.NotFound）或只有 message
	// 文本（客户端只透传 message 时会落到这一支）
	if strings.Contains(low, "invalidendpointormodel") ||
		strings.Contains(low, "does not exist or you do not have access") {
		return fmt.Errorf("%w\n  提示：该模型未开通或无权访问——请在火山方舟控制台「开通管理」开通对应模型，"+
			"或改用推理接入点 ID（形如 ep-xxxxxxxx）填入 config 的模型字段", err)
	}
	return err
}

// arkVideoModelAttempt 提交 Seedance 任务并轮询到完成。
func (c *Client) arkVideoModelAttempt(ctx context.Context, prompt, imageRef string, opt VideoOptions, m string) (string, error) {
	body := arkVideoTaskBody(prompt, imageRef, opt, m)
	data, err := c.postCtx(ctx, arkAPIPath(c.cfg.BaseURL, "/contents/generations/tasks"), body)
	if err != nil {
		return "", arkModelHint(err)
	}
	var tr arkTaskResp
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", fmt.Errorf("解析视频任务响应失败: %w", err)
	}
	if tr.ID == "" {
		return "", fmt.Errorf("视频任务提交失败: %s", truncateForLog(data))
	}
	return c.arkVideoWait(ctx, tr.ID, opt.Timeout, nil)
}

// arkVideoWait 轮询 Seedance 任务直至成功/失败（GET /api/v3/contents/generations/tasks/{id}）。
func (c *Client) arkVideoWait(ctx context.Context, taskID string, timeout time.Duration, logf func(string, ...any)) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", &APIError{Kind: KindTimeout, Op: "contents.tasks", Message: "批次已取消"}
		case <-time.After(arkPollInterval):
		}
		data, err := c.getCtx(ctx, arkAPIPath(c.cfg.BaseURL, "/contents/generations/tasks/"+taskID))
		if err != nil {
			// 轮询期间的网络抖动不应直接判失败（任务仍在服务端跑），累计若干次才放弃
			if isTransient(err) {
				continue
			}
			return "", err
		}
		var tr arkTaskResp
		if err := json.Unmarshal(data, &tr); err != nil {
			return "", fmt.Errorf("解析任务状态失败: %w", err)
		}
		switch strings.ToLower(tr.Status) {
		case "succeeded":
			if tr.Content.VideoURL == "" {
				return "", &APIError{Kind: KindUnknown, Op: "contents.tasks", Message: "任务成功但无视频 URL"}
			}
			return tr.Content.VideoURL, nil
		case "failed", "cancelled", "expired":
			msg := tr.Status
			if tr.Error != nil && tr.Error.Message != "" {
				msg = tr.Error.Message
			}
			return "", &APIError{Kind: KindUnknown, Op: "contents.tasks", Message: "视频生成失败: " + msg}
		}
		if logf != nil {
			logf("      …生成中（%s/%s）", taskID, tr.Status)
		}
	}
	return "", &APIError{Kind: KindTimeout, Op: "contents.tasks",
		Message: fmt.Sprintf("视频任务超时（%v）：Seedance 排队过久，可加大 video.timeout_minutes 后重跑", timeout)}
}
