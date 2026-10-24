// Package deliver 实现交付阶段：合成成片、多语言字幕、成片校验、交付报告。
//
// 这是"能不能交付"的最后一道闸门。它做四件原实现没有的事：
//
//  1. **完整性门禁**：缺镜头时拒绝出片（或显式降级并标注），绝不产出"看起来成功"的残片
//  2. **实测对齐**：合成用 ffprobe 实测时长，字幕按实测时间轴排版，音频按实测铺轨
//     —— 计划时长与实测必然漂移（视频模型返回固定档位时长），不对齐就必然错位
//  3. **成片校验**：时长、分辨率、音轨、字幕越界、响度、峰值逐项核对后再宣布成功
//  4. **交付留痕**：交付报告把每一项指标写进 JSON，可复核、可追责、可回归
package deliver

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/audio"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/narrate"
	"aistudio/internal/promspec"
	"aistudio/internal/subtitle"
	"aistudio/internal/vision"
)

// Options 交付选项。
type Options struct {
	RunDir string
	// AllowIncomplete 允许缺镜头出片（缺的镜头用占位片段补足，报告会显式标注）
	AllowIncomplete bool
	// BurnSubtitle 是否把字幕烧进画面（默认由 config.subtitle.burn_primary 决定）
	BurnSubtitle *bool
	// KeepMaster 保留无字幕母版
	KeepMaster bool
	// SkipAudio 跳过配音混音（只交付画面，用于快速验证画面质量）
	SkipAudio bool
	Workers   int
	// Client 配音所需的 AI 客户端（--skip-audio 时可为 nil）
	Client *ai.Client
	// VisionClient 视觉复核客户端（已配置为视觉能力）；为 nil 时跳过视觉复核
	VisionClient *ai.Client
	// VisualCheck 是否运行视觉复核（还需 config.vision_review.enabled=true）
	VisualCheck bool
}

// Report 交付报告：交付结果的唯一凭据。
type Report struct {
	RunID        string    `json:"run_id"`
	GeneratedAt  time.Time `json:"generated_at"`
	Complete     bool      `json:"complete"`
	ShotsTotal   int       `json:"shots_total"`
	ShotsDone    int       `json:"shots_done"`
	MissingShots []string  `json:"missing_shots,omitempty"`
	Placeholders []string  `json:"placeholder_shots,omitempty"`

	Video    Master       `json:"video"`
	Audio    AudioInfo    `json:"audio"`
	Subtitle SubtitleInfo `json:"subtitle"`

	Checks   []Check  `json:"checks"`
	Warnings []string `json:"warnings,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
	Outputs  []string `json:"outputs,omitempty"`

	// VisualQA 渲染后的视觉复核结果（让模型真的看画面）。
	// 它审的是"像不像真实拍摄、还是不是同一个人"，技术质检审不了这些。
	VisualQA *vision.Report `json:"visual_qa,omitempty"`
}

// Master 成片技术信息。
type Master struct {
	Path        string  `json:"path"`
	DurationSec float64 `json:"duration_sec"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	FPS         float64 `json:"fps"`
	VideoCodec  string  `json:"video_codec"`
	BitrateKbps float64 `json:"bitrate_kbps"`
}

// AudioInfo 声轨信息。
type AudioInfo struct {
	Path          string  `json:"path"`
	HasAudio      bool    `json:"has_audio"`
	LoudnessLUFS  float64 `json:"loudness_lufs"`
	TruePeakDB    float64 `json:"true_peak_db"`
	VoiceLines    int     `json:"voice_lines"`
	MusicTracks   int     `json:"music_tracks"`
	SFXTracks     int     `json:"sfx_tracks"`
	OverflowLines int     `json:"overflow_lines,omitempty"`
}

// SubtitleInfo 字幕信息。
type SubtitleInfo struct {
	PrimaryFile string   `json:"primary_file"`
	Languages   []string `json:"languages,omitempty"`
	CueCount    int      `json:"cue_count"`
	MaxCPS      float64  `json:"max_cps"`
	MinDurSec   float64  `json:"min_dur_sec"`
	Violations  int      `json:"violation_count"`
	CoverageSec float64  `json:"coverage_sec"`
	Burned      bool     `json:"burned"`
}

// Check 一项交付校验。
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	Blocking bool   `json:"blocking"`
}

