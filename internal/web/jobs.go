// Package web 提供 studio 的 Web 控制台：REST API + 内嵌单页前端。
//
// 设计原则：Web 层不重新实现流水线，而是把既有 CLI 子命令作为唯一执行入口
// （受控子进程）。这样运行锁（.lock）、断点续跑、预算熔断、原子写盘等
// CLI 已有的安全机制对 Web 发起的任务原样生效——Web 只是另一只手，
// 不是第二条流水线。
package web

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Job 任务：一次受控的 CLI 子进程执行。
type Job struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`             // run/resume/render/animatic/deliver/polish/takes/verify/doctor/probe-ref
	RunID     string         `json:"run_id,omitempty"` // 关联的 run（run 类型任务完成后回填新建的 run）
	Label     string         `json:"label"`            // 人读的一句话描述
	Params    map[string]any `json:"params,omitempty"` // 原始请求参数（回显用）
	Status    string         `json:"status"`           // running/done/failed/canceled/interrupted
	Pid       int            `json:"pid,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
	ExitCode  int            `json:"exit_code,omitempty"`
	LogFile   string         `json:"log_file"` // 相对 root 的日志路径
}

// JobStatus 任务运行态
const (
	StatusRunning     = "running"
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusCanceled    = "canceled"
	StatusInterrupted = "interrupted" // 服务重启时任务还标着 running → 进程已随服务死亡
)

var jobTypes = map[string]bool{
	"run": true, "resume": true, "render": true, "animatic": true,
	"deliver": true, "polish": true, "takes": true, "verify": true,
	"doctor": true, "probe-ref": true, "assets": true, "dub": true,
}

// JobManager 管理任务的注册、执行、取消与持久化。
type JobManager struct {
	mu       sync.Mutex
	root     string
	studioEx string
	jobs     map[string]*Job
	order    []string // 按 ID（含时间戳）天然有序
	waiters  map[string]*exec.Cmd
}

// NewJobManager 构造任务管理器并加载历史索引。
// 上次服务退出时仍标为 running 的任务统一改为 interrupted——
// 子进程随服务死亡，"还在跑"绝不能表现为"在跑"。
func NewJobManager(root string) (*JobManager, error) {
	m := &JobManager{
		root:    root,
		jobs:    map[string]*Job{},
		waiters: map[string]*exec.Cmd{},
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("定位 studio 可执行文件失败: %w", err)
	}
	m.studioEx = exe
	if err := os.MkdirAll(m.jobDir(), 0o755); err != nil {
		return nil, err
	}
	m.loadIndex()
	return m, nil
}

func (m *JobManager) jobDir() string    { return filepath.Join(m.root, ".web", "jobs") }
func (m *JobManager) indexPath() string { return filepath.Join(m.jobDir(), "index.json") }

func (m *JobManager) loadIndex() {
	data, err := os.ReadFile(m.indexPath())
	if err != nil {
		return
	}
	var list []*Job
	if json.Unmarshal(data, &list) != nil {
		return
	}
	for _, j := range list {
		if j.Status == StatusRunning {
			j.Status = StatusInterrupted
			now := time.Now()
			j.EndedAt = &now
		}
		m.jobs[j.ID] = j
		m.order = append(m.order, j.ID)
	}
}

func (m *JobManager) saveIndexLocked() {
	list := make([]*Job, 0, len(m.order))
	for _, id := range m.order {
		list = append(list, m.jobs[id])
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := m.indexPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, m.indexPath())
	}
}

