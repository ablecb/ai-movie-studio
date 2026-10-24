package skills

import (
	"fmt"
	"strings"
	"sync"

	"aistudio/internal/ai"
	"aistudio/internal/promspec"
	"aistudio/internal/shotkit"
	"aistudio/internal/skill"
)

// assetsSkill 素材库阶段：分镜设计之前生成一致性素材。
//
// 顺序即生产语义（用户期望的工作流）：场景描述（script）→ 角色圣经（characters）
// → 素材（本阶段）→ 分镜（storyboard）→ …… → 视频（render）→ 配音（deliver）。
// 导演在分镜前先拿到"角色长什么样、场景长什么样"的视觉基准；角色参考帧落在
// output/refs/，后续渲染按 token 原样复用（图生图锁脸），一致性由此贯穿全片。
//
// 素材清单先规划后生成：一次便宜的文本 LLM 调用决定"需要哪几张"（反复出场的
// 角色加角度变体、同地点同时段的场景归并共用、显著状态变化加场景变体），
// 失败兜底回默认（每角色一帧 base、场景不归并）。费用集中在出图，预算熔断照常生效。
type assetsSkill struct{}

func (assetsSkill) Name() string { return "assets" }
func (assetsSkill) Description() string {
	return "素材库：生成角色参考帧与场景 establishing 图（一致性素材，分镜前的视觉基准）"
}

