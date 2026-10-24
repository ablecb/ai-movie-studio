package shotkit

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/conc"
	"aistudio/internal/config"
	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
)

// bibleCharacter 角色圣经（characters.json）的最小解析结构。
type bibleCharacter struct {
	Name              string `json:"name"`
	Appearance        string `json:"appearance"`
	Wardrobe          string `json:"wardrobe"`
	Voice             string `json:"voice"`
	TTSVoice          string `json:"tts_voice"`
	ConsistencyTokens string `json:"consistency_tokens"`
}

// CharactersFromBible 从角色圣经编译角色规格（与 shotspec 的兜底路径一致）。
//
// 素材阶段先于分镜/生成规格运行时没有结构化 CharacterSpec：Face 用圣经的
// appearance 填充（身份信息最全的自由文本），其余字段用与 shotspec 相同的
// 缺省值兜底，保证参考帧提示词的锚点非空。锚点缺失时从角色名派生，
// 保证参考帧文件名与多参考图路径可用。
func CharactersFromBible(raw []byte) ([]promspec.CharacterSpec, error) {
	var doc struct {
		Characters []bibleCharacter `json:"characters"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("角色圣经解析失败: %w", err)
	}
	if len(doc.Characters) == 0 {
		return nil, fmt.Errorf("角色圣经为空")
	}
	out := make([]promspec.CharacterSpec, 0, len(doc.Characters))
	for _, c := range doc.Characters {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		token := strings.TrimSpace(c.ConsistencyTokens)
		if token == "" {
			token = safeName(c.Name)
		}
		out = append(out, promspec.CharacterSpec{
			Name:        c.Name,
			Token:       token,
			Wardrobe:    c.Wardrobe,
			Voice:       c.Voice,
			TTSVoice:    c.TTSVoice,
			Face:        c.Appearance,
			Skin:        "natural skin with visible pores and fine texture",
			Distinctive: []string{"consistent facial structure across all shots"},
			Age:         "adult",
			Ethnicity:   "East Asian",
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("角色圣经中没有有效角色")
	}
	return out, nil
}

// SceneInputsFromScript 从剧本（script.json）编译场景素材输入：
// 动作描述优先 action，缺失依次回落 conflict / turn。
func SceneInputsFromScript(raw []byte) ([]SceneInput, error) {
	var doc struct {
		Scenes []struct {
			ID        string `json:"id"`
			Location  string `json:"location"`
			TimeOfDay string `json:"time_of_day"`
			Tone      string `json:"tone"`
			Action    string `json:"action"`
			Conflict  string `json:"conflict"`
			Turn      string `json:"turn"`
		} `json:"scenes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("剧本解析失败: %w", err)
	}
	if len(doc.Scenes) == 0 {
		return nil, fmt.Errorf("剧本中没有场次")
	}
	out := make([]SceneInput, 0, len(doc.Scenes))
	for _, sc := range doc.Scenes {
		action := strings.TrimSpace(sc.Action)
		if action == "" {
			action = strings.TrimSpace(sc.Conflict)
		}
		if action == "" {
			action = strings.TrimSpace(sc.Turn)
		}
		out = append(out, SceneInput{
			ID: sc.ID, Location: sc.Location, TimeOfDay: sc.TimeOfDay,
			Tone: sc.Tone, Action: action,
		})
	}
	return out, nil
}

// SceneInput 场景素材的源信息（来自 script.json 的场次）。
type SceneInput struct {
	ID        string
	Location  string
	TimeOfDay string
	Tone      string
	Action    string
}

