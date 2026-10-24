package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aistudio/internal/config"
	"aistudio/internal/deliver"
	"aistudio/internal/engine"
	"aistudio/internal/ffx"
	"aistudio/internal/promspec"
	"aistudio/internal/render"
	"aistudio/internal/selfheal"
	"aistudio/internal/shotkit"
)

//go:embed webui
var uiFS embed.FS

// Server Web 控制台服务。
type Server struct {
	root string
	jobs *JobManager
	mux  *http.ServeMux

	capsMu   sync.Mutex
	caps     *ffx.Capabilities
	capsOnce sync.Once
}

// NewServer 构造服务（root = 工作区根目录，runs/、config.json 等都锚定于此）。
func NewServer(root string) (*Server, error) {
	jm, err := NewJobManager(root)
	if err != nil {
		return nil, err
	}
	s := &Server{root: root, jobs: jm}
	s.buildRoutes()
	return s, nil
}

func (s *Server) buildRoutes() {
	mux := http.NewServeMux()

	// ---- API ----
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/runs/{id}/delete", s.handleRunDelete)
	mux.HandleFunc("POST /api/runs/{id}/restore", s.handleRunRestore)
	mux.HandleFunc("DELETE /api/runs/{id}", s.handleRunPurge)
	mux.HandleFunc("GET /api/runs/{id}/artifacts", s.handleArtifactList)
	mux.HandleFunc("GET /api/runs/{id}/artifacts/{name}", s.handleArtifact)
	mux.HandleFunc("GET /api/runs/{id}/assets", s.handleRunAssets)
	mux.HandleFunc("GET /api/runs/{id}/ref-prompt", s.handleRefPrompt)
	mux.HandleFunc("GET /api/runs/{id}/file/{path...}", s.handleRunFile)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/jobs", s.handleJobCreate)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /api/jobs/{id}/log", s.handleJobLog)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.handleJobCancel)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/ai-calls", s.handleAICalls)
	mux.HandleFunc("GET /api/ai-calls/{name}", s.handleAICall)
	mux.HandleFunc("GET /api/ai-calls/{name}/artifact", s.handleAICallArtifact)
	mux.HandleFunc("GET /api/ai-calls/{name}/reqimg", s.handleAICallReqImg)

	// ---- 内嵌前端 ----
	sub, _ := fs.Sub(uiFS, "webui")
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA 兜底：无扩展名的路径都回 index.html，由前端路由接管
		if r.URL.Path != "/" && strings.Contains(path.Base(r.URL.Path), ".") {
			fileServer.ServeHTTP(w, r)
			return
		}
		index, err := uiFS.ReadFile("webui/index.html")
		if err != nil {
			http.Error(w, "前端资源缺失", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
	s.mux = mux
}

// Handler 返回根 handler。
func (s *Server) Handler() http.Handler { return s.mux }

// ---- 通用工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// loadConfigSafe 载入配置供展示；失败不致命（系统页显示错误）。
func (s *Server) loadConfigSafe() (*config.Config, error) {
	cfg, err := config.Load(filepath.Join(s.root, "config.json"))
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("配置自检未通过: %w", err)
	}
	return cfg, nil
}

func (s *Server) detectCaps() *ffx.Capabilities {
	s.capsOnce.Do(func() {
		s.capsMu.Lock()
		defer s.capsMu.Unlock()
		s.caps = ffx.Detect()
	})
	return s.caps
}

// ---- 数据模型 ----

type fileInfo struct {
	Path    string    `json:"path"` // 相对 run 目录（/ 分隔）
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Kind    string    `json:"kind"` // video/image/audio/doc/other
}

func kindOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".mkv", ".webm":
		return "video"
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return "image"
	case ".wav", ".mp3", ".m4a", ".aac", ".flac", ".ogg":
		return "audio"
	case ".json", ".srt", ".txt", ".md", ".sh", ".ps1", ".csv":
		return "doc"
	}
	return "other"
}

