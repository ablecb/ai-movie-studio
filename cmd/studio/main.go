// studio —— 专业级 AI 电影制作工作流引擎的命令行入口。
//
// 生产流程（每一步都有明确的交付物与门禁）：
//
//	studio run     --brief "…"                  # 1. 全流程创作与质检（产出生成规格）
//	studio render  --run runs/<id> --dry-run    # 2. 成本预估与计划自检（不花钱）
//	studio render  --run runs/<id>              # 3. 逐镜头生成 + 客观质检 + 换种重渲
//	studio deliver --run runs/<id>              # 4. 配音/字幕/混音/合成/成片校验
//	studio verify  --run runs/<id>              # 5. 复核交付报告
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/deliver"
	"aistudio/internal/engine"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
	"aistudio/internal/quality"
	"aistudio/internal/render"
	"aistudio/internal/selfheal"
	"aistudio/internal/shotkit"
)

// rootCtx 批次级上下文：Ctrl-C 取消后，所有在途/排队的付费调用立即终止。
// "用户按 Ctrl-C 不该继续烧钱"由此成为现实，而不是注释里的承诺。
var rootCtx context.Context = context.Background()

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rootCtx = ctx

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(os.Args[2:])
	case "resume":
		err = cmdResume(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "animatic":
		err = cmdAnimatic(os.Args[2:])
	case "deliver":
		err = cmdDeliver(os.Args[2:])
	case "polish":
		err = cmdPolish(os.Args[2:])
	case "takes":
		err = cmdTakes(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "presets":
		err = cmdPresets()
	case "lessons":
		err = cmdLessons()
	case "probe-ref":
		err = cmdProbeRef(os.Args[2:])
	case "assets":
		err = cmdAssets(os.Args[2:])
	case "backfill-calls":
		err = cmdBackfillCalls(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`studio —— 专业级 AI 电影制作工作流引擎

用法:
  studio run      --workflow workflows/movie.json [--brief "故事梗概"] [--until script]
  studio resume   --run runs/run-xxx
  studio status   --run runs/run-xxx
  studio doctor   --run runs/run-xxx            # 环境自检 + AI 诊断失败阶段

  studio render   --run runs/run-xxx [--dry-run] [--max-shots 2] [--workers 2]
                  [--quality speed|quality] [--shots sh003,sh007] [--force-ref]
                  [--images-only]                  # 只出首帧（配合 animatic 预览节奏）
  studio animatic --run runs/run-xxx              # 拼动态分镜：花钱出视频前先看节奏
  studio deliver  --run runs/run-xxx [--allow-incomplete] [--no-burn] [--skip-audio]
  studio polish   --run runs/run-xxx [--rounds 2]    # 视觉复核闭环：复核→定点重渲→重交付→再复核
  studio takes    --run runs/run-xxx --shots sh003   # 关键镜头多 take 对比（并排预览+评分）
  studio verify   --run runs/run-xxx [--render] [--qc]

  studio presets                                # 查看内置视觉风格
  studio lessons                                # 查看经验记忆库
  studio probe-ref [--out runs/probe-ref]       # 参考图生效性探针（花几分钱实测出图接口
                                                #   是否真的吃参考图，决定一致性走哪条路）
  studio serve [--addr 127.0.0.1:8787]          # Web 控制台：按业务模块（创作/渲染/声音/
                                                #   交付/质检/运行/系统）在浏览器里操作流水线
  studio assets  --run runs/run-xxx             # 素材库：基于剧本生成一致性素材（角色参考帧 +
                                                #   场景 establishing 图）；--chars/--scenes 定点重出
  studio backfill-calls [--dry-run]             # 历史调用记录补档：反查归档"当时随请求发出的图"
                                                #   （首帧/尾帧/参考帧），旧记录详情页即可回看；先停 serve

环境变量:
  AI_API_KEY     大模型 API 密钥（未设置时进入 mock 离线演示模式）
  STUDIO_FFMPEG  ffmpeg 可执行文件路径（不在 PATH 中时指定）
  STUDIO_FFPROBE ffprobe 可执行文件路径

典型生产流程:
  studio run --brief "…" && studio render --run runs/<id> && studio deliver --run runs/<id>`)
}

// ---- 参数与装配 ----

type runFlags struct {
	workflow, brief, config, runDir string
}

func parseRunFlags(fs *flag.FlagSet) *runFlags {
	rf := &runFlags{}
	fs.StringVar(&rf.workflow, "workflow", "workflows/movie.json", "工作流定义文件")
	fs.StringVar(&rf.brief, "brief", "", "创作意图 / 故事梗概（覆盖工作流内置 brief）")
	fs.StringVar(&rf.config, "config", "config.json", "AI 与生产参数配置")
	fs.StringVar(&rf.runDir, "run", "", "run 目录")
	return rf
}

// mustConfig 载入并自检配置。生产环境的参数问题必须在开工前暴露。
func mustConfig(path string) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
	// ffmpeg 位置：允许用环境变量覆盖（很多环境 ffmpeg 不在 PATH）
	ffx.SetBinary("ffmpeg", os.Getenv("STUDIO_FFMPEG"))
	ffx.SetBinary("ffprobe", os.Getenv("STUDIO_FFPROBE"))
	// 曲库目录锚定：cwd 找不到时回落到可执行文件目录（否则换目录运行 = 静默无配乐）
	cfg.Music.LibraryDir = resolveDataPath(cfg.Music.LibraryDir)
	return cfg
}

