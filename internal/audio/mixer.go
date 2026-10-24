// Package audio 实现生产级声音后期：多轨混音、侧链闪避、响度归一化。
//
// 为什么这些是"专业级"的硬指标：
//
//   - 对白必须清晰：BGM 压过人声是最业余的错误。行业做法是侧链压缩（人声一出现，
//     配乐自动让路 6-10dB），而不是简单把 BGM 调小
//   - 响度必须达标：流媒体平台统一按 -14 LUFS 回放，不归一化就会出现
//     "别人的片子比我的响"的观感差距
//   - 峰值必须受限：超过 -1 dBTP 会在有损编码后削波，产生刺耳失真
//
// 同时本包承担 ffmpeg 版本兼容：现实环境里 ffmpeg 版本差异很大（有 2016 年的老构建），
// 关键滤镜的参数语法变化必须探测后适配，否则会在"看起来应该能跑"的地方崩掉。
package audio

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"aistudio/internal/ffx"
)

// Options 混音参数。
type Options struct {
	SampleRate     int
	Channels       int
	TargetLoudness float64 // LUFS，流媒体标准 -14
	TruePeakDB     float64 // dBTP，通常 -1.0
	// VoiceCompress 对白总线轻压缩（默认开启）。TTS 逐句合成的电平天然不一致，
	// 同一角色忽大忽小；轻参数（3:1、低阈值）只稳住底部，不压平表演起伏。
	VoiceCompress bool
	// SkipNormalize 跳过响度归一化。
	// 用途：需要验证混音**相对关系**（例如侧链闪避到底压了多少 dB）时，
	// 归一化会把整体电平拉回目标值，掩盖真实的相对变化。
	SkipNormalize bool
}

func (o Options) withDefaults() Options {
	if o.SampleRate <= 0 {
		o.SampleRate = 24000
	}
	if o.Channels <= 0 {
		o.Channels = 2
	}
	if o.TargetLoudness == 0 {
		o.TargetLoudness = -14
	}
	if o.TruePeakDB == 0 {
		o.TruePeakDB = -1.0
	}
	return o
}

// Track 一条待混入的音轨。
type Track struct {
	Path     string
	StartSec float64
	GainDB   float64
	Loop     bool // 循环铺满（BGM 通常短于成片）
	// DurSec 素材实测时长。0 表示由 Mix 自动探测。
	// 显式提供可省一次探测；但绝不能瞎填——补静音的样本数由它算出。
	DurSec float64
	// EndSec 轨道在成片时间轴上的截断点（秒，绝对时间）。0 = 不截断。
	//
	// 用途：cue 对齐场景边界时，长素材必须在场景结束处收掉，
	// 否则会漫进下一条 cue 的时间段，两段配乐叠加成"同时响两首"。
	// 截断作用于源素材（adelay 之前），span = EndSec - StartSec。
	EndSec float64
	// FadeOutSec 结尾淡出时长（秒，0=不淡出）。配乐在场景边界收尾时
	// 2s 淡出替代戛然而止；淡出作用于截断后的源素材，最长不超过 span 的一半。
	FadeOutSec float64
}

// MixRequest 一次混音请求。
type MixRequest struct {
	RunDir      string
	VoiceTracks []Track // 逐句台词（已单独处理过音色/语速）
	MusicTracks []Track // BGM
	SFXTracks   []Track // 音效
	TotalSec    float64 // 目标总时长（=成片实测时长）
	Out         string
	// DuckDB 人声出现时 BGM 的压低量（dB，负值）。0 = 不启用侧链闪避。
	// 作为请求参数而非全局常量：既能按片调节，也便于用测试证明闪避确实生效。
	DuckDB float64
	// DebugFilterGraph 非 nil 时回填生成的 filter_complex 字符串。
	// 滤镜图是字符串拼出来的，出问题时唯一的有效手段就是把它打出来。
	DebugFilterGraph *string
}

// MixResult 混音结果与实测指标。
type MixResult struct {
	Out          string   `json:"out"`
	DurationSec  float64  `json:"duration_sec"`
	LoudnessLUFS float64  `json:"loudness_lufs"`
	TruePeakDB   float64  `json:"true_peak_db"`
	VoiceCount   int      `json:"voice_count"`
	MusicCount   int      `json:"music_count"`
	SFXCount     int      `json:"sfx_count"`
	Notes        []string `json:"notes,omitempty"`
}

