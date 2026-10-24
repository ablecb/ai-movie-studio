package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/shotkit"
)

// backfill-calls：把历史调用记录里"当时随请求发出的图"反查归档，让详情页能回看。
//
// 记录层此前不落请求图，但图的源文件都在 run 目录里：视频首帧 output/shots/<shot>.png、
// 尾帧锚点 <shot>.planned.png、角色参考帧 output/refs/（shotplan.json 逐镜登记）。
// 记录里有 run 与镜头 ID（视频记录的 local_path 常缺失，改用"响应存档副本与
// shots/*.mp4 按文件大小配对"反查——同一 URL 两次下载字节一致；被后续重渲覆盖的
// 旧镜头大小对不上，自然排除，不会张冠李戴）。
//
// 时间戳核对：源文件 mtime ≤ 调用结束时间 = 调用之后没被重写过，才认定是
// "当时那张"。只补空字段，绝不覆盖运行期归档或新记录；--dry-run 只报告不落盘。
// 注意：运行期记录器按 进程内锁+原子写 保护单条记录，回填前最好先停掉 serve，
// 避免与在途调用的读改写撞车。
func cmdBackfillCalls(args []string) error {
	fs := flag.NewFlagSet("backfill-calls", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "只报告能补什么，不写任何文件")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := os.Getenv("AI_CALL_LOG")
	switch dir {
	case "off":
		return errors.New("AI_CALL_LOG=off，调用记录已停用，无历史可补")
	case "":
		dir = "logs/ai_calls"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取调用记录目录失败: %w", err)
	}

	planCache := map[string]*shotkit.Plan{}
	stats := map[string]int{}
	var backfilled, skipped, failed []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec ai.CallRecord
		if json.Unmarshal(data, &rec) != nil || rec.Request == nil || rec.Response == nil {
			continue
		}
		stats["扫描"]++
		if rec.Request.FirstFrameFile != "" || rec.Request.EndFrameFile != "" || len(rec.Request.RefImageFiles) > 0 {
			stats["已有存档"]++
			continue
		}
		if rec.Run == "" {
			skipped = append(skipped, fmt.Sprintf("%s 无项目关联", rec.ID))
			continue
		}
		runDir := filepath.Join("runs", rec.Run)
		if _, err := os.Stat(runDir); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s 项目目录不存在（%s）", rec.ID, rec.Run))
			continue
		}

		cands, err := collectBackfillCandidates(&rec, runDir, dir, planCache)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s %v", rec.ID, err))
			continue
		}
		if len(cands) == 0 {
			skipped = append(skipped, fmt.Sprintf("%s 找不到可反查的请求图（%s %s）", rec.ID, rec.Kind, rec.Call))
			continue
		}
		if *dry {
			names := make([]string, 0, len(cands))
			for _, c := range cands {
				names = append(names, fmt.Sprintf("%s←%s", c.role, filepath.Base(c.src)))
			}
			fmt.Printf("  可补 %s（%s %s）：%s\n", rec.ID, rec.Kind, rec.Call, strings.Join(names, " "))
			backfilled = append(backfilled, rec.ID)
			continue
		}
		if err := applyBackfill(&rec, cands, dir); err != nil {
			failed = append(failed, fmt.Sprintf("%s %v", rec.ID, err))
			continue
		}
		out, err := json.MarshalIndent(&rec, "", " ")
		if err == nil {
			if werr := writeAtomic(path, out); werr != nil {
				failed = append(failed, fmt.Sprintf("%s 写回失败: %v", rec.ID, werr))
				continue
			}
		}
		backfilled = append(backfilled, rec.ID)
	}

	fmt.Printf("\n档案回填%s：%d 条记录\n", dryLabel(*dry), stats["扫描"])
	for _, k := range []string{"已有存档"} {
		if stats[k] > 0 {
			fmt.Printf("  %s：%d\n", k, stats[k])
		}
	}
	fmt.Printf("  补档成功：%d\n", len(backfilled))
	if len(failed) > 0 {
		fmt.Printf("  失败：%d\n", len(failed))
		for _, s := range failed {
			fmt.Println("    " + s)
		}
	}
	if len(skipped) > 0 {
		fmt.Printf("  跳过（无法可靠反查，保持\"未记录\"原状）：%d\n", len(skipped))
		for _, s := range skipped {
			fmt.Println("    " + s)
		}
	}
	return nil
}

// reqCand 一张待归档的请求图：role 与源文件。
type reqCand struct {
	role string
	src  string
	ext  string
}