type stageView struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	Revisions  int    `json:"revisions"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	RepairNote string `json:"repair_note,omitempty"`
	OutputFile string `json:"output_file,omitempty"`
}

// workflowNode 流程图视图的一个节点（阶段定义侧，不含运行状态）。
// 状态由 state.json 提供、前端按 id 合并——两边的版本可能不一致（工作流改过），
// 这里只给出依赖形状与展示名。
type workflowNode struct {
	ID        string   `json:"id"`
	Title     string   `json:"title,omitempty"`
	Skill     string   `json:"skill,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

type runSummary struct {
	ID         string    `json:"id"`
	Dir        string    `json:"dir"`
	Brief      string    `json:"brief"`
	Workflow   string    `json:"workflow"`
	Mock       bool      `json:"mock"`
	UpdatedAt  time.Time `json:"updated_at"`
	StageDone  int       `json:"stage_done"`
	StageFail  int       `json:"stage_fail"`
	StageTotal int       `json:"stage_total"`
	Complete   *bool     `json:"complete"` // 来自渲染清单（null = 未渲染）
	HasReport  bool      `json:"has_report"`
	CostCNY    float64   `json:"cost_cny"`
	ShotsDone  int       `json:"shots_done"`
	ShotsTotal int       `json:"shots_total"`
}

func readRunSummary(runDir string) (*runSummary, error) {
	data, err := os.ReadFile(filepath.Join(runDir, "state.json"))
	if err != nil {
		return nil, err
	}
	var st engine.RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	sum := &runSummary{
		ID:        st.ID,
		Dir:       filepath.ToSlash(runDir),
		Brief:     st.Brief,
		Workflow:  st.Workflow,
		Mock:      st.Mock,
		UpdatedAt: st.UpdatedAt,
	}
	sum.StageTotal = len(st.Stages)
	for _, s := range st.Stages {
		switch s.Status {
		case "done":
			sum.StageDone++
		case "failed":
			sum.StageFail++
		}
	}
	var man render.Manifest
	if readJSONFile(filepath.Join(runDir, "output", "render_manifest.json"), &man) == nil {
		c := man.Complete
		sum.Complete = &c
		sum.CostCNY = man.Cost.ActualCNY
		sum.ShotsTotal = len(man.Shots)
		sum.ShotsDone = man.Count("done")
	}
	if fileExists(filepath.Join(runDir, "output", "delivery_report.json")) {
		sum.HasReport = true
	}
	return sum, nil
}

