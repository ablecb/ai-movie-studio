package ai

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// 火山引擎豆包语音合成（Seed-TTS / BigTTS）
//
// 为什么需要第三家 TTS：glm-tts 无 emotion/pitch 入参，MiniMax 的情绪枚举
// 强度单一；豆包 Seed-TTS 的情绪随上下文预测，emotion/emotion_scale 真正可调，
// 是国内短剧配音的第一梯队方案（同源技术就是剪映/抖音的配音）。
//
// 接入方式：providers.tts.base_url 指向 openspeech.bytedance.com 即走本实现。
// 选 V1 HTTP 非流式接口（POST /api/v1/tts）：一次一句、JSON 返回 base64 音频，
// 与"逐句合成再实测铺轨"的配音流水线同构；V3 流式接口是实时场景的时延优化，
// 对离线逐句合成没有增益。注意 V1 不支持 2.0 音色（*_uranus_*），选音色时
// 用 *_mars_bigtts / *_wvae_bigtts 系列。
//
// 双凭据：appid（body.app.appid，标识账号）+ access token（请求头
// Authorization: Bearer;<token>）。分号格式与 OpenAI 风格的空格格式不同，
// doCtx 写死了空格格式不能复用，这里单独发请求（seedPost）。
//
// 情绪：多情感音色支持 emotion（枚举）+ enable_emotion + emotion_scale(1-5)。
// 台本情绪经 MapEmotion 归一后转成豆包枚举；映射不上就不传该参数——
// Seed-TTS 的上下文情绪预测比映射错强。
// ---------------------------------------------------------------------------

// isSeedTTSEndpoint 判断端点是否为火山语音平台。
// 火山方舟（ark.*.volces.com，出图/出视频）与本接口（openspeech）同属字节，
// 但鉴权体系不同：方舟单凭据 Bearer，语音 appid+token 双凭据。
func isSeedTTSEndpoint(baseURL string) bool {
	u := strings.ToLower(baseURL)
	return strings.Contains(u, "openspeech.bytedance.com") || strings.Contains(u, "bytedance.com")
}

// IsSeedTTSEndpoint 供外部包判断 TTS 端点是否为火山语音平台——
// narrate 层据此决定 emotion 参数是否真正生效（2.0 代际不吃该参数）。
func IsSeedTTSEndpoint(baseURL string) bool { return isSeedTTSEndpoint(baseURL) }

// ---------------------------------------------------------------------------
// V3 单向流式接口（新版控制台，X-Api-Key 单凭据）
//
// POST /api/v3/tts/unidirectional：一次性输入、NDJSON 分片流式返回
// （每行 {code, message, data}，data 为 base64 音频分片，顺序拼接即完整文件）。
// 鉴权二选一：新版控制台 X-Api-Key；旧版 X-Api-App-Id + X-Api-Access-Key。
// Resource-Id 选音色代际：seed-tts-1.0 → 1.x 音色（*_mars_bigtts 等，
// emotion 参数可用）；seed-tts-2.0 → 2.0 音色（*_uranus_*，情绪靠上下文预测）。
// ---------------------------------------------------------------------------

type seedV3Body struct {
	User struct {
		UID string `json:"uid"`
	} `json:"user"`
	ReqParams struct {
		Text       string           `json:"text"`
		Speaker    string           `json:"speaker"`
		AudioParms seedV3AudioParms `json:"audio_params"`
	} `json:"req_params"`
}

type seedV3AudioParms struct {
	Format       string `json:"format"`
	SampleRate   int    `json:"sample_rate"`
	SpeechRate   int    `json:"speech_rate,omitempty"`   // [-50,100]：100=2x，-50=0.5x
	LoudnessRate int    `json:"loudness_rate,omitempty"` // [-50,100]
	Emotion      string `json:"emotion,omitempty"`       // 仅 1.0 代际；2.0 靠上下文预测
	EmotionScale int    `json:"emotion_scale,omitempty"`
}

