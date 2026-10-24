// Package render 实现"逐镜头生成流水线"，是消除 AI 痕迹的主战场。
//
// 生产级流程（与原实现的区别逐项对应 AI 痕迹的来源）：
//
//  1. 角色参考帧先行   —— 锁死"同一张脸"，这是跨镜头一致性的唯一可靠手段
//  2. 参考图 + 固定种子 —— 每个出镜镜头都以参考图做图生图，种子由 (镜头,轮次) 确定性派生
//  3. 首帧本地规整      —— 尺寸不符会在提交阶段报错白花一次出图费，本地缩放免费
//  4. 生成后客观质检    —— 模糊/过曝/纯色/人脸占比畸大 用信号处理判定，不靠"感觉"
//  5. 不过即换种重渲    —— 缺陷镜头自动进入下一轮，最多 rerender_rounds 轮
//  6. 付费错误分类熔断  —— 余额不足立即停批，不做逐镜头重试烧钱
//  7. 全程产出 manifest —— 每个镜头的每一轮尝试、指标、缺陷都留痕，可复核可追责
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
	"aistudio/internal/quality"
	"aistudio/internal/shotkit"
)

// Options 渲染选项。
type Options struct {
	RunDir   string
	MaxShots int      // 试渲前 N 个镜头（0=全部）；按优先级排序后取，优先渲关键镜头
	ShotIDs  []string // 定点重渲指定镜头（成本最小化的返修手段）
	Workers  int
	Quality  string // quality | speed
	Timeout  time.Duration
	// DryRun 只做预估与自检，不产生任何付费调用
	DryRun bool
	// AllowIncomplete 允许缺镜头出片（默认 false：宁可不出片也不出假成片）
	AllowIncomplete bool
	// ForceRef 强制重新生成角色参考帧（会改变全片角色长相，属重大操作）
	ForceRef bool
	// ImagesOnly 只生成角色参考帧与各镜头首帧图，不生成视频（图片级质检照常）。
	// 这是 animatic 动态分镜的前置：用出图的小钱先拼一部"灰盒预览"，
	// 确认节奏与叙事后再为视频付大钱——电影工业"先 animatic 再拍摄"的 AI 版。
	// 产物状态记为 "imaged"；之后跑完整 render 会直接复用这些首帧（不重复出图）。
	ImagesOnly bool
}

// ShotRecord 单镜头生成记录（含全部尝试轮次）。
type ShotRecord struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"` // done | failed
	Video       string        `json:"video,omitempty"`
	Image       string        `json:"image,omitempty"`
	PlanDur     float64       `json:"plan_duration_sec"`
	ActualDur   float64       `json:"actual_duration_sec,omitempty"`
	Attempts    int           `json:"attempts"`
	Rounds      []RoundRecord `json:"rounds,omitempty"`
	Seed        int64         `json:"seed"`
	RefUsed     bool          `json:"ref_available"`
	UsedRef     bool          `json:"used_ref_image"`
	Model       string        `json:"model,omitempty"`
	ChainedFrom string        `json:"chained_from,omitempty"` // 链式衔接：承接末帧的上镜 ID
	LastFrame   string        `json:"last_frame,omitempty"`   // 末帧缓存（output/frames/<id>_last.png）
	// PlannedFrame 链式衔接镜头的"分镜构图锚点帧"（output/shots/<id>.planned.png）。
	// 作为首尾帧数组的尾帧传给 CogVideoX：首帧承接上镜末帧（连续性），
	// 尾帧锚定本镜构图（可控性），运动的终点不再漂移。
	PlannedFrame string                `json:"planned_frame,omitempty"`
	Error        string                `json:"error,omitempty"`
	Severity     quality.Severity      `json:"severity"`
	Defects      []quality.Defect      `json:"defects,omitempty"`
	VideoMetrics *quality.VideoVerdict `json:"video_metrics,omitempty"`
	ImageMetrics *quality.ImageVerdict `json:"image_metrics,omitempty"`
	// Night 夜戏镜头：verify --render 复跑质检时按同样口径放行夜戏阈值
	Night bool `json:"night,omitempty"`
}

// RoundRecord 一轮生成尝试的完整留痕。
type RoundRecord struct {
	Round        int                   `json:"round"`
	Seed         int64                 `json:"seed"`
	StartedAt    time.Time             `json:"started_at"`
	DurationSec  float64               `json:"duration_sec"`
	ImageVerdict *quality.ImageVerdict `json:"image_verdict,omitempty"`
	VideoVerdict *quality.VideoVerdict `json:"video_verdict,omitempty"`
	Error        string                `json:"error,omitempty"`
	Severity     quality.Severity      `json:"severity"`
}

// Manifest 渲染清单：交付与复核的唯一依据。
type Manifest struct {
	RunID          string              `json:"run_id"`
	Quality        string              `json:"quality"`
	StartedAt      time.Time           `json:"started_at"`
	FinishedAt     time.Time           `json:"finished_at"`
	DurationMin    float64             `json:"duration_minutes"`
	Shots          []ShotRecord        `json:"shots"`
	Refs           []shotkit.RefResult `json:"refs,omitempty"`
	Cost           CostReport          `json:"cost"`
	PlanDuration   float64             `json:"plan_total_duration_sec"`
	ActualDuration float64             `json:"actual_total_duration_sec"`
	Missing        []string            `json:"missing_shots,omitempty"`
	Blocked        []string            `json:"blocked_shots,omitempty"`
	Rerendered     []string            `json:"rerendered_shots,omitempty"`
	// Imaged 仅产出首帧的镜头数（--images-only 模式）。这类镜头不算缺失
	// （图片资产已就位且可复用），但也不算完成——它们还没有视频。
	Imaged   int      `json:"imaged_shots,omitempty"`
	Complete bool     `json:"complete"`
	Notes    []string `json:"notes,omitempty"`
}

// CostReport 成本报告（元）。
type CostReport struct {
	EstimatedCNY float64             `json:"estimated_cny"`
	ActualCNY    float64             `json:"actual_cny"`
	ByKind       map[string]ai.Spend `json:"by_kind,omitempty"`
}

// Count 统计某状态的镜头数。
func (m *Manifest) Count(status string) int {
	n := 0
	for _, s := range m.Shots {
		if s.Status == status {
			n++
		}
	}
	return n
}