// collectBackfillCandidates 按调用类型反查"当时发出的图"。
// 视频调用：首帧 = shots/<shot>.png，尾帧锚点 = shots/<shot>.planned.png；
// 出图调用：参考帧 = shotplan.json 里该镜头登记的角色参考帧。
func collectBackfillCandidates(rec *ai.CallRecord, runDir, logDir string, planCache map[string]*shotkit.Plan) ([]reqCand, error) {
	endAt := rec.EndAt
	shotID := shotIDOf(rec, runDir, logDir)
	if shotID == "" {
		return nil, nil
	}
	switch {
	case rec.Kind == ai.KindVideo:
		var cands []reqCand
		if p := filepath.Join(runDir, "output", "shots", shotID+".png"); usable(p, endAt) {
			cands = append(cands, reqCand{"first_frame", p, ".png"})
		}
		if rec.Request.EndImage {
			if p := filepath.Join(runDir, "output", "shots", shotID+".planned.png"); usable(p, endAt) {
				cands = append(cands, reqCand{"end_frame", p, ".png"})
			}
		}
		return cands, nil
	case rec.Kind == ai.KindImage && (rec.Call == "render.shot_image" || rec.Call == "render.shot_image_edit"):
		plan := loadPlan(planCache, runDir)
		if plan == nil {
			return nil, nil
		}
		task, ok := plan.TaskOf(shotID)
		if !ok {
			return nil, nil
		}
		refs := task.RefImages
		if len(refs) == 0 && task.RefImage != "" {
			refs = []string{task.RefImage}
		}
		var cands []reqCand
		for _, refRel := range refs {
			if refRel == "" {
				continue
			}
			p := filepath.Join(runDir, filepath.FromSlash(refRel))
			if usable(p, endAt) {
				cands = append(cands, reqCand{"ref_image", p, strings.ToLower(filepath.Ext(p))})
			}
		}
		return cands, nil
	}
	return nil, nil
}

// shotIDOf 反查镜头 ID：优先响应里的 local_path（出图记录都有），
// 缺失时（视频记录常见）用响应存档副本与 shots/*.mp4 按文件大小配对——
// 同一产物两次下载字节一致；大小碰撞或多命中都放弃，宁缺毋滥。
func shotIDOf(rec *ai.CallRecord, runDir, logDir string) string {
	if lp := rec.Response.LocalPath; lp != "" {
		base := filepath.Base(filepath.FromSlash(lp))
		id := strings.TrimSuffix(base, filepath.Ext(base))
		if strings.HasPrefix(id, "sh") {
			return id
		}
	}
	if rec.Kind != ai.KindVideo || rec.Response.ArchivePath == "" {
		return ""
	}
	arch, err := os.Stat(filepath.Join(logDir, filepath.FromSlash(rec.Response.ArchivePath)))
	if err != nil {
		return ""
	}
	shotsDir := filepath.Join(runDir, "output", "shots")
	matches, _ := os.ReadDir(shotsDir)
	hit := ""
	for _, m := range matches {
		if m.IsDir() || !strings.HasSuffix(m.Name(), ".mp4") {
			continue
		}
		info, err := m.Info()
		if err != nil || info.Size() != arch.Size() {
			continue
		}
		if hit != "" {
			return "" // 多命中：大小碰撞，放弃
		}
		hit = strings.TrimSuffix(m.Name(), ".mp4")
	}
	return hit
}

// usable 源文件存在且在调用结束时已存在、之后没被重写（mtime ≤ 调用结束）。
func usable(path string, endAt time.Time) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || endAt.IsZero() {
		return false
	}
	return !st.ModTime().After(endAt)
}

func loadPlan(cache map[string]*shotkit.Plan, runDir string) *shotkit.Plan {
	if p, ok := cache[runDir]; ok {
		return p
	}
	var p shotkit.Plan
	data, err := os.ReadFile(filepath.Join(runDir, "output", "shotplan.json"))
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err != nil {
		cache[runDir] = nil
		return nil
	}
	cache[runDir] = &p
	return &p
}

// applyBackfill 复制源文件进 artifacts/（命名与运行期归档一致），把路径写回记录。
func applyBackfill(rec *ai.CallRecord, cands []reqCand, logDir string) error {
	artDir := filepath.Join(logDir, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return err
	}
	refN := 0
	for _, c := range cands {
		var name string
		if c.role == "ref_image" {
			name = fmt.Sprintf("%s_req_ref_%d", rec.ID, refN)
			refN++
		} else {
			name = rec.ID + "_req_" + c.role
		}
		data, err := os.ReadFile(c.src)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", c.src, err)
		}
		dst := filepath.Join(artDir, name+c.ext)
		if err := writeAtomic(dst, data); err != nil {
			return fmt.Errorf("写存档失败: %w", err)
		}
		rel := filepath.ToSlash(filepath.Join("artifacts", name+c.ext))
		switch c.role {
		case "first_frame":
			rec.Request.FirstFrameFile = rel
			if rec.Request.FirstFrame == "" {
				rec.Request.FirstFrame = "data-url" // 历史链路固定以内联 data URL 发送首帧
			}
		case "end_frame":
			rec.Request.EndFrameFile = rel
		case "ref_image":
			rec.Request.RefImageFiles = append(rec.Request.RefImageFiles, rel)
		}
	}
	return nil
}

// writeAtomic 原子写：先写临时文件再改名（与记录层 atomicWrite 同一纪律）。
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dryLabel(dry bool) string {
	if dry {
		return "（试运行）"
	}
	return ""
}
