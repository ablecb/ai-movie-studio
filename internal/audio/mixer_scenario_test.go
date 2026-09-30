package audio

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aistudio/internal/ffx"
)

// tone 生成测试音（指定频率、时长、幅度的正弦）。
func tone(t *testing.T, path string, freq, dur, amp float64) {
	t.Helper()
	if _, err := ffx.Run(60*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency="+strconv.FormatFloat(freq, 'f', 0, 64)+
			":duration="+strconv.FormatFloat(dur, 'f', 3, 64)+":sample_rate=24000",
		"-af", "volume="+strconv.FormatFloat(amp, 'f', 3, 64), path); err != nil {
		t.Skipf("环境无法生成测试音频（跳过）: %v", err)
	}
}

// rmsWindow 测量音频在 [from, to) 区间的均方根电平（dBFS）。
// 全静音返回 -120（有限值，避免 ±Inf 参与比较）。
func rmsWindow(t *testing.T, path string, from, to float64) float64 {
	t.Helper()
	out, err := ffx.Run(5*time.Minute, "ffmpeg", "-hide_banner", "-nostats",
		"-ss", strconv.FormatFloat(from, 'f', 3, 64),
		"-t", strconv.FormatFloat(to-from, 'f', 3, 64),
		"-i", path, "-af", "astats", "-f", "null", "-")
	if err != nil && out == "" {
		t.Fatalf("测量 %s 的 [%.1f,%.1f) 区间失败: %v", filepath.Base(path), from, to, err)
	}
	best := math.Inf(-1)
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, "RMS level dB:")
		if i < 0 {
			continue
		}
		s := strings.TrimSpace(line[i+len("RMS level dB:"):])
		if v, perr := strconv.ParseFloat(s, 64); perr == nil {
			if math.IsInf(v, -1) {
				continue // 静音
			}
			if v > best {
				best = v
			}
		}
	}
	if math.IsInf(best, -1) {
		return -120
	}
	return best
}

// TestMixWithBGMAndSFX 验证配乐与音效真的被混进来了，且时长与成片对齐。
//
// 这条代码路径此前从未被执行过：历史上所有运行 music_tracks=0，
// 配乐子混音、侧链闪避、音效铺轨分支全部没被跑到过。
func TestMixWithBGMAndSFX(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := t.TempDir()
	voice := filepath.Join(dir, "voice.wav")
	music := filepath.Join(dir, "music.wav")
	sfx := filepath.Join(dir, "sfx.wav")
	tone(t, voice, 440, 2.0, 0.5)
	tone(t, music, 220, 6.0, 0.3)
	tone(t, sfx, 880, 0.3, 0.5)

	out := filepath.Join(dir, "mix.wav")
	res, err := Mix(MixRequest{
		VoiceTracks: []Track{{Path: voice, StartSec: 0}},
		MusicTracks: []Track{{Path: music, StartSec: 0, GainDB: -12}},
		SFXTracks:   []Track{{Path: sfx, StartSec: 1.0, GainDB: -10}},
		TotalSec:    6.0,
		Out:         out,
		DuckDB:      -9,
	}, Options{SampleRate: 24000, Channels: 2, TargetLoudness: -14, TruePeakDB: -1})
	if err != nil {
		t.Fatalf("混音失败: %v", err)
	}
	if res.MusicCount != 1 || res.SFXCount != 1 || res.VoiceCount != 1 {
		t.Errorf("轨数统计错误: voice=%d music=%d sfx=%d（各应为 1）",
			res.VoiceCount, res.MusicCount, res.SFXCount)
	}
	// 时长必须等于成片长度，否则音画会不同步
	if math.Abs(res.DurationSec-6.0) > 0.15 {
		t.Errorf("混音时长 %.2fs，应约 6.0s（必须与成片对齐）", res.DurationSec)
	}
	if res.TruePeakDB <= -60 {
		t.Errorf("混音结果近乎静音（峰值 %.1f dBFS）", res.TruePeakDB)
	}
}