// Start 创建并启动一个任务。同一 run 同时只允许一个任务——
// CLI 的运行锁会兜底，但在这里提前拒绝能给出更友好的报错。
func (m *JobManager) Start(jobType, runID string, params map[string]any) (*Job, error) {
	if !jobTypes[jobType] {
		return nil, fmt.Errorf("未知任务类型 %q", jobType)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if runID != "" {
		for _, j := range m.jobs {
			if j.Status == StatusRunning && j.RunID == runID {
				return nil, fmt.Errorf("run %s 已有任务在执行中（%s），请等待完成或先取消", runID, j.Type)
			}
		}
	}
	running := 0
	for _, j := range m.jobs {
		if j.Status == StatusRunning {
			running++
		}
	}
	if running >= 2 {
		return nil, fmt.Errorf("已有 %d 个任务在执行，请等待完成后再提交（重活串行更安全）", running)
	}

	args, label, err := m.buildArgs(jobType, runID, params)
	if err != nil {
		return nil, err
	}

	j := &Job{
		ID:        newJobID(jobType),
		Type:      jobType,
		RunID:     runID,
		Label:     label,
		Params:    params,
		Status:    StatusRunning,
		StartedAt: time.Now(),
	}
	j.LogFile = filepath.ToSlash(filepath.Join(".web", "jobs", j.ID+".log"))
	m.jobs[j.ID] = j
	m.order = append(m.order, j.ID)

	cmd := exec.Command(m.studioEx, args...)
	cmd.Dir = m.root
	cmd.Env = os.Environ()
	logF, err := os.Create(filepath.Join(m.jobDir(), j.ID+".log"))
	if err != nil {
		delete(m.jobs, j.ID)
		m.order = m.order[:len(m.order)-1]
		return nil, fmt.Errorf("创建任务日志失败: %w", err)
	}
	cmd.Stdout = logF
	cmd.Stderr = logF
	applySysProcAttr(cmd) // Unix 下独立进程组，便于整组终止
	if err := cmd.Start(); err != nil {
		logF.Close()
		delete(m.jobs, j.ID)
		m.order = m.order[:len(m.order)-1]
		return nil, fmt.Errorf("启动任务进程失败: %w", err)
	}
	j.Pid = cmd.Process.Pid
	m.waiters[j.ID] = cmd
	m.saveIndexLocked()

	go func() {
		err := cmd.Wait()
		logF.Close()
		m.finish(j, err)
	}()
	return j, nil
}

// finish 记录任务结束状态；run 类型任务回填新建的 run 目录。
func (m *JobManager) finish(j *Job, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	j.EndedAt = &now
	switch {
	case err == nil:
		j.Status = StatusDone
		j.ExitCode = 0
	case j.Status == StatusCanceled:
		// 状态已由 Cancel 写好
	default:
		j.Status = StatusFailed
		if ee, ok := err.(*exec.ExitError); ok {
			j.ExitCode = ee.ExitCode()
		} else {
			j.ExitCode = -1
		}
	}
	if j.Type == "run" && j.Status == StatusDone && j.RunID == "" {
		if id := newestRunUnder(m.root, j.StartedAt); id != "" {
			j.RunID = id
		}
	}
	delete(m.waiters, j.ID)
	m.saveIndexLocked()
}

// Cancel 终止任务进程树。Windows 用 taskkill /T /F（连带子进程 ffmpeg 等）。
func (m *JobManager) Cancel(id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("任务 %s 不存在", id)
	}
	if j.Status != StatusRunning {
		m.mu.Unlock()
		return fmt.Errorf("任务 %s 已结束（%s），无需取消", id, j.Status)
	}
	j.Status = StatusCanceled
	cmd := m.waiters[id]
	m.saveIndexLocked()
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		killProcessTree(cmd.Process.Pid)
	}
	return nil
}

// Get 返回任务详情。
func (m *JobManager) Get(id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("任务 %s 不存在", id)
	}
	return j, nil
}

// List 返回任务列表（新的在前），可按 run 过滤、限量。
func (m *JobManager) List(runID string, limit int) []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		j := m.jobs[m.order[i]]
		if runID != "" && j.RunID != runID {
			continue
		}
		out = append(out, j)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// TailLog 读取任务日志的最后 n 行（n<=0 读全部）。
