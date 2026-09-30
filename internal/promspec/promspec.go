// Package promspec 把"电影感"编译成确定性的摄影机语言。
//
// 为什么需要它：让大模型自由写 image_prompt 会产出"cinematic, 8k, masterpiece"这类
// 触发 AI 学生成模式的词，画面立刻带上塑料感/糖水 HDR/过度锐化的 AI 痕迹。
// 真实电影感来自可复现的摄影参数——数字负片、球面定焦、T2.0 浅景深、单一动机光源、
// 保留皮肤纹理与镜头瑕疵。本包把这些参数固化成 DSL，由代码编译成提示词：
//   - 同一镜头反复生成得到**稳定且可解释**的提示词（可进版本管理、可 diff）
//   - 模型只负责提供创作判断（景别/运镜/光位/色温），摄影语言由代码保证下限
//   - 负面提示词按 AI 伪影分类固化，覆盖"塑料皮肤/蜡像/多指/CG/水印/文字"
package promspec

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 摄影机包（Camera Package）
// ---------------------------------------------------------------------------

// Lens 镜头参数：焦段 + 光圈 + 玻璃特征。
// 电影感的第一来源是"特定焦段的透视 + 大光圈的浅景深"，而非"8k 超清"。
type Lens struct {
	MM        int    `json:"mm"`
	TStop     string `json:"t_stop"`    // 例如 "T1.8"
	Character string `json:"character"` // 玻璃特征：vintage softness / modern clinical
	DoF       string `json:"dof"`       // 景深描述
	Flare     string `json:"flare"`     // 眩光特征
	SortKey   int    `json:"sort_key"`  // 排序序号（确定性输出用）
}

// Camera 机身：传感器/胶片特性决定质感基调。
type Camera struct {
	Name      string `json:"name"`       // 例如 "ARRI Alexa 35"
	Format    string `json:"format"`     // 例如 "Super 35 digital negative"
	Texture   string `json:"texture"`    // 例如 "14 stops dynamic range, organic highlight rolloff"
	Mount     string `json:"mount"`      //
	FilmStock string `json:"film_stock"` // 例如 "Kodak Vision3 250D emulation"
}

// LightSetup 照明：单一动机光源是"不像 AI"的关键——AI 图常见无来源的均匀补光。
type LightSetup struct {
	Key       string  `json:"key"`        // 主光动机，例如 "single practical desk lamp camera-left"
	Quality   string  `json:"quality"`    // 光质：soft / hard / diffused
	Direction string  `json:"direction"`  // 光位：camera-left 3/4 back
	ColorTemp float64 `json:"color_temp"` // 开尔文，例如 3200
	Ratio     string  `json:"ratio"`      // 光比，例如 "8:1 contrast"
	Practical string  `json:"practical"`  // 画面内可见光源
}

// Grade 调色：胶片感的关键在于"不完美"——颗粒、光晕、轻微失焦、低饱和的肤色。
type Grade struct {
	Palette  string `json:"palette"`  // 色彩方案
	Contrast string `json:"contrast"` //
	Grain    string `json:"grain"`    // 颗粒
	Halation string `json:"halation"` // 高光泛光（胶片标志性特征）
	LUT      string `json:"lut"`      //
}

// Composition 构图：本 DSL 不接受 "centered, beautiful"，只接受可执行的取景指令。
type Composition struct {
	Frame      string `json:"frame"`          // 例如 "rule of thirds, subject on left third"
	SubjectPos string `json:"subject_pos"`    // 主体位置
	Headroom   string `json:"headroom"`       // 头room 规则
	Negative   string `json:"negative_space"` // 留白用途（字幕/构图透气）
}

// ---------------------------------------------------------------------------
// 全局风格
// ---------------------------------------------------------------------------

// Style 全局视觉风格：整片统一，逐镜头只做变奏。这是"同一部电影"的保证。
type Style struct {
	Name      string   `json:"name"`        // 例如 "70mm naturalism"
	Aspect    string   `json:"aspect"`      // 例如 "2.39:1"
	Camera    Camera   `json:"camera"`      //
	Grade     Grade    `json:"grade"`       //
	Photoreal []string `json:"photoreal"`   // 真实感硬约束
	TimeOfDay string   `json:"time_of_day"` //
	Locations []string `json:"locations"`   //
	Score     string   `json:"score"`       // 情绪基调（用于镜头间节奏统一）
}

// ---------------------------------------------------------------------------
// 人物（角色一致性）
// ---------------------------------------------------------------------------

