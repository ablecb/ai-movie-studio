package quality

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"aistudio/internal/config"
)

// writePNG 生成测试图（确定性，不依赖外部素材）。
func writePNG(t *testing.T, path string, w, h int, fn func(x, y int) color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, fn(x, y))
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestCheckImageRejectsFlatColor(t *testing.T) {
	// 纯色画面：亮度标准差 ≈ 0，是"生成失败"的典型形态，必须拦住
	dir := t.TempDir()
	p := filepath.Join(dir, "flat.png")
	writePNG(t, p, 640, 360, func(x, y int) color.RGBA {
		return color.RGBA{128, 128, 128, 255}
	})
	g := NewGate(config.Default().Quality)
	v := g.CheckImage(p, false)
	if v.OK {
		t.Fatal("纯色画面应被判为不合格")
	}
	if v.Severity != SeverityBlock {
		t.Errorf("纯色画面应为阻断级（block），实际 %s", v.Severity)
	}
}

func TestCheckImageRejectsTooDark(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dark.png")
	writePNG(t, p, 640, 360, func(x, y int) color.RGBA {
		// 极暗但非纯色（加一点噪声，避免落入"纯色"分支）
		v := uint8(4 + (x+y)%3)
		return color.RGBA{v, v, v, 255}
	})
	g := NewGate(config.Default().Quality)
	v := g.CheckImage(p, false)
	if v.OK {
		t.Fatal("过暗画面应被判为不合格")
	}
}

func TestCheckImageAcceptsDetailedImage(t *testing.T) {
	// 有结构、有细节、亮度适中的图应当通过
	dir := t.TempDir()
	p := filepath.Join(dir, "good.png")
	writePNG(t, p, 640, 360, func(x, y int) color.RGBA {
		// 棋盘 + 渐变：产生足够的边缘与亮度方差
		base := uint8(60)
		if (x/8+y/8)%2 == 0 {
			base = 190
		}
		grad := uint8((x * 40) / 640)
		return color.RGBA{base, uint8(int(base)/2 + int(grad)), uint8(90 + int(grad)/2), 255}
	})
	g := NewGate(config.Default().Quality)
	v := g.CheckImage(p, false)
	if !v.OK {
		t.Fatalf("有结构的图应通过质检，实际缺陷: %v（指标 %+v）", v.Defects, v.Metrics)
	}
	if v.Metrics.Sharpness <= 0 {
		t.Error("清晰度指标应大于 0")
	}
}

func TestCheckImageMissingFaceWhenExpected(t *testing.T) {
	// 该有角色出镜却没有肤色区域 → 说明人物没画出来，必须报出
	dir := t.TempDir()
	p := filepath.Join(dir, "noface.png")
	writePNG(t, p, 640, 360, func(x, y int) color.RGBA {
		base := uint8(50)
		if (x/6+y/6)%2 == 0 {
			base = 200
		}
		return color.RGBA{base, base, uint8(255 - int(base)), 255}
	})
	g := NewGate(config.Default().Quality)
	v := g.CheckImage(p, true) // hasExpectedFace = true
	found := false
	for _, d := range v.Defects {
		if d.Code == "E_FACE_MISSING" {
			found = true
		}
	}
	if !found {
		t.Errorf("应报出 E_FACE_MISSING，实际缺陷: %v", v.Codes())
	}
}

func TestAnalyzeImageUnreadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broken.png")
	if err := os.WriteFile(p, []byte("this is not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewGate(config.Default().Quality)
	v := g.CheckImage(p, false)
	if v.OK || v.Severity != SeverityBlock {
		t.Errorf("损坏文件应为阻断级失败，实际 OK=%v severity=%s", v.OK, v.Severity)
	}
}

func TestSortDefectsPutsBlockingFirst(t *testing.T) {
	ds := []Defect{
		{Code: "A", Severity: SeverityRerender},
		{Code: "B", Severity: SeverityBlock},
		{Code: "C", Severity: SeverityOK},
	}
	got := SortDefects(ds)
	if got[0].Code != "B" {
		t.Errorf("阻断级缺陷应排在最前，实际顺序: %v", got)
	}
}

func TestWorst(t *testing.T) {
	if Worst(SeverityOK, SeverityRerender) != SeverityRerender {
		t.Error("Worst 应返回 rerender")
	}
	if Worst(SeverityRerender, SeverityBlock, SeverityOK) != SeverityBlock {
		t.Error("Worst 应返回 block")
	}
	// warn 只应压过 ok，不应压过 rerender：否则真实的随机性缺陷会被降级成"仅告警"而漏渲
	if Worst(SeverityOK, SeverityWarn) != SeverityWarn {
		t.Error("Worst 应返回 warn")
	}
	if Worst(SeverityWarn, SeverityRerender) != SeverityRerender {
		t.Error("rerender 不应被 warn 压过")
	}
}

// 时长漂移必须只判 warn：它是模型固定片长造成的，换种重渲不会改变结果，
// 判成 rerender 会让每一次渲染都陷入"重渲→同样漂移→再重渲"的付费循环。
func TestDurationDriftDoesNotTriggerRerender(t *testing.T) {
	if SeverityWarn == SeverityRerender {
		t.Fatal("warn 与 rerender 不能是同一个值")
	}
	// 只 warn 的镜头不得被判定为需要重渲（渲染循环据此决定是否再花钱）
	if Worst(SeverityWarn) == SeverityRerender {
		t.Fatal("仅 warn 时不得要求重渲")
	}
	if Worst(SeverityOK, SeverityWarn) == SeverityRerender {
		t.Fatal("warn 不得升级为 rerender")
	}
	// 但 warn 必须比 ok 重，否则时长漂移会被静默吞掉
	if Worst(SeverityOK, SeverityWarn) == SeverityOK {
		t.Fatal("warn 必须高于 ok，不能被静默吞掉")
	}
}

func TestLaplacianVarianceDetectsBlur(t *testing.T) {
	// 清晰棋盘 vs 均匀灰：清晰度必须显著更高
	w, h := 64, 64
	sharp := make([]float64, w*h)
	flat := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%2 == 0 {
				sharp[y*w+x] = 240
			} else {
				sharp[y*w+x] = 15
			}
			flat[y*w+x] = 128
		}
	}
	vs, vf := laplacianVariance(sharp, w, h), laplacianVariance(flat, w, h)
	if vs <= vf*10 {
		t.Errorf("清晰图方差 %.1f 应远大于平坦图 %.1f", vs, vf)
	}
	if vf > 0.001 {
		t.Errorf("纯色图拉普拉斯方差应为 0，实际 %.4f", vf)
	}
}

func TestIsSkinTone(t *testing.T) {
	cases := []struct {
		r, g, b float64
		want    bool
	}{
		{220, 170, 140, true},  // 典型肤色
		{128, 128, 128, false}, // 灰
		{30, 60, 200, false},   // 蓝
		{40, 200, 60, false},   // 绿
		{250, 250, 250, false}, // 白
	}
	for _, c := range cases {
		if got := isSkinTone(c.r, c.g, c.b); got != c.want {
			t.Errorf("isSkinTone(%.0f,%.0f,%.0f)=%v，期望 %v", c.r, c.g, c.b, got, c.want)
		}
	}
}

func TestLargestComponent(t *testing.T) {
	// 两个连通块：大的 6 格，小的 1 格
	w, h := 5, 3
	mask := make([]bool, w*h)
	for _, i := range []int{0, 1, 2, 5, 6, 7} {
		mask[i] = true
	}
	mask[14] = true
	ratio := largestComponent(mask, w, h)
	want := 6.0 / float64(w*h)
	if math.Abs(ratio-want) > 1e-9 {
		t.Errorf("最大连通域占比 %.4f，期望 %.4f", ratio, want)
	}
}

var _ = fmt.Sprintf