// Mix 多轨混音 + 响度归一化。
//
// 实现要点：
//   - 人声先混成 [voice] 子混音，再用它作为侧链触发源压低 BGM
//   - amix 使用 normalize=0：否则 ffmpeg 会按输入数量自动衰减，越加轨越小声
//   - 最后统一响度归一化（loudnorm；老构建降级为测量+增益+限幅）
func Mix(req MixRequest, opt Options) (*MixResult, error) {
	opt = opt.withDefaults()
	if req.TotalSec <= 0 {
		return nil, fmt.Errorf("混音需要明确的目标时长（TotalSec），否则无法保证与画面对齐")
	}
	if req.Out == "" {
		return nil, fmt.Errorf("缺少输出路径")
	}
	all := make([]Track, 0, len(req.VoiceTracks)+len(req.MusicTracks)+len(req.SFXTracks))
	all = append(all, req.VoiceTracks...)
	all = append(all, req.MusicTracks...)
	all = append(all, req.SFXTracks...)
	// 存在性校验（本地 stat，快）串行做；实测时长（每次一个 ffprobe 子进程）
	// 并行补齐——台词多时轨道可达几十条，串行探测的等待不可忽略
	var needProbe []int
	for i := range all {
		if !FileExists(all[i].Path) {
			return nil, fmt.Errorf("音轨文件不存在: %s", all[i].Path)
		}
		// 补齐实测时长：补静音的样本数由它算出，不能靠猜
		if all[i].DurSec <= 0 {
			needProbe = append(needProbe, i)
		}
	}
	paths := make([]string, len(needProbe))
	for j, i := range needProbe {
		paths[j] = all[i].Path
	}
	durs, errs := ffx.ProbeBatch(paths, 0)
	for j, i := range needProbe {
		// 探测失败保持 0，与原实现一致：混音的补静音按 TotalSec 计算，不依赖单轨时长
		if errs[j] == nil {
			all[i].DurSec = durs[j]
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("没有任何音轨可混")
	}
	req.VoiceTracks = all[:len(req.VoiceTracks)]
	req.MusicTracks = all[len(req.VoiceTracks) : len(req.VoiceTracks)+len(req.MusicTracks)]
	req.SFXTracks = all[len(req.VoiceTracks)+len(req.MusicTracks):]

	var inputs []string
	var filters []string
	noDuck := ""
	label := func(i int, tag string) string { return fmt.Sprintf("[%s%d]", tag, i) }

	voiceIdx, musicIdx, sfxIdx := []int{}, []int{}, []int{}
	for _, t := range req.VoiceTracks {
		voiceIdx = append(voiceIdx, len(inputs))
		inputs = append(inputs, t.Path)
	}
	for _, t := range req.MusicTracks {
		musicIdx = append(musicIdx, len(inputs))
		inputs = append(inputs, t.Path)
	}
	for _, t := range req.SFXTracks {
		sfxIdx = append(sfxIdx, len(inputs))
		inputs = append(inputs, t.Path)
	}

	// 侧链闪避的启用条件：有配乐 + 有对白 + 显式配置了压低量 + 滤镜可用。
	// 三者缺一就退化为纯叠加，并在 Notes 里说明，避免"以为做了闪避其实没做"。
	useDuck := len(musicIdx) > 0 && len(voiceIdx) > 0 && req.DuckDB != 0 && ffx.Detect().HasSidechain
	if len(musicIdx) > 0 && len(voiceIdx) > 0 && req.DuckDB != 0 && !ffx.Detect().HasSidechain {
		noDuck = "当前 ffmpeg 无 sidechaincompress：配乐未做人声闪避，BGM 可能压住对白"
	} else if len(musicIdx) > 0 && len(voiceIdx) > 0 && req.DuckDB == 0 {
		noDuck = "audio.duck_db 为 0：配乐未做人声闪避（对白清晰度需人工确认）"
	}
	// 每条轨都要在进 amix 之前补满目标时长（见 finishInput）：
	// 老构建（ffmpeg 3.x）的 amix 在任一输入提前 EOF 时不是"停止输出"，
	// 而是直接崩溃（"Cannot allocate memory"），且部分构建崩溃后仍 exit 0、
	// 只写出崩溃前的部分内容——补在 amix 之后救不了已经崩掉的 amix。
	// 子混音输出因此天然恰好 TotalSec，总线无需再补。
	norm := func(in, out string) string {
		return fmt.Sprintf("%saformat=sample_fmts=fltp:channel_layouts=%s%s",
			in, channelLayout(opt.Channels), out)
	}

	// ---- 人声子混音 ----
	// 需要闪避时把人声分成两路：一路给侧链做触发源，一路进总线。
	// 否则 sidechaincompress 会独占人声标签，总线的 amix 就再也引用不到它。
	voiceOut, voiceKey := "", ""
	if len(voiceIdx) > 0 {
		voiceKey = "voice" // 侧链触发源
		if useDuck {
			voiceOut = "[voicebus]" // 进总线那一路的最终标签
		} else {
			voiceOut = "[voice]"
		}
	}
	if len(voiceIdx) > 0 {
		var parts []string
		for n, i := range voiceIdx {
			ms := int(math.Max(0, req.VoiceTracks[n].StartSec) * 1000)
			chain := withDelay(fmt.Sprintf("aresample=%d", opt.SampleRate), ms)
			chain += fmt.Sprintf(",volume=%s", dbToVolume(req.VoiceTracks[n].GainDB))
			parts = append(parts, fmt.Sprintf("[%d:a]%s%s", i, finishInput(chain, req.TotalSec, opt), label(i, "v")))
		}
		var voiceMix string
		if len(voiceIdx) == 1 {
			voiceMix = fmt.Sprintf("[v%d]anull[vmix]", voiceIdx[0])
		} else {
			var refs []string
			for _, i := range voiceIdx {
				refs = append(refs, label(i, "v"))
			}
			voiceMix = AmixFilter(refs, "vmix")
		}
		parts = append(parts, voiceMix)

		// 对白总线轻压缩：TTS 逐句合成的电平天然不一致（同一角色忽大忽小），
		// 轻参数（3:1、低阈值、慢启动）稳住底部，不做重压——"压平对白起伏"
		// 是纪录片式灾难，表演的动态要保留。
		vmixSrc := "[vmix]"
		if opt.VoiceCompress {
			parts = append(parts,
				"[vmix]acompressor=threshold=0.06:ratio=3:attack=12:release=180:makeup=1[vmixc]")
			vmixSrc = "[vmixc]"
		}

		// 总线用的人声：子混音输出已恰好 TotalSec，直接规整格式
		busVoice := voiceOut
		if useDuck {
			// 人声要同时用于"进总线"和"作侧链触发"两份用途，
			// 而一个滤镜输出 pad 只能被消费一次 —— 必须显式 asplit 分叉。
			//
			// 两条路都必须补满目标时长，尤其**触发路**：
			// sidechaincompress 会跟随触发输入的结束而停止输出，
			// 触发源提前结束时配乐在那一刻之后整段消失。
			parts = append(parts, fmt.Sprintf("%sasplit=2[voicedry][voicekeyraw]", vmixSrc))
			parts = append(parts, norm("[voicedry]", "[voicedryf]"))
			parts = append(parts, norm("[voicekeyraw]", "[voicekeyf]"))
			voiceKey = "voicekeyf"
			busVoice = "[voicedryf]"
		} else {
			parts = append(parts, norm(vmixSrc, "[voice]"))
			busVoice = "[voice]"
		}
		voiceOut = busVoice
		filters = append(filters, strings.Join(parts, ";"))
	}

	// ---- 音乐子混音（含侧链闪避）----
	musicLabel := ""
	if len(musicIdx) > 0 {
		var parts []string
		var refs []string
		for n, i := range musicIdx {
			t := req.MusicTracks[n]
			chain := fmt.Sprintf("aresample=%d,volume=%s", opt.SampleRate, dbToVolume(t.GainDB))
			if t.Loop && hasAloop() {
				// 循环铺满整片：BGM 通常短于成片，不循环则后半段无声
				chain += ",aloop=loop=-1:size=2e9"
			}
			// cue 对齐场景边界：截断必须发生在 adelay 之前的源素材上
			//（atrim 在 adelay 之后会把"延迟后的静音头"算进时长，截错位置）。
			// span 用 EndSec-StartSec：素材在场景结束处收掉，不漫入下一条 cue。
			if t.EndSec > t.StartSec {
				chain += fmt.Sprintf(",atrim=0:%.3f,asetpts=PTS-STARTPTS", t.EndSec-t.StartSec)
			}
			// 场景尾淡出：配乐在场景边界"戛然而止"是 AI 电影一眼假的细节之一，
			// 2 秒淡出让音乐转折落在切换点上而不是撞在切换点上
			if t.FadeOutSec > 0.05 && t.EndSec > t.StartSec {
				span := t.EndSec - t.StartSec
				d := math.Min(t.FadeOutSec, span*0.5)
				chain += fmt.Sprintf(",afade=t=out:st=%.3f:d=%.3f", span-d, d)
			}
			chain = withDelay(chain, int(math.Max(0, t.StartSec)*1000))
			parts = append(parts, fmt.Sprintf("[%d:a]%s%s", i, finishInput(chain, req.TotalSec, opt), label(i, "m")))
			refs = append(refs, label(i, "m"))
		}
		if len(refs) == 1 {
			parts = append(parts, fmt.Sprintf("%sanull[musicmix]", refs[0]))
		} else {
			parts = append(parts, AmixFilter(refs, "musicmix"))
		}
		// 配乐子混音输出已恰好 TotalSec（同人声，见 finishInput）
		parts = append(parts, norm("[musicmix]", "[musicnorm]"))
		musicSrc := "[musicnorm]"
		if useDuck {
			// 人声出现时 BGM 让路。
			//
			// 接线方向极易搞错：input#0 是**被压缩的信号**（配乐），input#1 才是触发信号（人声）。
			// 接反了会变成"压人声"，听感上是"对白忽大忽小"。
			//
			// 参数映射见 sidechainParams：threshold 固定在较低值（保证触发可靠），
			// 用 ratio 控制压低量；阈值取得过高时配乐会稳在阈值下方而完全不触发
			// （实测 threshold=0.3 时压低量 0dB）。
			th, ratio := sidechainParams(req.DuckDB)
			parts = append(parts, fmt.Sprintf(
				"%s[%s]sidechaincompress=threshold=%.4f:ratio=%.2f:attack=20:release=350:makeup=1[music]",
				musicSrc, voiceKey, th, ratio))
		} else {
			parts = append(parts, musicSrc+"anull[music]")
		}
		filters = append(filters, strings.Join(parts, ";"))
		musicLabel = "[music]"
	}

	// ---- 音效子混音 ----
	sfxLabel := ""
	if len(sfxIdx) > 0 {
		var parts []string
		var refs []string
		for n, i := range sfxIdx {
			t := req.SFXTracks[n]
			chain := withDelay(fmt.Sprintf("aresample=%d", opt.SampleRate), int(math.Max(0, t.StartSec)*1000))
			chain += fmt.Sprintf(",volume=%s", dbToVolume(t.GainDB))
			parts = append(parts, fmt.Sprintf("[%d:a]%s%s", i, finishInput(chain, req.TotalSec, opt), label(i, "s")))
			refs = append(refs, label(i, "s"))
		}
		if len(refs) == 1 {
			parts = append(parts, fmt.Sprintf("%sanull[sfxmix]", refs[0]))
		} else {
			parts = append(parts, AmixFilter(refs, "sfxmix"))
		}
		parts = append(parts, norm("[sfxmix]", "[sfxnorm]"))
		sfxLabel = "[sfxnorm]"
		filters = append(filters, strings.Join(parts, ";"))
	}

	// ---- 总混音 ----
	var busRefs []string
	for _, l := range []string{voiceOut, musicLabel, sfxLabel} {
		if l != "" {
			busRefs = append(busRefs, l)
		}
	}
	// 各子混音先统一采样格式与声道布局再进总线：
	// 否则 amix 会因输入格式不一致而无法选择输出格式（老构建会直接报错）。
	if len(busRefs) > 0 {
		var norm []string
		for n, l := range busRefs {
			norm = append(norm, fmt.Sprintf("%saformat=sample_fmts=fltp:channel_layouts=%s[n%d]",
				l, channelLayout(opt.Channels), n))
		}
		filters = append(filters, strings.Join(norm, ";"))
		var nrefs []string
		for n := range busRefs {
			nrefs = append(nrefs, fmt.Sprintf("[n%d]", n))
		}
		busRefs = nrefs
	}
	var bus string
	if len(busRefs) == 1 {
		bus = fmt.Sprintf("%sanull[bus]", busRefs[0])
	} else {
		bus = AmixFilter(busRefs, "bus")
	}
	// 总时长对齐成片：先裁到目标长度，再补静音到目标长度。
	//
	// 顺序与参数名都踩过坑：
	//   - 老构建（ffmpeg 3.x）上 `bus,apad,atrim=0:N` 会把输出截断在"最后一段有声音的
	//     位置"（例如 2s），成片后半段直接无声 —— 必须先 atrim 再 apad
	//   - apad 的"补到多长"参数在新版本叫 whole_dur（秒），老版本只有 whole_len（样本数）
	// 总线已由各输入补满，这里只做限长兜底（不再补静音，避免与逐轨补静音重复）
	bus += fmt.Sprintf(";[bus]atrim=0:%.3f,aresample=%d,aformat=sample_fmts=fltp:channel_layouts=%s[loudin]",
		req.TotalSec, opt.SampleRate, channelLayout(opt.Channels))
	filters = append(filters, bus)

	useLoudnorm := ffx.HasLoudnorm() && !opt.SkipNormalize
	// 响度归一化不在混音图内做：单遍 loudnorm 是动态模式（实时调增益），
	// 音乐尾音会被"泵"一下、语音起伏被压平。混音先原样产出，
	// 随后用两遍 loudnorm（测量 → 线性增益）做广播级归一化，见 normalizeTwoPass。
	filters = append(filters, "[loudin]anull[out]")

	args := []string{"-hide_banner", "-y"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	graph := strings.Join(filters, ";")
	if req.DebugFilterGraph != nil {
		*req.DebugFilterGraph = graph
	}
	args = append(args,
		"-filter_complex", graph,
		"-map", "[out]",
		"-ar", strconv.Itoa(opt.SampleRate),
		"-ac", strconv.Itoa(opt.Channels),
		// 输出端再钉一次时长：apad 补的静音在部分老构建上不会被后续滤镜继续拉长
		// （表现为"音轨只到最后一句话结束"），-t 是确定性的兜底。
		"-t", fmt.Sprintf("%.3f", req.TotalSec),
		req.Out)

	if _, err := ffx.Run(20*time.Minute, "ffmpeg", args...); err != nil {
		return nil, fmt.Errorf("混音失败: %w", err)
	}
	res := &MixResult{
		Out: req.Out, VoiceCount: len(voiceIdx),
		MusicCount: len(musicIdx), SFXCount: len(sfxIdx),
	}
	if useLoudnorm {
		// 严格模式：归一化失败即失败——"保持原混音"会让响度指标静默不达标
		if err := normalizeTwoPass(req.Out, opt); err != nil {
			return nil, fmt.Errorf("两遍响度归一化失败: %w", err)
		}
	} else if opt.SkipNormalize {
		res.Notes = append(res.Notes, "已跳过响度归一化（SkipNormalize）")
	} else {
		// 严格模式：缺 loudnorm 拒绝降级归一化（「测量+增益+限幅」没有动态范围
		// 控制，响度看似达标但质感与广播级两遍归一化不同，属于静默的产物劣化）。
		return nil, fmt.Errorf("当前 ffmpeg 缺少 loudnorm 滤镜，拒绝降级归一化：请升级 ffmpeg（4.1+）；确需跳过归一化请显式设置 SkipNormalize")
	}
	// 一律以实测为准：JSON 解析可能受版本输出格式影响
	if lufs, err := MeasureLoudness(req.Out); err == nil && lufs != 0 {
		res.LoudnessLUFS = lufs
	}
	if peak, err := PeakDBFS(req.Out); err == nil {
		res.TruePeakDB = peak
	}
	if p, err := ffx.ProbeFile(req.Out); err == nil {
		res.DurationSec = p.DurationSec
	}
	// 终检：混音时长必须与成片对齐。老构建的滤镜崩溃可能只写出前几秒，
	// 缺这道闸，残缺混音会带着"混音完成"的日志一路混进成片。
	if res.DurationSec > 0 && res.DurationSec < req.TotalSec-0.5 {
		return nil, fmt.Errorf("混音时长 %.2fs 短于成片 %.2fs（滤镜链提前结束，产物残缺不可信）",
			res.DurationSec, req.TotalSec)
	}
	if len(musicIdx) == 0 {
		res.Notes = append(res.Notes, "本次混音没有配乐轨：成片只有人声，观感偏干（配乐/环境声缺失）")
	}
	if noDuck != "" {
		res.Notes = append(res.Notes, noDuck)
	}
	return res, nil
}

// normalizeTwoPass 两遍 loudnorm（EBU R128 广播级做法）：
// 第一遍只测量（input_i/tp/lra/thresh + target_offset），第二遍带 measured_* +
// linear=true 做**纯线性增益**。单遍 loudnorm（此前实现）是动态模式：它在整条
// 音轨上实时调增益，音乐尾音会被"泵"一下、对白起伏被压平——linear=true 在
// 没有 measured_* 参数时是无效参数，等于白写。两遍模式消除泵感，且实测输出
// 必须更接近目标、不能变成静音（护栏：归一化只许改善，不许把声音弄没）。
func normalizeTwoPass(path string, opt Options) error {
	measureOut, err := ffx.Run(10*time.Minute, "ffmpeg", "-hide_banner", "-nostats", "-i", path,
		"-af", fmt.Sprintf("loudnorm=I=%.1f:TP=%.1f:LRA=11:print_format=json", opt.TargetLoudness, opt.TruePeakDB),
		"-f", "null", "-")
	if err != nil && measureOut == "" {
		return err
	}
	m := parseLoudnormJSON(measureOut)
	if m == nil || m.InputI == 0 {
		return fmt.Errorf("loudnorm 测量输出无法解析")
	}
	if m.InputI < -60 {
		return fmt.Errorf("实测响度 %.1f LUFS 低于可信下限（-60），放弃归一化以免破坏音频", m.InputI)
	}
	tmp := path + ".norm.wav"
	chain := fmt.Sprintf(
		"loudnorm=I=%.1f:TP=%.1f:LRA=11:measured_I=%.2f:measured_TP=%.2f:measured_LRA=%.2f:measured_thresh=%.2f:offset=%.2f:linear=true",
		opt.TargetLoudness, opt.TruePeakDB, m.InputI, m.InputTP, m.InputLRA, m.InputThresh, m.TargetOffset)
	if _, err := ffx.Run(20*time.Minute, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-i", path, "-af", chain,
		"-ar", strconv.Itoa(opt.SampleRate), "-ac", strconv.Itoa(opt.Channels), tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	after, mErr := MeasureLoudness(tmp)
	if mErr == nil && (after < -60 || peakBelow(tmp, -60)) {
		_ = os.Remove(tmp)
		return fmt.Errorf("归一化后音频接近静音（%.1f LUFS），已回滚保持原音频", after)
	}
	if mErr == nil && math.Abs(after-opt.TargetLoudness) > math.Abs(m.InputI-opt.TargetLoudness) {
		_ = os.Remove(tmp)
		return fmt.Errorf("归一化未改善响度（%.1f → %.1f LUFS），已回滚保持原音频", m.InputI, after)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// peakBelow 判断文件峰值是否低于阈值（测量失败按不越限处理，由响度护栏兜底）。
func peakBelow(path string, threshold float64) bool {
	peak, err := PeakDBFS(path)
	return err == nil && peak < threshold
}

// loudnormStats loudnorm 的测量输出。
type loudnormStats struct {
	InputI       float64 `json:"input_i,string"`
	InputTP      float64 `json:"input_tp,string"`
	InputLRA     float64 `json:"input_lra,string"`
	InputThresh  float64 `json:"input_thresh,string"`
	OutputI      float64 `json:"output_i,string"`
	OutputTP     float64 `json:"output_tp,string"`
	TargetOffset float64 `json:"target_offset,string"`
}

func parseLoudnormJSON(out string) *loudnormStats {
	i := strings.LastIndex(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		return nil
	}
	var st loudnormStats
	if err := json.Unmarshal([]byte(out[i:j+1]), &st); err != nil {
		return nil
	}
	return &st
}

// ---------------------------------------------------------------------------
// 单条台词处理
// ---------------------------------------------------------------------------

// VoiceOptions 单条台词的声音处理参数。
type VoiceOptions struct {
	Pitch      float64 // 音高系数（<1 低沉，>1 明亮）
	Speed      float64 // 目标加速比（1.0 = 不变）
	Slot       float64 // 所属镜头时长；>0 时自动计算必要的压缩量
	MaxSpeed   float64 // 加速上限（超过会有"花栗鼠"感）
	SampleRate int
	Channels   int
	// TrimSilence 裁掉首尾静音。TTS 产物普遍带 0.1-0.3s 首尾静音：
	// 不裁掉则"实测时长"虚高，超槽判断被带偏——语速正常也会被加速，
	// 观感上就是赶话；同时入点对齐会整体提前。
	TrimSilence bool
}

// VoiceResult 单条台词的处理结果。
type VoiceResult struct {
	// Dur 处理后实测时长（秒）。
	Dur float64
	// Speed 实际生效的加速比（1.0 = 未加速）。
	Speed float64
}

// ProcessVoice 处理单条台词：首尾静音裁剪 + 音色（变调）+ 时长适配。
//
// 三件事必须按此顺序、且时长换算要分清"哪一步改时长"：
//   - 裁剪：在原始时间轴上切掉首尾静音，之后的换算全部基于裁后时长
//   - 变调用 asetrate + atempo(1/pitch) 组合，改变音色但**恢复原语速**——
//     因此变调块的输出时长等于输入时长（历史上把 expect 算成 rawDur/pitch，
//     导致高音色低估时长放任溢出、低音色高估时长无谓加速）
//   - 时长适配再用 atempo 单独处理，且受上限约束
func ProcessVoice(in, out string, opt VoiceOptions) (VoiceResult, error) {
	if opt.Pitch <= 0 {
		opt.Pitch = 1.0
	}
	if opt.MaxSpeed <= 1 {
		opt.MaxSpeed = 1.15
	}
	if opt.SampleRate <= 0 {
		opt.SampleRate = 24000
	}
	srcDur, err := ProbeDuration(in)
	if err != nil {
		return VoiceResult{}, err
	}

	// 首尾静音裁剪：探测失败按"不裁"处理——裁剪是优化项，不能让它中断配音
	trimStart, trimEnd := 0.0, srcDur
	if opt.TrimSilence {
		if s, e, ok := silenceBounds(in, srcDur); ok {
			trimStart, trimEnd = s, e
		}
	}
	effDur := trimEnd - trimStart
	if effDur < 0.25 {
		return VoiceResult{}, fmt.Errorf("TTS 音频近乎静音（有效时长 %.2fs/%.2fs），疑似合成失败", effDur, srcDur)
	}

	var chains []string
	if trimStart > 0.005 || trimEnd < srcDur-0.005 {
		// atrim 在原始时间轴上裁切；asetpts 把时间戳归零，避免输出头部留空洞
		chains = append(chains, fmt.Sprintf("atrim=start=%.3f:end=%.3f,asetpts=PTS-STARTPTS", trimStart, trimEnd))
	}
	if math.Abs(opt.Pitch-1.0) > 0.005 {
		chains = append(chains,
			fmt.Sprintf("asetrate=%d*%.4f", opt.SampleRate, opt.Pitch),
			fmt.Sprintf("aresample=%d", opt.SampleRate),
		)
		chains = append(chains, atempoChain(1/opt.Pitch)...)
	}

	expect := effDur // 变调块时长自补偿（见函数注释），期望时长=裁后时长
	speed := opt.Speed
	if speed <= 0 {
		speed = 1.0
	}
	if opt.Slot > 0 && expect/speed > opt.Slot {
		if need := expect / opt.Slot; need > speed {
			speed = need
		}
	}
	if speed > opt.MaxSpeed {
		speed = opt.MaxSpeed // 宁可持续到下一镜头，不做花栗鼠
	}
	chains = append(chains, atempoChain(speed)...)

	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in}
	if len(chains) > 0 {
		args = append(args, "-af", strings.Join(chains, ","))
	}
	args = append(args, "-ar", strconv.Itoa(opt.SampleRate),
		"-ac", strconv.Itoa(maxInt(1, opt.Channels)), out)
	if _, err := ffx.Run(5*time.Minute, "ffmpeg", args...); err != nil {
		return VoiceResult{}, err
	}
	dur, err := ProbeDuration(out)
	if err != nil {
		return VoiceResult{}, err
	}
	return VoiceResult{Dur: dur, Speed: speed}, nil
}

// atempoChain 构造 atempo 链（单次 atempo 仅支持 0.5-2.0，超出需串联）。
func atempoChain(ratio float64) []string {
	if ratio <= 0 || math.Abs(ratio-1.0) < 0.005 {
		return nil
	}
	var out []string
	for ratio > 2.0 {
		out = append(out, "atempo=2.0")
		ratio /= 2.0
	}
	for ratio < 0.5 {
		out = append(out, "atempo=0.5")
		ratio /= 0.5
	}
	out = append(out, fmt.Sprintf("atempo=%.5f", ratio))
	return out
}

var (
	reSilenceStart = regexp.MustCompile(`silence_start:\s*(-?\d+\.?\d*)`)
	reSilenceEnd   = regexp.MustCompile(`silence_end:\s*(-?\d+\.?\d*)`)
)

// silenceBounds 用 silencedetect 探测首尾静音边界，返回 (start, end, ok)：
// start=首个非静音点，end=末个非静音点之后。ok=false 表示无法判定
// （探测失败或整段皆静音），调用方按"不裁剪"处理——裁剪是优化项，
// 探测失败绝不能中断配音；整段静音则交给调用方的时长下限检查拦截。
//
// 阈值刻意保守（-45dB / 最短 0.08s）：只吃接近数字静音的部分，
// 不碰换气声和词尾拖音，否则反而会剪出"憋气感"。
func silenceBounds(path string, dur float64) (start, end float64, ok bool) {
	out, err := ffx.Run(2*time.Minute, "ffmpeg", "-hide_banner", "-nostats", "-i", path,
		"-af", "silencedetect=noise=-45dB:d=0.08", "-f", "null", "-")
	if err != nil && out == "" {
		return 0, dur, false
	}
	var starts, ends []float64
	for _, m := range reSilenceStart.FindAllStringSubmatch(out, -1) {
		if v, e := strconv.ParseFloat(m[1], 64); e == nil {
			starts = append(starts, v)
		}
	}
	for _, m := range reSilenceEnd.FindAllStringSubmatch(out, -1) {
		if v, e := strconv.ParseFloat(m[1], 64); e == nil {
			ends = append(ends, v)
		}
	}
	if len(starts) == 0 {
		return 0, dur, true // 全程无静音段：无需裁剪
	}
	start, end = 0.0, dur
	// 头部：第一段静音从文件头开始（允许极小偏移）→ 裁到它的结束点；
	// 若该静音段没有结束点，说明整段皆静音（有效时长归零，让上层拦下）
	if starts[0] <= 0.05 {
		if len(ends) > 0 {
			start = ends[0]
		} else {
			start = dur
		}
	}
	// 尾部：最后一段静音未闭合（延伸到 EOF）或其结束点贴着文件尾
	if len(ends) == len(starts)-1 {
		end = starts[len(starts)-1]
	} else if len(ends) == len(starts) && ends[len(ends)-1] >= dur-0.05 {
		end = starts[len(starts)-1]
	}
	if end < start {
		end = start
	}
	return start, end, true
}

// NormalizeLoudness 单文件响度归一化。
func NormalizeLoudness(in, out string, targetLUFS, truePeakDB float64, opt Options) (float64, error) {
	opt = opt.withDefaults()
	if targetLUFS == 0 {
		targetLUFS = opt.TargetLoudness
	}
	if truePeakDB == 0 {
		truePeakDB = opt.TruePeakDB
	}
	// 严格模式：缺 loudnorm 直接失败，不做降级归一化
	if !ffx.HasLoudnorm() {
		return 0, fmt.Errorf("当前 ffmpeg 缺少 loudnorm 滤镜，无法做响度归一化：请升级 ffmpeg（4.1+）")
	}
	if _, err := ffx.Run(20*time.Minute, "ffmpeg", "-hide_banner", "-y", "-i", in,
		"-af", fmt.Sprintf("loudnorm=I=%.1f:TP=%.1f:LRA=11:print_format=json", targetLUFS, truePeakDB),
		"-ar", strconv.Itoa(opt.SampleRate), "-ac", strconv.Itoa(opt.Channels), out); err != nil {
		return 0, err
	}
	return MeasureLoudness(out)
}

// MeasureLoudness 用 ebur128 实测积分响度（LUFS）。
// 老构建的 ebur128 没有 JSON 输出，因此解析 "I: -35.4 LUFS" 文本格式。
func MeasureLoudness(path string) (float64, error) {
	if !ffx.Detect().HasEbur128 {
		return 0, fmt.Errorf("当前 ffmpeg 无 ebur128 滤镜，无法测量响度")
	}
	out, err := ffx.Run(10*time.Minute, "ffmpeg", "-hide_banner", "-nostats", "-i", path,
		"-filter_complex", "ebur128=peak=true", "-f", "null", "-")
	if err != nil && out == "" {
		return 0, err
	}
	// Summary 段的 "I:  -35.4 LUFS" 是整段统计值
	if idx := strings.LastIndex(out, "Integrated loudness"); idx >= 0 {
		if m := reIntegrated.FindStringSubmatch(out[idx:]); len(m) == 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				return v, nil
			}
		}
	}
	// 兼容只有逐帧输出的情况：取最后一次 I 值
	matches := reIntegrated.FindAllStringSubmatch(out, -1)
	if len(matches) > 0 {
		if v, err := strconv.ParseFloat(matches[len(matches)-1][1], 64); err == nil {
			return v, nil
		}
	}
	return 0, fmt.Errorf("无法从 ebur128 输出解析响度")
}

var (
	reIntegrated = regexp.MustCompile(`I:\s*(-?\d+\.?\d*)\s*LUFS`)
	rePeak       = regexp.MustCompile(`(?:Peak|TPK|FTPK):\s*(-?\d+\.?\d*)\s*dBFS`)
	rePeakLevel  = regexp.MustCompile(`Peak level dB:\s*(-?\d+\.?\d*|inf|-inf)`)
)

// PeakDBFS 返回文件峰值。
//
// 用 astats 而不是 ebur128 的 Peak：老构建（ffmpeg 3.x）的 ebur128 真峰值实现有明显偏差
// （实测 0.6 幅值正弦被报成 -22.5 dBFS），用它做增益校正会把整轨推爆后被限幅器压成静音。
//
// 返回 error 而不是静默返回 0：调用方（交付校验）拿 0 会把"无法测量"当成
// "峰值 0 dBFS 以下"直接判通过——测量失败必须让门禁显式失败（fail-closed）。
func PeakDBFS(path string) (float64, error) {
	out, err := ffx.Run(10*time.Minute, "ffmpeg", "-hide_banner", "-nostats", "-i", path,
		"-af", "astats=metadata=0:reset=0", "-f", "null", "-")
	if err != nil && out == "" {
		return 0, err
	}
	best := math.Inf(-1)
	for _, m := range rePeakLevel.FindAllStringSubmatch(out, -1) {
		if len(m) == 2 {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > best {
				best = v
			}
		}
	}
	// astats 不可用时退回 ebur128（精度差但聊胜于无）
	if math.IsInf(best, -1) {
		if out2, err := ffx.Run(10*time.Minute, "ffmpeg", "-hide_banner", "-nostats", "-i", path,
			"-filter_complex", "ebur128=peak=true", "-f", "null", "-"); err == nil || out2 != "" {
			for _, m := range rePeak.FindAllStringSubmatch(out2, -1) {
				if len(m) == 2 {
					if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > best {
						best = v
					}
				}
			}
		}
	}
	if math.IsInf(best, -1) {
		return 0, fmt.Errorf("无法从 astats/ebur128 输出解析峰值")
	}
	return best, nil
}

// ConcatAudio 顺序拼接音频。
func ConcatAudio(out string, parts []string, opt Options) error {
	opt = opt.withDefaults()
	if len(parts) == 0 {
		return fmt.Errorf("没有可拼接的音频")
	}
	var inputs []string
	var filters []string
	var refs []string
	for i, p := range parts {
		if !FileExists(p) {
			return fmt.Errorf("音频片段不存在: %s", p)
		}
		inputs = append(inputs, "-i", p)
		filters = append(filters, fmt.Sprintf("[%d:a]aresample=%d,aformat=sample_fmts=fltp:channel_layouts=%s[c%d]",
			i, opt.SampleRate, channelLayout(opt.Channels), i))
		refs = append(refs, fmt.Sprintf("[c%d]", i))
	}
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=0:a=1[out]",
		strings.Join(refs, ""), len(parts)))
	args := append([]string{"-hide_banner", "-y"}, inputs...)
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[out]",
		"-ar", strconv.Itoa(opt.SampleRate), "-ac", strconv.Itoa(opt.Channels), out)
	_, err := ffx.Run(20*time.Minute, "ffmpeg", args...)
	return err
}

