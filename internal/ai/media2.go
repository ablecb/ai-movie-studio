package ai

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 异步任务响应（智谱 /async-result）
// ---------------------------------------------------------------------------

type taskResp struct {
	ID          string `json:"id"`
	TaskStatus  string `json:"task_status"` // PROCESSING | SUCCESS | FAIL
	VideoResult []struct {
		URL           string `json:"url"`
		CoverImageURL string `json:"cover_image_url"`
	} `json:"video_result"`
	Msg string `json:"msg"`
}

// ---------------------------------------------------------------------------
// 出图（含参考图能力 —— 角色跨镜头一致性的技术基础）
// ---------------------------------------------------------------------------

// ImageOptions 出图参数。
type ImageOptions struct {
	Model string
	Size  string
	// Quality 出图质量档（hd | standard）；空则沿用服务端默认。
	Quality string
	// Watermark 是否在图上保留平台水印。
	//
	// false 表示"不要水印"：请求里会**显式**写 watermark_enabled=false。
	// 不能靠"不传"来关——官方 spec 里该字段默认 true（显式水印 + 隐式数字水印）。
	Watermark bool
	// RefImage 参考图（base64 或 URL）。传了就进入图生图/参考图模式——
	// 这是"同一张脸"能跨镜头保持的唯一可靠手段，纯文字描述做不到。
	RefImage string
	// RefStrength 参考强度（部分接口支持）；0 表示不传。
	RefStrength float64
}

// imageBody 组装出图请求。
//
// 用 map 而不是结构体，是为了让 watermark_enabled 只在"确实要求去水印"时出现：
// 结构体加 omitempty 会在要水印时静默丢掉字段（bool 零值不可区分"未设置"）。
func imageBody(prompt string, opt ImageOptions, refImage, model string) []byte {
	body := map[string]any{"model": model, "prompt": prompt}
	if opt.Size != "" {
		body["size"] = opt.Size
	}
	if opt.Quality != "" {
		body["quality"] = opt.Quality
	}
	if refImage != "" {
		body["image"] = refImage
		if opt.RefStrength > 0 {
			body["ref_strength"] = opt.RefStrength
		}
	}
	if !opt.Watermark {
		body["watermark_enabled"] = false
	}
	data, _ := json.Marshal(body)
	return data
}

type imageResp struct {
	Data []struct {
		URL string `json:"url"`
	} `json:"data"`
	Error *apiError `json:"error"`
}

// ImageGenEx 文生图/参考图生图，返回图片 URL。自动按 fallback 列表降级。
func (c *Client) ImageGenEx(prompt string, opt ImageOptions, fallbacks []string) (string, string, error) {
	if c.cfg.Mock {
		return "mock://image", opt.Model, nil
	}
	if opt.Size == "" {
		opt.Size = "1280x720"
	}
	models := withFallback(opt.Model, fallbacks)
	var lastErr error
	for _, m := range models {
		body := imageBody(prompt, opt, opt.RefImage, m)
		data, err := c.reservePost("images.generations", "/images/generations", body, c.estImageCost())
		if err != nil {
			lastErr = err
			// 不支持参考图的错误 → 去掉参考图重试同一模型（降级而非失败）
			if opt.RefImage != "" && isRefUnsupported(err) {
				body2 := imageBody(prompt, opt, "", m)
				if data2, err2 := c.reservePost("images.generations", "/images/generations", body2, c.estImageCost()); err2 == nil {
					if url := parseImageURL(data2); url != "" {
						c.chargeImage()
						return url, m, nil
					}
				}
				lastErr = fmt.Errorf("模型 %s 不支持参考图，已降级为纯提示词出图", m)
			}
			if IsFatal(err) || isModelUnavailable(err) {
				continue // 换下一个模型
			}
			continue
		}
		if url := parseImageURL(data); url != "" {
			c.chargeImage()
			return url, m, nil
		}
		lastErr = fmt.Errorf("模型 %s 返回空图像结果", m)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的出图模型")
	}
	return "", "", lastErr
}

