package render

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"aistudio/internal/audio"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
)

// ---------------------------------------------------------------------------
// 动态分镜（animatic）：花钱生成视频**之前**的灰盒预览。
//
// 电影工业的标准流程是"剧本 → 分镜 → 动态分镜（animatic）→ 拍摄"：先用最便宜的
// 手段把剪辑节奏看一遍，确认叙事成立再进摄影棚。AI 电影里"拍摄"是最贵的环节
// （视频按秒计费），却常常是第一个执行的——节奏问题只能在花完钱之后发现。
//
// 本命令把已有的首帧图（render --images-only 的产物）按分镜时长拼成一部
// 静帧预览片：每个镜头一张首帧停 planned/实测的时长，硬切衔接，不转场不调色。
// 它回答的问题只有一个："这个叙事节奏值不值得花钱生成视频？"
//
// 声音：若此前跑过 deliver（output/audio + artifacts/narration.json 存在），
// 逐句台词按实测入点铺进时间轴（scratch track）；否则出无声预览并显式说明。
// ---------------------------------------------------------------------------

// animaticShot 预览用的单镜头信息。
type animaticShot struct {
	ID      string
	Image   string // 绝对路径；空 = 用占位灰卡
	Dur     float64
	Missing bool // 首帧缺失：占位灰卡 + 显式告警
}

// Animatic 拼装动态分镜，输出 output/animatic.mp4，返回其路径。
func Animatic(cfg *config.Config, runDir string) (string, error) {
	// 镜头顺序与时长取 shotspec（artifacts）：它是分镜的叙事顺序。
	// 不能用 output/shotplan.json——那是**渲染优先级排序后**的顺序
	// （关键镜头先行），按它拼片等于把剧情打乱。
	kit, err := loadKit(runDir)
	if err != nil {
		return "", fmt.Errorf("读取生成规格失败: %w（需先完成 studio run 的 shotspec 阶段）", err)
	}
	w, h := parseSize(cfg.Video.Size)
	if w == 0 {
		w, h = 1920, 1080
	}
	fps := cfg.Video.FPS
	if fps <= 0 {
		fps = 24
	}

	// 收集每镜的首帧图与时长（有渲染清单时用实测时长，节奏更接近真实成片）
	var shots []animaticShot
	var missingImgs []string
	for _, s := range kit.ShotList {
		sh := animaticShot{ID: s.ID, Dur: s.DurationSec}
		img := filepath.Join(runDir, "output", "shots", s.ID+".png")
		if fsx.FileExists(img) {
			sh.Image = img
		} else {
			sh.Missing = true
			missingImgs = append(missingImgs, s.ID)
		}
		shots = append(shots, sh)
	}
	if man, err := loadManifestForAnimatic(runDir); err == nil {
		for i := range shots {
			if d, ok := man[shots[i].ID]; ok && d > 0.3 {
				shots[i].Dur = d
			}
		}
	}

	var total float64
	for _, s := range shots {
		total += s.Dur
	}
	if total <= 0 {
		return "", fmt.Errorf("镜头时长合计为 0，无法拼装动态分镜")
	}

	outDir := filepath.Join(runDir, "output")
	out := filepath.Join(outDir, "animatic.mp4")

	// 组装滤镜图：图片输入 loop 到镜头时长；缺图镜头用深灰底卡占位（可见即可，
	// 不做文字标注——drawtext 依赖系统字体，Windows 下缺 CJK 字体会直接失败）
	var inputs []string
	var filters []string
	for i, s := range shots {
		if s.Missing {
			inputs = append(inputs, "-f", "lavfi", "-i",
				fmt.Sprintf("color=c=0x1a1e26:size=%dx%d:rate=%d", w, h, fps))
			filters = append(filters, fmt.Sprintf("[%d:v]trim=duration=%.3f,setpts=PTS-STARTPTS,scale=%d:%d,setsar=1,format=yuv420p[n%d]",
				i, s.Dur, w, h, i))
			continue
		}
		inputs = append(inputs, "-loop", "1", "-t", fmt.Sprintf("%.3f", s.Dur), "-i", s.Image)
		filters = append(filters, fmt.Sprintf(
			"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p[n%d]",
			i, w, h, w, h, fps, i))
	}
	var refs []string
	for i := range shots {
		refs = append(refs, fmt.Sprintf("[n%d]", i))
	}
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[vcat]", strings.Join(refs, ""), len(shots)))
	filters = append(filters, "[vcat]null[vout]")

	args := append([]string{"-hide_banner", "-y"}, inputs...)

	// 声音：有此前 deliver 的逐句配音产物时铺 scratch track（按实测入点）。
	// 基底静音 anullsrc 铺满全片时长，逐句 adelay 到位后与基底 amix。
	// 配音铺轨必须用**分镜自身的时间轴**：分镜时长是"计划值 + 已渲染镜头的
	// 实测值"，而成片时间轴（narration.json 的 abs_start_sec）只含已渲染镜头
	// 且按实测连续排列——两套时间轴必然错位，直接用 abs 入点会让台词
	// 越往后越偏（表现为"台词先后顺序对不上"）。改为按 shot 相对入点换算。
	startOf := make(map[string]float64, len(shots))
	var t float64
	for _, s := range shots {
		startOf[s.ID] = t
		t += s.Dur
	}
	voiceTracks := loadAnimaticVoice(runDir, startOf)
	hasVoice := len(voiceTracks) > 0 && ffx.HasFilter("amix")
	if hasVoice {
		base := len(shots)
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("anullsrc=r=%d:cl=mono", cfg.Audio.SampleRate))
		var amix []string
		amix = append(amix, fmt.Sprintf("[%d:a]aformat=channel_layouts=mono,apad,atrim=0:%.3f[abuf]",
			base, total))
		var arefs []string
		for k, vt := range voiceTracks {
			args = append(args, "-i", vt.path)
			delayMs := int(vt.startSec * 1000)
			amix = append(amix, fmt.Sprintf("[%d:a]aresample=%d,aformat=channel_layouts=mono,adelay=%d[a%d]",
				base+1+k, cfg.Audio.SampleRate, delayMs, k))
			arefs = append(arefs, fmt.Sprintf("[a%d]", k))
		}
		amix = append(amix, fmt.Sprintf("%s;[amixed]apad,atrim=0:%.3f[aout]",
			audio.AmixFilter(append([]string{"[abuf]"}, arefs...), "amixed"),
			total))
		filters = append(filters, amix...)
	}

	// 输入全部就位后再接滤镜图与映射：-map 每个流只允许出现一次，
	// 重复的 -map [vout] 会让（至少老版本）ffmpeg 判"label 已被使用"而整单失败
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[vout]",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28",
		"-pix_fmt", "yuv420p", "-movflags", "+faststart")
	if hasVoice {
		args = append(args, "-map", "[aout]",
			"-c:a", "aac", "-b:a", "128k", "-ar", fmt.Sprint(cfg.Audio.SampleRate))
	}
	args = append(args, "-t", fmt.Sprintf("%.3f", total), out)

	fmt.Printf("▶ 拼装动态分镜：%d 个镜头 / %.1f 秒（%s）\n",
		len(shots), total, map[bool]string{true: "含 scratch 配音", false: "无声预览"}[hasVoice])
	if len(missingImgs) > 0 {
		fmt.Printf("  ⚠ %d 个镜头缺首帧，用灰卡占位：%s\n  补齐：studio render --run %s --images-only\n",
			len(missingImgs), strings.Join(missingImgs, ","), runDir)
	}
	if !hasVoice {
		fmt.Println("  ⚠ 无声预览：台词节奏以 deliver 阶段配音实测为准")
	}
	if _, err := ffx.Run(30*time.Minute, "ffmpeg", args...); err != nil {
		// 滤镜图拼装错误极难肉眼排查：失败时把整图打出来（限长）
		g := strings.Join(filters, ";")
		if len(g) > 20000 {
			g = g[:20000] + "…"
		}
		return "", fmt.Errorf("动态分镜拼装失败: %w\n  filter_complex: %s", err, g)
	}
	rel := filepath.ToSlash(filepath.Join("output", "animatic.mp4"))
	fmt.Printf("  🎞 动态分镜就绪: %s\n    这是\"值不值得为视频付钱\"的判断依据：确认节奏后跑 studio render --run %s\n",
		rel, runDir)
	return rel, nil
}