// Silence 生成指定时长的静音轨。
func Silence(out string, sec float64, opt Options) error {
	opt = opt.withDefaults()
	if sec <= 0 {
		sec = 0.1
	}
	_, err := ffx.Run(2*time.Minute, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", opt.SampleRate, channelLayout(opt.Channels)),
		"-t", fmt.Sprintf("%.3f", sec), out)
	return err
}

// ProbeDuration 探测媒体时长（统一走 ffx，与其他模块口径一致）。
func ProbeDuration(path string) (float64, error) {
	p, err := ffx.ProbeFile(path)
	if err != nil {
		return 0, err
	}
	return p.DurationSec, nil
}

// EnsureWAVDir 确保输出目录存在。
func EnsureWAVDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// ---------------------------------------------------------------------------
// ffmpeg 版本兼容层
// ---------------------------------------------------------------------------

var (
	adelayOnce sync.Once
	adelayAll  bool
	aloopOnce  sync.Once
	aloopOK    bool
)

// supportsAdelayAll 探测 adelay 是否支持 all=1（ffmpeg 4.2+）。
func supportsAdelayAll() bool {
	adelayOnce.Do(func() {
		out, err := ffx.Run(30*time.Second, "ffmpeg", "-hide_banner", "-h", "filter=adelay")
		if err != nil && out == "" {
			return
		}
		adelayAll = strings.Contains(out, "<boolean>") && strings.Contains(out, "all")
	})
	return adelayAll
}