// ExportImageBody 供测试断言"实际发出去的请求体"，无需起 HTTP 服务。
func ExportImageBody(prompt string, opt ImageOptions, refImage, model string) []byte {
	return imageBody(prompt, opt, refImage, model)
}

// ExportVideoBody 同上，用于视频请求。
func ExportVideoBody(prompt, imageRef string, opt VideoOptions, model string) []byte {
	return videoBody(prompt, imageRef, opt, model)
}

// ImageGen 兼容旧签名：文生图，固定尺寸。默认不加水印（交付物不能带角标）。
func (c *Client) ImageGen(prompt, size string) (string, error) {
	url, _, err := c.ImageGenEx(prompt, ImageOptions{Model: "cogview-4", Size: size}, nil)
	return url, err
}

// chargeImage 出图计费。
func (c *Client) chargeImage() {
	p, ok := c.priceFor(CostImage)
	if !ok {
		return
	}
	c.recordMedia(CostImage, 1, p.ImagePerShot)
}

func parseImageURL(data []byte) string {
	var ir imageResp
	if err := json.Unmarshal(data, &ir); err != nil {
		return ""
	}
	if len(ir.Data) == 0 {
		return ""
	}
	return ir.Data[0].URL
}

// isRefUnsupported 判断错误是否属于"该接口不支持参考图/图生图"。
// 必须是"参数不认识/不支持"级别的证据才允许降级：单独的 "image" 一词太宽——
// 图片接口的任何报错（尺寸、内容审核、5xx 正文）几乎都含这个词，误判的代价是
// 悄悄丢掉参考图再做一次付费调用，跨镜头角色一致性随之失效。
func isRefUnsupported(err error) bool {
	low := strings.ToLower(err.Error())
	// 参数名级命中：服务端明确不认识该参数
	if containsAny(low, "image_url", "img2img", "reference_image", "参考图") {
		return true
	}
	// 组合命中："不支持/未知参数" + 图类词
	for _, kw := range []string{"not support", "unsupported", "unknown parameter", "invalid parameter", "unexpected field"} {
		if strings.Contains(low, kw) && containsAny(low, "image", "ref") {
			return true
		}
	}
	return false
}

// isModelUnavailable 判断错误是否属于"模型不可用"，用于 fallback 判定。
func isModelUnavailable(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.Kind == KindUnsupported || ae.StatusCode == 404
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if ae, ok := err.(*APIError); ok {
			*target = ae
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func withFallback(primary string, fallbacks []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	add(primary)
	for _, f := range fallbacks {
		add(f)
	}
	if len(out) == 0 {
		out = append(out, "cogview-4")
	}
	return out
}

// ---------------------------------------------------------------------------
// 图生视频
// ---------------------------------------------------------------------------

// VideoOptions 视频生成参数。
type VideoOptions struct {
	Model   string
	Quality string // quality | speed
	Size    string
	FPS     int
	// Duration 视频时长档位（秒）。智谱只接受 5 或 10；0 表示不传（服务端默认 5）。
	Duration  int
	Timeout   time.Duration
	WithAudio bool
	// Watermark 是否在视频上保留平台水印。
	//
	// false 表示"不要水印"：请求里会**显式**写 watermark_enabled=false。
	// 不能靠"不传"来关——官方 spec 里该字段默认 true。
	Watermark bool
	Fallbacks []string
	// RefImages 参考图列表（vidu2-reference 等参考图生视频模型用，1-3 张）。
	// 非空且模型为 vidu 系时优先于 imageRef。
	RefImages []string
	// AspectRatio 画幅（vidu 系参数，如 "16:9"）；空则按 Size 推导。
	AspectRatio string
	// MovementAmplitude 运动幅度（vidu 系参数：auto/small/medium/large）。
	MovementAmplitude string
	// EndImage 尾帧（data URL 或 URL）。与 imageRef（首帧）一起构成
	// **首尾帧数组**：CogVideoX-3 的 image_url 接受两张图，第一张为首帧、
	// 第二张为尾帧——链式衔接的官方能力（首帧承接上镜末帧，尾帧锚定
	// 本镜分镜构图，运动被夹在两个确定状态之间，不再中途漂移）。
	// 仅对 CogVideoX 系生效；vidu 系参考图模型没有尾帧概念，忽略该字段。
	// 模型不支持首尾帧数组而失败时，VideoGen 会自动降级为单首帧重试。
	EndImage string
}

// isViduModel 判断是否为 vidu 系模型（请求体结构与 CogVideoX 不同）。
func isViduModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "vidu")
}

