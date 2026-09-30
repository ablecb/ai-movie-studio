// Package quality 实现客观技术质检：把"这张图/这个镜头能不能用"从主观判断
// 变成可复核的数值。
//
// 为什么必须客观化：主观审片（让模型看图打分）会漏掉确定性缺陷——纯黑帧、糊成一团、
// 人脸占比畸大（典型的 AI 畸形脸信号）。这些用信号处理几十毫秒就能判定，
// 而且判定结果可复现、可写进日志、可作交付依据。
//
// 判定三态（Severity）：
//   - ok    通过
//   - rerender 可修复缺陷（换种子重渲，不阻断交付）
//   - block   不可交付缺陷（必须补拍，绝不允许静默放行）
package quality

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aistudio/internal/config"
	"aistudio/internal/ffx"
)

// Severity 缺陷严重度。
type Severity string

const (
	SeverityOK   Severity = "ok"
	SeverityWarn Severity = "warn" // 已知且不可通过重渲修复；必须由下游按实测值吸收
	// SeverityRerender 换种子重渲即可修复。**判定的前提是该缺陷确实由随机性造成**：
	// 若某个模型对同一输入恒定产出同一结果（例如固定片长），换种子重渲只会重复付费。
	SeverityRerender Severity = "rerender"
	SeverityBlock    Severity = "block" // 必须补拍，不可静默放行
)

// Defect 一条明确缺陷。Code 是稳定标识（便于统计与自动化），Message 给人看。
type Defect struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
	Metric   float64  `json:"metric"`
	Limit    float64  `json:"limit"`
}

// ImageMetrics 图像客观指标。
type ImageMetrics struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	MeanLuma    float64 `json:"mean_luma"`    // 平均亮度 0-255
	StdLuma     float64 `json:"std_luma"`     // 亮度标准差（纯色/纯黑检测）
	Sharpness   float64 `json:"sharpness"`    // 拉普拉斯方差（模糊检测）
	ClippedHigh float64 `json:"clipped_high"` // 过曝像素占比
	ClippedLow  float64 `json:"clipped_low"`  // 死黑像素占比
	FaceRatio   float64 `json:"face_ratio"`   // 最大肤色连通区域占画面比例
	SkinRatio   float64 `json:"skin_ratio"`   // 肤色像素总占比
	EdgeDensity float64 `json:"edge_density"` // 边缘密度（糊成一团的检测）
	// MeanSaturation 平均饱和度（HSV S，0-1）。糖水片（重散景+青橙过饱和）是
	// AI 出图的默认审美，也是"一眼 AI"的最大来源——饱和度是它最稳定的信号。
	MeanSaturation float64 `json:"mean_saturation"`
}

// ImageVerdict 图像质检结论。
type ImageVerdict struct {
	Path     string       `json:"path"`
	OK       bool         `json:"ok"`
	Severity Severity     `json:"severity"`
	Metrics  ImageMetrics `json:"metrics"`
	Defects  []Defect     `json:"defects"`
}

// Codes 返回缺陷码列表（便于日志与统计）。
func (v ImageVerdict) Codes() []string {
	out := make([]string, 0, len(v.Defects))
	for _, d := range v.Defects {
		out = append(out, d.Code)
	}
	return out
}

// String 人类可读摘要。
func (v ImageVerdict) String() string {
	if v.OK {
		return fmt.Sprintf("%s: 通过 (清晰度=%.0f 亮度=%.0f 肤色=%.2f%%)",
			filepath.Base(v.Path), v.Metrics.Sharpness, v.Metrics.MeanLuma, v.Metrics.FaceRatio*100)
	}
	var parts []string
	for _, d := range v.Defects {
		parts = append(parts, d.Message)
	}
	return fmt.Sprintf("%s: %s —— %s", filepath.Base(v.Path), v.Severity, strings.Join(parts, "；"))
}

// Gate 质检门禁。
type Gate struct {
	cfg config.QualityGate
}

// NewGate 构造门禁。
func NewGate(cfg config.QualityGate) *Gate { return &Gate{cfg: cfg} }