// SceneAsset 场景 establishing 图素材（场次级的美术基准参考图）。
type SceneAsset struct {
	SceneID  string `json:"scene_id"`
	Location string `json:"location"`
	File     string `json:"file,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	// PromptSrcHash 场景源信息的哈希：场景描述没变就复用译文与图片，变了才重新生成。
	PromptSrcHash string `json:"prompt_src_hash,omitempty"`
	// Variant 场景图变体：""（主基准，同地点同时段的场次共享同一文件）/
	// 状态变体 slug（rain-night、destroyed…，从主基准图生图派生的附加参考）。
	Variant     string    `json:"variant,omitempty"`
	Purpose     string    `json:"purpose,omitempty"`
	Seed        int64     `json:"seed"`
	Model       string    `json:"model,omitempty"`
	Error       string    `json:"error,omitempty"`
	GeneratedAt time.Time `json:"generated_at,omitempty"`
}

// AssetIndex 素材库索引（output/assets/index.json）：
// 角色参考帧（渲染管线一致性消费的同一批文件）+ 场景 establishing 图。
type AssetIndex struct {
	UpdatedAt  time.Time    `json:"updated_at"`
	Characters []RefResult  `json:"characters,omitempty"`
	Scenes     []SceneAsset `json:"scenes,omitempty"`
}

// LoadAssetIndex 读取素材库索引；不存在返回空索引。
func LoadAssetIndex(runDir string) *AssetIndex {
	ix := &AssetIndex{}
	_ = fsx.ReadJSON(filepath.Join(runDir, "output", "assets", "index.json"), ix)
	return ix
}

// VariantOf 取某场次指定状态变体的场景图文件（相对 run 目录）；无该变体返回 ""。
// state 是素材规划声明的 slug（shotspec 的 scene_state 经校验后透传到这里）。
func (ix *AssetIndex) VariantOf(sceneID, state string) string {
	if sceneID == "" || state == "" {
		return ""
	}
	for _, sc := range ix.Scenes {
		if sc.SceneID == sceneID && sc.Variant == state && strings.TrimSpace(sc.File) != "" {
			return sc.File
		}
	}
	return ""
}

// BaselineOf 取某场次的主基准图（场景图文件路径，相对 run 目录）。
//
// 只认 Variant=="" 的主基准：同地点同时段的场次归并共享的就是这一张，正是
// "跨镜头同一个环境"的锚点。状态变体（雨夜/被毁…）是特定镜头的美术状态，
// 属于哪一镜由分镜决定，不能由渲染管线替它挑——挑错比不挑更坏。
// 无主基准时退回到该场次第一个带文件的条目：只有状态变体的 run 里，
// 那张也比"什么都没有"强（兜底方向与 EnsureSceneAssets 的复用规则一致）。
func (ix *AssetIndex) BaselineOf(sceneID string) string {
	if sceneID == "" {
		return ""
	}
	fallback := ""
	for _, sc := range ix.Scenes {
		if sc.SceneID != sceneID || strings.TrimSpace(sc.File) == "" {
			continue
		}
		if sc.Variant == "" {
			return sc.File
		}
		if fallback == "" {
			fallback = sc.File
		}
	}
	return fallback
}

// MergeCharacters 把本次处理的角色条目合并进索引（按 Token+变体覆盖，其余保留）。
func (ix *AssetIndex) MergeCharacters(fresh []RefResult) {
	if len(fresh) == 0 {
		return
	}
	freshOf := make(map[string]RefResult, len(fresh))
	for _, r := range fresh {
		freshOf[r.Token+"\x00"+r.Variant] = r
	}
	out := make([]RefResult, 0, len(ix.Characters)+len(fresh))
	seen := map[string]bool{}
	for _, r := range ix.Characters {
		if f, ok := freshOf[r.Token+"\x00"+r.Variant]; ok {
			out = append(out, f)
			seen[r.Token+"\x00"+r.Variant] = true
		} else {
			out = append(out, r)
		}
	}
	for _, r := range fresh {
		if !seen[r.Token+"\x00"+r.Variant] {
			out = append(out, r)
		}
	}
	ix.Characters = out
}

// MergeScenes 把本次处理的场景条目合并进索引（按 SceneID+变体覆盖，其余保留）。
func (ix *AssetIndex) MergeScenes(fresh []SceneAsset) {
	if len(fresh) == 0 {
		return
	}
	freshOf := make(map[string]SceneAsset, len(fresh))
	for _, sc := range fresh {
		freshOf[sc.SceneID+"\x00"+sc.Variant] = sc
	}
	out := make([]SceneAsset, 0, len(ix.Scenes)+len(fresh))
	seen := map[string]bool{}
	for _, sc := range ix.Scenes {
		if f, ok := freshOf[sc.SceneID+"\x00"+sc.Variant]; ok {
			out = append(out, f)
			seen[sc.SceneID+"\x00"+sc.Variant] = true
		} else {
			out = append(out, sc)
		}
	}
	for _, sc := range fresh {
		if !seen[sc.SceneID+"\x00"+sc.Variant] {
			out = append(out, sc)
		}
	}
	ix.Scenes = out
}

// Save 原子写入索引。
func (ix *AssetIndex) Save(runDir string) error {
	dir := filepath.Join(runDir, "output", "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ix.UpdatedAt = time.Now()
	return fsx.AtomicWriteJSON(filepath.Join(dir, "index.json"), ix)
}

// EnsureSceneAssets 生成/复用场景 establishing 图（按（地点+时段）归并共享，支持状态变体）。
//
// 场景图是场次级的"美术基准"：整场戏所在环境/光线/情绪的一张定调图，
// 供人工核对环境一致性，也可作为后续图生图的参考素材。
// 归并规则：同地点同时段的场次共用一张组图——同一锚点反复出现才有一致性，
// 多张略有差异的"同一场景"反而是漂移源；plan 声明的状态变体（雨夜/被毁…）
// 从组图图生图派生为附加参考，不改主基准。plan 为 nil 时行为 = 逐场次出图（不归并）。
// 断点复用规则：文件在、源信息未变、上次成功 → 直接复用；
// --force 或场景描述变了才重新出图（描述变更 = 环境基准应当更新）。
// 译文缓存在索引里：场景描述不变不重复花翻译钱。
func EnsureSceneAssets(client *ai.Client, cfg *config.Config, runDir string,
	style promspec.Style, scenes []SceneInput, prev []SceneAsset, plan *AssetPlan, force bool) ([]SceneAsset, error) {

	dir := filepath.Join(runDir, "output", "assets", "scenes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	workers := cfg.Image.Workers
	if workers <= 0 {
		workers = 3
	}
	// 文本能力客户端（场景提示词翻译）：端点取 providers.text——
	// 场景描述翻译与出图可能在不同供应商，翻译必须走文本端点。
	textc, err := CapClient(client, cfg, ai.CapText, cfg.Providers.Text, cfg.Providers.Text.Model)
	if err != nil {
		return nil, err
	}
	prevOf := make(map[string]SceneAsset, len(prev))
	for _, p := range prev {
		prevOf[p.SceneID+"\x00"+p.Variant] = p
	}
	if plan == nil {
		plan = &AssetPlan{}
	}

	// ---- 1) 组图：同（地点+时段）一组，每组一次出图；rep 取剧本顺序第一个 ----
	groups := groupSceneInputs(scenes)
	groupAssets := make([]SceneAsset, len(groups))
	var fatalErr error
	var failed atomic.Bool
	var mu sync.Mutex
	conc.ForEachIdx(len(groups), workers, func(gi int) {
		if failed.Load() {
			return
		}
		rep := scenes[groups[gi].Rep]
		rel := filepath.ToSlash(filepath.Join("output", "assets", "scenes", safeName(rep.ID)+".png"))
		abs := filepath.Join(runDir, rel)
		srcHash := sceneSrcHash(rep)
		prevKey := rep.ID + "\x00"

		res := SceneAsset{SceneID: rep.ID, Location: rep.Location, Seed: DeriveSeed(rep.ID, 0, "scene-asset")}
		if p, ok := prevOf[prevKey]; ok {
			res.Seed = p.Seed
		}
		if !force && fsx.FileExists(abs) {
			if p, ok := prevOf[prevKey]; ok && p.Error == "" && p.PromptSrcHash == srcHash {
				// 复用：文件在、上次成功、场景描述没变
				p.File = rel
				groupAssets[gi] = p
				return
			}
		}

		prompt := composeScenePrompt(style, rep)
		// 场景描述翻译成英文工业提示词（模型对英文提示词服从度更高）；
		// 译文缓存在索引里，场景没变不重复翻译。
		// 严格模式：翻译失败即失败——静默回落中文提示词是产物质量的隐性劣化。
		if !client.Mock() {
			if p, ok := prevOf[prevKey]; ok && p.Error == "" && p.PromptSrcHash == srcHash && strings.TrimSpace(p.Prompt) != "" {
				prompt = p.Prompt
			} else {
				en, terr := translateScene(textc, rep)
				if terr != nil {
					res.Error = "场景提示词翻译失败: " + terr.Error()
					groupAssets[gi] = res
					if ai.IsFatal(terr) {
						mu.Lock()
						fatalErr = terr
						mu.Unlock()
						failed.Store(true)
					}
					return
				}
				if strings.TrimSpace(en) == "" {
					res.Error = "场景提示词翻译返回空结果"
					groupAssets[gi] = res
					return
				}
				prompt = composeScenePromptEN(style, en)
			}
		}
		res.Prompt = prompt
		res.PromptSrcHash = srcHash

		url, model, err := client.ImageGenEx("assets.scene_base", prompt, ai.ImageOptions{
			Model:     cfg.Image.Model,
			Size:      cfg.Image.Size,
			Quality:   cfg.Image.Quality,
			Watermark: cfg.Image.Watermark,
		})
		if err != nil {
			res.Error = err.Error()
			groupAssets[gi] = res
			if ai.IsFatal(err) {
				mu.Lock()
				fatalErr = err
				mu.Unlock()
				failed.Store(true)
			}
			return
		}
		if err := Download(url, abs); err != nil {
			res.Error = err.Error()
			groupAssets[gi] = res
			return
		}
		ai.AttachCallArtifact(url, rel) // 调用记录回填本地路径（平台 URL 会过期）
		res.File = rel
		res.Model = model
		res.GeneratedAt = time.Now()
		groupAssets[gi] = res
	})
	if fatalErr != nil {
		return nil, fatalErr
	}

	// ---- 2) 每个场次一条主基准条目（同组成员共享组图文件与缓存） ----
	out := make([]SceneAsset, 0, len(scenes))
	for gi, g := range groups {
		for _, mi := range g.Members {
			sc := scenes[mi]
			entry := groupAssets[gi]
			entry.SceneID = sc.ID
			entry.Location = sc.Location
			out = append(out, entry)
		}
	}

	// ---- 3) 状态变体：从组图图生图派生，主基准不动 ----
	idxOf := make(map[string]int, len(scenes))
	for i, sc := range scenes {
		idxOf[sc.ID] = i
	}
	groupOf := make([]int, len(scenes))
	for gi, g := range groups {
		for _, mi := range g.Members {
			groupOf[mi] = gi
		}
	}
	type stateJob struct {
		sc   SceneInput
		st   PlannedSceneState
		base SceneAsset
	}
	var jobs []stateJob
	for _, st := range plan.SceneStates {
		for _, sid := range st.SceneIDs {
			mi, ok := idxOf[sid]
			if !ok {
				continue
			}
			base := groupAssets[groupOf[mi]]
			if base.Error != "" || base.File == "" {
				continue // 没有主基准就没有派生锚点
			}
			jobs = append(jobs, stateJob{sc: scenes[mi], st: st, base: base})
		}
	}
	conc.ForEachIdx(len(jobs), workers, func(ji int) {
		if failed.Load() {
			return
		}
		job := jobs[ji]
		rel := filepath.ToSlash(filepath.Join("output", "assets", "scenes",
			safeName(job.sc.ID+"_"+job.st.State)+".png"))
		abs := filepath.Join(runDir, rel)
		srcHash := sceneStateHash(job.base.PromptSrcHash, job.st.State)
		prevKey := job.sc.ID + "\x00" + job.st.State

		res := SceneAsset{SceneID: job.sc.ID, Location: job.sc.Location, Variant: job.st.State,
			Purpose: job.st.Purpose, Seed: DeriveSeed(job.sc.ID+"-"+job.st.State, 0, "scene-asset")}
		if p, ok := prevOf[prevKey]; ok {
			res.Seed = p.Seed
		}
		if !force && fsx.FileExists(abs) {
			if p, ok := prevOf[prevKey]; ok && p.Error == "" && p.PromptSrcHash == srcHash {
				p.File = rel
				mu.Lock()
				out = append(out, p)
				mu.Unlock()
				return
			}
		}
		cond := strings.TrimSpace(job.st.DirectiveEN)
		if cond == "" {
			cond = strings.TrimSpace(job.st.Purpose) // 中文回落：与主基准提示词同一容忍度
		}
		if cond == "" {
			cond = strings.ReplaceAll(job.st.State, "-", " ")
		}
		res.Prompt = job.base.Prompt + "\nScene condition variant: " + cond
		res.PromptSrcHash = srcHash

		opt := ai.ImageOptions{
			Model:     cfg.Image.Model,
			Size:      cfg.Image.Size,
			Quality:   cfg.Image.Quality,
			Watermark: cfg.Image.Watermark,
		}
		// 从组图派生：变化的是状态，不变的环境由参考图保证。
		// 严格模式：读不出基准图即失败——静默降级为纯文生图会让"环境不变"的保证失效
		ref, rerr := ReadAsDataURL(filepath.Join(runDir, filepath.FromSlash(job.base.File)))
		if rerr != nil {
			res.Error = "读取场景基准图失败: " + rerr.Error()
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
			return
		}
		opt.RefImage = ref
		url, model, err := client.ImageGenEx("assets.scene_state", res.Prompt, opt)
		if err != nil {
			res.Error = err.Error()
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
			if ai.IsFatal(err) {
				mu.Lock()
				fatalErr = err
				mu.Unlock()
				failed.Store(true)
			}
			return
		}
		if err := Download(url, abs); err != nil {
			res.Error = "下载状态变体失败: " + err.Error()
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
			return
		}
		ai.AttachCallArtifact(url, rel)
		res.File = rel
		res.Model = model
		res.GeneratedAt = time.Now()
		mu.Lock()
		out = append(out, res)
		mu.Unlock()
	})
	if fatalErr != nil {
		return nil, fatalErr
	}
	var fails []string
	for _, s := range out {
		if s.Error != "" {
			fails = append(fails, s.SceneID+": "+s.Error)
		}
	}
	// 严格模式：场景素材是首帧/尾帧锚点的环境基准，任何一张失败都拒绝继续
	if len(fails) > 0 {
		return out, fmt.Errorf("场景素材未全部就绪，拒绝继续（%d 张失败）：\n  - %s",
			len(fails), strings.Join(fails, "\n  - "))
	}
	return out, nil
}

// composeScenePrompt 场景 establishing 图提示词：全局风格锚点 + 场景内容（中文回落）。
func composeScenePrompt(style promspec.Style, sc SceneInput) string {
	content := fmt.Sprintf("Location: %s. Time of day: %s. Mood: %s. Key content: %s",
		sc.Location, sc.TimeOfDay, sc.Tone, sc.Action)
	return composeScenePromptEN(style, content)
}

func composeScenePromptEN(style promspec.Style, content string) string {
	var b strings.Builder
	b.WriteString("Cinematic establishing shot for a live-action feature film, no people in focus, no text, no subtitles, no watermark.\n")
	if len(style.Photoreal) > 0 {
		b.WriteString(style.Photoreal[0])
		b.WriteString(".\n")
	}
	if n := len(style.Photoreal); n > 3 {
		for _, line := range style.Photoreal[1:4] {
			b.WriteString(line)
			b.WriteString(".\n")
		}
	}
	if style.Grade.Palette != "" {
		fmt.Fprintf(&b, "Color grade: %s.\n", style.Grade.Palette)
	}
	if style.Grade.Grain != "" {
		fmt.Fprintf(&b, "Texture: %s.\n", style.Grade.Grain)
	}
	b.WriteString("Scene: ")
	b.WriteString(content)
	b.WriteString(".")
	return b.String()
}

// translateScene 把中文场次描述翻成英文 establishing-shot 提示词。
func translateScene(client *ai.Client, sc SceneInput) (string, error) {
	out, err := client.Chat("assets.scene_translate",
		"You are a film production translator. Convert the given scene description into ONE concise English "+
			"cinematographic establishing-shot description (2-3 sentences, concrete visual language: environment, "+
			"lighting, atmosphere). Output ONLY the English description, no markdown, no explanations.",
		fmt.Sprintf("场景地点：%s\n时段：%s\n情绪基调：%s\n画面内容：%s",
			sc.Location, sc.TimeOfDay, sc.Tone, sc.Action))
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(out, "```")
	out = strings.TrimSpace(out)
	if len(out) > 1200 {
		out = out[:1200]
	}
	return out, nil
}

func sceneSrcHash(sc SceneInput) string {
	h := sha1.Sum([]byte(sc.Location + "\x1f" + sc.TimeOfDay + "\x1f" + sc.Tone + "\x1f" + sc.Action))
	return hex.EncodeToString(h[:8])
}

// sceneStateHash 状态变体的源哈希：主基准源信息或状态名任一变化都触发重新派生。
func sceneStateHash(baseHash, state string) string {
	h := sha1.Sum([]byte(baseHash + "\x1f" + state))
	return hex.EncodeToString(h[:8])
}
