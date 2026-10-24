package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/shotkit"
)

// 场景基准图接入的回归：环境一致性靠"一张可逐像素比对的基准图"，
// 而不是提示词里的一句话。三条边界必须锁住：
//   - 参考图序：角色在前、场景在后（顺序即语义，错位会让模型把环境图当角色图）
//   - 素材库未生成 = 未提供：不算失败，不阻断渲染
//   - 索引声明了文件但文件不在盘上 = 损坏：显式失败，不静默丢锚

func TestPlannedRefImagesOrdersCharactersThenScene(t *testing.T) {
	task := shotkit.Task{
		RefImages:     []string{"output/refs/lin.png", "output/refs/chen.png"},
		SceneRefImage: "output/assets/scenes/s1.png",
	}
	refs := plannedRefImages(task)
	if len(refs) != 3 || refs[0] != "output/refs/lin.png" || refs[2] != "output/assets/scenes/s1.png" {
		t.Fatalf("场景基准必须排在全部角色参考帧之后: %v", refs)
	}
	// 无场景基准：行为与原实现一致（只有角色参考帧）
	if refs := plannedRefImages(shotkit.Task{RefImages: []string{"output/refs/lin.png"}}); len(refs) != 1 {
		t.Fatalf("无场景基准时不应多出参考图: %v", refs)
	}
	// 无人物出镜：场景基准就是唯一的参考图
	if refs := plannedRefImages(shotkit.Task{SceneRefImage: "output/assets/scenes/s1.png"}); len(refs) != 1 {
		t.Fatalf("无角色时场景基准应作为唯一参考图: %v", refs)
	}
}

// writeAssetIndex 落盘素材库索引（只写场景条目，够渲染回填用）。
func writeAssetIndex(t *testing.T, runDir string, ix shotkit.AssetIndex) {
	t.Helper()
	data, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runDir, "output", "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFillSceneRefsWithoutAssetsIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	tasks := []shotkit.Task{{ShotID: "sh001", SceneID: "s1"}}
	n, err := fillSceneRefs(dir, tasks)
	if err != nil {
		t.Fatalf("素材库未生成（无索引）应视为「未提供」而非失败: %v", err)
	}
	if n != 0 || tasks[0].SceneRefImage != "" {
		t.Fatalf("无素材时不应回填: n=%d task=%+v", n, tasks[0])
	}
}

func TestFillSceneRefsAnchorsShotsOfScene(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.ToSlash(filepath.Join("output", "assets", "scenes", "s1.png"))
	writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "png")
	writeAssetIndex(t, dir, shotkit.AssetIndex{Scenes: []shotkit.SceneAsset{
		{SceneID: "s1", File: rel},
		{SceneID: "s2", File: "output/assets/scenes/s2.png"}, // 文件缺失：该场次镜头应报错
	}})

	tasks := []shotkit.Task{
		{ShotID: "sh001", SceneID: "s1"},
		{ShotID: "sh002", SceneID: "s1"},
		{ShotID: "sh003", SceneID: "s3"}, // 索引里没有：保持未锚定
	}
	n, err := fillSceneRefs(dir, tasks)
	if err != nil {
		t.Fatalf("s1 的基准图在盘上，不应失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("同场次两个镜头都应被锚定，实际 %d", n)
	}
	for _, i := range []int{0, 1} {
		if tasks[i].SceneRefImage != rel {
			t.Errorf("镜头 %s 未回填基准图: %q", tasks[i].ShotID, tasks[i].SceneRefImage)
		}
	}
	if tasks[2].SceneRefImage != "" {
		t.Errorf("无场景素材的场次不应被回填: %q", tasks[2].SceneRefImage)
	}

	// 索引声明但文件被删：必须显式失败（静默丢锚 = 环境一致性悄悄失效）
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	if _, err := fillSceneRefs(dir, []shotkit.Task{{ShotID: "sh001", SceneID: "s1"}}); err == nil {
		t.Fatal("索引声明了基准图但文件丢失时应失败")
	} else if !strings.Contains(err.Error(), "场景基准图缺失") {
		t.Fatalf("失败信息应点明原因，实际: %v", err)
	}
}

// 环境状态绑定：声明了 scene_state 的镜头必须拿该状态变体当环境锚——
// 高潮段"云层散尽"却锚"雷云蔽日"，锚点会跟画面描述打架。
func TestFillSceneRefsUsesStateVariant(t *testing.T) {
	dir := t.TempDir()
	baseRel := filepath.ToSlash(filepath.Join("output", "assets", "scenes", "s4.png"))
	varRel := filepath.ToSlash(filepath.Join("output", "assets", "scenes", "s4_clouds-dispersed.png"))
	for _, rel := range []string{baseRel, varRel} {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "png")
	}
	writeAssetIndex(t, dir, shotkit.AssetIndex{Scenes: []shotkit.SceneAsset{
		{SceneID: "s4", File: baseRel},
		{SceneID: "s4", Variant: "clouds-dispersed", File: varRel},
	}})

	tasks := []shotkit.Task{
		{ShotID: "sh036", SceneID: "s4"},                                    // 初始状态：主基准
		{ShotID: "sh045", SceneID: "s4", SceneRefState: "clouds-dispersed"}, // 状态变体
		{ShotID: "sh046", SceneID: "s4", SceneRefState: "clouds-dispersed"}, // 同一状态：复用同一张
	}
	n, err := fillSceneRefs(dir, tasks)
	if err != nil {
		t.Fatalf("状态变体在盘上，不应失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("三个镜头都应被锚定，实际 %d", n)
	}
	if tasks[0].SceneRefImage != baseRel {
		t.Errorf("未声明状态的镜头应绑主基准，实际 %q", tasks[0].SceneRefImage)
	}
	if tasks[1].SceneRefImage != varRel || tasks[2].SceneRefImage != varRel {
		t.Errorf("声明状态变体的镜头应绑变体基准，实际 %q / %q", tasks[1].SceneRefImage, tasks[2].SceneRefImage)
	}

	// 声明了状态但索引里没有该变体：显式失败，不回退主基准——
	// 静默回退 = "这一镜的环境锚"悄悄指向另一个状态
	bad := []shotkit.Task{{ShotID: "sh047", SceneID: "s4", SceneRefState: "sky-cleared"}}
	if _, err := fillSceneRefs(dir, bad); err == nil {
		t.Fatal("状态变体缺失时应失败")
	} else if !strings.Contains(err.Error(), "没有该变体") {
		t.Fatalf("失败信息应点明原因，实际: %v", err)
	}
}