// Run 执行渲染流水线。
func Run(client *ai.Client, cfg *config.Config, opt Options) (*Manifest, error) {
	if opt.Workers <= 0 {
		opt.Workers = cfg.Video.Workers
	}
	if opt.Timeout <= 0 {
		opt.Timeout = time.Duration(cfg.Video.TimeoutMinutes) * time.Minute
	}
	if opt.Quality == "" {
		opt.Quality = cfg.Video.DefaultQuality
	}

	// 1. 载入生成规格（shotspec 阶段的产物）
	kit, err := loadKit(opt.RunDir)
	if err != nil {
		return nil, err
	}

	// 2. 构造/过滤生成计划
	rerenderSet := map[string]bool{}
	for _, id := range opt.ShotIDs {
		if s := strings.TrimSpace(id); s != "" {
			rerenderSet[s] = true
		}
	}
	plan := shotkit.BuildPlan(kit, rerenderSet, 0)
	if len(plan.Tasks) == 0 {
		return nil, fmt.Errorf("没有可渲染的镜头（检查 --shots 是否命中实际镜头 ID）")
	}
	if problems := shotkit.ValidatePlan(plan, cfg); len(problems) > 0 {
		return nil, fmt.Errorf("生成计划自检未通过:\n  - %s", strings.Join(problems, "\n  - "))
	}

	// 3. 排序 + 试渲截断：先渲关键/高风险镜头，尽早暴露问题（而不是渲完才发现风格不对）
	sort.SliceStable(plan.Tasks, func(i, j int) bool {
		a, b := plan.Tasks[i], plan.Tasks[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if riskRank(a.Risk) != riskRank(b.Risk) {
			return riskRank(a.Risk) > riskRank(b.Risk)
		}
		return a.ShotID < b.ShotID
	})
	if opt.MaxShots > 0 && opt.MaxShots < len(plan.Tasks) {
		plan.Tasks = plan.Tasks[:opt.MaxShots]
	}

	// 4. 成本预估（付费调用之前必须给出）
	var totalSec float64
	for _, t := range plan.Tasks {
		totalSec += t.DurationSec
	}
	anchorShots := 0
	if cfg.Video.EndFrameAnchor && !opt.ImagesOnly {
		anchorShots = len(plan.Tasks) // 每镜尾帧锚点一张（vidu 路径跳过，此处取上限）
	}
	est := client.EstimateVideoCost(len(plan.Tasks), totalSec, 0, 0, anchorShots)
	fmt.Printf("▶ 渲染计划：%d 个镜头 / 计划时长 %.1fs / 画质 %s / 并发 %d\n",
		len(plan.Tasks), totalSec, opt.Quality, opt.Workers)
	fmt.Printf("  %s\n", est.String())
	if client.OverBudget() {
		return nil, fmt.Errorf("已达预算上限，拒绝开始渲染（调整 budget.max_cost_cny 后重试）")
	}
	if cfg.Budget.MaxCostCNY <= 0 {
		// 0 = 无上限是出厂默认，但生产里最贵的事故就是"跑着跑着烧穿余额"
		fmt.Println("  ⚠ 预算上限未设置（budget.max_cost_cny=0）：本次渲染花费上不封顶，强烈建议设置上限")
	}
	// 按能力派生图像/视频客户端：providers.image / providers.video 可指向
	// 与文本不同的供应商（默认：Seedream 出图 + Seedance 出视频，方舟端点）。
	// 放在 dry-run 之前：缺密钥属于"开工前必须暴露"的自检项，且此处零付费调用。
	imgc, err := shotkit.CapClient(client, cfg, ai.CapImage, cfg.Providers.Image, cfg.Image.Model)
	if err != nil {
		return nil, err
	}
	vidc, err := shotkit.CapClient(client, cfg, ai.CapVideo, cfg.Providers.Video, cfg.VideoModel(cfg.Video.DefaultQuality))
	if err != nil {
		return nil, err
	}

	if opt.DryRun {
		fmt.Println("  （--dry-run：已完成预估与自检，未产生任何付费调用）")
		// 打印首个镜头的完整提示词：花的钱买到的就是这段文字，
		// 开跑之前应当能亲眼确认它是否达到"电影感"标准。
		printSamplePrompt(plan)
		return dryManifest(opt, plan, est), nil
	}

	// 5. 角色参考帧：跨镜头一致性的地基，必须在生成任何镜头之前完成
	// 参考帧质检客户端与 deliver 的视觉复核同源（providers.vision + models.vision），
	// 共享同一份花费账本。
	// 严格模式：质检已启用（默认 strict）但视觉端点不可用 → 拒绝开工。
	// 静默跳过等于"以为查过、其实没查"——参考帧的错误会经图生图放大到每个出镜镜头。
	var refVision *ai.Client
	if cfg.RefQCEnabled() && !client.Mock() {
		rv, rerr := shotkit.CapClient(client, cfg, ai.CapVision, cfg.Providers.Vision, cfg.Models.Vision)
		if rerr != nil {
			return nil, rerr
		}
		refVision = rv
		if err := refVision.Ping(); err != nil {
			return nil, fmt.Errorf("参考帧质检已启用（ref_qc.mode=%s）但视觉端点探活失败：%w\n"+
				"  处理：修复 providers.vision 端点/密钥，或将 ref_qc.mode 设为 off 显式关闭质检后重跑", cfg.RefQC.Mode, err)
		}
	}
	fmt.Println("  …准备角色参考帧（跨镜头一致性的基准）")
	// 渲染期只保证主锚点帧就绪（plan=nil）：变体帧由素材阶段按规划生成，
	// 渲染补帧若带变体会引入"渲染中途换装/换角度"的非预期费用。
	refs, err := shotkit.EnsureRefFrames(client, cfg, opt.RunDir, kit, nil, opt.ForceRef, refVision)
	if err != nil {
		return nil, fmt.Errorf("角色参考帧生成失败: %w", err)
	}
	// 把参考帧路径回写到计划（BuildPlan 早于参考帧生成）。
	// 必须遍历 Characters（出镜角色，始终有值）而不是 RefChars——RefChars 只在
	// BuildPlan 时参考帧路径已知才会填充，而那时永远是空的，回写因此从未生效过，
	// 所有镜头都退化为纯提示词出图（跨镜头换脸的根因）。
	// RefImages 收齐全部出镜角色的参考帧：多角色同框镜头交给多参考图模型
	// （providers.image_edit）或参考图生视频模型（video.reference_model），
	// 结束"只有第一个角色有图像级约束"的历史。
	for i := range plan.Tasks {
		var refs []string
		var names []string
		for _, name := range plan.Tasks[i].Characters {
			if c, ok := kit.CharOf(name); ok && c.RefImage != "" {
				refs = append(refs, c.RefImage)
				names = append(names, name)
			}
		}
		if len(refs) > 0 {
			plan.Tasks[i].RefImages = refs
			plan.Tasks[i].RefNames = names
			plan.Tasks[i].RefImage = refs[0]
			plan.Tasks[i].UsedRef = true
		}
	}
	// 场景基准图回填：同场次所有镜头共享同一张 establishing 基准（assets/scenes），
	// 出图时与角色参考帧一起喂给多参考图模型——环境一致性由此从"提示词的一句话"
	// 变成"一张可逐像素比对的基准图"。
	anchored, ferr := fillSceneRefs(opt.RunDir, plan.Tasks)
	if ferr != nil {
		return nil, ferr
	}
	if anchored > 0 {
		fmt.Printf("  🖼 场景基准图已接入 %d/%d 个镜头（环境一致性锚定）\n", anchored, len(plan.Tasks))
	} else {
		fmt.Println("  · 场景基准图未接入（素材库未生成场景图）：环境一致性退化为提示词描述，可在「素材库」生成后重渲")
	}

	outDir := filepath.Join(opt.RunDir, "output", "shots")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	// 两个清单是 deliver/vision/定点重渲的唯一依据：写失败必须显式暴露，
	// 静默失败会产生"渲染成功但下游什么都看不到"的断头 run（严格模式：直接失败）
	if err := fsx.AtomicWriteJSON(filepath.Join(opt.RunDir, "output", "shotplan.json"), plan); err != nil {
		return nil, fmt.Errorf("shotplan.json 写入失败（视觉复核/定点重渲的唯一依据）：%w", err)
	}

	manifest := &Manifest{
		RunID: filepath.Base(opt.RunDir), Quality: opt.Quality,
		StartedAt: time.Now(), Shots: make([]ShotRecord, 0, len(plan.Tasks)),
		Refs: refs, PlanDuration: totalSec,
	}
	manifest.Cost.EstimatedCNY = est.TotalCNY

	gate := quality.NewGate(cfg.Quality)

	// 多参考图生成端点（可选）：两人同框等镜头用它直接产出"合影式"首帧。
	// 未配置（留空）时显式走单参考帧/纯文生图路径；配置了就必须可用——
	// 严格模式：配置错误（端点不受支持/缺密钥）直接拒绝开工，不允许"配了等于没配"。
	var editClient *ai.Client
	if ep := cfg.Providers.ImageEdit; ep.BaseURL != "" && ep.Model != "" && !client.Mock() {
		if !ai.ImageEditSupported(ep.BaseURL) {
			return nil, fmt.Errorf("providers.image_edit 端点 %s 不受支持（目前仅火山方舟/阿里百炼）："+
				"请修正 base_url，或清空该配置以显式走单参考帧路径", ep.BaseURL)
		}
		if cfg.APIKeyFor(ep) == "" {
			return nil, fmt.Errorf("providers.image_edit 已配置但缺少密钥（环境变量 %s）："+
				"多参考图是双角色镜头的图像级一致性保障，拒绝静默跳过", ep.APIKeyEnv)
		}
		ec := client.For(ai.CapImageEdit, ai.Endpoint{
			BaseURL: ep.BaseURL, APIKeyEnv: ep.APIKeyEnv,
			APIKey: cfg.APIKeyFor(ep), Model: ep.Model,
		})
		if ec.Mock() {
			return nil, fmt.Errorf("providers.image_edit 端点 %s 不可用（该能力无密钥，退回离线模式）", ep.BaseURL)
		}
		editClient = ec
		fmt.Printf("  🧩 多参考图出图已启用: %s（模型 %s）\n", ec.EndpointInfo(), ep.Model)
	}
	// 环境锚在单参考帧路径下带不动：一次只能喂一张图，有角色的镜头会用它锁身份，
	// 场景基准图"配了却用不上"必须说出来——静默丢失正是这类保障最常见的死法。
	// 离线演示模式不出图，不适用（警告会误导为"配置问题"）。
	if anchored > 0 && editClient == nil && !client.Mock() {
		fmt.Println("  ⚠ providers.image_edit 未配置：角色+场景的多图约束不可用，场景基准图仅在无人物出镜的镜头生效（单参考帧一次只能带一张图）")
	}

	// 6. 逐镜头生成 + 客观质检 + 换种重渲
	manifest.Shots = generateAll(client.Context(), imgc, vidc, cfg, gate, opt, plan, outDir, editClient)

	// 定点重渲（--shots）只生成部分镜头：必须把历史 manifest 里未重渲镜头的
	// 留痕合并回来。否则清单会被本次运行覆盖成只剩几个镜头，deliver 会把
	// 磁盘上明明存在的镜头判成"缺失"而拒绝交付。
	if merged := mergePrevShots(opt.RunDir, manifest.Shots); len(merged) > len(manifest.Shots) {
		manifest.Shots = merged
	}

	// 7. 汇总与成本
	finishManifest(manifest, cfg)
	if byKind, sum := client.Spend(); sum > 0 {
		manifest.Cost.ActualCNY = sum
		manifest.Cost.ByKind = byKind
	}
	if err := fsx.AtomicWriteJSON(filepath.Join(opt.RunDir, "output", "render_manifest.json"), manifest); err != nil {
		fmt.Printf("  ⚠ render_manifest.json 写入失败: %v（deliver 将无法看到本次渲染结果）\n", err)
	}
	printSummary(manifest)
	return manifest, nil
}

// chainFrame 链式衔接的承接帧：上镜 ID + 末帧路径（path 为空 = 不承接）。
type chainFrame struct {
	prevID string
	path   string
}

// generateAll 并发执行全部镜头；每轮结束后把不合格镜头带进下一轮重渲。
//
// 链式衔接（同场景末帧→首帧）引入了镜头间依赖：承接镜头必须等上一镜产出后
// 才能开工。实现为"轮内小循环"——每遍收集就绪任务、并发派发、等待完成后
// 重新收集，直到本轮再无可推进的任务。非承接镜头的并发行为不变；
// 上一镜失败时承接镜头退化为分镜首帧，绝不因衔接依赖留下空洞镜头。
func generateAll(parent context.Context, imgc, vidc *ai.Client, cfg *config.Config, gate *quality.Gate,
	opt Options, plan *shotkit.Plan, outDir string, editClient *ai.Client) []ShotRecord {

	records := make([]ShotRecord, len(plan.Tasks))
	for i, t := range plan.Tasks {
		records[i] = ShotRecord{
			ID: t.ShotID, Status: "pending", PlanDur: t.DurationSec,
			Seed: t.Seed, RefUsed: t.RefImage != "", Severity: quality.SeverityOK,
			Night: t.Night,
		}
	}
	idxOf := make(map[string]int, len(plan.Tasks))
	for i, t := range plan.Tasks {
		idxOf[t.ShotID] = i
	}

	// chainFrameOf 解析镜头 i 的承接末帧：上镜已完成则提取/复用其末帧。
	// 返回状态："ready"（可开工，含承接帧或不承接）、"wait"（等上镜）、
	// "skip"（上镜失败/不在本批：按分镜首帧兜底）。
	chainFrameOf := func(i int) (chainFrame, string) {
		if !cfg.Video.ChainSameScene || !plan.Tasks[i].ChainFromPrev {
			return chainFrame{}, "ready"
		}
		prevID, ok := plan.PrevOf[plan.Tasks[i].ShotID]
		if !ok {
			return chainFrame{}, "ready" // 片头/跨场景：无承接
		}
		pi, ok := idxOf[prevID]
		if !ok {
			return chainFrame{}, "skip" // 定点重渲等场景：上镜不在本批，按分镜首帧走
		}
		switch records[pi].Status {
		case "done":
			return chainFrame{prevID: prevID, path: ensureLastFrame(opt.RunDir, &records[pi])}, "ready"
		case "pending":
			return chainFrame{}, "wait"
		default:
			return chainFrame{}, "skip"
		}
	}

	var fatal error
	dispatch := func(pending []int, chainFrames map[int]chainFrame, round int) {
		runCtx, cancel := context.WithCancel(parent)
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxInt(1, opt.Workers))
		for _, idx := range pending {
			mu.Lock()
			stop := fatal != nil
			mu.Unlock()
			if stop {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				t := plan.Tasks[i]
				if round > 0 {
					t.Seed = shotkit.DeriveSeed(t.ShotID, round, seedSalt(t))
				}
				rec := renderShot(runCtx, imgc, vidc, cfg, gate, opt, t, outDir, round, editClient, chainFrames[i])
				mu.Lock()
				records[i] = mergeRecord(records[i], rec)
				if rec.Error != "" && fatal == nil && ai.IsFatal(fmt.Errorf("%s", rec.Error)) {
					fatal = fmt.Errorf("%s", rec.Error)
					cancel()
				}
				mu.Unlock()
			}(idx)
		}
		wg.Wait()
		cancel()
	}

	// 定点重渲（--shots）= 换种重渲：直接以第 1 轮种子进入，旧产物不参与断点复用——
	// 重渲的意义就是换一次结果；round 0 的复用分支只为全新渲染/断点续跑服务。
	base := 0
	if len(opt.ShotIDs) > 0 {
		base = 1
	}
	for round := base; round <= cfg.Video.RerenderRounds+base; round++ {
		if round > 0 && opt.ImagesOnly {
			break // --images-only：没有视频就没有"重渲"，首帧失败交给下一轮完整渲染
		}
		if round > 0 {
			fmt.Printf("↻ 第 %d 轮重渲：%d 个镜头待定（换种子 + 提示词变体，参考图不变）\n", round, countPending(records))
		}
		// 轮内推进循环：承接镜头在上一镜完成后的下一遍被拾起。
		// 末帧解析（ensureLastFrame 会写 records[pi].LastFrame）只在主 goroutine
		// 的收集阶段进行，避免与 worker 的 records 写入竞争。
		// dispatched 保证每个镜头本轮只派发一次——失败镜头的补救交给下一轮
		// 换种重渲，否则这里会无限重试同一批失败镜头。
		dispatched := map[int]bool{}
		for {
			var pending []int
			chainFrames := map[int]chainFrame{}
			for i := range plan.Tasks {
				if dispatched[i] {
					continue
				}
				t := plan.Tasks[i]
				vid := filepath.Join(outDir, t.ShotID+".mp4")
				if round == 0 {
					// 断点续跑：已存在的合格产物直接复用（已付费的资产不能重买）
					if fsx.FileExists(vid) {
						if records[i].Status != "done" {
							records[i].Status = "done"
							records[i].Video = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".mp4"))
							img := filepath.Join(outDir, t.ShotID+".png")
							if fsx.FileExists(img) {
								records[i].Image = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".png"))
							}
							if d, err := shotkit.ProbeDuration(vid); err == nil {
								records[i].ActualDur = d
							}
							fmt.Printf("  ⇢ %s 已存在，跳过（断点续跑）\n", t.ShotID)
						}
						dispatched[i] = true
						continue
					}
					cf, state := chainFrameOf(i)
					if state == "wait" {
						continue // 上镜还没产出：本轮下一遍再试
					}
					chainFrames[i] = cf
					dispatched[i] = true
					pending = append(pending, i)
					continue
				}
				if records[i].Status != "done" || records[i].Severity == quality.SeverityRerender {
					cf, _ := chainFrameOf(i)
					chainFrames[i] = cf
					dispatched[i] = true
					pending = append(pending, i)
				}
			}
			if len(pending) == 0 {
				break
			}
			dispatch(pending, chainFrames, round)
			if fatal != nil {
				break
			}
		}
		if fatal != nil {
			fmt.Printf("  ⛔ 不可恢复错误，已中止整批渲染：%v\n", fatal)
			break
		}
	}
	// 兜底：因"等上镜"而一直没跑的承接镜头（上镜失败等），按分镜首帧补齐，
	// 绝不让依赖关系把镜头变成空洞。
	var leftovers []int
	for i := range records {
		if records[i].Status == "pending" {
			leftovers = append(leftovers, i)
		}
	}
	if len(leftovers) > 0 && fatal == nil {
		fmt.Printf("  ↪ %d 个镜头的衔接依赖未满足，按分镜首帧兜底生成\n", len(leftovers))
		dispatch(leftovers, map[int]chainFrame{}, 0)
	}
	return records
}