// SceneCtx 告知质检该镜头所处的场景语境。阈值是按"日常日景"标定的，
// 不带语境就会误杀合法画面：夜戏/黑色电影的压暗黑位是**光比设计**，
// 不是生成失败——误判的代价是付费重渲循环（重渲永远不会让它变亮）。
type SceneCtx struct {
	// Night 夜戏（time_of_day 为 夜/深夜/night 等）。放行项：
	//  - 亮度下限放宽到 8（noir 式压黑合法）
	//  - 跳过"人脸缺失"判定（肤色启发式在暗光/低饱和调色下不可靠）
	//  - 视频黑帧占比上限放宽（画面里大面积阴影是正常的）
	Night bool
}

// nightMinLuma 夜戏允许的最低平均亮度。
const nightMinLuma = 8

// CheckImage 对单张图做技术质检。
// hasExpectedFace=true 时启用"人脸占比下限"检查：该有人脸的镜头没有人脸，
// 通常意味着生成失败或人物被画成了别的东西（比模糊更严重的可用性问题）。
func (g *Gate) CheckImage(path string, hasExpectedFace bool) ImageVerdict {
	return g.CheckImageScene(path, hasExpectedFace, SceneCtx{})
}

// CheckImageScene 带场景语境的图像质检（夜戏阈值放行，见 SceneCtx）。
func (g *Gate) CheckImageScene(path string, hasExpectedFace bool, sc SceneCtx) ImageVerdict {
	v := ImageVerdict{Path: path, OK: true, Severity: SeverityOK}
	m, err := AnalyzeImage(path)
	if err != nil {
		v.OK = false
		v.Severity = SeverityBlock
		v.Defects = append(v.Defects, Defect{
			Code: "E_IMAGE_UNREADABLE", Severity: SeverityBlock,
			Message: "图像无法解码（文件损坏或非图片）: " + err.Error(),
		})
		return v
	}
	v.Metrics = *m

	add := func(code string, sev Severity, metric, limit float64, msg string) {
		v.OK = false
		if sev == SeverityBlock || v.Severity != SeverityBlock {
			v.Severity = sev
		}
		v.Defects = append(v.Defects, Defect{Code: code, Severity: sev, Metric: metric, Limit: limit, Message: msg})
	}

	if m.Width < 512 || m.Height < 288 {
		add("E_IMAGE_TOO_SMALL", SeverityBlock, float64(m.Width), 512,
			fmt.Sprintf("分辨率过低（%dx%d），无法满足交付画质", m.Width, m.Height))
	}
	// 亮度下限：夜戏放宽到 nightMinLuma（压黑是设计，不是失败）
	minLuma := g.cfg.MinMeanLuma
	if sc.Night && minLuma > nightMinLuma {
		minLuma = nightMinLuma
	}
	if m.MeanLuma < minLuma {
		add("E_IMAGE_TOO_DARK", SeverityRerender, m.MeanLuma, minLuma,
			fmt.Sprintf("画面过暗（平均亮度 %.1f < %.1f），疑似生成失败", m.MeanLuma, minLuma))
	}
	if m.MeanLuma > g.cfg.MaxMeanLuma {
		add("E_IMAGE_OVEREXPOSED", SeverityRerender, m.MeanLuma, g.cfg.MaxMeanLuma,
			fmt.Sprintf("画面过曝（平均亮度 %.1f > %.1f）", m.MeanLuma, g.cfg.MaxMeanLuma))
	}
	if m.StdLuma < g.cfg.MinStdLuma {
		add("E_IMAGE_FLAT", SeverityBlock, m.StdLuma, g.cfg.MinStdLuma,
			fmt.Sprintf("画面近乎纯色（亮度标准差 %.1f），不是有效画面", m.StdLuma))
	}
	if m.Sharpness < g.cfg.MinSharpness {
		add("E_IMAGE_BLURRY", SeverityRerender, m.Sharpness, g.cfg.MinSharpness,
			fmt.Sprintf("画面模糊（清晰度 %.1f < %.1f），AI 出图糊化是典型失败模式", m.Sharpness, g.cfg.MinSharpness))
	}
	if m.EdgeDensity < 0.008 && m.StdLuma >= g.cfg.MinStdLuma {
		add("E_IMAGE_NO_DETAIL", SeverityRerender, m.EdgeDensity, 0.008,
			"画面缺少结构细节，疑似大面积涂抹/糊化")
	}
	if g.cfg.MaxFaceRatio > 0 && m.FaceRatio > g.cfg.MaxFaceRatio {
		add("E_FACE_OVERSIZED", SeverityRerender, m.FaceRatio, g.cfg.MaxFaceRatio,
			fmt.Sprintf("人脸占画面 %.0f%%（上限 %.0f%%），构图畸变，AI 畸形脸的典型特征",
				m.FaceRatio*100, g.cfg.MaxFaceRatio*100))
	}
	// 夜戏跳过人脸缺失判定：肤色启发式要求足够的光照与饱和度，
	// 夜景/低照度调色下的合法画面会被系统性误判（假阳性 → 付费重渲且永远过不了）。
	// 夜戏里"角色在不在画面里"交给视觉复核（它看的是骨相，不是肤色）。
	if hasExpectedFace && !sc.Night && g.cfg.MinFaceRatio > 0 && m.FaceRatio < g.cfg.MinFaceRatio {
		add("E_FACE_MISSING", SeverityRerender, m.FaceRatio, g.cfg.MinFaceRatio,
			"该镜头应有角色出镜，但画面中检测不到人脸，疑似生成失败")
	}
	// 糖水片拦截：重散景 + 青橙过饱和是 AI 出图默认审美，饱和度是最稳定信号。
	// 夜戏放宽（霓虹/夜景灯光天然高饱和），0 = 关闭该检查。
	if g.cfg.MaxMeanSaturation > 0 {
		maxSat := g.cfg.MaxMeanSaturation
		if sc.Night {
			maxSat += 0.10
		}
		if m.MeanSaturation > maxSat {
			add("E_IMAGE_OVERSATURATED", SeverityRerender, m.MeanSaturation, maxSat,
				fmt.Sprintf("画面过饱和（%.2f > %.2f），糖水片质感是\"一眼 AI\"的最大来源；建议检查调色词与负面词", m.MeanSaturation, maxSat))
		}
	}
	return v
}