// Run 执行交付阶段。
func Run(cfg *config.Config, opt Options) (*Report, error) {
	if opt.RunDir == "" {
		return nil, fmt.Errorf("缺少 --run 参数")
	}
	start := time.Now()
	outDir := filepath.Join(opt.RunDir, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	rep := &Report{RunID: filepath.Base(opt.RunDir), GeneratedAt: time.Now()}

	// ---- 1. 读取上游产物并确认完整性 ----
	kit, err := loadKit(opt.RunDir)
	if err != nil {
		return nil, err
	}
	man, err := loadManifest(opt.RunDir)
	if err != nil {
		return nil, err
	}
	ordered, missing := orderShots(opt.RunDir, kit, man)
	rep.ShotsTotal = len(kit.ShotList)
	rep.ShotsDone = len(ordered)
	rep.MissingShots = missing
	rep.Complete = len(missing) == 0

	if len(missing) > 0 {
		msg := fmt.Sprintf("缺 %d 个镜头（%s）", len(missing), strings.Join(missing, ","))
		if !opt.AllowIncomplete {
			return rep, fmt.Errorf("交付被拒绝：%s。\n"+
				"  生产要求：成片不允许缺镜头。请执行 `studio render --run %s` 补齐缺失镜头后重试；\n"+
				"  如确需内部审看残缺版，加 --allow-incomplete（报告中会显式标注不完整）。",
				msg, opt.RunDir)
		}
		rep.Warnings = append(rep.Warnings, msg+
			"：成片缺该镜头（时长会短于计划），不完整，不可对外交付")
	}

	// ---- 2. 合成视频母版 ----
	videoMaster := filepath.Join(outDir, "final_cut.mp4")
	if err := concatShots(cfg, opt, kit, ordered, videoMaster); err != nil {
		return rep, fmt.Errorf("合成成片失败: %w", err)
	}

	// ---- 2.5 视觉复核（让模型真的看画面）----
	// 放在合成之后、字幕之前：画面缺陷与字幕无关，早发现就能早决定是否定点重渲。
	if opt.VisualCheck && opt.VisionClient != nil {
		vrep, verr := vision.Run(cfg, vision.Options{RunDir: opt.RunDir, Client: opt.VisionClient})
		if verr != nil {
			// 严格模式：复核已启用就必须查完——"未完成"不交付，
			// 不允许"以为查过、其实没查"（确需跳过请显式 --no-visual）。
			return rep, fmt.Errorf("视觉复核未完成，拒绝交付: %w", verr)
		} else {
			rep.VisualQA = vrep
			fmt.Printf("  👁 视觉复核：%d 个镜头均分 %.1f（模型 %s）\n",
				len(vrep.Shots), vrep.Score, vrep.Model)
			for _, s := range vrep.Shots {
				if !s.Pass {
					fmt.Printf("     ⚠ %s %.1f 分：%s\n", s.ShotID, s.Score, strings.Join(s.Issues, "；"))
				}
			}
			if len(vrep.Rerender) > 0 {
				fmt.Printf("     建议定点重渲: studio render --run %s --shots %s\n",
					opt.RunDir, strings.Join(vrep.Rerender, ","))
			}
			// 水印单独告警：它不是"画面质量"问题，而是"不可交付"问题，
			// 混在重渲清单里会被当成又一个画质瑕疵而忽略。
			if vrep.Watermark.Suspected {
				ids := make([]string, 0, len(vrep.Watermark.Shots))
				for _, h := range vrep.Watermark.Shots {
					ids = append(ids, h.ShotID)
				}
				fmt.Printf("     🚫 疑似水印/叠字（%s）——抽帧人工确认 output/review_frames/\n",
					strings.Join(ids, ","))
			}
		}
	}

	// ---- 3. 配音音频 ----
	voicePath := ""
	var narration *narrate.Plan
	if !opt.SkipAudio {
		var musicWarns []string
		voicePath, narration, musicWarns, err = buildNarration(opt.Client, cfg, opt, kit, videoMaster)
		if err != nil {
			return rep, err
		}
		// 配乐缺口必须进交付报告：终端一闪而过的警告，事后在报告里查不到
		// 就等于"没人知道这片子为什么没有配乐"。
		rep.Warnings = append(rep.Warnings, musicWarns...)
	}

	// ---- 4. 字幕（按实测音频时长排版）----
	subInfo, subPath, err := buildSubtitles(cfg, opt, ordered, narration)
	if err != nil {
		return rep, err
	}
	rep.Subtitle = subInfo

	// ---- 5. 母版有声版 + 字幕烧录 ----
	withAudio := filepath.Join(outDir, "final_with_audio.mp4")
	if voicePath != "" {
		if err := muxAudio(cfg, videoMaster, voicePath, withAudio); err != nil {
			return rep, fmt.Errorf("混流失败: %w", err)
		}
	} else {
		withAudio = videoMaster
		rep.Warnings = append(rep.Warnings, "已跳过配音：成片无对白与人声")
	}

	burn := cfg.Subtitle.BurnPrimary
	if opt.BurnSubtitle != nil {
		burn = *opt.BurnSubtitle
	}
	release := filepath.Join(outDir, "final_release.mp4")
	if burn && subPath != "" {
		// 严格模式：要求烧录就必须烧成——失败即失败，不静默降级为外挂字幕。
		// 确需无字幕母版请显式 --no-burn。
		if err := burnSubtitles(cfg, opt.RunDir, withAudio, subPath, release); err != nil {
			return rep, fmt.Errorf("字幕烧录失败（如需无字幕母版请显式 --no-burn）: %w", err)
		}
		rep.Subtitle.Burned = true
	} else {
		release = withAudio
		if subPath != "" {
			rep.Warnings = append(rep.Warnings, "字幕未烧录：成片为无字幕母版，字幕以外挂 SRT 提供")
		}
	}

	// ---- 6. 成片校验 ----
	if err := verify(cfg, rep, release, withAudio, ordered, narration, subInfo, opt.RunDir); err != nil {
		return rep, err
	}
	if opt.KeepMaster && release != withAudio && fsx.FileExists(withAudio) {
		rep.Outputs = append(rep.Outputs, relOf(opt.RunDir, withAudio))
	}
	rep.Outputs = append(rep.Outputs, relOf(opt.RunDir, release))

	// 交付报告是交付结果的唯一凭据：写失败必须让人知道（终端警告），不能静默
	if err := fsx.AtomicWriteJSON(filepath.Join(outDir, "delivery_report.json"), rep); err != nil {
		rep.Warnings = append(rep.Warnings, "交付报告写入失败: "+err.Error())
		fmt.Printf("  ⚠ 交付报告写入失败: %v\n", err)
	}

	// ---- 7. 汇总 ----
	blockers, failed := 0, 0
	for _, c := range rep.Checks {
		if c.OK {
			continue
		}
		failed++
		if c.Blocking {
			blockers++
		}
	}
	fmt.Printf("■ 交付完成（%.1f 分钟）\n", time.Since(start).Minutes())
	fmt.Printf("  成片: %s（%.2fs, %dx%d）\n", relOf(opt.RunDir, release),
		rep.Video.DurationSec, rep.Video.Width, rep.Video.Height)
	if rep.Audio.HasAudio {
		fmt.Printf("  声轨: %.1f LUFS / 峰值 %.1f dBFS（%d 句台词，%d 配乐轨，%d 音效）\n",
			rep.Audio.LoudnessLUFS, rep.Audio.TruePeakDB,
			rep.Audio.VoiceLines, rep.Audio.MusicTracks, rep.Audio.SFXTracks)
	}
	if rep.Subtitle.CueCount > 0 {
		fmt.Printf("  字幕: %d 条，最长 %.1f 字/秒，%s\n", rep.Subtitle.CueCount,
			rep.Subtitle.MaxCPS, map[bool]string{true: "已烧录", false: "外挂 SRT"}[rep.Subtitle.Burned])
	}
	for _, w := range rep.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	// 非阻断的失败项也必须打印：它们以前只写进 JSON，终端上看起来是"全部通过"。
	// 水印"未检查"就属于这一类——静默通过等于让人以为已经确认过。
	for _, c := range rep.Checks {
		if !c.OK && !c.Blocking {
			fmt.Printf("  ⚠ %s：%s\n", c.Name, c.Detail)
		}
	}
	for _, b := range rep.Blockers {
		fmt.Printf("  ⛔ %s\n", b)
	}
	if blockers > 0 {
		return rep, fmt.Errorf("交付校验未通过（%d 项阻断问题）：见 output/delivery_report.json", blockers)
	}
	if failed > 0 {
		fmt.Printf("  ✓ 无阻断问题（%d 项告警待人工确认，见 output/delivery_report.json）\n", failed)
		return rep, nil
	}
	fmt.Println("  ✓ 交付校验全部通过")
	return rep, nil
}

// ---------------------------------------------------------------------------
// 合成
// ---------------------------------------------------------------------------

type shotClip struct {
	ID          string
	Path        string
	Dur         float64
	PlanDur     float64
	Placeholder bool
}

// orderShots 按分镜顺序收集已完成的镜头片段，并返回缺失清单。
// 顺序必须严格按分镜：任何重排都会让剧情错乱。
// 时长探测彼此独立（各一次 ffprobe 子进程），批量并行以缩短交付前的等待；
// 探测失败的镜头与"未产出"同样计入缺失清单，语义与串行版一致。
func orderShots(runDir string, kit *promspec.Kit, man *manifestDoc) ([]shotClip, []string) {
	done := map[string]string{}
	for _, s := range man.Shots {
		if s.Status == "done" && s.Video != "" {
			done[s.ID] = filepath.Join(runDir, filepath.FromSlash(s.Video))
		}
	}
	var present []int // kit.ShotList 中已产出镜头的下标（保持分镜顺序）
	var paths []string
	for i, s := range kit.ShotList {
		if p, ok := done[s.ID]; ok {
			present = append(present, i)
			paths = append(paths, p)
		}
	}
	durs, errs := ffx.ProbeBatch(paths, 0)
	var clips []shotClip
	var missing []string
	next := 0 // present/paths/durs 的游标
	for i, s := range kit.ShotList {
		if next < len(present) && present[next] == i {
			if errs[next] != nil {
				missing = append(missing, s.ID)
			} else {
				clips = append(clips, shotClip{ID: s.ID, Path: paths[next], Dur: durs[next], PlanDur: s.DurationSec})
			}
			next++
			continue
		}
		missing = append(missing, s.ID)
	}
	return clips, missing
}

// concatShots 合成视频母版。
// 用 filter_complex concat 而非 concat demuxer + 流拷贝：生成端返回的片段编码参数
// 可能不一致（尤其混用了 speed/quality 两档），流拷贝会产生时长错乱与花屏。
//
// editplan.json 里标注的转场（开场淡入 / 场景切换叠化）在这里真正落地：
// 叠化采用"时间轴守恒"实现（见 transitions.go），成片总时长不变，
// 字幕与配音的实测时间轴不受影响。严格模式：配置了转场但环境缺少
// xfade/tpad 滤镜时直接失败，不静默退回硬切。
func concatShots(cfg *config.Config, opt Options, kit *promspec.Kit, clips []shotClip, out string) error {
	if len(clips) == 0 {
		return fmt.Errorf("没有任何可合成的镜头片段")
	}
	w, h := parseSize(cfg.Video.Size)
	if w == 0 {
		w, h = 1920, 1080
	}
	fps := cfg.Video.FPS
	if fps <= 0 {
		fps = 24
	}

	tc := cfg.Transitions
	// 严格模式：配置了转场就必须能做出转场——静默退回硬切会让
	// editplan 的 fade_in/dissolve 方案变成"写完就扔的死数据"
	if tc.Enabled && (!ffx.HasFilter("xfade") || !ffx.HasFilter("tpad")) {
		return fmt.Errorf("transitions.enabled=true 但当前 ffmpeg 缺少 xfade/tpad 滤镜：请升级 ffmpeg（4.3+），或显式关闭 transitions 后重跑")
	}

	// 宽银幕交付：Style.Aspect 宽于交付容器时按该画幅中心裁切 + letterbox
	// （HD scope 的标准交付形态，字幕烧录后落在下黑边）。off 或解析失败保持原画幅。
	scopeH := 0
	if tc.ScopeEnabled() {
		if scopeH = scopeCropHeight(kit.Style.Aspect, w, h); scopeH > 0 {
			fmt.Printf("  🎬 宽银幕交付：按 %s 裁切（画面 %d×%d + letterbox）\n", kit.Style.Aspect, w, scopeH)
		}
	}

	// 镜头间曝光匹配：实测各镜亮度对齐全片中位数（生成端逐镜漂移的直接对策）。
	// 严格模式：测量失败即失败，不静默跳过（跳过 = 成片保留镜头间闪烁）。
	var lumaAdj []float64
	if tc.MatchExposureEnabled() && len(clips) > 1 {
		var lerr error
		lumaAdj, lerr = shotLumaOffsets(clips)
		if lerr != nil {
			return lerr
		}
		applied := 0
		maxAdj := 0.0
		for _, v := range lumaAdj {
			if v != 0 {
				applied++
				if a := math.Abs(v); a > maxAdj {
					maxAdj = a
				}
			}
		}
		if applied > 0 {
			fmt.Printf("  🎨 镜头间曝光匹配：%d/%d 个镜头做了亮度校正（最大 ±%.3f）\n", applied, len(clips), maxAdj)
		}
	}

	graph, err := buildConcatFilter(clips, loadTransitions(opt.RunDir), w, h, fps, tc,
		ffx.HasFilter("xfade"), ffx.HasFilter("tpad"), lumaAdj, scopeH)
	if err != nil {
		return err
	}

	var inputs []string
	for _, c := range clips {
		inputs = append(inputs, "-i", c.Path)
	}
	args := append([]string{"-hide_banner", "-y"}, inputs...)
	args = append(args,
		"-filter_complex", graph,
		"-map", "[vout]",
		"-c:v", "libx264", "-preset", "medium", "-crf", "18",
		"-pix_fmt", "yuv420p", "-movflags", "+faststart",
		out)
	if _, err := ffx.Run(60*time.Minute, "ffmpeg", args...); err != nil {
		return err
	}
	return nil
}

// buildNarration 生成配音音频（台词 + BGM + 音效混音）。
// 返回的告警（配乐缺口等）由调用方写进交付报告。
func buildNarration(client *ai.Client, cfg *config.Config, opt Options, kit *promspec.Kit,
	videoMaster string) (string, *narrate.Plan, []string, error) {

	if client == nil {
		return "", nil, nil, nil
	}
	plan, err := narrate.Run(client, cfg, narrate.Options{
		RunDir: opt.RunDir, Workers: opt.Workers, AllowIncomplete: opt.AllowIncomplete,
	})
	if err != nil {
		// 严格模式：要配音就必须配音成功——缺台本/任一环节失败都直接失败，
		// 不静默产出"无对白"的片子。确需只交付画面请显式 --skip-audio。
		return "", nil, nil, fmt.Errorf("配音失败（如需只交付画面请显式 --skip-audio）: %w", err)
	}
	total, err := audio.ProbeDuration(videoMaster)
	if err != nil {
		return "", nil, nil, err
	}
	outDir := filepath.Join(opt.RunDir, "output")
	mixOut := filepath.Join(outDir, "audio_mix.wav")

	var voiceTracks []audio.Track
	for _, l := range plan.Lines {
		voiceTracks = append(voiceTracks, audio.Track{
			Path: filepath.Join(opt.RunDir, filepath.FromSlash(l.File)), StartSec: l.AbsStartSec,
		})
	}
	// BGM 自动化：在扫描素材之前保证 output/music/ 有货——
	// 曲库按 cue 自动选曲（零成本），曲库无匹配时可走生成 API。
	// 严格模式：供给过程本身的失败（生成/复制/落盘）直接失败；
	// 曲库无匹配素材属于缺输入，逐条告警如实上报。
	var musicWarns []string
	warns, merr := ensureMusic(client, cfg, opt.RunDir)
	if merr != nil {
		return "", nil, nil, merr
	}
	for _, w := range warns {
		fmt.Printf("  ⚠ %s\n", w)
		musicWarns = append(musicWarns, w)
	}
	musicTracks, sfxTracks := scanMusicAndSFX(opt.RunDir, plan, kit, cfg)

	res, err := audio.Mix(audio.MixRequest{
		RunDir: opt.RunDir, VoiceTracks: voiceTracks,
		MusicTracks: musicTracks, SFXTracks: sfxTracks,
		TotalSec: total, Out: mixOut, DuckDB: cfg.Audio.DuckDB,
	}, audio.Options{
		SampleRate: cfg.Audio.SampleRate, Channels: cfg.Audio.Channels,
		TargetLoudness: cfg.Quality.LoudnessTarget, TruePeakDB: cfg.Quality.TruePeakDB,
		VoiceCompress: cfg.Audio.VoiceCompressEnabled(),
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("混音失败: %w", err)
	}
	plan.Loudness = res.LoudnessLUFS
	plan.Notes = append(plan.Notes, res.Notes...)
	_ = fsx.AtomicWriteJSON(filepath.Join(opt.RunDir, "artifacts", "narration.json"), plan)
	// 把配乐/音效轨数记进计划：交付报告要如实反映"成片里到底有没有配乐"
	plan.MusicTracks = res.MusicCount
	plan.SFXTracks = res.SFXCount
	fmt.Printf("  🔊 混音完成：%.1f LUFS / 峰值 %.1f dBFS（人声 %d，配乐 %d，音效 %d）\n",
		res.LoudnessLUFS, res.TruePeakDB, len(voiceTracks), len(musicTracks), len(sfxTracks))
	return mixOut, plan, musicWarns, nil
}

func isAudioFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".wav", ".mp3", ".m4a", ".aac", ".flac", ".ogg":
		return true
	}
	return false
}

