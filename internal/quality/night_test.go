package quality

import (
	"image/color"
	"path/filepath"
	"testing"

	"aistudio/internal/config"
)

// 夜戏放行回归：压黑的夜戏是光比设计，不是生成失败——
// 这类画面曾按日景阈值被判"过暗"触发付费重渲，且重渲永远无法让它变亮。

// makeNoirImage 生成一张"noir 式压黑"的测试图：平均亮度 15（低于日景下限 18，
// 高于夜戏下限 8），且对比度与锐度足够（黑白棋盘，std≥14、拉普拉斯方差高），
// 不会落入"纯色/模糊"分支——要验证的只是亮度阈值这一个判据。
func makeNoirImage(t *testing.T, dir, name string) string {
	p := filepath.Join(dir, name)
	writePNG(t, p, 640, 360, func(x, y int) color.RGBA {
		// 2px 黑白棋盘：mean≈15（<日景下限 18，>夜戏下限 8），std≈15，
		// 且对 Sobel/拉普拉斯有响应（1px 棋盘频率太高会被算子对称抵消）
		v := uint8(((x/2 + y/2) % 2) * 30)
		return color.RGBA{v, v, v, 255}
	})
	return p
}

func TestNightSceneAllowsDarkNoirImage(t *testing.T) {
	dir := t.TempDir()
	p := makeNoirImage(t, dir, "noir.png")
	g := NewGate(config.Default().Quality)

	day := g.CheckImageScene(p, false, SceneCtx{})
	if day.OK {
		t.Fatalf("日景阈值下亮度 %.1f 应判过暗", day.Metrics.MeanLuma)
	}
	night := g.CheckImageScene(p, false, SceneCtx{Night: true})
	if !night.OK {
		t.Fatalf("夜戏下同样的画面不应判不合格: %v", night.Codes())
	}
}

func TestNightSceneSkipsFaceMissingCheck(t *testing.T) {
	// 肤色启发式在暗光/低饱和下不可靠：夜戏跳过人脸缺失判定（视觉复核接管）
	dir := t.TempDir()
	p := makeNoirImage(t, dir, "noir_face.png")
	g := NewGate(config.Default().Quality)

	night := g.CheckImageScene(p, true, SceneCtx{Night: true})
	for _, d := range night.Defects {
		if d.Code == "E_FACE_MISSING" {
			t.Fatal("夜戏不应触发人脸缺失判定（肤色启发式在暗光下不可靠）")
		}
	}
}
