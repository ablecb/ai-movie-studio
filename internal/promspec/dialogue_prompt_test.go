package promspec

import (
	"strings"
	"testing"
)

// 台词进视频提示词的回归：生成端是无声生成（音频不进请求），不写台词，模型
// 只知道"画面里有人在说话"——说什么、说多久、哪个时间段完全随机。写进去至少
// 让"说话"有一句话的起止与节奏；"看不见嘴"的镜头还要明确禁止配可见唇动。

func TestVideoPromptCarriesDialogueWithWindow(t *testing.T) {
	kit := sampleKit()
	s := sampleShot() // 特写 + 一句台词 AtSec 0.3，镜头 4.5s
	vp := VideoPrompt(kit.Style, s)

	if !strings.Contains(vp, "信号源……是麦田？") {
		t.Errorf("视频提示词必须带台词原文（否则嘴型节奏完全随机）:\n%s", vp)
	}
	if !strings.Contains(vp, "0.3s-4.5s") {
		t.Errorf("视频提示词必须带镜头内时间窗（末句出点=镜头结束）:\n%s", vp)
	}
	if !strings.Contains(vp, "信使·林") {
		t.Errorf("视频提示词必须指认说话人:\n%s", vp)
	}
	// 默认（未声明 covered/offscreen）= 嘴部可见：节奏约束、且不得要求"不许动嘴"
	if !strings.Contains(vp, "mouth visible") {
		t.Errorf("默认应标记嘴部可见:\n%s", vp)
	}
	// 有台词的镜头节拍全是台词，动作描述曾整个缺失——表演节奏没得可依
	if !strings.Contains(vp, sampleShot().ActionEn) {
		t.Errorf("有台词的镜头仍须带画面/表演描述:\n%s", vp)
	}
}

func TestVideoPromptMouthCoveredForbidsLipMovement(t *testing.T) {
	kit := sampleKit()
	s := sampleShot()
	s.MouthVisibility = "covered"
	vp := VideoPrompt(kit.Style, s)
	if !strings.Contains(vp, "mouth not clearly visible") {
		t.Errorf("covered 应标注嘴部不可见:\n%s", vp)
	}
	if !strings.Contains(vp, "do NOT animate visible lip movement") {
		t.Errorf("covered/offscreen 必须明确禁止可见唇动:\n%s", vp)
	}
}

// 旁白不是"动作"：写成 Action 会让模型给它配画面动作；它也不该出现"动嘴"约束。
func TestVideoPromptTreatsNarrationAsOffscreenLine(t *testing.T) {
	kit := sampleKit()
	s := sampleShot()
	s.Beats = []Beat{
		{Kind: "narration", Speaker: "narrator", Text: "深空第七年，一束信号撕开了寂静。", AtSec: 0},
		{Kind: "action", Text: "she leans toward the console", AtSec: 2.0},
	}
	vp := VideoPrompt(kit.Style, s)
	if strings.Contains(vp, "Action: 深空第七年") {
		t.Errorf("旁白不得被当作画面动作:\n%s", vp)
	}
	if !strings.Contains(vp, "narration, off-screen") {
		t.Errorf("旁白应标注为画外音:\n%s", vp)
	}
	if !strings.Contains(vp, "she leans toward the console") {
		t.Errorf("真正的动作节拍必须保留:\n%s", vp)
	}
}

// 无台词的镜头：不得出现台词块（旧行为保持；提示词里不该有凭空的"有人在说话"）。
func TestVideoPromptWithoutDialogueHasNoDialogueBlock(t *testing.T) {
	kit := sampleKit()
	s := sampleShot()
	s.Beats = nil
	vp := VideoPrompt(kit.Style, s)
	if strings.Contains(vp, "Dialogue line") || strings.Contains(vp, "Dialogue rules") {
		t.Errorf("无台词镜头不应出现台词块:\n%s", vp)
	}
}

// 时间窗取下一个节拍的入点：同一镜多句台词时，前一句的出点不能拖到镜头结束。
func TestBeatWindowEndsAtNextBeat(t *testing.T) {
	s := sampleShot()
	s.DurationSec = 6
	s.Beats = []Beat{
		{Kind: "dialogue", Speaker: "信使·林", Text: "第一句", AtSec: 0.0},
		{Kind: "dialogue", Speaker: "信使·林", Text: "第二句", AtSec: 3.0},
	}
	start, end := s.beatWindow(s.Beats[0])
	if start != 0 || end != 3.0 {
		t.Fatalf("首句窗口应为 [0, 3.0)，实际 [%.1f, %.1f]", start, end)
	}
	start, end = s.beatWindow(s.Beats[1])
	if start != 3.0 || end != 6.0 {
		t.Fatalf("末句出点应取镜头结束 6.0，实际 [%.1f, %.1f]", start, end)
	}
}
