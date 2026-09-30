package ai

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// MiniMax T2A v2 语音合成（人声自然度的主升级路径）
//
// 为什么需要第二家 TTS：glm-tts 的接口只有 7 个预置音色 + speed/volume 两个参数，
// 没有 emotion/pitch 入参，也没有 SSML——"人声机械呆板"是模型接口的能力上限，
// 不是提示词能救的。MiniMax T2A v2（speech-2.6/2.8 系列）提供：
//   - emotion 枚举：happy/sad/angry/fearful/disgusted/surprised/neutral 等
//   - pitch（音调）独立调节，比本地 asetrate 变调自然一个量级
//   - 音色复刻（voice clone）与数百个预置音色
//
// 接入方式：providers.tts.base_url 指向 MiniMax 平台（含 "minimax" 即走本实现），
// 请求路径 /v1/t2a_v2，鉴权与智谱一致（Bearer）。响应 data.audio 默认 hex 编码，
// 兼容 base64（不同版本行为有差异，两种都试）。
// ---------------------------------------------------------------------------

// minimaxTTSBody T2A v2 请求体（按官方 schema；多余参数不传）。
type minimaxTTSBody struct {
	Model        string              `json:"model"`
	Text         string              `json:"text"`
	VoiceSetting minimaxVoiceSetting `json:"voice_setting"`
	AudioSetting minimaxAudioSetting `json:"audio_setting"`
}

type minimaxVoiceSetting struct {
	VoiceID string  `json:"voice_id"`
	Speed   float64 `json:"speed,omitempty"`
	Vol     float64 `json:"vol,omitempty"`
	// Pitch 音调调整。取值 [-12, 12] 的整数（半音）。
	// TTSOptions.Pitch 是比值（1.05 = 略高），这里换算成半音。
	Pitch                int    `json:"pitch,omitempty"`
	Emotion              string `json:"emotion,omitempty"` // 枚举见 emotionEnums；空 = 模型自动识别
	LatexRead            bool   `json:"latex_read,omitempty"`
	EnglishNormalization bool   `json:"english_normalization,omitempty"`
}

type minimaxAudioSetting struct {
	SampleRate int    `json:"sample_rate"`
	Bitrate    int    `json:"bitrate"`
	Format     string `json:"format"` // mp3 | pcm | flac | wav
	Channel    int    `json:"channel"`
}