// listRuns 扫描 runs/ 下含 state.json 的目录，按更新时间倒序。
// trash=false 只列正常项目；trash=true 只列回收站里的（逻辑删除的）。
func (s *Server) listRuns(trash bool) []*runSummary {
	entries, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		return nil
	}
	out := []*runSummary{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, "runs", e.Name())
		if !fileExists(filepath.Join(dir, "state.json")) {
			continue
		}
		if isRunDeleted(dir) != trash {
			continue
		}
		if sum, err := readRunSummary(dir); err == nil {
			out = append(out, sum)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

type systemInfo struct {
	FFmpegOK      bool     `json:"ffmpeg_ok"`
	FFmpegPath    string   `json:"ffmpeg_path"`
	FFprobePath   string   `json:"ffprobe_path"`
	Version       string   `json:"version"`
	Loudnorm      bool     `json:"loudnorm"`
	Subtitles     bool     `json:"subtitles"`
	Sidechain     bool     `json:"sidechain"`
	Warnings      []string `json:"warnings,omitempty"`
	Mock          bool     `json:"mock"`
	ConfigSummary string   `json:"config_summary"`
	BudgetCNY     float64  `json:"budget_cny"`
	ConfigError   string   `json:"config_error,omitempty"`
}

func (s *Server) systemInfo() systemInfo {
	caps := s.detectCaps()
	info := systemInfo{
		FFmpegOK:    len(caps.Missing) == 0,
		FFmpegPath:  caps.FFmpegPath,
		FFprobePath: caps.FFprobePath,
		Version:     caps.Version,
		Loudnorm:    caps.HasLoudnorm,
		Subtitles:   caps.HasSubtitles,
		Sidechain:   caps.HasSidechain,
		Warnings:    caps.Warnings,
	}
	if cfg, err := s.loadConfigSafe(); err == nil {
		// 生效口径与 ai.New 一致：配置了密钥（环境变量或本地字面量）即真实模式，
		// 不然 config.json 写着 mock:true 而 local 层配了 Key 时会显示错误状态。
		info.Mock = cfg.Mock && cfg.APIKey() == ""
		info.ConfigSummary = cfg.String()
		info.BudgetCNY = cfg.Budget.MaxCostCNY
	} else {
		info.ConfigError = err.Error()
	}
	return info
}

// ---- Handlers ----

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	runs := s.listRuns(false)
	done, reported := 0, 0
	for _, rr := range runs {
		if rr.Complete != nil && *rr.Complete {
			done++
		}
		if rr.HasReport {
			reported++
		}
	}
	writeJSON(w, 200, map[string]any{
		"root":          s.root,
		"generated":     time.Now(),
		"runs_total":    len(runs),
		"runs_complete": done,
		"runs_reported": reported,
		"recent_runs":   firstN(runs, 8),
		"jobs":          s.jobs.List("", 8),
		"system":        s.systemInfo(),
	})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("trash") == "1" {
		out := []map[string]any{}
		for _, sum := range s.listRuns(true) {
			out = append(out, map[string]any{
				"summary": sum,
				"deleted": readDeletedInfo(filepath.Join(s.root, "runs", sum.ID)),
			})
		}
		writeJSON(w, 200, out)
		return
	}
	writeJSON(w, 200, s.listRuns(false))
}

func (s *Server) runDir(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return "", fmt.Errorf("非法 run id")
	}
	dir := filepath.Join(s.root, "runs", id)
	if !fileExists(filepath.Join(dir, "state.json")) {
		return "", fmt.Errorf("run %s 不存在", id)
	}
	return dir, nil
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	sum, err := readRunSummary(dir)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	var st engine.RunState
	_ = json.Unmarshal(data, &st)

	resp := map[string]any{
		"summary": sum,
		"mock":    st.Mock,
		"usage":   st.Usage,
		"jobs":    s.jobs.List(sum.ID, 10),
		"deleted": isRunDeleted(dir),
	}

	// 阶段按工作流顺序（stage_order 优先），与 CLI status 一致
	var stageOrder []string
	if len(st.StageOrder) > 0 {
		stageOrder = st.StageOrder
	} else {
		for id := range st.Stages {
			stageOrder = append(stageOrder, id)
		}
		sort.Strings(stageOrder)
	}
	// 兜底：状态里有、但 order 清单漏掉的阶段（旧 state 兼容），按字母序补在末尾
	seenStage := map[string]bool{}
	for _, id := range stageOrder {
		seenStage[id] = true
	}
	var extraStages []string
	for id := range st.Stages {
		if !seenStage[id] {
			extraStages = append(extraStages, id)
		}
	}
	sort.Strings(extraStages)
	stageOrder = append(stageOrder, extraStages...)
	stages := make([]stageView, 0, len(stageOrder))
	for _, id := range stageOrder {
		sv, ok := st.Stages[id]
		if !ok {
			continue
		}
		stages = append(stages, stageView{
			ID: id, Status: sv.Status, Attempts: sv.Attempts, Revisions: sv.Revisions,
			DurationMS: sv.DurationMS, Error: sv.Error, RepairNote: sv.RepairNote, OutputFile: sv.OutputFile,
		})
	}
	resp["stages"] = stages

	// 工作流图（创作中心"流程图"视图的数据源）：节点=阶段定义，边=depends_on。
	// 读不到定义文件时不返回该字段，前端回落为线性阶段条带。
	if wf, werr := engine.LoadWorkflow(filepath.Join(s.root, "workflows", "movie.json")); werr == nil {
		nodes := make([]workflowNode, 0, len(wf.Stages))
		for _, st := range wf.Stages {
			nodes = append(nodes, workflowNode{ID: st.ID, Title: st.Describe(), Skill: st.Skill, DependsOn: st.DependsOn})
		}
		resp["workflow"] = map[string]any{"name": wf.Name, "nodes": nodes}
	}

	var tp, tc, calls int64
	for _, u := range st.Usage {
		tp += u.PromptTokens
		tc += u.CompletionTokens
		calls += int64(u.Calls)
	}
	resp["tokens"] = map[string]int64{"prompt": tp, "completion": tc, "calls": calls}

	resp["artifacts"] = s.listFiles(dir, "artifacts")
	resp["outputs"] = s.listFiles(dir, "output")

	var man render.Manifest
	if readJSONFile(filepath.Join(dir, "output", "render_manifest.json"), &man) == nil {
		// 渲染进行中清单尚未写盘：按磁盘实况回填已生成的镜头产物，
		// 页面不等整轮渲染结束就能逐镜预览
		backfillShotOutputs(&man, filepath.Join(dir, "output", "shots"))
		resp["manifest"] = &man
	}
	var report deliver.Report
	if readJSONFile(filepath.Join(dir, "output", "delivery_report.json"), &report) == nil {
		resp["report"] = &report
	}
	var shotplan struct {
		Style promspec.Style `json:"style"`
	}
	if readJSONFile(filepath.Join(dir, "output", "shotplan.json"), &shotplan) == nil {
		resp["style"] = shotplan.Style
	}
	writeJSON(w, 200, resp)
}