// loadManifestForAnimatic 读取渲染清单的实测时长（可选：没有就按计划时长）。
func loadManifestForAnimatic(runDir string) (map[string]float64, error) {
	var m Manifest
	if err := fsx.ReadJSON(filepath.Join(runDir, "output", "render_manifest.json"), &m); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, s := range m.Shots {
		if s.Status == "done" && s.ActualDur > 0 {
			out[s.ID] = s.ActualDur
		}
	}
	return out, nil
}

// animaticVoice 一句待铺轨的台词音频。
type animaticVoice struct {
	path     string
	startSec float64
}

// loadAnimaticVoice 收集 deliver 阶段已产出的逐句配音（含实测入点）。
// animatic 是渲染前的预览，通常没有这些产物——返回空即无声预览。
func loadAnimaticVoice(runDir string, startOf map[string]float64) []animaticVoice {
	var plan struct {
		Lines []struct {
			ShotID   string  `json:"shot_id"`
			StartSec float64 `json:"start_sec"` // 相对所属镜头起点的入点（J-cut 为负）
			File     string  `json:"file"`
		} `json:"lines"`
	}
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "narration.json"), &plan); err != nil {
		return nil
	}
	var out []animaticVoice
	for _, l := range plan.Lines {
		if l.File == "" {
			continue
		}
		base, ok := startOf[l.ShotID]
		if !ok {
			continue // 镜头不在分镜时间轴上（不应发生，防御）
		}
		start := base + l.StartSec
		if start < 0 {
			start = 0 // J-cut 提前量越出片头时钳到 0（adelay 不接受负值）
		}
		p := filepath.Join(runDir, filepath.FromSlash(l.File))
		if fsx.FileExists(p) {
			out = append(out, animaticVoice{path: p, startSec: start})
		}
	}
	return out
}
