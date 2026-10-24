package shotkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"aistudio/internal/ai"
	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
)

// 素材规划：AI 基于剧本与角色圣经决定"需要哪几张素材"，而不是一刀切的一角色一帧、
// 一场景一张。规则（与制片逻辑对齐）：
//
//   - 角色：参考帧是渲染的"锁脸锚点"，主锚点（base）永远有且只有一张；变体帧
//     （侧面/背面/全身/特写/换装/伤妆）只在剧本动作确实需要该角度或状态时才加——
//     出现 20 次但全是正面中景的角色一张就够，出现 2 次但有背影的角色需要背影帧。
//   - 场景：同一地点同一时段全片共用一张 establishing 图（复用同一锚点才是
//     一致性，多张略有差异的图反而漂移）；需要多张的是时段/状态变化（雨夜/被毁），
//     由 scene_states 声明为附加变体。

// MaxFramesPerChar 单角色帧数上限（含 base）：一致性靠锚点质量，不靠数量堆叠。
const MaxFramesPerChar = 4

// MaxSceneStatesPerScene 单场次状态变体上限。
const MaxSceneStatesPerScene = 2

// RefFrameSpec 角色的一帧参考图规格。
type RefFrameSpec struct {
	// Variant 帧变体：base（主锚点）/ side / back / full_body / closeup /
	// wardrobe-xxx（换装）/ state-xxx（伤妆等状态）。小写连字符 slug。
	Variant string `json:"variant"`
	// Purpose 规划理由（中文，展示用：为什么需要这一帧）。
	Purpose string `json:"purpose,omitempty"`
	// DirectiveEN 英文画面指令（该变体的构图/角度/状态描述），进提示词。
	DirectiveEN string `json:"directive_en,omitempty"`
	// PromptOverride 人工编辑过的提示词（Web 弹窗编辑后回传）。非空时直接
	// 使用该提示词，不再从角色圣经/风格编译——质检仍按圣经核对。
	PromptOverride string `json:"prompt,omitempty"`
}

// PlannedChar 一个角色的素材规划。
type PlannedChar struct {
	Name   string         `json:"name"`
	Frames []RefFrameSpec `json:"frames"`
}

// PlannedSceneState 场景状态变体：某场次的环境发生显著变化（雨夜/被毁/断电），
// 在组图（同地点同时段共用基准图）之外追加的附加参考。
type PlannedSceneState struct {
	SceneIDs    []string `json:"scene_ids"`
	State       string   `json:"state"`
	Purpose     string   `json:"purpose,omitempty"`
	DirectiveEN string   `json:"directive_en,omitempty"`
}

// AssetPlan 素材清单。nil 计划 = 旧行为（每角色一帧 base、场景不归并）。
type AssetPlan struct {
	Characters  []PlannedChar
	SceneStates []PlannedSceneState
}

// FramesFor 返回某角色的帧序列（base 恒在首位）；未规划的角色只有 base。
func (p *AssetPlan) FramesFor(name string) []RefFrameSpec {
	if p != nil {
		for _, pc := range p.Characters {
			if pc.Name == name && len(pc.Frames) > 0 {
				return pc.Frames
			}
		}
	}
	return []RefFrameSpec{{Variant: "base"}}
}

// planPath 规划文件：output/assets/plan.json。持久化规划使"编辑提示词重出"
// 弹窗能展示与上次一致的帧清单（含变体与人工改过的提示词）。
func planPath(runDir string) string {
	return filepath.Join(runDir, "output", "assets", "plan.json")
}

// SaveAssetPlan 持久化素材规划（失败静默——规划存档是增强，不是依赖）。
func SaveAssetPlan(runDir string, p *AssetPlan) {
	if p == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(planPath(runDir)), 0o755)
	_ = fsx.AtomicWriteJSON(planPath(runDir), p)
}

