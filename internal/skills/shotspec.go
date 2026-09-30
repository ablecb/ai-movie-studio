package skills

import (
	"fmt"
	"strings"

	"aistudio/internal/promspec"
	"aistudio/internal/skill"
	"aistudio/internal/srt"
)

// ---------------- S5 生成规格（唯一真源：台词/画面/节拍在此绑定） ----------------
//
// 这一步是"台词对得上、字幕不出错"的结构性保证：
//
//	voiceover.json 是台词的唯一真源，shotspec 把每一句台词绑定到具体镜头的具体节拍，
//	后续配音、字幕、剪辑全部读同一份绑定结果。原实现里台词分散在 script（剧本对白）、
//	storyboard（镜头）、voiceover（台本）三处，任一处漂移就会出现"字幕和声音对不上"。
//
// 同时，机器视觉语言由代码（promspec）编译，模型只负责创作判断（景别/运镜/光位/情绪），
// 提示词质量因此有确定的下限，而不是每次运行都靠运气。

// shotSpecOut 模型产出的创作判断（不含可直接编译的提示词文本）。
type shotSpecOut struct {
	Style      styleOut       `json:"style"`
	Characters []characterOut `json:"characters"`
	Shots      []shotOne      `json:"shots"`
}

type styleOut struct {
	Name      string `json:"name"`
	Aspect    string `json:"aspect"`
	TimeOfDay string `json:"time_of_day"`
	Palette   string `json:"palette"`
}

type characterOut struct {
	Name        string   `json:"name"`
	Age         string   `json:"age"`
	Ethnicity   string   `json:"ethnicity"`
	Build       string   `json:"build"`
	Hair        string   `json:"hair"`
	Face        string   `json:"face"`
	Skin        string   `json:"skin"`
	Eyes        string   `json:"eyes"`
	Distinctive []string `json:"distinctive"`
	Wardrobe    string   `json:"wardrobe"`
	Voice       string   `json:"voice"`
	TTSVoice    string   `json:"tts_voice"`
	Emotion     string   `json:"emotion"`
}

type shotOne struct {
	ID             string   `json:"id"`
	ActionEn       string   `json:"action_en"`
	ShotSizeEn     string   `json:"shot_size_en"`
	CameraMoveEn   string   `json:"camera_move_en"`
	LensMM         int      `json:"lens_mm"`
	TStop          string   `json:"t_stop"`
	LightKey       string   `json:"light_key"`
	LightQuality   string   `json:"light_quality"`
	LightDirection string   `json:"light_direction"`
	LightRatio     string   `json:"light_ratio"`
	ColorTemp      float64  `json:"color_temp"`
	Composition    string   `json:"composition"`
	SubjectPos     string   `json:"subject_position"`
	Emotion        string   `json:"emotion"`
	Note           string   `json:"note"`
	Characters     []string `json:"characters"`
}

type shotSpecSkill struct{}

func (shotSpecSkill) Name() string { return "shotspec" }
func (shotSpecSkill) Description() string {
	return "生成规格：镜头参数/角色锚点/台词绑定（跨镜头一致性与字幕对齐的地基）"
}