// adelayFilter 按 ffmpeg 版本生成 adelay 参数。
//
// 返回空字符串表示"不需要延迟"——这一点很关键：老构建（ffmpeg 3.x）对
// adelay=0|0 会直接报错 "At least one delay >0 must be specified"，
// 于是"第一句台词起点为 0"这种最常见的情况反而会让整条混音链失败。
//
// 新版本：adelay=1500:all=1（所有声道生效）
// 老版本：adelay=1500|1500（逐声道），声道数不足时自动补静音，安全
func adelayFilter(ms int) string {
	if ms <= 0 {
		return ""
	}
	if supportsAdelayAll() {
		return fmt.Sprintf("adelay=%d:all=1", ms)
	}
	return fmt.Sprintf("adelay=%d|%d", ms, ms)
}

// withDelay 把 adelay 追加到滤镜链（不需要延迟时原样返回）。
func withDelay(chain string, ms int) string {
	d := adelayFilter(ms)
	if d == "" {
		return chain
	}
	if chain == "" {
		return d
	}
	return chain + "," + d
}

// eofMargin 混音输入的统一超出余量（秒）。
// 老构建（ffmpeg 3.x）的 amix 在滤镜图冲刷 EOF 时必然崩溃（实测任意输入组合都复现
// "Cannot allocate memory"），唯一稳定的用法是让输出端 -t 在 EOF 之前截停：
// 每条输入统一整型到 TotalSec+eofMargin，EOF 永远晚于输出截止点，amix 永远不冲刷。
const eofMargin = 2.0