// AnalyzeImage 计算图像客观指标（纯 Go 解码，无外部依赖）。
func AnalyzeImage(path string) (*ImageMetrics, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("图像尺寸非法: %dx%d", w, h)
	}

	// 灰度（ITU-R BT.601）+ 降采样步长（质检不需要全分辨率，速度优先）
	step := 1
	for (w/step)*(h/step) > 640*360 {
		step++
	}
	gw, gh := w/step, h/step
	gray := make([]float64, gw*gh)
	var sum, sum2, satSum float64
	high, low := 0, 0
	skin := make([]bool, gw*gh)
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			r, gg, bb, _ := img.At(b.Min.X+x*step, b.Min.Y+y*step).RGBA()
			rf, gf, bf := float64(r>>8), float64(gg>>8), float64(bb>>8)
			lum := 0.299*rf + 0.587*gf + 0.114*bf
			gray[y*gw+x] = lum
			sum += lum
			sum2 += lum * lum
			if lum > 250 {
				high++
			}
			if lum < 6 {
				low++
			}
			skin[y*gw+x] = isSkinTone(rf, gf, bf)
			// HSV 饱和度 S = (max-min)/max：糖水片检测信号
			mx := math.Max(rf, math.Max(gf, bf))
			if mx > 0 {
				satSum += (mx - math.Min(rf, math.Min(gf, bf))) / mx
			}
		}
	}
	n := float64(len(gray))
	mean := sum / n
	variance := sum2/n - mean*mean
	if variance < 0 {
		variance = 0
	}

	m := &ImageMetrics{
		Width: w, Height: h,
		MeanLuma:       mean,
		StdLuma:        math.Sqrt(variance),
		ClippedHigh:    float64(high) / n,
		ClippedLow:     float64(low) / n,
		MeanSaturation: satSum / n,
	}
	m.Sharpness = laplacianVariance(gray, gw, gh)
	m.EdgeDensity = edgeDensity(gray, gw, gh)

	// 肤色连通域：最大连通块占比 ≈ 人脸占比
	skinCount := 0
	for _, s := range skin {
		if s {
			skinCount++
		}
	}
	m.SkinRatio = float64(skinCount) / n
	m.FaceRatio = largestComponent(skin, gw, gh) / n
	return m, nil
}

