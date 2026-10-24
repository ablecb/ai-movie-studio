package audio

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aistudio/internal/ffx"
)

// TestProcessVoiceNotSilent 回归测试：台词处理后的音频绝不能变成静音。
//
// 这个测试对应一个真实踩过的坑：处理后整轨静音，而响度测量又被误读成"正常"，
// 于是一路放行到成片——成片有画面、有字幕、却没有声音。
func TestProcessVoiceNotSilent(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.wav")

	// 生成 3 秒 440Hz 测试音（等价于"有声音的 TTS 产物"）
	if _, err := ffx.Run(60e9, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=24000",
		"-af", "volume=0.5", raw); err != nil {
		t.Fatalf("生成测试音频失败: %v", err)
	}
	inPeak, err := PeakDBFS(raw)
	if err != nil {
		t.Fatalf("测量输入峰值失败: %v", err)
	}
	if inPeak <= -60 {
		t.Fatalf("测试音频本身就没有声音（峰值 %.1f dBFS）", inPeak)
	}

	for _, pitch := range []float64{1.0, 1.05, 0.92} {
		out := filepath.Join(dir, fmt.Sprintf("out_%.2f.wav", pitch))
		res, err := ProcessVoice(raw, out, VoiceOptions{
			Pitch: pitch, Speed: 1.0, Slot: 2.5, MaxSpeed: 1.15,
			SampleRate: 24000, Channels: 2,
		})
		if err != nil {
			t.Fatalf("ProcessVoice(pitch=%.2f) 失败: %v", pitch, err)
		}
		if res.Dur <= 0 || res.Speed <= 0 {
			t.Fatalf("ProcessVoice(pitch=%.2f) 返回非法结果 %+v", pitch, res)
		}
		st, err := os.Stat(out)
		if err != nil || st.Size() < 1000 {
			t.Fatalf("ProcessVoice(pitch=%.2f) 输出文件异常: %v", pitch, err)
		}
		outPeak, perr := PeakDBFS(out)
		if perr != nil {
			t.Fatalf("测量输出峰值失败: %v", perr)
		}
		if outPeak <= -60 {
			t.Errorf("ProcessVoice(pitch=%.2f) 处理后静音：峰值 %.1f dBFS（输入 %.1f）", pitch, outPeak, inPeak)
		}
	}
}

