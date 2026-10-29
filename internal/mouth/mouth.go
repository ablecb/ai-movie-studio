// Package mouth 实现镜头内"说话窗口"检测：抽帧 → 视觉模型逐帧判嘴态 →
// 聚合成时间窗，供配音把台词对准角色的张嘴区间（段落级口型同步）。
//
// 为什么是"窗口"而不是逐音素对齐：图生视频产出的是哑剧——模型自己决定
// 何时张嘴，与台词音频没有任何机制约束。逐音素对齐只有 OmniHuman 这类
// 音频驱动生成能做到（见 render 的对白镜头分流）；本包解决的是更普遍的
// 问题——让声音的起止与画面里"张嘴-闭嘴"的区间重合，段落级听感即自然。
//
// 检测是付费视觉调用（每镜 ~12 帧），结果缓存到 artifacts/mouth_windows.json，
// 重跑交付不重复检测。检测失败/低置信度不阻断配音：调用方回落到普通入点。
package mouth

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
)

// frameAt 一帧及其在镜头中的时刻（秒）。
type frameAt struct {
	path string
	t    float64
}

// Window 一次说话活动的检测窗口（相对镜头起点，秒）。
type Window struct {
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"` // 说话帧 / 有效帧
	Frames     int     `json:"frames"`     // 参与判断的帧数
	Speaking   int     `json:"speaking"`   // 判定为说话中的帧数
}

// Valid 窗口是否可用于对齐。
// 单看置信度 ≥0.15 太松：12 帧里 2 帧误判（16.7%）也能过线——那种窗多半
// 是表情变化造成的假窗，把声音锚上去反而错位。叠加 Speaking≥3：至少 3 帧
// 说话（25% 密度）才认可窗口；mock 模式全帧说话，天然通过。
func (w *Window) Valid() bool {
	return w != nil && w.End-w.Start >= 0.4 && w.Confidence >= 0.15 && w.Speaking >= 3
}

// Options 检测参数。
type Options struct {
	Frames int  // 抽帧数（默认 12）
	Width  int  // 抽帧宽度（默认 480，够判嘴态即可）
	Mock   bool // 离线演示：返回固定窗口，不调视觉
	Debug  func(string, ...any)
}

const systemPrompt = "你是影视后期质检助手，只输出严格 JSON，不输出任何其他文字。"