func (shotSpecSkill) Run(ctx *skill.Context) (any, error) {
	var script Script
	var sb Storyboard
	var bible CharacterBible
	var vo VoiceoverScript
	if err := ctx.DepOf("script", &script); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("storyboard", &sb); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("characters", &bible); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("voiceover", &vo); err != nil {
		return nil, err
	}

	// 风格：优先由剧本 style 字段命中预设（确定性），保证整片视觉统一
	preset := promspec.PresetFor(script.Style + " " + script.Genre + " " + ctx.Brief)
	style := preset.Style

	out := &shotSpecOut{}
	if ctx.AI.Mock() {
		out = mockShotSpec(bible, sb)
	} else {
		system := filmBase + `
你是摄影指导兼视觉规格撰写者。你的输出不是最终提示词，而是**创作判断**：景别、运镜、光学参数、
照明方案、构图、情绪、以及每个镜头的表演节拍。提示词将由系统根据你的判断按固定模板编译。

schema：
{"style":{"name":"整体视觉风格名","aspect":"画幅如 2.39:1","time_of_day":"主时段","palette":"色彩方案"},
 "characters":[{"name":"角色名","age":"年龄","ethnicity":"族裔","build":"体态","hair":"发型发色","face":"骨相与五官细节（决定是否同一个人）","skin":"皮肤质感（必须写毛孔/纹理等真实细节）","eyes":"眼睛特征","distinctive":["标志性特征如疤痕/痣/饰品"],"wardrobe":"服装","voice":"音色与语速","emotion":"本片中的主要情绪基调"}],
 "shots":[{"id":"sh001","action_en":"英文画面与表演描述，具体到可拍摄","shot_size_en":"英文景别描述","camera_move_en":"英文运镜描述","lens_mm":50,"t_stop":"T2.0","light_key":"主光动机（画面内的光源）","light_quality":"soft|hard|diffused","light_direction":"光位如 camera-left three-quarter back","light_ratio":"光比如 8:1","color_temp":3200,"composition":"构图规则","subject_position":"主体位置","emotion":"情绪基调","note":"衔接与连续性说明","characters":["出场角色名"]}]}

硬性要求：
1. shots 必须与给定分镜**逐个对应**（相同 id、相同顺序、数量一致），不得增删；
2. characters 必须覆盖角色圣经中所有角色，且 appearance 细节比圣经更具体（这是同一个人跨镜头不变形的依据）；
3. 照明必须有明确的画面内动机（窗户/台灯/屏幕/火光），禁止"无来源的均匀补光"——那是最明显的 AI 感；
4. lens_mm 按景别选择（大远景 24 / 远景 32 / 全景 40 / 中景 50 / 近景 65 / 特写 85 / 大特写 100）；
5. 只输出 JSON。`
		user := "创作意图：" + ctx.Brief +
			"\n剧本：" + marshal(script) +
			"\n分镜表（shots 必须与之一一对应）：" + marshal(sb) +
			"\n角色圣经（characters 必须覆盖）：" + marshal(bible) +
			"\n建议整体风格：" + preset.Name + "（" + style.Name + "）"
		if err := callJSON(ctx, "shotspec", system, user, out); err != nil {
			return nil, err
		}
	}

	// ---- 组装 Kit：模型给判断，代码给摄影语言 ----
	kit := &promspec.Kit{Style: style, Version: 1}
	if out.Style.Palette != "" {
		kit.Style.Grade.Palette = out.Style.Palette
	}
	if out.Style.Aspect != "" {
		kit.Style.Aspect = out.Style.Aspect
	}
	if out.Style.TimeOfDay != "" {
		kit.Style.TimeOfDay = out.Style.TimeOfDay
	}
	if out.Style.Name != "" {
		kit.Style.Name = out.Style.Name
	}

	// 角色：以角色圣经为骨架，模型细节优先，缺失则回落圣经字段
	for _, c := range bible.Characters {
		spec := promspec.CharacterSpec{
			Name: c.Name, Token: strings.TrimSpace(c.ConsistencyTokens),
			Wardrobe: c.Wardrobe, Voice: c.Voice, TTSVoice: c.TTSVoice,
		}
		if o, ok := findCharacter(out.Characters, c.Name); ok {
			spec.Age = o.Age
			spec.Ethnicity = o.Ethnicity
			spec.Build = o.Build
			spec.Hair = o.Hair
			spec.Face = o.Face
			spec.Skin = o.Skin
			spec.Eyes = o.Eyes
			spec.Distinctive = o.Distinctive
			spec.Emotion = o.Emotion
			if strings.TrimSpace(o.Wardrobe) != "" {
				spec.Wardrobe = o.Wardrobe
			}
			if strings.TrimSpace(o.Voice) != "" {
				spec.Voice = o.Voice
			}
			if strings.TrimSpace(o.TTSVoice) != "" {
				spec.TTSVoice = o.TTSVoice
			}
		}
		// 兜底：模型没给细节时用圣经的 appearance 填充，保证锚点非空
		if spec.Face == "" {
			spec.Face = c.Appearance
		}
		if spec.Skin == "" {
			spec.Skin = "natural skin with visible pores and fine texture"
		}
		if len(spec.Distinctive) == 0 {
			spec.Distinctive = []string{"consistent facial structure across all shots"}
		}
		if spec.Age == "" {
			spec.Age = "adult"
		}
		if spec.Ethnicity == "" {
			spec.Ethnicity = "East Asian"
		}
		kit.Characters = append(kit.Characters, spec)
	}

	// 台词按镜头归组（voiceover 是唯一真源）
	linesByShot := map[string][]NarrationLine{}
	for _, n := range vo.Narration {
		linesByShot[n.ShotID] = append(linesByShot[n.ShotID], n)
	}

	for _, sh := range sb.Shots {
		o, _ := findShot(out.Shots, sh.ID)
		spec := promspec.ShotSpec{
			ID: sh.ID, SceneID: sh.SceneID, Location: sh.Location,
			Description: sh.Description, ShotSize: sh.ShotSize, CameraMove: sh.CameraMove,
			DurationSec: sh.DurationSec, Characters: append([]string(nil), sh.Characters...),
		}
		spec.ShotSizeEn = promspec.ShotSizeEn(sh.ShotSize)
		spec.CameraMoveEn = promspec.CameraMoveEn(sh.CameraMove)
		spec.Lens = promspec.LensFor(sh.ShotSize, 0)
		spec.TimeOfDay = kit.Style.TimeOfDay

		if o.ID != "" {
			if strings.TrimSpace(o.ActionEn) != "" {
				spec.ActionEn = o.ActionEn
			}
			if strings.TrimSpace(o.ShotSizeEn) != "" {
				spec.ShotSizeEn = o.ShotSizeEn
			}
			if strings.TrimSpace(o.CameraMoveEn) != "" {
				spec.CameraMoveEn = o.CameraMoveEn
			}
			if o.LensMM > 0 {
				spec.Lens.MM = o.LensMM
			}
			if strings.TrimSpace(o.TStop) != "" {
				spec.Lens.TStop = o.TStop
			}
			spec.Light = promspec.LightSetup{
				Key: o.LightKey, Quality: o.LightQuality, Direction: o.LightDirection,
				Ratio: o.LightRatio, ColorTemp: o.ColorTemp,
			}
			spec.Composition = promspec.Composition{
				Frame: o.Composition, SubjectPos: o.SubjectPos,
				Headroom: "normal headroom, no crop on face or hands",
			}
			spec.Emotion = o.Emotion
			spec.Continuity = o.Note
			if len(o.Characters) > 0 {
				spec.Characters = intersect(o.Characters, sh.Characters, bible)
			}
		}
		if spec.ActionEn == "" {
			spec.ActionEn = sh.Description
		}
		if spec.Light.Key == "" {
			// 没有动机光源是最典型的 AI 感，必须补一个具体动机
			spec.Light.Key = "single motivated source from a visible practical in frame"
			spec.Light.Quality = "soft directional"
			spec.Light.Direction = "three-quarter back"
			spec.Light.Ratio = "6:1 contrast"
		}
		if spec.Composition.Frame == "" {
			spec.Composition.Frame = "balanced cinematic framing, rule of thirds"
			spec.Composition.SubjectPos = "subject on the left third"
		}

		// ---- 台词绑定：把台本逐句挂到镜头节拍上（字幕与配音的唯一依据）----
		var dialogueSec float64
		lines := linesByShot[sh.ID]
		at := 0.0
		for _, l := range lines {
			kind := "dialogue"
			if isNarrator(l.Speaker) {
				kind = "narration"
			}
			sec := srt.EstimateSpeakSec(l.Text)
			dialogueSec += sec
			spec.Beats = append(spec.Beats, promspec.Beat{
				Kind: kind, Speaker: l.Speaker, Text: l.Text,
				AtSec: at, Emotion: l.Emotion,
			})
			// 台词在镜头内的排布：留出入口LeadIn，避免"话音未落画面已切"
			at += sec + 0.25
			if at > sh.DurationSec {
				at = sh.DurationSec
			}
		}
		// 无台词的镜头补一个动作节拍，保证 video_prompt 里有可执行的表演描述
		if len(spec.Beats) == 0 {
			spec.Beats = append(spec.Beats, promspec.Beat{
				Kind: "action", Text: spec.ActionEn, AtSec: 0, Emotion: spec.Emotion,
			})
		}
		spec.DialogueSec = dialogueSec

		// 优先级与风险：决定渲染顺序（先渲关键/高风险镜头，尽早发现问题）
		spec.Priority = priorityOf(spec, script)
		spec.Risk = riskOf(spec)
		kit.ShotList = append(kit.ShotList, spec)
	}

	// ---- 校验：这是"字幕与台词对得上"的确定性保障 ----
	if err := validateShotSpec(kit, sb, vo); err != nil {
		return nil, err
	}
	return kit, nil
}