// CharacterSpec 角色外观规格。Anchors 必须是"对生成模型有视觉约束力"的具象描述，
// 而不是"beautiful woman"——后者每个镜头都会变脸。
type CharacterSpec struct {
	Name        string   `json:"name"`
	Token       string   `json:"token"` // 一致性锚点串（小写连字符），跨镜头逐字引用
	Age         string   `json:"age"`
	Ethnicity   string   `json:"ethnicity"`
	Build       string   `json:"build"`
	Hair        string   `json:"hair"`
	Face        string   `json:"face"` // 骨相/五官细节，决定"是不是同一个人"
	Skin        string   `json:"skin"` // 皮肤质感（必须含毛孔/纹理，防塑料感）
	Eyes        string   `json:"eyes"`
	Distinctive []string `json:"distinctive"` // 标志性特征（疤/痣/饰品），跨镜头复现的抓手
	Wardrobe    string   `json:"wardrobe"`
	Voice       string   `json:"voice"`     // 音色设定 → 映射 TTS 音色
	TTSVoice    string   `json:"tts_voice"` // 实际音色名（可由 config.voice_map 覆盖）
	RefImage    string   `json:"ref_image"` // 角色参考帧（渲染产物，相对 run 目录）
	RefSeed     int64    `json:"ref_seed"`  // 参考帧种子（同角色同种子=同脸）
	Emotion     string   `json:"emotion"`   // 本片中的主要情绪基调
}

// AnchorText 编译成英文角色锚定串，逐镜头原样注入 image_prompt。
// 顺序固定，保证同一角色在不同镜头的描述完全一致（描述漂移=换脸）。
func (c CharacterSpec) AnchorText() string {
	parts := []string{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" {
			parts = append(parts, s)
		}
	}
	add(c.Ethnicity)
	add(c.Age)
	add(c.Build)
	add(c.Hair)
	add("face: " + c.Face)
	add("skin: " + c.Skin)
	add("eyes: " + c.Eyes)
	if len(c.Distinctive) > 0 {
		d := append([]string(nil), c.Distinctive...)
		sort.Strings(d)
		add("distinguishing features: " + strings.Join(d, ", "))
	}
	// 锚点是给模型看的，不能为空；空锚点会让"一致性"彻底失效
	if len(parts) == 0 {
		return "consistent character appearance"
	}
	return strings.Join(parts, ", ")
}

// WardrobeText 服装描述（独立于外观，便于按场景换装但不换脸）。
func (c CharacterSpec) WardrobeText() string {
	if strings.TrimSpace(c.Wardrobe) == "" {
		return ""
	}
	return c.Wardrobe
}

// ---------------------------------------------------------------------------
// 镜头规格
// ---------------------------------------------------------------------------

// Beat 台词/动作节拍：台词与画面的对应关系写进规格，避免"台词与画面对不上"。
type Beat struct {
	Kind    string  `json:"kind"`    // action | dialogue | reaction
	Speaker string  `json:"speaker"` //
	Text    string  `json:"text"`    // 台词原文（字幕与配音的唯一真源）
	AtSec   float64 `json:"at_sec"`  // 该节拍在镜头内的秒点
	Emotion string  `json:"emotion"` //
}

// ShotSpec 单镜头完整规格。渲染、配音、字幕三方都以它为唯一真源。
type ShotSpec struct {
	ID           string      `json:"id"`
	SceneID      string      `json:"scene_id"`
	Location     string      `json:"location"`
	TimeOfDay    string      `json:"time_of_day"`
	Description  string      `json:"description"` // 中文画面描述（人类可读）
	ActionEn     string      `json:"action_en"`   // 英文动作描述（喂生成模型）
	ShotSize     string      `json:"shot_size"`   // 中文枚举
	ShotSizeEn   string      `json:"shot_size_en"`
	CameraMove   string      `json:"camera_move"` // 中文
	CameraMoveEn string      `json:"camera_move_en"`
	Lens         Lens        `json:"lens"`
	Light        LightSetup  `json:"light"`
	Composition  Composition `json:"composition"`
	DurationSec  float64     `json:"duration_sec"`
	Characters   []string    `json:"characters"`   // 出场角色名（引用 characters 产物）
	Beats        []Beat      `json:"beats"`        // 镜头内节拍（含台词）
	Emotion      string      `json:"emotion"`      // 情绪基调
	Continuity   string      `json:"continuity"`   // 与前后镜头的衔接说明（防跳轴/防穿帮）
	DialogueSec  float64     `json:"dialogue_sec"` // 本地重算：本镜头台词占用秒数
	Seed         int64       `json:"seed"`         // 本镜头生成种子（重渲时递增，其余不变）
	Risk         string      `json:"risk"`         // low|medium|high：高风险镜头优先试渲
	Priority     int         `json:"priority"`     // 关键镜头优先级（先渲关键镜头验证画质）
}