func countPending(records []ShotRecord) int {
	n := 0
	for _, r := range records {
		if r.Status != "done" {
			n++
		}
	}
	return n
}

// ensureLastFrame 提取/复用一镜的末帧（链式衔接用）。
// 返回末帧的绝对路径；提取失败返回 ""（调用方按分镜首帧兜底）。
func ensureLastFrame(runDir string, rec *ShotRecord) string {
	if rec.Video == "" {
		return ""
	}
	if rec.LastFrame != "" {
		p := filepath.Join(runDir, filepath.FromSlash(rec.LastFrame))
		if fsx.FileExists(p) {
			return p
		}
	}
	vid := filepath.Join(runDir, filepath.FromSlash(rec.Video))
	out := filepath.Join(runDir, "output", "frames", rec.ID+"_last.png")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return ""
	}
	if err := extractLastFrame(vid, out); err != nil {
		fmt.Printf("  ⚠ %s 末帧提取失败：%v（该镜头按分镜首帧衔接）\n", rec.ID, err)
		return ""
	}
	rec.LastFrame = filepath.ToSlash(filepath.Join("output", "frames", rec.ID+"_last.png"))
	return out
}

// extractLastFrame 用 ffmpeg 抽取视频最后一帧。
func extractLastFrame(vidPath, outPNG string) error {
	_, err := ffx.Run(2*time.Minute, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-sseof", "-0.1", "-i", vidPath,
		"-frames:v", "1", "-update", "1", "-q:v", "2",
		outPNG)
	return err
}

func seedSalt(t shotkit.Task) string {
	if len(t.RefChars) > 0 {
		return t.RefChars[0]
	}
	return ""
}