// newClient 构造客户端并注入计费/预算。
// 必须在创建任何派生客户端（For）之前挂上批次上下文——取消要覆盖全部付费出口。
func newClient(cfg *config.Config) *ai.Client {
	ac := ai.New(ai.Config{
		BaseURL: cfg.BaseURL, APIKeyEnv: cfg.APIKeyEnv, APIKey: cfg.APIKey(), Model: cfg.Model,
		Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens, Mock: cfg.Mock,
	})
	ai.EnableByEnv() // AI 调用记录（logs/ai_calls/，AI_CALL_LOG=off 可停用）
	ac.SetParentContext(rootCtx)
	ac.SetPrices(priceMap(cfg))
	ac.SetBudget(cfg.Budget.MaxCostCNY)
	ac.SetWarnRatio(cfg.Budget.WarnRatio)
	return ac
}

// newTextClient 构造面向文本能力的客户端（技能阶段的大脑）。
//
// providers.text 指向与顶层不同的端点时（如 config.deepseek.json：文本走 DeepSeek、
// 画面仍走智谱），文本调用必须派生到该端点——否则模型名与端点错配，
// 请求会带着 deepseek-flash 打到智谱并报"模型不存在"。
func newTextClient(cfg *config.Config) (*ai.Client, error) {
	base := newClient(cfg)
	return shotkit.CapClient(base, cfg, ai.CapText, cfg.Providers.Text, cfg.Providers.Text.Model)
}

// priceMap 把配置单价转成客户端可用的计价表。
func priceMap(cfg *config.Config) map[string]ai.Price {
	out := map[string]ai.Price{}
	conv := func(kind string) ai.Price {
		p := cfg.PriceFor(kind)
		return ai.Price{
			PromptPer1K:     p.PromptPer1K,
			CompletionPer1K: p.CompletionPer1K,
			ImagePerShot:    p.ImagePerShot,
			VideoPerSec:     p.VideoPerSec,
			TTSPer1KChars:   p.TTSPer1KChars,
		}
	}
	for _, k := range []string{ai.CostText, ai.CostVision, ai.CostTranslate, ai.CostImage, ai.CostVideo, ai.CostTTS} {
		out[k] = conv(k)
	}
	return out
}

func newMemory() *selfheal.Memory {
	path := resolveDataPath("memory/lessons.json")
	mem, err := selfheal.LoadMemory(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 经验记忆库不可读，使用空库: %v\n", err)
		mem = &selfheal.Memory{Path: path}
	}
	return mem
}

// resolveDataPath 锚定工作区数据路径（经验记忆库/曲库等）。
// 优先当前工作目录；cwd 下不存在时回落到可执行文件所在目录——
// 否则从别的目录调用 studio 会**静默**丢失经验库与曲库（不报错，只是找不到）。
func resolveDataPath(rel string) string {
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return rel
}