// videoBody 组装视频请求。理由同 imageBody：null/缺失与 false 语义不同，必须显式表达。
//
// 两种请求体：
//   - CogVideoX：image_url 为单张首帧，或 **首尾帧数组**（官方： cogvideox-3 支持
//     两张图，第一张首帧、第二张尾帧——场景衔接的官方能力）
//   - Vidu（reference 等）：image_url 为参考图数组，无 size/fps/quality/duration，
//     改用 aspect_ratio / movement_amplitude
func videoBody(prompt, imageRef string, opt VideoOptions, model string) []byte {
	if isViduModel(model) {
		body := map[string]any{"model": model, "with_audio": opt.WithAudio}
		if prompt != "" {
			body["prompt"] = prompt
		}
		refs := opt.RefImages
		if len(refs) == 0 && imageRef != "" {
			refs = []string{imageRef}
		}
		if len(refs) > 0 {
			body["image_url"] = refs
		}
		if opt.AspectRatio != "" {
			body["aspect_ratio"] = opt.AspectRatio
		}
		if opt.MovementAmplitude != "" {
			body["movement_amplitude"] = opt.MovementAmplitude
		}
		if !opt.Watermark {
			body["watermark_enabled"] = false
		}
		data, _ := json.Marshal(body)
		return data
	}
	body := map[string]any{"model": model, "with_audio": opt.WithAudio}
	if prompt != "" {
		body["prompt"] = prompt
	}
	if imageRef != "" && opt.EndImage != "" {
		// 首尾帧数组：CogVideoX-3 官方支持（第一张首帧、第二张尾帧）
		body["image_url"] = []string{imageRef, opt.EndImage}
	} else if imageRef != "" {
		body["image_url"] = imageRef
	}
	if opt.Quality != "" {
		body["quality"] = opt.Quality
	}
	if opt.Size != "" {
		body["size"] = opt.Size
	}
	if opt.FPS > 0 {
		body["fps"] = opt.FPS
	}
	if opt.Duration > 0 {
		body["duration"] = opt.Duration
	}
	if !opt.Watermark {
		body["watermark_enabled"] = false
	}
	data, _ := json.Marshal(body)
	return data
}

// VideoGen 提交图生视频任务并等待完成，返回视频 URL。
// imageRef 可以是图片 URL 或 base64——两者都支持，避免把大图 base64 后超长。
//
// 首尾帧数组（EndImage 非空）不是所有降级模型都支持：整轮模型都失败且错误
// 非致命（余额/鉴权类）时，自动去掉尾帧以单首帧重试一轮——衔接控制是增强项，
// 不能因为它不被某个 fallback 模型支持就把镜头卡死。
func (c *Client) VideoGen(prompt, imageRef string, opt VideoOptions) (string, error) {
	if c.cfg.Mock {
		return "mock://video", nil
	}
	url, err := c.videoGenOnce(prompt, imageRef, opt)
	if err != nil && opt.EndImage != "" && !IsFatal(err) {
		opt.EndImage = ""
		return c.videoGenOnce(prompt, imageRef, opt)
	}
	return url, err
}

func (c *Client) videoGenOnce(prompt, imageRef string, opt VideoOptions) (string, error) {
	if opt.Timeout <= 0 {
		opt.Timeout = 20 * time.Minute
	}
	models := withFallback(opt.Model, opt.Fallbacks)
	var lastErr error
	for _, m := range models {
		// 预算预留包住整个尝试（提交 + 轮询）：提交成功到按实测秒数过账之间
		// 可能隔几分钟，不预留的话并发 worker 会在"还没记账"的窗口里集体过闸
		if err := c.reserve("videos.generations", c.estVideoCost(float64(opt.Duration))); err != nil {
			return "", err
		}
		url, err := c.videoModelAttempt(prompt, imageRef, opt, m)
		c.settle(c.estVideoCost(float64(opt.Duration)))
		if err != nil {
			lastErr = err
			if IsFatal(err) {
				return "", err // 余额/鉴权问题：立即上抛，整批熔断
			}
			continue
		}
		return url, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的视频模型")
	}
	return "", lastErr
}