// seedV3RequestBody 构造 V3 请求体（纯函数便于测试）。
// speedRatio [0.1,2.0] 映射为 speech_rate [-50,100]；2.0 代际不传 emotion。
func seedV3RequestBody(text, voice string, opt TTSOptions, resourceID string) seedV3Body {
	var body seedV3Body
	body.User.UID = "aistudio"
	body.ReqParams.Text = text
	body.ReqParams.Speaker = seedDefaultVoice(voice)
	speed := opt.Speed
	if speed <= 0 || abs1(speed-1.0) < 0.01 {
		speed = 1.0
	}
	speechRate := int((speed-1.0)*100 + 0.5)
	if speechRate > 100 {
		speechRate = 100
	}
	if speechRate < -50 {
		speechRate = -50
	}
	body.ReqParams.AudioParms = seedV3AudioParms{
		Format:     "wav",
		SampleRate: 24000,
		SpeechRate: speechRate,
	}
	// 2.0 代际没有 emotion 入参（情绪由模型按上下文预测）——传了会被拒绝
	if !strings.Contains(resourceID, "2.0") {
		if emo := seedEmotion(MapEmotion(opt.Emotion)); emo != "" {
			body.ReqParams.AudioParms.Emotion = emo
			body.ReqParams.AudioParms.EmotionScale = seedEmotionScale(opt.Emotion)
		}
	}
	return body
}

// seedV3ParseResponse 解析 V3 NDJSON 响应。
// 帧约定（与官方语义一致）：code=0 的行携带 data（base64 音频分片，顺序拼接）；
// code=20000000 是"合成成功结束"标记，**不是错误**；其余非零 code 为合成失败。
// 先累积 data 再判断结束/错误——有的响应把数据与结束码放在同一行。
func seedV3ParseResponse(data []byte) ([]byte, error) {
	var audio []byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var part struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &part); err != nil {
			continue // 非 JSON 行（如 SSE 前缀）跳过
		}
		if part.Data != "" {
			chunk, err := base64.StdEncoding.DecodeString(part.Data)
			if err != nil {
				return nil, fmt.Errorf("音频分片 base64 解码失败: %w", err)
			}
			audio = append(audio, chunk...)
		}
		if part.Code == 20000000 {
			break // 成功结束标记
		}
		if part.Code != 0 {
			return nil, fmt.Errorf("code=%d %s", part.Code, part.Message)
		}
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("响应中没有音频分片")
	}
	return audio, nil
}

// seedTTSV3 V3 单向流式合成（新版控制台 X-Api-Key / 旧版 appid+token 双兼容）。
func (c *Client) seedTTSV3(text string, opt TTSOptions) ([]byte, error) {
	resource := c.cfg.Model
	if resource == "" {
		resource = "seed-tts-1.0"
	}
	if !strings.HasPrefix(resource, "seed-tts") {
		resource = "seed-tts-1.0" // model 字段被挪作他用时兜底，不让非法值打到 API
	}
	body := seedV3RequestBody(text, opt.Voice, opt, resource)
	headers := map[string]string{
		"X-Api-Resource-Id": resource,
		"X-Api-Request-Id":  seedReqID(),
	}
	if appid := c.cfg.AppID; appid != "" {
		headers["X-Api-App-Id"] = appid
		headers["X-Api-Access-Key"] = c.cfg.Key()
	} else {
		headers["X-Api-Key"] = c.cfg.Key()
	}
	data, err := c.seedPostV3("/api/v3/tts/unidirectional", headers, mustJSON(body), c.estTTSCost(len([]rune(text))))
	if err != nil {
		return nil, err
	}
	audio, err := seedV3ParseResponse(data)
	if err != nil {
		return nil, fmt.Errorf("豆包 TTS 合成失败（resource=%s）: %w", resource, err)
	}
	p, ok := c.priceFor(CostTTS)
	if ok {
		c.recordMedia(CostTTS, float64(len([]rune(text))), float64(len([]rune(text)))/1000*p.TTSPer1KChars)
	}
	return audio, nil
}