// TestSidechainDuckingActuallyLowersMusic 证明侧链闪避真的起作用。
//
// 测量必须在**闪避后的配乐本身**上做：把人声混进来之后，人声的电平会掩盖配乐的变化
// （实测端到端窗口只差 0.4dB，看起来像"没生效"，实际配乐已被压低数 dB）。
// 因此这里把 sidechaincompress 的输出单独引出再测。
func TestSidechainDuckingActuallyLowersMusic(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	if !ffx.Detect().HasSidechain {
		t.Skip("当前 ffmpeg 无 sidechaincompress，闪避不可用")
	}
	dir := t.TempDir()
	voice := filepath.Join(dir, "voice.wav")
	music := filepath.Join(dir, "music.wav")
	tone(t, voice, 440, 1.5, 0.9)
	tone(t, music, 220, 6.0, 0.5)

	th, ratio := sidechainParams(-9)
	measure := func(duck bool, name string) (during, tail float64) {
		out := filepath.Join(dir, name+".wav")
		var g string
		if duck {
			sc := fmt.Sprintf("[music][voice]sidechaincompress=threshold=%.4f:ratio=%.2f:attack=5:release=900:makeup=1[ducked]",
				th, ratio)
			// 与 Mix 一致的人声链路：带 adelay（台词按镜头起点铺轨）+ 补静音
			g = "[1:a]aresample=24000,atrim=0:6,aformat=sample_fmts=fltp:channel_layouts=stereo[music];" +
				"[0:a]aresample=24000,adelay=1000|1000,atrim=0:6,apad=pad_len=120000," +
				"aformat=sample_fmts=fltp:channel_layouts=stereo[voice];" + sc + ";[ducked]aresample=24000[out]"
		} else {
			// 不闪避：直接输出配乐本身（不留未连接的滤镜输出）
			g = "[1:a]aresample=24000,atrim=0:6,aformat=sample_fmts=fltp:channel_layouts=stereo[out]"
		}
		if _, err := ffx.Run(3*time.Minute, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-i", voice, "-i", music, "-filter_complex", g, "-map", "[out]",
			"-ar", "24000", "-ac", "1", out); err != nil {
			t.Fatalf("生成 %s 失败: %v", name, err)
		}
		// 人声段（1.1-2.4s，人声 1.0-2.5s）与尾段（4.5-5.5s，已过 release）
		return rmsWindow(t, out, 1.1, 2.4), rmsWindow(t, out, 4.5, 5.5)
	}

	duringFlat, tailFlat := measure(false, "flat")
	duringDucked, tailDucked := measure(true, "ducked")

	// 闪避应显著压低人声段的配乐
	if drop := duringFlat - duringDucked; drop < 3.0 {
		t.Errorf("人声段配乐未被压低：未闪避 %.1f dB，闪避后 %.1f dB（仅降 %.1f dB，应 >3dB）",
			duringFlat, duringDucked, drop)
	} else {
		t.Logf("实测：人声段配乐 %.1f → %.1f dB（压低 %.1f dB）", duringFlat, duringDucked, drop)
	}
	// 尾段应基本不受影响：证明是"响应人声"而不是整轨调小
	if diff := math.Abs(tailFlat - tailDucked); diff > 2.0 {
		t.Errorf("无人声段也被改变了 %.1f dB（闪避应只在人声出现时生效）", diff)
	}
}