// videoModelAttempt 单模型尝试：提交任务 → 轮询到完成。
// 预算预留由调用方（videoGenOnce）包住整个尝试。
func (c *Client) videoModelAttempt(prompt, imageRef string, opt VideoOptions, m string) (string, error) {
	body := videoBody(prompt, imageRef, opt, m)
	data, err := c.post("/videos/generations", body)
	if err != nil {
		return "", err
	}
	var tr taskResp
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", fmt.Errorf("解析视频任务响应失败: %w", err)
	}
	if tr.ID == "" {
		return "", fmt.Errorf("视频任务提交失败: %s", truncateForLog(data))
	}
	return c.VideoWait(tr.ID, opt.Timeout, nil)
}

// VideoSubmit 提交任务返回任务 ID（兼容接口）。
func (c *Client) VideoSubmit(prompt, imageB64, quality, size string, fps int) (string, error) {
	body := videoBody(prompt, imageB64, VideoOptions{Quality: quality, Size: size, FPS: fps, WithAudio: false}, "cogvideox-3")
	data, err := c.post("/videos/generations", body)
	if err != nil {
		return "", err
	}
	var tr taskResp
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", fmt.Errorf("解析视频任务响应失败: %w", err)
	}
	if tr.ID == "" {
		return "", fmt.Errorf("视频任务提交失败: %s", truncateForLog(data))
	}
	return tr.ID, nil
}

// VideoWait 轮询视频任务直至成功/失败，返回视频 URL。
func (c *Client) VideoWait(taskID string, timeout time.Duration, logf func(string, ...any)) (string, error) {
	deadline := time.Now().Add(timeout)
	interval := 10 * time.Second
	for time.Now().Before(deadline) {
		select {
		case <-c.Context().Done():
			return "", &APIError{Kind: KindTimeout, Op: "videos.wait", Message: "批次已取消"}
		case <-time.After(interval):
		}
		data, err := c.get("/async-result/" + taskID)
		if err != nil {
			// 轮询期间的网络抖动不应直接判失败（任务仍在服务端跑），累计若干次才放弃
			if isTransient(err) {
				continue
			}
			return "", err
		}
		var tr taskResp
		if err := json.Unmarshal(data, &tr); err != nil {
			return "", fmt.Errorf("解析任务状态失败: %w", err)
		}
		switch tr.TaskStatus {
		case "SUCCESS":
			if len(tr.VideoResult) == 0 || tr.VideoResult[0].URL == "" {
				return "", &APIError{Kind: KindUnknown, Op: "videos.wait", Message: "任务成功但无视频 URL"}
			}
			return tr.VideoResult[0].URL, nil
		case "FAIL":
			return "", &APIError{Kind: KindUnknown, Op: "videos.wait", Message: "视频生成失败: " + tr.Msg}
		}
		if logf != nil {
			logf("      …生成中（%s）", taskID)
		}
	}
	return "", &APIError{Kind: KindTimeout, Op: "videos.wait",
		Message: fmt.Sprintf("视频任务超时（%v）：服务端排队过久，可加大 video.timeout_minutes 后重跑", timeout)}
}

func isTransient(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.Kind == KindNetwork || ae.Kind == KindRateLimit || ae.Kind == KindTimeout
}

// RecordVideoCost 记录一段视频的花费（按实测秒数）。
func (c *Client) RecordVideoCost(seconds float64) {
	p, ok := c.priceFor(CostVideo)
	if !ok {
		return
	}
	c.recordMedia(CostVideo, seconds, seconds*p.VideoPerSec)
}

// ---------------------------------------------------------------------------
// 视觉复核（渲染后质检的关键：让模型真的"看"画面）
// ---------------------------------------------------------------------------

