package deliver

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"aistudio/internal/ffx"
	"aistudio/internal/quality"
)

// ---------------------------------------------------------------------------
// 镜头间曝光匹配（concat 阶段的确定性"AI 味"消除）
//
// 生成端每个镜头独立出图/出视频，曝光与色调各镜漂移：同一场景里上一镜偏亮、
// 下一镜偏暗，成片上表现为"一切换就闪一下"——观众未必说得出原因，但一定感觉得到。
//
// 做法（刻意保守）：实测每个镜头的平均亮度，与全片中位数比较，对偏差镜头施加
// 小幅 eq 亮度校正（±0.06，约 ±15/255 亮度级）。它只拉齐**曝光**，不碰调色——
// 风格（胶片型号/色板/颗粒）由 promspec 在提示词层统一，后期不抢戏。
// eq 插在 scale 与 pad 之间：letterbox 黑边在校正之后添加，不会被抬成灰边。
// ---------------------------------------------------------------------------

const (
	// lumaMatchGain 亮度差 → eq brightness 的映射系数（0-255 差值 / 255 × 增益）
	lumaMatchGain = 0.8
	// lumaMatchMax 单镜最大校正幅度。超过 ±0.06（约 15 亮度级）说明不是漂移
	// 而是该镜头本身就是暗场/亮场（夜戏、逆光、雪地），强行拉齐会毁掉光比设计。
	lumaMatchMax = 0.06
	// lumaMatchMinRelMatch 低于 2/255 的差异视为噪声，不做校正（避免无谓重编码差异）
	lumaMatchMinDelta = 2.0
)

// shotLumaOffsets 计算每个镜头的 eq 亮度校正量（无差异/测量失败 → 0）。
// 抽每镜中点一帧缩到 320px 测量：本地 ffmpeg 调用，毫秒级，不产生付费调用。
func shotLumaOffsets(clips []shotClip) []float64 {
	n := len(clips)
	if n == 0 {
		return nil
	}
	offsets := make([]float64, n)
	tmpDir, err := os.MkdirTemp("", "studio-exposure")
	if err != nil {
		fmt.Printf("  ⚠ 镜头间曝光匹配跳过（临时目录创建失败）: %v\n", err)
		return offsets
	}
	defer os.RemoveAll(tmpDir)

	lumas := make([]float64, n)
	valid := make([]float64, 0, n)
	for i, c := range clips {
		fp := filepath.Join(tmpDir, fmt.Sprintf("%s.jpg", c.ID))
		if _, err := ffx.Run(60*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%.3f", c.Dur*0.5), "-i", c.Path,
			"-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "5", fp); err != nil {
			continue
		}
		m, err := quality.AnalyzeImage(fp)
		if err != nil || m == nil {
			continue
		}
		lumas[i] = m.MeanLuma
		valid = append(valid, m.MeanLuma)
	}
	if len(valid) < n/2 {
		// 过半镜头测不出来（抽帧/解码异常）：数据不可信，整体放弃而不是瞎校
		return offsets
	}
	median := lumaMedian(valid)
	for i := range offsets {
		if lumas[i] == 0 {
			continue // 该镜测量失败：不动它
		}
		delta := median - lumas[i]
		if math.Abs(delta) < lumaMatchMinDelta {
			continue
		}
		off := delta / 255.0 * lumaMatchGain
		if off > lumaMatchMax {
			off = lumaMatchMax
		}
		if off < -lumaMatchMax {
			off = -lumaMatchMax
		}
		offsets[i] = off
	}
	return offsets
}

// lumaMedian 中位数：曝光基准必须抗单镜离群值（一个纯黑转场帧不能把全片拉暗）。
func lumaMedian(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