// ---- 创作阶段 ----

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	rf := parseRunFlags(fs)
	until := fs.String("until", "", "跑到指定阶段后停止（人工确认后再继续，最省钱的用法）")
	from := fs.String("from", "", "从指定阶段开始（跳过之前的阶段）")
	fs.Parse(args)

	cfg := mustConfig(rf.config)
	wf, err := engine.LoadWorkflow(rf.workflow)
	if err != nil {
		return err
	}
	if rf.brief != "" {
		wf.Brief = rf.brief
	}
	if strings.TrimSpace(wf.Brief) == "" {
		return fmt.Errorf("缺少创作意图：用 --brief 或在工作流文件中设置 brief")
	}
	if *until != "" {
		if err := engine.TruncateWorkflow(wf, *until, true); err != nil {
			return err
		}
		fmt.Printf("▶ 将在阶段 %s 后停止（便于人工确认剧本/分镜后再继续）\n", *until)
	}
	if *from != "" {
		if err := engine.TruncateWorkflow(wf, *from, false); err != nil {
			return err
		}
	}

	ac, err := newTextClient(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("⚙ 生效配置: %s\n", cfg.String())
	e, err := engine.NewRun(wf, ac, cfg, newMemory(), "runs")
	if err != nil {
		return err
	}
	ai.SetCallScope(e.State.ID) // 调用记录关联到本项目
	release, err := fsx.LockRun(e.Dir)
	if err != nil {
		return err
	}
	defer release()
	return e.Run()
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	rf := parseRunFlags(fs)
	until := fs.String("until", "", "跑到指定阶段后停止（配合断点续跑做分段确认）")
	from := fs.String("from", "", "从此阶段开始重跑（该阶段与已完成的下游重置为待执行）；配合 --until 同阶段可只重跑单个阶段")
	fs.Parse(args)
	if rf.runDir == "" {
		return fmt.Errorf("resume 需要 --run runs/run-xxx")
	}
	cfg := mustConfig(rf.config)
	wf, err := engine.LoadWorkflow(rf.workflow)
	if err != nil {
		return err
	}
	ai.SetCallScope(filepath.Base(rf.runDir)) // 调用记录关联到本项目
	tc, err := newTextClient(cfg)
	if err != nil {
		return err
	}
	e, err := engine.Resume(rf.runDir, wf, tc, cfg, newMemory())
	if err != nil {
		return err
	}
	// 重跑某阶段：必须在 Resume 之后（Resume 会先把上次 skipped 翻回 pending，
	// 顺序颠倒会把"需重做"的标记误翻成待执行，语义就散了）。
	// until 一并传入：窗口内（≤until）的下游本次直接重跑，窗口外的标记为需重做。
	if *from != "" {
		if err := e.RerunFrom(*from, *until); err != nil {
			return err
		}
		if *until == "" {
			fmt.Printf("▶ 将重跑阶段 %s 及其后续\n", *from)
		} else if *until == *from {
			fmt.Printf("▶ 将只重跑阶段 %s（后续阶段已标记为需重做）\n", *from)
		} else {
			fmt.Printf("▶ 将从阶段 %s 重跑到 %s（窗口外的下游已标记为需重做）\n", *from, *until)
		}
	}
	if *until != "" {
		if err := engine.TruncateWorkflow(wf, *until, true); err != nil {
			return err
		}
		fmt.Printf("▶ 将在阶段 %s 后停止（已完成阶段自动跳过）\n", *until)
	}
	release, err := fsx.LockRun(e.Dir)
	if err != nil {
		return err
	}
	defer release()
	return e.Run()
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	rf := parseRunFlags(fs)
	fs.Parse(args)
	if rf.runDir == "" {
		return fmt.Errorf("status 需要 --run runs/run-xxx")
	}
	data, err := os.ReadFile(filepath.Join(rf.runDir, "state.json"))
	if err != nil {
		return err
	}
	var st engine.RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	fmt.Printf("Run: %s  工作流: %s  更新: %s\n", st.ID, st.Workflow, st.UpdatedAt.Format(time.RFC3339))
	fmt.Printf("Brief: %s\n\n", st.Brief)

	ids := make([]string, 0, len(st.Stages))
	if len(st.StageOrder) > 0 {
		for _, id := range st.StageOrder {
			if _, ok := st.Stages[id]; ok {
				ids = append(ids, id)
			}
		}
	} else {
		for id := range st.Stages {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	for _, id := range ids {
		s := st.Stages[id]
		mark := map[string]string{"done": "✓", "failed": "✗", "running": "…", "skipped": "⤼", "pending": "·"}[s.Status]
		line := fmt.Sprintf(" %s %-12s %-8s 尝试%d 返修%d", mark, id, s.Status, s.Attempts, s.Revisions)
		if s.Error != "" {
			line += "  错误: " + truncateRun(s.Error, 60)
		}
		fmt.Println(line)
	}
	if len(st.Usage) > 0 {
		var tp, tc int64
		fmt.Println("\nToken 用量（成本核算）:")
		keys := make([]string, 0, len(st.Usage))
		for k := range st.Usage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, id := range keys {
			u := st.Usage[id]
			fmt.Printf("  %-12s 调用%d 次  prompt %d  completion %d\n", id, u.Calls, u.PromptTokens, u.CompletionTokens)
			tp += u.PromptTokens
			tc += u.CompletionTokens
		}
		fmt.Printf("  合计 prompt %d + completion %d = %d\n", tp, tc, tp+tc)
	}
	printRenderStatus(rf.runDir)
	return nil
}

// printRenderStatus 展示渲染完成度——交付是否可能，看这一行就知道。
func printRenderStatus(runDir string) {
	var man struct {
		Complete   bool     `json:"complete"`
		Missing    []string `json:"missing_shots"`
		ActualDur  float64  `json:"actual_total_duration_sec"`
		PlanDur    float64  `json:"plan_total_duration_sec"`
		Rerendered []string `json:"rerendered_shots"`
		Cost       struct {
			ActualCNY float64 `json:"actual_cny"`
		} `json:"cost"`
	}
	if err := readJSON(filepath.Join(runDir, "output", "render_manifest.json"), &man); err != nil {
		return
	}
	fmt.Printf("\n渲染: %s  实测总长 %.2fs（计划 %.2fs）",
		map[bool]string{true: "完整 ✓", false: "不完整 ⛔"}[man.Complete], man.ActualDur, man.PlanDur)
	if man.Cost.ActualCNY > 0 {
		fmt.Printf("  花费 %.2f 元", man.Cost.ActualCNY)
	}
	fmt.Println()
	if len(man.Missing) > 0 {
		fmt.Printf("  缺失镜头: %s\n", strings.Join(man.Missing, ","))
		fmt.Printf("  补齐: studio render --run %s\n", runDir)
	}
	if len(man.Rerendered) > 0 {
		fmt.Printf("  换种重渲过: %s\n", strings.Join(man.Rerendered, ","))
	}
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	rf := parseRunFlags(fs)
	fs.Parse(args)
	cfg := mustConfig(rf.config)

	// 环境自检：付费调用之前先确认工具链可用
	fmt.Println("环境自检:")
	if err := ffx.Preflight(); err != nil {
		fmt.Printf("  ✗ %v\n", err)
	} else {
		fmt.Printf("  ✓ ffmpeg: %s\n", ffx.BinPath("ffmpeg"))
		fmt.Printf("  ✓ ffprobe: %s\n", ffx.BinPath("ffprobe"))
	}
	caps := ffx.Detect()
	fmt.Printf("  · %s\n", caps.Version)
	fmt.Printf("  · 能力: loudnorm=%v ebur128=%v 字幕=%v 侧链闪避=%v ffprobe=%v\n",
		caps.HasLoudnorm, caps.HasEbur128, caps.HasSubtitles, caps.HasSidechain, caps.FFprobePath != "")
	for _, w := range caps.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	ac := newClient(cfg)
	mode := "真实模型（会产生真实费用）"
	if ac.Mock() {
		mode = "离线演示（未设置 API Key，付费能力不可用）"
	}
	fmt.Printf("  · 模型模式: %s\n", mode)
	fmt.Printf("  · 配置: %s\n", cfg.String())
	fmt.Printf("  · 预算: %s\n", map[bool]string{true: "不限制", false: fmt.Sprintf("%.2f 元", cfg.Budget.MaxCostCNY)}[cfg.Budget.MaxCostCNY <= 0])
	if !cfg.Image.Watermark || !cfg.Video.Watermark {
		fmt.Println("  · 去水印: 已请求关闭（请求体显式 watermark_enabled=false）")
		fmt.Println("    ⚠ 需先在智谱控制台签署免责声明（个人中心-安全管理-去水印管理），否则仍会带水印")
		fmt.Println("    ⚠ 隐式数字水印/文件元数据标记不受该参数控制；交付前请抽帧核对画面角标")
	}

	if rf.runDir == "" {
		fmt.Println("\n提示: 加 --run runs/run-xxx 可同时诊断失败阶段")
		return nil
	}
	data, err := os.ReadFile(filepath.Join(rf.runDir, "state.json"))
	if err != nil {
		return err
	}
	var st engine.RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	var failed []string
	for id, s := range st.Stages {
		if s.Status == "failed" {
			failed = append(failed, id)
		}
	}
	if len(failed) == 0 {
		fmt.Println("\n✓ 没有失败阶段")
		printRenderStatus(rf.runDir)
		return nil
	}
	sort.Strings(failed)
	for _, id := range failed {
		s := st.Stages[id]
		plan := selfheal.Diagnose(ac, selfheal.DiagnoseInput{
			SkillName: id, Attempts: s.Attempts, Error: s.Error,
			Lessons: newMemory().RecallFor(id, s.Error, 5),
		})
		fmt.Printf("\n阶段 [%s] 失败诊断:\n  根因: %s\n  建议: %s\n  修复: %s\n",
			id, plan.RootCause, plan.Action, plan.Hint)
	}
	fmt.Println("\n处理后可执行: studio resume --run " + rf.runDir)
	return nil
}

// ---- 渲染阶段 ----

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	maxShots := fs.Int("max-shots", 0, "只渲前 N 个镜头（按优先级：关键/高风险优先，用于试点验证画质）")
	shots := fs.String("shots", "", "定点重渲指定镜头，逗号分隔（如 sh003,sh007）")
	workers := fs.Int("workers", 0, "并发数（默认取配置）")
	qualityMode := fs.String("quality", "", "画质 speed|quality（默认取配置）")
	timeoutMin := fs.Int("timeout-minutes", 0, "单个镜头超时（分钟）")
	dryRun := fs.Bool("dry-run", false, "只做成本预估与计划自检，不产生任何付费调用")
	imagesOnly := fs.Bool("images-only", false, "只生成角色参考帧与首帧图（不生成视频）：供 animatic 拼动态分镜预览，后续完整渲染会复用这些首帧")
	allowIncomplete := fs.Bool("allow-incomplete", false, "允许缺镜头出片（默认拒绝）")
	forceRef := fs.Bool("force-ref", false, "强制重新生成角色参考帧（会改变全片角色长相，慎用）")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)

	if *runDir == "" {
		return fmt.Errorf("缺少 --run 参数")
	}
	ai.SetCallScope(filepath.Base(*runDir)) // 调用记录关联到本项目（渲染）
	cfg := mustConfig(*configPath)
	if err := chooseQuality(cfg, *qualityMode); err != nil {
		return err
	}
	if *timeoutMin > 0 {
		cfg.Video.TimeoutMinutes = *timeoutMin
	}
	if *workers > 0 {
		cfg.Video.Workers = *workers
	}

	ac := newClient(cfg)
	if ac.Mock() && !*dryRun {
		// 不阻断：离线模式产出的是占位素材，能完整验证流水线但不产生真实画面。
		// 明确告警，避免"以为在生成真画面"。
		fmt.Println("⚠ 当前为离线演示模式（未设置 AI_API_KEY）：将生成占位素材用于链路验证，不是真实画面。")
		fmt.Println("  要产出真实影片：export AI_API_KEY=<你的密钥> 后重跑。")
	}
	if err := ffx.Preflight(); err != nil {
		return err
	}
	var shotList []string
	for _, s := range strings.Split(*shots, ",") {
		if t := strings.TrimSpace(s); t != "" {
			shotList = append(shotList, t)
		}
	}
	man, err := render.Run(ac, cfg, render.Options{
		RunDir: *runDir, MaxShots: *maxShots, ShotIDs: shotList,
		Workers: cfg.Video.Workers, Quality: cfg.Video.DefaultQuality,
		Timeout: time.Duration(cfg.Video.TimeoutMinutes) * time.Minute,
		DryRun:  *dryRun, AllowIncomplete: *allowIncomplete, ForceRef: *forceRef,
		ImagesOnly: *imagesOnly,
	})
	if err != nil {
		return err
	}
	if !man.Complete && !*dryRun && !*imagesOnly {
		// 渲染不完整：明确返回失败，避免脚本串到后面的交付步骤
		return fmt.Errorf("渲染未完成：缺 %d 个镜头（%s）。重跑同一命令可断点续跑补齐",
			len(man.Missing), strings.Join(man.Missing, ","))
	}
	// --images-only 的自然下一步：直接拼动态分镜，一条命令看到节奏
	if *imagesOnly {
		if _, err := render.Animatic(cfg, *runDir); err != nil {
			fmt.Printf("  ⚠ 动态分镜拼装失败（首帧已就绪，可稍后重跑 animatic）: %v\n", err)
		}
	}
	return nil
}

