package vision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
)

// buildRunDir 构造一个最小可复核的 run 目录：一个真实可探测的镜头片段 + 必要产物。
func buildRunDir(t *testing.T, shots []string) string {
	t.Helper()
	dir := t.TempDir()
	shotsDir := filepath.Join(dir, "output", "shots")
	if err := os.MkdirAll(shotsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// shotspec：用最小 JSON 直接落盘（避免依赖 skills 包）
	kitJSON := `{"style":{"name":"t","aspect":"2.39:1"},"version":1,"characters":[{"name":"林","token":"lin-female35"}],"shots":[`
	for i, id := range shots {
		if i > 0 {
			kitJSON += ","
		}
		kitJSON += `{"id":"` + id + `","scene_id":"s1","description":"测试镜头","shot_size":"中景","camera_move":"固定",` +
			`"duration_sec":2.0,"characters":["林"],"beats":[{"kind":"dialogue","speaker":"林","text":"台词","at_sec":0.2}]}`
	}
	kitJSON += `]}`
	artDir := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "shotspec.json"), []byte(kitJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 用 ffmpeg 生成真实可抽帧的片段（否则抽帧环节无从验证）
	man := &struct {
		Shots []map[string]any `json:"shots"`
	}{}
	for i, id := range shots {
		p := filepath.Join(shotsDir, id+".mp4")
		_ = os.Remove(p)
		// 每镜用不同亮度，便于人工确认抽帧确实来自不同镜头
		color := []string{"0x304050", "0x604020", "0x204040", "0x503030"}[i%4]
		_, err := ffx.Run(120*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "color=c="+color+":s=640x360:r=24",
			"-vf", "noise=alls=20:allf=t+u", "-t", "2.0",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "30", "-an", p)
		if err != nil {
			t.Skipf("环境无法生成测试视频（跳过）: %v", err)
		}
		man.Shots = append(man.Shots, map[string]any{
			"id": id, "status": "done",
			"video":               filepath.ToSlash(filepath.Join("output", "shots", id+".mp4")),
			"actual_duration_sec": 2.0,
		})
	}
	if err := fsx.AtomicWriteJSON(filepath.Join(dir, "output", "render_manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunExtractsFramesAndMergesResults 验证视觉复核主流程：
// 抽帧 → 送检 → 合并结论 → 生成重渲清单。
func TestRunExtractsFramesAndMergesResults(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := buildRunDir(t, []string{"sh001", "sh002"})

	cfg := config.Default()
	cfg.VisionReview.Enabled = true
	cfg.VisionReview.SampleFrames = 3

	// 离线客户端会返回固定的复核结论（首镜不合格），用于验证合并与重渲清单
	client := ai.New(ai.Config{Model: "test", Mock: true})

	rep, err := Run(cfg, Options{RunDir: dir, Client: client})
	if err != nil {
		t.Fatalf("视觉复核失败: %v", err)
	}
	if rep.FramesTotal < 2 {
		t.Errorf("抽帧数过少: %d（两个镜头各应抽到帧）", rep.FramesTotal)
	}
	if len(rep.Shots) != 2 {
		t.Fatalf("期望 2 个镜头的复核结论，实际 %d", len(rep.Shots))
	}
	// 离线 mock 对 sh001 给 7.0（不达标）、对 sh002/sh003 给 8.6/8.2（达标）
	var sh001 *ShotReview
	for i := range rep.Shots {
		if rep.Shots[i].ShotID == "sh001" {
			sh001 = &rep.Shots[i]
		}
	}
	if sh001 == nil {
		t.Fatal("缺少 sh001 的复核结论")
	}
	if sh001.Pass {
		t.Errorf("sh001 得分 %.1f 低于达标线，不应判为通过", sh001.Score)
	}
	// 抽帧文件必须真实存在（报告里的路径要能人工核查）
	for _, f := range rep.Frames {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.File))); err != nil {
			t.Errorf("报告中引用的帧文件不存在: %s", f.File)
		}
	}
}

// TestRunRejectsWhenDisabled 未启用时必须明确拒绝，而不是静默跳过。
func TestRunRejectsWhenDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.VisionReview.Enabled = false
	client := ai.New(ai.Config{Model: "test", Mock: true})
	if _, err := Run(cfg, Options{RunDir: t.TempDir(), Client: client}); err == nil {
		t.Error("未启用视觉复核时应返回错误")
	}
}

// TestRunWithoutClient 缺少视觉客户端时必须报错（不能悄悄不做复核）。
func TestRunWithoutClient(t *testing.T) {
	cfg := config.Default()
	cfg.VisionReview.Enabled = true
	if _, err := Run(cfg, Options{RunDir: t.TempDir()}); err == nil {
		t.Error("缺少视觉客户端时应返回错误")
	}
}