// finishInput 把单条输入整型成"恰好 TotalSec+eofMargin、统一格式"的 amix 输入：
//
//	chain,aformat=…,apad=whole_len=N,atrim=0:TotalSec+eofMargin
//
// 老构建（ffmpeg 3.x）的 amix 只要有一个输入提前 EOF 就直接崩溃
// （实测报 "Error while filtering: Cannot allocate memory" 且部分构建仍 exit 0，
// 只写出崩溃前的部分内容——历史上表现为"成片只响第一句台词，此后整段无声"），
// 甚至所有输入等长时在正常 EOF 冲刷处也会崩。因此：
//   - apad=whole_len=N：不足 N 样本的补到 N（对无限循环的 aloop 流自动不生效）
//   - atrim：超过 N 的裁到 N
//
// 两者叠加后任何输入（短素材、带延迟、循环、超长）都恰好等长，且 EOF 严格晚于
// 输出端 -t 的截止点——amix 只被消费到 TotalSec 就随 ffmpeg 收尾，永不冲刷。
// 顺序不能反：先 atrim 后 apad 会把"已裁短的流"再补上一截，各输入长度重新不等。
func finishInput(chain string, totalSec float64, opt Options) string {
	end := totalSec + eofMargin
	n := int64(end*float64(opt.SampleRate)) + 1
	return fmt.Sprintf("%s,aformat=sample_fmts=fltp:channel_layouts=%s,apad=whole_len=%d,atrim=0:%.3f",
		chain, channelLayout(opt.Channels), n, end)
}