// printSamplePrompt 展示首个镜头的提示词与运动描述。
// 生产环境里"提示词质量"决定了成片是否像 AI，开跑前应当可检查。
func printSamplePrompt(plan *shotkit.Plan) {
	if len(plan.Tasks) == 0 {
		return
	}
	t := plan.Tasks[0]
	fmt.Printf("\n—— 示例提示词（镜头 %s，种子 %d，参考图=%v）——\n", t.ShotID, t.Seed, t.RefImage != "")
	fmt.Println(t.ImagePrompt)
	fmt.Println("—— 运动提示词 ——")
	fmt.Println(t.VideoPrompt)
	fmt.Printf("—— 负面提示词 ——\n%s\n\n", t.Negative)
}

// mergeRecord 把本轮结果合并进累计记录（保留历史轮次，便于事后复核"每轮为什么失败"）。
func mergeRecord(prev, cur ShotRecord) ShotRecord {
	prev.Attempts = cur.Attempts
	prev.Rounds = append(prev.Rounds, cur.Rounds...)
	prev.Seed = cur.Seed
	prev.UsedRef = cur.UsedRef
	prev.RefUsed = cur.RefUsed
	if cur.Model != "" {
		prev.Model = cur.Model
	}
	prev.Severity = cur.Severity
	prev.Defects = cur.Defects
	prev.Error = cur.Error
	prev.VideoMetrics = cur.VideoMetrics
	prev.ImageMetrics = cur.ImageMetrics
	if cur.Status == "done" {
		prev.Status = "done"
		prev.Video = cur.Video
		prev.Image = cur.Image
		prev.ActualDur = cur.ActualDur
		prev.Error = ""
	}
	if cur.Status == "imaged" {
		// --images-only 的产物状态必须落账：否则 manifest 全记 pending，
		// 镜头被误判缺失、动态分镜拿不到首帧路径，已付费的首帧"看不见"。
		prev.Status = "imaged"
		prev.Image = cur.Image
	}
	return prev
}

