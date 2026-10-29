package narrate

import (
	"strings"
	"testing"

	"aistudio/internal/config"
	"aistudio/internal/mouth"
	"aistudio/internal/promspec"
)

// diff 浮点容差比较：时间轴运算是 64 位浮点，精确相等不可靠。
func diff(a, b float64) bool {
	d := a - b
	return d > 1e-9 || d < -1e-9
}

// TestLineFingerprint 指纹必须对"影响合成结果的每个要素"敏感——
// 缓存键漏掉任何一个要素，就会拿旧音频冒充新配置（历史上按行序号
// 缓存造成的静默错配）。行序号不参与指纹：台本插句/调序不影响命中。
func TestLineFingerprint(t *testing.T) {
	base := lineFingerprint("seed-tts-2.0", "雷霆万钧！", "voice_a", 1.0, "angry")
	variants := []struct {
		name  string
		model string
		text  string
		voice string
		emo   string
		speed float64
	}{
		{"换模型", "seed-tts-1.0", "雷霆万钧！", "voice_a", "angry", 1.0},
		{"改文本", "seed-tts-2.0", "雷霆万钧？", "voice_a", "angry", 1.0},
		{"换音色", "seed-tts-2.0", "雷霆万钧！", "voice_b", "angry", 1.0},
		{"换情绪", "seed-tts-2.0", "雷霆万钧！", "voice_a", "sad", 1.0},
		{"情绪清空", "seed-tts-2.0", "雷霆万钧！", "voice_a", "", 1.0},
		{"调语速", "seed-tts-2.0", "雷霆万钧！", "voice_a", "angry", 0.95},
	}
	for _, v := range variants {
		if got := lineFingerprint(v.model, v.text, v.voice, v.speed, v.emo); got == base {
			t.Errorf("%s：指纹不应与基准相同", v.name)
		}
	}
	if again := lineFingerprint("seed-tts-2.0", "雷霆万钧！", "voice_a", 1.0, "angry"); again != base {
		t.Errorf("同参数两次指纹应一致（缓存命中的前提）：%s vs %s", again, base)
	}
}

// TestJCutLead 声音提前量的三重约束：不越前镜起点、装得进本镜槽位、
// 太小不如不做。
func TestJCutLead(t *testing.T) {
	slot := ShotSlot{Start: 10.0, Duration: 4.0}
	prev := ShotSlot{Start: 5.0, Duration: 5.0}

	if got := jcutLead(&Line{MeasuredSec: 2.0}, slot, prev, 0.3); got != 0.3 {
		t.Errorf("正常情况应取上限 0.3，got %v", got)
	}
	// 提前 0.3 会越过前镜起点（prev.Start=9.85）→ 钳到 0.15
	tightPrev := ShotSlot{Start: 9.85, Duration: 0.15}
	if got := jcutLead(&Line{MeasuredSec: 2.0}, slot, tightPrev, 0.3); diff(got, 0.15) {
		t.Errorf("越过前镜起点应钳到 0.15，got %v", got)
	}
	// 台词提前后总占用装不进本镜槽位：lead = Duration - MeasuredSec = 0.2
	if got := jcutLead(&Line{MeasuredSec: 3.8}, slot, prev, 0.3); diff(got, 0.2) {
		t.Errorf("超长台词应钳到 0.2，got %v", got)
	}
	// 提前量太小没有意义
	if got := jcutLead(&Line{MeasuredSec: 2.0}, slot, prev, 0.03); got != 0 {
		t.Errorf("<0.05 的提前量应放弃，got %v", got)
	}
}

// TestLayoutLine 排麦三规则：口型窗口锚定入点并放弃 J-cut；
// 与上一句冲突时顶到游标之后（绝不叠音）；超槽如实标注。
func TestLayoutLine(t *testing.T) {
	cfg := &config.Config{}
	cfg.Audio.LeadInSec = 0.18
	cfg.Audio.LineGapSec = 0.28

	// 1) 口型窗口：入点=窗口起点，J-cut 被放弃（声音已锚定在镜头内）
	win := &mouth.Window{Start: 1.0, End: 3.0, Confidence: 0.5, Frames: 12, Speaking: 5}
	slot := ShotSlot{ShotID: "s1", Start: 10.0, Duration: 5.0, SpeakWin: win}
	l := &Line{MeasuredSec: 1.5}
	cursor := layoutLine(l, slot, 0, cfg, 0.3)
	if l.StartSec != 1.0 || l.AbsStartSec != 11.0 {
		t.Errorf("窗口锚定：StartSec=%v AbsStartSec=%v，want 1.0/11.0", l.StartSec, l.AbsStartSec)
	}
	if l.JCutSec != 0 {
		t.Errorf("窗口模式下 J-cut 应放弃，got %v", l.JCutSec)
	}
	if cursor != 12.5 {
		t.Errorf("游标应推进到句尾 12.5，got %v", cursor)
	}

	// 2) 排麦：入点撞上游标 → 顶到 cursor+gap，绝不叠音
	l2 := &Line{MeasuredSec: 1.0}
	layoutLine(l2, slot, 11.5, cfg, 0)
	if l2.AbsStartSec != 11.78 {
		t.Errorf("窗口起点 11.0 撞上游标，应顶到 11.78，got %v", l2.AbsStartSec)
	}

	// 3) 无窗口：入点=镜头起点+铅入
	l3 := &Line{MeasuredSec: 1.0}
	plain := ShotSlot{ShotID: "s2", Start: 20.0, Duration: 5.0}
	layoutLine(l3, plain, 0, cfg, 0)
	if diff := l3.StartSec - 0.18; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("无窗口应按铅入 0.18s，got %v", l3.StartSec)
	}

	// 4) 超槽：溢出如实标注
	l4 := &Line{MeasuredSec: 6.0}
	layoutLine(l4, plain, 0, cfg, 0)
	if !l4.Overflow {
		t.Error("6s > 槽位 5s 应标记 Overflow")
	}
}

