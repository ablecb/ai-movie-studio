package deliver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/config"
)

// ---- 测试辅助 ----

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func configForMusicTest(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Music.LibraryDir = filepath.Join(t.TempDir(), "nolib") // 不存在的曲库目录
	cfg.Music.Generate = false
	return cfg
}

// 曲库按文件名标签自动打分选曲。
func TestPickMusicForCue(t *testing.T) {
	lib := []musicLibItem{
		{File: "library/music/悬疑_克制_60bpm_弦乐.wav", Tags: []string{"悬疑", "克制", "弦乐"}, Tempo: 60, DurationSec: 30},
		{File: "library/music/坚定_上扬_90bpm_圆号.wav", Tags: []string{"坚定", "上扬", "圆号"}, Tempo: 90, DurationSec: 20},
	}
	best, score := pickMusicForCue(lib, "悬疑克制", "第一幕", "低音弦乐+电子脉冲", 10)
	if score <= 0 || best.File != "library/music/悬疑_克制_60bpm_弦乐.wav" {
		t.Fatalf("应命中悬疑弦乐素材: %s score=%.2f", best.File, score)
	}
	if _, score := pickMusicForCue(lib, "欢快舞曲", "第三幕", "电子鼓点", 10); score > 0 {
		t.Fatalf("无相关标签时不应乱选: score=%.2f", score)
	}
}

// 文件名 → 标签/BPM 的解析规则（曲库约定：情绪_情绪_BPM_配器.wav）。
func TestBPMTokens(t *testing.T) {
	if m := reBPMToken.FindStringSubmatch("60bpm"); m == nil || m[1] != "60" {
		t.Fatalf("60bpm 应解析出 60: %v", m)
	}
	if m := reBPMToken.FindStringSubmatch("弦乐"); m != nil {
		t.Fatalf("非 BPM 词不应命中")
	}
}

// ensureMusic：已有素材的 cue 不重复供给；曲库无匹配时给出可执行的告警。
func TestEnsureMusic_AlreadySatisfied(t *testing.T) {
	dir := t.TempDir()
	// music.json：cue01 已有文件 → 不应产生任何告警
	writeTestJSON(t, dir+"/artifacts/music.json", map[string]any{
		"cue_sheet": []map[string]any{
			{"id": "cue01", "section": "第一幕", "mood": "悬疑", "tempo": "60 BPM", "instruments": "弦乐", "duration_sec": 10},
		},
	})
	writeTestJSON(t, dir+"/artifacts/narration.json", map[string]any{})
	if err := mkdirAll(dir + "/output/music"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(dir+"/output/music/cue01.wav", "x"); err != nil {
		t.Fatal(err)
	}
	cfg := configForMusicTest(t)
	warns, err := ensureMusic(nil, cfg, dir, nil, nil)
	if err != nil {
		t.Fatalf("已有素材时不应报错: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("cue01 已有素材不应告警: %v", warns)
	}
}

func TestEnsureMusic_MissingCueWarns(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, dir+"/artifacts/music.json", map[string]any{
		"cue_sheet": []map[string]any{
			{"id": "cue01", "section": "第一幕", "mood": "悬疑", "tempo": "60 BPM", "instruments": "弦乐", "duration_sec": 10},
		},
	})
	writeTestJSON(t, dir+"/artifacts/narration.json", map[string]any{})
	if err := mkdirAll(dir + "/output/music"); err != nil {
		t.Fatal(err)
	}
	cfg := configForMusicTest(t)
	warns, err := ensureMusic(nil, cfg, dir, nil, nil)
	if err != nil {
		t.Fatalf("缺素材是缺输入不是错误，不应报错: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("缺素材应产生 1 条告警: %v", warns)
	}
	if !contains(warns[0], "cue01") {
		t.Fatalf("告警应指明 cue: %s", warns[0])
	}
}