func chooseQuality(cfg *config.Config, q string) error {
	if q == "" {
		return nil
	}
	switch strings.ToLower(q) {
	case "speed", "economy":
		cfg.Video.DefaultQuality = "speed"
	case "quality", "high":
		cfg.Video.DefaultQuality = "quality"
	default:
		return fmt.Errorf("未知画质档位 %q（可选 speed|quality）", q)
	}
	return nil
}

// cmdAnimatic 拼装动态分镜（gray-box animatic）：
// 用 --images-only 产出的首帧按分镜时长静帧拼接，配音产物存在时一并铺入。
// 电影工业"先 animatic 再拍摄"的 AI 版——节奏不成立就不该为视频付钱。
func cmdAnimatic(args []string) error {
	fs := flag.NewFlagSet("animatic", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)
	if *runDir == "" {
		return fmt.Errorf("缺少 --run 参数")
	}
	cfg := mustConfig(*configPath)
	if err := ffx.Preflight(); err != nil {
		return err
	}
	_, err := render.Animatic(cfg, *runDir)
	return err
}

// ---- 交付阶段 ----

func cmdDeliver(args []string) error {
	fs := flag.NewFlagSet("deliver", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	allowIncomplete := fs.Bool("allow-incomplete", false, "允许缺镜头出片（内部审看用，报告会标注）")
	noBurn := fs.Bool("no-burn", false, "不烧录字幕（只出无字幕母版 + 外挂 SRT）")
	forceDub := fs.Bool("force-dub", false, "强制重新合成全部台词音频（无视 TTS 缓存，重新付费）")
	skipAudio := fs.Bool("skip-audio", false, "跳过配音（只验证画面）")
	visual := fs.Bool("visual", false, "运行视觉复核（让模型看画面；需配置 vision 端点与密钥）")
	noVisual := fs.Bool("no-visual", false, "跳过视觉复核（即使配置里已启用）")
	keepMaster := fs.Bool("keep-master", true, "保留无字幕母版")
	workers := fs.Int("workers", 0, "配音并发数")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)

	if *runDir == "" {
		return fmt.Errorf("缺少 --run 参数")
	}
	release, err := fsx.LockRun(*runDir)
	if err != nil {
		return err
	}
	defer release()
	cfg := mustConfig(*configPath)
	var client *ai.Client
	if !*skipAudio {
		if err := ffx.Preflight(); err != nil {
			return err
		}
		client = newClient(cfg)
	}

	// 视觉复核：按配置或命令行决定是否执行，并**提前探活**——
	// 选错模型（例如把不支持视觉的模型配到 vision）会在真正复核时才报错，
	// 那时渲染已经花完钱了。
	doVisual := (cfg.VisionReview.Enabled || *visual) && !*noVisual
	var visionClient *ai.Client
	if doVisual {
		visionClient, err = newVisionClient(cfg)
		if err != nil {
			return err
		}
		if cfg.Mock {
			// 离线演示（mock）没有真实端点可查：显式跳过复核（显式模式，不是降级）
			fmt.Println("⚠ 离线演示模式：跳过视觉复核")
			doVisual = false
		} else if visionClient.Mock() {
			// 严格模式：复核已启用就必须能复核——密钥缺失直接失败，
			// 不允许"以为查过、其实没查"。确需跳过请显式 --no-visual。
			return fmt.Errorf("视觉复核已启用但缺少密钥（环境变量 %s）：请配置密钥，或用 --no-visual 显式跳过复核",
				cfg.Providers.Vision.APIKeyEnv)
		} else if err := visionClient.Ping(); err != nil {
			return fmt.Errorf("视觉复核已启用但能力探活失败：%w\n  处理：修复 providers.vision 端点/模型配置，或用 --no-visual 显式跳过复核", err)
		} else {
			fmt.Printf("👁 视觉复核已启用: %s\n", visionClient.EndpointInfo())
		}
	}
	burn := !*noBurn
	_, err = deliver.Run(cfg, deliver.Options{
		RunDir: *runDir, AllowIncomplete: *allowIncomplete, ForceDub: *forceDub,
		BurnSubtitle: &burn, KeepMaster: *keepMaster,
		SkipAudio: *skipAudio, Workers: *workers, Client: client,
		VisionClient: visionClient, VisualCheck: doVisual,
	})
	return err
}