// TestWindowFitBudget 窗长适配回归测试（钉住旧 bug：条件方向写反使
// 分支永远不触发）。2.2s 音频配 2.0s 窗口，旧条件 2.0/2.2=0.91 >= 1.15
// 为假（不触发），新逻辑必须触发。
func TestWindowFitBudget(t *testing.T) {
	w := &mouth.Window{Start: 1.0, End: 3.0, Confidence: 0.5, Frames: 12, Speaking: 5} // win=2.0s

	if budget, ok := windowFitBudget(2.2, w, 1.15); !ok || budget != 2.0 {
		t.Errorf("2.2s 音频应适配到 2.0s 窗口（加速 1.1x），got %v/%v", budget, ok)
	}
	if _, ok := windowFitBudget(1.5, w, 1.15); ok {
		t.Error("音频短于窗口时无需压缩，不应启用窗长适配")
	}
	if _, ok := windowFitBudget(2.4, w, 1.15); ok {
		t.Error("2.4/2.0=1.2 超加速上限，不应适配（宁可溢出也不花栗鼠）")
	}
	if _, ok := windowFitBudget(2.2, nil, 1.15); ok {
		t.Error("nil 窗口不应适配")
	}
	bad := &mouth.Window{Start: 1.0, End: 1.3, Confidence: 0.1, Frames: 12, Speaking: 1}
	if _, ok := windowFitBudget(2.2, bad, 1.15); ok {
		t.Error("低质窗口（过短+低置信度）不应适配")
	}
}

// TestValidateVoiceoverSpeakers 台本说话人 × 镜头角色的一致性门禁：
// 错配必须报错（历史上 AI 把杨戬的台词挂到猪八戒的镜头，成片"嘴配错声"）；
// 旁白、双向包含匹配、空角色表跳过。
func TestValidateVoiceoverSpeakers(t *testing.T) {
	kit := &promspec.Kit{
		ShotList: []promspec.ShotSpec{
			{ID: "sh007", Characters: []string{"猪八戒"}},
			{ID: "sh005", Characters: []string{"杨戬"}},
			{ID: "sh099"}, // 角色表为空：无法校验，跳过
		},
	}
	// 错配：杨戬的台词挂在猪八戒的镜头 → 报错且信息含镜头与角色
	swapped := &voiceoverDoc{Narration: []narrationLine{
		{ShotID: "sh007", Speaker: "杨戬", Text: "天蓬元帅触犯天规。"},
	}}
	err := validateVoiceoverSpeakers(kit, swapped)
	if err == nil {
		t.Fatal("说话人错配应报错")
	}
	for _, want := range []string{"sh007", "杨戬", "猪八戒"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q：%v", want, err)
		}
	}
	// 正确归属 + 旁白 + 异写 + 空角色表 → 通过
	ok := &voiceoverDoc{Narration: []narrationLine{
		{ShotID: "sh007", Speaker: "猪八戒", Text: "只记得俺贪睡好吃？"},
		{ShotID: "sh005", Speaker: "narrator", Text: "杀意，一闪而过。"},
		{ShotID: "sh005", Speaker: "杨戬（青年）", Text: "异写角色应包含匹配通过"},
		{ShotID: "sh099", Speaker: "路人甲", Text: "空角色表跳过"},
	}}
	if err := validateVoiceoverSpeakers(kit, ok); err != nil {
		t.Errorf("合法台本不应报错：%v", err)
	}
}

// TestCheckTTSDuration 合成产物合理性：时长须与文本体量匹配
// （防截断/串音），mock 模式跳过。
func TestCheckTTSDuration(t *testing.T) {
	if err := checkTTSDuration(strings.Repeat("字", 12), 3.0, false); err != nil {
		t.Errorf("12 字≈3s 应通过，got %v", err)
	}
	if err := checkTTSDuration(strings.Repeat("字", 40), 40.0, false); err == nil {
		t.Error("40 字应约 10s，40s（比值 4.0）应报错")
	}
	if err := checkTTSDuration(strings.Repeat("字", 40), 3.0, false); err == nil {
		t.Error("40 字只出 3s（比值 0.3）疑似截断，应报错")
	}
	if err := checkTTSDuration("任何文本", 999, true); err != nil {
		t.Errorf("mock 模式不校验，got %v", err)
	}
}