// imageRef 把本地图片转成接口可接受的 data URL；已是 URL/base64 则原样返回。
func (c *Client) imageRef(path string) (string, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path, nil
	}
	if strings.HasPrefix(path, "data:") {
		return path, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// FileToDataURL 供渲染模块把首帧图作为参考图传给出图/视频接口。
func FileToDataURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// ---------------------------------------------------------------------------
// TTS
// ---------------------------------------------------------------------------

type ttsReq struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice,omitempty"`
	ResponseFormat string  `json:"response_format,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
}

// ttsMaxInputChars glm-tts 的单次合成上限（官方文档 1024 字符）。
const ttsMaxInputChars = 1000

// 默认模型与降级链：glm-tts 是新一代语音大模型（按上下文预判情绪与语调），
// 自然度明显优于初代 cogtts；cogtts 保留为降级项，防服务端下线旧名。
const (
	defaultTTSModel  = "glm-tts"
	fallbackTTSModel = "cogtts"
)

// TTSOptions TTS 调用参数（与 Image/Video 一致：模型由调用方按配置传入）。
type TTSOptions struct {
	Voice string
	// Speed 服务端合成语速（glm-tts 支持 0.5-2）。<=0 或 ≈1 表示服务端默认。
	Speed float64
	// Model 合成模型；空则用 defaultTTSModel。
	Model string
	// FallbackModels 模型降级链（仅"模型不存在/不支持"时逐个重试）。
	FallbackModels []string
	// Emotion 情绪标注（台本 emotion 字段原样传入）。
	// glm-tts 无该参数（忽略）；MiniMax 等支持情感枚举的供应商会映射后真正生效。
	Emotion string
	// Pitch 音高比值（1.0=原声，1.05=略高）。仅对支持 pitch 的供应商生效；
	// glm-tts 的变调仍由本地 asetrate 完成（narrate 层），两者语义一致。
	Pitch float64
	// Volume 音量比值（1.0=原声）。MiniMax 支持 (0,10]；glm-tts 无该参数。
	Volume float64
}

// TTS 语音合成：返回音频字节（wav）。
// 支持语速参数：慢语速旁白是"电影感"的一部分，不能只有默认语速。
func (c *Client) TTS(text, voice string) ([]byte, error) {
	return c.TTSWith(text, TTSOptions{Voice: voice})
}

// TTSWithSpeed 指定语速的语音合成（speed<=0 表示用服务端默认）。
func (c *Client) TTSWithSpeed(text, voice string, speed float64) ([]byte, error) {
	return c.TTSWith(text, TTSOptions{Voice: voice, Speed: speed})
}

// TTSWith 按参数合成语音。返回 wav 音频字节。
//
// "机器音"的三个主要来源都在这一层处理：
//  1. 模型：语音自然度首先由模型决定。glm-tts（新一代语音大模型，能按上下文
//     预判情绪与语调）明显优于初代 cogtts，因此默认走 glm-tts，
//     模型不可用时按降级链逐个重试
//  2. 文本：TTS 的停顿、重音、语调完全跟着标点走（见 normalizeTTSText）
//  3. 语速：服务端语速是"说话人真的这么说"，远比后期 atempo 拉伸自然；
//     本地 atempo 只应作为超槽兜底
func (c *Client) TTSWith(text string, opt TTSOptions) ([]byte, error) {
	voice := opt.Voice
	if voice == "" {
		voice = "tongtong"
	}
	text = normalizeTTSText(text)
	if text == "" {
		return nil, fmt.Errorf("%w（整句只有符号/舞台提示？）", ErrEmptyTTSText)
	}
	if n := len([]rune(text)); n > ttsMaxInputChars {
		return nil, fmt.Errorf("TTS 文本过长（%d 字符，上限 %d），请在台本中拆分该句", n, ttsMaxInputChars)
	}
	if c.cfg.Mock {
		// 离线演示也必须产出"时长与文本成比例"的真实 WAV：
		// 否则字幕对齐、铺轨、混音这些链路无法在无 Key 环境下被验证。
		return synthWAV(text), nil
	}
	// 供应商路由：MiniMax 端点走 T2A v2（emotion/pitch 参数真正生效），
	// 其余（智谱等 OpenAI 风格 /audio/speech）走原路径。
	// 两家的鉴权方式一致（Bearer），差异只在请求体与响应编码。
	if isMiniMaxEndpoint(c.cfg.BaseURL) {
		return c.minimaxTTS(text, opt)
	}
	speed := opt.Speed
	if math.Abs(speed-1.0) < 0.01 {
		speed = 0 // 1.0 就是服务端默认，不必显式传参
	}
	models := append([]string{opt.Model}, opt.FallbackModels...)
	if opt.Model == "" {
		models[0] = defaultTTSModel
		if len(models) == 1 {
			models = append(models, fallbackTTSModel)
		}
	}
	var lastErr error
	for _, model := range models {
		if model == "" {
			continue
		}
		body, _ := json.Marshal(ttsReq{
			Model: model, Input: text, Voice: voice,
			ResponseFormat: "wav", Speed: speed,
		})
		data, err := c.reservePost("audio.speech", "/audio/speech", body, c.estTTSCost(len([]rune(text))))
		if err == nil {
			if len(data) < 100 {
				return nil, fmt.Errorf("TTS 返回异常（%d 字节），疑似失败响应", len(data))
			}
			p, ok := c.priceFor(CostTTS)
			if ok {
				c.recordMedia(CostTTS, float64(len([]rune(text))), float64(len([]rune(text)))/1000*p.TTSPer1KChars)
			}
			return data, nil
		}
		lastErr = err
		// 只有"模型不存在/不支持"才值得换模型重试；鉴权、余额、限流换了也一样
		var ae *APIError
		if !errors.As(err, &ae) || ae.Kind != KindUnsupported {
			return nil, err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("没有可用的 TTS 模型")
}

// ErrEmptyTTSText 台词规范化后没有可朗读内容（例如整句只有舞台提示）。
// 调用方应跳过该句而不是中断整个配音流程。
var ErrEmptyTTSText = errors.New("TTS 文本为空")

var (
	// 省略号：≥2 个半角句点归一为"……"
	reEllipsisRun = regexp.MustCompile(`\.{2,}`)
	// 汉字后的半角句点视为句号（"好的." → "好的。"）；
	// 数字间与英文后的句点保留（3.5 / Mr. Smith 不能动）
	reHanDot = regexp.MustCompile(`(\p{Han})\.`)
	// 汉字后的半角标点转全角（,?!;:）——中文语句里的半角标点
	// 会被部分模型当普通字符跳过，丢失停顿
	reHanPunct = regexp.MustCompile(`(\p{Han})([,!?;:])`)
)

// normalizeTTSText 合成前的文本规范化。TTS 的韵律（停顿/重音/语调）完全由
// 标点驱动，台本文本里混进的符号、半角标点、缺句尾标点都会直接变成
// "平、赶、怪"的机器感。规则刻意保守：只做有把握的替换，不碰内容。
func normalizeTTSText(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	// 台本里常见的舞台提示（（低声）/（脚步声）等）不是台词，读出来就是事故
	s = stripParenthesized(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			b.WriteRune('，') // 换行/制表符按停顿处理
		case '~', '^', '|', '`', '*', '#', '_', '<', '>', '\\', '/', '•', '·', '※', '➤', '▶':
			// 纯排版符号：TTS 要么读出怪音要么卡顿，直接丢弃
		case '"', '\'', '“', '”', '‘', '’', '「', '」', '『', '』':
			// 引号内容要保留，但引号本身合成时容易读出停顿异常，去掉
		default:
			b.WriteRune(r)
		}
	}
	s = b.String()
	s = reEllipsisRun.ReplaceAllString(s, "……")
	s = reHanDot.ReplaceAllString(s, "$1。")
	s = reHanPunct.ReplaceAllStringFunc(s, func(m string) string {
		r := []rune(m)
		full := map[rune]string{',': "，", '!': "！", '?': "？", ';': "；", ':': "："}[r[len(r)-1]]
		if full == "" {
			return m
		}
		return string(r[:len(r)-1]) + full
	})
	// 连续省略号归一为单个"……"，连续逗号/句号只留一个（TTS 会读出怪异长停顿）
	s = collapseRuns(s, '…', 2)
	s = collapseRuns(s, '，', 1)
	s = collapseRuns(s, '。', 1)
	// 首尾的悬空停顿标点是舞台提示被剔除后的残留，念出来就是开头/结尾一卡
	s = strings.Trim(s, "，。；：")
	if s == "" {
		return ""
	}
	// 句尾必须有终结标点：没有的话最后一句会被念得又平又急
	r := []rune(s)
	switch r[len(r)-1] {
	case '。', '？', '！', '…', '，', '；', '：', '—':
	default:
		s += "。"
	}
	return s
}