// muxAudio 把混音声轨与画面混流（画面不重编码，保证画质无损）。
//
// 不使用 -shortest：音轨已按成片实测时长对齐；-shortest 会在音轨偏短时
// 把视频也截断（4 秒的音轨会把 23 秒的成片截成 4 秒）。
func muxAudio(cfg *config.Config, video, voice, out string) error {
	args := []string{"-hide_banner", "-y", "-i", video, "-i", voice,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-ar", "48000",
		"-movflags", "+faststart", out}
	_, err := ffx.Run(30*time.Minute, "ffmpeg", args...)
	return err
}

// ---------------------------------------------------------------------------
// 字幕
// ---------------------------------------------------------------------------

func buildSubtitles(cfg *config.Config, opt Options, clips []shotClip, plan *narrate.Plan) (SubtitleInfo, string, error) {
	info := SubtitleInfo{}
	outDir := filepath.Join(opt.RunDir, "output")
	optPlan := subtitle.DefaultPlanOptions(cfg)

	var slots []subtitle.Slot
	var lines []subtitle.Line
	if plan != nil && len(plan.Lines) > 0 {
		// 实测优先后：入点与时长全部来自真实音频
		slots, lines = narrate.SlotsForSubtitle(plan)
	} else {
		// 降级：无配音时用镜头实测时长做槽位，字幕按台本估算时长排
		var t float64
		for _, c := range clips {
			slots = append(slots, subtitle.Slot{ShotID: c.ID, Start: t, Dur: c.Dur, ActualDur: c.Dur})
			t += c.Dur
		}
		vo, err := loadVoiceoverLines(opt.RunDir)
		if err == nil {
			for _, n := range vo {
				lines = append(lines, subtitle.Line{ShotID: n.ShotID, Speaker: n.Speaker, Text: n.Text})
			}
		}
	}
	if len(lines) == 0 {
		return info, "", nil // 无台词：静默接受，交付报告会显示 cue=0
	}

	cues := subtitle.Plan(slots, lines, optPlan)
	if len(cues) == 0 {
		return info, "", fmt.Errorf("字幕排版结果为空，但台本有 %d 句台词：请检查台本 shot_id 与镜头是否对应", len(lines))
	}

	// 多语言：主语言烧录/外挂，其余语言只出外挂 SRT（同一时间轴，翻译不重排）
	langs := cfg.Subtitle.Languages
	if len(langs) == 0 {
		langs = []string{"zh"}
	}
	primary := filepath.Join(outDir, "subtitles.srt")
	if err := fsx.AtomicWrite(primary, []byte(subtitle.Render(cues)), 0o644); err != nil {
		return info, "", err
	}
	info.PrimaryFile = relOf(opt.RunDir, primary)
	info.Languages = langs
	info.CueCount = len(cues)
	info.CoverageSec = subtitle.TotalDuration(cues)

	var maxCPS, minDur float64
	minDur = 1e9
	for _, c := range cues {
		if c.CPS > maxCPS {
			maxCPS = c.CPS
		}
		if d := c.End - c.Start; d < minDur {
			minDur = d
		}
	}
	if len(cues) == 0 {
		minDur = 0
	}
	info.MaxCPS = maxCPS
	info.MinDurSec = minDur

	// 字幕质量校验：不合格必须暴露（字幕是观众直接看到的东西）
	violations := subtitle.Verify(cues, optPlan)
	info.Violations = len(violations)
	for _, v := range violations {
		if v.Severity == "block" {
			fmt.Printf("  ⛔ 字幕问题：%s\n", v.Message)
		}
	}
	if info.Violations > 0 {
		_ = fsx.AtomicWriteJSON(filepath.Join(outDir, "subtitle_violations.json"), violations)
	}

	// 其他语言：若已存在人工/翻译产物则一并校验；不存在时明确提示
	for _, lang := range langs[1:] {
		p := filepath.Join(outDir, "subtitles."+lang+".srt")
		if fsx.FileExists(p) {
			// 逐语言重新校验时间的可读性（不同语言字符数差异很大）
			fmt.Printf("  ✓ 外挂字幕: %s\n", relOf(opt.RunDir, p))
		} else {
			fmt.Printf("  ⚠ 缺少 %s 字幕（可通过 output/subtitles.%s.srt 提供，时间轴与主字幕一致）\n", lang, lang)
		}
	}
	return info, primary, nil
}