// seedPostV3 与 seedPost 同构（预算预留+结算），鉴权/资源头由 headers 传入，
// 响应为流式 NDJSON，整体读回后由 seedV3ParseResponse 解析。
func (c *Client) seedPostV3(path string, headers map[string]string, body []byte, est float64) ([]byte, error) {
	if err := c.guardBudget("POST " + path); err != nil {
		return nil, err
	}
	if err := c.reserve("seed.tts.v3", est); err != nil {
		return nil, err
	}
	defer c.settle(est)
	req, err := http.NewRequestWithContext(c.Context(), http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ClassifyTransport("POST "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ClassifyTransport("POST "+path, err)
	}
	if resp.StatusCode >= 300 {
		return nil, ClassifyHTTP(path, resp.StatusCode, truncateForLog(data))
	}
	return data, nil
}

// seedEmotion 把 MapEmotion 的通用枚举转成豆包枚举，白名单之外一律返回空：
// 不认识的值透传给 API 是引火烧身（音色不支持该枚举会 400），不如不传——
// Seed-TTS 的上下文情绪预测比映射错强。豆包无 fearful/disgusted/neutral。
func seedEmotion(e string) string {
	switch e {
	case "happy", "sad", "angry":
		return e
	case "fearful":
		return "fear"
	case "surprised":
		return "surprise"
	default:
		return ""
	}
}

type seedTTSBody struct {
	App struct {
		AppID   string `json:"appid"`
		Token   string `json:"token"` // 官方约定传占位值；真正鉴权在请求头
		Cluster string `json:"cluster"`
	} `json:"app"`
	User struct {
		UID string `json:"uid"`
	} `json:"user"`
	Audio   seedTTSAudio `json:"audio"`
	Request struct {
		ReqID     string `json:"reqid"`
		Text      string `json:"text"`
		TextType  string `json:"text_type"`
		Operation string `json:"operation"`
		Model     string `json:"model,omitempty"` // seed-tts-1.1 等；空 = 服务端默认
	} `json:"request"`
}

type seedTTSAudio struct {
	VoiceType     string  `json:"voice_type"`
	Encoding      string  `json:"encoding"`
	Rate          int     `json:"rate,omitempty"`
	SpeedRatio    float64 `json:"speed_ratio,omitempty"`
	Emotion       string  `json:"emotion,omitempty"`
	EnableEmotion bool    `json:"enable_emotion,omitempty"`
	// EmotionScale 情绪强度 1-5（默认 4）。电影台词要"收着演"：4 起步不过火，
	// 台本标了极端情绪（战吼/嘶吼）由调用方抬到 5。
	EmotionScale int `json:"emotion_scale,omitempty"`
}

type seedTTSResp struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Data     string `json:"data"` // base64 音频（code=3000 时有效）
	Addition struct {
		DurationMs int `json:"duration"`
	} `json:"addition"`
}

// seedTTSRequestBody 构造请求体（独立成纯函数以便测试断言）。
func seedTTSRequestBody(text string, opt TTSOptions, appid, model string) seedTTSBody {
	var body seedTTSBody
	body.App.AppID = appid
	body.App.Token = "access_token"
	body.App.Cluster = "volcano_tts"
	body.User.UID = "aistudio"
	// 语速归一要在 clamp 之前：Speed=0 表示"未指定"，clamp 会把它推到下限 0.2
	speed := opt.Speed
	if speed <= 0 || abs1(speed-1.0) < 0.01 {
		speed = 1.0
	}
	body.Audio = seedTTSAudio{
		VoiceType:  seedDefaultVoice(opt.Voice),
		Encoding:   "wav",
		Rate:       24000,
		SpeedRatio: clampRange(speed, 0.2, 3.0),
	}
	if emo := seedEmotion(MapEmotion(opt.Emotion)); emo != "" {
		body.Audio.Emotion = emo
		body.Audio.EnableEmotion = true
		body.Audio.EmotionScale = seedEmotionScale(opt.Emotion)
	}
	body.Request.ReqID = seedReqID()
	body.Request.Text = text
	body.Request.TextType = "plain"
	body.Request.Operation = "query"
	body.Request.Model = model
	return body
}