// validateShotSpec 校验生成规格。任何一条不通过都会带着具体修复指令重跑，
// 而不是把问题带到渲染阶段（渲染是付费环节，错误代价高得多）。
func validateShotSpec(kit *promspec.Kit, sb Storyboard, vo VoiceoverScript) error {
	if len(kit.ShotList) != len(sb.Shots) {
		return fmt.Errorf("产出校验失败: 生成规格镜头数(%d)与分镜数(%d)不一致，必须一一对应",
			len(kit.ShotList), len(sb.Shots))
	}
	for i, s := range kit.ShotList {
		if s.ID != sb.Shots[i].ID {
			return fmt.Errorf("产出校验失败: 生成规格第 %d 个镜头 id=%q 与分镜 id=%q 错位，必须严格按分镜顺序",
				i+1, s.ID, sb.Shots[i].ID)
		}
		if strings.TrimSpace(s.ActionEn) == "" {
			return fmt.Errorf("产出校验失败: 镜头 %s 缺少 action_en 画面描述，无法生成", s.ID)
		}
		if strings.TrimSpace(s.Light.Key) == "" {
			return fmt.Errorf("产出校验失败: 镜头 %s 缺少照明动机（light_key）；无动机光源是最明显的 AI 感，必须写明画面内光源", s.ID)
		}
		if s.Lens.MM <= 0 {
			return fmt.Errorf("产出校验失败: 镜头 %s 缺少有效焦段（lens_mm）", s.ID)
		}
		// 台词累计时长不得超过镜头时长太多（超出会导致配音溢出、字幕越界）
		if s.DialogueSec > s.DurationSec*1.15 {
			return fmt.Errorf("产出校验失败: 镜头 %s 台词约 %.1fs 超过镜头时长 %.1fs，请精简台词或延长镜头",
				s.ID, s.DialogueSec, s.DurationSec)
		}
		// 出场角色必须都在角色表里（否则提示词里没有锚点，跨镜头必然换脸）
		for _, name := range s.Characters {
			if _, ok := kit.CharOf(name); !ok {
				return fmt.Errorf("产出校验失败: 镜头 %s 出场角色 %q 不在角色表中，角色一致性无法保证，请补充角色条目", s.ID, name)
			}
		}
	}
	// 台词逐句核对：台本里每一句都必须被绑定到某个镜头（漏绑=字幕丢句）
	bound := map[string]int{}
	for _, s := range kit.ShotList {
		for _, b := range s.Beats {
			if b.Kind == "dialogue" || b.Kind == "narration" {
				bound[strings.TrimSpace(b.Text)]++
			}
		}
	}
	for _, n := range vo.Narration {
		t := strings.TrimSpace(n.Text)
		if bound[t] == 0 {
			return fmt.Errorf("产出校验失败: 台本台词 %q（镜头 %s）没有被绑定到任何镜头节拍，字幕会丢失该句", t, n.ShotID)
		}
		bound[t]--
	}
	return nil
}