// LoadAssetPlan 读取持久化的规划；不存在返回 nil。
func LoadAssetPlan(runDir string) *AssetPlan {
	p := &AssetPlan{}
	if err := fsx.ReadJSON(planPath(runDir), p); err != nil {
		return nil
	}
	if len(p.Characters) == 0 && len(p.SceneStates) == 0 {
		return nil
	}
	return p
}

// MergePromptOverrides 把人工编辑的提示词覆盖进规划（按 name+variant 匹配）。
// 只搬 PromptOverride，规划里的其它字段（变体增删/理由）以既有规划为准。
func MergePromptOverrides(plan, overrides *AssetPlan) {
	if plan == nil || overrides == nil {
		return
	}
	for _, oc := range overrides.Characters {
		for pi := range plan.Characters {
			if plan.Characters[pi].Name != oc.Name {
				continue
			}
			for _, of := range oc.Frames {
				if strings.TrimSpace(of.PromptOverride) == "" {
					continue
				}
				for fi := range plan.Characters[pi].Frames {
					if plan.Characters[pi].Frames[fi].Variant == of.Variant {
						plan.Characters[pi].Frames[fi].PromptOverride = of.PromptOverride
					}
				}
			}
		}
	}
}

// RefPromptFor 编译某一帧实际发送的提示词：人工覆盖优先，否则 base 走标准像、
// 变体走变体指令。生成与提示词预览共用此入口，保证"所见即所发"。
func RefPromptFor(style promspec.Style, c promspec.CharacterSpec, fr RefFrameSpec) string {
	if strings.TrimSpace(fr.PromptOverride) != "" {
		return fr.PromptOverride
	}
	if fr.Variant == "" || fr.Variant == "base" {
		return promspec.RefPrompt(style, c)
	}
	return variantRefPrompt(style, c, fr)
}

// DefaultAssetPlan 兜底规划：每角色一张 base，无场景状态变体。
// 规划 LLM 失败/离线演示时的行为与旧版素材阶段完全一致。
func DefaultAssetPlan(chars []promspec.CharacterSpec) *AssetPlan {
	p := &AssetPlan{}
	for _, c := range chars {
		p.Characters = append(p.Characters, PlannedChar{
			Name: c.Name, Frames: []RefFrameSpec{{Variant: "base"}},
		})
	}
	return p
}

// planSystemPrompt 素材规划师的系统提示词：规则即制片约束。
const planSystemPrompt = `你是电影制片的数据管理员，负责为一致性素材库规划"需要生成哪几张参考图"。
角色参考帧是渲染时图生图锁脸的锚点：主锚点（base）全片唯一；多角度帧帮助多参考模型锁定侧脸/全身。
场景 establishing 图是场次美术基准：同一地点同一时段全片共用一张，复用同一锚点才有一致性。

规划规则（严格遵守）：
1. 每个角色必须有且仅有一个 base 帧（正面标准像）。
2. 变体帧只在剧本动作/剧情明确需要时添加：动作里出现"转身/背影"→ back 或 side；"倒地/全身/身形"→ full_body；
   "面部特写/眼神/瞳孔"→ closeup；剧情明确换装 → wardrobe-xxx；受伤/变身等状态 → state-xxx。
   变体必须给出 directive_en（英文画面指令）。判断不了就不要加——多一张没被镜头用到的帧就是白花钱。
3. 反复出场（≥2 场）的核心角色，若剧本没有明确角度线索，最多补 side 和 full_body 两张通用变体；
   只出场一次的角色一律只有 base。
4. 每个角色总帧数不超过 4（含 base）。
5. scene_states 仅当剧本明确写出某场戏中场地发生显著状态变化（大雨/被摧毁/断电/爆炸后）才输出；
   state 用小写连字符英文 slug（如 rain-night、destroyed），并给 directive_en。没有就输出空数组。
6. 只输出 JSON，不要 markdown、不要解释。

输出 schema：
{"characters":[{"name":"角色名","frames":[{"variant":"base","purpose":"中文理由","directive_en":"英文指令（base 可省略）"}]}],
 "scene_states":[{"scene_ids":["s6"],"state":"rain-night","purpose":"中文理由","directive_en":"英文指令"}]}`

