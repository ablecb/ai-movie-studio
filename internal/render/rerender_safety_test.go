package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/quality"
)

// 重渲产物保护回归：重渲失败绝不能销毁上一轮的合格产物（已付费资产），
// 重渲成功必须真的换一次结果（含首帧图）。

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(data)
}

func TestSetAsideAndRestore(t *testing.T) {
	dir := t.TempDir()
	vid := filepath.Join(dir, "sh001.mp4")
	writeFile(t, vid, "good-video-round0")

	aside := setAside(vid)
	if aside == "" {
		t.Fatal("文件存在时 setAside 应返回暂存路径")
	}
	if _, err := os.Stat(vid); !os.IsNotExist(err) {
		t.Fatal("setAside 后原路径不应再有文件")
	}
	if readFile(t, aside) != "good-video-round0" {
		t.Fatal("暂存文件内容不一致")
	}

	// 本轮失败：新产物不存在 → 暂存恢复原位
	restoreAside(vid, aside, filepath.Join(dir, "invalid"), "sh001", 1, "vid")
	if readFile(t, vid) != "good-video-round0" {
		t.Fatal("restoreAside 未把上一轮产物恢复原位")
	}
}

func TestRestoreAsideQuarantinesNewArtifact(t *testing.T) {
	dir := t.TempDir()
	vid := filepath.Join(dir, "sh001.mp4")
	invalidDir := filepath.Join(dir, "invalid")
	// renderShot 入口会建 invalid/；quarantine 只做改名（目录必须已存在）
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, vid, "good-previous")
	aside := setAside(vid)
	writeFile(t, vid, "bad-new-take") // 本轮生成了坏产物

	restoreAside(vid, aside, invalidDir, "sh001", 1, "vid")

	if readFile(t, vid) != "good-previous" {
		t.Fatal("失败收尾后原位应是上一轮合格产物")
	}
	matches, _ := filepath.Glob(filepath.Join(invalidDir, "sh001_r1_vid*"))
	if len(matches) != 1 {
		t.Fatalf("本轮坏产物应被隔离留证，invalid/ 实际: %v", matches)
	}
}

func TestRetireAsideKeepsEvidence(t *testing.T) {
	dir := t.TempDir()
	invalidDir := filepath.Join(dir, "invalid")
	if err := os.MkdirAll(invalidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(dir, "sh001.mp4.prev")
	writeFile(t, aside, "old-take")

	retireAside(aside, invalidDir, "sh001", 2, "vid")

	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatal("退役暂存文件应被移走")
	}
	matches, _ := filepath.Glob(filepath.Join(invalidDir, "sh001_r2_prev_vid*"))
	if len(matches) != 1 {
		t.Fatalf("上一轮 take 应留档到 invalid/ 供对比复盘: %v", matches)
	}
}

func TestMergeRecordKeepsDoneOnFailure(t *testing.T) {
	prev := ShotRecord{ID: "sh001", Status: "done", Video: "output/shots/sh001.mp4", ActualDur: 5.1}
	cur := ShotRecord{
		ID: "sh001", Status: "failed", Error: "图生视频失败", Severity: quality.SeverityBlock,
		Rounds: []RoundRecord{{Round: 1, Error: "图生视频失败"}},
	}

	merged := mergeRecord(prev, cur)
	if merged.Status != "done" || merged.Video == "" {
		t.Fatal("重渲失败时 mergeRecord 必须保留 done 状态与旧视频路径（文件由 restoreAside 恢复）")
	}
	if len(merged.Rounds) != 1 {
		t.Fatalf("失败轮次应留痕，实际 %d 轮", len(merged.Rounds))
	}
}

func TestMergeRecordKeepsImaged(t *testing.T) {
	prev := ShotRecord{ID: "sh001", Status: "pending"}
	cur := ShotRecord{
		ID: "sh001", Status: "imaged", Image: "output/shots/sh001.png",
		ImageMetrics: &quality.ImageVerdict{OK: true},
		Rounds:       []RoundRecord{{Round: 0}},
	}

	merged := mergeRecord(prev, cur)
	if merged.Status != "imaged" || merged.Image == "" {
		t.Fatal("--images-only 的 imaged 状态与首帧路径必须落账，否则 manifest 全记 pending、镜头被误判缺失")
	}
	if merged.Video != "" {
		t.Fatal("imaged 没有视频，不应写入 Video")
	}
}

func TestVariantForCyclesBeyondList(t *testing.T) {
	base := "cinematic shot"
	seen := map[string]bool{}
	for round := 1; round <= len(roundVariants)*2; round++ {
		v := variantFor(base, round, 990)
		if v == base {
			t.Fatalf("第 %d 轮应追加变体（不能悄悄重跑同一提示词）", round)
		}
		seen[v] = true
		if !strings.HasPrefix(v, base) {
			t.Fatalf("变体应附加在原提示词之后: %s", v)
		}
	}
	if len(seen) < 2 {
		t.Fatal("多轮变体应产生至少两种不同的提示词")
	}
	// 超长保护：加不下变体时原样返回
	long := strings.Repeat("x", 505)
	if got := variantFor(long, 1, 500); got != long {
		t.Fatal("超出提示词上限时应原样返回")
	}
}