func (m *JobManager) TailLog(id string, n int) (string, error) {
	if _, err := m.Get(id); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(m.jobDir(), id+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if n <= 0 {
		return text, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// buildArgs 把任务参数翻译成 CLI 参数。必须与 cmd/studio 各子命令的
// flag 定义保持一致——这是 Web 与流水线之间唯一的契约。
func (m *JobManager) buildArgs(jobType, runID string, p map[string]any) ([]string, string, error) {
	get := func(key string) any { return p[key] }
	boolP := func(key string) bool { b, _ := get(key).(bool); return b }
	intP := func(key string) int {
		switch v := get(key).(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	strP := func(key string) string { s, _ := get(key).(string); return strings.TrimSpace(s) }
	floatP := func(key string) float64 { f, _ := get(key).(float64); return f }

	// dub 是 UI 语义（"只管声音的 deliver"），CLI 里没有 dub 子命令——
	// 直接把 jobType 当子命令拼会让子进程打印用法后退出（历史上首次点击
	// 一键配音即失败的原因）。映射到 deliver，配音旗标在下方 case 里补。
	cliSub := jobType
	if jobType == "dub" {
		cliSub = "deliver"
	}
	args := []string{cliSub}
	add := func(kv ...string) { args = append(args, kv...) }
	addIf := func(cond bool, kv ...string) {
		if cond {
			args = append(args, kv...)
		}
	}
	// runPath 把 run ID 解析为 CLI 可用的 --run 参数（绝对路径，不依赖子进程 cwd）
	runPath := func() string { return filepath.Join(m.root, "runs", runID) }

	switch jobType {
	case "run":
		brief := strP("brief")
		if brief == "" {
			return nil, "", fmt.Errorf("缺少创作意图（brief）")
		}
		add("--brief", brief)
		if w := strP("workflow"); w != "" {
			add("--workflow", w)
		}
		if u := strP("until"); u != "" {
			add("--until", u)
		}
		if f := strP("from"); f != "" {
			add("--from", f)
		}
		add("--config", configArg(p))
		label := "创作流水线：" + truncate(brief, 40)
		if u := strP("until"); u != "" {
			label += fmt.Sprintf("（跑到 %s 后停）", u)
		}
		return args, label, nil

	case "resume":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		// until：智能"生成素材"按钮用——缺 characters 时先续跑到 assets 后停，
		// 已完成阶段自动跳过，不会重复花钱
		addIf(strP("until") != "", "--until", strP("until"))
		// from：流程图上"重跑该阶段"用——该阶段与已完成的下游重置为待执行；
		// from==until 即"只重跑这一个阶段"
		addIf(strP("from") != "", "--from", strP("from"))
		add("--config", configArg(p))
		label := "断点续跑 " + runID
		if u := strP("until"); u != "" {
			label = fmt.Sprintf("续跑到 %s 后停 %s", u, runID)
		}
		if f := strP("from"); f != "" {
			if f == strP("until") {
				label = fmt.Sprintf("重跑 %s %s", f, runID)
			} else {
				label = fmt.Sprintf("重跑 %s 起 %s", f, runID)
			}
		}
		return args, label, nil

	case "status":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		add("--config", configArg(p))
		return args, "断点续跑 " + runID, nil

	case "render":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		addIf(boolP("dry_run"), "--dry-run")
		addIf(boolP("images_only"), "--images-only")
		addIf(boolP("force_ref"), "--force-ref")
		addIf(boolP("allow_incomplete"), "--allow-incomplete")
		if v := intP("max_shots"); v > 0 {
			add("--max-shots", fmt.Sprint(v))
		}
		if v := strP("shots"); v != "" {
			add("--shots", v)
		}
		if v := intP("workers"); v > 0 {
			add("--workers", fmt.Sprint(v))
		}
		if v := strP("quality"); v != "" {
			add("--quality", v)
		}
		if v := intP("timeout_minutes"); v > 0 {
			add("--timeout-minutes", fmt.Sprint(v))
		}
		add("--config", configArg(p))
		label := "渲染 " + runID
		switch {
		case boolP("dry_run"):
			label = "成本预估（dry-run） " + runID
		case boolP("images_only"):
			label = "仅出首帧 " + runID
		case strP("shots") != "":
			label = "定点重渲 " + strP("shots")
		}
		return args, label, nil

	case "animatic":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		add("--config", configArg(p))
		return args, "拼装动态分镜 " + runID, nil

	case "dub":
		// 一键配音：deliver 的声音语义快捷方式——TTS + 口型对齐 + 音效/配乐
		// 生成 + 混音 + 字幕 + 成片。残缺 run 只配已渲染镜头（报告如实标注），
		// 视觉复核不在配音链路里（打磨闭环/交付页有独立入口）。
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		add("--allow-incomplete")
		add("--no-visual")
		addIf(boolP("force_dub"), "--force-dub")
		add("--config", configArg(p))
		return args, "一键配音 " + runID, nil

	case "deliver":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		addIf(boolP("allow_incomplete"), "--allow-incomplete")
		addIf(boolP("no_burn"), "--no-burn")
		addIf(boolP("skip_audio"), "--skip-audio")
		addIf(boolP("force_dub"), "--force-dub")
		addIf(boolP("visual"), "--visual")
		addIf(boolP("no_visual"), "--no-visual")
		if v := intP("workers"); v > 0 {
			add("--workers", fmt.Sprint(v))
		}
		add("--config", configArg(p))
		return args, "交付 " + runID, nil

	case "polish":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		if v := intP("rounds"); v > 0 {
			add("--rounds", fmt.Sprint(v))
		}
		if v := floatP("pass_score"); v > 0 {
			add("--pass-score", fmt.Sprintf("%.2f", v))
		}
		addIf(boolP("no_burn"), "--no-burn")
		if v := intP("workers"); v > 0 {
			add("--workers", fmt.Sprint(v))
		}
		add("--config", configArg(p))
		return args, "打磨闭环 " + runID, nil

	case "takes":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		shots := strP("shots")
		if shots == "" {
			return nil, "", fmt.Errorf("takes 需要指定镜头（shots，如 sh003）")
		}
		add("--run", runPath(), "--shots", shots)
		if v := strP("pick"); v != "" {
			add("--pick", v)
		}
		add("--config", configArg(p))
		return args, "多 take 对比 " + shots, nil

	case "verify":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		addIf(boolP("render"), "--render")
		add("--config", configArg(p))
		return args, "复核交付报告 " + runID, nil

	case "doctor":
		if runID != "" {
			add("--run", runPath())
		}
		add("--config", configArg(p))
		return args, "环境自检", nil

	case "probe-ref":
		add("--config", configArg(p))
		if v := strP("out"); v != "" {
			add("--out", v)
		}
		return args, "参考图生效性探针", nil

	case "assets":
		if runID == "" {
			return nil, "", fmt.Errorf("缺少 run")
		}
		add("--run", runPath())
		addIf(boolP("force"), "--force")
		addIf(boolP("no_chars"), "--no-chars")
		addIf(boolP("no_scenes"), "--no-scenes")
		if v := strP("chars"); v != "" {
			add("--chars", v)
		}
		if v := strP("scenes"); v != "" {
			add("--scenes", v)
		}
		// 人工提示词覆盖（编辑弹窗回传）：落盘到 run 目录，CLI --prompt-file 读取
		if po, ok := p["prompt_override"].(map[string]any); ok {
			path := filepath.Join(runPath(), ".prompt_override.json")
			if data, merr := json.Marshal(po); merr == nil && os.WriteFile(path, data, 0o644) == nil {
				add("--prompt-file", path)
			}
		}
		add("--config", configArg(p))
		label := "生成素材库 " + runID
		switch {
		case strP("chars") != "":
			label = "角色素材重出 " + strP("chars")
		case strP("scenes") != "":
			label = "场景素材重出 " + strP("scenes")
		case boolP("force"):
			label = "素材库强制重出 " + runID
		}
		return args, label, nil
	}
	return nil, "", fmt.Errorf("未知任务类型 %q", jobType)
}

// configArg 允许任务级覆盖配置文件（默认 config.json）。
func configArg(p map[string]any) string {
	if s, ok := p["config"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return "config.json"
}

func newJobID(jobType string) string {
	return fmt.Sprintf("%s-%s", jobType, time.Now().Format("20060102-150405.000"))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// newestRunUnder 找 runs/ 下最近更新的 run 目录（run 任务完成后回填用）。
func newestRunUnder(root string, after time.Time) string {
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err != nil {
		return ""
	}
	type cand struct {
		id string
		t  time.Time
	}
	var best *cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		if st.ModTime().Before(after) {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "runs", e.Name(), "state.json")); err != nil {
			continue
		}
		if best == nil || st.ModTime().After(best.t) {
			best = &cand{id: e.Name(), t: st.ModTime()}
		}
	}
	if best == nil {
		return ""
	}
	return best.id
}