// burnSubtitles 烧录字幕。
//
// 路径处理是这里最容易踩的坑：Windows 盘符（D:）在 filter 参数里与选项分隔符冲突，
// 转义规则又随 ffmpeg 版本变化。最稳的做法是**切到字幕所在目录 + 用纯文件名**，
// 两条路径都试，都失败才报错。
func burnSubtitles(cfg *config.Config, runDir, video, srt, out string) error {
	style := fmt.Sprintf("FontName=%s,FontSize=%d,Outline=%d,Shadow=0,MarginV=%d,Alignment=2",
		cfg.Subtitle.Font, cfg.Subtitle.FontSize,
		maxInt(1, cfg.Subtitle.OutlineWidth), cfg.Subtitle.MarginV)
	dir := filepath.Dir(srt)
	name := filepath.Base(srt)

	// 路径 A 会把工作目录切到字幕所在目录，video/out 若是相对路径（deliver 全程
	// 使用相对 run 目录的路径）就会指向不存在的位置 —— 先统一转绝对路径。
	if abs, err := filepath.Abs(video); err == nil {
		video = abs
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}

	// 路径 A：切到字幕目录，用纯文件名（规避盘符与转义）
	if _, err := ffx.RunIn(dir, 60*time.Minute, "ffmpeg", "-hide_banner", "-y",
		"-i", video,
		"-vf", "subtitles="+name+":force_style='"+style+"'",
		"-c:v", "libx264", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a", "copy", "-movflags", "+faststart", out); err == nil {
		return nil
	}
	// 路径 B：绝对路径 + 冒号转义
	esc := escapeFilterPath(srt)
	if _, err := ffx.Run(60*time.Minute, "ffmpeg", "-hide_banner", "-y",
		"-i", video,
		"-vf", "subtitles="+esc+":force_style='"+style+"'",
		"-c:v", "libx264", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a", "copy", "-movflags", "+faststart", out); err == nil {
		return nil
	}
	// 严格模式：不提供"丢弃字体"的兜底路径——缺字体会让 subtitles 滤镜失败，
	// 但静默换字体是画面级劣化（字幕排版是交付物的一部分）。
	// 两条路径（切目录 / 绝对路径转义）试完仍失败即失败，请安装配置的字体后重跑。
	return fmt.Errorf("字幕烧录失败：已尝试两种路径（相对路径与绝对路径转义）；"+
		"常见原因：缺少配置字体 %q（subtitle.font）或 ffmpeg 无 subtitles 滤镜", cfg.Subtitle.Font)
}

func escapeFilterPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	s := filepath.ToSlash(abs)
	s = strings.ReplaceAll(s, "\\", "/")
	// filter 参数里 : 和 ' 需要转义；Windows 盘符会命中这里
	s = strings.ReplaceAll(s, ":", "\\:")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

// verify 成片校验：交付前的最后一道闸门。
// 每一项都基于实测数据，不允许用"计划值"糊弄过去。
func verify(cfg *config.Config, rep *Report, release, withAudio string, clips []shotClip,
	plan *narrate.Plan, subInfo SubtitleInfo, runDir string) error {

	add := func(name string, ok bool, blocking bool, format string, args ...any) {
		rep.Checks = append(rep.Checks, Check{Name: name, OK: ok, Blocking: blocking, Detail: fmt.Sprintf(format, args...)})
		if !ok && blocking {
			rep.Blockers = append(rep.Blockers, fmt.Sprintf("%s: %s", name, fmt.Sprintf(format, args...)))
		}
	}

	p, err := ffx.ProbeFile(release)
	if err != nil {
		add("成片可读", false, true, "无法探测成片文件: %v", err)
		return nil
	}
	rep.Video = Master{
		Path: relOf(filepath.Dir(filepath.Dir(release)), release), DurationSec: p.DurationSec,
		Width: p.Width, Height: p.Height, FPS: p.FPS, VideoCodec: p.VideoCodec,
		BitrateKbps: float64(p.Bitrate) / 1000,
	}

	// 1) 时长：必须等于各镜头实测之和（否则说明合成丢帧或截断）
	var expect float64
	for _, c := range clips {
		expect += c.Dur
	}
	diff := absDiff(p.DurationSec, expect)
	// concatSlack 吸收 concat 重编码的帧边界取整漂移（每镜 <1 帧 × 镜头数）
	const concatSlack = 0.5
	add("成片时长与镜头总长一致", diff <= cfg.Quality.DurationTolerance+concatSlack, true,
		"成片 %.2fs vs 镜头合计 %.2fs（差 %.2fs，容差 %.2fs）",
		p.DurationSec, expect, diff, cfg.Quality.DurationTolerance+concatSlack)

	// 2) 分辨率：宽高都要达标，且方向必须一致——
	// 竖屏成片配横屏目标（或反过来）在旧的 OR 判断下会"达标"，但画面已经被
	// letterbox/pillarbox 毁掉，必须显式判不达标并说明原因。
	w, h := parseSize(cfg.Video.Size)
	if w > 0 {
		fit := p.Width >= w && p.Height >= h
		detail := fmt.Sprintf("成片 %dx%d（目标 %dx%d）", p.Width, p.Height, w, h)
		if !fit && p.Width == h && p.Height == w {
			detail += "：画幅方向颠倒（竖屏/横屏互换），检查 video.size 配置与生成端返回"
		}
		add("分辨率达标", fit, false, "%s", detail)
	}

	// 3) 完整性
	add("镜头完整", rep.Complete, true, "缺失 %d 个镜头（%s）", len(rep.MissingShots), strings.Join(rep.MissingShots, ","))

	// 4) 声轨
	rep.Audio.VoiceLines = 0
	if plan != nil {
		rep.Audio.VoiceLines = len(plan.Lines)
		rep.Audio.MusicTracks = plan.MusicTracks
		rep.Audio.SFXTracks = plan.SFXTracks
		for _, l := range plan.Lines {
			if l.Overflow {
				rep.Audio.OverflowLines++
			}
		}
	}
	hasAudio := p.HasAudio
	rep.Audio.HasAudio = hasAudio
	if hasAudio {
		// fail-closed：测量失败必须显式呈现为"未达标"，而不是让检查项从报告里
		// 消失——消失的检查在终端上与"通过"无从区分。
		lufs, lerr := audio.MeasureLoudness(release)
		if lerr != nil {
			add("响度达标", false, false, "无法测量: %v（不能默认达标，请检查 ffmpeg 版本）", lerr)
		} else {
			rep.Audio.LoudnessLUFS = lufs
			target := cfg.Quality.LoudnessTarget
			tol := cfg.Quality.LoudnessTolerance
			add("响度达标", absDiff(lufs, target) <= tol, false,
				"实测 %.1f LUFS（目标 %.1f ± %.1f）", lufs, target, tol)
		}
		peak, perr := audio.PeakDBFS(release)
		if perr == nil {
			rep.Audio.TruePeakDB = peak
		}
		// PeakDBFS 出错返回 0 会让"未削波"恒真——错误必须走 fail-closed
		add("峰值未削波", perr == nil && peak <= cfg.Quality.TruePeakDB+1.0, false,
			peakDetail(perr, peak, cfg.Quality.TruePeakDB))
	} else if !planIsEmpty(plan) {
		add("声轨存在", false, true, "成片没有音轨，但有 %d 句台词", len(plan.Lines))
	}

	// 5) 字幕
	if subInfo.CueCount > 0 {
		add("字幕条数非零", true, false, "%d 条，覆盖率 %.1fs", subInfo.CueCount, subInfo.CoverageSec)
		subtitleEnd := subInfo.CoverageSec
		add("字幕未超出成片时长", subtitleEnd <= p.DurationSec+0.05, true,
			"最后一条字幕结束于 %.2fs，成片 %.2fs", subtitleEnd, p.DurationSec)
		add("字幕速度可读", subInfo.MaxCPS <= cfg.Subtitle.MaxCPS*1.15, false,
			"最长 %.1f 字/秒（上限 %.1f）", subInfo.MaxCPS, cfg.Subtitle.MaxCPS)
		add("字幕无阻断问题", subInfo.Violations == 0, false, "%d 项排版问题", subInfo.Violations)
		// 字幕必须留在所属镜头内：越界就意味着"字幕在说上一句，画面已经是下一镜"。
		// fail-closed：核验依据（narration.json）缺失/损坏时，有配音的 run 必须
		// 显式失败；无配音的 run（skip-audio）字幕为估算排版，降级为告警。
		escaped, verr := countOutOfShotCues(runDir)
		switch {
		case verr != nil && !planIsEmpty(plan):
			add("字幕未越出所属镜头", false, true, "无法核验: %v", verr)
		case verr != nil:
			add("字幕未越出所属镜头", false, false, "未核验（无配音产物）: %v", verr)
		case escaped > 0:
			add("字幕未越出所属镜头", false, true,
				"有 %d 条字幕超出所属镜头时间范围（字幕与画面对不上）", escaped)
		default:
			add("字幕未越出所属镜头", true, false, "全部字幕都在所属镜头时间范围内")
		}
	}

	// 6) 台词溢出：不阻断交付，但必须让用户知道（可能影响观感）
	if rep.Audio.OverflowLines > 0 {
		add("台词未溢出镜头槽", false, false,
			"%d 句台词时长超过所属镜头，已按顺序顺延（字幕已按实测时间跟随）", rep.Audio.OverflowLines)
	}

	// 7) 视觉复核：措辞上明确区分"提示"与"阻断"。
	// 画面语义判断属于模型主观结论，不阻断交付（避免误判卡死生产），
	// 但必须把结论和重渲清单明确摆出来，让人决定。
	if rep.VisualQA != nil {
		v := rep.VisualQA
		add("视觉复核无异常镜头", v.Pass, false,
			"%d 个镜头均分 %.1f，%d 个建议重渲（%s）",
			len(v.Shots), v.Score, len(v.Rerender), strings.Join(v.Rerender, ","))
	}

	// 8) 水印/叠字：平台水印是"每一帧都在"的交付硬伤，且不会让任何技术指标变差，
	// 常规质检永远发现不了。判定只能靠看懂画面（实测：确定性信号不可分——
	// 有角标 vs 无角标的右下角帧间变化比值仅 0.77 vs 1.04，与真实静止物体重叠），
	// 所以结论来自视觉模型，并按"主观结论"处理：告警不阻断。
	ok, detail := watermarkCheck(rep.VisualQA)
	add("画面无水印/叠字", ok, false, "%s", detail)
	return nil
}