// Detect 检测单个镜头视频中的说话窗口。
// 返回 (nil, nil) 表示"镜头里没检测到说话"或视频不可用——与错误区分开，
// 调用方对两者都应回落到普通入点，但前者是正常结果。
func Detect(client *ai.Client, video string, dur float64, opt Options) (*Window, error) {
	if opt.Frames <= 0 {
		opt.Frames = 12
	}
	if opt.Width <= 0 {
		opt.Width = 480
	}
	if st, err := os.Stat(video); err != nil || st.Size() == 0 {
		return nil, nil
	}
	if dur <= 0 {
		p, err := ffx.ProbeFile(video)
		if err != nil {
			return nil, fmt.Errorf("探测镜头时长失败: %w", err)
		}
		dur = p.DurationSec
	}
	if dur < 0.5 {
		return nil, nil
	}

	dir, err := os.MkdirTemp("", "mouth-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// 全程均匀抽帧（含首尾——张嘴往往就在镜头开头，这是要对齐的起点）。
	var frames []frameAt
	for i := 0; i < opt.Frames; i++ {
		t := dur * (float64(i) + 0.5) / float64(opt.Frames) // 帧中心时刻
		fp := filepath.Join(dir, fmt.Sprintf("f%02d.jpg", i))
		if _, err := ffx.Run(60*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%.3f", t), "-i", video, "-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:-2", opt.Width), "-q:v", "4", fp); err != nil {
			continue
		}
		if st, err := os.Stat(fp); err != nil || st.Size() == 0 {
			continue
		}
		frames = append(frames, frameAt{path: fp, t: t})
	}
	if len(frames) < 3 {
		return nil, nil
	}

	if opt.Mock {
		return &Window{Start: 0.3, End: dur - 0.3, Confidence: 0.8, Frames: len(frames), Speaking: len(frames)}, nil
	}

	// 逐帧嘴态判定：一张图一个判断，比"时间序列理解"可靠得多——
	// 视觉模型做单帧属性判断是送分题，做跨帧时序推理则不可靠。
	images := make([]ai.ImageRef, len(frames))
	for i, f := range frames {
		images[i] = ai.ImageRef{Path: f.path}
	}
	var resp struct {
		Frames []struct {
			Index int `json:"i"`
			Mouth int `json:"mouth"` // 0=闭合 1=张开/说话中
		} `json:"frames"`
	}
	prompt := fmt.Sprintf(
		"这是同一个镜头视频按时间顺序均匀抽取的 %d 帧。逐帧判断画面主体的嘴部状态："+
			"明显张开或正在说话=1，闭合或非说话表情=0。画面里没有清晰人脸的帧也输出 0。"+
			"只输出 JSON：{\"frames\":[{\"i\":1,\"mouth\":0},...]}，i 从 1 开始按给定顺序。",
		len(frames))
	if err := client.VisionJSONEx(systemPrompt, prompt, images, "auto", &resp); err != nil {
		return nil, fmt.Errorf("嘴态视觉判定失败: %w", err)
	}

	states := make([]bool, len(frames)) // 与 frames 对齐的说话标记
	answered := map[int]bool{}
	for _, fr := range resp.Frames {
		if fr.Index >= 1 && fr.Index <= len(frames) {
			answered[fr.Index] = true
			if fr.Mouth > 0 {
				states[fr.Index-1] = true
			}
		}
	}
	if len(answered) < 3 {
		return nil, nil // 模型输出不可解析：当作没检测到，调用方回落
	}

	return aggregate(frames, states), nil
}

// aggregate 把逐帧嘴态聚合成时间窗：取最长连续说话段（允许 1 帧间隔的毛刺），
// 窗口边界外扩半帧间隔补偿抽帧粒度。
func aggregate(frames []frameAt, states []bool) *Window {
	bestLo, bestHi, lo := -1, -1, -1
	for i, sp := range states {
		if !sp {
			continue
		}
		// 允许 1 帧的漏判毛刺：当前帧距段首超过 2 帧间隔则断段
		if lo >= 0 && i-lo > 2 {
			lo = -1
		}
		if lo < 0 {
			lo = i
		}
		if bestLo < 0 || i-lo > bestHi-bestLo {
			bestLo, bestHi = lo, i
		}
	}
	if bestLo < 0 {
		return nil
	}
	step := 0.0
	if len(frames) > 1 {
		step = frames[1].t - frames[0].t
	}
	pad := step * 0.75
	start := frames[bestLo].t - pad
	end := frames[bestHi].t + pad
	if start < 0 {
		start = 0
	}
	speaking := 0
	for _, sp := range states {
		if sp {
			speaking++
		}
	}
	conf := float64(speaking) / float64(len(frames))
	return &Window{Start: start, End: end, Confidence: conf, Frames: len(frames), Speaking: speaking}
}

// LoadCache 读取缓存的窗口表（shot_id → window）；文件不存在返回空表。
func LoadCache(runDir string) map[string]*Window {
	out := map[string]*Window{}
	var cached map[string]*Window
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "mouth_windows.json"), &cached); err != nil {
		return out
	}
	for k, v := range cached {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// SaveCache 原子写入窗口缓存。
func SaveCache(runDir string, m map[string]*Window) error {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sorted := make(map[string]*Window, len(m))
	for _, k := range keys {
		sorted[k] = m[k]
	}
	return fsx.AtomicWriteJSON(filepath.Join(runDir, "artifacts", "mouth_windows.json"), sorted)
}
