package skills

import (
	"fmt"
	"math"
	"strings"

	"aistudio/internal/promspec"
	"aistudio/internal/shotkit"
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
	ID             string  `json:"id"`
	ActionEn       string  `json:"action_en"`
	ShotSizeEn     string  `json:"shot_size_en"`
	CameraMoveEn   string  `json:"camera_move_en"`
	LensMM         int     `json:"lens_mm"`
	TStop          string  `json:"t_stop"`
	LightKey       string  `json:"light_key"`
	LightQuality   string  `json:"light_quality"`
	LightDirection string  `json:"light_direction"`
	LightRatio     string  `json:"light_ratio"`
	ColorTemp      float64 `json:"color_temp"`
	Composition    string  `json:"composition"`
	SubjectPos     string  `json:"subject_position"`
	Emotion        string  `json:"emotion"`
	Note           string  `json:"note"`
	// SceneState 本镜的环境状态变体 slug（只能取自素材规划为该场次声明的清单；
	// 空 = 仍处于场次初始状态）。渲染据此选环境基准图。
	SceneState string `json:"scene_state"`
	// MouthVisibility 说话人嘴部在画面中的可见性（visible/covered/offscreen）。
	// 有画面内台词必须声明；没有画面内台词必须留空。
	MouthVisibility string `json:"mouth_visibility"`
	// ShotSize 景别修正（中文枚举，空 = 沿用分镜）。仅在"说话人嘴部清晰可见 +
	// 特写/大特写"导致口型穿帮、必须改景别时填写；代码据此重算 shot_size_en 与焦段。
	ShotSize   string   `json:"shot_size"`
	Characters []string `json:"characters"`
}

// mouthVisibilityEnum 说话人嘴部可见性的合法取值。
var mouthVisibilityEnum = map[string]bool{"visible": true, "covered": true, "offscreen": true}

// resolveShotSize 应用 shotspec 对分镜景别的修正（空 = 不改；改则限枚举内合法值）。
//
// 为什么必须允许修正：分镜给的"特写正对说话人"在无声生成 + 后期配音的管线里
// 必然口型穿帮，而景别是分镜的机器枚举、镜头语言由代码编译——修正只能落在这个
// 字段上（只改 shot_size_en 而留着中文枚举，会得到"英文说中景、焦段按特写算"的
// 自相矛盾规格）。景别一改，英文描述与光学参数按新景别重编译。
func resolveShotSize(spec *promspec.ShotSpec, declared string) error {
	d := strings.TrimSpace(declared)
	if d == "" || d == spec.ShotSize {
		return nil
	}
	if !shotSizeEnum[d] {
		return fmt.Errorf("产出校验失败: 镜头 %s 的 shot_size=%q 不在枚举内（大远景/远景/全景/中景/近景/特写/大特写）", spec.ID, d)
	}
	spec.ShotSize = d
	spec.ShotSizeEn = promspec.ShotSizeEn(d)
	spec.Lens = promspec.LensFor(d, 0)
	return nil
}

// applyMouthVisibility 校验并归一 mouth_visibility（口型穿帮的确定性下限）。
//
// 背景：生成端是无声生成（音频不进请求）、TTS 在交付期才叠上去——口型与台词
// 之间没有任何绑定机制。"嘴清晰可见地在说话"是穿帮的最坏形态，而"看不见嘴"的
// 构图（侧脸/背身/前景遮挡/说话人出画）天然规避，且本就是电影常用的语言。
//
// 规则：无画面内台词 → 必须留空（填了归一为空并告警）；有画面内台词 → 必须声明，
// 且 visible 不允许搭配 特写/大特写。
func applyMouthVisibility(spec *promspec.ShotSpec, declared string, logf func(string, ...any)) error {
	declared = strings.ToLower(strings.TrimSpace(declared))
	onScreen := spec.SpeakingCharacters()
	if len(onScreen) == 0 {
		if declared != "" && logf != nil {
			logf("  ⚠ 镜头 %s 没有画面内台词却声明了 mouth_visibility=%s，已清空", spec.ID, declared)
		}
		spec.MouthVisibility = ""
		return nil
	}
	if declared == "" {
		return fmt.Errorf("产出校验失败: 镜头 %s 有画面内台词（%s）却没有声明 mouth_visibility（visible/covered/offscreen）：生成端无声、配音后期叠加，口型与台词必然对不上，必须声明可见性让构图规避穿帮", spec.ID, strings.Join(onScreen, "、"))
	}
	if !mouthVisibilityEnum[declared] {
		return fmt.Errorf("产出校验失败: 镜头 %s 的 mouth_visibility=%q 不合法（可用 visible/covered/offscreen）", spec.ID, declared)
	}
	if declared == "visible" && (spec.ShotSize == "特写" || spec.ShotSize == "大特写") {
		return fmt.Errorf("产出校验失败: 镜头 %s 是 %s 且说话人嘴部清晰可见（visible）——正对嘴部的特写是口型穿帮的最坏形态。请二选一：① 改成看不见嘴的构图并声明 covered/offscreen（侧脸/背身/前景遮挡/说话人出画，并在 composition 里写明遮挡方式）；② 用 shot_size 把景别修正为 中景/近景 等", spec.ID, spec.ShotSize)
	}
	spec.MouthVisibility = declared
	return nil
}

