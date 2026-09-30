package promspec

import (
	"strings"
	"testing"
)

func sampleKit() Kit {
	preset := PresetFor("科幻")
	return Kit{
		Style: preset.Style,
		Characters: []CharacterSpec{{
			Name: "信使·林", Token: "lin-female35-short-hair",
			Age: "35", Ethnicity: "East Asian", Build: "slim",
			Hair: "short black hair", Face: "high cheekbones, thin scar through left eyebrow",
			Skin: "weathered with visible pores", Eyes: "dark brown with catchlights",
			Distinctive: []string{"brass compass on chest strap"},
			Wardrobe:    "worn grey flight suit",
		}},
	}
}

func sampleShot() ShotSpec {
	return ShotSpec{
		ID: "sh001", SceneID: "s1", Location: "cockpit", TimeOfDay: "night",
		Description: "控制台信号灯", ActionEn: "a close-up of a blinking console light reflected in her eyes",
		ShotSize: "特写", ShotSizeEn: ShotSizeEn("特写"),
		CameraMove: "缓推", CameraMoveEn: CameraMoveEn("缓推"),
		Lens: LensFor("特写", 0),
		Light: LightSetup{
			Key: "single amber console glow", Quality: "soft directional",
			Direction: "camera-left three-quarter", Ratio: "8:1 contrast", ColorTemp: 3200,
		},
		Composition: Composition{Frame: "rule of thirds", SubjectPos: "subject on the left third"},
		DurationSec: 4.5, Characters: []string{"信使·林"},
		Beats: []Beat{{Kind: "dialogue", Speaker: "信使·林", Text: "信号源……是麦田？", AtSec: 0.3}},
	}
}

// TestShotPromptCarriesCinematicLanguage 确保提示词包含"电影感"的可执行参数，
// 而不是"cinematic, 8k, masterpiece"这类会触发 AI 学生成模式的空话。
func TestShotPromptCarriesCinematicLanguage(t *testing.T) {
	kit := sampleKit()
	p := ShotPrompt(kit.Style, sampleShot(), kit.Characters)

	must := []string{
		"T1.8",                    // 光圈：特写用大光圈浅景深（决定电影感）
		"85mm",                    // 焦段与景别匹配
		"ARRI",                    // 机型
		"Kodak",                   // 胶片/色彩科学
		"skin",                    // 皮肤质感（防塑料感）
		"pores",                   // 毛孔（防过度磨皮）
		"Aspect ratio: 2.39:1",    // 画幅
		"Avoid:",                  // 必须有负面提示词段
		"lin-female35-short-hair", // 一致性锚点必须逐字出现
	}
	for _, m := range must {
		if !strings.Contains(p, m) {
			t.Errorf("提示词缺少关键要素 %q\n---\n%s", m, p)
		}
	}
}

// TestShotPromptHasMotivatedLight 照明必须有画面内动机——
// "无来源的均匀补光"是最明显的 AI 感来源。
func TestShotPromptHasMotivatedLight(t *testing.T) {
	kit := sampleKit()
	p := ShotPrompt(kit.Style, sampleShot(), kit.Characters)
	if !strings.Contains(p, "Lighting:") {
		t.Fatal("提示词缺少照明段落")
	}
	if !strings.Contains(p, "amber console glow") {
		t.Error("照明动机未被写入提示词")
	}
}

// TestNegativePromptCoversAITells 负面提示词必须覆盖典型 AI 破绽。
func TestNegativePromptCoversAITells(t *testing.T) {
	neg := NegativePrompt()
	for _, want := range []string{"extra fingers", "plastic skin", "watermark", "3d render", "uncanny valley"} {
		if !strings.Contains(neg, want) {
			t.Errorf("负面提示词缺少 %q", want)
		}
	}
	// 分组裁剪：空镜不需要 anatomy 组
	empty := ShotSpec{ID: "sh009"}
	groups := NegativeGroupsFor(empty)
	for _, g := range groups {
		if g == "anatomy" || g == "face" {
			t.Errorf("无人物的镜头不应强调 %s 组（浪费提示词权重）", g)
		}
	}
}

// TestVideoPromptRestrainsMotion AI 视频最容易出戏的是"运动过火"与"人脸漂移"。
func TestVideoPromptRestrainsMotion(t *testing.T) {
	kit := sampleKit()
	vp := VideoPrompt(kit.Style, sampleShot())
	for _, want := range []string{"locked", "physically plausible", "identity stay locked", "no morphing"} {
		if !strings.Contains(strings.ToLower(vp), strings.ToLower(want)) {
			t.Errorf("运动提示词缺少克制约束 %q\n---\n%s", want, vp)
		}
	}
}

