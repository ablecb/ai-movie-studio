package subtitle

import (
	"strings"
	"testing"
	"time"

	"aistudio/internal/config"
)

func TestWrapChineseByShortestBreaker(t *testing.T) {
	// 超长中文必须折成两行，且不丢字
	text := "深空第七年，一束信号撕开了寂静的黑暗，某种东西正在靠近。"
	lines := Wrap(text, 18, 2)
	if len(lines) != 2 {
		t.Fatalf("期望折成 2 行，实际 %d 行: %v", len(lines), lines)
	}
	joined := strings.Join(lines, "")
	cleaned := strings.NewReplacer(" ", "").Replace(joined)
	want := strings.NewReplacer(" ", "").Replace(text)
	if cleaned != want {
		t.Errorf("折行丢字或改字:\n  原文 %q\n  折行 %q", want, cleaned)
	}
	for i, l := range lines {
		if n := len([]rune(l)); n > 24 { // 允许最后一行略超
			t.Errorf("第 %d 行过长（%d 字）: %q", i+1, n, l)
		}
	}
}

func TestWrapShortTextStaysOneLine(t *testing.T) {
	for _, s := range []string{"我回来了。", "signal received", ""} {
		lines := Wrap(s, 18, 2)
		if len(lines) != 1 {
			t.Errorf("短文本 %q 不应折行，实际 %v", s, lines)
		}
	}
}

func TestPlanAlignsToMeasuredAudio(t *testing.T) {
	// 核心需求：字幕必须跟着**实测**音频时长走，而不是按字数估算
	slots := []Slot{
		{ShotID: "sh001", Start: 0, Dur: 6},
		{ShotID: "sh002", Start: 6, Dur: 6},
	}
	lines := []Line{
		{ShotID: "sh001", Speaker: "narrator", Text: "深空第七年，一束信号撕开了寂静。",
			StartSec: 0.28, DurSec: 3.72},
		{ShotID: "sh002", Speaker: "信使·林", Text: "我回来了。",
			StartSec: 6.31, DurSec: 1.19},
	}
	opt := DefaultPlanOptions(config.Default())
	cues := Plan(slots, lines, opt)
	if len(cues) != 2 {
		t.Fatalf("期望 2 条字幕，实际 %d", len(cues))
	}
	// 入点必须等于实测入点（这是"字幕跟得上台词"的关键）
	if diff := cues[0].Start - 0.28; diff > 0.01 || diff < -0.01 {
		t.Errorf("第 1 条入点 %.3f，应为实测 0.28", cues[0].Start)
	}
	if diff := cues[1].Start - 6.31; diff > 0.01 || diff < -0.01 {
		t.Errorf("第 2 条入点 %.3f，应为实测 6.31", cues[1].Start)
	}
	// 显示时长必须覆盖人声并留出余量，不能短于实测人声
	if d := cues[0].End - cues[0].Start; d < 3.72 {
		t.Errorf("第 1 条字幕仅 %.2fs，短于实测人声 3.72s（会出现字幕跟不上声音的情况）", d)
	}
	// 不得越出所属镜头
	if cues[0].End > 6+0.001 {
		t.Errorf("第 1 条字幕结束于 %.3f，越出镜头（镜头 0-6s）", cues[0].End)
	}
	if cues[1].End > 12+0.001 {
		t.Errorf("第 2 条字幕结束于 %.3f，越出镜头（镜头 6-12s）", cues[1].End)
	}
}

func TestPlanRespectsCPSLimit(t *testing.T) {
	// 短镜头 + 长台词：字幕必须拉长显示时间以满足 CPS 上限，且不得越界
	slots := []Slot{{ShotID: "s1", Start: 0, Dur: 8}}
	long := "这是一句非常长的台词用来检验每秒字符数的限制是否生效以及字幕是否可读"
	lines := []Line{{ShotID: "s1", Text: long, DurSec: 2.0}}
	opt := DefaultPlanOptions(config.Default())
	cues := Plan(slots, lines, opt)
	if len(cues) != 1 {
		t.Fatalf("期望 1 条字幕，实际 %d", len(cues))
	}
	c := cues[0]
	if c.CPS > opt.MaxCPS+0.05 {
		t.Errorf("CPS %.1f 超过上限 %.1f（观众读不完）", c.CPS, opt.MaxCPS)
	}
	if c.End > 8.001 {
		t.Errorf("字幕越出镜头: %.3f > 8", c.End)
	}
}

func TestPlanNoOverlapWithinShot(t *testing.T) {
	slots := []Slot{{ShotID: "s1", Start: 0, Dur: 6}}
	lines := []Line{
		{ShotID: "s1", Text: "第一句话。", StartSec: 0.2, DurSec: 1.0},
		{ShotID: "s1", Text: "第二句话。", StartSec: 3.0, DurSec: 1.0},
	}
	cues := Plan(slots, lines, DefaultPlanOptions(config.Default()))
	if len(cues) != 2 {
		t.Fatalf("期望 2 条字幕，实际 %d", len(cues))
	}
	if cues[1].Start < cues[0].End {
		t.Errorf("同镜头内字幕重叠: 第1条结束 %.2f，第2条开始 %.2f", cues[0].End, cues[1].Start)
	}
}

func TestVerifyDetectsBlockingProblems(t *testing.T) {
	opt := DefaultPlanOptions(config.Default())
	// 0.2s 一闪而过 + 与下一条重叠 → 必须报出阻断问题
	cues := []Cue{
		{Index: 1, Start: 0, End: 0.2, Lines: []string{"太快了"}},
		{Index: 2, Start: 0.1, End: 3, Lines: []string{"重叠"}},
	}
	vs := Verify(cues, opt)
	if len(vs) == 0 {
		t.Fatal("期望报出字幕问题，实际 0 项")
	}
	if !HasBlocking(vs) {
		t.Errorf("期望存在阻断级问题（一闪而过/重叠），实际: %v", vs)
	}
}

func TestRenderSRTFormat(t *testing.T) {
	cues := []Cue{
		{Index: 1, Start: 0.28, End: 4.0, Lines: []string{"第一行", "第二行"}},
	}
	out := Render(cues)
	want := "1\n00:00:00,280 --> 00:00:04,000\n第一行\n第二行\n\n"
	if out != want {
		t.Errorf("SRT 输出不符:\n得到 %q\n期望 %q", out, want)
	}
}

func TestSRTTimeRounding(t *testing.T) {
	// 毫秒必须四舍五入而不是截断，否则字幕会比实际早退 0.5ms 以上
	if got := srtTime(1.9999); got != "00:00:02,000" {
		t.Errorf("srtTime(1.9999)=%q，期望 00:00:02,000", got)
	}
	if got := srtTime(1.0004); got != "00:00:01,000" {
		t.Errorf("srtTime(1.0004)=%q，期望 00:00:01,000", got)
	}
}

// TestEstimateSpeakSecChinese 校验中文语速估算口径（4 字/秒）。
func TestEstimateSpeakSecChinese(t *testing.T) {
	// 10 字中文 → 约 2.5s
	d := targetDuration("深空第七年一束信号撕开", 0, DefaultPlanOptions(config.Default()))
	if d < 1.5 || d > 3.5 {
		t.Errorf("10 字台词目标时长 %.2fs，期望约 2.5s", d)
	}
	// 有实测时长时必须以实测为准（+0.4s 余量）
	d2 := targetDuration("任意文本", 3.0, DefaultPlanOptions(config.Default()))
	if d2 < 3.0 || d2 > 3.6 {
		t.Errorf("有实测时长 3.0s 时应为约 3.4s，实际 %.2f", d2)
	}
}

var _ = time.Now