// priorityOf 关键镜头判定：有台词的、情绪强烈的、包含主角的镜头优先渲。
func priorityOf(s promspec.ShotSpec, script Script) int {
	p := 1
	if len(s.SpeakingCharacters()) > 0 {
		p += 3
	}
	if len(s.DialogueBeats()) > 0 {
		p += 2
	}
	if s.ShotSize == "特写" || s.ShotSize == "大特写" {
		p += 2 // 特写最容易暴露 AI 痕迹，先渲先验
	}
	if len(s.Characters) > 0 {
		p++
	}
	if s.SceneID == "s1" {
		p++ // 开场镜头决定观众第一印象
	}
	return p
}

// riskOf 高风险判定：多人、复杂运镜、夜景、大远景（AI 容易糊）。
func riskOf(s promspec.ShotSpec) string {
	score := 0
	if len(s.Characters) > 1 {
		score += 2
	}
	switch s.CameraMove {
	case "手持跟随", "航拍", "环绕", "升降":
		score += 2
	}
	if s.TimeOfDay == "夜" || s.TimeOfDay == "night" {
		score++
	}
	if s.ShotSize == "大远景" || s.ShotSize == "远景" {
		score++
	}
	if s.Light.ColorTemp >= 5000 {
		score++
	}
	switch {
	case score >= 4:
		return "high"
	case score >= 2:
		return "medium"
	default:
		return "low"
	}
}