// knownVariants 有默认英文指令的角度变体；其余 slug 必须自带 directive_en。
var knownVariants = map[string]string{
	"base":      "three-quarter head-and-shoulders portrait plus a full-body front view of the SAME person",
	"side":      "side profile head-and-shoulders portrait, head turned 90 degrees, full side of the face clearly visible",
	"back":      "rear view from directly behind, back of head and shoulders visible, face not visible",
	"full_body": "full-body shot from head to feet, neutral standing pose, entire silhouette readable",
	"closeup":   "tight close-up of the face filling the frame, eyes sharp, neutral expression",
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// PlanAssets 生成素材规划：一次便宜的文本 LLM 调用。
// 离线模式/无角色/无场次等"没有规划对象"的情况按默认清单处理（非错误）；
// 严格模式：真实调用失败、输出无法解析都直接失败——静默回退默认清单会改变
// 素材规模（不加角度变体、不归并场景），属于产物级劣化。
func PlanAssets(client *ai.Client, brief string, scenes []SceneInput, chars []promspec.CharacterSpec,
	logf func(format string, args ...any)) (*AssetPlan, error) {
	def := DefaultAssetPlan(chars)
	if client == nil || client.Mock() || len(chars) == 0 || len(scenes) == 0 {
		return def, nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "创作意图：%s\n\n角色圣经：\n", firstLine(brief))
	for _, c := range chars {
		fmt.Fprintf(&b, "- %s｜外形：%s｜服装：%s\n", c.Name, truncRunes(c.Face, 80), truncRunes(c.Wardrobe, 60))
	}
	b.WriteString("\n场次（按剧本顺序）：\n")
	for _, sc := range scenes {
		fmt.Fprintf(&b, "- %s｜地点：%s｜时段：%s｜画面：%s\n",
			sc.ID, sc.Location, sc.TimeOfDay, truncRunes(sc.Action, 160))
	}

	// 输出预算必须显式给足：思考型模型（glm-5.3-flash）的思考内容计入输出预算，
	// 本调用又是"长提示词 + 结构化 JSON"——走全局默认（4096）时思考把预算耗尽、
	// 正文一个字都写不出来（真实失败过两次：两次请求 max_tokens 都是 4096）。
	// 与 skills 的 callTune 同口径：结构化产出低温 + 给"思考 + JSON"双份空间。
	text, err := client.ChatOpts("assets.plan", planSystemPrompt, b.String(),
		ai.CallOpts{Temperature: 0.4, MaxTokens: 16384})
	if err != nil {
		return nil, fmt.Errorf("素材规划调用失败: %w", err)
	}
	var raw rawPlan
	extracted, err := ai.ExtractJSON(text)
	if err == nil {
		err = json.Unmarshal(extracted, &raw)
	}
	if err != nil {
		return nil, fmt.Errorf("素材规划输出解析失败: %w", err)
	}
	return sanitizePlan(&raw, scenes, chars, logf), nil
}

// rawPlan 规划模型输出的原始 schema。
type rawPlan struct {
	Characters  []PlannedChar       `json:"characters"`
	SceneStates []PlannedSceneState `json:"scene_states"`
}

// sanitizePlan 清洗规划输出：base 恒在首位、slug 合法、去重、限流；
// 未覆盖的角色补 base。规划是省钱手段，清洗兜底必须保守。
func sanitizePlan(raw *rawPlan, scenes []SceneInput, chars []promspec.CharacterSpec, logf func(string, ...any)) *AssetPlan {
	byName := make(map[string]string, len(chars)) // 宽松名匹配：规划输出名 → 圣经标准名
	for _, c := range chars {
		byName[c.Name] = c.Name
		for _, pc := range raw.Characters {
			if pc.Name != c.Name && (strings.Contains(pc.Name, c.Name) || strings.Contains(c.Name, pc.Name)) {
				byName[pc.Name] = c.Name
			}
		}
	}

	p := &AssetPlan{}
	seen := map[string]bool{}
	for _, pc := range raw.Characters {
		name, ok := byName[pc.Name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		frames := []RefFrameSpec{{Variant: "base"}}
		for _, fr := range pc.Frames {
			v := strings.ToLower(strings.TrimSpace(fr.Variant))
			if v == "" || v == "base" {
				continue
			}
			if !slugRe.MatchString(v) {
				logf("  ⚠ 规划清洗：丢弃非法变体 %q（%s）", fr.Variant, name)
				continue
			}
			d := strings.TrimSpace(fr.DirectiveEN)
			if d == "" {
				if def, ok := knownVariants[v]; ok {
					d = def
				} else {
					logf("  ⚠ 规划清洗：丢弃无英文指令的变体 %q（%s）", v, name)
					continue
				}
			}
			if len(frames) >= MaxFramesPerChar {
				logf("  ⚠ 规划清洗：%s 超过 %d 帧上限，多余变体已裁剪", name, MaxFramesPerChar)
				break
			}
			frames = append(frames, RefFrameSpec{Variant: v, Purpose: strings.TrimSpace(fr.Purpose), DirectiveEN: d})
		}
		p.Characters = append(p.Characters, PlannedChar{Name: name, Frames: frames})
	}
	for _, c := range chars { // 规划漏掉的角色补 base
		if !seen[c.Name] {
			p.Characters = append(p.Characters, PlannedChar{
				Name: c.Name, Frames: []RefFrameSpec{{Variant: "base"}},
			})
		}
	}

	validScene := map[string]bool{}
	for _, sc := range scenes {
		validScene[sc.ID] = true
	}
	statesPerScene := map[string]int{}
	claimed := map[string]bool{} // 全局去重：不同条目声明同一（场次，状态）时只保留先到者
	for _, ss := range raw.SceneStates {
		st := strings.ToLower(strings.TrimSpace(ss.State))
		if !slugRe.MatchString(st) {
			logf("  ⚠ 规划清洗：丢弃非法场景状态 %q", ss.State)
			continue
		}
		ids := map[string]bool{}
		for _, sid := range ss.SceneIDs {
			if !validScene[sid] || statesPerScene[sid] >= MaxSceneStatesPerScene {
				continue
			}
			if ids[sid] || claimed[sid+"\x00"+st] {
				continue
			}
			ids[sid] = true
			claimed[sid+"\x00"+st] = true
			statesPerScene[sid]++
		}
		if len(ids) == 0 {
			continue
		}
		p.SceneStates = append(p.SceneStates, PlannedSceneState{
			SceneIDs:    sortedKeys(ids),
			State:       st,
			Purpose:     strings.TrimSpace(ss.Purpose),
			DirectiveEN: strings.TrimSpace(ss.DirectiveEN),
		})
	}
	return p
}

// sceneGroupKey 场景归并键：同地点同时段的场次共用一张 establishing 图。
// CJK 文本里的空格纯属书写差异（"烂尾楼天台"="烂尾楼 天台"），归一时全部去掉。
func sceneGroupKey(sc SceneInput) string {
	strip := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if !unicode.IsSpace(r) {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return strip(sc.Location) + "|" + strip(sc.TimeOfDay)
}

// sceneGroup 一个归并组：rep 是出图代表场次（剧本顺序第一个），成员共享组图。
type sceneGroup struct {
	Key     string
	Rep     int
	Members []int
}

// groupSceneInputs 按（地点+时段）归并场次，保持剧本顺序。
func groupSceneInputs(scenes []SceneInput) []sceneGroup {
	var out []sceneGroup
	indexOf := map[string]int{}
	for i, sc := range scenes {
		k := sceneGroupKey(sc)
		if gi, ok := indexOf[k]; ok {
			out[gi].Members = append(out[gi].Members, i)
			continue
		}
		indexOf[k] = len(out)
		out = append(out, sceneGroup{Key: k, Rep: i, Members: []int{i}})
	}
	return out
}

// CountSceneGroups 归并后的场景基准图数量（供日志/统计展示）。
func CountSceneGroups(scenes []SceneInput) int { return len(groupSceneInputs(scenes)) }

// variantDirective 变体帧的英文画面指令：规划给了用规划的，否则用角度默认值。
func variantDirective(fr RefFrameSpec) string {
	if d := strings.TrimSpace(fr.DirectiveEN); d != "" {
		return d
	}
	if d, ok := knownVariants[fr.Variant]; ok {
		return d
	}
	return "alternate view of the same person"
}

// variantRefPrompt 变体帧提示词：同一人物的指定角度/状态帧。
// 变体以主锚点帧为图生图输入，身份锁定靠参考图；提示词负责角度与状态语义。
func variantRefPrompt(style promspec.Style, c promspec.CharacterSpec, fr RefFrameSpec) string {
	var b strings.Builder
	b.WriteString("Character reference frame for a film production, neutral studio lighting, ")
	b.WriteString("plain mid-grey seamless background.\n")
	b.WriteString("SAME PERSON as the reference image, identical facial structure, identical hair, ")
	b.WriteString("identical identity anchors.\n")
	fmt.Fprintf(&b, "Character: %s.\n", c.AnchorText())
	if len(c.Distinctive) > 0 {
		fmt.Fprintf(&b, "Identity anchors that MUST be preserved: %s.\n", strings.Join(c.Distinctive, ", "))
	}
	if w := c.WardrobeText(); w != "" && !strings.HasPrefix(fr.Variant, "wardrobe-") {
		// 换装变体的服装由 directive_en 描述，避免与圣经服装自相矛盾
		fmt.Fprintf(&b, "Wardrobe: %s.\n", w)
	}
	fmt.Fprintf(&b, "Frame: %s.\n", variantDirective(fr))
	fmt.Fprintf(&b, "Camera: 85mm prime at T2.8, sharp focus, no distortion.\n")
	fmt.Fprintf(&b, "Look: %s, %s.\n", style.Camera.Format, style.Camera.Texture)
	if style.Grade.Palette != "" {
		fmt.Fprintf(&b, "Color: %s.\n", style.Grade.Palette)
	}
	b.WriteString("Requirements: photorealistic, identical person to the reference image, ")
	b.WriteString("visible skin pores and natural complexion, no retouching, no beauty filter.\n")
	fmt.Fprintf(&b, "Avoid: %s.", promspec.NegativePrompt("face", "anatomy", "style", "artifacts"))
	return b.String()
}

// ResolveStyle 全片风格解析（素材/预览共用）：shotplan → shotspec → 剧本关键词
// 命中预设 → naturalism 兜底。与渲染、CLI 素材命令同源。
func ResolveStyle(runDir string) (promspec.Style, error) {
	var holder struct {
		Style promspec.Style `json:"style"`
	}
	for _, p := range []string{
		filepath.Join(runDir, "output", "shotplan.json"),
		filepath.Join(runDir, "artifacts", "shotspec.json"),
	} {
		if data, err := os.ReadFile(p); err == nil {
			if json.Unmarshal(data, &holder) == nil && holder.Style.Name != "" {
				return holder.Style, nil
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(runDir, "artifacts", "script.json")); err == nil {
		var doc struct {
			Style string `json:"style"`
			Genre string `json:"genre"`
		}
		if json.Unmarshal(data, &doc) == nil {
			return promspec.PresetFor(doc.Style + " " + doc.Genre).Style, nil
		}
	}
	if p, ok := promspec.PresetByName("naturalism"); ok {
		return p.Style, nil
	}
	return promspec.Style{}, fmt.Errorf("无法确定全片风格（shotplan/shotspec/script 均缺失且默认预设不可用）")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // 场景数很少，插入排序足矣且保持稳定
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func truncRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncRunes(s, 120)
}
