package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"aistudio/internal/ai"
)

// AI 调用记录 API：读取 logs/ai_calls/ 下的调用账本（每次调用一个 JSON 文件）。
// 列表按文件名倒序 = 时间倒序；q 对操作类型/模型/项目做模糊匹配。

var aiCallNameRe = regexp.MustCompile(`^[0-9A-Za-z_.-]+$`)

func (s *Server) aiCallsDir() string { return filepath.Join(s.root, "logs", "ai_calls") }

// handleAICalls GET /api/ai-calls?kind=&model=&q=&start=&end=&page=&page_size=
// 分页查询：类型 / 模型 / 关键字（操作、项目）/ 时间范围筛选。
// start/end 支持 2006-01-02（当天起止）或 RFC3339。
func (s *Server) handleAICalls(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	page := 1
	if n, err := strconv.Atoi(qp.Get("page")); err == nil && n > 0 {
		page = n
	}
	pageSize := 50
	if n, err := strconv.Atoi(qp.Get("page_size")); err == nil && n > 0 {
		pageSize = n
	}
	if pageSize > 200 {
		pageSize = 200
	}
	kind := strings.ToLower(strings.TrimSpace(qp.Get("kind")))
	model := strings.ToLower(strings.TrimSpace(qp.Get("model")))
	q := strings.ToLower(strings.TrimSpace(qp.Get("q")))
	startMs, endMs := int64(0), int64(0)
	if v := strings.TrimSpace(qp.Get("start")); v != "" {
		if t, err := parseTimeParam(v, false); err == nil {
			startMs = t.UnixMilli()
		}
	}
	if v := strings.TrimSpace(qp.Get("end")); v != "" {
		if t, err := parseTimeParam(v, true); err == nil {
			endMs = t.UnixMilli()
		}
	}

	entries, err := os.ReadDir(s.aiCallsDir())
	if err != nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "total": 0, "page": page, "page_size": pageSize})
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // 文件名以毫秒时间戳开头：倒序即最新在前

	skip := (page - 1) * pageSize
	items := make([]ai.CallRecord, 0, pageSize)
	total := 0
	for _, name := range names {
		// 时间筛选直接用文件名里的毫秒时间戳，不读文件
		if ts := nameUnixMs(name); ts > 0 {
			if startMs > 0 && ts < startMs {
				continue
			}
			if endMs > 0 && ts > endMs {
				continue
			}
		}
		data, err := os.ReadFile(filepath.Join(s.aiCallsDir(), name))
		if err != nil {
			continue
		}
		var rec ai.CallRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if kind != "" && strings.ToLower(rec.Kind) != kind {
			continue
		}
		if model != "" && !strings.Contains(strings.ToLower(rec.Model), model) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(rec.Call), q) &&
			!strings.Contains(strings.ToLower(rec.Run), q) {
			continue
		}
		total++
		if total > skip && len(items) < pageSize {
			items = append(items, rec)
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// nameUnixMs 记录文件名以毫秒时间戳开头（<unixms>_<pid>_<seq>.json），解析失败返回 0。
func nameUnixMs(name string) int64 {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0
	}
	ms, err := strconv.ParseInt(name[:i], 10, 64)
	if err != nil {
		return 0
	}
	return ms
}

// parseTimeParam 支持 2006-01-02（endOfDay=true 时取当天最后一毫秒）与 RFC3339。
func parseTimeParam(v string, endOfDay bool) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Millisecond)
		}
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %s", v)
}

// handleAICallArtifact GET /api/ai-calls/{name}/artifact 服务记录层存档的媒体产物
// （logs/ai_calls/artifacts/<id>.png|.mp4，由记录器在媒体调用成功后异步下载）。
func (s *Server) handleAICallArtifact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !aiCallNameRe.MatchString(name) {
		writeJSON(w, 400, map[string]any{"error": "非法记录名"})
		return
	}
	name = strings.TrimSuffix(name, ".json")
	matches, err := filepath.Glob(filepath.Join(s.aiCallsDir(), "artifacts", name+".*"))
	if err != nil || len(matches) == 0 {
		writeJSON(w, 404, map[string]any{"error": "存档不存在"})
		return
	}
	http.ServeFile(w, r, matches[0])
}

// handleAICallReqImg GET /api/ai-calls/{name}/reqimg?role=first_frame|end_frame|ref_image&idx=N
// 服务请求侧图片的存档副本：记录器把当时随请求发出的图（首帧/尾帧/参考图）归档到
// artifacts/，路径写回 CallRequest 的 *File 字段——详情页据此回看"喂给模型的到底是哪张图"。
func (s *Server) handleAICallReqImg(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !aiCallNameRe.MatchString(name) {
		writeJSON(w, 400, map[string]any{"error": "非法记录名"})
		return
	}
	data, err := os.ReadFile(filepath.Join(s.aiCallsDir(), strings.TrimSuffix(name, ".json")+".json"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "记录不存在"})
		return
	}
	var rec ai.CallRecord
	if json.Unmarshal(data, &rec) != nil || rec.Request == nil {
		writeJSON(w, 404, map[string]any{"error": "记录损坏或无请求内容"})
		return
	}
	role := r.URL.Query().Get("role")
	idx, _ := strconv.Atoi(r.URL.Query().Get("idx"))
	var rel string
	switch role {
	case "first_frame":
		rel = rec.Request.FirstFrameFile
	case "end_frame":
		rel = rec.Request.EndFrameFile
	case "ref_image":
		if idx >= 0 && idx < len(rec.Request.RefImageFiles) {
			rel = rec.Request.RefImageFiles[idx]
		}
	default:
		writeJSON(w, 400, map[string]any{"error": "非法 role"})
		return
	}
	if rel == "" {
		writeJSON(w, 404, map[string]any{"error": "该记录没有请求图片存档（旧记录或归档失败）"})
		return
	}
	// 路径穿越防护：只接受记录层自己生成的 artifacts/<纯文件名> 形态
	base := filepath.Base(rel)
	if filepath.ToSlash(rel) != "artifacts/"+base || strings.Contains(base, "..") {
		writeJSON(w, 400, map[string]any{"error": "非法存档路径"})
		return
	}
	http.ServeFile(w, r, filepath.Join(s.aiCallsDir(), "artifacts", base))
}

// handleAICall GET /api/ai-calls/{name} 单条记录详情。name 为记录 ID（可带 .json）。
func (s *Server) handleAICall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !aiCallNameRe.MatchString(name) {
		writeJSON(w, 400, map[string]any{"error": "非法记录名"})
		return
	}
	if !strings.HasSuffix(name, ".json") {
		name += ".json" // 前端传的是记录 ID（无扩展名）
	}
	data, err := os.ReadFile(filepath.Join(s.aiCallsDir(), name))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "记录不存在"})
		return
	}
	var rec ai.CallRecord
	if json.Unmarshal(data, &rec) != nil {
		writeJSON(w, 500, map[string]any{"error": "记录损坏"})
		return
	}
	writeJSON(w, 200, rec)
}