func seedDefaultVoice(voice string) string {
	if voice == "" {
		return "zh_male_M392_conversation_wvae_bigtts" // 官方文档示例音色
	}
	return voice
}

// seedEmotionScale 情绪强度：极端表演类（战吼/嘶吼/哭喊）给满 5，其余 4 收着演。
func seedEmotionScale(emotion string) int {
	for _, w := range []string{"吼", "喊", "哭", "炸裂", "嘶", "燃"} {
		if strings.Contains(emotion, w) {
			return 5
		}
	}
	return 4
}

func seedReqID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func abs1(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// seedTTS 豆包语音合成。返回 wav 音频字节。
func (c *Client) seedTTS(text string, opt TTSOptions) ([]byte, error) {
	appid := c.cfg.AppID
	if appid == "" {
		return nil, fmt.Errorf("豆包语音合成需要双凭据：providers.tts 配 appid_env（或 appid）+ api_key_env")
	}
	model := opt.Model
	if model == "" {
		model = c.cfg.Model
	}
	// 严格模式：appid 走 V1 接口，而 "seed-tts-2.0" 是 V3 的资源 ID 语义，
	// V1 不支持 2.0 代际（含 *_uranus_* 音色）。带病请求会以一次难排查的
	// 400 收场——配置矛盾当场报错，给出两条明确出路。
	if strings.Contains(model, "2.0") {
		return nil, fmt.Errorf("providers.tts 配置矛盾：appid 走 V1 接口，不支持 model=%q（V3 资源 ID 语义、2.0 代际）——去掉 appid 走 V3 单凭据，或把 model 改为 seed-tts-1.x", model)
	}
	body := seedTTSRequestBody(text, opt, appid, model)
	data, err := c.seedPost("seed.tts", "/api/v1/tts", mustJSON(body), c.estTTSCost(len([]rune(text))))
	if err != nil {
		return nil, err
	}
	var resp seedTTSResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析豆包 TTS 响应失败: %w", err)
	}
	if resp.Code != 3000 {
		return nil, ClassifyHTTP("seed.tts", 400,
			fmt.Sprintf("code=%d %s", resp.Code, resp.Message))
	}
	if resp.Data == "" {
		return nil, fmt.Errorf("豆包 TTS 返回空音频（code=%d %s）", resp.Code, resp.Message)
	}
	audio, err := base64.StdEncoding.DecodeString(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("豆包 TTS 音频 base64 解码失败: %w", err)
	}
	p, ok := c.priceFor(CostTTS)
	if ok {
		c.recordMedia(CostTTS, float64(len([]rune(text))), float64(len([]rune(text)))/1000*p.TTSPer1KChars)
	}
	return audio, nil
}

// seedPost 与 reservePost 同构（预算预留 + 结算），但鉴权头用火山的
// "Bearer;<token>" 分号格式——doCtx 写死空格格式，不能复用。
func (c *Client) seedPost(op, path string, body []byte, est float64) ([]byte, error) {
	if err := c.guardBudget("POST " + path); err != nil {
		return nil, err
	}
	if err := c.reserve(op, est); err != nil {
		return nil, err
	}
	defer c.settle(est)
	req, err := http.NewRequestWithContext(c.Context(), http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer;"+c.cfg.Key())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ClassifyTransport("POST "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ClassifyTransport("POST "+path, err)
	}
	if resp.StatusCode >= 300 {
		return nil, ClassifyHTTP(path, resp.StatusCode, truncateForLog(data))
	}
	return data, nil
}
