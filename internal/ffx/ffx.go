// Package ffx 统一封装 ffmpeg/ffprobe 调用。
//
// 生产环境里 ffmpeg 相关故障占了后期环节的大半：二进制缺失、滤镜不支持、编码器参数不被
// 接受的版本差异。因此这里做三件事：
//  1. 所有调用都带 context 超时，避免某个镜头卡死拖垮整批渲染
//  2. 错误信息带上完整命令行与 stderr 尾部（否则只剩 "exit status 1"）
//  3. 提供 Preflight 自检，把"环境不可用"提前到开工前暴露，而不是渲染到一半才发现
package ffx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"aistudio/internal/conc"
)

// Available 检查某个可执行文件是否在 PATH 中。
func Available(name string) bool {
	_, err := LookPath(name)
	return err == nil
}

// overrides 允许显式指定 ffmpeg/ffprobe 路径（很多环境里 ffmpeg 不在 PATH，
// 而是随某个桌面软件分发，例如 C:\Users\X\AppData\Roaming\bilibili\ffmpeg）。
var (
	overrideMu sync.RWMutex
	overrides  = map[string]string{}
	discovered = map[string]string{}
)

// SetBinary 显式指定可执行文件路径（来自配置或命令行）。
func SetBinary(name, path string) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	if path == "" {
		delete(overrides, name)
		return
	}
	overrides[name] = path
}

// LookPath 解析可执行文件：显式指定 > PATH > 常见安装位置自动发现。
func LookPath(name string) (string, error) {
	overrideMu.RLock()
	if p, ok := overrides[name]; ok && p != "" {
		overrideMu.RUnlock()
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("%s 指定的路径不存在: %s", name, p)
	}
	if p, ok := discovered[name]; ok && p != "" {
		overrideMu.RUnlock()
		return p, nil
	}
	overrideMu.RUnlock()

	if p, err := exec.LookPath(name); err == nil {
		rememberDiscovered(name, p)
		return p, nil
	}
	if p := discover(name); p != "" {
		rememberDiscovered(name, p)
		return p, nil
	}
	return "", fmt.Errorf("找不到 %s", name)
}

func rememberDiscovered(name, path string) {
	overrideMu.Lock()
	discovered[name] = path
	overrideMu.Unlock()
}

