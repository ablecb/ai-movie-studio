package skills

import (
	"strings"
	"testing"

	"aistudio/internal/skill"
)

// TestAssignDistinctVoices 音色分配是"一开口就知道是谁"的硬约束：
// 模型选得好的保留，缺失/重复/越池的确定性补齐，tongtong（旁白默认音色）最后分配。
func TestAssignDistinctVoices(t *testing.T) {
	pool := []string{"tongtong", "chuichui", "xiaochen", "jam"}

	// 模型选择全部合法：原样保留
	chars := []Character{
		{Name: "A", TTSVoice: "jam"}, {Name: "B", TTSVoice: "xiaochen"},
	}
	if notes := assignDistinctVoices(chars, pool); len(notes) != 0 {
		t.Fatalf("合法选择不应产生告警: %v", notes)
	}
	if chars[0].TTSVoice != "jam" || chars[1].TTSVoice != "xiaochen" {
		t.Fatalf("合法选择被改动: %+v", chars)
	}

	// 模型重复/越池/缺失：按池序补齐，且 tongtong 排最后（留给旁白）
	chars = []Character{
		{Name: "A", TTSVoice: "chuichui"},
		{Name: "B", TTSVoice: "chuichui"},    // 重复 → 补 xiaochen
		{Name: "C", TTSVoice: "not-a-voice"}, // 越池 → 补 jam
		{Name: "D"},                          // 缺失 → 补 tongtong（池里只剩它）
	}
	notes := assignDistinctVoices(chars, pool)
	for _, c := range chars {
		if c.TTSVoice == "" {
			t.Fatalf("角色 %s 未分到音色: %+v", c.Name, c)
		}
	}
	got := map[string]string{}
	for _, c := range chars {
		got[c.Name] = c.TTSVoice
	}
	if got["A"] != "chuichui" || got["B"] != "xiaochen" || got["C"] != "jam" || got["D"] != "tongtong" {
		t.Fatalf("分配结果不符合预期: %v", got)
	}

	// 池小于角色数：超出的清空回落（narrate 变调兜底），并显式告警
	chars = []Character{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}, {Name: "E"}}
	notes = assignDistinctVoices(chars, pool)
	if len(notes) == 0 {
		t.Fatal("池不足时应有告警")
	}
	distinct := map[string]int{}
	for _, c := range chars {
		if c.TTSVoice != "" {
			distinct[c.TTSVoice]++
			if distinct[c.TTSVoice] > 1 {
				t.Fatalf("音色 %s 被重复分配（应清空回落）: %+v", c.TTSVoice, chars)
			}
		}
	}
	if len(distinct) != len(pool) {
		t.Fatalf("应恰好用尽音色池: %v", distinct)
	}
	if chars[4].TTSVoice != "" {
		t.Fatalf("第 5 个角色应无音色可分（空 = 回落默认+变调）: %+v", chars[4])
	}
}

// TestVoicePoolOf 工作流参数支持逗号字符串与数组两种写法；缺失回落默认池。
func TestVoicePoolOf(t *testing.T) {
	ctx := &skill.Context{Params: map[string]any{}}
	if got := voicePoolOf(ctx); len(got) != 4 || got[0] != "tongtong" {
		t.Fatalf("缺省应回落默认池: %v", got)
	}
	ctx.Params["voice_pool"] = "a, b,,c"
	if got := voicePoolOf(ctx); len(got) != 3 || got[1] != "b" {
		t.Fatalf("字符串解析错误: %v", got)
	}
	ctx.Params["voice_pool"] = []any{"x", "y"}
	if got := voicePoolOf(ctx); len(got) != 2 || got[0] != "x" {
		t.Fatalf("数组解析错误: %v", got)
	}
	ctx.Params["voice_pool"] = " , "
	if got := voicePoolOf(ctx); len(got) != 4 {
		t.Fatalf("空参数应回落默认池: %v", got)
	}
}

// mock 角色经过分配后必须人手一个音色（端到端前的快速回归）。
func TestMockCharactersGetDistinctVoices(t *testing.T) {
	chars := mockCharacters().Characters
	// 复制一份再分配（mockCharacters 每次新建，直接传也行）
	if notes := assignDistinctVoices(chars, defaultVoicePool); len(notes) != 0 {
		t.Fatalf("默认池应覆盖 mock 角色数: %v", notes)
	}
	seen := map[string]bool{}
	for _, c := range chars {
		if c.TTSVoice == "" {
			t.Fatalf("角色 %s 没有音色", c.Name)
		}
		if seen[c.TTSVoice] {
			t.Fatalf("音色 %s 被重复分配", c.TTSVoice)
		}
		seen[c.TTSVoice] = true
		if !strings.Contains(strings.Join(defaultVoicePool, ","), c.TTSVoice) {
			t.Fatalf("音色 %s 不在池内", c.TTSVoice)
		}
	}
}
