// Package engine 实现工作流引擎：
//   - 按 DAG 波次调度技能阶段：同一波次内依赖已满足的阶段并行执行
//   - 失败自动重试（带退避）；可规则判定的错误免 LLM 诊断；重试耗尽后调用 selfheal 诊断
//   - "review" 审片阶段不达标 → 按 Issues.stage 定向打回受影响阶段重做（专业级闭环）
//   - 上游失败/跳过自动级联标记下游 skipped，杜绝带病运行
//   - 状态持久化到 runs/<id>/state.json（原子写），支持断点续跑；记录 token 用量供成本核算
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/fsx"
	"aistudio/internal/selfheal"
	"aistudio/internal/skill"
	"aistudio/internal/skills"
)

// Stage 工作流阶段定义
type Stage struct {
	ID           string         `json:"id"`
	Skill        string         `json:"skill"`
	DependsOn    []string       `json:"depends_on,omitempty"`
	Params       map[string]any `json:"params,omitempty"`
	Retries      int            `json:"retries,omitempty"`       // 失败后额外重试次数（默认 1）
	MaxRevisions int            `json:"max_revisions,omitempty"` // 审片打回重做上限（仅 review 阶段）
}

// Describe 阶段标题（id 之后的展示名）
func (s *Stage) Describe() string {
	if t, ok := s.Params["title"].(string); ok && t != "" {
		return t
	}
	return s.ID
}

// Workflow 工作流定义
type Workflow struct {
	Name   string  `json:"name"`
	Brief  string  `json:"brief"`
	Stages []Stage `json:"stages"`
}

// LoadWorkflow 从 JSON 文件加载工作流并校验
func LoadWorkflow(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wf Workflow
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("工作流文件解析失败: %w", err)
	}
	seen := map[string]bool{}
	for i, s := range wf.Stages {
		if s.ID == "" || s.Skill == "" {
			return nil, fmt.Errorf("阶段 #%d 缺少 id 或 skill", i)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("阶段 ID 重复: %s", s.ID)
		}
		seen[s.ID] = true
		for _, dep := range s.DependsOn {
			if !seen[dep] {
				return nil, fmt.Errorf("阶段 %s 依赖的 %s 不在它之前", s.ID, dep)
			}
		}
	}
	return &wf, nil
}

// TruncateWorkflow 按阶段截断工作流，用于人工介入关口。
//
// until=true  ：只保留到该阶段（含），用于"先花几分钟确认剧本，再决定要不要花钱渲染"
// until=false ：从该阶段开始（前面的阶段从定义中移除），用于"只重跑后半段"
//
// 这是最省钱的开关：剧本跑偏时，下游全部白烧。生产流程必须能分段确认。
func TruncateWorkflow(wf *Workflow, stageID string, until bool) error {
	idx := -1
	for i, s := range wf.Stages {
		if s.ID == stageID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("阶段 %q 不在工作流中", stageID)
	}
	if until {
		wf.Stages = wf.Stages[:idx+1]
		return nil
	}
	kept := wf.Stages[idx:]
	// 被移除的阶段不能残留在依赖里，否则阶段永远无法就绪
	inKept := map[string]bool{}
	for _, s := range kept {
		inKept[s.ID] = true
	}
	for i := range kept {
		var deps []string
		for _, d := range kept[i].DependsOn {
			if inKept[d] {
				deps = append(deps, d)
			}
		}
		kept[i].DependsOn = deps
	}
	wf.Stages = kept
	return nil
}

// ---- 运行状态 ----