// discover 在常见位置查找 ffmpeg/ffprobe。
// 现实情况：大量机器上 ffmpeg 是随某个软件附带的，不在 PATH 里。
// 生产工具应该尽量自己找到它，而不是要求用户改系统环境变量。
func discover(name string) string {
	exe := name
	if runtime.GOOS == "windows" {
		exe = name + ".exe"
	}
	var roots []string
	// 0) 已解析出 ffmpeg 时，优先在同目录找 ffprobe（同一构建必然成套）
	if name != "ffmpeg" {
		if ff, err := LookPath("ffmpeg"); err == nil {
			sibling := filepath.Join(filepath.Dir(ff), exe)
			if fileExists(sibling) {
				return sibling
			}
		}
	}
	// 0.5) 项目自带 tools/ 目录（便于把 ffmpeg 随项目分发，不污染系统）
	if wd, err := os.Getwd(); err == nil {
		for _, pat := range []string{
			filepath.Join(wd, "tools", exe),
			filepath.Join(wd, "tools", "ffmpeg", "bin", exe),
		} {
			if fileExists(pat) {
				return pat
			}
		}
		if entries, err := os.ReadDir(filepath.Join(wd, "tools")); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					cand := filepath.Join(wd, "tools", e.Name(), "bin", exe)
					if fileExists(cand) {
						return cand
					}
				}
			}
		}
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			roots = append(roots,
				filepath.Join(appData, "bilibili", "ffmpeg"),
				filepath.Join(appData, "ffmpeg", "bin"),
			)
		}
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			roots = append(roots,
				filepath.Join(localAppData, "Microsoft", "WinGet", "Links"),
				filepath.Join(localAppData, "Programs", "ffmpeg", "bin"),
			)
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			roots = append(roots, filepath.Join(pf, "ffmpeg", "bin"))
		}
		roots = append(roots, `C:\ffmpeg\bin`, `C:\ffmpeg`, `D:\ffmpeg\bin`, `D:\ffmpeg`)
	} else {
		roots = append(roots, "/usr/local/bin", "/usr/bin", "/opt/homebrew/bin", "/snap/bin")
	}
	for _, r := range roots {
		cand := filepath.Join(r, exe)
		if fileExists(cand) {
			return cand
		}
		// ffmpeg 静态包常解压成 ffmpeg-x.y-full_build/bin/ffmpeg.exe
		if entries, err := os.ReadDir(r); err == nil {
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(strings.ToLower(e.Name()), "ffmpeg") {
					cand := filepath.Join(r, e.Name(), "bin", exe)
					if fileExists(cand) {
						return cand
					}
				}
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// BinPath 返回 ffmpeg 或 ffprobe 的解析结果，供日志展示"到底用了哪个二进制"。
func BinPath(name string) string {
	p, err := LookPath(name)
	if err != nil {
		return "(未找到 " + name + ")"
	}
	return p
}

// Dir 返回已解析出的 ffmpeg 所在目录（用于把同目录的 ffprobe 也纳入发现范围）。
func Dir() string {
	p, err := LookPath("ffmpeg")
	if err != nil {
		return ""
	}
	return filepath.Dir(p)
}

// Version 返回 ffmpeg 版本字符串（生产排障时需要知道用的是哪个构建）。
func Version() string {
	out, err := run(context.Background(), 20*time.Second, "ffmpeg", "-hide_banner", "-version")
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	return strings.TrimSpace(line)
}

// Preflight 启动自检：ffmpeg 必须可用，且关键滤镜齐备。
// 生产环境应在任何付费调用之前运行它——环境不齐导致的失败最不值得花钱。
func Preflight() error {
	c := Detect()
	if len(c.Missing) > 0 {
		return fmt.Errorf("缺少可执行文件 %s：请安装 ffmpeg（推荐 full/essentials 构建，含 ffprobe）并加入 PATH，\n"+
			"  或用环境变量 STUDIO_FFMPEG / STUDIO_FFPROBE 指定路径", strings.Join(c.Missing, ", "))
	}
	// 这些能力/滤镜是后期链路的关键依赖。
	// 严格模式：缺失即阻塞开工（而不是降级产出：没有 ffprobe 就没有"实测优先"，
	// 没有 loudnorm 就没有合规的响度归一化）。
	if c.FFprobePath == "" {
		return fmt.Errorf("缺少 ffprobe：请安装完整 ffmpeg 包（含 ffprobe），或用 STUDIO_FFPROBE 指定路径")
	}
	var lack []string
	if !c.HasEbur128 {
		lack = append(lack, "响度测量（ebur128）")
	}
	if !c.HasLoudnorm {
		lack = append(lack, "响度归一化（loudnorm）")
	}
	if !c.HasSidechain {
		lack = append(lack, "侧链闪避（sidechaincompress）")
	}
	if len(lack) > 0 {
		return fmt.Errorf("当前 ffmpeg 构建缺少关键能力：%s\n  版本: %s\n  请更换完整版构建（如 gyan.dev essentials/full）",
			strings.Join(lack, "、"), c.Version)
	}
	return nil
}

// Filters 返回可用的滤镜名单（兼容旧调用）。
func HasFilter(name string) bool {
	out, err := run(context.Background(), 20*time.Second, "ffmpeg", "-hide_banner", "-filters")
	if err != nil {
		return false
	}
	return strings.Contains(out, " "+name+" ")
}

// Encoders 检查编码器可用性（libx264 缺失时要能降级到其他 H.264 编码器）。
func HasEncoder(name string) bool {
	out, err := run(context.Background(), 20*time.Second, "ffmpeg", "-hide_banner", "-encoders")
	if err != nil {
		return false
	}
	return strings.Contains(out, " "+name+" ")
}

// Capabilities 记录当前 ffmpeg 构建的能力。
// 生产环境里 ffmpeg 版本差异很大（有些是随软件附带的 2016 年老构建），
// 因此关键能力必须探测后降级，而不是直接假设存在。
type Capabilities struct {
	FFmpegPath   string   `json:"ffmpeg_path"`
	FFprobePath  string   `json:"ffprobe_path"`
	Version      string   `json:"version"`
	HasLoudnorm  bool     `json:"has_loudnorm"`  // 响度归一化（严格模式必需）
	HasEbur128   bool     `json:"has_ebur128"`   // 响度测量
	HasSubtitles bool     `json:"has_subtitles"` // 字幕烧录
	HasSidechain bool     `json:"has_sidechain"` // 侧链闪避（音乐让路人声）
	Missing      []string `json:"missing,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

var (
	capMu   sync.Mutex
	capOnce *Capabilities
)

// Detect 探测当前环境能力（结果缓存，多次调用只探测一次）。
func Detect() *Capabilities {
	capMu.Lock()
	defer capMu.Unlock()
	if capOnce != nil {
		return capOnce
	}
	c := &Capabilities{}
	if p, err := LookPath("ffmpeg"); err == nil {
		c.FFmpegPath = p
	} else {
		c.Missing = append(c.Missing, "ffmpeg")
	}
	if p, err := LookPath("ffprobe"); err == nil {
		c.FFprobePath = p
	} else {
		c.Warnings = append(c.Warnings,
			"未找到 ffprobe：严格模式下媒体探测将直接失败（请安装完整 ffmpeg 包，含 ffprobe）")
	}
	if c.FFmpegPath != "" {
		c.Version = Version()
		filters, _ := run(context.Background(), 20*time.Second, "ffmpeg", "-hide_banner", "-filters")
		c.HasLoudnorm = strings.Contains(filters, " loudnorm ")
		c.HasEbur128 = strings.Contains(filters, " ebur128 ")
		c.HasSubtitles = strings.Contains(filters, " subtitles ")
		c.HasSidechain = strings.Contains(filters, " sidechaincompress ")
		if !c.HasLoudnorm {
			c.Warnings = append(c.Warnings,
				"当前 ffmpeg 缺少 loudnorm 滤镜：严格模式下响度归一化将直接失败（请升级到 ffmpeg 4.1+）")
		}
		if !c.HasSubtitles {
			c.Warnings = append(c.Warnings, "当前 ffmpeg 缺少 subtitles 滤镜：字幕无法烧录（显式 --no-burn 可只出外挂 SRT）")
		}
	}
	capOnce = c
	return c
}

// HasLoudnorm 便捷判断（供音频模块选择归一化策略）。
func HasLoudnorm() bool { return Detect().HasLoudnorm }

// apad 的"补到指定时长"参数在新旧版本里名字不同：
// 老版本（3.x）只有 whole_len（样本数），新版本才有 whole_dur（秒）。
// 记错这个名字的后果很隐蔽：整条混音链直接报 "Option not found"。
var (
	apadOnce sync.Once
	apadDur  bool
)

// Run 执行 ffmpeg/ffprobe 命令，返回合并输出。失败时错误里带命令行与输出尾部。
func Run(timeout time.Duration, name string, args ...string) (string, error) {
	return run(context.Background(), timeout, name, args...)
}

// RunIn 在指定工作目录执行。
//
// 为什么需要：subtitles 等滤镜按**工作目录**解析相对路径，而 Windows 盘符（D:）在
// filter 参数里与选项分隔符冲突、转义规则又随版本变化。用"切到字幕所在目录 + 纯文件名"
// 是最稳的跨版本做法。
func RunIn(dir string, timeout time.Duration, name string, args ...string) (string, error) {
	return runIn(context.Background(), dir, timeout, name, args...)
}

// RunCtx 带上下文执行（批量渲染时可随批次的取消一起终止）。
func RunCtx(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return run(ctx, timeout, name, args...)
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return runIn(ctx, "", timeout, name, args...)
}

func runIn(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	bin, err := LookPath(name)
	if err != nil {
		return "", fmt.Errorf("执行 %s 失败: %w", name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	out := buf.String()
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("%s 超时（%v）: %s", name, timeout, tail(out, 400))
		}
		return out, fmt.Errorf("%s 执行失败: %w\n  命令: %s %s\n  输出尾部: %s",
			name, err, name, strings.Join(redact(args), " "), tail(out, 600))
	}
	// 老构建（ffmpeg 3.x）在滤镜图中途崩溃时可能仍以退出码 0 结束，
	// 只把崩掉之前的部分写进输出文件（实测 amix 输入提前 EOF 时报
	// "Error while filtering: Cannot allocate memory" 却 exit 0）。
	// 只看退出码就会把残缺文件当成功产物混进成片——必须连 stderr 一起盯住。
	if strings.Contains(out, "Error while filtering") {
		return out, fmt.Errorf("%s 滤镜执行中断（退出码 0 但输出残缺）\n  命令: %s %s\n  输出尾部: %s",
			name, name, strings.Join(redact(args), " "), tail(out, 600))
	}
	return out, nil
}

// tail 取输出尾部若干字符（ffmpeg 的错误信息都在最后几行）。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// redact 参数脱敏：命令行可能含 base64 图片等超长内容，日志里只留前 60 字符。
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if len(a) > 60 {
			out[i] = a[:60] + fmt.Sprintf("…(%d bytes)", len(a))
			continue
		}
		out[i] = a
	}
	return out
}

// Probe 返回媒体文件的探测结果。
type Probe struct {
	Path        string
	DurationSec float64
	Width       int
	Height      int
	FPS         float64
	VideoCodec  string
	AudioCodec  string
	Bitrate     int64
	HasAudio    bool
}

// ProbeFile 用 ffprobe 读取媒体信息。这是"实测优先"原则的基础：
// 任何时长都必须实测，不允许用计划值代替。
//
// 严格模式：ffprobe 缺失或探测失败都直接报错，不做"解析 ffmpeg 输出"的降级——
// 时长/编码参数的精度是下游（字幕重排、混音、质检）正确性的前提，精度不可降。
func ProbeFile(path string) (*Probe, error) {
	if _, err := LookPath("ffprobe"); err != nil {
		return nil, fmt.Errorf("未找到 ffprobe：请安装完整 ffmpeg 包（含 ffprobe），或用 STUDIO_FFPROBE 指定路径")
	}
	return probeWithFFprobe(path)
}

// ProbeBatch 并行探测多个媒体文件的时长，结果与输入同序。
// 探测是独立的子进程调用（每次几十到几百毫秒），成片几十个镜头时
// 串行探测累积的墙钟时间不可忽略。单个文件失败只影响自己位置的 error，
// 不影响其他文件——失败策略（首错中止还是忽略）由调用方决定。
func ProbeBatch(paths []string, workers int) (durations []float64, errs []error) {
	durations = make([]float64, len(paths))
	errs = make([]error, len(paths))
	conc.ForEachIdx(len(paths), workers, func(i int) {
		p, err := ProbeFile(paths[i])
		if err != nil {
			errs[i] = err
			return
		}
		durations[i] = p.DurationSec
	})
	return durations, errs
}

func probeWithFFprobe(path string) (*Probe, error) {
	out, err := Run(60*time.Second, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration,bit_rate:stream=codec_type,codec_name,width,height,avg_frame_rate,duration",
		"-of", "default=noprint_wrappers=1",
		path)
	if err != nil {
		return nil, err
	}
	p := &Probe{Path: path}
	var cur string
	for _, line := range strings.Split(out, "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := kv[0], kv[1]
		switch k {
		case "codec_type":
			cur = v
		case "codec_name":
			if cur == "video" && p.VideoCodec == "" {
				p.VideoCodec = v
			} else if cur == "audio" && p.AudioCodec == "" {
				p.AudioCodec = v
				p.HasAudio = true
			}
		case "width":
			if cur == "video" {
				fmt.Sscanf(v, "%d", &p.Width)
			}
		case "height":
			if cur == "video" {
				fmt.Sscanf(v, "%d", &p.Height)
			}
		case "avg_frame_rate":
			if cur == "video" && p.FPS == 0 {
				var num, den float64
				if n, _ := fmt.Sscanf(v, "%f/%f", &num, &den); n == 2 && den > 0 {
					p.FPS = num / den
				}
			}
		case "duration":
			var d float64
			fmt.Sscanf(v, "%f", &d)
			if cur == "video" && p.DurationSec == 0 && d > 0 {
				p.DurationSec = d
			} else if cur == "format" && d > 0 {
				p.DurationSec = d
			}
		}
	}
	// format 段的 duration 更权威（容器时长），单独再取一次
	if out2, err := Run(30*time.Second, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path); err == nil {
		var d float64
		if n, _ := fmt.Sscanf(strings.TrimSpace(out2), "%f", &d); n == 1 && d > 0 {
			p.DurationSec = d
		}
	}
	if p.DurationSec <= 0 {
		return nil, fmt.Errorf("无法探测媒体时长（文件可能损坏）: %s", path)
	}
	return p, nil
}
