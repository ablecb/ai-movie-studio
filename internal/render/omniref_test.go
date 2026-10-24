package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/shotkit"
)

// 多模态参考生视频的参考图序与逐张声明回归：@imageN 编号必须与参考图序一一对应，
// 顺序即语义——角色身份锚 → 场景环境锚 → 本镜首帧构图锚。错位会把环境锚
// 当成"又一个角色"，声明与实际发出的图不一致等于没有声明。
func TestBuildOmniRefsDeclarationsMatchOrder(t *testing.T) {
	runDir := t.TempDir()
	writeFile := func(rel string) {
		t.Helper()
		p := filepath.Join(runDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("png-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("output/refs/hero.png")
	writeFile("output/refs/comic.png")
	writeFile("output/assets/scenes/s1.png")
	writeFile("output/shots/sh001.png")

	task := shotkit.Task{
		RefImages:     []string{"output/refs/hero.png", "output/refs/comic.png"},
		RefNames:      []string{"杨戬", "猪八戒"},
		SceneRefImage: "output/assets/scenes/s1.png",
	}
	refs, decls, err := buildOmniRefs(runDir, task, filepath.Join(runDir, "output", "shots", "sh001.png"))
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if len(refs) != 4 {
		t.Fatalf("应为 4 张参考图（2 角色 + 场景 + 首帧）: %d", len(refs))
	}
	for _, want := range []string{"@image1", "@image2", "@image3", "@image4"} {
		if !strings.Contains(decls, want) {
			t.Fatalf("声明缺 %s（编号必须连续且与图序对应）:\n%s", want, decls)
		}
	}
	if !strings.Contains(decls, "杨戬") || !strings.Contains(decls, "猪八戒") {
		t.Fatalf("角色声明应含角色名（张冠李戴比没有声明更糟）:\n%s", decls)
	}
	if !strings.Contains(decls, "establishing baseline") || !strings.Contains(decls, "approved opening frame") {
		t.Fatalf("场景锚/构图锚声明缺失:\n%s", decls)
	}
	// 每张参考图都必须以 data URL 形态就绪（声明里说了"附上"就必须真附上）
	for i, r := range refs {
		if !strings.HasPrefix(r, "data:image/") {
			t.Fatalf("第 %d 张参考图不是 data URL: %.40s", i+1, r)
		}
	}
}

// 无人物出镜的环境镜头：场景基准 + 首帧也能支撑参考模式；
// 三类锚全缺时必须显式失败（静默发出空参考列表会得到一次不受控的文生视频）。
func TestBuildOmniRefsEmptyIsExplicitError(t *testing.T) {
	_, _, err := buildOmniRefs(t.TempDir(), shotkit.Task{}, "")
	if err == nil || !strings.Contains(err.Error(), "至少一张参考图") {
		t.Fatalf("无任何参考图应显式失败: %v", err)
	}
}