// renderShot 生成单个镜头：首帧（参考图锁定身份 / 承接上镜末帧）→ 图生视频 → 客观质检。
//
// 首帧来源优先级（一致性从高到低）：
//  1. 链式衔接：直接用上一镜末帧（同场景连续性最好的方式，且省一次出图费）
//  2. 多参考图模型：两人同框等镜头把全部角色参考帧一起喂给 image_edit 端点
//  3. 单参考帧图生图：主角色参考帧 + ref_strength（单角色镜头的基线手段）
//  4. 纯文生图：无角色出镜的镜头
func renderShot(ctx context.Context, imgc, vidc *ai.Client, cfg *config.Config, gate *quality.Gate,
	opt Options, t shotkit.Task, outDir string, round int, editClient *ai.Client, cf chainFrame) ShotRecord {

	rec := ShotRecord{
		ID: t.ShotID, Status: "failed", PlanDur: t.DurationSec,
		Seed: t.Seed, RefUsed: t.RefImage != "", UsedRef: t.RefImage != "",
		Severity: quality.SeverityOK,
	}
	chainPath := cf.path
	if chainPath != "" {
		rec.ChainedFrom = cf.prevID
	}
	imgPath := filepath.Join(outDir, t.ShotID+".png")
	vidPath := filepath.Join(outDir, t.ShotID+".mp4")
	plannedPath := filepath.Join(outDir, t.ShotID+".planned.png")
	invalidDir := filepath.Join(outDir, "invalid")
	_ = os.MkdirAll(invalidDir, 0o755)

	start := time.Now()
	rr := RoundRecord{Round: round, Seed: t.Seed, StartedAt: start}

	// 重渲轮（round>0）先把上一轮产物移开暂存，两个目的：
	//  1. 首帧图不再因"文件已存在"被复用——换种重渲必须换一次结果（含首帧），
	//     否则图像级缺陷（构图错/糊）在结构上永远修不掉
	//  2. 本轮失败时上一轮的合格视频/图片能原样恢复——已付费的好资产不能
	//     因为一轮失败的重渲被隔离销毁（manifest 记 done、文件却在 invalid/）
	var prevVid, prevImg, prevPlanned string
	if round > 0 {
		prevVid = setAside(vidPath)
		prevImg = setAside(imgPath)
		// 锚点同样"换种重渲换一次结果"：提示词变体变了，复用旧锚点
		// 会把新 take 又钉回旧构图
		if cfg.Video.EndFrameAnchor {
			prevPlanned = setAside(plannedPath)
		}
	}

	// 离线演示：用 ffmpeg 生成真实可探测的占位素材，保证字幕重排/混音/交付校验链路可验证
	if imgc.Mock() {
		if err := mockAssets(cfg, t, imgPath, vidPath); err != nil {
			rr.Error = "离线占位素材生成失败: " + err.Error()
			rr.Severity = quality.SeverityBlock
			rec.Error = rr.Error
			rec.Severity = quality.SeverityBlock
		} else {
			rec.Status = "done"
			rec.Image = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".png"))
			rec.Video = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".mp4"))
			if d, err := shotkit.ProbeDuration(vidPath); err == nil {
				rec.ActualDur = d
			} else {
				rec.ActualDur = t.DurationSec
			}
			rec.Attempts = 1
		}
		rr.DurationSec = time.Since(start).Seconds()
		rec.Rounds = []RoundRecord{rr}
		return rec
	}

	// 提示词组装：官方 API 没有 negative_prompt / seed 参数——
	// 负面词内嵌进首帧提示词（"avoid:"），重渲轮次用提示词变体真正改变结果
	// （实测 cogvideox 忽略种子字段，只换种子等于花两倍钱买同一张图）。
	imgPrompt := withNegatives(t.ImagePrompt, t.Negative)
	videoPrompt := t.VideoPrompt
	if round > 0 {
		imgPrompt = variantFor(imgPrompt, round, 990)
		videoPrompt = variantFor(t.VideoPrompt, round, 500)
	}

	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= cfg.Video.ShotRetries; attempt++ {
		attempts = attempt
		if ctx.Err() != nil {
			lastErr = fmt.Errorf("批次已取消")
			break
		}
		if attempt > 1 {
			wait := time.Duration(attempt) * 5 * time.Second
			fmt.Printf("  ↻ %s 第 %d 次尝试（等待 %v）\n", t.ShotID, attempt, wait)
			select {
			case <-ctx.Done():
				lastErr = fmt.Errorf("批次已取消")
			case <-time.After(wait):
			}
			if lastErr != nil {
				break
			}
		}

		// ---- 1) 首帧图（链式承接 > 多参考图 > 单参考帧 > 纯文生图）----
		var iv quality.ImageVerdict
		if !fsx.FileExists(imgPath) {
			model, usedRef, ferr := genFirstFrame(imgc, editClient, cfg, opt, t, imgPrompt, chainPath, imgPath)
			if ferr != nil {
				lastErr = fmt.Errorf("首帧出图失败: %w", ferr)
				rr.Error = lastErr.Error()
				if ai.IsFatal(ferr) {
					break
				}
				continue
			}
			rec.Model = model
			rec.UsedRef = usedRef
			fmt.Printf("  🖼 %s 首帧完成（参考图=%v 模型=%s 种子=%d）\n", t.ShotID, usedRef, model, t.Seed)
		}

		// ---- 2) 首帧客观质检：不合格就别送去做视频（视频比图贵得多）----
		iv = gate.CheckImageScene(imgPath, t.MustHaveFace, quality.SceneCtx{Night: t.Night})
		rr.ImageVerdict = &iv
		if !iv.OK && iv.Severity == quality.SeverityBlock {
			lastErr = fmt.Errorf("首帧质检未通过: %s", strings.Join(iv.Codes(), ","))
			rr.Error = lastErr.Error()
			rr.Severity = quality.SeverityBlock
			quarantine(imgPath, invalidDir, t.ShotID, round, "img")
			continue
		}

		// ---- 2.5) --images-only：首帧就位即完成（animatic 预览路径）----
		if opt.ImagesOnly {
			rec.Status = "imaged"
			rec.Image = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".png"))
			rec.ImageMetrics = &iv
			rec.Attempts = attempts
			rr.DurationSec = time.Since(start).Seconds()
			rr.Severity = quality.SeverityOK
			rec.Rounds = []RoundRecord{rr}
			fmt.Printf("  🖼 %s 首帧就绪（--images-only：跳过视频生成）\n", t.ShotID)
			return rec
		}

		// ---- 3) 图生视频 ----
		ref, err := shotkit.ReadAsDataURL(imgPath)
		if err != nil {
			lastErr = fmt.Errorf("读取首帧失败: %w", err)
			rr.Error = lastErr.Error()
			continue
		}
		// 尾帧锚点（首尾帧数组的第二张）：把运动的终点钉在分镜构图上。
		// 配置开启后**每镜都带**（不再只限链式承接镜头）——有承接时首帧=上镜
		// 末帧、尾帧=本镜构图（首尾夹逼，接缝连续且运动有终点）；无承接时
		// 首尾两端同为分镜构图（画面锁在两端，中途不漂离）。
		// 严格模式：开启后锚点就必须就绪——生成/质检失败即本轮失败（重试），
		// 不允许静默退回单首帧（那等于把"运动终点被钉住"的保障悄悄拿掉）。
		endImage := ""
		// 多模态参考生视频（video.reference_model，Seedance 2.x 全模态参考）：
		// 一次上传该镜头相关的全部参考图，并在提示词里逐张声明用途（@imageN）——
		// 出镜角色标准像（身份锚）→ 场次 establishing 基准（环境锚）→ 本镜首帧（构图锚）。
		// 参考任务与首尾帧任务在官方语义下二选一，走参考模式即放弃尾帧锚点，
		// 构图控制改由"首帧参考图声明"承担。
		// 首帧已在盘上（--images-only / 断点续跑）时链式承接本就不会被消费
		// （复用优先），不再因 chainPath 把镜头挡回首尾帧路径。
		firstFrameOnDisk := fsx.FileExists(imgPath)
		viaRefModel := cfg.Video.ReferenceModel != "" &&
			(len(t.RefImages) > 0 || t.SceneRefImage != "") &&
			(chainPath == "" || firstFrameOnDisk)
		if cfg.Video.EndFrameAnchor && !viaRefModel {
			// 已存在则复用（断点续跑/失败重试省钱）；重渲轮会被 setAside 移走强制重出
			if !fsx.FileExists(plannedPath) {
				if _, _, _, perr := genPlannedFrame(imgc, editClient, cfg, opt, t, imgPrompt, plannedPath); perr != nil {
					lastErr = fmt.Errorf("尾帧锚点生成失败（end_frame_anchor 已开启，拒绝退回单首帧）: %w", perr)
					rr.Error = lastErr.Error()
					continue
				}
			}
			pv := gate.CheckImageScene(plannedPath, t.MustHaveFace, quality.SceneCtx{Night: t.Night})
			if !pv.OK && pv.Severity == quality.SeverityBlock {
				// 坏锚点不留在盘上反复复用：隔离留证，本轮失败重出
				quarantine(plannedPath, invalidDir, t.ShotID, round, "anchor")
				lastErr = fmt.Errorf("尾帧锚点质检未通过（%s）（end_frame_anchor 已开启，拒绝退回单首帧）", strings.Join(pv.Codes(), ","))
				rr.Error = lastErr.Error()
				continue
			}
			data, derr := shotkit.ReadAsDataURL(plannedPath)
			if derr != nil {
				lastErr = fmt.Errorf("读取尾帧锚点失败: %w", derr)
				rr.Error = lastErr.Error()
				continue
			}
			endImage = data
			rec.PlannedFrame = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".planned.png"))
			fmt.Printf("  🖼 %s 尾帧锚点就绪（分镜构图，首尾帧锁定）\n", t.ShotID)
		} else if viaRefModel {
			// 参考生视频任务与首/尾帧在官方任务类型上互斥：不为一张用不上的图付费
			fmt.Printf("  ↷ %s 走多模态参考生视频（%s），该任务类型无首尾帧概念，跳过锚点\n", t.ShotID, cfg.Video.ReferenceModel)
		}
		// 多模态参考生视频（Seedance 2.x 全模态参考）：一次上传该镜头相关的全部
		// 参考照片，提示词里逐张声明用途（官方 @imageN 语法）。身份/环境/构图三类
		// 约束同时以图像级强度生效，一致性显著优于"单首帧 + 文字提示"。
		// 严格模式：这条路走了就必须走通——失败即本轮失败，不回落普通图生视频
		//（回落意味着同框第二人/环境锚/构图锚又失去图像级约束，是静默的产物劣化）。
		videoURL := ""
		if viaRefModel {
			refs, decls, rerr := buildOmniRefs(opt.RunDir, t, imgPath)
			if rerr != nil {
				lastErr = fmt.Errorf("多模态参考组装失败: %w", rerr)
				rr.Error = lastErr.Error()
				continue
			}
			vURL, verr := vidc.VideoGen("render.shot_video", videoPrompt+"\n\n"+decls, "", ai.VideoOptions{
				Model: cfg.Video.ReferenceModel, Timeout: opt.Timeout,
				Watermark: cfg.Video.Watermark, RefImages: refs,
				Size: cfg.Video.Size, AspectRatio: aspectOf(cfg.Video.Size),
				// 时长档位与首尾帧路径同一套换算（5/10s；2.x 支持到 30s），
				// 不传的话模型自选时长，交付重排会对不上分镜节奏
				Duration: videoDurationFor(t.DurationSec, cfg),
				// 运镜意图服从分镜，不交给模型自定（"固定"被推成横摇这类事
				// 只有显式参数能拦住）：vidu 系用运动幅度，Seedance 用 camera_fixed
				MovementAmplitude: promspec.MovementAmplitude(t.CameraMove),
				CameraFixed:       t.CameraMove == "固定",
			})
			if verr != nil {
				lastErr = fmt.Errorf("多模态参考生视频失败（%s）: %w", cfg.Video.ReferenceModel, verr)
				rr.Error = lastErr.Error()
				if ai.IsFatal(verr) {
					break
				}
				continue
			}
			videoURL = vURL
			rec.Model = cfg.Video.ReferenceModel
			rec.UsedRef = true
			fmt.Printf("  🎞 %s 多模态参考生视频（%s ×%d 参考图，逐张声明）\n", t.ShotID, cfg.Video.ReferenceModel, len(refs))
		} else {
			vURL, verr := vidc.VideoGen("render.shot_video_ref", videoPrompt, ref, ai.VideoOptions{
				Model: cfg.VideoModel(opt.Quality), Quality: qualityMode(opt.Quality),
				Size: cfg.Video.Size, FPS: cfg.Video.FPS, Timeout: opt.Timeout,
				WithAudio: cfg.Video.WithAudio,
				Watermark: cfg.Video.Watermark,
				// 计划时长 >5.5s 的镜头请求 10s 档：官方支持 5/10 两档，
				// 不用的话所有镜头都会被服务端压成 ~5s，只能靠后期拉伸凑时长
				Duration: videoDurationFor(t.DurationSec, cfg),
				// 首尾帧数组：首帧=上镜末帧（有承接时）或分镜构图，尾帧=本镜分镜构图
				// （video.end_frame_anchor）。严格模式：模型不支持即失败（不降级）。
				EndImage: endImage,
				// Seedance 的机位固定开关（智谱/vidu 忽略该字段）
				CameraFixed: t.CameraMove == "固定",
			})
			if verr != nil {
				lastErr = fmt.Errorf("图生视频失败: %w", verr)
				rr.Error = lastErr.Error()
				if ai.IsFatal(verr) {
					break
				}
				continue
			}
			videoURL = vURL
			if rec.Model == "" {
				rec.Model = cfg.VideoModel(opt.Quality)
			}
		}
		url := videoURL

		// 半截文件绝不落地为正式产物：下载失败就重试，不留下"看起来成功"的坏文件
		tmpVid := vidPath + ".part"
		if err := shotkit.Download(url, tmpVid); err != nil {
			lastErr = fmt.Errorf("视频下载失败: %w", err)
			rr.Error = lastErr.Error()
			_ = os.Remove(tmpVid)
			continue
		}

		// ---- 4) 视频客观质检（对临时文件做，通过后才改名生效）----
		vv := gate.CheckVideoScene(tmpVid, t.DurationSec, quality.SceneCtx{Night: t.Night})
		rr.VideoVerdict = &vv
		if !vv.OK && vv.Severity == quality.SeverityBlock {
			lastErr = fmt.Errorf("视频质检未通过: %s", strings.Join(defectCodes(vv.Defects), ","))
			rr.Error = lastErr.Error()
			rr.Severity = quality.SeverityBlock
			quarantine(tmpVid, invalidDir, t.ShotID, round, "vid")
			continue
		}
		if err := os.Rename(tmpVid, vidPath); err != nil {
			lastErr = fmt.Errorf("落盘视频失败: %w", err)
			rr.Error = lastErr.Error()
			continue
		}
		vidc.RecordVideoCost(vv.DurationSec)
		// 本轮成功：上一轮暂存产物降级为证据留档（新 take 已就位，旧 take 可对比复盘）
		retireAside(prevVid, invalidDir, t.ShotID, round, "vid")
		retireAside(prevImg, invalidDir, t.ShotID, round, "img")
		retireAside(prevPlanned, invalidDir, t.ShotID, round, "planned")

		rec.Status = "done"
		rec.Image = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".png"))
		rec.Video = filepath.ToSlash(filepath.Join("output", "shots", t.ShotID+".mp4"))
		ai.AttachCallArtifact(url, rec.Video) // 调用记录回填本地路径（平台 URL 会过期）；必须在 rec.Video 就位后调用，空路径会被静默忽略
		rec.ActualDur = vv.DurationSec
		rec.Attempts = attempts
		rec.VideoMetrics = &vv
		rec.ImageMetrics = &iv
		if !vv.OK {
			rec.Severity = vv.Severity
			rec.Defects = append(rec.Defects, vv.Defects...)
			fmt.Printf("  ⚠ %s 通过但存在可改进项：%s\n", t.ShotID, strings.Join(defectCodes(vv.Defects), ","))
		}
		rr.DurationSec = time.Since(start).Seconds()
		rr.Severity = rec.Severity
		rec.Rounds = []RoundRecord{rr}
		fmt.Printf("  🎬 %s 视频完成（实测 %.2fs / 计划 %.2fs）\n", t.ShotID, vv.DurationSec, t.DurationSec)
		return rec
	}

	rec.Attempts = attempts
	if lastErr != nil {
		rec.Error = lastErr.Error()
	}
	if rec.Severity == "" {
		rec.Severity = quality.SeverityBlock
	}
	rr.DurationSec = time.Since(start).Seconds()
	rr.Severity = rec.Severity
	if rr.Error == "" && rec.Error != "" {
		rr.Error = rec.Error
	}
	rec.Rounds = []RoundRecord{rr}
	// 本轮失败的收尾：
	//  - round>0：本轮新产物（若有）隔离留证，上一轮合格产物原样恢复——
	//    重渲失败绝不销毁已付费资产
	//  - round 0：没有暂存。vidPath 此处不可能存在（成功路径已 return）；
	//    imgPath 若在说明"图合格、视频失败"，保留它——断点续跑直接复用，
	//    不再花一次出图钱（此前无条件隔离等于白白销毁合格的付费资产）
	if round > 0 {
		restoreAside(vidPath, prevVid, invalidDir, t.ShotID, round, "vid")
		restoreAside(imgPath, prevImg, invalidDir, t.ShotID, round, "img")
		if cfg.Video.EndFrameAnchor {
			restoreAside(plannedPath, prevPlanned, invalidDir, t.ShotID, round, "planned")
		}
	}
	return rec
}