// backfillShotOutputs 清单里缺失的镜头产物按磁盘实况回填（渲染进行中清单未落盘的场景）。
// 只认完整落盘的文件：.part 半截文件与 invalid/ 隔离区都不会被误报为可播放。
func backfillShotOutputs(man *render.Manifest, shotsDir string) {
	for i := range man.Shots {
		sh := &man.Shots[i]
		if sh.Video == "" && fileExists(filepath.Join(shotsDir, sh.ID+".mp4")) {
			sh.Video = filepath.ToSlash(filepath.Join("output", "shots", sh.ID+".mp4"))
		}
		if sh.Image == "" && fileExists(filepath.Join(shotsDir, sh.ID+".png")) {
			sh.Image = filepath.ToSlash(filepath.Join("output", "shots", sh.ID+".png"))
		}
		if sh.Video != "" && sh.Status != "done" {
			sh.Status = "done"
		}
	}
}

func (s *Server) listFiles(dir, sub string) []fileInfo {
	base := filepath.Join(dir, sub)
	var out []fileInfo
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, fileInfo{
			Path:    filepath.ToSlash(rel),
			Name:    d.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Kind:    kindOf(d.Name()),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (s *Server) handleArtifactList(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, s.listFiles(dir, "artifacts"))
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	name := strings.TrimSuffix(path.Base(r.PathValue("name")), ".json")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeErr(w, 400, fmt.Errorf("非法产物名"))
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, "artifacts", name+".json"))
	if err != nil {
		writeErr(w, 404, fmt.Errorf("产物 %s 不存在", name))
		return
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		writeJSON(w, 200, map[string]any{"name": name, "raw": string(data)})
		return
	}
	writeJSON(w, 200, map[string]any{"name": name, "data": v})
}

// handleRunAssets 返回素材库索引（output/assets/index.json）。
// 没有素材库时返回空索引而不是 404——前端据此展示"尚未生成"空态。
func (s *Server) handleRunAssets(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ix := shotkit.LoadAssetIndex(dir)
	writeJSON(w, 200, map[string]any{
		"index":      ix,
		"has_assets": ix.Characters != nil || ix.Scenes != nil,
	})
}