func findCharacter(list []characterOut, name string) (characterOut, bool) {
	for _, c := range list {
		if nameMatches(c.Name, name) {
			return c, true
		}
	}
	return characterOut{}, false
}

func findShot(list []shotOne, id string) (shotOne, bool) {
	for _, s := range list {
		if s.ID == id {
			return s, true
		}
	}
	return shotOne{}, false
}

// intersect 取模型给出场角色与分镜出场角色的交集；模型漏填时回落到分镜。
func intersect(modelChars, sbChars []string, bible CharacterBible) []string {
	out := make([]string, 0, len(sbChars))
	for _, s := range sbChars {
		for _, m := range modelChars {
			if nameMatches(m, s) {
				out = append(out, s)
				break
			}
		}
	}
	if len(out) == 0 {
		return append([]string(nil), sbChars...)
	}
	return out
}

// mockShotSpec 离线演示数据：与模型产出结构一致，保证离线也能验证全链路。
func mockShotSpec(bible CharacterBible, sb Storyboard) *shotSpecOut {
	out := &shotSpecOut{
		Style: styleOut{Name: "grounded science fiction", Aspect: "2.39:1", TimeOfDay: "夜",
			Palette: "cool teal shadows against warm amber practicals, restrained saturation"},
	}
	for _, c := range bible.Characters {
		out.Characters = append(out.Characters, characterOut{
			Name: c.Name, Age: "35", Ethnicity: "East Asian", Build: "slim, wiry",
			Hair: "short black hair, slightly grown out", Face: c.Appearance,
			Skin:        "weathered skin with visible pores and a faint scar texture",
			Eyes:        "dark brown eyes with steady catchlights",
			Distinctive: []string{"thin scar through the left eyebrow", "brass compass on the chest strap"},
			Wardrobe:    c.Wardrobe, Voice: c.Voice, Emotion: "restrained determination",
		})
	}
	lights := []string{"amber control console glow", "cold blue holographic ring light",
		"golden sunset through the canopy", "warm wheat-field bounce light"}
	for i, sh := range sb.Shots {
		out.Shots = append(out.Shots, shotOne{
			ID:             sh.ID,
			ActionEn:       sh.Description,
			ShotSizeEn:     promspec.ShotSizeEn(sh.ShotSize),
			CameraMoveEn:   promspec.CameraMoveEn(sh.CameraMove),
			LensMM:         promspec.LensFor(sh.ShotSize, 0).MM,
			TStop:          "T2.0",
			LightKey:       lights[i%len(lights)],
			LightQuality:   "soft directional",
			LightDirection: "camera-left three-quarter",
			LightRatio:     "8:1 contrast",
			ColorTemp:      3200,
			Composition:    "rule of thirds, subject on the left third",
			SubjectPos:     "subject on the left third",
			Emotion:        "tense",
			Note:           "keep screen direction consistent with the previous shot",
			Characters:     sh.Characters,
		})
	}
	return out
}

// ShotSpecKit 供渲染端读取类型（编译期保证结构一致）。
type ShotSpecKit = promspec.Kit
