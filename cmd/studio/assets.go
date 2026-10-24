package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
	"aistudio/internal/shotkit"
)

// cmdAssets 素材库：基于剧本生成一致性素材。
//
// 角色参考帧与 render 共用同一批文件（output/refs/）与同一套圣经核对质检——
// 这里生成的素材渲染时直接复用（同 token 同文件即同一张脸）。
// 场景 establishing 图落在 output/assets/scenes/，是场次级美术基准。
// 全部索引进 output/assets/index.json 供 Web 控制台素材库模块展示。
func cmdAssets(args []string) error {
	fs := flag.NewFlagSet("assets", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	chars := fs.String("chars", "", "只处理指定角色（名字，逗号分隔；默认全部）")
	scenes := fs.String("scenes", "", "只处理指定场景（id，逗号分隔，如 s1,s2；默认全部）")
	force := fs.Bool("force", false, "强制重新生成（角色参考帧重出会改变全片长相，慎用）")
	noChars := fs.Bool("no-chars", false, "跳过角色参考帧")
	noScenes := fs.Bool("no-scenes", false, "跳过场景素材")
	promptFile := fs.String("prompt-file", "", "人工提示词覆盖文件（JSON，schema 同素材规划；Web 编辑弹窗写入）")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)
	if *runDir == "" {
		return fmt.Errorf("缺少 --run 参数")
	}
	ai.SetCallScope(filepath.Base(*runDir)) // 调用记录关联到本项目（素材）
	release, err := fsx.LockRun(*runDir)
	if err != nil {
		return err
	}
	defer release()

	cfg := mustConfig(*configPath)
	if err := ffx.Preflight(); err != nil {
		return err
	}
	ac := newClient(cfg)
	if ac.Mock() {
		fmt.Println("⚠ 当前为离线演示模式：将生成占位素材，不是真实画面。")
	}
	// 按能力派生客户端：素材规划走文本端点、参考帧/场景图走出图端点，
	// 两者可指向不同供应商（如文本 DeepSeek + 画面智谱）
	textc, err := shotkit.CapClient(ac, cfg, ai.CapText, cfg.Providers.Text, cfg.Providers.Text.Model)
	if err != nil {
		return err
	}
	imgc, err := shotkit.CapClient(ac, cfg, ai.CapImage, cfg.Providers.Image, cfg.Image.Model)
	if err != nil {
		return err
	}
	style, err := resolveStyle(*runDir)
	if err != nil {
		return err
	}
	fmt.Printf("🎨 全片风格: %s（%s）\n", style.Name, style.Aspect)

	started := time.Now()

	// ---- 素材规划：AI 基于剧本与角色圣经决定"需要哪几张" ----
	// 一次便宜的文本调用；失败兜底回默认清单（每角色一帧、场景不归并）。
	var charSpecs []promspec.CharacterSpec
	haveChars := false
	if kit, err := loadShotspec(*runDir); err == nil {
		charSpecs, haveChars = kit.Characters, true
	} else if raw, rerr := os.ReadFile(filepath.Join(*runDir, "artifacts", "characters.json")); rerr == nil {
		if cs, cerr := shotkit.CharactersFromBible(raw); cerr == nil {
			charSpecs, haveChars = cs, true
			fmt.Println("  ⚠ shotspec 未产出：角色规格从角色圣经编译（完成分镜/生成规格后可重出更精细的参考帧）")
		}
	}
	var scriptScenes []shotkit.SceneInput
	if raw, rerr := os.ReadFile(filepath.Join(*runDir, "artifacts", "script.json")); rerr == nil {
		if ss, serr := shotkit.SceneInputsFromScript(raw); serr == nil {
			scriptScenes = ss
		}
	}
	// 前置校验放在规划打印之前：缺什么阶段就明确说缺什么，并给出补救路径
	if !*noChars && !haveChars {
		return fmt.Errorf("尚未执行 characters 阶段（角色圣经缺失，shotspec 也未产出）:角色参考帧需要角色外形与一致性锚点。\n"+
			"  补救：先断点续跑完成 characters——创作中心 → ▶ 断点续跑，或 studio resume --run %s --until characters，再生成素材",
			filepath.ToSlash(*runDir))
	}
	plan, err := shotkit.PlanAssets(textc, readRunBrief(*runDir), scriptScenes, charSpecs,
		func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) })
	if err != nil {
		return err
	}
	// 人工提示词覆盖（Web 编辑弹窗回传）：按 name+variant 合并进规划。
	// 严格模式：显式给了覆盖文件就必须生效——静默忽略等于"以为改过、其实没改"。
	if *promptFile != "" {
		data, rerr := os.ReadFile(*promptFile)
		if rerr != nil {
			return fmt.Errorf("提示词覆盖文件读取失败: %w", rerr)
		}
		var ov shotkit.AssetPlan
		if jerr := json.Unmarshal(data, &ov); jerr != nil {
			return fmt.Errorf("提示词覆盖文件解析失败: %w", jerr)
		}
		shotkit.MergePromptOverrides(plan, &ov)
		fmt.Println("  ✏ 已应用人工提示词覆盖（质检仍按角色圣经核对）")
	}
	shotkit.SaveAssetPlan(*runDir, plan) // 规划存档：编辑弹窗下次展示同一份帧清单
	if len(charSpecs) > 0 || len(scriptScenes) > 0 {
		fmt.Printf("📋 素材规划（按剧本需要生成，单角色上限 %d 帧）:\n", shotkit.MaxFramesPerChar)
		for _, pc := range plan.Characters {
			fmt.Printf("  · %s：%s\n", pc.Name, frameLabels(pc.Frames))
		}
		if n := shotkit.CountSceneGroups(scriptScenes); n > 0 && n < len(scriptScenes) {
			fmt.Printf("  · 场景归并：%d 个场次 → %d 张基准图（同地点同时段共用一张）\n", len(scriptScenes), n)
		}
	}

	// ---- 角色帧与场景图并行生成（互不依赖），各自内部再按 image.workers 并发 ----
	var (
		refResults          []shotkit.RefResult
		sceneAssets         []shotkit.SceneAsset
		charsErr, scenesErr error
	)
	var wg sync.WaitGroup

	if !*noChars {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selected := charSpecs
			if want := splitList(*chars); len(want) > 0 {
				selected = filterCharacters(charSpecs, want)
				if len(selected) == 0 {
					charsErr = fmt.Errorf("没有匹配的角色: %s（可选: %s）",
						*chars, strings.Join(charNames(charSpecs), "、"))
					return
				}
			}
			// 参考帧质检与 deliver 视觉复核同源，共享花费账本。
			// 严格模式：质检已启用但视觉端点不可用 → 失败（静默跳过 = 以为查过、其实没查）。
			var refVision *ai.Client
			if cfg.RefQCEnabled() && !imgc.Mock() {
				rv, rerr := shotkit.CapClient(imgc, cfg, ai.CapVision, cfg.Providers.Vision, cfg.Models.Vision)
				if rerr != nil {
					charsErr = rerr
					return
				}
				refVision = rv
				if perr := refVision.Ping(); perr != nil {
					charsErr = fmt.Errorf("参考帧质检已启用（ref_qc.mode=%s）但视觉端点探活失败：%w\n"+
						"  处理：修复 providers.vision 端点/密钥，或将 ref_qc.mode 设为 off 显式关闭质检后重跑", cfg.RefQC.Mode, perr)
					return
				}
			}
			sub := promspec.Kit{Style: style, Characters: selected}
			fmt.Printf("🧑‍🎨 角色参考帧: %d 个角色%s\n", len(selected),
				map[bool]string{true: "（强制重出——全片角色长相将改变）", false: ""}[*force])
			refResults, charsErr = shotkit.EnsureRefFrames(imgc, cfg, *runDir, &sub, plan, *force, refVision)
			if charsErr != nil {
				charsErr = fmt.Errorf("角色参考帧生成失败: %w", charsErr)
				return
			}
			for _, r := range refResults {
				label := r.Name
				if r.Variant != "" && r.Variant != "base" {
					label += "·" + r.Variant
				}
				switch {
				case r.Error != "":
					fmt.Printf("  ✗ %s: %s\n", label, r.Error)
				case r.QCStatus == "failed":
					fmt.Printf("  ✗ %s: 质检未通过: %s\n", label, strings.Join(r.QCIssues, "；"))
				default:
					fmt.Printf("  ✓ %s → %s（%s）\n", label, r.Path, r.QCSummary())
				}
			}
		}()
	}

	if !*noScenes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if len(scriptScenes) == 0 {
				scenesErr = fmt.Errorf("尚未执行 script 阶段（剧本缺失）:artifacts/script.json 不存在或没有场次。" +
					"\n  补救：先完成创作流水线的 script 阶段（创作中心 → ▶ 断点续跑），再生成场景素材")
				return
			}
			selected := scriptScenes
			if want := splitList(*scenes); len(want) > 0 {
				selected = filterScenes(scriptScenes, want)
				if len(selected) == 0 {
					scenesErr = fmt.Errorf("没有匹配的场景: %s", *scenes)
					return
				}
			}
			ix := shotkit.LoadAssetIndex(*runDir)
			fmt.Printf("🏞 场景素材: %d 个场次 → %d 张基准图%s\n", len(selected),
				shotkit.CountSceneGroups(selected),
				map[bool]string{true: "（强制重出）", false: ""}[*force])
			sceneAssets, scenesErr = shotkit.EnsureSceneAssets(imgc, cfg, *runDir, style, selected, ix.Scenes, plan, *force)
			if scenesErr != nil {
				scenesErr = fmt.Errorf("场景素材生成失败: %w", scenesErr)
				return
			}
			for _, s := range sceneAssets {
				label := s.SceneID + "（" + s.Location + "）"
				if s.Variant != "" {
					label += "·" + s.Variant
				}
				if s.Error != "" {
					fmt.Printf("  ✗ %s: %s\n", label, s.Error)
				} else {
					fmt.Printf("  ✓ %s → %s\n", label, s.File)
				}
			}
		}()
	}
	wg.Wait()
	if charsErr != nil {
		return charsErr
	}
	if scenesErr != nil {
		return scenesErr
	}

	// ---- 合并索引：替换本次处理过的条目，保留未处理的 ----
	ix := shotkit.LoadAssetIndex(*runDir)
	ix.MergeCharacters(refResults)
	ix.MergeScenes(sceneAssets)
	if err := ix.Save(*runDir); err != nil {
		return fmt.Errorf("写素材库索引失败: %w", err)
	}
	fmt.Printf("✅ 素材库已更新（%.1f 秒）: %s\n", time.Since(started).Seconds(),
		filepath.ToSlash(filepath.Join(*runDir, "output", "assets", "index.json")))
	fmt.Println("  角色参考帧将被渲染管线自动复用（图生图锁脸）；场景图为场次美术基准。")
	return nil
}

