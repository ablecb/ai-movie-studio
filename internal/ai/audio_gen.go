package ai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 豆包音频生成（Seed-Audio 1.0）—— 影视级音效/配乐的生成通道
//
// music.json 的 cue_sheet（BGM）与 sound_fx（逐镜头环境声/音效）一直只有
// "方案"没有"素材"：曲库空、MiniMax 未配，混音器永远拿到 0 配乐 0 音效。
// 豆包音频生成模型（与 TTS 同账号同鉴权，providers.music 指向 bytedance
// 端点即可复用）单条 prompt 直出音效/音乐/歌曲，最长 120s。
//
// 接口（实测 2026-10）：POST /api/v3/tts/create，**扁平请求体/扁平响应**
// （与 TTS V3 的 req_params 包装 + NDJSON 分片都不同，勿混用）：
//
//	请求：{model: "seed-audio-1.0", text_prompt, audio_config: {format, sample_rate}}
//	响应：{code?: int, message?: string, audio: base64, duration: 秒}
//
// 鉴权头与 TTS V3 相同（X-Api-Key 或 appid+access-key 双凭据）。
// ---------------------------------------------------------------------------

type audioGenBody struct {
	Model       string           `json:"model"`
	TextPrompt  string           `json:"text_prompt"`
	AudioConfig *audioGenAudioCF `json:"audio_config,omitempty"`
}

type audioGenAudioCF struct {
	Format     string `json:"format,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
}

type audioGenResp struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Audio    string `json:"audio"`
	Duration any    `json:"duration"` // 秒；不同版本可能是数字或字符串，只做日志展示
}

// audioGenRespAudio 解析音频生成响应（纯函数便于测试）。
// 成功以 audio 非空为准——成功响应里 code 可能缺省（null→0）。
func audioGenRespAudio(data []byte) ([]byte, error) {
	var resp audioGenResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析音频生成响应失败: %w", err)
	}
	if resp.Audio == "" {
		if resp.Code != 0 || resp.Message != "" {
			return nil, fmt.Errorf("code=%d %s", resp.Code, resp.Message)
		}
		return nil, fmt.Errorf("音频生成返回空音频")
	}
	b, err := base64.StdEncoding.DecodeString(resp.Audio)
	if err != nil {
		return nil, fmt.Errorf("音频 base64 解码失败: %w", err)
	}
	return b, nil
}

// audioGen 豆包音频生成。seconds 为期望时长（秒）：接口无独立时长参数，
// 作为提示写进 text_prompt（模型按描述决定实际长度，单次上限 120s）。
func (c *Client) audioGen(prompt string, seconds int) ([]byte, error) {
	model := c.cfg.Model
	if !strings.HasPrefix(model, "seed-audio") {
		model = "seed-audio-1.0"
	}
	text := prompt
	if seconds > 0 && seconds <= 120 {
		text = fmt.Sprintf("%s\n时长约 %d 秒。", prompt, seconds)
	}
	body := audioGenBody{
		Model:       model,
		TextPrompt:  text,
		AudioConfig: &audioGenAudioCF{Format: "mp3", SampleRate: 44100},
	}
	headers := map[string]string{}
	if appid := c.cfg.AppID; appid != "" {
		headers["X-Api-App-Id"] = appid
		headers["X-Api-Access-Key"] = c.cfg.Key()
	} else {
		headers["X-Api-Key"] = c.cfg.Key()
	}
	data, err := c.seedPostV3("/api/v3/tts/create", headers, mustJSON(body), 0)
	if err != nil {
		return nil, err
	}
	audio, err := audioGenRespAudio(data)
	if err != nil {
		return nil, err
	}
	if p, ok := c.priceFor(CostMusic); ok {
		c.recordMedia(CostMusic, 1, p.MusicPerCall)
	}
	return audio, nil
}
