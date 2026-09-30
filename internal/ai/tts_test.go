package ai

import (
	"strings"
	"testing"
)

// TestNormalizeTTSText 校验合成前文本规范化：TTS 的停顿/语调完全由标点驱动，
// 台本里的符号、半角标点、缺句尾标点都会直接变成"平、赶、怪"的机器感。
func TestNormalizeTTSText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"句尾补终结标点", "夜色像一块浸了水的绒布", "夜色像一块浸了水的绒布。"},
		{"汉字后半角标点转全角", "等等,你听到了吗?是的,警报响了!", "等等，你听到了吗？是的，警报响了！"},
		{"去舞台提示并清理悬空停顿", "（压低声音）别动。听。（远处传来金属声）", "别动。听。"},
		{"小数点不受影响", "信号频率是3.5赫兹,请确认。", "信号频率是3.5赫兹，请确认。"},
		{"英文缩写不受影响", "服务器down了,Mr.Smith", "服务器down了，Mr.Smith。"},
		{"去排版符号", "第3章#雨夜**追踪**", "第3章雨夜追踪。"},
		{"省略号归一", "怎么会这样………不可能……", "怎么会这样……不可能……"},
		{"换行按停顿处理", "第一句\n第二句", "第一句，第二句。"},
		{"保留中文引号内文字", "他说“快走”。", "他说快走。"},
		{"只有舞台提示→空", "   （无台词）  ", ""},
	}
	for _, c := range cases {
		got := normalizeTTSText(c.in)
		if got != c.want {
			t.Errorf("%s:\n  in:  %q\n  got: %q\n  want:%q", c.name, c.in, got, c.want)
		}
	}
}

// TestNormalizeTTSTextKeepsContent 保证规范化不丢正文。
func TestNormalizeTTSTextKeepsContent(t *testing.T) {
	in := "信号消失了，整座城市陷入黑暗，剩下的人只能等待。"
	if got := normalizeTTSText(in); strings.ReplaceAll(got, "，", "，") != in {
		t.Errorf("正文被改写: %q → %q", in, got)
	}
}