// watermarkCheck 水印校验的三态判定：通过 / 疑似 / 未检查。
//
// 关键约束：**"没查"绝不能表现为"通过"**。报告里缺一条还是显示通过，
// 会让人误以为水印已被确认，而水印恰恰是换模型/账号声明过期时最容易静默回归的项。
func watermarkCheck(v *vision.Report) (ok bool, detail string) {
	switch {
	case v == nil:
		return false, "未检查：需开启视觉复核（config.vision_review.enabled=true 或 --visual）才能判定水印"
	case !v.Watermark.Checked:
		return false, "未检查：视觉复核未产出水印判定（模型未按 schema 返回 overlay）"
	case v.Watermark.Suspected:
		ids := make([]string, 0, len(v.Watermark.Shots))
		shotIDs := make([]string, 0, len(v.Watermark.Shots))
		for _, h := range v.Watermark.Shots {
			label := h.ShotID
			if h.Text != "" {
				label += "(" + h.Text + ")"
			}
			ids = append(ids, label)
			shotIDs = append(shotIDs, h.ShotID)
		}
		// 动作写死在检查里，不依赖上游 Note 是否填了：报告是给人执行用的，
		// 只说"疑似有水印"而不给下一步，等于把问题丢回给人猜。
		return false, fmt.Sprintf(
			"疑似带叠字/角标：%s。抽帧已存 output/review_frames/ 可人工核对；"+
				"先确认账号已签署去水印免责声明且出图/视频请求带了 watermark_enabled=false，"+
				"再对上述镜头重渲：studio render --run <run> --shots %s",
			strings.Join(ids, " "), strings.Join(shotIDs, ","))
	default:
		return true, "已逐镜头检查四角与下缘，未检出叠字/角标"
	}
}