// resolveStyle 全片风格：shotplan → shotspec → 默认预设（实现下沉到 shotkit，
// 供 Web 提示词预览共用同一套解析，保证"所见即所发"）。
func resolveStyle(runDir string) (promspec.Style, error) {
	return shotkit.ResolveStyle(runDir)
}

// loadShotspec 载入生成规格（角色 token/种子都在这里）。
func loadShotspec(runDir string) (*promspec.Kit, error) {
	data, err := os.ReadFile(filepath.Join(runDir, "artifacts", "shotspec.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 shotspec.json 失败: %w", err)
	}
	var kit promspec.Kit
	if err := json.Unmarshal(data, &kit); err != nil {
		return nil, fmt.Errorf("shotspec 解析失败: %w", err)
	}
	return &kit, nil
}

func splitList(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func charNames(cs []promspec.CharacterSpec) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func filterCharacters(cs []promspec.CharacterSpec, want []string) []promspec.CharacterSpec {
	var out []promspec.CharacterSpec
	for _, c := range cs {
		for _, w := range want {
			if c.Name == w {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func filterScenes(ss []shotkit.SceneInput, want []string) []shotkit.SceneInput {
	var out []shotkit.SceneInput
	for _, s := range ss {
		for _, w := range want {
			if strings.EqualFold(s.ID, w) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// readRunBrief 从 state.json 读取创作意图（best effort，供素材规划提示词）。
func readRunBrief(runDir string) string {
	var st struct {
		Brief string `json:"brief"`
	}
	if data, err := os.ReadFile(filepath.Join(runDir, "state.json")); err == nil {
		if json.Unmarshal(data, &st) == nil {
			return st.Brief
		}
	}
	return ""
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