// setAside 把既有产物移到旁路暂存（.prev 后缀），返回暂存路径（无文件时返回 ""）。
func setAside(path string) string {
	if !fsx.FileExists(path) {
		return ""
	}
	aside := path + ".prev"
	if err := os.Rename(path, aside); err != nil {
		return ""
	}
	return aside
}

// restoreAside 重渲失败收尾：本轮新产物（若有）隔离留证，上一轮产物恢复原位。
func restoreAside(path, aside, invalidDir, shotID string, round int, kind string) {
	if fsx.FileExists(path) {
		quarantine(path, invalidDir, shotID, round, kind)
	}
	if aside != "" {
		_ = os.Rename(aside, path)
	}
}

// retireAside 重渲成功后处置上一轮暂存产物：挪进隔离区留证（而非删除），
// 事后可对比"换种前后的两个 take"；挪不动就删掉，不残留脏文件。
func retireAside(aside, invalidDir, shotID string, round int, kind string) {
	if aside == "" {
		return
	}
	dst := filepath.Join(invalidDir, fmt.Sprintf("%s_r%d_prev_%s%s", shotID, round, kind, filepath.Ext(aside)))
	if err := os.Rename(aside, dst); err != nil {
		_ = os.Remove(aside)
	}
}

// quarantine 把不合格产物移入隔离区。
func quarantine(path, invalidDir, shotID string, round int, kind string) {
	if !fsx.FileExists(path) {
		return
	}
	dst := filepath.Join(invalidDir, fmt.Sprintf("%s_r%d_%s%s", shotID, round, kind, filepath.Ext(path)))
	_ = os.Rename(path, dst)
}

// genFirstFrame 生成首帧，按一致性强度从高到低四级降级：
//  1. 链式承接：上一镜末帧直接规整成本镜首帧（零成本、连续性最好）
//  2. 多参考图模型：全部出镜角色参考帧一起喂给 image_edit 端点（同框双方都有约束）
//  3. 单参考帧图生图：主角色参考帧 + ref_strength（单角色基线手段）
//  4. 纯文生图：无角色出镜的镜头
//
// 返回（模型标识, 是否用了参考图, 错误）。
func genFirstFrame(client, editClient *ai.Client, cfg *config.Config, opt Options,
	t shotkit.Task, imgPrompt, chainPath, imgPath string) (string, bool, error) {

	// 1) 链式承接
	if chainPath != "" {
		tmp := filepath.Join(filepath.Dir(imgPath), "."+t.ShotID+".chain.png")
		tw, th := firstFrameSize(cfg)
		if err := shotkit.NormalizeFrame(chainPath, tmp, tw, th); err != nil {
			return "", false, fmt.Errorf("承接末帧规整失败: %w", err)
		}
		if err := os.Rename(tmp, imgPath); err != nil {
			return "", false, fmt.Errorf("承接末帧落盘失败: %w", err)
		}
		fmt.Printf("  🖼 %s 首帧=上镜末帧（%s）\n", t.ShotID, filepath.Base(chainPath))
		return "chain-prev-frame", true, nil
	}
	_, model, usedRef, err := genPlannedFrame(client, editClient, cfg, opt, t, imgPrompt, imgPath)
	return model, usedRef, err
}

// plannedFrame 链式衔接的尾帧锚点产物：绝对路径 + 相对 run 目录的留痕路径。
type plannedFrame struct {
	path string
	rel  string
}

// sceneRefNote 场景基准图随请求附上时的声明：明确它的位置与用途——
// 不声明的话模型只会把最后一张图当"又一张泛泛的参考"，环境锚定落不到实处。
// 只在场景基准真的随请求发出时才能追加（图序最后一张）。
const sceneRefNote = "\nEnvironment reference: the final reference image is this scene's establishing " +
	"baseline — keep the location, set dressing, lighting direction and atmosphere consistent with it."

// plannedRefImages 组装出图参考图序：角色在前（身份锚），场景基准在后（环境锚）。
//
// 顺序即语义：多参考图请求里模型按序理解每张图，prompt 也按这个顺序声明，
// 二者错位会让"环境锚点"被当成"又一个角色"。
// 不在这里预截断：4 张上限由多参考图层统一执行（media_edit，截断后登记存档
// 的才是真实随请求发出的那几张）。
func plannedRefImages(t shotkit.Task) []string {
	refs := make([]string, 0, len(t.RefImages)+1)
	refs = append(refs, t.RefImages...)
	if t.SceneRefImage != "" {
		refs = append(refs, t.SceneRefImage)
	}
	return refs
}

// buildOmniRefs 组装多模态参考生视频的参考图序与逐张用途声明（官方 @imageN 语法）。
//
// 顺序即语义，声明编号必须与请求 content 里参考图的实际顺序一一对应：
//   - 出镜角色标准像（身份锚）：RefNames 与 RefImages 同源同序，缺参考帧的角色
//     两侧同时缺席，不会张冠李戴
//   - 场次 establishing 基准（环境锚）
//   - 本镜首帧（构图锚，已定稿时）：替代参考任务里不存在的首帧 role
//
// 返回（参考图 data URL 列表, 追加到提示词尾部的声明块, 错误）。
func buildOmniRefs(runDir string, t shotkit.Task, firstFramePath string) ([]string, string, error) {
	refs := make([]string, 0, len(t.RefImages)+2)
	var b strings.Builder
	b.WriteString("Reference images, each declared by position (all attached as reference_image):\n")
	add := func(path, decl string) error {
		if !filepath.IsAbs(path) {
			path = filepath.Join(runDir, filepath.FromSlash(path))
		}
		data, err := shotkit.ReadAsDataURL(path)
		if err != nil {
			return err
		}
		refs = append(refs, data)
		fmt.Fprintf(&b, "- @image%d %s\n", len(refs), decl)
		return nil
	}
	for i, name := range t.RefNames {
		if i >= len(t.RefImages) {
			break
		}
		decl := fmt.Sprintf("is the standard portrait of %s — every appearance of %s in the video must match this face, "+
			"hairstyle and wardrobe exactly; identity stays locked.", name, name)
		if err := add(t.RefImages[i], decl); err != nil {
			return nil, "", fmt.Errorf("角色参考帧（%s）读取失败: %w", name, err)
		}
	}
	if t.SceneRefImage != "" {
		if err := add(t.SceneRefImage,
			"is the establishing baseline of this scene's location — keep the environment, set dressing, "+
				"lighting direction and atmosphere consistent with it."); err != nil {
			return nil, "", fmt.Errorf("场景基准图读取失败: %w", err)
		}
	}
	if firstFramePath != "" && fsx.FileExists(firstFramePath) {
		if err := add(firstFramePath,
			"is the approved opening frame of this shot — begin the video from this exact framing "+
				"and keep the composition anchored to it."); err != nil {
			return nil, "", fmt.Errorf("首帧（构图锚）读取失败: %w", err)
		}
	}
	if len(refs) == 0 {
		return nil, "", fmt.Errorf("多模态参考生视频需要至少一张参考图（角色/场景/首帧均未就绪）")
	}
	return refs, b.String(), nil
}