// isSkinTone 归一化 RGB 肤色判定。
//
// 关键点：皮肤的红通道一定是最强通道，因此归一化后 nr 恒为 1.0——
// 对 nr 设上下限是无效检查（真实肤色的 nr 必然是 1.0，会被误杀）。
// 有效的判据是**绿蓝相对红的比例区间**，加上经典的 RGB 肤色不等式。
func isSkinTone(r, g, b float64) bool {
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	if mx <= 0 || mx-mn < 15 {
		return false // 灰阶/低饱和不算肤色（黑白片、水泥墙）
	}
	ng, nb := g/mx, b/mx
	if !(ng > 0.42 && ng < 0.92) {
		return false
	}
	if !(nb > 0.20 && nb < 0.78) {
		return false
	}
	// 必须红通道最强，且红绿差明显（排除灰白与偏青材质）
	return r > 95 && g > 40 && b > 20 && r > g && r > b && (r-g) >= 15 && (g-b) >= 0
}

// laplacianVariance 拉普拉斯方差：图像清晰度的标准度量（值越低越糊）。
func laplacianVariance(gray []float64, w, h int) float64 {
	if w < 3 || h < 3 {
		return 0
	}
	var sum, sum2 float64
	var n float64
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			lap := 4*gray[i] - gray[i-1] - gray[i+1] - gray[i-w] - gray[i+w]
			sum += lap
			sum2 += lap * lap
			n++
		}
	}
	if n == 0 {
		return 0
	}
	mean := sum / n
	v := sum2/n - mean*mean
	if v < 0 {
		return 0
	}
	return v
}

// edgeDensity 边缘密度：Sobel 幅值超过阈值的像素占比。
func edgeDensity(gray []float64, w, h int) float64 {
	if w < 3 || h < 3 {
		return 0
	}
	thresh := 28.0
	edges, n := 0, 0
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			gx := -gray[i-w-1] - 2*gray[i-1] - gray[i+w-1] + gray[i-w+1] + 2*gray[i+1] + gray[i+w+1]
			gy := -gray[i-w-1] - 2*gray[i-w] - gray[i-w+1] + gray[i+w-1] + 2*gray[i+w] + gray[i+w+1]
			if math.Hypot(gx, gy) > thresh {
				edges++
			}
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(edges) / float64(n)
}

// largestComponent 二值图最大连通域占比（4 邻域）
func largestComponent(mask []bool, w, h int) float64 {
	if len(mask) == 0 {
		return 0
	}
	visited := make([]bool, len(mask))
	best := 0
	stack := make([]int, 0, 256)
	for start := range mask {
		if !mask[start] || visited[start] {
			continue
		}
		count := 0
		stack = stack[:0]
		stack = append(stack, start)
		visited[start] = true
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			count++
			y, x := i/w, i%w
			if x > 0 && mask[i-1] && !visited[i-1] {
				visited[i-1] = true
				stack = append(stack, i-1)
			}
			if x < w-1 && mask[i+1] && !visited[i+1] {
				visited[i+1] = true
				stack = append(stack, i+1)
			}
			if y > 0 && mask[i-w] && !visited[i-w] {
				visited[i-w] = true
				stack = append(stack, i-w)
			}
			if y < h-1 && mask[i+w] && !visited[i+w] {
				visited[i+w] = true
				stack = append(stack, i+w)
			}
		}
		if count > best {
			best = count
		}
	}
	return float64(best) / float64(len(mask))
}

// ---------------------------------------------------------------------------
// 视频质检
// ---------------------------------------------------------------------------

// FrameSample 单帧客观指标。
type FrameSample struct {
	AtSec     float64 `json:"at_sec"`
	Luma      float64 `json:"luma"`
	Sharpness float64 `json:"sharpness"`
}