// hasAloop 探测 aloop 滤镜（BGM 循环用；老构建没有则只播一遍）。
func hasAloop() bool {
	aloopOnce.Do(func() {
		out, err := ffx.Run(30*time.Second, "ffmpeg", "-hide_banner", "-filters")
		aloopOK = err == nil && strings.Contains(out, " aloop ")
	})
	return aloopOK
}

var (
	amixOnce      sync.Once
	amixNormalize bool
)

// supportsAmixNormalize 探测 amix 是否支持 normalize 选项（ffmpeg 4.0+）。
//
// 老版本 amix 会把所有输入按 1/N 衰减，于是"轨越多声音越小"。
// 由于 N 是已知的，可以在混音后补回 N 倍增益——与 normalize=0 等价。
func supportsAmixNormalize() bool {
	amixOnce.Do(func() {
		out, err := ffx.Run(30*time.Second, "ffmpeg", "-hide_banner", "-h", "filter=amix")
		if err != nil && out == "" {
			return
		}
		amixNormalize = strings.Contains(out, "normalize")
	})
	return amixNormalize
}

// AmixFilter 生成跨版本可用的 amix 链（老构建自动以 volume=N 补回 1/N 衰减）。
// 供包外（render.animatic 等自组滤镜图的模块）复用，避免各处重写版本探测。
func AmixFilter(refs []string, outLabel string) string {
	n := len(refs)
	if supportsAmixNormalize() {
		return fmt.Sprintf("%samix=inputs=%d:normalize=0:dropout_transition=0[%s]",
			strings.Join(refs, ""), n, outLabel)
	}
	// 老版本：amix 自动做 1/N 衰减，混音后补回 N 倍
	return fmt.Sprintf("%samix=inputs=%d:dropout_transition=0,volume=%d[%s]",
		strings.Join(refs, ""), n, n, outLabel)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// FileExists 文件存在且非空。
func FileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func dbToVolume(db float64) string {
	if db == 0 {
		return "1.0"
	}
	return fmt.Sprintf("%.4f", math.Pow(10, db/20))
}

func channelLayout(ch int) string {
	if ch >= 2 {
		return "stereo"
	}
	return "mono"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// sidechainParams 把"希望压低多少 dB"映射成 sidechaincompress 的阈值与压缩比。
//
// 为什么需要映射：sidechaincompress 没有"压低 N dB"这样的直接参数，
// 只有 threshold（阈值，0-1 线性）与 ratio（压缩比）。参数选错的后果很隐蔽：
// 阈值高于配乐自身电平时会完全不触发 —— 实测 threshold=0.3 时压低量为 0dB，
// 也就是"配了闪避参数但等于没做"。
//
// 策略：阈值固定在较低值，保证对白一出现就可靠触发；
// 用 ratio 控制压低量（ratio 越大压得越狠）。
// 实测参考（配乐为主输入、人声为触发，threshold=0.02、voice≈0.9）：
//
//	ratio=2  → 约 -3 dB
//	ratio=4  → 约 -6 dB
//	ratio=8  → 约 -12 dB
//
// duckDB 为希望压低的分贝数（负值），典型 -9。返回的 ratio 上限 20（滤镜支持范围）。
func sidechainParams(duckDB float64) (threshold, ratio float64) {
	duck := -math.Abs(duckDB) // 统一成负值
	if duck <= 0 {
		duck = -9
	}
	// 经验拟合：ratio ≈ 2^(duck/3)，即每 3dB 压低量对应一倍压缩比
	r := math.Pow(2, (-duck)/3.0)
	if r < 1 {
		r = 1
	}
	if r > 20 {
		r = 20
	}
	return 0.02, r
}
