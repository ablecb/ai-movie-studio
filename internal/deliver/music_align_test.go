package deliver

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/narrate"
	"aistudio/internal/promspec"
)

// makeTestWAV 生成一段真实可探测时长的 wav（无 ffmpeg 环境跳过）。
func makeTestWAV(t *testing.T, path string, sec float64) {
	t.Helper()
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力（跳过）: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ffx.Run(60*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", path); err != nil {
		t.Skipf("无法生成测试音频（跳过）: %v", err)
	}
}

// buildAlignRunDir 构造最小 run 目录：2 场景 3 镜头 + 已投喂的 cue 素材。
// 实测镜头槽：sh001(s1) 0-3s，sh002(s1) 3-7s，sh003(s2) 7-12s。
func buildAlignRunDir(t *testing.T) (string, *promspec.Kit, *narrate.Plan) {
	t.Helper()
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "artifacts", "music.json"), map[string]any{
		"cue_sheet": []map[string]any{
			{"id": "cue01", "section": "第二幕", "scene_id": "s2", "mood": "上扬", "duration_sec": 2},
			{"id": "cue02", "section": "尾声", "mood": "释然", "duration_sec": 2}, // 旧方案：无 scene_id
		},
	})
	kit := &promspec.Kit{ShotList: []promspec.ShotSpec{
		{ID: "sh001", SceneID: "s1", DurationSec: 3},
		{ID: "sh002", SceneID: "s1", DurationSec: 4},
		{ID: "sh003", SceneID: "s2", DurationSec: 5},
	}}
	writeTestJSON(t, filepath.Join(dir, "artifacts", "shotspec.json"), kit)
	plan := &narrate.Plan{TotalSec: 12, Shots: []narrate.ShotSlot{
		{ShotID: "sh001", Start: 0, Duration: 3},
		{ShotID: "sh002", Start: 3, Duration: 4},
		{ShotID: "sh003", Start: 7, Duration: 5},
	}}
	makeTestWAV(t, filepath.Join(dir, "output", "music", "cue01.wav"), 2)
	makeTestWAV(t, filepath.Join(dir, "output", "music", "cue02.wav"), 2)
	return dir, kit, plan
}

// cue 绑定了 scene_id 时，必须按实测镜头槽对齐到**所属场景的起止**，
// 而不是从 0 顺序铺——配乐的情绪段与画面段重合是"按剧本配乐"的全部意义。
func TestScanMusicAlignsCueToScene(t *testing.T) {
	dir, kit, plan := buildAlignRunDir(t)
	cfg := config.Default()

	music, sfx := scanMusicAndSFX(dir, plan, kit, cfg)
	if len(music) != 2 {
		t.Fatalf("期望 2 条配乐轨，实际 %d", len(music))
	}
	// cue01 → s2：起点=场景首镜（sh003）实测入点 7s，终点=场景末镜出点 12s
	if music[0].StartSec != 7 || music[0].EndSec != 12 {
		t.Errorf("cue01 应对齐 s2（7→12s），实际 %.2f→%.2f", music[0].StartSec, music[0].EndSec)
	}
	// cue02 无 scene_id（旧方案文件）：退回顺序铺，排在最后一条已对齐 cue 之后
	if music[1].StartSec != 12 || music[1].EndSec != 0 {
		t.Errorf("cue02 应顺序铺在 12s，实际 %.2f（EndSec=%v）", music[1].StartSec, music[1].EndSec)
	}
	if len(sfx) != 0 {
		t.Errorf("不应有音效轨: %d", len(sfx))
	}
}

// 音效必须精确挂在所属镜头的实测起点（既有行为的回归保护）。
func TestScanMusicSFXBindsToShotSlot(t *testing.T) {
	dir, kit, plan := buildAlignRunDir(t)
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力（跳过）: %v", err)
	}
	makeTestWAV(t, filepath.Join(dir, "output", "sfx", "sh003.sfx.wav"), 1)
	cfg := config.Default()
	_, sfx := scanMusicAndSFX(dir, plan, kit, cfg)
	if len(sfx) != 1 {
		t.Fatalf("期望 1 条音效轨，实际 %d", len(sfx))
	}
	if sfx[0].StartSec != 7 {
		t.Errorf("sh003 的音效应挂在实测起点 7s，实际 %.2f", sfx[0].StartSec)
	}
}

// 旧方案文件（cue 无 scene_id）必须保持旧行为：从 0 顺序铺，不能因缺字段而丢配乐。
func TestScanMusicFallsBackToSequential(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "artifacts", "music.json"), map[string]any{
		"cue_sheet": []map[string]any{
			{"id": "cue01", "mood": "悬疑", "duration_sec": 2},
		},
	})
	kit := &promspec.Kit{ShotList: []promspec.ShotSpec{{ID: "sh001", SceneID: "s1", DurationSec: 3}}}
	writeTestJSON(t, filepath.Join(dir, "artifacts", "shotspec.json"), kit)
	plan := &narrate.Plan{TotalSec: 3, Shots: []narrate.ShotSlot{{ShotID: "sh001", Start: 0, Duration: 3}}}
	makeTestWAV(t, filepath.Join(dir, "output", "music", "cue01.wav"), 2)

	music, _ := scanMusicAndSFX(dir, plan, kit, config.Default())
	if len(music) != 1 || music[0].StartSec != 0 || music[0].EndSec != 0 {
		t.Fatalf("无 scene_id 的 cue 应顺序铺在 0s 且不截断: %+v", music)
	}
}
