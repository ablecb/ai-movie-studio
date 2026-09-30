package vision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/promspec"
)

// TestBatchRefsDedupesByFirstAppearance 标准像按批次内角色首次出现排序、去重，
// 且只收"有参考帧"的角色。
func TestBatchRefsDedupesByFirstAppearance(t *testing.T) {
	shotOf := map[string]promspec.ShotSpec{
		"sh001": {ID: "sh001", Characters: []string{"林", "陈"}},
		"sh002": {ID: "sh002", Characters: []string{"林", "王"}},
	}
	refOf := map[string]string{"林": "/r/lin.png", "陈": "/r/chen.png"} // 王没有参考帧
	refs := batchRefs(shotOf, []string{"sh001", "sh002"}, refOf)
	if len(refs) != 2 {
		t.Fatalf("期望 2 张标准像（去重+跳过缺帧角色），实际 %d: %+v", len(refs), refs)
	}
	if refs[0].Name != "林" || refs[1].Name != "陈" {
		t.Errorf("应按首次出现排序: %+v", refs)
	}
}

// TestBuildPromptDeclaresRefOrder 有标准像时，提示词必须声明图序
// （标准像在前、镜头帧在后），否则模型无从知道每张图是什么。
func TestBuildPromptDeclaresRefOrder(t *testing.T) {
	shotOf := map[string]promspec.ShotSpec{
		"sh001": {ID: "sh001", Characters: []string{"林"}, Description: "中景：林抬头"},
	}
	framesOf := map[string][]string{"sh001": {"f1.jpg", "f2.jpg"}}
	refs := []refLabel{{Name: "林", Path: "/r/lin.png"}}

	p := buildPrompt(shotOf, []string{"sh001"}, framesOf, refs, 7.5)
	if !strings.Contains(p, "标准像") || !strings.Contains(p, "第 1 张图 = 角色「林」的标准像") {
		t.Errorf("提示词未声明标准像图序:\n%s", p)
	}
	if !strings.Contains(p, "【镜头 sh001】") {
		t.Errorf("提示词缺少镜头段:\n%s", p)
	}

	// 无标准像：不得出现标准像段（旧 run 的行为保持不变）
	p = buildPrompt(shotOf, []string{"sh001"}, framesOf, nil, 7.5)
	if strings.Contains(p, "标准像") {
		t.Errorf("无参考帧时不应出现标准像段:\n%s", p)
	}
}

// writeTestJSON 落盘一个测试 JSON 产物。
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

// TestLoadCharRefs 从 shotplan.json 建立映射；产物缺失时优雅返回空。
func TestLoadCharRefs(t *testing.T) {
	dir := t.TempDir()
	if got := loadCharRefs(dir); got != nil {
		t.Fatalf("缺 shotplan.json 时应返回 nil，实际 %v", got)
	}

	refRel := filepath.ToSlash(filepath.Join("output", "refs", "lin.png"))
	refAbs := filepath.Join(dir, filepath.FromSlash(refRel))
	if err := os.MkdirAll(filepath.Dir(refAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refAbs, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(dir, "output", "shotplan.json"), map[string]any{
		"characters": []map[string]any{
			{"name": "林", "ref_image": refRel},
			{"name": "陈", "ref_image": "output/refs/missing.png"}, // 文件不存在：跳过
		},
	})
	got := loadCharRefs(dir)
	if len(got) != 1 || got["林"] != refAbs {
		t.Fatalf("应只映射存在且文件在盘上的角色: %v", got)
	}
	if _, err := os.Stat(refAbs); err != nil {
		t.Fatal(err)
	}
}