// ---- 逻辑删除（软删除） ----
//
// 打标不删档：run 目录原样保留（成片/素材/任务记录都不动），仅在目录内写一个
// .deleted 标记。列表默认隐藏已删项目，回收站可恢复；彻底删除必须显式二次确认。

type deletedInfo struct {
	DeletedAt time.Time `json:"deleted_at"`
	Note      string    `json:"note,omitempty"`
}

func deletedMarkerPath(runDir string) string { return filepath.Join(runDir, ".deleted") }

func isRunDeleted(runDir string) bool {
	_, err := os.Stat(deletedMarkerPath(runDir))
	return err == nil
}

func readDeletedInfo(runDir string) *deletedInfo {
	data, err := os.ReadFile(deletedMarkerPath(runDir))
	if err != nil {
		return nil
	}
	var di deletedInfo
	_ = json.Unmarshal(data, &di)
	return &di
}

func (s *Server) runningJobFor(runID string) string {
	for _, j := range s.jobs.List(runID, 0) {
		if j.Status == StatusRunning {
			return j.Type
		}
	}
	return ""
}

// handleRunDelete 逻辑删除：写入标记，文件保留。
func (s *Server) handleRunDelete(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if isRunDeleted(dir) {
		writeErr(w, 400, fmt.Errorf("项目已在回收站中"))
		return
	}
	if jt := s.runningJobFor(filepath.Base(dir)); jt != "" {
		writeErr(w, 400, fmt.Errorf("该项目有任务执行中（%s），请等待完成或先取消", jt))
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	di := deletedInfo{DeletedAt: time.Now(), Note: req.Note}
	data, _ := json.MarshalIndent(di, "", "  ")
	tmp := deletedMarkerPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := os.Rename(tmp, deletedMarkerPath(dir)); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// handleRunRestore 从回收站恢复。
func (s *Server) handleRunRestore(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := os.Remove(deletedMarkerPath(dir)); err != nil {
		if os.IsNotExist(err) {
			writeErr(w, 400, fmt.Errorf("该项目不在回收站中"))
			return
		}
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// handleRunPurge 彻底删除（物理删除 run 目录，不可恢复）。
func (s *Server) handleRunPurge(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if r.URL.Query().Get("confirm") != "1" {
		writeErr(w, 400, fmt.Errorf("彻底删除不可恢复，需要显式确认（?confirm=1）"))
		return
	}
	if jt := s.runningJobFor(filepath.Base(dir)); jt != "" {
		writeErr(w, 400, fmt.Errorf("该项目有任务执行中（%s），请等待完成或先取消", jt))
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// resolveWorkspacePath 校验并解析 root 内的相对路径。
// 拒绝绝对路径、..、点开头的隐藏目录（.web/.git 等）。
func (s *Server) resolveWorkspacePath(rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if rel == "" || path.IsAbs(rel) {
		return "", fmt.Errorf("非法路径")
	}
	clean := path.Clean(rel)
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("路径越界")
	}
	for _, seg := range strings.Split(clean, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return "", fmt.Errorf("禁止访问隐藏路径")
		}
	}
	return filepath.Join(s.root, filepath.FromSlash(clean)), nil
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	abs, err := s.resolveWorkspacePath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.serveFile(w, r, abs)
}

func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	rel := path.Clean(r.PathValue("path"))
	if strings.HasPrefix(rel, "..") {
		writeErr(w, 400, fmt.Errorf("路径越界"))
		return
	}
	s.serveFile(w, r, filepath.Join(dir, filepath.FromSlash(rel)))
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, abs string) {
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(w, 404, fmt.Errorf("文件不存在"))
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%q", filepath.Base(abs)))
	} else {
		w.Header().Set("Content-Disposition", "inline")
	}
	// http.ServeFile 自带 Range 支持（视频/音频拖动播放依赖它）
	http.ServeFile(w, r, abs)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleJobCreate(w, r)
		return
	}
	writeJSON(w, 200, s.jobs.List(r.URL.Query().Get("run"), atoiDefault(r.URL.Query().Get("limit"), 50)))
}

func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type   string         `json:"type"`
		Run    string         `json:"run"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Params == nil {
		req.Params = map[string]any{}
	}
	// 已逻辑删除的项目：先恢复才能再派任务（付费出口不碰回收站里的东西）
	if req.Run != "" {
		if dir, err := s.runDir(req.Run); err == nil && isRunDeleted(dir) {
			writeErr(w, 400, fmt.Errorf("项目已在回收站中，请先恢复再操作"))
			return
		}
	}
	job, err := s.jobs.Start(req.Type, req.Run, req.Params)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, job)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobs.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, job)
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	tail := atoiDefault(r.URL.Query().Get("tail"), 0)
	text, err := s.jobs.TailLog(r.PathValue("id"), tail)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]string{"log": text})
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// ---- 系统元信息 ----

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	meta := map[string]any{"root": s.root, "system": s.systemInfo()}

	// 配置文件掩码视图（api_key 永不出网）
	var cfgViews []map[string]any
	for _, f := range []string{"config.json", "config.local.json", "config.deepseek.json"} {
		p := filepath.Join(s.root, f)
		if !fileExists(p) {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v any
		if json.Unmarshal(data, &v) != nil {
			continue
		}
		cfgViews = append(cfgViews, map[string]any{"file": f, "content": maskSecrets(v)})
	}
	meta["configs"] = cfgViews

	var presets []map[string]any
	for _, name := range promspec.AllPresets() {
		p, ok := promspec.PresetByName(name)
		if !ok {
			continue
		}
		presets = append(presets, map[string]any{
			"name":        name,
			"style_name":  p.Style.Name,
			"aspect":      p.Style.Aspect,
			"camera":      p.Style.Camera.Name,
			"film_stock":  p.Style.Camera.FilmStock,
			"grade":       p.Style.Grade.Palette,
			"grain":       p.Style.Grade.Grain,
			"time_of_day": p.Style.TimeOfDay,
		})
	}
	meta["presets"] = presets

	// 工作流定义
	var flows []map[string]any
	if entries, err := os.ReadDir(filepath.Join(s.root, "workflows")); err == nil {
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			wf, err := engine.LoadWorkflow(filepath.Join(s.root, "workflows", e.Name()))
			if err != nil {
				continue
			}
			stageIDs := make([]string, 0, len(wf.Stages))
			for i := range wf.Stages {
				stageIDs = append(stageIDs, wf.Stages[i].ID+" · "+wf.Stages[i].Describe())
			}
			flows = append(flows, map[string]any{
				// file 是回传给 CLI 的 --workflow 参数，子进程 cwd 为项目根，必须带 workflows/ 前缀
				"file": "workflows/" + e.Name(), "name": wf.Name, "brief": wf.Brief, "stages": stageIDs,
			})
		}
	}
	meta["workflows"] = flows

	// 经验记忆库
	if mem, err := selfheal.LoadMemory(filepath.Join(s.root, "memory", "lessons.json")); err == nil {
		lessons := mem.Lessons
		sort.Slice(lessons, func(i, j int) bool { return lessons[i].Time.After(lessons[j].Time) })
		meta["lessons"] = lessons
	}

	writeJSON(w, 200, meta)
}

// maskSecrets 递归掩掉 api_key 字段值——配置可以看，密钥不能出网。
func maskSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if strings.EqualFold(k, "api_key") {
				s, _ := val.(string)
				if s != "" {
					out[k] = "••••（已配置，已隐藏）"
				} else {
					out[k] = ""
				}
				continue
			}
			out[k] = maskSecrets(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskSecrets(val)
		}
		return out
	}
	return v
}

// ---- 小工具 ----

func readJSONFile(p string, v any) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func firstN[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
