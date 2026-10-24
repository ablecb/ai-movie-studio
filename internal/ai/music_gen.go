package ai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 纯音乐/BGM 生成（MiniMax Music）
//
// 本项目的混音链（侧链闪避、响度归一）是现成的，缺的从来都是"把 cue 变成音频"。
// 曲库缺素材时按 cue 描述生成器乐：POST /v1/music_generation，
// 响应与 T2A 一致（data.audio hex/base64）。
//
// 失败语义：BGM 是增强项——生成失败只应让该 cue 保持缺失（交付报告如实说明
// "无配乐"），绝不能阻断整个交付。调用方（deliver）负责按告警降级。
// ---------------------------------------------------------------------------

type minimaxMusicBody struct {
	Model        string               `json:"model"`
	Prompt       string               `json:"prompt"`
	Lyrics       string               `json:"lyrics,omitempty"`
	AudioSetting *minimaxAudioSetting `json:"audio_setting,omitempty"`
	OutputFormat string               `json:"output_format,omitempty"`
}

// MusicGen 生成纯音乐片段。prompt 为器乐/情绪/节奏的描述（中文即可），
// seconds 为期望时长（秒）。返回音频字节（mp3）。
func (c *Client) MusicGen(prompt string, seconds int) ([]byte, error) {
	if c.cfg.Mock {
		return synthWAV("music"), nil
	}
	// 供应商路由：豆包端点走音频生成模型（音效/配乐同一通道，providers.music
	// 指向 bytedance 即可，与 TTS 共用凭据）；MiniMax 端点走 Music。
	if isSeedTTSEndpoint(c.cfg.BaseURL) {
		return c.audioGen(prompt, seconds)
	}
	if !isMiniMaxEndpoint(c.cfg.BaseURL) {
		return nil, fmt.Errorf("音乐生成支持 MiniMax（base_url 含 minimax）或豆包音频生成（base_url 含 bytedance）端点")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("音乐生成需要非空 prompt")
	}
	model := c.cfg.Model
	if model == "" {
		model = "music-1.5"
	}
	// lyrics 留空 = 纯器乐；prompt 里写明"纯音乐/无人声"双保险
	body := minimaxMusicBody{
		Model:        model,
		Prompt:       prompt,
		Lyrics:       "\n",
		AudioSetting: &minimaxAudioSetting{SampleRate: 32000, Bitrate: 256000, Format: "mp3", Channel: 2},
	}
	data, err := c.reservePost("music_generation", "/v1/music_generation", mustJSON(body), 0)
	if err != nil {
		return nil, err
	}
	var resp minimaxTTSResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析音乐生成响应失败: %w", err)
	}
	if resp.BaseResp.StatusCode != 0 {
		return nil, ClassifyHTTP("music_generation", 400,
			fmt.Sprintf("status_code=%d %s", resp.BaseResp.StatusCode, resp.BaseResp.StatusMsg))
	}
	if resp.Data.Audio == "" {
		return nil, fmt.Errorf("音乐生成返回空音频")
	}
	audio, derr := decodeAudioPayload(resp.Data.Audio)
	if derr != nil {
		return nil, derr
	}
	if p, ok := c.priceFor(CostMusic); ok {
		c.recordMedia(CostMusic, 1, p.MusicPerCall)
	}
	return audio, nil
}