// newVisionClient 构造面向视觉能力的客户端。
//
// 视觉复核常常与生成端不是同一家服务（例如画面用智谱、复核用 DeepSeek
// deepseek-flash）。ai.Client.For 会保留同一个花费账本，因此复核花费同样计入预算熔断。
// 严格模式：providers.vision 是独立端点但缺密钥 → 报错（不允许静默继承顶层密钥或退回离线）。
func newVisionClient(cfg *config.Config) (*ai.Client, error) {
	base := newClient(cfg)
	return shotkit.CapClient(base, cfg, ai.CapVision, cfg.Providers.Vision, cfg.Models.Vision)
}

// ---- 参考图生效性探针 ----

// cmdProbeRef 用真实 API 回答一个决定一致性问题走向的问题：
// 出图接口到底吃不吃参考图？
//
// 智谱 /images/generations 的官方文档里没有 image/ref_strength 参数——
// 项目一直在发这两个字段，如果服务端只是静默忽略未知字段，那么
// "参考图=true"就是假象，角色一致性从未真正生效过。本命令做三步实测：
//  1. 纯文生图：生成一张有辨识度的参考图（红色自行车+蓝门）
//  2. 带参考图再生成：要求"同一场景，前轮被拆下靠在门边"——不听参考图
//     就画不出同一辆车/同一扇门
//  3. 视觉模型对比两图：参考图的主体/构图/色调是否延续到第二张
//
// 结论决定改造路线：honored=true → 现有单参考帧路径有效，重点补多角色；
// false → 参考图被忽略，一致性必须走多参考图模型（providers.image_edit）。
func cmdProbeRef(args []string) error {
	fs := flag.NewFlagSet("probe-ref", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	outDir := fs.String("out", "runs/probe-ref", "探针产物目录")
	fs.Parse(args)

	cfg := mustConfig(*configPath)
	ac := newClient(cfg)
	if ac.Mock() {
		return fmt.Errorf("参考图探针需要真实 API（当前为离线模式）。请 export AI_API_KEY=<密钥> 后重试")
	}
	// 出图能力客户端：端点取 providers.image（探针测的就是这条路的参考图行为）
	imgc, err := shotkit.CapClient(ac, cfg, ai.CapImage, cfg.Providers.Image, cfg.Image.Model)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	fmt.Println("▶ 参考图生效性探针（3 次调用：出图×2 + 视觉复核×1）")
	refPrompt := "cinematic photo: a vintage red bicycle with a wicker basket leaning against a blue wooden door, warm afternoon light, shallow depth of field"
	fmt.Println("  1/3 生成参考图（红色自行车+蓝色木门）…")
	refURL, refModel, err := imgc.ImageGenEx("probe.ref_image", refPrompt, ai.ImageOptions{
		Model: cfg.Image.Model, Size: cfg.Image.RefSize, Watermark: cfg.Image.Watermark,
	})
	if err != nil {
		return fmt.Errorf("参考图生成失败: %w", err)
	}
	refPath := filepath.Join(*outDir, "ref.png")
	if err := downloadAsset(refURL, refPath); err != nil {
		return err
	}
	fmt.Printf("     模型=%s → %s\n", refModel, refPath)

	fmt.Println("  2/3 带参考图再生成（同一场景：前轮拆下靠在门边）…")
	ref, err := ai.FileToDataURL(refPath)
	if err != nil {
		return err
	}
	probePrompt := "the exact same scene, same camera angle, same red bicycle and blue wooden door: the bicycle's front wheel has been removed and leans against the door"
	probeURL, probeModel, err := imgc.ImageGenEx("probe.image", probePrompt, ai.ImageOptions{
		Model: cfg.Image.Model, Size: cfg.Image.RefSize, Watermark: cfg.Image.Watermark,
		RefImage: ref, RefStrength: 0.6,
	})
	if err != nil {
		return fmt.Errorf("带参考图生成失败: %w", err)
	}
	probePath := filepath.Join(*outDir, "probe.png")
	if err := downloadAsset(probeURL, probePath); err != nil {
		return err
	}
	fmt.Printf("     模型=%s → %s\n", probeModel, probePath)

	vc, err := newVisionClient(cfg)
	if err != nil {
		return err
	}
	if vc.Mock() {
		fmt.Println("  3/3 视觉复核密钥未配置：请人工对比两图后判断")
		fmt.Printf("     参考: %s\n     结果: %s\n", refPath, probePath)
		return nil
	}
	fmt.Println("  3/3 视觉模型对比判定…")
	var verdict struct {
		RefHonored bool    `json:"ref_honored"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
	}
	err = vc.VisionJSONEx(
		"你是图像一致性评审员。只输出 JSON。",
		"第1张图是参考图，第2张图是带着参考图重新生成的结果（要求：同一场景，自行车前轮拆下靠在门边）。"+
			"判断第2张图是否真实延续了参考图的主体（同型号红色自行车、同一扇蓝色木门）、构图与色调。"+
			`输出 JSON：{"ref_honored": true/false, "confidence": 0到1, "reason": "一句话依据"}`,
		[]ai.ImageRef{{Path: refPath}, {Path: probePath}}, "auto", &verdict)
	if err != nil {
		return fmt.Errorf("视觉判定失败: %w\n  可人工对比 %s 与 %s", err, refPath, probePath)
	}
	conf := "低"
	switch {
	case verdict.Confidence >= 0.75:
		conf = "高"
	case verdict.Confidence >= 0.45:
		conf = "中"
	}
	fmt.Printf("\n■ 探针结论：参考图%s生效（置信度 %s，%.2f）\n",
		map[bool]string{true: "✓", false: "✗ 未"}[verdict.RefHonored], conf, verdict.Confidence)
	fmt.Printf("  依据：%s\n", verdict.Reason)
	if verdict.RefHonored {
		fmt.Println("  下一步：单参考帧路径有效。重点补齐多角色同框（video.reference_model / providers.image_edit 已在本次升级中接入）。")
	} else {
		fmt.Println("  下一步：参考图被服务端忽略（仅静默透传）。角色一致性必须走多参考图模型：")
		fmt.Println("    在 config.local.json 配置 providers.image_edit（火山方舟 Seedream 4.0 或阿里百炼 Qwen-Image-Edit），")
		fmt.Println("    本次升级的渲染管线会自动用它产出首帧；多角色镜头同时走 video.reference_model（vidu2-reference）。")
	}
	return nil
}

// downloadAsset 下载 URL 到本地（探针用），复用 shotkit 的原子下载。
func downloadAsset(url, path string) error {
	return shotkit.Download(url, path)
}

// ---- 复核 ----

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	renderQC := fs.Bool("render", false, "重新对已生成镜头做客观质检（不看主观判断）")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)
	if *runDir == "" {
		return fmt.Errorf("缺少 --run 参数")
	}
	cfg := mustConfig(*configPath)

	// 1) 交付报告
	var rep deliver.Report
	if err := readJSON(filepath.Join(*runDir, "output", "delivery_report.json"), &rep); err != nil {
		fmt.Println("尚未交付（无 delivery_report.json）")
	} else {
		fmt.Printf("交付报告（%s）\n", rep.GeneratedAt.Format("2006-01-02 15:04:05"))
		fmt.Printf("  完整性: %s（%d/%d 镜头）\n",
			map[bool]string{true: "完整", false: "不完整"}[rep.Complete], rep.ShotsDone, rep.ShotsTotal)
		fmt.Printf("  成片: %.2fs %dx%d %s %.0f kbps\n", rep.Video.DurationSec,
			rep.Video.Width, rep.Video.Height, rep.Video.VideoCodec, rep.Video.BitrateKbps)
		if rep.Audio.HasAudio {
			fmt.Printf("  声轨: %.1f LUFS / 峰值 %.1f dBFS / %d 句台词 / %d 配乐 / %d 音效\n",
				rep.Audio.LoudnessLUFS, rep.Audio.TruePeakDB,
				rep.Audio.VoiceLines, rep.Audio.MusicTracks, rep.Audio.SFXTracks)
		} else {
			fmt.Println("  声轨: 无")
		}
		fmt.Printf("  字幕: %d 条 / 最长 %.1f 字/秒 / %s\n", rep.Subtitle.CueCount,
			rep.Subtitle.MaxCPS, map[bool]string{true: "已烧录", false: "外挂"}[rep.Subtitle.Burned])
		fmt.Println("  校验项:")
		for _, c := range rep.Checks {
			mark := "✓"
			if !c.OK {
				if c.Blocking {
					mark = "⛔"
				} else {
					mark = "⚠"
				}
			}
			fmt.Printf("    %s %-22s %s\n", mark, c.Name, c.Detail)
		}
		for _, w := range rep.Warnings {
			fmt.Printf("  ⚠ %s\n", w)
		}
		for _, b := range rep.Blockers {
			fmt.Printf("  ⛔ %s\n", b)
		}
	}

	// 2) 镜头客观质检复跑（可选）
	if *renderQC {
		fmt.Println("\n镜头客观质检:")
		if err := qcShots(cfg, *runDir); err != nil {
			return err
		}
	}
	return nil
}

// qcShots 对已生成镜头复跑客观质检：任何"看起来不对"的镜头都应在这里现形。
func qcShots(cfg *config.Config, runDir string) error {
	var man render.Manifest
	if err := readJSON(filepath.Join(runDir, "output", "render_manifest.json"), &man); err != nil {
		return fmt.Errorf("读取渲染清单失败: %w", err)
	}
	gate := quality.NewGate(cfg.Quality)
	bad := 0
	for _, s := range man.Shots {
		if s.Status != "done" || s.Video == "" {
			continue
		}
		vv := gate.CheckVideoScene(filepath.Join(runDir, filepath.FromSlash(s.Video)), s.PlanDur,
			quality.SceneCtx{Night: s.Night})
		mark := "✓"
		if !vv.OK {
			mark = "⚠"
			if vv.Severity == quality.SeverityBlock {
				mark = "⛔"
				bad++
			}
		}
		fmt.Printf("  %s %s 实测 %.2fs 清晰度 %.1f 黑帧比 %.0f%% 亮度漂移 %.1f\n",
			mark, s.ID, vv.DurationSec, vv.MeanSharpness, vv.BlackRatio*100, vv.LumaDrift)
		for _, d := range vv.Defects {
			fmt.Printf("      - [%s] %s\n", d.Code, d.Message)
		}
	}
	if bad > 0 {
		return fmt.Errorf("有 %d 个镜头存在阻断级质量问题，建议定点重渲：studio render --run %s --shots <id>", bad, runDir)
	}
	return nil
}

func cmdPresets() error {
	fmt.Println("内置视觉风格（由 code 固化，保证整片统一；brief/剧本风格关键词会自动命中）:")
	for _, name := range promspec.AllPresets() {
		p, ok := promspec.PresetByName(name)
		if !ok {
			continue
		}
		fmt.Printf("  %-12s %s\n", name, p.Style.Name)
		fmt.Printf("               画幅 %s · 机身 %s · %s\n",
			p.Style.Aspect, p.Style.Camera.Name, p.Style.Camera.FilmStock)
	}
	return nil
}

func cmdLessons() error {
	mem, err := selfheal.LoadMemory(resolveDataPath("memory/lessons.json"))
	if err != nil {
		return err
	}
	if len(mem.Lessons) == 0 {
		fmt.Println("经验记忆库为空 —— 引擎还没有踩过坑")
		return nil
	}
	fmt.Printf("经验记忆库（%d 条）:\n", len(mem.Lessons))
	for _, l := range mem.Lessons {
		fmt.Printf("  %s [%s] %s\n    %s\n", l.ID, l.Skill, l.Time.Format("01-02 15:04"), l.Lesson)
	}
	return nil
}

// ---- 工具 ----

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func truncateRun(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