// stripParenthesized 去掉全角/半角括号及其内容（台本舞台提示）。
// 只处理成对出现的括号；未闭合的括号保留原样，避免误删正文。
// 注意 IndexRune 返回的是**字节下标**，跳过闭括号必须按其字节长度，
// 否则会把多字节括号切剩半个，留下非法 UTF-8 序列（解码成 U+FFFD 乱码）。
func stripParenthesized(s string) string {
	for _, pair := range [][2]rune{{'（', '）'}, {'(', ')'}} {
		closeLen := len(string(pair[1]))
		for {
			open := strings.IndexRune(s, pair[0])
			if open < 0 {
				break
			}
			close := strings.IndexRune(s[open:], pair[1])
			if close < 0 {
				break // 未闭合：可能是正文里的普通括号，不动它
			}
			s = s[:open] + "，" + s[open+close+closeLen:]
		}
	}
	return s
}

// collapseRuns 把 rune 的连续出现折叠到最多 max 次。
func collapseRuns(s string, target rune, max int) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if r == target {
			count++
			if count <= max {
				b.WriteRune(r)
			}
			continue
		}
		count = 0
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Mock 视觉响应（离线演示用）
// ---------------------------------------------------------------------------

var mockVisionMu sync.Mutex

// synthWAV 生成时长与文本匹配的占位音频（16bit 单声道 24kHz WAV）。
// 用低频正弦 + 包络模拟语音节奏，时长按中文 4 字/秒估算，便于离线验证时间轴。
func synthWAV(text string) []byte {
	const rate = 24000
	runes := []rune(text)
	// 与 srt.EstimateSpeakSec 口径一致：中文 4 字/秒，最少 0.6s
	sec := float64(len(runes)) / 4.0
	if sec < 0.6 {
		sec = 0.6
	}
	n := int(sec * rate)
	data := make([]int16, n)
	for i := 0; i < n; i++ {
		t := float64(i) / rate
		// 220Hz 基频 + 颤音，幅度取 0.6：与真实语音的录音电平相当。
		// （电平过低会让响度测量把整轨判为静音，掩盖真实问题）
		v := 0.6 * math.Sin(2*math.Pi*220*t) * (1 + 0.15*math.Sin(2*math.Pi*6*t))
		env := 1.0
		if t < 0.05 {
			env = t / 0.05
		}
		if rem := sec - t; rem < 0.05 {
			env = rem / 0.05
		}
		data[i] = int16(v * env * 32767)
	}
	return encodeWAV(data, rate)
}

func encodeWAV(samples []int16, rate int) []byte {
	var buf bytes.Buffer
	dataLen := len(samples) * 2
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(rate))
	binary.Write(&buf, binary.LittleEndian, uint32(rate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	for _, s := range samples {
		binary.Write(&buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}

func mockVision(prompt string, out any) error {
	mockVisionMu.Lock()
	defer mockVisionMu.Unlock()
	// 离线演示：返回"首轮有缺陷、复检通过"的结构，便于验证重渲闭环
	payload := `{
	  "score": 7.4, "pass": false,
	  "shots": [
	    {"id":"sh001","score":7.0,"pass":false,"issues":["hand appears with six fingers"],"fix":"重新生成该镜头，避免手部入画或换种子"},
	    {"id":"sh002","score":8.6,"pass":true,"issues":[]},
	    {"id":"sh003","score":8.2,"pass":true,"issues":[]}
	  ],
	  "feedback":"sh001 手部畸形，需换种子重渲。"
	}`
	return json.Unmarshal([]byte(payload), out)
}