// TestMixPadsToFullLength 多轨混音必须补满目标时长。
//
// 回归点：老构建（ffmpeg 3.x）的 amix 一有输入结束就停止输出，
// "2 秒人声 + 6 秒配乐"曾得到 2 秒音轨 —— 成片后半段整段无声。
func TestMixPadsToFullLength(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := t.TempDir()
	short := filepath.Join(dir, "short.wav")
	long := filepath.Join(dir, "long.wav")
	tone(t, short, 440, 2.0, 0.4)
	tone(t, long, 220, 6.0, 0.3)

	out := filepath.Join(dir, "out.wav")
	res, err := Mix(MixRequest{
		VoiceTracks: []Track{{Path: short, StartSec: 0}},
		MusicTracks: []Track{{Path: long, StartSec: 0, GainDB: -12}},
		TotalSec:    6.0,
		Out:         out,
		DuckDB:      -9,
	}, Options{SampleRate: 24000, Channels: 2, SkipNormalize: true})
	if err != nil {
		t.Fatalf("混音失败: %v", err)
	}
	if math.Abs(res.DurationSec-6.0) > 0.2 {
		t.Fatalf("混音时长 %.2fs，应为 6.0s（amix 会因短输入提前结束，必须逐轨补满）", res.DurationSec)
	}
	// 人声结束后的段落必须有声音（配乐仍在）
	tail := rmsWindow(t, out, 4.0, 5.5)
	if tail < -60 {
		t.Errorf("尾段近乎静音（%.1f dBFS）：配乐没有铺满，成片后半段会无声", tail)
	}
	// 单轨场景同样要对齐时长
	out2 := filepath.Join(dir, "out2.wav")
	res2, err := Mix(MixRequest{
		VoiceTracks: []Track{{Path: short, StartSec: 0}},
		TotalSec:    5.0,
		Out:         out2,
	}, Options{SampleRate: 24000, Channels: 2, SkipNormalize: true})
	if err != nil {
		t.Fatalf("单轨混音失败: %v", err)
	}
	if math.Abs(res2.DurationSec-5.0) > 0.2 {
		t.Errorf("单轨混音时长 %.2fs，应为 5.0s（需补静音到成片长度）", res2.DurationSec)
	}
}

// TestMixNoMusicReportsNote 没有配乐时必须明确告知，不能静默出干片。
func TestMixNoMusicReportsNote(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := t.TempDir()
	voice := filepath.Join(dir, "voice.wav")
	tone(t, voice, 440, 2.0, 0.5)

	res, err := Mix(MixRequest{
		VoiceTracks: []Track{{Path: voice, StartSec: 0}},
		TotalSec:    2.0,
		Out:         filepath.Join(dir, "m.wav"),
		DuckDB:      -9,
	}, Options{SampleRate: 24000, Channels: 2})
	if err != nil {
		t.Fatalf("混音失败: %v", err)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "没有配乐") {
			found = true
		}
	}
	if !found {
		t.Errorf("缺少配乐时应在 Notes 中说明，实际: %v", res.Notes)
	}
}

// TestMixRejectsMissingTrack 素材缺失必须显式失败，而不是静默少混一轨。
func TestMixRejectsMissingTrack(t *testing.T) {
	dir := t.TempDir()
	_, err := Mix(MixRequest{
		VoiceTracks: []Track{{Path: filepath.Join(dir, "nope.wav")}},
		TotalSec:    2.0,
		Out:         filepath.Join(dir, "m.wav"),
	}, Options{})
	if err == nil {
		t.Error("音轨文件不存在时应返回错误")
	}
}

// TestMixRejectsUnknownTotal 目标时长未知时不能混音（否则无法与画面对齐）。
func TestMixRejectsUnknownTotal(t *testing.T) {
	dir := t.TempDir()
	voice := filepath.Join(dir, "v.wav")
	tone(t, voice, 440, 1.0, 0.5)
	if _, err := Mix(MixRequest{
		VoiceTracks: []Track{{Path: voice}}, Out: filepath.Join(dir, "m.wav"),
	}, Options{}); err == nil {
		t.Error("未指定目标时长时应返回错误")
	}
	if _, err := os.Stat(filepath.Join(dir, "m.wav")); err == nil {
		t.Error("校验失败时不应产出文件")
	}
}

var _ = fmt.Sprintf