// TestProcessVoiceTrimsSilence 验证首尾静音裁剪：
// TTS 产物普遍带首尾静音，不裁掉会虚报时长 → 超槽判断被带偏 → 无谓加速。
func TestProcessVoiceTrimsSilence(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := t.TempDir()
	padded := filepath.Join(dir, "padded.wav")
	// 0.6s 静音 + 2.0s 正弦 + 0.5s 静音：模拟带首尾静音的 TTS 产物
	// （老构建的 anullsrc 没有 duration 选项，时长用输入侧 -t 限制）
	if _, err := ffx.Run(60e9, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", "0.6", "-i", "anullsrc=r=24000:cl=mono",
		"-f", "lavfi", "-t", "2", "-i", "sine=frequency=440:sample_rate=24000",
		"-f", "lavfi", "-t", "0.5", "-i", "anullsrc=r=24000:cl=mono",
		"-filter_complex", "[0:a][1:a][2:a]concat=n=3:v=0:a=1,volume=0.5",
		"-ar", "24000", "-ac", "1", padded); err != nil {
		t.Fatalf("生成带静音的测试音频失败: %v", err)
	}
	srcDur, err := ProbeDuration(padded)
	if err != nil {
		t.Fatalf("探测源时长失败: %v", err)
	}

	// 不裁剪：Slot 足够时不得加速
	outKeep := filepath.Join(dir, "keep.wav")
	res, err := ProcessVoice(padded, outKeep, VoiceOptions{SampleRate: 24000, Channels: 1})
	if err != nil {
		t.Fatalf("ProcessVoice(不裁剪) 失败: %v", err)
	}
	if res.Speed != 1.0 {
		t.Errorf("未启用裁剪且槽位充足时不应加速：speed=%.3f", res.Speed)
	}

	// 裁剪：输出时长应≈2.0s（正弦段），而不是 3.1s
	outTrim := filepath.Join(dir, "trim.wav")
	res, err = ProcessVoice(padded, outTrim, VoiceOptions{
		SampleRate: 24000, Channels: 1, TrimSilence: true,
	})
	if err != nil {
		t.Fatalf("ProcessVoice(裁剪) 失败: %v", err)
	}
	if res.Dur > 2.3 || res.Dur < 1.7 {
		t.Errorf("裁剪后时长 %.2fs，预期≈2.0s（源 %.2fs）", res.Dur, srcDur)
	}
	if res.Dur >= srcDur-0.3 {
		t.Errorf("裁剪没有生效：%.2fs → %.2fs", srcDur, res.Dur)
	}

	// 裁剪 + 紧槽位：加速决策应基于裁后时长（3.1s 放不进 2.2s，
	// 但裁后的 2.0s 能放进——不得触发加速）
	outFit := filepath.Join(dir, "fit.wav")
	res, err = ProcessVoice(padded, outFit, VoiceOptions{
		SampleRate: 24000, Channels: 1, TrimSilence: true, Slot: 2.2, MaxSpeed: 1.15,
	})
	if err != nil {
		t.Fatalf("ProcessVoice(裁剪+槽位) 失败: %v", err)
	}
	if res.Speed > 1.01 {
		t.Errorf("裁后时长本可放进槽位却被加速：speed=%.3f, dur=%.2f", res.Speed, res.Dur)
	}

	// 整段静音必须被拦下（否则静音台词会一路混进成片）
	silent := filepath.Join(dir, "silent.wav")
	if _, err := ffx.Run(60e9, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", "2", "-i", "anullsrc=r=24000:cl=mono", silent); err != nil {
		t.Fatalf("生成静音失败: %v", err)
	}
	if _, err := ProcessVoice(silent, filepath.Join(dir, "silent_out.wav"), VoiceOptions{
		SampleRate: 24000, Channels: 1, TrimSilence: true,
	}); err == nil {
		t.Errorf("整段静音未被拦截")
	}
}

// TestSilenceBoundsParsing 校验 silencedetect 输出解析的各边界组合。
func TestSilenceBoundsParsing(t *testing.T) {
	// 用一个假的 ffmpeg 路径不可行，这里直接测解析逻辑的关键分支：
	// silenceBounds 依赖 ffx.Run，无法注入；改为验证"解析函数行为"通过
	// 典型输出的正则提取（与实现同源的正则）。
	cases := []struct {
		name   string
		out    string
		starts int
		ends   int
	}{
		{"头部+尾部静音", "[silencedetect @ 0x1] silence_start: 0.012\n[silencedetect @ 0x1] silence_end: 0.31 | silence_duration: 0.298\n[silencedetect @ 0x1] silence_start: 2.7\n[silencedetect @ 0x1] silence_end: 3.0 | silence_duration: 0.3\n", 2, 2},
		{"仅尾部未闭合", "[silencedetect @ 0x1] silence_start: 1.844\n", 1, 0},
		{"无静音", "no silence here\n", 0, 0},
	}
	for _, c := range cases {
		starts := reSilenceStart.FindAllStringSubmatch(c.out, -1)
		ends := reSilenceEnd.FindAllStringSubmatch(c.out, -1)
		if len(starts) != c.starts || len(ends) != c.ends {
			t.Errorf("%s: starts=%d(期望%d) ends=%d(期望%d)", c.name, len(starts), c.starts, len(ends), c.ends)
		}
	}
}