// containsStr 值是否在列表中（环境状态 slug 校验用）。
func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// validateSceneState 校验并归一 scene_state：空（场次初始状态）恒合法；非空必须
// 命中素材规划为该场次声明的状态清单——错的 slug 会让渲染期拿不到基准图（硬失败）
// 或指向另一状态的图（静默带偏整镜画面），两种都比在创作阶段拦下来贵。
// 返回归一后的 slug（空字符串 = 不绑定状态）。
func validateSceneState(shotID, sceneID, state string, statesByScene map[string][]string) (string, error) {
	st := strings.TrimSpace(state)
	if st == "" {
		return "", nil
	}
	valid := statesByScene[sceneID]
	if len(valid) == 0 {
		return "", fmt.Errorf("产出校验失败: 镜头 %s 填了 scene_state=%q，但素材规划没有为场次 %s 声明任何状态变体（该字段应留空字符串）", shotID, st, sceneID)
	}
	if !containsStr(valid, st) {
		return "", fmt.Errorf("产出校验失败: 镜头 %s 的 scene_state=%q 不在场次 %s 声明的状态清单内（可用：%s）", shotID, st, sceneID, strings.Join(valid, "、"))
	}
	return st, nil
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

	// 场次环境状态清单（素材规划产出）：scene_state 只能取这里为该场次声明过的 slug。
	// 没有规划（未跑素材阶段）时清单为空，所有镜头的 scene_state 必须留空——
	// 渲染会退回场次主基准图作环境锚，这是"未提供"而非降级。
	statesByScene := map[string][]string{}
	if plan := shotkit.LoadAssetPlan(ctx.WorkDir); plan != nil {
		for _, st := range plan.SceneStates {
			for _, sid := range st.SceneIDs {
				statesByScene[sid] = append(statesByScene[sid], st.State)
			}
		}
	}

	out := &shotSpecOut{}
	if ctx.AI.Mock() {
		out = mockShotSpec(bible, sb)
	} else {
		system := filmBase + `
你是摄影指导兼视觉规格撰写者。你的输出不是最终提示词，而是**创作判断**：景别、运镜、光学参数、
照明方案、构图、情绪、以及每个镜头的表演节拍。提示词将由系统根据你的判断按固定模板编译。
action_en / shot_size_en / camera_move_en / light_key 会逐字编译进图像与视频生成提示词：
一律使用中性、健康的视觉词汇——严禁出现 blood、wound、corpse、weapon、hijack、restrain、inject、
dead 等易触发内容审核的词（即使剧情需要，也改用光影对比、构图挤压、道具暗示来表达张力，
如 dim stains、worn props、tense silhouette）；审核拦截是 HTTP 400 硬失败，整镜报废。

schema：
{"style":{"name":"整体视觉风格名","aspect":"画幅如 2.39:1","time_of_day":"主时段","palette":"色彩方案"},
 "characters":[{"name":"角色名","age":"年龄","ethnicity":"族裔","build":"体态","hair":"发型发色","face":"骨相与五官细节（决定是否同一个人）","skin":"皮肤质感（必须写毛孔/纹理等真实细节）","eyes":"眼睛特征","distinctive":["标志性特征如疤痕/痣/饰品"],"wardrobe":"服装","voice":"音色与语速","emotion":"本片中的主要情绪基调"}],
 "shots":[{"id":"sh001","action_en":"英文画面与表演描述，具体到可拍摄","shot_size_en":"英文景别描述","camera_move_en":"英文运镜描述","lens_mm":50,"t_stop":"T2.0","light_key":"主光动机（画面内的光源）","light_quality":"soft|hard|diffused","light_direction":"光位如 camera-left three-quarter back","light_ratio":"光比如 8:1","color_temp":3200,"composition":"构图规则","subject_position":"主体位置","emotion":"情绪基调","note":"衔接与连续性说明","scene_state":"环境状态变体 slug（只能取场次状态清单里的值；仍处于场次初始状态时留空字符串）","mouth_visibility":"visible|covered|offscreen（有画面内台词必须声明；没有画面内台词留空字符串）","shot_size":"景别修正（中文枚举，一般留空 = 沿用分镜；仅在口型风险必须改景别时填）","characters":["出场角色名"]}]}

硬性要求：
1. shots 必须与给定分镜**逐个对应**（相同 id、相同顺序、数量一致），不得增删；
2. characters 必须覆盖角色圣经中所有角色，且 appearance 细节比圣经更具体（这是同一个人跨镜头不变形的依据）；
3. 照明必须有明确的画面内动机（窗户/台灯/屏幕/火光），禁止"无来源的均匀补光"——那是最明显的 AI 感；
4. lens_mm 按景别选择（大远景 24 / 远景 32 / 全景 40 / 中景 50 / 近景 65 / 特写 85 / 大特写 100）；
5. scene_state 只在下方给出「场次环境状态清单」时才能填，且必须是该场次声明过的 slug；
   本镜画面环境仍处于场次初始状态就留空。填了系统就会拿该状态的美术基准图作为本镜的
   环境锚——状态与环境描述不符的锚点会带偏整镜画面（如"云层散尽"的镜头拿"雷云蔽日"的基准）；
6. mouth_visibility：说话人嘴部在画面里的可见性，这是口型穿帮的确定性下限——生成端是无声
   生成、配音在后期叠加，口型与台词必然对不上，只有"让观众看不见嘴在说话"才能规避：
   · 有画面内台词（说话人不是 narrator）的镜头**必须**声明 visible / covered / offscreen；
   · visible = 嘴部清晰可见；covered = 侧脸/背身/前景遮挡/虚焦，看不见嘴；offscreen = 说话人不在画内（画外音）；
   · **visible 不允许搭配 特写/大特写**（会被拒绝）：正对嘴部的特写是穿帮的最坏形态。请把台词
     改由看不见嘴的构图承载（听者反应镜头、过肩、侧脸、前景遮挡、手部/道具大特写、说话人出画），
     在 composition 里写明遮挡方式，并声明 covered/offscreen；
   · 没有画面内台词的镜头必须留空字符串；
7. shot_size：一般必须留空（沿用分镜景别）。仅在"说话人嘴部可见 + 特写/大特写"且无法用构图规避时，
   才用它在枚举内修正景别（如 中景/近景）；代码会据此重算 shot_size_en 与镜头焦段；
8. 只输出 JSON。`
		user := "创作意图：" + ctx.Brief +
			"\n剧本：" + marshal(script) +
			"\n分镜表（shots 必须与之一一对应）：" + marshal(sb) +
			"\n角色圣经（characters 必须覆盖）：" + marshal(bible) +
			"\n建议整体风格：" + preset.Name + "（" + style.Name + "）"
		if len(statesByScene) > 0 {
			user += "\n场次环境状态清单（scene_state 只能取这里的值，其余镜头留空）：" + marshal(statesByScene)
		}
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
		var specMouthDecl string // 模型声明的嘴部可见性（节拍构建后校验）
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
			st, serr := validateSceneState(sh.ID, sh.SceneID, o.SceneState, statesByScene)
			if serr != nil {
				return nil, serr
			}
			spec.SceneState = st
			if len(o.Characters) > 0 {
				spec.Characters = intersect(o.Characters, sh.Characters, bible)
			}
			// 景别修正放在全部覆盖之后：它要压过分镜的枚举值（口型风险时才允许）
			if serr := resolveShotSize(&spec, o.ShotSize); serr != nil {
				return nil, serr
			}
			specMouthDecl = o.MouthVisibility
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
		for _, l := range lines {
			dialogueSec += srt.EstimateSpeakSec(l.Text)
		}
		// 台词比镜头长 → 确定性修复：延长镜头容纳台词。
		// 台本是唯一真源（不可改写），分镜已人工确认——自修复让模型"精简台词或
		// 延长镜头"属于不可执行动作（schema 里没有 duration，台本文本必须逐句
		// 原样绑定），只会烧光 3 轮重试后以同样原因失败
		if dialogueSec > spec.DurationSec*1.15 {
			newDur := math.Ceil(dialogueSec/1.15*2) / 2
			ctx.Log("  ↻ 镜头 %s 时长 %.1fs → %.1fs（容纳台词约 %.1fs）", sh.ID, spec.DurationSec, newDur, dialogueSec)
			spec.DurationSec = newDur
		}
		at := 0.0
		for _, l := range lines {
			kind := "dialogue"
			if isNarrator(l.Speaker) {
				kind = "narration"
			}
			sec := srt.EstimateSpeakSec(l.Text)
			spec.Beats = append(spec.Beats, promspec.Beat{
				Kind: kind, Speaker: l.Speaker, Text: l.Text,
				AtSec: at, Emotion: l.Emotion,
			})
			// 台词在镜头内的排布：留出入口LeadIn，避免"话音未落画面已切"
			at += sec + 0.25
			if at > spec.DurationSec {
				at = spec.DurationSec
			}
		}
		// 无台词的镜头补一个动作节拍，保证 video_prompt 里有可执行的表演描述
		if len(spec.Beats) == 0 {
			spec.Beats = append(spec.Beats, promspec.Beat{
				Kind: "action", Text: spec.ActionEn, AtSec: 0, Emotion: spec.Emotion,
			})
		}
		spec.DialogueSec = dialogueSec
		// 口型风险门禁必须放在节拍构建之后：判定依据是"画面内说话人"（旁白不算），
		// 而发声人信息在节拍里。
		if merr := applyMouthVisibility(&spec, specMouthDecl, ctx.Log); merr != nil {
			return nil, merr
		}

		// 优先级与风险：决定渲染顺序（先渲关键/高风险镜头，尽早发现问题）
		spec.Priority = priorityOf(spec, script)
		spec.Risk = riskOf(spec)
		kit.ShotList = append(kit.ShotList, spec)
	}

	// ---- 校验：这是"字幕与台词对得上"的确定性保障 ----
	if err := validateShotSpec(kit, sb, vo); err != nil {
		return nil, err
	}
	// 口型风险汇总：visible 的台词镜头就是"若将来上口型重对，要重对哪些镜"的工作清单；
	// 打成一行，避免用户在成片里才发现"说话镜头正对着嘴"。
	var lines, mvVisible, mvSafe int
	for _, s := range kit.ShotList {
		if len(s.SpeakingCharacters()) == 0 {
			continue
		}
		lines++
		if s.MouthVisibility == "visible" {
			mvVisible++
		} else {
			mvSafe++
		}
	}
	if lines > 0 {
		ctx.Log("  👄 有台词的镜头 %d 个：嘴部可见 %d（生成端无声生成、配音后期叠加，口型必然对不上——靠构图让观众看不出）· 已规避 %d（covered/offscreen）",
			lines, mvVisible, mvSafe)
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
			// 离线演示同样走口型门禁：有画面内台词的镜头声明"看不见嘴"的构图
			// （见 applyMouthVisibility）——mock 与真实链路判分口径一致才可信。
			MouthVisibility: mockMouthVisibility(sh),
			Characters:      sh.Characters,
		})
	}
	return out
}

// mockMouthVisibility 离线演示的嘴部可见性：有角色出镜的镜头按"看不见嘴"处理。
func mockMouthVisibility(sh Shot) string {
	if len(sh.Characters) > 0 {
		return "covered"
	}
	return ""
}

// ShotSpecKit 供渲染端读取类型（编译期保证结构一致）。
type ShotSpecKit = promspec.Kit
