// Package srt 实现镜头槽位制的字幕规划：
// 每条口播文案铺进所属镜头的时间槽内，多条按顺序均分槽位，
// 单条时长贴合预估语速（中文约 4 字/秒），不越出镜头边界。
// 同一算法同时服务于两处：
//   - 流水线阶段（用分镜计划时长先出一版 SRT）
//   - 渲染完成后（用 ffprobe 实测镜头时长重排，消除"计划≠实际"的漂移）
package srt

import (
	"fmt"
	"strings"
	"unicode"
)

// Line 一条口播文案（来自配音台本，保持台本顺序）
type Line struct {
	ShotID string
	Text   string
}

// Slot 镜头槽位（计划时长或实测时长）
type Slot struct {
	ShotID string
	Start  float64
	Dur    float64
}

// Cue 一条成片字幕
type Cue struct {
	Start float64
	End   float64
	Text  string
}

const (
	minDur = 1.0 // 单条最短显示时长（秒）
	maxDur = 6.0 // 单条最长显示时长（秒），超过说明文案太长，应精简而不是挂字幕
)

// EstimateSpeakSec 估算口播时长：CJK 字符 4 字/秒，西文按 2.5 词/秒
func EstimateSpeakSec(text string) float64 {
	var cjk int
	words := 0
	prevLatin := false
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			cjk++
			prevLatin = false
		case r == ' ' || r == ',' || r == '.' || r == '!' || r == '?':
			if prevLatin {
				words++
			}
			prevLatin = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			prevLatin = true
		default:
			prevLatin = false
		}
	}
	if prevLatin {
		words++
	}
	est := float64(cjk)/4.0 + float64(words)/2.5
	if est < 0.6 {
		est = 0.6
	}
	return est
}

// Plan 把文案铺进槽位，返回按时序排列的字幕。
// 规则：
//   - 同一镜头 n 条文案均分槽位（base = dur/n）
//   - 单条不超过 语速估算+0.8s（避免人声结束后字幕久挂）
//     与 maxDur 的较小者，也不小于 minDur
//   - 一律夹在所属镜头槽位内（这是"字幕与画面同步"的硬约束）
func Plan(slots []Slot, lines []Line) []Cue {
	byShot := map[string][]string{}
	for _, l := range lines {
		if l.ShotID == "" || l.Text == "" {
			continue
		}
		byShot[l.ShotID] = append(byShot[l.ShotID], l.Text)
	}
	var cues []Cue
	for _, s := range slots {
		texts := byShot[s.ShotID]
		if len(texts) == 0 || s.Dur <= 0 {
			continue
		}
		base := s.Dur / float64(len(texts))
		cursor := s.Start
		end := s.Start + s.Dur
		for _, t := range texts {
			d := base
			if speak := EstimateSpeakSec(t) + 0.8; speak < d {
				d = speak
			}
			if d < minDur {
				d = minDur
			}
			if d > maxDur {
				d = maxDur
			}
			if cursor+d > end {
				d = end - cursor
			}
			if d < 0.4 { // 槽位尾部放不下一条可读字幕，丢弃
				break
			}
			cues = append(cues, Cue{Start: cursor, End: cursor + d, Text: t})
			cursor += d
		}
	}
	return cues
}

// Render 输出标准 SRT 文本
func Render(cues []Cue) string {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(c.Start), srtTime(c.End), c.Text)
	}
	return b.String()
}

func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	s := int(sec)
	ms := int((sec - float64(s)) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", s/3600, (s%3600)/60, s%60, ms)
}