// VideoVerdict 视频质检结论。
type VideoVerdict struct {
	Path          string        `json:"path"`
	OK            bool          `json:"ok"`
	Severity      Severity      `json:"severity"`
	DurationSec   float64       `json:"duration_sec"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	FreezeRatio   float64       `json:"freeze_ratio"` // 冻结帧占比（AI 视频常见"画面卡住"）
	BlackRatio    float64       `json:"black_ratio"`  // 黑帧占比
	Frames        []FrameSample `json:"frames"`
	MeanSharpness float64       `json:"mean_sharpness"`
	LumaDrift     float64       `json:"luma_drift"` // 亮度漂移（AI 视频渐变跑偏）
	Defects       []Defect      `json:"defects"`
}

// CheckVideo 对生成的镜头片段做客观质检。
// 抽帧策略：均匀抽取最多 6 帧（AI 视频时长 5-10 秒，6 帧足以发现卡帧/黑帧/糊化）。
func (g *Gate) CheckVideo(path string, expectDur float64) VideoVerdict {
	return g.CheckVideoScene(path, expectDur, SceneCtx{})
}

// CheckVideoScene 带场景语境的视频质检（夜戏阈值放行，见 SceneCtx）。
func (g *Gate) CheckVideoScene(path string, expectDur float64, sc SceneCtx) VideoVerdict {
	v := VideoVerdict{Path: path, OK: true, Severity: SeverityOK}
	p, err := ffx.ProbeFile(path)
	if err != nil {
		v.OK = false
		v.Severity = SeverityBlock
		v.Defects = append(v.Defects, Defect{Code: "E_VIDEO_UNREADABLE", Severity: SeverityBlock,
			Message: "视频无法探测（文件损坏或半截下载）: " + err.Error()})
		return v
	}
	v.DurationSec, v.Width, v.Height = p.DurationSec, p.Width, p.Height

	if p.Width < 1024 || p.Height < 576 {
		v.OK = false
		v.Severity = SeverityBlock
		v.Defects = append(v.Defects, Defect{Code: "E_VIDEO_TOO_SMALL", Severity: SeverityBlock,
			Metric: float64(p.Width), Limit: 1024,
			Message: fmt.Sprintf("视频分辨率过低（%dx%d），不满足交付标准", p.Width, p.Height)})
	}
	if expectDur > 0 && g.cfg.MaxCutDriftSec > 0 {
		drift := math.Abs(p.DurationSec - expectDur)
		if drift > g.cfg.MaxCutDriftSec {
			v.OK = false
			// 只降级到 warn，**绝不触发换种重渲**：实测 cogvideox-3 对任意时长请求
			// 恒定产出约 5.1s，换种子不会改变结果，重渲只是重复付费。
			// 正确处置是下游按 ffprobe 实测值重排字幕与配音（narrate 已经这么做），
			// 或者把 shotspec 的计划时长直接对齐模型能力（5s / 10s）。
			if v.Severity != SeverityBlock {
				v.Severity = SeverityWarn
			}
			v.Defects = append(v.Defects, Defect{Code: "W_DURATION_DRIFT", Severity: SeverityWarn,
				Metric: drift, Limit: g.cfg.MaxCutDriftSec,
				Message: fmt.Sprintf("镜头时长 %.2fs 与计划 %.2fs 相差 %.2fs；该差异由模型固定片长造成，"+
					"重渲无法修复，已交由下游按实测重排字幕与配音",
					p.DurationSec, expectDur, drift)})
		}
	}

	// 抽帧分析
	frames, err := g.sampleFrames(path, p.DurationSec, 6)
	if err != nil {
		// 抽帧失败不算致命：视频本身可探测，只是无法做帧级质检
		v.Defects = append(v.Defects, Defect{Code: "W_FRAME_SAMPLE_FAILED", Severity: SeverityRerender,
			Message: "抽帧质检失败（不影响成片）: " + err.Error()})
		return v
	}
	v.Frames = frames
	var sumSharp float64
	blacks, freezes := 0, 0
	for i, f := range frames {
		sumSharp += f.Sharpness
		if f.Luma < 12 {
			blacks++
		}
		if i > 0 && math.Abs(f.Luma-frames[i-1].Luma) < 0.8 {
			freezes++
		}
	}
	v.MeanSharpness = sumSharp / float64(len(frames))
	v.BlackRatio = float64(blacks) / float64(len(frames))
	v.FreezeRatio = float64(freezes) / float64(len(frames)-1+1)
	if len(frames) > 1 {
		v.LumaDrift = math.Abs(frames[len(frames)-1].Luma - frames[0].Luma)
	}

	// 黑帧判定：夜戏放宽——帧级黑位下限 12→6、占比上限 0.5→0.75。
	// 夜景/黑色电影里大面积阴影合法，但"每一帧都全黑"在夜戏下同样无效：
	// 完全失败的夜戏生成（纯黑输出）仍会被拦住。
	blackLuma, blackRatioLimit := 12.0, 0.5
	if sc.Night {
		blackLuma, blackRatioLimit = 6.0, 0.75
	}
	if v.BlackRatio >= blackRatioLimit {
		v.OK = false
		v.Severity = SeverityBlock
		v.Defects = append(v.Defects, Defect{Code: "E_VIDEO_BLACK", Severity: SeverityBlock,
			Metric: v.BlackRatio, Limit: blackRatioLimit,
			Message: fmt.Sprintf("%.0f%% 的抽样帧接近全黑（帧黑位下限 %.0f），视频内容无效",
				v.BlackRatio*100, blackLuma)})
	}
	// FreezeRatio 有意不设门禁：它用"相邻抽样帧亮度差 < 0.8"做冻结近似，而
	// 固定机位长镜头（分镜里的"固定"运镜）亮度差同样接近 0——该近似无法区分
	// "构图不动"与"画面冻结"，设门禁会把所有固定镜头误杀。真冻结交给视觉复核
	// 的"画面完整性"检查项（它看的是内容，不是亮度）。
	if v.MeanSharpness < g.cfg.MinSharpness*0.6 {
		v.OK = false
		if v.Severity != SeverityBlock {
			v.Severity = SeverityRerender
		}
		v.Defects = append(v.Defects, Defect{Code: "E_VIDEO_BLURRY", Severity: SeverityRerender,
			Metric: v.MeanSharpness, Limit: g.cfg.MinSharpness * 0.6,
			Message: fmt.Sprintf("视频整体偏糊（平均清晰度 %.1f）", v.MeanSharpness)})
	}
	if v.LumaDrift > 90 {
		v.OK = false
		if v.Severity != SeverityBlock {
			v.Severity = SeverityRerender
		}
		v.Defects = append(v.Defects, Defect{Code: "E_VIDEO_LUMA_DRIFT", Severity: SeverityRerender,
			Metric: v.LumaDrift, Limit: 90,
			Message: fmt.Sprintf("镜头内亮度漂移 %.0f，疑似 AI 视频渐变跑偏", v.LumaDrift)})
	}
	return v
}

// sampleFrames 均匀抽帧并逐帧计算客观指标。
func (g *Gate) sampleFrames(path string, dur float64, n int) ([]FrameSample, error) {
	if n < 2 {
		n = 2
	}
	tmp, err := os.MkdirTemp("", "studio-qc-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	// 按时间点抽帧：比 fps 抽样更可控，且能覆盖首尾
	times := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		// 避开首尾各 5%，AI 视频首尾常有过渡伪影
		t := dur * (0.05 + 0.9*float64(i)/float64(n-1))
		times = append(times, t)
	}
	var out []FrameSample
	for i, t := range times {
		fp := filepath.Join(tmp, fmt.Sprintf("f%02d.png", i))
		args := []string{"-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%.3f", t), "-i", path,
			"-frames:v", "1", "-vf", "scale=640:-2", fp}
		if _, err := ffx.Run(60*time.Second, "ffmpeg", args...); err != nil {
			continue // 单帧失败不致命
		}
		m, err := AnalyzeImage(fp)
		if err != nil {
			continue
		}
		out = append(out, FrameSample{AtSec: t, Luma: m.MeanLuma, Sharpness: m.Sharpness})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未能抽取任何帧")
	}
	return out, nil
}

// SortDefects 按严重度排序（block 在前），便于日志阅读。
func SortDefects(defects []Defect) []Defect {
	out := append([]Defect(nil), defects...)
	sort.SliceStable(out, func(i, j int) bool {
		rank := func(s Severity) int {
			switch s {
			case SeverityBlock:
				return 0
			case SeverityRerender:
				return 1
			case SeverityWarn:
				return 2
			default:
				return 3
			}
		}
		return rank(out[i].Severity) < rank(out[j].Severity)
	})
	return out
}

// Worst 汇总一组结论中最严重的等级。
func Worst(severities ...Severity) Severity {
	worst := SeverityOK
	for _, s := range severities {
		switch s {
		case SeverityBlock:
			return SeverityBlock
		case SeverityRerender:
			worst = SeverityRerender
		case SeverityWarn:
			// 不要把已有的 rerender 降级回 warn
			if worst != SeverityRerender {
				worst = SeverityWarn
			}
		}
	}
	return worst
}