const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// StageState 单阶段运行状态
type StageState struct {
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	Revisions  int    `json:"revisions"`
	OutputFile string `json:"output_file,omitempty"`
	Error      string `json:"error,omitempty"`
	RepairNote string `json:"repair_note,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// RunState 一次运行的完整状态
type RunState struct {
	ID         string                  `json:"id"`
	Workflow   string                  `json:"workflow"`
	Brief      string                  `json:"brief"`
	Mock       bool                    `json:"mock"`
	StageOrder []string                `json:"stage_order,omitempty"` // 工作流阶段顺序（status 按此展示）
	Stages     map[string]*StageState  `json:"stages"`
	Usage      map[string]ai.TokenStat `json:"usage,omitempty"` // 每技能 token 用量（成本核算）
	UpdatedAt  time.Time               `json:"updated_at"`
}

// Engine 工作流引擎
type Engine struct {
	Reg    *skill.Registry
	Client *ai.Client
	Memory *selfheal.Memory
	State  *RunState
	Dir    string // runs/<id>
	// DrillFault 故障演练：注入一次模拟错误，完整演练"重试→AI 诊断→修复重试"路径
	DrillFault bool
	cfg        *config.Config // 生产参数（媒体生成/质检等技能需要；纯 LLM 阶段不依赖）
	wf         *Workflow
	feedback   map[string]string // 阶段 ID → 审片返修意见
	mu         sync.Mutex        // 保护并发波次下的状态修改 / 落盘 / 日志交错
}

func buildRegistry() *skill.Registry {
	reg := skill.NewRegistry()
	skills.RegisterAll(reg)
	return reg
}

// NewRun 创建新运行
func NewRun(wf *Workflow, client *ai.Client, cfg *config.Config, mem *selfheal.Memory, runsRoot string) (*Engine, error) {
	id := "run-" + time.Now().Format("20060102-150405") + "-" + randHex(4)
	dir := filepath.Join(runsRoot, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建运行目录失败: %w", err)
	}
	st := &RunState{
		ID: id, Workflow: wf.Name, Brief: wf.Brief,
		Mock: client.Mock(), Stages: map[string]*StageState{},
		UpdatedAt: time.Now(),
	}
	for _, s := range wf.Stages {
		st.Stages[s.ID] = &StageState{Status: StatusPending}
		st.StageOrder = append(st.StageOrder, s.ID)
	}
	return &Engine{Reg: buildRegistry(), Client: client, Memory: mem,
		State: st, Dir: dir, cfg: cfg, wf: wf, feedback: map[string]string{}}, nil
}

// Resume 恢复已有运行（断点续跑）。中断的 running、被跳过的 skipped、已失败的 failed
// 阶段都会重跑——resume 的语义是"处理完问题后重来"，doctor 修复后重跑失败/跳过阶段
// 正是主路径（失败提示与 doctor 的指引都指向 resume）。
func Resume(dir string, wf *Workflow, client *ai.Client, cfg *config.Config, mem *selfheal.Memory) (*Engine, error) {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("状态文件解析失败: %w", err)
	}
	for _, ss := range st.Stages {
		if ss.Status == StatusRunning || ss.Status == StatusSkipped || ss.Status == StatusFailed {
			ss.Status = StatusPending // 上次中断/被跳过/失败（已处理）的阶段重新执行
		}
	}
	// state.json 可能落后于工作流定义（工作流加了阶段、或状态文件来自旧版本）：
	// 不补种的话 readyStages 会对 nil 解引用 panic，断点续跑反而变成崩溃
	for _, s := range wf.Stages {
		if st.Stages == nil {
			st.Stages = map[string]*StageState{}
		}
		if st.Stages[s.ID] == nil {
			st.Stages[s.ID] = &StageState{Status: StatusPending}
		}
	}
	// 阶段清单以工作流为准向上修复：until 截断创建的 run 或旧版本 state 可能
	// 只记录了部分阶段，导致控制台"工作流阶段"面板看不到后面的阶段
	// （各阶段状态都在 map 里，这里只修顺序清单，状态不丢）
	if len(wf.Stages) > len(st.StageOrder) {
		st.StageOrder = st.StageOrder[:0]
		for _, s := range wf.Stages {
			st.StageOrder = append(st.StageOrder, s.ID)
		}
	}
	return &Engine{Reg: buildRegistry(), Client: client, Memory: mem,
		State: &st, Dir: dir, cfg: cfg, wf: wf, feedback: map[string]string{}}, nil
}

// RerunFrom 重跑某阶段及其下游（人工定点重做的统一入口，Web 流程图与 CLI --from 共用）。
//
// 三个集合的处理各不相同：
//   - stageID 本身：重置为待执行（Revisions 清零——手工重做给一次新的返修预算）
//   - 下游 × 本次运行窗口内（untilID 为空=全部；否则工作流顺序 ≤ untilID 的部分）：
//     重置为待执行，本次会话就会真的重跑
//   - 下游 × 窗口外（--until 截断掉的）：已完成的重置为 skipped 并注明
//     "上游已重跑、产物可能过期"——下次 resume 自动重做
//
// 为什么窗口外不保持 done：上游产物变了，下游产物就是过期的。保持 done 会让
// 控制台与审片流程把过期产物当新结果——这是最贵的谎：看起来一切完成，
// 成片却对不上刚改的上游。skipped 是引擎既有的"需要重做"语义（resume 时
// 自动翻回 pending），不需要新状态种类，级联规则也天然一致。
func (e *Engine) RerunFrom(stageID, untilID string) error {
	idxOf := func(id string) int {
		for i := range e.wf.Stages {
			if e.wf.Stages[i].ID == id {
				return i
			}
		}
		return -1
	}
	startIdx := idxOf(stageID)
	if startIdx < 0 {
		var ids []string
		for i := range e.wf.Stages {
			ids = append(ids, e.wf.Stages[i].ID)
		}
		return fmt.Errorf("阶段 %q 不在工作流中（可选: %s）", stageID, strings.Join(ids, ", "))
	}
	windowEnd := len(e.wf.Stages) - 1 // 默认：窗口到工作流末尾（不截断）
	if untilID != "" {
		if u := idxOf(untilID); u >= 0 {
			windowEnd = u
		}
	}

	// 下游闭包（含自身）：沿 depends_on 反向传播
	affected := map[string]bool{stageID: true}
	for changed := true; changed; {
		changed = false
		for i := range e.wf.Stages {
			s := &e.wf.Stages[i]
			if affected[s.ID] {
				continue
			}
			for _, d := range s.DependsOn {
				if affected[d] {
					affected[s.ID] = true
					changed = true
					break
				}
			}
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range affected {
		st := e.State.Stages[id]
		if st == nil {
			continue
		}
		idx := idxOf(id)
		if id == stageID {
			st.Status = StatusPending
			st.Revisions = 0
			st.Error = ""
			st.RepairNote = ""
			continue
		}
		if idx >= 0 && idx <= windowEnd {
			// 本次运行窗口内：直接排队重跑
			st.Status = StatusPending
			st.Error = ""
			st.RepairNote = ""
			continue
		}
		if st.Status == StatusDone {
			st.Status = StatusSkipped
			st.RepairNote = fmt.Sprintf("上游 %s 已重跑，本阶段产物可能过期（续跑将自动重做）", stageID)
		}
	}
	e.saveLocked()
	return nil
}

// ---- 执行 ----

// Run 执行整个工作流直到终态。
// 每一轮挑出"依赖已满足的全部 pending 阶段"并行执行（互不依赖的分支并行提速），
// 一轮结束后处理审片闭环与失败级联，直到没有可推进的阶段。
func (e *Engine) Run() error {
	fmt.Printf("▶ 工作流 [%s] 运行开始  run=%s  mode=%s\n", e.State.Workflow, e.State.ID, e.modeName())

	for {
		e.handleDoneReviews() // 上轮（或断点前）已完成的审片先结算，可能打回重做
		ready := e.readyStages()
		if len(ready) == 0 {
			break
		}
		if err := e.runWave(ready); err != nil {
			return err
		}
		e.cascadeSkips()
	}

	var done, failed, skipped, pending int
	for _, st := range e.State.Stages {
		switch st.Status {
		case StatusDone:
			done++
		case StatusFailed:
			failed++
		case StatusSkipped:
			skipped++
		case StatusPending:
			pending++
		}
	}
	e.save()
	fmt.Printf("\n■ 运行结束：%d 成功 / %d 失败 / %d 跳过   产物: %s\n", done, failed, skipped, e.Dir)
	if pending > 0 {
		fmt.Printf("  ⚠ %d 个阶段因上游失败未执行（已标记跳过可 resume 重跑）\n", pending)
	}
	if p, c := e.totalTokens(); p+c > 0 {
		fmt.Printf("  ¥ 本次运行 token 用量: prompt %d + completion %d = %d\n", p, c, p+c)
	}
	if failed > 0 {
		return fmt.Errorf("%d 个阶段失败，可用 `studio doctor` 分析或 `studio resume` 续跑", failed)
	}
	return nil
}

type stageRef struct {
	s  *Stage
	st *StageState
}

// readyStages 返回依赖已满足的全部 pending 阶段（同一波次可并行）
func (e *Engine) readyStages() []*stageRef {
	var out []*stageRef
	for i := range e.wf.Stages {
		s := &e.wf.Stages[i]
		st := e.State.Stages[s.ID]
		if st == nil {
			// 防御：状态缺失（旧 state.json / 手工编辑）按 pending 处理，而不是 panic
			st = &StageState{Status: StatusPending}
			e.State.Stages[s.ID] = st
		}
		if st.Status == StatusPending && e.depsDone(s) {
			out = append(out, &stageRef{s, st})
		}
	}
	return out
}

// runWave 执行一波阶段：单个则直接跑，多个则并行。
// 返回首个不可恢复错误（未注册技能/产物写盘失败），其余阶段照常跑完。
func (e *Engine) runWave(wave []*stageRef) error {
	if len(wave) == 1 {
		if err := e.runStage(wave[0].s, wave[0].st); err != nil {
			return err
		}
		return nil
	}
	if len(wave) > 1 {
		ids := make([]string, 0, len(wave))
		for _, r := range wave {
			ids = append(ids, r.s.ID)
		}
		fmt.Printf("⚡ 并行波次: %s\n", strings.Join(ids, " + "))
	}
	errCh := make(chan error, len(wave))
	var wg sync.WaitGroup
	for _, r := range wave {
		wg.Add(1)
		go func(s *Stage, st *StageState) {
			defer wg.Done()
			errCh <- e.runStage(s, st)
		}(r.s, r.st)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// handleDoneReviews 结算所有已完成但未处理的审片（打回重做）。
// 仅当返修区间内没有正在执行的阶段时才结算，避免并发改写运行中阶段的状态。
func (e *Engine) handleDoneReviews() {
	for i := range e.wf.Stages {
		s := &e.wf.Stages[i]
		if s.Skill != "review" {
			continue
		}
		st := e.State.Stages[s.ID]
		if st.Status != StatusDone {
			continue
		}
		if e.rangeHasRunning(s) {
			continue
		}
		if err := e.handleReview(s, st); err != nil {
			fmt.Printf("⚠ 审片结算失败: %v\n", err)
		}
	}
}

func (e *Engine) rangeHasRunning(review *Stage) bool {
	startIdx, reviewIdx, ok := e.revisionRange(review)
	if !ok {
		return false
	}
	for i := startIdx; i <= reviewIdx; i++ {
		if rs := e.State.Stages[e.wf.Stages[i].ID]; rs != nil && rs.Status == StatusRunning {
			return true
		}
	}
	return false
}

func (e *Engine) modeName() string {
	if e.Client.Mock() {
		return "mock（离线演示）"
	}
	return "real（模型: " + e.Client.ModelName() + "）"
}

func (e *Engine) depsDone(s *Stage) bool {
	for _, d := range s.DependsOn {
		ds := e.State.Stages[d]
		if ds == nil || (ds.Status != StatusDone && ds.Status != StatusSkipped) {
			return false
		}
	}
	return true
}

// cascadeSkips 上游失败/跳过的下游阶段全部标记 skipped（带原因），防止带病运行
func (e *Engine) cascadeSkips() {
	changed := true
	for changed {
		changed = false
		for i := range e.wf.Stages {
			s := &e.wf.Stages[i]
			st := e.State.Stages[s.ID]
			if st.Status != StatusPending {
				continue
			}
			var broken []string
			for _, d := range s.DependsOn {
				ds := e.State.Stages[d]
				if ds != nil && (ds.Status == StatusFailed || ds.Status == StatusSkipped) {
					broken = append(broken, d)
				}
			}
			if len(broken) > 0 {
				st.Status = StatusSkipped
				st.Error = "上游阶段未产出: " + strings.Join(broken, "、")
				st.RepairNote = "处理上游问题后用 `studio resume` 重跑（resume 会重置跳过状态）"
				changed = true
			}
		}
	}
	if changed {
		e.save()
	}
}

// runStage 执行单个阶段：重试 → 规则预过滤/AI 诊断 → 带修复提示重试 → 产物落盘
// 返回 error 仅限不可恢复的基础设施错误（未注册技能、产物写盘失败），业务失败记入状态由引擎继续。
func (e *Engine) runStage(s *Stage, st *StageState) error {
	e.mu.Lock()
	st.Status = StatusRunning
	st.Error = ""
	e.saveLocked()
	e.mu.Unlock()
	start := time.Now()
	e.logf("\n[%s] %s (skill=%s)\n", s.ID, s.Describe(), s.Skill)

	sk, ok := e.Reg.Get(s.Skill)
	if !ok {
		e.mu.Lock()
		st.Status = StatusFailed
		st.Error = fmt.Sprintf("未注册的技能: %s", s.Skill)
		e.saveLocked()
		e.mu.Unlock()
		return fmt.Errorf("%s", st.Error)
	}

	retries := s.Retries
	if retries < 1 {
		retries = 1
	}
	total := retries + 1 // 总尝试次数

	var lastErr error
	for attempt := 1; attempt <= total; attempt++ {
		// 并行波次里其他阶段可能在锁内序列化整个 State（saveLocked），
		// st 的任何字段写入都必须持锁，否则是 -race 可检出的数据竞争
		e.mu.Lock()
		st.Attempts = attempt
		e.mu.Unlock()
		out, err := func() (any, error) {
			e.mu.Lock()
			inject := e.DrillFault && attempt == 1
			if inject {
				e.DrillFault = false // 只注入一次
			}
			e.mu.Unlock()
			if inject {
				e.logf("  ⚡ 故障演练：注入模拟错误（输出非法 JSON）")
				return nil, fmt.Errorf("输出 JSON 解析失败: unexpected end of JSON input")
			}
			return e.execSkill(sk, s, st)
		}()
		if err == nil {
			return e.finishStage(s, st, out, start)
		}
		lastErr = err
		e.logf("  ✗ 尝试 %d/%d 失败: %v", attempt, total, err)

		if attempt < total {
			var plan selfheal.RepairAction
			if act, hit := prefilter(err.Error()); hit {
				// 可规则判定的错误不需要花一次 LLM 诊断（诊断本身也是钱）
				plan = act
				e.logf("  ⚙ 规则命中: %s", plan.RootCause)
			} else {
				e.logf("  ⚙ 自我修复：分析失败原因…")
				plan = selfheal.Diagnose(e.Client, selfheal.DiagnoseInput{
					SkillName: s.Skill, Attempts: attempt, Error: err.Error(),
					Lessons: e.Memory.RecallFor(s.Skill, err.Error(), 5),
				})
				e.logf("  ⚙ 根因: %s\n  ⚙ 动作: %s %s", plan.RootCause, plan.Action, plan.Hint)
			}
			switch plan.Action {
			case "abort":
				goto exhausted
			default: // retry
				// 严格模式：失败阶段只有"修复重试"或"中止"两条路，
				// 不存在被自我修复机制静默跳过的阶段
				if wait := backoffFor(err.Error(), attempt); wait > 0 {
					e.logf("  ⏳ 退避 %v 后重试", wait)
					time.Sleep(wait)
				}
				e.mu.Lock()
				st.RepairNote = plan.Hint
				e.mu.Unlock()
				_ = e.Memory.Add(s.Skill, selfheal.Signature(err.Error()), "失败后有效的修复提示: "+plan.Hint, "selfheal")
				e.save()
			}
		}
	}

exhausted:
	// 重试耗尽：沉淀教训，标记失败（引擎继续跑其余阶段，最后汇总）
	e.mu.Lock()
	st.Status = StatusFailed
	st.Error = lastErr.Error()
	st.DurationMS = time.Since(start).Milliseconds()
	e.saveLocked()
	e.mu.Unlock()
	_ = e.Memory.Add(s.Skill, selfheal.Signature(lastErr.Error()), "最终失败模式: "+truncate(lastErr.Error(), 200), "selfheal")
	return nil
}

// prefilter 规则预过滤：鉴权/余额类错误直接 abort，限流/网络类直接退避重试，
// 都不必调用 LLM 诊断——确定性错误交给确定性规则，省钱省时。
func prefilter(errStr string) (selfheal.RepairAction, bool) {
	e := strings.ToLower(errStr)
	switch {
	case containsAny(e, "401", "403", "unauthorized", "invalid api key", "api key", "鉴权", "密钥"):
		return selfheal.RepairAction{
			Action:    "abort",
			RootCause: "鉴权失败（规则命中）：API Key 无效或缺失，检查 AI_API_KEY 后用 resume 续跑",
		}, true
	case containsAny(e, "1113", "余额", "欠费", "balance", "quota exceeded"):
		return selfheal.RepairAction{
			Action:    "abort",
			RootCause: "API 余额/配额不足（规则命中）：充值后用 resume 续跑，断点产物不丢失",
		}, true
	case containsAny(e, "429", "rate limit", "too many requests", "限流", "请求过快"):
		return selfheal.RepairAction{
			Action: "retry",
			Hint:   "触发限流，退避后原样重试即可",
		}, true
	case containsAny(e, "timeout", "connection refused", "connection reset", "broken pipe", "eof", "网络"):
		return selfheal.RepairAction{
			Action:    "retry",
			Hint:      "网络波动导致的失败，直接原样重试",
			RootCause: "网络层错误（规则命中）",
		}, true
	}
	return selfheal.RepairAction{}, false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// backoffFor 退避时长：限流类长退避（给服务端窗口冷却），其余线性短退避
func backoffFor(errStr string, attempt int) time.Duration {
	if containsAny(strings.ToLower(errStr), "429", "rate limit", "限流") {
		return 30 * time.Second
	}
	d := time.Duration(attempt) * 2 * time.Second
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// execSkill 组装上下文并执行技能
func (e *Engine) execSkill(sk skill.Skill, s *Stage, st *StageState) (any, error) {
	params := map[string]any{}
	for k, v := range s.Params {
		params[k] = v
	}
	params["revision"] = st.Revisions
	if fb := e.feedbackFor(s.ID, st); fb != "" {
		params["revision_feedback"] = fb
	}
	e.mu.Lock()
	rh := st.RepairNote
	e.mu.Unlock()
	if rh != "" {
		params["repair_hint"] = rh
	}

	deps := map[string]json.RawMessage{}
	for _, d := range s.DependsOn {
		raw, err := e.loadArtifact(d)
		if err != nil {
			return nil, fmt.Errorf("加载依赖 %s 失败: %w", d, err)
		}
		deps[d] = raw
	}

	ctx := &skill.Context{
		RunID:   e.State.ID,
		WorkDir: e.Dir,
		Brief:   e.State.Brief,
		Params:  params,
		Deps:    deps,
		AI:      e.Client,
		Cfg:     e.cfg,
		Lessons: e.Memory.Recall(sk.Name(), 5),
		Log:     e.logf,
	}
	return sk.Run(ctx)
}

// feedbackFor 返修意见只注入被定向打回的区间（handleReview 负责登记）
func (e *Engine) feedbackFor(stageID string, st *StageState) string {
	if st.Revisions <= 0 {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.feedback[stageID]
}

// finishStage 保存成功阶段的产物
func (e *Engine) finishStage(s *Stage, st *StageState, out any, start time.Time) error {
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	artDir := filepath.Join(e.Dir, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return err
	}
	f := filepath.Join("artifacts", s.ID+".json")
	if err := fsx.AtomicWrite(filepath.Join(e.Dir, f), data, 0o644); err != nil {
		return err
	}
	e.mu.Lock()
	st.Status = StatusDone
	st.OutputFile = f
	st.Error = ""
	st.RepairNote = ""
	st.DurationMS = time.Since(start).Milliseconds()
	e.saveLocked()
	e.mu.Unlock()
	e.logf("  ✓ 完成 (%.1fs) → %s\n", time.Since(start).Seconds(), f)
	return nil
}

// revisionRange 计算审片返修区间 [startIdx, reviewIdx]
func (e *Engine) revisionRange(review *Stage) (startIdx, reviewIdx int, ok bool) {
	target := "storyboard"
	if t, ok := review.Params["target"].(string); ok && t != "" {
		target = t
	}
	startIdx, reviewIdx = -1, -1
	for i := range e.wf.Stages {
		switch e.wf.Stages[i].ID {
		case target:
			startIdx = i
		case review.ID:
			reviewIdx = i
		}
	}
	if startIdx < 0 || reviewIdx < 0 || startIdx >= reviewIdx {
		return -1, -1, false
	}
	return startIdx, reviewIdx, true
}

// handleReview 审片不达标 → 定向打回重做。
// 打回起点取"返修意见点名的最早阶段"与配置 target 中更靠前的一个：
// 意见只涉及 prompts 时不再重烧 script/characters/storyboard——每轮返修省下无关阶段的 LLM 调用。
func (e *Engine) handleReview(s *Stage, st *StageState) error {
	var rev reviewArtifact
	raw, err := e.loadArtifact(s.ID)
	if err != nil {
		return err
	}
	// 审片产物不可信（解析失败 / 空洞无内容）时重跑审片本身，
	// 而不是把零值 {Score:0,Pass:false} 当作"严重不达标"烧光返修轮数——
	// 真实的不达标审片必然带分数与意见，两者皆缺说明产物损坏或模型漏答
	if err := json.Unmarshal(raw, &rev); err != nil || isEmptyReview(rev) {
		reason := "审片产物损坏"
		if err == nil {
			reason = "审片产物为空（无分数无意见）"
		}
		e.mu.Lock()
		st.Status = StatusPending
		st.Error = reason + "，重跑审片"
		e.saveLocked()
		e.mu.Unlock()
		fmt.Printf("  ⚠ %s，将重跑审片阶段（不计返修轮数）\n", reason)
		return nil
	}

	maxRev := s.MaxRevisions
	if maxRev <= 0 {
		maxRev = 1
	}
	if rev.Pass || st.Revisions >= maxRev {
		if !rev.Pass {
			fmt.Printf("  ⚠ 审片仍不达标（%.1f 分），已达返修上限，记录教训后放行\n", rev.Score)
			_ = e.Memory.Add(s.Skill, "review-below-threshold",
				"多次返修后仍低于阈值：常见于 brief 过于宏大或镜头数超出模型能力，建议拆分场景分集制作", "review")
		}
		return nil
	}

	startIdx, reviewIdx, ok := e.revisionRange(s)
	if !ok {
		return fmt.Errorf("审片返修目标区间无效")
	}
	// 定向：返修意见点名的最早阶段若在 target 之前，扩大打回范围到它
	stageIdx := map[string]int{}
	for i := range e.wf.Stages {
		stageIdx[e.wf.Stages[i].ID] = i
	}
	for _, iss := range rev.Issues {
		if idx, exists := stageIdx[iss.Stage]; exists && idx >= 0 && idx < startIdx {
			startIdx = idx
		}
	}

	// 状态与返修意见的写入全部持锁：handleDoneReviews 虽在主循环执行，
	// 但不锁住的话未来任何并发重构都会变成 -race 可检出的数据竞争
	e.mu.Lock()
	st.Revisions++
	for i := startIdx; i <= reviewIdx; i++ { // 包含 review 自身：返修后必须重新审片
		id := e.wf.Stages[i].ID
		rs := e.State.Stages[id]
		rs.Status = StatusPending
		rs.Revisions = st.Revisions
		e.feedback[id] = rev.Feedback // 返修意见注入重做阶段
	}
	e.saveLocked()
	e.mu.Unlock()
	// 返修意见沉淀为经验教训（自我成长）
	_ = e.Memory.Add(s.Skill, "review-revision",
		"制片人返修意见: "+truncate(rev.Feedback, 150), "review")
	fmt.Printf("  ↩ 审片不达标（%.1f 分），定向打回 [%s…] 重做（第 %d 次返修）：%s\n",
		rev.Score, e.wf.Stages[startIdx].ID, st.Revisions, truncate(rev.Feedback, 120))
	return nil
}

func (e *Engine) loadArtifact(stageID string) (json.RawMessage, error) {
	st := e.State.Stages[stageID]
	if st == nil || st.OutputFile == "" {
		return nil, fmt.Errorf("阶段 %s 尚无产物", stageID)
	}
	return os.ReadFile(filepath.Join(e.Dir, st.OutputFile))
}

// reviewArtifact 审片产物中引擎关心的字段
type reviewArtifact struct {
	Score    float64 `json:"score"`
	Pass     bool    `json:"pass"`
	Feedback string  `json:"feedback"`
	Issues   []struct {
		Stage string `json:"stage"`
	} `json:"issues"`
}

// isEmptyReview 反序列化成功但内容空洞：真实的不达标审片必然带分数与意见，
// 两者皆缺说明模型漏答或产物被截断，不能当作"0 分不达标"触发返修
func isEmptyReview(rev reviewArtifact) bool {
	return !rev.Pass && rev.Score == 0 && rev.Feedback == "" && len(rev.Issues) == 0
}

// save 持久化运行状态（自带加锁，可在任意 goroutine 调用）
func (e *Engine) save() {
	e.mu.Lock()
	e.saveLocked()
	e.mu.Unlock()
}

// saveLocked 持久化运行状态（调用方必须已持有 e.mu）。
// state.json 是断点续跑与已付费产物的账本，写坏即全部作废，绝不允许半截文件。
func (e *Engine) saveLocked() {
	e.State.UpdatedAt = time.Now()
	if u := e.Client.UsageSnapshot(); len(u) > 0 {
		e.State.Usage = u
	}
	data, err := json.MarshalIndent(e.State, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 状态序列化失败: %v\n", err)
		return
	}
	if err := fsx.AtomicWrite(filepath.Join(e.Dir, "state.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 状态保存失败（断点续跑可能受影响）: %v\n", err)
	}
}

// logf 并发安全的控制台日志
func (e *Engine) logf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Printf("  "+format+"\n", args...)
}

func (e *Engine) totalTokens() (prompt, completion int64) {
	for _, st := range e.State.Usage {
		prompt += st.PromptTokens
		completion += st.CompletionTokens
	}
	return
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