func (assetsSkill) Run(ctx *skill.Context) (any, error) {
	if ctx.Cfg == nil {
		return nil, fmt.Errorf("assets 阶段需要生产配置（引擎未注入 config）")
	}
	var scriptDoc struct {
		Style  string `json:"style"`
		Genre  string `json:"genre"`
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
	if err := ctx.DepOf("script", &scriptDoc); err != nil {
		return nil, err
	}
	bibleRaw, ok := ctx.Deps["characters"]
	if !ok {
		return nil, fmt.Errorf("缺少依赖阶段 characters 的输出")
	}

	// 风格：剧本关键词命中内置预设（确定性，无 LLM 调用）；
	// 后续 prompts/shotspec 阶段会进一步细化，参考帧按 token 复用不受影响。
	preset := promspec.PresetFor(scriptDoc.Style + " " + scriptDoc.Genre + " " + ctx.Brief)
	style := preset.Style

	chars, err := shotkit.CharactersFromBible(bibleRaw)
	if err != nil {
		return nil, err
	}
	sceneInputs := make([]shotkit.SceneInput, 0, len(scriptDoc.Scenes))
	for _, sc := range scriptDoc.Scenes {
		sceneInputs = append(sceneInputs, shotkit.SceneInput{
			ID: sc.ID, Location: sc.Location, TimeOfDay: sc.TimeOfDay, Tone: sc.Tone,
			Action: firstNonEmpty(sc.Action, sc.Conflict, sc.Turn),
		})
	}

	// 图像能力客户端：端点取 providers.image（文本/视觉/出图可分别在不同供应商）。
	// ctx.AI 是文本大脑，出图必须走派生客户端，否则（文本走 DeepSeek 等配置下）
	// 出图请求会带着文本端点的模型名发出去。
	imgc, err := shotkit.CapClient(ctx.AI, ctx.Cfg, ai.CapImage, ctx.Cfg.Providers.Image, ctx.Cfg.Image.Model)
	if err != nil {
		return nil, err
	}

	// 素材规划：AI 基于剧本决定"需要哪几张"。严格模式：规划调用失败即失败
	//（静默回退默认清单会改变素材规模），由引擎按阶段重试策略处理。
	plan, planErr := shotkit.PlanAssets(ctx.AI, ctx.Brief, sceneInputs, chars, func(f string, a ...any) {
		ctx.Log("  "+f, a...)
	})
	if planErr != nil {
		return nil, planErr
	}
	shotkit.SaveAssetPlan(ctx.WorkDir, plan) // 规划存档：编辑提示词弹窗与预览共用
	ctx.Log("  📋 素材规划（按剧本需要生成，单角色上限 %d 帧）:", shotkit.MaxFramesPerChar)
	for _, pc := range plan.Characters {
		ctx.Log("    · %s：%s", pc.Name, frameLabels(pc.Frames))
	}
	if n := shotkit.CountSceneGroups(sceneInputs); n < len(sceneInputs) {
		ctx.Log("    · 场景归并：%d 个场次 → %d 张基准图（同地点同时段共用一张）", len(sceneInputs), n)
	}

	// 参考帧质检与渲染/deliver 同源（providers.vision），共享花费账本。
	// 严格模式：质检已启用但视觉端点不可用 → 拒绝继续（静默跳过 = 以为查过、其实没查）。
	var refVision *ai.Client
	if ctx.Cfg.RefQCEnabled() && !ctx.AI.Mock() {
		rv, rerr := shotkit.CapClient(ctx.AI, ctx.Cfg, ai.CapVision, ctx.Cfg.Providers.Vision, ctx.Cfg.Models.Vision)
		if rerr != nil {
			return nil, rerr
		}
		refVision = rv
		if err := refVision.Ping(); err != nil {
			return nil, fmt.Errorf("参考帧质检已启用（ref_qc.mode=%s）但视觉端点探活失败：%w\n"+
				"  处理：修复 providers.vision 端点/密钥，或将 ref_qc.mode 设为 off 显式关闭质检后重跑", ctx.Cfg.RefQC.Mode, err)
		}
	}

	ctx.Log("  🧑‍🎨 角色参考帧: %d 个角色 %d 帧（风格 %s · %s）",
		len(chars), totalPlannedFrames(plan), style.Name, style.Aspect)
	ctx.Log("  🏞 场景 establishing 图: %d 个场次 → %d 张基准图 + %d 张状态变体（与角色帧并行）",
		len(sceneInputs), shotkit.CountSceneGroups(sceneInputs), len(plan.SceneStates))

	// 角色帧与场景图互不依赖（场景图不含人物、角色帧不含环境），两批并行生成，
	// 各自内部再按 image.workers 并发——墙钟时间约等于两批中较慢的一批。
	var refs []shotkit.RefResult
	var scenes []shotkit.SceneAsset
	var refErr, sceneErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		refs, refErr = shotkit.EnsureRefFrames(imgc, ctx.Cfg, ctx.WorkDir, &promspec.Kit{Style: style, Characters: chars}, plan, false, refVision)
		if refErr != nil {
			refErr = fmt.Errorf("角色参考帧生成失败: %w", refErr)
			return
		}
		for _, r := range refs {
			label := r.Name
			if r.Variant != "" && r.Variant != "base" {
				label += "·" + r.Variant
			}
			switch {
			case r.Error != "":
				ctx.Log("    ✗ %s: %s", label, r.Error)
			case r.QCStatus == "failed":
				ctx.Log("    ✗ %s: 质检未通过: %s", label, strings.Join(r.QCIssues, "；"))
			default:
				ctx.Log("    ✓ %s → %s（%s）", label, r.Path, r.QCSummary())
			}
		}
	}()
	go func() {
		defer wg.Done()
		ix := shotkit.LoadAssetIndex(ctx.WorkDir)
		scenes, sceneErr = shotkit.EnsureSceneAssets(imgc, ctx.Cfg, ctx.WorkDir, style, sceneInputs, ix.Scenes, plan, false)
		if sceneErr != nil {
			sceneErr = fmt.Errorf("场景素材生成失败: %w", sceneErr)
			return
		}
		for _, sc := range scenes {
			label := sc.SceneID + "（" + sc.Location + "）"
			if sc.Variant != "" {
				label += "·" + sc.Variant
			}
			if sc.Error != "" {
				ctx.Log("    ✗ %s: %s", label, sc.Error)
			} else {
				ctx.Log("    ✓ %s → %s", label, sc.File)
			}
		}
	}()
	wg.Wait()
	if refErr != nil {
		return nil, refErr
	}
	if sceneErr != nil {
		return nil, sceneErr
	}

	ix := shotkit.LoadAssetIndex(ctx.WorkDir)
	ix.MergeCharacters(refs)
	ix.MergeScenes(scenes)
	if err := ix.Save(ctx.WorkDir); err != nil {
		return nil, fmt.Errorf("写素材库索引失败: %w", err)
	}
	ctx.Log("  ✅ 素材库已更新: output/assets/index.json（渲染与素材库页面共用）")
	return ix, nil
}

// frameLabels 帧序列的展示标签（基准/变体+理由）。
func frameLabels(frames []shotkit.RefFrameSpec) string {
	labels := make([]string, 0, len(frames))
	for _, fr := range frames {
		switch {
		case fr.Variant == "" || fr.Variant == "base":
			labels = append(labels, "基准")
		case fr.Purpose != "":
			labels = append(labels, fr.Variant+"（"+fr.Purpose+"）")
		default:
			labels = append(labels, fr.Variant)
		}
	}
	return strings.Join(labels, "、")
}

// totalPlannedFrames 规划的总角色帧数（含 base）。
func totalPlannedFrames(plan *shotkit.AssetPlan) int {
	n := 0
	for _, pc := range plan.Characters {
		n += len(pc.Frames)
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