// fillSceneRefs 把场次 establishing 基准图回填到生成计划的每个镜头（环境一致性锚点）。
//
// 选图规则：shotspec 声明了 scene_state 的镜头取该状态变体（如"雷云散尽"的美术
// 基准），其余取场次主基准。这条绑定不能反着做（一律用主基准）：高潮段环境已经
// 剧变，却拿初始状态的基准图当"必须保持一致"的参照，锚点会跟画面描述打架。
//
// 语义边界（"未提供"与"丢失"必须分开）：
//   - 素材库没有场景条目（老 run / 跳过素材阶段）= 未提供：不算失败，环境一致性
//     退化为提示词描述，由调用方打印说明。
//   - 声明了状态但索引里没有该变体 / 索引声明了文件但文件不在盘上 = 素材缺失：
//     显式失败——静默回退会让"这一镜的环境锚"变成另一张图或整个消失，
//     而"以为锚了、其实没锚"比重渲前发现贵得多。
//
// 返回成功锚定的镜头数。
func fillSceneRefs(runDir string, tasks []shotkit.Task) (int, error) {
	ix := shotkit.LoadAssetIndex(runDir)
	if len(ix.Scenes) == 0 {
		return 0, nil
	}
	n := 0
	for i := range tasks {
		state := strings.TrimSpace(tasks[i].SceneRefState)
		rel := ""
		if state != "" {
			rel = ix.VariantOf(tasks[i].SceneID, state)
			if rel == "" {
				return n, fmt.Errorf("镜头 %s 声明了环境状态 %q，但素材索引中场次 %s 没有该变体的场景图\n"+
					"  处理：重新生成素材（studio assets --run %s）补齐该状态，或修正 shotspec 中的 scene_state；"+
					"静默回退主基准会让这一镜的环境锚指向另一个状态",
					tasks[i].ShotID, state, tasks[i].SceneID, filepath.ToSlash(runDir))
			}
		} else {
			rel = ix.BaselineOf(tasks[i].SceneID)
			if rel == "" {
				continue
			}
		}
		if !fsx.FileExists(filepath.Join(runDir, filepath.FromSlash(rel))) {
			return n, fmt.Errorf("场次 %s 的场景基准图缺失（素材索引声明的文件不在盘上）: %s\n"+
				"  处理：重新生成素材（studio assets --run %s），或删除 output/assets/index.json 中该条目后再渲染；"+
				"静默跳过会让整片的环境一致性锚定悄悄消失",
				tasks[i].SceneID, rel, filepath.ToSlash(runDir))
		}
		tasks[i].SceneRefImage = rel
		n++
	}
	return n, nil
}

// genPlannedFrame 按"分镜构图"生成一张首帧（多参考图 > 单参考帧 > 纯文生图）。
// 两个用途：
//   - 非链式镜头的首帧（genFirstFrame 的 2-4 级路径）
//   - 链式镜头的尾帧锚点（首尾帧数组的第二张：把运动的终点钉在分镜构图上）
//
// 返回（产物路径, 模型标识, 是否用了参考图, 错误）。
func genPlannedFrame(client, editClient *ai.Client, cfg *config.Config, opt Options,
	t shotkit.Task, imgPrompt, outPath string) (plannedFrame, string, bool, error) {

	tw, th := firstFrameSize(cfg)
	rel := filepath.ToSlash(filepath.Join("output", "shots", filepath.Base(outPath)))
	refPaths := plannedRefImages(t)

	// 2) 多参考图模型：多角色同框，或"角色 + 场景基准"。
	// 严格模式：配置了 image_edit 且本镜需要多图约束，就必须走通——
	// 失败即本轮失败，不回落单参考帧（回落意味着同框第二人/环境锚失去图像级约束）。
	if editClient != nil && len(refPaths) >= 2 {
		// 场景基准排在参考图序最后，超上限（ai.MaxEditRefs）截断时会被裁掉：
		// 声明必须与实际发出的图一致，没发出去的图不能让提示词声称"以它为准"。
		if t.SceneRefImage != "" {
			if len(refPaths) <= ai.MaxEditRefs {
				imgPrompt += sceneRefNote
			} else {
				fmt.Printf("  ⚠ %s 参考图 %d 张超出单次上限 %d：场景基准图本次未随请求附上（角色身份锚优先）\n",
					t.ShotID, len(refPaths), ai.MaxEditRefs)
			}
		}
		refs := make([]string, 0, len(refPaths))
		for _, rp := range refPaths {
			ref, err := shotkit.ReadAsDataURL(filepath.Join(opt.RunDir, filepath.FromSlash(rp)))
			if err != nil {
				return plannedFrame{}, "", false, fmt.Errorf("多参考图出图需要全部参考图（角色/场景基准），读取 %s 失败: %w", rp, err)
			}
			refs = append(refs, ref)
		}
		url, model, err := editClient.ImageEditMulti("render.shot_image_edit", imgPrompt, refs, ai.ImageOptions{
			Model: cfg.Providers.ImageEdit.Model, Size: cfg.Image.Size,
		})
		if err != nil {
			return plannedFrame{}, "", false, fmt.Errorf("多参考图出图失败（providers.image_edit 已配置，拒绝降级单参考帧）: %w", err)
		}
		if err := shotkit.Download(url, outPath); err != nil {
			return plannedFrame{}, "", false, fmt.Errorf("多参考图出图下载失败: %w", err)
		}
		normalizeFirstFrame(outPath, tw, th)
		ai.AttachCallArtifact(url, rel)
		fmt.Printf("  🖼 %s 首帧（多参考图×%d 模型=%s）\n", t.ShotID, len(refs), model)
		return plannedFrame{outPath, rel}, "edit:" + model, true, nil
	}

	imgOpt := ai.ImageOptions{
		Model:     cfg.Image.Model,
		Size:      cfg.Image.Size,
		Quality:   cfg.Image.Quality,
		Watermark: cfg.Image.Watermark,
	}
	// 3) 单参考帧图生图：主角色参考帧优先；无人物出镜的环境镜头用场景基准图——
	// "同一个地点"要的正是这张图（纯文字描述环境的一致性远低于参考图）。
	// 严格模式：参考图读不出来即失败——静默丢掉参考图等于把该镜头降级成
	// 纯文生图（跨镜头换脸/环境漂移的根因）。
	primaryRef, fromScene := t.RefImage, false
	if primaryRef == "" && t.SceneRefImage != "" {
		primaryRef, fromScene = t.SceneRefImage, true
	}
	if primaryRef != "" && cfg.Video.ImageToImage {
		if fromScene {
			// 这一路图生图用的就是场景基准图：声明与实际一致
			imgPrompt += sceneRefNote
		}
		ref, err := shotkit.ReadAsDataURL(filepath.Join(opt.RunDir, filepath.FromSlash(primaryRef)))
		if err != nil {
			kind := "角色参考帧"
			if fromScene {
				kind = "场景基准图"
			}
			return plannedFrame{}, "", false, fmt.Errorf("读取%s失败（拒绝降级为纯文生图）: %w", kind, err)
		}
		imgOpt.RefImage = ref
		imgOpt.RefStrength = 0.6
	}
	// 4) 出图（有参考图 = 图生图；没有 = 纯文生图）
	url, model, err := client.ImageGenEx("render.shot_image", imgPrompt, imgOpt)
	if err != nil {
		return plannedFrame{}, "", false, err
	}
	if err := shotkit.Download(url, outPath); err != nil {
		return plannedFrame{}, "", false, fmt.Errorf("首帧下载失败: %w", err)
	}
	normalizeFirstFrame(outPath, tw, th)
	ai.AttachCallArtifact(url, rel)
	return plannedFrame{outPath, rel}, model, imgOpt.RefImage != "", nil
}

// firstFrameSize 首帧/锚点帧的规整尺寸（与视频分辨率一致，避免提交阶段报错）。
func firstFrameSize(cfg *config.Config) (int, int) {
	tw, th := parseSize(cfg.Video.Size)
	if tw == 0 {
		tw, th = 1920, 1080
	}
	return tw, th
}

// normalizeFirstFrame 本地规整尺寸：尺寸不符会在提交视频阶段报错，白花一次出图费。
func normalizeFirstFrame(imgPath string, tw, th int) {
	if tw <= 0 {
		return
	}
	tmp := strings.TrimSuffix(imgPath, filepath.Ext(imgPath)) + ".norm.png"
	if err := shotkit.NormalizeFrame(imgPath, tmp, tw, th); err == nil {
		if err := os.Rename(tmp, imgPath); err != nil {
			_ = os.Remove(tmp)
		}
	}
}

// withNegatives 把编译好的负面提示词内嵌进首帧提示词尾部。
// 智谱图像/视频接口都没有 negative_prompt 参数——不内嵌就等于整个负面词
// DSL 白写。图像侧对 "avoid:" 指令的遵守度远好于视频侧，且首帧决定画面基调。
func withNegatives(prompt, negative string) string {
	neg := strings.TrimSpace(negative)
	if neg == "" {
		return prompt
	}
	budget := 990 - utf8.RuneCountInString(prompt) - len(" | avoid: ")
	if budget <= 0 {
		return prompt
	}
	// 装不下时按条截断（负面组已按重要性排序：face/anatomy 在前），
	// 而不是整个放弃——整段丢弃意味着反 AI 伪影的负面词对最长的
	// 主角镜头恰恰一个都送不出去，防线在最需要的地方失效
	if utf8.RuneCountInString(neg) <= budget {
		return prompt + " | avoid: " + neg
	}
	used := 0
	var kept []string
	for _, it := range strings.Split(neg, ", ") {
		n := utf8.RuneCountInString(it)
		if used > 0 {
			n += 2 // ", "
		}
		if used+n > budget {
			break
		}
		kept = append(kept, it)
		used += n
	}
	if len(kept) == 0 {
		return prompt
	}
	return prompt + " | avoid: " + strings.Join(kept, ", ")
}