type minimaxTTSResp struct {
	Data struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
	} `json:"data"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// emotionEnums MiniMax 支持的情感枚举（用于校验映射结果是否合法）。
var emotionEnums = map[string]bool{
	"happy": true, "sad": true, "angry": true, "fearful": true,
	"disgusted": true, "surprised": true, "neutral": true, "fluent": true,
}

// MapEmotion 把台本里的中文情绪标注映射为 MiniMax emotion 枚举。
//
// 台本的 emotion 是自由文本（如"低声，几乎像自言自语"、"急切"），模型经常给出
// 枚举外的描述。这里做确定性的关键词映射：命中即返回枚举，未命中返回空
// （空 = 不传该参数，让模型从文本自动识别情绪，效果仍好于映射错）。
func MapEmotion(emotion string) string {
	s := strings.ToLower(strings.TrimSpace(emotion))
	if s == "" {
		return ""
	}
	// 已是合法枚举（英文）直接返回
	if emotionEnums[s] {
		return s
	}
	// 关键词按优先级匹配；同一句里"又哭又笑"取先命中的类别是可接受的取舍
	rules := []struct {
		emo   string
		words []string
	}{
		{"angry", []string{"愤怒", "怒", "厉声", "咆哮", "恼"}},
		{"fearful", []string{"恐惧", "害怕", "惊恐", "紧张", "不安", "颤抖"}},
		{"sad", []string{"悲伤", "低沉", "哽咽", "沉重", "忧郁", "难过", "遗憾", "哭", "低声"}},
		{"surprised", []string{"惊讶", "惊", "意外", "震惊"}},
		{"disgusted", []string{"厌恶", "嫌弃", "鄙夷"}},
		{"happy", []string{"开心", "兴奋", "愉快", "欣慰", "笑", "喜悦", "明快", "上扬"}},
		{"neutral", []string{"平静", "冷静", "沉着", "克制"}},
	}
	for _, r := range rules {
		for _, w := range r.words {
			if strings.Contains(s, w) {
				return r.emo
			}
		}
	}
	return ""
}

// isMiniMaxEndpoint 判断端点是否为 MiniMax 平台。
func isMiniMaxEndpoint(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "minimax")
}

// pitchRatioToSemitone 把比值音高（1.0=原声）换算成半音数（四舍五入，钳在 ±12）。
// 1.05 ≈ +1 半音，0.92 ≈ -1.5 → -1。
func pitchRatioToSemitone(ratio float64) int {
	if ratio <= 0 || math.Abs(ratio-1.0) < 0.01 {
		return 0
	}
	semis := int(math.Round(12 * math.Log2(ratio)))
	if semis > 12 {
		semis = 12
	}
	if semis < -12 {
		semis = -12
	}
	return semis
}

// minimaxTTS MiniMax T2A v2 合成。返回 mp3 音频字节。
func (c *Client) minimaxTTS(text string, opt TTSOptions) ([]byte, error) {
	voice := opt.Voice
	if voice == "" {
		voice = "female-shaonv" // MiniMax 常用预置音色；voice_map 可覆盖
	}
	speed := opt.Speed
	if speed <= 0 || math.Abs(speed-1.0) < 0.01 {
		speed = 1.0
	}
	vol := opt.Volume
	if vol <= 0 {
		vol = 1.0
	}
	body := minimaxTTSBody{
		Model: opt.Model,
		Text:  text,
		VoiceSetting: minimaxVoiceSetting{
			VoiceID: voice,
			Speed:   clampRange(speed, 0.5, 2.0),
			Vol:     clampRange(vol, 0.1, 10.0),
			Pitch:   pitchRatioToSemitone(opt.Pitch),
			Emotion: MapEmotion(opt.Emotion),
		},
		AudioSetting: minimaxAudioSetting{
			SampleRate: 24000,
			Bitrate:    128000,
			Format:     "mp3",
			Channel:    1,
		},
	}
	if body.Model == "" {
		body.Model = "speech-2.6-hd"
	}
	data, err := c.reservePost("t2a_v2", "/v1/t2a_v2", mustJSON(body), c.estTTSCost(len([]rune(text))))
	if err != nil {
		return nil, err
	}
	var resp minimaxTTSResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析 MiniMax TTS 响应失败: %w", err)
	}
	if resp.BaseResp.StatusCode != 0 {
		return nil, ClassifyHTTP("t2a_v2", 400,
			fmt.Sprintf("status_code=%d %s", resp.BaseResp.StatusCode, resp.BaseResp.StatusMsg))
	}
	if resp.Data.Audio == "" {
		return nil, fmt.Errorf("MiniMax TTS 返回空音频")
	}
	audio, derr := decodeAudioPayload(resp.Data.Audio)
	if derr != nil {
		return nil, derr
	}
	p, ok := c.priceFor(CostTTS)
	if ok {
		c.recordMedia(CostTTS, float64(len([]rune(text))), float64(len([]rune(text)))/1000*p.TTSPer1KChars)
	}
	return audio, nil
}

// decodeAudioPayload 响应音频兼容 hex 与 base64 两种编码（随 API 版本有差异）。
func decodeAudioPayload(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	return nil, fmt.Errorf("MiniMax TTS 音频无法解码（hex/base64 均失败，长度 %d）", len(s))
}

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// ExportMinimaxTTSBody 供测试断言实际请求体。
func ExportMinimaxTTSBody(text string, opt TTSOptions) minimaxTTSBody {
	voice := opt.Voice
	if voice == "" {
		voice = "female-shaonv"
	}
	speed := opt.Speed
	if speed <= 0 || speed == 1.0 {
		speed = 1.0
	}
	vol := opt.Volume
	if vol <= 0 {
		vol = 1.0
	}
	body := minimaxTTSBody{
		Model: opt.Model,
		Text:  text,
		VoiceSetting: minimaxVoiceSetting{
			VoiceID: voice,
			Speed:   clampRange(speed, 0.5, 2.0),
			Vol:     clampRange(vol, 0.1, 10.0),
			Pitch:   pitchRatioToSemitone(opt.Pitch),
			Emotion: MapEmotion(opt.Emotion),
		},
		AudioSetting: minimaxAudioSetting{SampleRate: 24000, Bitrate: 128000, Format: "mp3", Channel: 1},
	}
	if body.Model == "" {
		body.Model = "speech-2.6-hd"
	}
	return body
}