// DialogueBeats 返回该镜头的台词节拍（按时间排序）。
func (s ShotSpec) DialogueBeats() []Beat {
	var out []Beat
	for _, b := range s.Beats {
		if b.Kind == "dialogue" && strings.TrimSpace(b.Text) != "" {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AtSec < out[j].AtSec })
	return out
}

// HasCharacter 该镜头是否有指定角色出场。
func (s ShotSpec) HasCharacter(name string) bool {
	for _, c := range s.Characters {
		if c == name {
			return true
		}
	}
	return false
}

// IsNarrator 判断说话人是否为旁白。
// 旁白不属于画面内角色，不参与人脸一致性校验，也不占用角色音色表。
func IsNarrator(speaker string) bool {
	s := strings.ToLower(strings.TrimSpace(speaker))
	return s == "narrator" || s == "vo" || strings.Contains(speaker, "旁白") || strings.Contains(speaker, "画外音")
}

// SpeakingCharacters 返回镜头中说话的**画面内**角色（用于配音与定格帧选择）。
func (s ShotSpec) SpeakingCharacters() []string {
	var out []string
	for _, b := range s.DialogueBeats() {
		if b.Speaker == "" || IsNarrator(b.Speaker) {
			continue
		}
		found := false
		for _, c := range out {
			if c == b.Speaker {
				found = true
				break
			}
		}
		if !found {
			out = append(out, b.Speaker)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 提示词编译
// ---------------------------------------------------------------------------

// Kit 一部片子的完整生成规格。由 shotspec 阶段产出，渲染/配音/字幕共同消费。
type Kit struct {
	Style      Style           `json:"style"`
	ShotList   []ShotSpec      `json:"shots"`
	Characters []CharacterSpec `json:"characters"`
	Version    int             `json:"version"`
}

// CharOf 按名字取角色规格。
func (k *Kit) CharOf(name string) (CharacterSpec, bool) {
	for _, c := range k.Characters {
		if c.Name == name {
			return c, true
		}
	}
	return CharacterSpec{}, false
}

// ShotOf 按 ID 取镜头规格。
func (k *Kit) ShotOf(id string) (ShotSpec, bool) {
	for _, s := range k.ShotList {
		if s.ID == id {
			return s, true
		}
	}
	return ShotSpec{}, false
}

// TotalDuration 本地重算全片时长（永远不信任模型自报）。
func (k *Kit) TotalDuration() float64 {
	var t float64
	for _, s := range k.ShotList {
		t += s.DurationSec
	}
	return t
}

// photorealBase 真实感硬约束。
// 这些词不是装饰：AI 学生成最明显的破绽是塑料皮肤、无毛孔、过锐、对称到不真实、
// 眼睛无高光。把它们写成正向要求，比只在负面里写 "bad quality" 有效得多。
var photorealBase = []string{
	"photorealistic live-action film still, shot on a real cinema camera",
	"authentic skin texture with visible pores, fine facial hair and natural subsurface scattering",
	"catchlights in both eyes, slight natural asymmetry in the face",
	"real fabric weave, visible seams, natural wear and creases in wardrobe",
	"motivated practical light sources visible in frame",
	"shallow depth of field with organic bokeh, some foreground occlusion",
	"subtle film grain, gentle halation around highlights, natural highlight rolloff",
	"slightly imperfect framing and focus like a real camera operator",
	"true-to-life color science, desaturated skin tones, no HDR glow",
}

// negativeBase 负面提示词按 AI 伪影分类固化。
// 分类的好处：缺什么补什么，且不会因为负面串过长而稀释权重。
var negativeGroups = map[string][]string{
	"anatomy": {
		"extra fingers", "fused fingers", "six fingers", "missing fingers",
		"deformed hands", "extra limbs", "extra arms", "twisted limbs",
		"deformed ears", "asymmetric eyes", "cross-eyed", "floating teeth",
		"bad proportions", "elongated neck",
	},
	"face": {
		"plastic skin", "wax figure", "doll-like face", "airbrushed skin",
		"over-smoothed skin", "poreless skin", "uncanny valley", "dead eyes",
		"no catchlight", "lifeless gaze", "smudged facial features",
		"changing face between shots", "inconsistent character identity",
	},
	"artifacts": {
		"watermark", "signature", "text", "caption", "logo", "UI overlay",
		"jpeg artifacts", "compression blocks", "banding", "posterization",
		"oversharpened halos", "duplicated subject", "cloned face",
		"warped background geometry", "melted details", "blurry mush",
	},
	"style": {
		"anime", "cartoon", "3d render", "cgi", "cgi plastic look", "video game",
		"illustration", "digital painting", "concept art", "matte painting",
		"oil painting", "sketch", "low poly",
	},
	"lighting": {
		"HDR glow", "over-saturated colors", "flat frontal key light",
		"studio beauty lighting", "unmotivated lighting", "blown out highlights",
		"crushed black shadows", "neon color cast",
	},
	// look 糖水片质感：重散景 + 青橙过饱和 + 磨皮滤镜是 AI 出图的默认审美，
	// 也是"一眼 AI"的最大来源。负面词在这里挡一层，饱和度 QC 在质检里再挡一层。
	"look": {
		"heavy bokeh background", "over-blurred background",
		"teal and orange oversaturated grade", "beauty filter",
		"stock photo look", "glossy advertisement look",
	},
	"composition": {
		"cropped head", "cropped hands", "cut off ear", "extreme close-up of fingers",
		"centered passport photo composition", "cluttered background",
	},
}

// NegativePrompt 编译负面提示词。groups 为空则输出全部组。
func NegativePrompt(groups ...string) string {
	if len(groups) == 0 {
		groups = []string{"anatomy", "face", "artifacts", "style", "lighting", "look", "composition"}
	}
	var out []string
	seen := map[string]bool{}
	for _, g := range groups {
		items, ok := negativeGroups[g]
		if !ok {
			continue
		}
		for _, it := range items {
			if !seen[it] {
				seen[it] = true
				out = append(out, it)
			}
		}
	}
	return strings.Join(out, ", ")
}

// NegativeGroupsFor 按镜头内容挑选负面组：有人的镜头才强调 anatomy/face，
// 纯空镜加 anatomy 只会浪费提示词权重。look（糖水质感）恒在——它是全片审美约束。
func NegativeGroupsFor(s ShotSpec) []string {
	groups := []string{"artifacts", "style", "lighting", "look", "composition"}
	if len(s.Characters) > 0 {
		groups = append([]string{"face", "anatomy"}, groups...)
	}
	return groups
}

// RefPrompt 编译角色参考帧提示词（三视图式标准像）。
// 参考帧是整个一致性的地基：先用最短提示词锁死一张标准脸，之后所有镜头都以它为锚。
func RefPrompt(style Style, c CharacterSpec) string {
	var b strings.Builder
	b.WriteString("Character reference sheet for a film production, neutral studio lighting, ")
	b.WriteString("plain mid-grey seamless background, ")
	b.WriteString("three-quarter head-and-shoulders portrait plus a full-body front view of the SAME person, ")
	b.WriteString("same person in both views, identical facial structure.\n")
	fmt.Fprintf(&b, "Character: %s.\n", c.AnchorText())
	// 标志性特征单独升级为 MUST 级约束：图像模型最容易丢掉的就是这类细节
	// （眼镜/疤痕/饰品），而它们恰恰是"是不是同一个人"的视觉抓手——
	// 参考帧丢了眼镜，成片里眼镜忽有忽无（真实案例）。
	if len(c.Distinctive) > 0 {
		fmt.Fprintf(&b, "Identity anchors that MUST be clearly visible: %s. Do not omit or alter any of them.\n", strings.Join(c.Distinctive, ", "))
	}
	if w := c.WardrobeText(); w != "" {
		fmt.Fprintf(&b, "Wardrobe: %s.\n", w)
	}
	if c.Emotion != "" {
		fmt.Fprintf(&b, "Expression: %s, relaxed neutral.\n", c.Emotion)
	}
	b.WriteString("Camera: 85mm prime at T2.8, eye-level, sharp on both eyes, no distortion.\n")
	fmt.Fprintf(&b, "Look: %s, %s.\n", style.Camera.Format, style.Camera.Texture)
	if style.Grade.Palette != "" {
		fmt.Fprintf(&b, "Color: %s.\n", style.Grade.Palette)
	}
	b.WriteString("Requirements: photorealistic, identifiable individual, consistent identity across views, ")
	b.WriteString("visible skin pores and natural complexion, no retouching, no beauty filter, ")
	b.WriteString("flat even lighting so the face is fully readable.\n")
	fmt.Fprintf(&b, "Avoid: %s.", NegativePrompt("face", "anatomy", "style", "artifacts"))
	return b.String()
}

// ShotPrompt 编译单镜头的首帧图提示词。
// 结构固定为 8 段：画面语言 → 摄影机 → 光学 → 照明 → 调色 → 角色锚点 → 真实感 → 负面。
// 固定结构让不同镜头的提示词只在"应有的维度"上变化，模型不会随机漂移风格。
func ShotPrompt(style Style, s ShotSpec, chars []CharacterSpec) string {
	var b strings.Builder

	// 1. 画面语言
	b.WriteString("Photorealistic film still from a live-action feature.\n")
	size := strings.TrimSpace(s.ShotSizeEn)
	if size == "" {
		size = s.ShotSize
	}
	action := strings.TrimSpace(s.ActionEn)
	if action == "" {
		action = strings.TrimSpace(s.Description)
	}
	fmt.Fprintf(&b, "Shot: %s. %s\n", size, action)
	if s.Location != "" || s.TimeOfDay != "" {
		fmt.Fprintf(&b, "Setting: %s, %s.\n", s.Location, orDefault(s.TimeOfDay, style.TimeOfDay))
	}
	if s.Emotion != "" {
		fmt.Fprintf(&b, "Emotional tone: %s.\n", s.Emotion)
	}

	// 2. 摄影机
	fmt.Fprintf(&b, "Camera: %s, %s, %s.\n", style.Camera.Name, style.Camera.Format, style.Camera.FilmStock)

	// 3. 光学：焦段+光圈+景深+眩光
	if s.Lens.MM > 0 {
		fmt.Fprintf(&b, "Optics: %dmm spherical prime at %s, %s, %s, %s.\n",
			s.Lens.MM, orDefault(s.Lens.TStop, "T2.0"),
			orDefault(s.Lens.DoF, "shallow depth of field"),
			orDefault(s.Lens.Character, "modern cinema glass"),
			orDefault(s.Lens.Flare, "controlled flare"))
	}

	// 4. 照明：单一动机光源 + 光比，这是"真实摄影"与"AI 打光"的分界
	if s.Light.Key != "" {
		fmt.Fprintf(&b, "Lighting: %s, %s, %s, %s, %s.\n",
			s.Light.Key, orDefault(s.Light.Quality, "soft directional"),
			orDefault(s.Light.Direction, "three-quarter"),
			orDefault(s.Light.Ratio, "6:1 contrast"),
			kelvin(s.Light.ColorTemp))
	} else {
		fmt.Fprintf(&b, "Lighting: motivated by visible practicals, %s, %s.\n",
			orDefault(style.TimeOfDay, "natural"), "6:1 contrast")
	}

	// 5. 调色
	fmt.Fprintf(&b, "Grade: %s, %s, %s, %s.\n",
		orDefault(style.Grade.Palette, "naturalistic color"),
		orDefault(style.Grade.Contrast, "filmic contrast curve"),
		orDefault(style.Grade.Grain, "fine 35mm grain"),
		orDefault(style.Grade.Halation, "subtle halation"))

	// 6. 构图
	if s.Composition.Frame != "" || s.Composition.SubjectPos != "" {
		fmt.Fprintf(&b, "Composition: %s, %s, %s.\n",
			orDefault(s.Composition.Frame, "balanced cinematic framing"),
			orDefault(s.Composition.SubjectPos, "subject on the left third"),
			orDefault(s.Composition.Headroom, "normal headroom, no crop"))
	}

	// 7. 角色锚点：逐字复用参考帧的描述，一句都不能改
	if len(chars) > 0 {
		b.WriteString("Characters (must match the reference sheet exactly):\n")
		for _, c := range chars {
			fmt.Fprintf(&b, "- %s [%s]: %s", c.Name, c.Token, c.AnchorText())
			if w := c.WardrobeText(); w != "" {
				fmt.Fprintf(&b, " Wardrobe: %s.", w)
			}
			b.WriteString("\n")
		}
	}

	// 8. 连续性：与前后镜头的衔接要求（视线/轴线/服装/道具状态）。
	// 该字段此前只落盘不消费——生成模型根本不知道上一镜发生了什么，
	// 跳轴与穿帮就是这么进来的。中文描述直接注入：生成端模型双语均可理解。
	if ct := strings.TrimSpace(s.Continuity); ct != "" {
		fmt.Fprintf(&b, "Continuity: match adjacent shots; %s\n", ct)
	}

	// 9. 画幅 + 真实感 + 负面
	fmt.Fprintf(&b, "Aspect ratio: %s.\n", orDefault(style.Aspect, "2.39:1"))
	b.WriteString("Realism requirements: " + strings.Join(photorealBase, "; ") + ".\n")
	fmt.Fprintf(&b, "Avoid: %s.", NegativePrompt(NegativeGroupsFor(s)...))
	return b.String()
}

// VideoPrompt 编译图生视频的运动提示词。
//
// 真实感的关键在"运动克制"：AI 视频最出戏的是大幅甩镜、人物瞬移、面部漂移。
// 因此运动描述必须包含"静止/微动"约束与物理可信度要求，并显式禁止面部形变。
func VideoPrompt(style Style, s ShotSpec) string {
	var b strings.Builder
	move := strings.TrimSpace(s.CameraMoveEn)
	if move == "" {
		move = s.CameraMove
	}
	fmt.Fprintf(&b, "Camera: %s.\n", orDefault(move, "locked off, no camera movement"))

	var actions []string
	for _, bt := range s.Beats {
		if bt.Kind == "dialogue" {
			continue
		}
		if t := strings.TrimSpace(bt.Text); t != "" {
			actions = append(actions, t)
		}
	}
	if len(actions) > 0 {
		fmt.Fprintf(&b, "Action: %s.\n", strings.Join(actions, " then "))
	}
	if len(s.SpeakingCharacters()) > 0 {
		b.WriteString("Dialogue performance: the speaking character delivers the line with subtle, restrained ")
		b.WriteString("facial performance and natural micro-expressions; lips move in sync with speech, ")
		b.WriteString("eye contact and blinks are natural.\n")
	}
	fmt.Fprintf(&b, "Duration: about %.1f seconds of continuous action at real-time speed.\n", s.DurationSec)
	b.WriteString("Motion constraints (critical for realism): ")
	b.WriteString("slow, physically plausible motion only; no sudden cuts; no jump in framing; ")
	b.WriteString("facial features and identity stay locked and unchanged throughout; ")
	b.WriteString("no morphing; no melting limbs; no extra fingers appearing; ")
	b.WriteString("fabric and hair move with believable weight; ")
	b.WriteString("background parallax consistent with the camera move; ")
	b.WriteString("no speed ramping, no slow-motion, no time-lapse.\n")
	if style.Grade.Grain != "" {
		b.WriteString("Keep the grain, contrast and color of the source frame unchanged throughout; do not re-grade.\n")
	}
	return b.String()
}

// NegativesFor 返回该镜头建议的负面提示词（供视频接口使用，部分接口需单独传）。
func NegativesFor(s ShotSpec) string {
	return NegativePrompt(NegativeGroupsFor(s)...)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func kelvin(k float64) string {
	if k <= 0 {
		return "natural daylight white balance"
	}
	return fmt.Sprintf("%.0fK white balance", k)
}

// ---------------------------------------------------------------------------
// 枚举映射：把中文枚举翻译成摄影机语言
// ---------------------------------------------------------------------------

var shotSizeToEn = map[string]string{
	"大远景": "extreme wide establishing shot, vast scale, tiny human figure",
	"远景":  "wide establishing shot, full body with environment",
	"全景":  "full shot, whole body head to toe with headroom",
	"中景":  "medium shot, waist up",
	"近景":  "medium close-up, chest up",
	"特写":  "close-up, face fills frame",
	"大特写": "extreme close-up on a single detail, very shallow focus",
}

var shotSizeLens = map[string]Lens{
	"大远景": {MM: 24, TStop: "T2.8", Character: "modern wide-angle cinema glass", DoF: "deep focus with distant detail", Flare: "clean"},
	"远景":  {MM: 32, TStop: "T2.8", Character: "spherical wide prime", DoF: "moderate depth of field", Flare: "slight"},
	"全景":  {MM: 40, TStop: "T2.0", Character: "spherical prime", DoF: "subject separated from background", Flare: "slight"},
	"中景":  {MM: 50, TStop: "T2.0", Character: "standard spherical prime", DoF: "shallow depth of field", Flare: "controlled"},
	"近景":  {MM: 65, TStop: "T1.8", Character: "fast portrait prime", DoF: "very shallow, creamy background falloff", Flare: "controlled"},
	"特写":  {MM: 85, TStop: "T1.8", Character: "vintage portrait glass with gentle softness", DoF: "razor-thin focus plane on the eyes", Flare: "mild anamorphic-style streak"},
	"大特写": {MM: 100, TStop: "T2.0", Character: "macro-capable prime", DoF: "extreme shallow focus on one detail", Flare: "mild"},
}

var cameraMoveToEn = map[string]string{
	"固定":   "locked-off tripod, completely static frame, no camera movement",
	"缓推":   "very slow dolly-in on a fluid head, steady, no shake",
	"缓拉":   "very slow dolly-out, steady, revealing more of the environment",
	"横移":   "slow lateral tracking move on a dolly, constant speed, level horizon",
	"摇":    "slow deliberate pan on a fluid head, smooth acceleration and settle",
	"手持跟随": "restrained handheld follow, subtle breathing motion only, no violent shake",
	"航拍":   "aerial drone move, slow and steady, level horizon, no roll",
	"升降":   "slow crane rise, steady vertical move, no roll",
	"环绕":   "slow arc around the subject, constant radius and speed",
}

// LensFor 按景别返回镜头参数（确定性映射，保证同景别光学一致）。
// 变奏：带轻微焦段抖动会破坏"同一部电影"的观感，因此只做固定偏移不随机。
func LensFor(shotSize string, variant int) Lens {
	l, ok := shotSizeLens[shotSize]
	if !ok {
		l = Lens{MM: 50, TStop: "T2.0", Character: "standard spherical prime", DoF: "shallow depth of field", Flare: "controlled"}
	}
	if variant > 0 {
		// 允许同景别在 1.4x 内变焦，模拟"换了一支头"，但保持同一玻璃家族
		l.MM = int(float64(l.MM) * 1.4)
	}
	l.SortKey = l.MM
	return l
}

// ShotSizeEn 中文景别 → 英文摄影描述。
func ShotSizeEn(shotSize string) string {
	if v, ok := shotSizeToEn[shotSize]; ok {
		return v
	}
	return shotSize
}

// CameraMoveEn 中文运镜 → 英文运镜描述。
func CameraMoveEn(move string) string {
	if v, ok := cameraMoveToEn[move]; ok {
		return v
	}
	return move
}

// cameraMoveAmplitude 运镜 → vidu 系 movement_amplitude 参数的映射。
//
// 为什么必须映射而不是一律 "auto"：auto 把幅度决定权交给模型，"缓推"会被
// 推成横摇、"固定"会有明显晃动——运镜的戏剧意图就丢了。幅度应服从分镜：
// 静态/缓动给 small，跟踪/摇移给 medium，大空间调度（航拍/升降/环绕）给 large。
var cameraMoveAmplitude = map[string]string{
	"固定":   "small",
	"缓推":   "small",
	"缓拉":   "small",
	"横移":   "medium",
	"摇":    "medium",
	"手持跟随": "medium",
	"航拍":   "large",
	"升降":   "large",
	"环绕":   "large",
}

// MovementAmplitude 把中文运镜映射为 vidu 系的运动幅度参数
// （small/medium/large；未知运镜回落 "auto"，让模型自行判断）。
func MovementAmplitude(move string) string {
	if v, ok := cameraMoveAmplitude[strings.TrimSpace(move)]; ok {
		return v
	}
	return "auto"
}

// ShotSizeEnum 合法景别枚举。
func ShotSizeEnum() []string {
	out := make([]string, 0, len(shotSizeToEn))
	for k := range shotSizeToEn {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CameraMoveEnum 合法运镜枚举。
func CameraMoveEnum() []string {
	out := make([]string, 0, len(cameraMoveToEn))
	for k := range cameraMoveToEn {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 风格预设
// ---------------------------------------------------------------------------

// StylePreset 内置风格预设。风格由代码固化，避免每次运行都得到不同的"电影感"。
type StylePreset struct {
	Name  string   `json:"name"`
	Alias []string `json:"alias"`
	Style Style    `json:"style"`
}

var stylePresets = []StylePreset{
	{
		Name:  "naturalism",
		Alias: []string{"写实", "纪实", "realistic", "natural", "剧情"},
		Style: Style{
			Name:   "contemporary naturalism",
			Aspect: "2.39:1",
			Camera: Camera{
				Name: "ARRI Alexa 35", Format: "Super 35 digital negative",
				Texture:   "14 stops of dynamic range with organic highlight rolloff",
				FilmStock: "Kodak Vision3 250D emulation",
			},
			Grade: Grade{
				Palette:  "muted naturalistic palette, desaturated skin, soft greens and warm neutrals",
				Contrast: "filmic S-curve, retained shadow detail",
				Grain:    "fine 35mm grain",
				Halation: "subtle halation on practicals",
			},
			Photoreal: photorealBase,
			TimeOfDay: "late afternoon",
			Score:     "restrained, human",
		},
	},
	{
		Name:  "scifi",
		Alias: []string{"科幻", "太空", "赛博", "未来", "sci-fi", "cyberpunk"},
		Style: Style{
			Name:   "grounded science fiction",
			Aspect: "2.39:1",
			Camera: Camera{
				Name: "ARRI Alexa Mini LF", Format: "large format digital negative",
				Texture:   "clinical clarity in highlights, deep shadow retention, minimal noise",
				FilmStock: "Kodak Vision3 500T emulation",
			},
			Grade: Grade{
				Palette:  "cool teal shadows against warm amber practicals, restrained saturation",
				Contrast: "high contrast with protected highlights",
				Grain:    "fine 35mm grain",
				Halation: "visible halation around bright practicals",
			},
			Photoreal: photorealBase,
			TimeOfDay: "night",
			Score:     "awe tinged with unease",
		},
	},
	{
		Name:  "warm",
		Alias: []string{"温情", "治愈", "怀旧", "warm", "家庭", "文艺"},
		Style: Style{
			Name:   "warm nostalgic realism",
			Aspect: "1.85:1",
			Camera: Camera{
				Name: "Kodak 35mm film camera", Format: "35mm photochemical negative",
				Texture:   "gentle grain structure, soft shoulder, natural color bleed",
				FilmStock: "Kodak Portra 400 emulation",
			},
			Grade: Grade{
				Palette:  "warm golden highlights, soft pastel shadows, creamy skin tones",
				Contrast: "low contrast, lifted blacks",
				Grain:    "pronounced organic film grain",
				Halation: "strong halation blooming around windows and lamps",
			},
			Photoreal: photorealBase,
			TimeOfDay: "golden hour",
			Score:     "tender, nostalgic",
		},
	},
	{
		Name:  "noir",
		Alias: []string{"悬疑", "黑色", "犯罪", "noir", "惊悚", "thriller"},
		Style: Style{
			Name:   "modern film noir",
			Aspect: "2.39:1",
			Camera: Camera{
				Name: "ARRI Alexa 35", Format: "Super 35 digital negative",
				Texture:   "deep shadow retention with crisp specular highlights",
				FilmStock: "Kodak Double-X 5222 emulation",
			},
			Grade: Grade{
				Palette:  "near-monochrome with a cold cyan cast and sodium-vapor accents",
				Contrast: "very high contrast, deep crushed blacks with edge light separation",
				Grain:    "coarse 35mm grain",
				Halation: "strong halation on hard sources",
			},
			Photoreal: photorealBase,
			TimeOfDay: "night",
			Score:     "tense, fatalistic",
		},
	},
	{
		Name:  "documentary",
		Alias: []string{"纪录片", "真实", "采访", "documentary", "vlog"},
		Style: Style{
			Name:   "observational documentary",
			Aspect: "1.78:1",
			Camera: Camera{
				Name: "Sony FX6", Format: "full frame digital",
				Texture:   "honest sensor rendering, slight noise in shadows, no beauty processing",
				FilmStock: "flat log profile graded for natural skin",
			},
			Grade: Grade{
				Palette:  "neutral documentary color, true skin tones, minimal stylization",
				Contrast: "gentle contrast, open shadows",
				Grain:    "very light grain",
				Halation: "minimal",
			},
			Photoreal: photorealBase,
			TimeOfDay: "daytime",
			Score:     "candid, unforced",
		},
	},
}

// PresetFor 按风格关键词匹配预设（brief 或剧本 style 字段里出现别名即可命中）。
// 命中多个时取最长别名，避免 "科幻温情" 这类复合描述选错基调。
func PresetFor(hint string) StylePreset {
	h := strings.ToLower(hint)
	best := stylePresets[0]
	bestLen := 0
	for _, p := range stylePresets {
		for _, a := range p.Alias {
			if strings.Contains(h, strings.ToLower(a)) && len([]rune(a)) > bestLen {
				best = p
				bestLen = len([]rune(a))
			}
		}
	}
	return best
}

// PresetByName 按预设名取风格。
func PresetByName(name string) (StylePreset, bool) {
	for _, p := range stylePresets {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return StylePreset{}, false
}

// AllPresets 返回全部预设名（供文档与 CLI 展示）。
func AllPresets() []string {
	out := make([]string, 0, len(stylePresets))
	for _, p := range stylePresets {
		out = append(out, p.Name)
	}
	return out
}