// roundVariants 换种重渲的提示词变体：实测 cogvideox 系忽略 seed 字段，
// 只换种子 = 花双倍钱买同一张图。变体 + 种子一起换，重渲才有实际意义。
// 变体按轮次循环取（见 variantFor），把 rerender_rounds 调大也不会退化成
// "悄悄重跑同一提示词"。
var roundVariants = []string{
	" Alternative take: slightly different framing and action timing, same characters, same location, same lighting.",
	" Another take: different camera angle and composition, same characters and story beat, fresh staging.",
	" Alternate performance pass: calmer pacing, more restrained gestures and glances, same characters and continuity.",
	" Alternate take: tighter composition with more negative space, slower camera move, same characters and story beat.",
}

// variantFor 追加第 round 轮的变体描述（round 从 1 起，变体循环取）；
// 超出 limit（接口提示词上限）则原样返回。
func variantFor(prompt string, round, limit int) string {
	if round <= 0 || len(roundVariants) == 0 {
		return prompt
	}
	v := roundVariants[(round-1)%len(roundVariants)]
	if utf8.RuneCountInString(prompt)+utf8.RuneCountInString(v) > limit {
		return prompt
	}
	return prompt + v
}

// videoDurationFor 计划时长超过 5.5s 的镜头请求 10s 档（CogVideoX-3 官方支持 5/10）。
func videoDurationFor(planSec float64, cfg *config.Config) int {
	if cfg.Video.AllowLongTakes && planSec > 5.5 {
		return 10
	}
	return 0
}

// aspectOf 从分辨率字符串推导 vidu 系的 aspect_ratio 参数。
func aspectOf(size string) string {
	w, h := 0, 0
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return "16:9"
	}
	ratio := float64(w) / float64(h)
	switch {
	case ratio > 1.5:
		return "16:9"
	case ratio < 0.67:
		return "9:16"
	default:
		return "1:1"
	}
}

func defectCodes(defects []quality.Defect) []string {
	out := make([]string, 0, len(defects))
	for _, d := range defects {
		out = append(out, d.Code)
	}
	return out
}

func parseSize(s string) (int, int) {
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil {
		return 0, 0
	}
	return w, h
}

func qualityMode(q string) string {
	if strings.EqualFold(q, "quality") {
		return "quality"
	}
	return "speed"
}

func riskRank(r string) int {
	switch r {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// loadKit 载入 shotspec 产物。
func loadKit(runDir string) (*promspec.Kit, error) {
	path := filepath.Join(runDir, "artifacts", "shotspec.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取生成规格失败（需先跑 shotspec 阶段）: %w", err)
	}
	var kit promspec.Kit
	if err := json.Unmarshal(data, &kit); err != nil {
		return nil, fmt.Errorf("生成规格解析失败: %w", err)
	}
	if len(kit.ShotList) == 0 {
		return nil, fmt.Errorf("生成规格中没有任何镜头")
	}
	return &kit, nil
}

// finishManifest 汇总交付完整性判定。这些计算直接决定"能不能交付"，必须确定性。
// mergePrevShots 读取上一次的渲染清单，把本次未涉及的镜头记录合并进来。
// 返回合并后的记录切片（本次记录优先，历史记录按 ID 去重补充）。
// 历史清单不存在或损坏时静默返回原切片——定点重渲不是必须运行过完整渲染。
func mergePrevShots(runDir string, current []ShotRecord) []ShotRecord {
	var prev Manifest
	if err := fsx.ReadJSON(filepath.Join(runDir, "output", "render_manifest.json"), &prev); err != nil {
		return current
	}
	have := make(map[string]bool, len(current))
	for _, r := range current {
		have[r.ID] = true
	}
	merged := current
	for _, r := range prev.Shots {
		if !have[r.ID] {
			merged = append(merged, r)
			have[r.ID] = true
		}
	}
	return merged
}

func finishManifest(m *Manifest, cfg *config.Config) {
	m.FinishedAt = time.Now()
	m.DurationMin = m.FinishedAt.Sub(m.StartedAt).Minutes()
	var actual float64
	var imaged int
	for _, s := range m.Shots {
		if s.Status == "done" {
			actual += s.ActualDur
			if len(s.Rounds) > 1 {
				m.Rerendered = append(m.Rerendered, s.ID)
			}
			continue
		}
		if s.Status == "imaged" {
			// --images-only 产物：不算缺失（首帧可复用），但也不是可交付镜头
			imaged++
			continue
		}
		m.Missing = append(m.Missing, s.ID)
		if s.Severity == quality.SeverityBlock {
			m.Blocked = append(m.Blocked, s.ID)
		}
	}
	m.Imaged = imaged
	m.ActualDuration = actual
	m.Complete = len(m.Missing) == 0
	if imaged > 0 {
		m.Notes = append(m.Notes, fmt.Sprintf(
			"--images-only 模式：%d 个镜头仅有首帧（animatic 预览用），尚无视频、不可交付；跑完整 render 复用首帧出视频", imaged))
	}
	if !m.Complete {
		m.Notes = append(m.Notes,
			fmt.Sprintf("缺 %d 个镜头（%s）：不允许作为成片交付", len(m.Missing), strings.Join(m.Missing, ",")))
	}
	tolerance := cfg.Quality.DurationTolerance
	if m.Complete && m.PlanDuration > 0 && absDiff(actual, m.PlanDuration) > tolerance {
		m.Notes = append(m.Notes, fmt.Sprintf(
			"成片时长 %.2fs 与计划 %.2fs 相差 %.2fs：字幕与配音必须按实测时长重排（deliver 阶段自动处理）",
			actual, m.PlanDuration, absDiff(actual, m.PlanDuration)))
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func printSummary(m *Manifest) {
	done := m.Count("done")
	fmt.Printf("■ 渲染结束：%d 成功 / %d 失败  用时 %.1f 分钟\n", done, len(m.Missing), m.DurationMin)
	if m.Imaged > 0 {
		fmt.Printf("  🖼 %d 个镜头仅产出首帧（--images-only）：跑 studio animatic 拼动态分镜预览\n", m.Imaged)
	}
	if m.Cost.ActualCNY > 0 {
		fmt.Printf("  ¥ 实际花费 %.2f 元（预估 %.2f 元）\n", m.Cost.ActualCNY, m.Cost.EstimatedCNY)
	}
	if len(m.Rerendered) > 0 {
		fmt.Printf("  ↻ 经历过换种重渲的镜头：%s\n", strings.Join(m.Rerendered, ","))
	}
	if !m.Complete {
		fmt.Printf("  ⛔ 交付不完整：缺失镜头 %s\n", strings.Join(m.Missing, ","))
		fmt.Println("     修复：重跑同一命令（断点续跑，只补失败镜头）；不要直接交付当前成片。")
	}
}

func dryManifest(opt Options, plan *shotkit.Plan, est ai.CostEstimate) *Manifest {
	m := &Manifest{
		RunID: filepath.Base(opt.RunDir), Quality: opt.Quality,
		StartedAt: time.Now(), FinishedAt: time.Now(),
	}
	for _, t := range plan.Tasks {
		m.PlanDuration += t.DurationSec
		m.Shots = append(m.Shots, ShotRecord{
			ID: t.ShotID, Status: "pending", PlanDur: t.DurationSec, Seed: t.Seed,
		})
	}
	m.Cost.EstimatedCNY = est.TotalCNY
	m.Notes = append(m.Notes, "dry-run：未产生任何付费调用")
	return m
}

// mockAssets 离线演示：用 ffmpeg 生成真实可探测的静帧+视频，
// 让字幕重排、混音、交付校验等下游链路在无 Key 环境下也能被真实验证。
func mockAssets(cfg *config.Config, t shotkit.Task, imgPath, vidPath string) error {
	if !ffx.Available("ffmpeg") {
		// 没有 ffmpeg 时退化为占位文件；下游会显式报错，而不是静默产出错片
		if err := fsx.AtomicWrite(imgPath, []byte("mock-image "+t.ShotID), 0o644); err != nil {
			return err
		}
		return fsx.AtomicWrite(vidPath, []byte("mock-video "+t.ShotID), 0o644)
	}
	w, h := parseSize(cfg.Video.Size)
	if w == 0 {
		w, h = 1920, 1080
	}
	if !fsx.FileExists(imgPath) {
		// 带噪点的灰阶静帧：有亮度方差，能通过"非纯色"质检
		if _, err := ffx.Run(60*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x303844:s=%dx%d", w, h),
			"-vf", "noise=alls=22:allf=t+u", "-frames:v", "1", imgPath); err != nil {
			return err
		}
	}
	_, err := ffx.Run(180*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-loop", "1", "-i", imgPath,
		"-t", fmt.Sprintf("%.3f", t.DurationSec),
		"-vf", "noise=alls=8:allf=t+u,format=yuv420p",
		"-r", "24",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28",
		"-an", vidPath)
	return err
}