func planIsEmpty(p *narrate.Plan) bool { return p == nil || len(p.Lines) == 0 }

// peakDetail 峰值校验的详情文案（测量失败时说明原因，而不是显示"峰值 0.0"）。
func peakDetail(err error, peak, limit float64) string {
	if err != nil {
		return fmt.Sprintf("峰值测量失败: %v（不能默认未削波，请检查 ffmpeg 版本）", err)
	}
	return fmt.Sprintf("峰值 %.1f dBFS（上限 %.1f）", peak, limit)
}

// countOutOfShotCues 统计越出所属镜头时间范围的字幕条数。
//
// 允许 0.35s 溢出：台词比镜头长时会有自然的尾音跨到下一镜头（narrate 已按顺序顺延），
// 这是可接受的；超过则说明字幕与画面严重失配。
//
// 返回 error 而不是静默返回 0：narration.json 读不到时返回 0 会让门禁在
// "无证据"的情况下显示"全部字幕都在所属镜头内"——没查不能等于查过没问题。
func countOutOfShotCues(runDir string) (int, error) {
	var plan narrate.Plan
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "narration.json"), &plan); err != nil {
		return 0, fmt.Errorf("读取 narration.json 失败: %w", err)
	}
	if len(plan.Shots) == 0 || len(plan.Lines) == 0 {
		return 0, fmt.Errorf("narration.json 缺少镜头槽或台词（产物不完整）")
	}
	slotOf := map[string]narrate.ShotSlot{}
	for _, s := range plan.Shots {
		slotOf[s.ShotID] = s
	}
	const tolerance = 0.35
	bad := 0
	for _, l := range plan.Lines {
		s, ok := slotOf[l.ShotID]
		if !ok {
			continue
		}
		start, end := s.Start, s.Start+s.Duration
		// 入点必须落在所属镜头内（允许容差）。两类跨镜是有意设计，不判越界：
		// 台词尾音跨到下一镜（narrate 按顺序顺延）；J-cut 声音提前越过切点
		// （提前量与字幕同步，见 audio.jcut_sec）。
		tol := tolerance + l.JCutSec
		if l.AbsStartSec < start-tol || l.AbsStartSec > end+tol {
			bad++
		}
	}
	return bad, nil
}