// TestFinalizeSeparatesRequestFailureFromQuality 请求失败 ≠ 画面不合格。
// 两者混为一谈会让一次网络抖动被误读成"片子不能用"。
func TestFinalizeSeparatesRequestFailureFromQuality(t *testing.T) {
	rep := &Report{Shots: []ShotReview{{ShotID: "sh001", Score: 9, Pass: true}}}
	cfg := config.Default()
	finalize(rep, cfg, 1) // 有一个批次请求失败
	if rep.Pass {
		t.Error("存在失败批次时整体结论不应为通过")
	}
	if len(rep.Rerender) != 0 {
		t.Errorf("请求失败不应被计入重渲清单（那不是画面问题）: %v", rep.Rerender)
	}
	found := false
	for _, n := range rep.Notes {
		if len(n) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("应记录批次失败的说明")
	}
}

// TestFinalizeSortsByShotID 结论按镜头顺序排列，便于人工按序核查。
func TestFinalizeSortsByShotID(t *testing.T) {
	rep := &Report{Shots: []ShotReview{
		{ShotID: "sh010", Score: 9, Pass: true},
		{ShotID: "sh002", Score: 9, Pass: true},
		{ShotID: "sh001", Score: 9, Pass: true},
	}}
	finalize(rep, config.Default(), 0)
	want := []string{"sh001", "sh002", "sh010"}
	for i, id := range want {
		if rep.Shots[i].ShotID != id {
			t.Errorf("排序错误，第 %d 个应为 %s，实际 %s", i, id, rep.Shots[i].ShotID)
		}
	}
}

// TestWatermarkHitIsReported 高信心的叠字判定必须进结论，并给出可执行的重渲清单。
func TestWatermarkHitIsReported(t *testing.T) {
	rep := &Report{Shots: []ShotReview{
		{ShotID: "sh001", Score: 8, Pass: true},
		{ShotID: "sh002", Score: 8, Pass: true, Overlay: &Overlay{
			Present: true, Text: "AI生成", Location: "右下角", Confidence: 0.95}},
	}}
	finalize(rep, config.Default(), 0)

	if !rep.Watermark.Checked {
		t.Error("有复核结论时应标记为已检查")
	}
	if !rep.Watermark.Suspected {
		t.Fatal("高信心叠字判定必须被报出——水印是每一帧都在的交付硬伤")
	}
	if len(rep.Watermark.Shots) != 1 || rep.Watermark.Shots[0].ShotID != "sh002" {
		t.Errorf("命中清单错误: %+v", rep.Watermark.Shots)
	}
	if !strings.Contains(rep.Watermark.Note, "sh002") {
		t.Errorf("结论里应给出具体镜头号，实际: %s", rep.Watermark.Note)
	}
}

// TestLowConfidenceOverlayNotReported 低信心判定不进结论：
// 画面里本来就有的招牌/窗贴极易被误认成角标，误报会让人不再相信这条检查。
func TestLowConfidenceOverlayNotReported(t *testing.T) {
	rep := &Report{Shots: []ShotReview{
		{ShotID: "sh001", Score: 8, Pass: true, Overlay: &Overlay{
			Present: true, Text: "某招牌", Location: "右下角", Confidence: 0.4}},
	}}
	finalize(rep, config.Default(), 0)
	if rep.Watermark.Suspected {
		t.Errorf("信心 %.2f 低于门槛 %.2f，不应报出", 0.4, watermarkConfidenceLine)
	}
}

// TestNoOverlayIsNotSameAsNotChecked "没查"与"没有"必须可区分：
// 模型未按 schema 返回 overlay 时（常见于模型漏答），不能当成"确认无水印"。
func TestNoOverlayIsNotSameAsNotChecked(t *testing.T) {
	rep := &Report{Shots: []ShotReview{{ShotID: "sh001", Score: 8, Pass: true}}}
	finalize(rep, config.Default(), 0)
	if rep.Watermark.Suspected {
		t.Error("没有 overlay 数据时不应报出水印")
	}
	if !strings.Contains(rep.Watermark.Note, "未检出") {
		t.Errorf("无叠字时的措辞应明确是'未检出'，实际: %s", rep.Watermark.Note)
	}
	// 但 Checked 仍为 true：这一轮确实逐镜头问过了（模型答"没有"也算答了）
	if !rep.Watermark.Checked {
		t.Error("完成复核后 Checked 应为 true")
	}
}