// TestAnchorTextDeterministic 同一角色的锚点描述必须逐字稳定（描述漂移=换脸）。
func TestAnchorTextDeterministic(t *testing.T) {
	kit := sampleKit()
	a := kit.Characters[0].AnchorText()
	b := kit.Characters[0].AnchorText()
	if a != b {
		t.Error("同一角色的锚点描述不稳定")
	}
	if !strings.Contains(a, "high cheekbones") {
		t.Errorf("锚点未包含关键面部特征: %s", a)
	}
}

// TestLensMatchesShotSize 焦段必须与景别匹配（大远景 24mm、特写 85mm 是摄影常识）。
func TestLensMatchesShotSize(t *testing.T) {
	wide := LensFor("大远景", 0)
	closeUp := LensFor("特写", 0)
	if wide.MM >= closeUp.MM {
		t.Errorf("大远景焦段(%d)应小于特写焦段(%d)", wide.MM, closeUp.MM)
	}
	if closeUp.MM != 85 {
		t.Errorf("特写焦段应为 85mm，实际 %d", closeUp.MM)
	}
}

// TestPresetMatching 风格预设必须能按中文关键词命中。
func TestPresetMatching(t *testing.T) {
	cases := map[string]string{
		"一部科幻短片":  "scifi",
		"温情家庭故事":  "warm",
		"悬疑犯罪惊悚":  "noir",
		"纪录片风格":   "documentary",
		"现代写实剧情片": "naturalism",
	}
	for hint, want := range cases {
		if got := PresetFor(hint).Name; got != want {
			t.Errorf("PresetFor(%q)=%q，期望 %q", hint, got, want)
		}
	}
}

// TestRefPromptLocksIdentity 参考帧提示词必须含"同一人"与"平面光"要求——
// 参考帧是整片一致性的地基，光照不一致会让参考图锁不住脸。
func TestRefPromptLocksIdentity(t *testing.T) {
	kit := sampleKit()
	p := RefPrompt(kit.Style, kit.Characters[0])
	for _, want := range []string{"SAME person", "neutral studio lighting", "no beauty filter", "full-body"} {
		if !strings.Contains(p, want) {
			t.Errorf("参考帧提示词缺少 %q\n---\n%s", want, p)
		}
	}
}

// TestDialogueBeatsSortedAndFiltered 台词节拍必须按时间排序且过滤空台词。
func TestDialogueBeatsSortedAndFiltered(t *testing.T) {
	s := ShotSpec{
		ID: "sh001",
		Beats: []Beat{
			{Kind: "dialogue", Speaker: "A", Text: "第二句", AtSec: 2},
			{Kind: "action", Text: "动作", AtSec: 0},
			{Kind: "dialogue", Speaker: "A", Text: "第一句", AtSec: 1},
			{Kind: "dialogue", Speaker: "A", Text: "   ", AtSec: 3},
		},
	}
	ds := s.DialogueBeats()
	if len(ds) != 2 {
		t.Fatalf("期望 2 条有效台词，实际 %d", len(ds))
	}
	if ds[0].Text != "第一句" || ds[1].Text != "第二句" {
		t.Errorf("台词未按时间排序: %v", ds)
	}
}

// TestIsNarrator 旁白识别要覆盖常见写法。
func TestIsNarrator(t *testing.T) {
	for _, s := range []string{"narrator", "NARRATOR", "旁白", "画外音", "VO"} {
		if !IsNarrator(s) {
			t.Errorf("%q 应被识别为旁白", s)
		}
	}
	for _, s := range []string{"信使·林", "系统·OS", ""} {
		if IsNarrator(s) {
			t.Errorf("%q 不应被识别为旁白", s)
		}
	}
}

// TestMovementAmplitude 运镜 → vidu 运动幅度的映射必须服从分镜意图：
// 静态/缓动 small，跟踪/摇移 medium，大空间调度 large，未知回落 auto。
func TestMovementAmplitude(t *testing.T) {
	cases := map[string]string{
		"固定":   "small",
		"缓推":   "small",
		"缓拉":   "small",
		"横移":   "medium",
		"摇":    "medium",
		"手持跟随": "medium",
		"航拍":   "large",
		"升降":   "large",
		"环绕":   "large",
		" 环绕 ": "large", // 带空白也应命中
		"未知运镜": "auto",
		"":     "auto",
	}
	for in, want := range cases {
		if got := MovementAmplitude(in); got != want {
			t.Errorf("MovementAmplitude(%q)=%q, want %q", in, got, want)
		}
	}
}