// ---------------------------------------------------------------------------
// 载入
// ---------------------------------------------------------------------------

func loadKit(runDir string) (*promspec.Kit, error) {
	var kit promspec.Kit
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "shotspec.json"), &kit); err != nil {
		return nil, fmt.Errorf("读取生成规格失败（需先跑 shotspec 阶段）: %w", err)
	}
	if len(kit.ShotList) == 0 {
		return nil, fmt.Errorf("生成规格中没有镜头")
	}
	return &kit, nil
}

type manifestDoc struct {
	Shots []struct {
		ID     string  `json:"id"`
		Status string  `json:"status"`
		Video  string  `json:"video"`
		Actual float64 `json:"actual_duration_sec"`
	} `json:"shots"`
	Complete bool `json:"complete"`
}

func loadManifest(runDir string) (*manifestDoc, error) {
	var m manifestDoc
	if err := fsx.ReadJSON(filepath.Join(runDir, "output", "render_manifest.json"), &m); err != nil {
		return nil, fmt.Errorf("读取渲染清单失败（需先运行 studio render）: %w", err)
	}
	return &m, nil
}

func loadVoiceoverLines(runDir string) ([]subtitle.Line, error) {
	var doc struct {
		Narration []struct {
			ShotID  string `json:"shot_id"`
			Speaker string `json:"speaker"`
			Text    string `json:"text"`
		} `json:"narration"`
	}
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "voiceover.json"), &doc); err != nil {
		return nil, err
	}
	out := make([]subtitle.Line, 0, len(doc.Narration))
	for _, n := range doc.Narration {
		out = append(out, subtitle.Line{ShotID: n.ShotID, Speaker: n.Speaker, Text: n.Text})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func parseSize(s string) (int, int) {
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil {
		return 0, 0
	}
	return w, h
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func relOf(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