// TestMeasureLoudnessParsing 校验响度测量能解析当前 ffmpeg 的输出格式。
func TestMeasureLoudnessParsing(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	if !ffx.Detect().HasEbur128 {
		t.Skip("当前 ffmpeg 无 ebur128")
	}
	dir := t.TempDir()
	loud := filepath.Join(dir, "loud.wav")
	if _, err := ffx.Run(60e9, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=24000",
		"-af", "volume=0.5", loud); err != nil {
		t.Fatalf("生成测试音频失败: %v", err)
	}
	lufs, err := MeasureLoudness(loud)
	if err != nil {
		t.Fatalf("测量响度失败: %v", err)
	}
	// 0.5 幅值正弦（峰值约 -6 dBFS）的积分响度实测约 -24 LUFS，
	// 不同 ffmpeg 版本有 1-2 LU 偏差，因此区间取得较宽。
	if lufs > -5 || lufs < -35 {
		t.Errorf("响度测量值异常: %.1f LUFS（预期 -35..-5）", lufs)
	}
	// 关键回归点：老构建（ffmpeg 3.x）的 ebur128 会明显低估峰值，
	// 因此这里先实测源文件峰值，再断言测量函数给出的值与之接近（±3dB）。
	srcPeak, perr := PeakDBFS(loud)
	if perr != nil {
		t.Fatalf("测量源峰值失败: %v", perr)
	}
	if srcPeak <= -60 {
		t.Fatalf("测试音频本身无声: %.1f dBFS", srcPeak)
	}
	// 用 volumedetect 作为独立参照
	ref := referencePeakDB(loud)
	if ref != 0 && math.Abs(srcPeak-ref) > 3 {
		t.Errorf("峰值测量与 volumedetect 不一致: astats %.1f vs volumedetect %.1f dBFS", srcPeak, ref)
	}
}

// referencePeakDB 用 volumedetect 独立测峰值，作为测量函数的交叉验证。
func referencePeakDB(path string) float64 {
	out, err := ffx.Run(60e9, "ffmpeg", "-hide_banner", "-i", path, "-af", "volumedetect", "-f", "null", "-")
	if err != nil && out == "" {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "max_volume:"); i >= 0 {
			s := strings.TrimSpace(line[i+len("max_volume:"):])
			s = strings.TrimSuffix(s, "dB")
			s = strings.TrimSpace(s)
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// TestNormalizeTwoPassNeverSilences 回归测试：响度归一化绝不能把音频变成静音。
//
// 真实踩过的坑：测量偏差导致增益被算得过大，推爆后经限幅器压成静音——
// "归一化把声音弄没了"。两遍 loudnorm（唯一保留的归一化路径）必须带回归护栏。
func TestNormalizeTwoPassNeverSilences(t *testing.T) {
	if err := ffx.Preflight(); err != nil {
		t.Skipf("环境不具备 ffmpeg 能力: %v", err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	if _, err := ffx.Run(60e9, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=24000",
		"-af", "volume=0.5", src); err != nil {
		t.Fatalf("生成测试音频失败: %v", err)
	}
	before, berr := PeakDBFS(src)
	if berr != nil {
		t.Fatalf("测量归一化前峰值失败: %v", berr)
	}
	if before <= -60 {
		t.Fatalf("测试音频本身无声: %.1f dBFS", before)
	}
	opt := Options{SampleRate: 24000, Channels: 2, TargetLoudness: -14, TruePeakDB: -1}
	if err := normalizeTwoPass(src, opt); err != nil {
		t.Fatalf("两遍响度归一化失败: %v", err)
	}
	after, aerr := PeakDBFS(src)
	if aerr != nil {
		t.Fatalf("测量归一化后峰值失败: %v", aerr)
	}
	if after <= -60 {
		t.Errorf("归一化后音频变成静音：峰值 %.1f → %.1f dBFS", before, after)
	}
}

// TestAtempoChain 确保串联 atempo 覆盖超范围比值且每段都在合法区间内。
func TestAtempoChain(t *testing.T) {
	cases := []struct {
		ratio float64
		want  int
	}{
		{1.0, 0},
		{1.2, 1},
		{0.8, 1},
		{3.5, 2},
		{0.2, 3},
	}
	for _, c := range cases {
		got := atempoChain(c.ratio)
		if len(got) != c.want {
			t.Errorf("atempoChain(%.2f) 段数=%d，期望 %d（%v）", c.ratio, len(got), c.want, got)
		}
	}
}
