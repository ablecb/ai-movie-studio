// Package narrate 实现生产级配音：音色映射、逐句实测时长、超槽处理、精确铺轨。
//
// 与"整段合成再对齐"的区别，也是台词对不上的根因：
//
//  1. 语音自然度三件套：新一代 TTS 模型（glm-tts）+ 发送前文本规范化
//     （标点驱动韵律，见 ai.normalizeTTSText）+ 服务端语速；
//     本地 atempo 拉伸只做超槽兜底
//  2. 音色来自角色设定（characters.json 的 voice 字段 → 音色表），
//     而不是靠关键词猜音调——同一个音色配所有角色，观众分不清谁在说话；
//     仅当角色与旁白共用同一音色时，才退而用变调区分
//  3. 每句台词单独合成并 **实测** 时长（含首尾静音裁剪），字幕与铺轨都以实测值为准，
//     而不是用"中文字数/4"的估算（估算偏差累积起来就是明显的音画不同步）
//  4. 超槽处理分三级：加大 TTS 语速（配置）→ 本地时间压缩（上限内）→ 记录溢出并延后，
//     绝不做"花栗鼠式"的无限加速
//  5. 相邻台词强制不重叠（排麦），杜绝两个人声叠在一起
package narrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/audio"
	"aistudio/internal/conc"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/mouth"
	"aistudio/internal/promspec"
	"aistudio/internal/shotkit"
	"aistudio/internal/subtitle"
)

// Options 配音选项。
type Options struct {
	RunDir   string
	Voice    string            // 默认音色（未在角色表/映射中指定时使用）
	VoiceMap map[string]string // 角色名 → 音色（覆盖角色设定）
	Workers  int
	// Force 强制重新合成已存在的台词音频（默认复用，避免重复付费）
	Force bool
	// AllowIncomplete 残缺交付（--allow-incomplete）：台本中所属镜头未渲染的
	// 台词跳过不合成；正常交付仍视为台本与分镜不一致而失败
	AllowIncomplete bool
}

// Line 一句台词的生成结果。
type Line struct {
	ShotID      string  `json:"shot_id"`
	Speaker     string  `json:"speaker"`
	Text        string  `json:"text"`
	Voice       string  `json:"voice"`
	Emotion     string  `json:"emotion,omitempty"` // 台本情绪标注（仅当 TTS 端点支持 emotion 参数时填充；不支持时为空）
	RawFile     string  `json:"raw_file"`          // 原始 TTS 音频（相对 run 目录）
	File        string  `json:"file"`              // 处理后的音频（相对 run 目录）
	MeasuredSec float64 `json:"measured_sec"`      // 处理后实测时长
	RawSec      float64 `json:"raw_sec"`
	StartSec    float64 `json:"start_sec"`     // 相对镜头起点的入点（J-cut 时为负）
	AbsStartSec float64 `json:"abs_start_sec"` // 相对成片的入点
	// JCutSec 该句提前于所属镜头画面的秒数（J-cut 声桥：声音先于画面入，
	// 带着观众进下一镜；口型声桥在台词长于张嘴区间时同样产生提前）。
	// 0 = 未做 J-cut。字幕按同一提前量排版，音画字仍然同步。
	JCutSec float64 `json:"jcut_sec,omitempty"`
	// MouthWin 口型说话窗口（相对镜头起点；nil=未做口型对齐）。
	// 写进交付报告与字幕排版依据，是"口型对齐"质检项的数据源。
	MouthWin  *mouth.Window `json:"mouth_win,omitempty"`
	Pitch     float64       `json:"pitch"`
	Speed     float64       `json:"speed"`
	SlotSec   float64       `json:"slot_sec"`
	Overflow  bool          `json:"overflow"`  // 仍未装进镜头槽（已顺延到下一镜头）
	Truncated bool          `json:"truncated"` // 因超长被压缩到上限
	Reused    bool          `json:"reused"`    // 断点续跑复用
}

// Plan 配音计划（交付阶段的唯一依据）。
type Plan struct {
	RunID       string            `json:"run_id"`
	GeneratedAt time.Time         `json:"generated_at"`
	TotalSec    float64           `json:"total_sec"` // 成片实测总时长
	Shots       []ShotSlot        `json:"shots"`
	Lines       []Line            `json:"lines"`
	Loudness    float64           `json:"loudness_lufs,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
	VoiceMap    map[string]string `json:"voice_map,omitempty"`
	// MusicTracks/SFXTracks 由混音阶段回填：
	// 交付报告据此如实说明"成片里到底有没有配乐"，而不是只报人声句数。
	MusicTracks int `json:"music_tracks,omitempty"`
	SFXTracks   int `json:"sfx_tracks,omitempty"`
}

// ShotSlot 镜头槽位（全部来自 ffprobe 实测）。
type ShotSlot struct {
	ShotID     string   `json:"shot_id"`
	Start      float64  `json:"start"`
	Duration   float64  `json:"duration"`
	PlanSec    float64  `json:"plan_duration_sec"`
	Characters []string `json:"characters,omitempty"`
	// SpeakWin 口型说话窗口（相对镜头起点；nil=未检测到/未开启对齐）。
	// 台词入点对到 WinStart、时长适配到窗长——段落级口型同步。
	SpeakWin *mouth.Window `json:"speak_win,omitempty"`
}

// Run 执行配音流水线：生成台词音频并产出配音计划。
func Run(client *ai.Client, cfg *config.Config, opt Options) (*Plan, error) {
	start := time.Now()
	if opt.RunDir == "" {
		return nil, fmt.Errorf("缺少 --run 参数")
	}
	if opt.Workers <= 0 {
		opt.Workers = 2
	}

	kit, err := loadKit(opt.RunDir)
	if err != nil {
		return nil, err
	}
	vo, err := loadVoiceover(opt.RunDir)
	if err != nil {
		return nil, err
	}
	// 台本与分镜的一致性门禁：非旁白台词的说话人必须是所属镜头的在场角色。
	// voiceover 由 AI 产出，曾把杨戬的台词挂到猪八戒的镜头上——成片变成
	// "这张嘴配那个人的声音"，比缺台词更穿帮。数据不一致当场报错（fail-closed）。
	if err := validateVoiceoverSpeakers(kit, vo); err != nil {
		return nil, err
	}
	slots, total, err := measureShots(opt.RunDir, kit)
	if err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("找不到任何镜头片段，请先运行 studio render")
	}
	slotOf := map[string]ShotSlot{}
	for _, s := range slots {
		slotOf[s.ShotID] = s
	}

	// 口型对齐（audio.mouth_align）：为有台词的镜头检测说话窗口。
	// 并行检测、缓存复用；失败回落 nil 窗口（走普通入点），绝不阻断配音。
	if cfg.Audio.MouthAlign {
		alignSpeakWindows(client, cfg, opt.RunDir, vo, slots)
		for i := range slots {
			slotOf[slots[i].ShotID] = slots[i]
		}
	}

	// 音色映射：命令行 > 配置 > 角色设定 > 默认
	voiceMap := buildVoiceMap(cfg, kit, opt)
	// 音色表必须可见：角色一开口是谁，开播前就能核对——
	// 也是发现"两个角色撞了音色"的第一现场（此时应查 voice_map 或音色池）。
	if keys := sortedVoiceKeys(voiceMap); len(keys) > 0 {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+voiceMap[k])
		}
		fmt.Printf("  🎙 音色表：%s\n", strings.Join(parts, "，"))
	}

	// TTS 供应商客户端：providers.tts 允许配音走另一家服务（如 MiniMax T2A v2，
	// emotion/pitch 参数才真正生效）。派生客户端共享同一个花费账本。
	// 严格模式：独立端点缺自己的密钥 → 报错——绝不允许继承主密钥去请求别家
	// （那既会 401，也让"配了 MiniMax 却打到智谱"这类错配悄悄发生）。
	ttsEp := cfg.Providers.TTS
	ttsClient, err := shotkit.CapClient(client, cfg, ai.CapTTS, ttsEp, ttsEp.Model)
	if err != nil {
		return nil, err
	}
	ttsModel := ttsEp.Model
	// consumeEmotion 标记当前 TTS 端点是否真正消费 emotion 参数：
	// MiniMax 与 seed-tts-1.x 支持；seed-tts-2.0（情绪靠上下文预测）与
	// glm-tts 不支持。不支持时情绪标注保持为空，不发送也不声称已生效。
	ttsBaseLower := strings.ToLower(ttsEp.BaseURL)
	consumeEmotion := strings.Contains(ttsBaseLower, "minimax") ||
		(ai.IsSeedTTSEndpoint(ttsEp.BaseURL) && !strings.Contains(ttsModel, "2.0"))
	// 台本级导演说明（tts_notes）里的全局语速（如"语速 0.88x"）：
	// 以前只写进 JSON 从不被消费。现在解析出来作为全局语速基准——
	// 配置 tts_speed 未显式设置（=1.0）时生效，逐句 speed 仍可覆盖。
	notesSpeed := parseTTSNotesSpeed(vo.TTSNotes)
	if notesSpeed > 0 {
		fmt.Printf("  🎚 台本 tts_notes 语速 %.2fx 生效\n", notesSpeed)
	}

	audioDir := filepath.Join(opt.RunDir, "output", "audio")
	procDir := filepath.Join(audioDir, "proc")
	for _, d := range []string{audioDir, procDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	plan := &Plan{
		RunID: filepath.Base(opt.RunDir), GeneratedAt: time.Now(),
		TotalSec: total, Shots: slots, VoiceMap: voiceMap,
	}

	// 逐句合成（并行）：TTS 请求、时长实测、变调、时长适配彼此独立，
	// 只依赖各自的台本行、音色与镜头槽位，可安全并行。
	// 真正必须串行的只有铺轨定位（排麦）：前一句的入点+实测时长决定
	// 下一句的最早入点——这一步在合成全部完成后按台本顺序进行，
	// 结果与串行版完全一致，只是把逐句的网络等待重叠了起来。
	workers := opt.Workers
	if workers <= 0 {
		workers = 2
	}
	lines := make([]*Line, len(vo.Narration))
	errs := make([]error, len(vo.Narration))
	var failed atomic.Bool
	conc.ForEachIdx(len(vo.Narration), workers, func(i int) {
		if failed.Load() {
			return // 已有失败：不再派发新的付费合成（与串行版"首错即停"同向）
		}
		line, err := prepareLine(client, ttsClient, ttsModel, notesSpeed, cfg, opt, kit, slotOf, voiceMap, consumeEmotion, vo.Narration[i])
		if err != nil {
			errs[i] = err
			failed.Store(true)
			return
		}
		lines[i] = line
	})
	for _, err := range errs {
		if err != nil {
			return nil, err // 首个错误按台本顺序返回，行为与串行版一致
		}
	}
	if skipped := countNil(lines); skipped > 0 {
		fmt.Printf("  ↷ %d 句台词所属镜头未渲染，已跳过（allow-incomplete）\n", skipped)
	}

	// 铺轨（串行，保持台本顺序——这是"台词与画面对应"的前提）
	// J-cut 前置信息：每个镜头在片中的前一镜、哪些镜头有台词。
	// 约束版 J-cut：某镜的**首句**台词且**前一镜完全没有台词**时，声音提前
	// ≤ audio.jcut_sec 越过切点——电影里最常见的转场手法，"声音带着观众进下一镜"。
	// 台词相撞/首镜/超槽等情况自动退化为普通排麦，绝不因此重叠。
	cursor := 0.0 // 已占用的时间游标，用于排麦（禁止叠音）
	shotHasDialogue := map[string]bool{}
	for _, n := range vo.Narration {
		shotHasDialogue[n.ShotID] = true
	}
	prevShotOf := map[string]string{}
	for i := 1; i < len(kit.ShotList); i++ {
		prevShotOf[kit.ShotList[i].ID] = kit.ShotList[i-1].ID
	}
	seenShot := map[string]bool{}
	for i, line := range lines {
		if line == nil || line.MeasuredSec == 0 {
			continue // 该句没有对应镜头片段，已跳过
		}
		slot := slotOf[vo.Narration[i].ShotID]
		var lead float64
		if cfg.Audio.JCutSec > 0 && !seenShot[slot.ShotID] {
			if prevID, ok := prevShotOf[slot.ShotID]; ok && !shotHasDialogue[prevID] {
				if prevSlot, ok := slotOf[prevID]; ok {
					lead = jcutLead(line, slot, prevSlot, cfg.Audio.JCutSec)
				}
			}
		}
		seenShot[slot.ShotID] = true
		// 声桥的前移下界：不得越出前一镜起点（无前一镜/前镜未渲染则不约束）
		prevStart := 0.0
		if prevID, ok := prevShotOf[slot.ShotID]; ok {
			if ps, ok := slotOf[prevID]; ok {
				prevStart = ps.Start
			}
		}
		cursor = layoutLine(line, slot, cursor, cfg, lead, prevStart)
		plan.Lines = append(plan.Lines, *line)
	}

	if len(plan.Lines) == 0 {
		return nil, fmt.Errorf("没有生成任何台词音频（检查台本与镜头 ID 是否对应）")
	}
	if err := fsx.AtomicWriteJSON(filepath.Join(opt.RunDir, "artifacts", "narration.json"), plan); err != nil {
		return nil, err
	}
	fmt.Printf("■ 配音完成：%d 句台词，实测总长 %.1fs，用时 %.1f 分钟\n",
		len(plan.Lines), total, time.Since(start).Minutes())
	overflow := 0
	for _, l := range plan.Lines {
		if l.Overflow {
			overflow++
		}
	}
	if overflow > 0 {
		fmt.Printf("  ⚠ %d 句台词超出所属镜头槽位（已按顺序顺延，字幕会跟随实测时间）：\n", overflow)
		for _, l := range plan.Lines {
			if l.Overflow {
				fmt.Printf("     [%s] %.2fs > 槽位 %.2fs：%s\n", l.ShotID, l.MeasuredSec, l.SlotSec, truncate(l.Text, 24))
			}
		}
	}
	return plan, nil
}

// alignSpeakWindows 就地填充 slots 的 SpeakWin：有台词的镜头才检测。
// 窗口缓存全局共享（artifacts/mouth_windows.json），重跑交付零检测成本。
func alignSpeakWindows(client *ai.Client, cfg *config.Config, runDir string,
	vo *voiceoverDoc, slots []ShotSlot) {
	// 嘴态判定是视觉调用：走 providers.vision 端点（与视觉复核同源），
	// 共享同一花费账本。端点不可用只降级对齐（回落普通入点），不阻断配音。
	visionClient, err := shotkit.CapClient(client, cfg, ai.CapVision, cfg.Providers.Vision, cfg.Models.Vision)
	if err != nil {
		fmt.Printf("  ⚠ 口型对齐不可用（%v），台词走普通入点\n", err)
		return
	}
	need := map[string]bool{}
	for _, n := range vo.Narration {
		if !promspec.IsNarrator(n.Speaker) {
			need[n.ShotID] = true
		}
	}
	cache := mouth.LoadCache(runDir)
	manifest, _ := loadManifest(runDir)
	videoOf := map[string]string{}
	if manifest != nil {
		for _, s := range manifest.Shots {
			if s.Status == "done" && s.Video != "" {
				videoOf[s.ID] = filepath.Join(runDir, filepath.FromSlash(s.Video))
			}
		}
	}
	var durOf map[string]float64
	if plan, err := loadPlanForWindows(runDir); err == nil {
		durOf = plan
	}
	var cacheMU sync.Mutex
	conc.ForEachIdx(len(slots), 2, func(i int) {
		s := slots[i]
		if !need[s.ShotID] {
			return
		}
		// cache map 被两个 worker 并发读写，须加锁——Go map 的并发读写
		// 会触发 "concurrent map read and map write" fatal。
		cacheMU.Lock()
		w, ok := cache[s.ShotID]
		cacheMU.Unlock()
		if ok {
			if w.Valid() {
				slots[i].SpeakWin = w
			}
			return
		}
		video := videoOf[s.ShotID]
		if video == "" {
			return
		}
		dur := durOf[s.ShotID]
		if dur <= 0 {
			dur = s.Duration
		}
		w, err := mouth.Detect(visionClient, video, dur, mouth.Options{Frames: 12, Width: 480})
		if err != nil {
			fmt.Printf("  ⚠ 口型检测 [%s] 失败（回落普通入点）: %v\n", s.ShotID, err)
			return
		}
		if w != nil && w.Valid() {
			slots[i].SpeakWin = w
			cacheMU.Lock()
			cache[s.ShotID] = w
			cacheMU.Unlock()
			fmt.Printf("  👄 [%s] 说话窗口 %.2fs–%.2fs（置信度 %.0f%%）\n",
				s.ShotID, w.Start, w.End, w.Confidence*100)
		}
	})
	if err := mouth.SaveCache(runDir, cache); err != nil {
		fmt.Printf("  ⚠ 口型窗口缓存写入失败（不影响本次交付）: %v\n", err)
	}
}

// loadPlanForWindows 从已缓存的实测时长里取镜头时长（避免重复 ffprobe）。
func loadPlanForWindows(runDir string) (map[string]float64, error) {
	var plan Plan
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "narration.json"), &plan); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, s := range plan.Shots {
		out[s.ShotID] = s.Duration
	}
	return out, nil
}

// countNil 统计被跳过的台词句（所属镜头未渲染，prepareLine 返回 nil）。
func countNil(lines []*Line) int {
	n := 0
	for _, l := range lines {
		if l == nil {
			n++
		}
	}
	return n
}

// prepareLine 合成并处理一句台词：TTS → 实测时长 → 合理性校验 → 变调 → 时长适配。
// 只依赖该句自己的台本行、音色与镜头槽位，与前后句无关，可安全并行；
// 铺轨入点（StartSec/AbsStartSec）因排麦依赖前一句的结束时间，
// 由 layoutLine 在全部合成完成后按台本顺序串行计算。
//
// ttsClient 是面向 TTS 端点的客户端（可与主客户端不同供应商）；
// notesSpeed 为台本 tts_notes 解析出的全局语速（0=未标注）；
// 逐句 speed 是最高优先级，其次 notes，最后配置 tts_speed。
// consumeEmotion 标记当前 TTS 端点是否真正消费 emotion 参数。
func prepareLine(client, ttsClient *ai.Client, ttsModel string, notesSpeed float64,
	cfg *config.Config, opt Options, kit *promspec.Kit,
	slotOf map[string]ShotSlot, voiceMap map[string]string, consumeEmotion bool, n narrationLine) (*Line, error) {

	// 全片去旁白（audio.skip_narrator）：对白驱动的短剧风格不需要画外音。
	// 合成入口过滤；plan.Lines 里不再有旁白（字幕/混音联动）。
	if cfg.Audio.SkipNarrator && promspec.IsNarrator(n.Speaker) {
		return nil, nil
	}
	slot, ok := slotOf[n.ShotID]
	if !ok {
		if opt.AllowIncomplete {
			// 残缺交付：该句所属镜头未渲染——显式跳过（不合成不占轨）。
			// 只有 --allow-incomplete 语义下才允许；正常交付仍视为数据不一致。
			return nil, nil
		}
		// 严格模式：台本引用了不存在的镜头 = 数据不一致，直接失败（不静默丢台词）
		return nil, fmt.Errorf("台词 [%s]（%s）没有对应镜头片段：台本与分镜不一致，请修正后重跑",
			n.ShotID, truncate(n.Text, 16))
	}
	voice := voiceMap[voiceKey(n.Speaker)]
	if voice == "" {
		voice = cfg.Audio.Voice
	}

	// 语速合成优先级：逐句 speed > 台本 tts_notes > 配置 tts_speed
	speed := cfg.Audio.TTSSpeed
	if notesSpeed > 0 && (speed <= 0 || speed == 1.0) {
		speed = notesSpeed
	}
	if n.Speed > 0 {
		speed = n.Speed
	}
	// 情绪标注：支持情感参数的供应商（MiniMax、seed-tts-1.0）真正生效；
	// 端点不支持（seed-tts-2.0 代际、glm-tts）时保持为空——不发送参数，
	// 也不在日志/narration.json 里声称情绪已被消费。
	emotion := ""
	if cfg.Audio.TTSEmotion && consumeEmotion {
		emotion = n.Emotion
	}

	// 缓存键 = 镜头名 + 内容指纹（文本/音色/语速/情绪/模型）：
	// 台本插句、调序不影响指纹；改任一合成要素则指纹变化，
	// 旧缓存自然失效重新合成——杜绝按行序号复用导致的静默错配。
	fp := lineFingerprint(ttsModel, n.Text, voice, speed, emotion)
	base := fmt.Sprintf("%s_%s", safeName(n.ShotID), fp)
	rawPath := filepath.Join(opt.RunDir, "output", "audio", base+".raw.wav")
	procPath := filepath.Join(opt.RunDir, "output", "audio", "proc", base+".wav")
	rawRel := filepath.ToSlash(filepath.Join("output", "audio", base+".raw.wav"))
	procRel := filepath.ToSlash(filepath.Join("output", "audio", "proc", base+".wav"))

	l := &Line{
		ShotID: n.ShotID, Speaker: n.Speaker, Text: n.Text, Voice: voice, Emotion: emotion,
		RawFile: rawRel, File: procRel, SlotSec: slot.Duration, Pitch: 1.0, Speed: 1.0,
	}

	// 1) 合成原始音频（断点续跑：同指纹已存在则复用，已付费的资产不重买；
	//    Force 为 true 时无视缓存强制重新合成，覆写旧文件）
	if fsx.FileExists(rawPath) && !opt.Force {
		l.Reused = true
	} else {
		fmt.Printf("  🎙 [%s] %s（音色 %s%s）合成中…\n", n.ShotID, n.Speaker, voice, emotionNote(emotion))
		data, err := ttsClient.TTSWith(n.Text, ai.TTSOptions{
			Voice: voice,
			// 服务端语速是"说话人真的这么说"，比后期拉伸自然；
			// 本地 atempo 只做超槽兜底。
			Speed:   speed,
			Model:   ttsModel,
			Emotion: emotion,
		})
		if err != nil {
			// 严格模式：无朗读文本的台词直接失败（不静默丢台词）——修正台本后重跑
			if errors.Is(err, ai.ErrEmptyTTSText) {
				return nil, fmt.Errorf("台词 [%s] %s 无可朗读文本（整句只有符号/舞台提示）: %s",
					n.ShotID, n.Speaker, truncate(n.Text, 20))
			}
			return nil, fmt.Errorf("TTS 失败 [%s %s]: %w", n.ShotID, n.Speaker, err)
		}
		if err := fsx.AtomicWrite(rawPath, data, 0o644); err != nil {
			return nil, err
		}
	}

	// 2) 原始时长：TTS 接口对 speed 的支持不一致，因此不依赖它，全部本地处理
	rawDur, err := measure(rawPath, client.Mock())
	if err != nil {
		return nil, fmt.Errorf("测量台词时长失败 [%s]: %w", n.ShotID, err)
	}
	l.RawSec = rawDur
	// 合理性校验：合成音频时长必须与文本体量匹配。
	// 不匹配说明 TTS 返回了错误内容（静音/截断/串音），这种问题必须当场拦住——
	// 放到成片里就是"台词与实际不符"，而观众一定会发现。
	if err := checkTTSDuration(n.Text, rawDur, client.Mock()); err != nil {
		return nil, fmt.Errorf("%s（镜头 %s，说话人 %s）", err.Error(), n.ShotID, n.Speaker)
	}

	// 3) 音色（变调）：仅当该角色与旁白**共用同一个 TTS 音色**时才用变调区分。
	// 变调（asetrate+atempo）会轻微改变共振峰听感，能不用就不用——
	// 音色表已区分角色时保持原声是最自然的方案；
	// 全员同音色时，变调是"同一个音色也能听出不同角色"的最低成本手段。
	if voice == voiceMap["narrator"] {
		l.Pitch = pitchFor(kit, n.Speaker)
	}

	// 4) 时长适配：有说话窗口时适配到窗长（口型段落对齐的核心），
	//    否则目标 = 镜头槽位可用时长（留出 0.25s 呼吸）
	slotBudget := slot.Duration - 0.25
	if slotBudget < 0.6 {
		slotBudget = slot.Duration
	}
	// 口型窗口装得下才按窗长适配（决策抽成纯函数，回归测试钉住方向——
	// 旧条件 win/raw >= MaxSpeedUp 方向写反，分支曾沦为空操作）
	if budget, ok := windowFitBudget(l.RawSec, slot.SpeakWin, cfg.Audio.MaxSpeedUp); ok {
		slotBudget = budget
	}
	proc, err := processToSlot(cfg, rawPath, procPath, l.Pitch, slotBudget)
	if err != nil {
		return nil, fmt.Errorf("处理台词 [%s] 失败: %w", n.ShotID, err)
	}
	l.File = procRel
	l.MeasuredSec = proc.Dur
	l.Speed = proc.Speed
	l.Truncated = proc.Truncated
	return l, nil
}

// jcutLead 计算安全的声音提前量：不越过前镜起点、不与游标冲突、
// 台词总长（提前量+实测时长）仍不超过本镜槽位。不满足任一条则返回 0（退化为普通排麦）。
func jcutLead(l *Line, slot, prevSlot ShotSlot, maxLead float64) float64 {
	lead := maxLead
	// 提前量不越过前一镜的起点（跨越超过一个镜头不是 J-cut，是错位）
	if slot.Start-lead < prevSlot.Start {
		lead = slot.Start - prevSlot.Start
	}
	// 台词提前进入前镜后，总占用仍须装进本镜槽位
	if lead+l.MeasuredSec > slot.Duration {
		lead = slot.Duration - l.MeasuredSec
	}
	if lead < 0.05 {
		return 0
	}
	return lead
}

// layoutLine 计算一句台词的铺轨入点并返回新的时间游标。
// 入点 = 镜头起点 + 指令入点，且不与上一句重叠（排麦）；
// 必须按台本顺序调用——上一句的结束时间决定本句的最早入点。
// jcutLead > 0 时入点提前越出镜头起点（J-cut），被排麦顶回则放弃提前。
// prevStart 为前一镜的实测起点（无前一镜传 0）：声桥前移不得越出它。
func layoutLine(l *Line, slot ShotSlot, cursor float64, cfg *config.Config, jcutLead float64, prevStart float64) float64 {
	rel := clamp(cfg.Audio.LeadInSec, 0, math.Max(0, slot.Duration-l.MeasuredSec))
	abs := slot.Start + rel
	if jcutLead > 0 {
		abs = slot.Start - jcutLead
	}
	// 口型窗口优先：入点=张嘴时刻。声音"等嘴"而不是嘴"等声"——
	// 窗口来自对这段视频的检测，置信度不足时上游不填窗口，走原逻辑。
	if w := slot.SpeakWin; w != nil && w.Valid() {
		abs = slot.Start + clamp(w.Start, 0, math.Max(0, slot.Duration))
		// 声桥（sound bridge）：台词比张嘴区间长、窗长适配被拒时，溢出
		// 不该全堆在闭嘴之后（"嘴闭了声音还在"是最直白的穿帮）。把入点
		// 前移让溢出落到窗口之前——声音先入，观众带着期待等嘴张开，
		// 声音结束点恰好落在闭嘴处（标准 J-cut 语法）。前移量受三重约束：
		// ≤溢出量（声音必须完整盖住窗口）、不与上一句叠音、不越出前一镜起点。
		if overflow := l.MeasuredSec - (w.End - w.Start); overflow > 0.05 {
			floor := cursor + cfg.Audio.LineGapSec
			floor = math.Max(floor, prevStart)
			if earliness := math.Min(overflow, abs-floor); earliness > 0 {
				abs -= earliness
			}
		}
		jcutLead = 0 // 下方按"入点是否越出镜头起点"重算真实提前量
	}
	if abs < cursor+cfg.Audio.LineGapSec {
		abs = cursor + cfg.Audio.LineGapSec
		jcutLead = 0 // 被上一句的尾巴顶回：放弃 J-cut，保证不叠音
	}
	if abs < slot.Start {
		jcutLead = slot.Start - abs // 声音越过切点：记为该句的真实提前量（字幕同步用）
	} else {
		jcutLead = 0 // 入点在镜头内（声桥被排麦吸收或无 J-cut）：无需字幕提前
	}
	l.StartSec = abs - slot.Start
	l.AbsStartSec = abs
	l.JCutSec = jcutLead
	l.MouthWin = slot.SpeakWin
	if l.MeasuredSec > slot.Duration {
		l.Overflow = true
	}
	note := ""
	if jcutLead > 0 {
		note = fmt.Sprintf("，J-cut 提前 %.2fs", jcutLead)
	}
	fmt.Printf("  🎬 [%s] %s → %.2fs（%.2fs%s）%s\n", l.ShotID, l.Speaker, abs, l.MeasuredSec, speedNote(l.Speed, l.Truncated), note)
	return abs + l.MeasuredSec
}

type procResult struct {
	Dur       float64
	Speed     float64
	Truncated bool
}

// processToSlot 把台词处理进槽位预算：静音裁剪 → 变调 → 必要时加速（有上限）。
// 是否需要加速由 ProcessVoice 按裁后实测时长决定，这里不重复实现一遍加速计算；
// "加速到上限仍放不下"从结果反推（处理后时长仍超出预算）。
func processToSlot(cfg *config.Config, raw, dst string, pitch, budget float64) (procResult, error) {
	res, err := audio.ProcessVoice(raw, dst, audio.VoiceOptions{
		Pitch: pitch, Slot: budget, MaxSpeed: cfg.Audio.MaxSpeedUp,
		SampleRate: cfg.Audio.SampleRate, Channels: cfg.Audio.Channels,
		TrimSilence: true,
	})
	if err != nil {
		return procResult{}, err
	}
	return procResult{
		Dur:   res.Dur,
		Speed: res.Speed,
		// 加速已被钳在上限，仍超出预算 = 上限救不回来，如实记录
		Truncated: res.Dur > budget+0.02,
	}, nil
}

// pitchFor 同音色兜底的音高区分：角色表有角色就略抬，旁白保持原声；
// 系统/广播/AI 类"非人声"角色压低，正好利用变调做出机械感。
func pitchFor(kit *promspec.Kit, speaker string) float64 {
	if promspec.IsNarrator(speaker) {
		return 1.0
	}
	if _, ok := kit.CharOf(speaker); ok {
		return 1.05 // 角色台词略高于旁白，形成层次
	}
	low := strings.ToLower(speaker)
	if strings.Contains(speaker, "系统") || strings.Contains(speaker, "广播") ||
		strings.Contains(low, "ai") || strings.Contains(low, "os") ||
		strings.Contains(speaker, "总部") {
		return 0.92 // 机械音：压低
	}
	return 1.03
}

// buildVoiceMap 构造角色 → 音色映射。
// 来源优先级：命令行 --voice-map > 配置 voice_map > 角色设定 tts_voice > 默认音色。
func buildVoiceMap(cfg *config.Config, kit *promspec.Kit, opt Options) map[string]string {
	out := map[string]string{}
	for _, c := range kit.Characters {
		if c.TTSVoice != "" {
			out[c.Name] = c.TTSVoice
		}
	}
	for k, v := range cfg.Audio.VoiceMap {
		out[k] = v
	}
	for k, v := range opt.VoiceMap {
		out[k] = v
	}
	if opt.Voice != "" {
		out["narrator"] = opt.Voice
	}
	if _, ok := out["narrator"]; !ok {
		out["narrator"] = cfg.Audio.NarratorVoice
	}
	return out
}

// sortedVoiceKeys 音色表按名字排序（确定性输出）。
func sortedVoiceKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func voiceKey(speaker string) string {
	if promspec.IsNarrator(speaker) {
		return "narrator"
	}
	return speaker
}

// reNotesSpeed 解析台本 tts_notes 里的全局语速："语速 0.88x" / "语速为1.1倍" / "0.95x"。
var reNotesSpeed = regexp.MustCompile(`语速[^0-9]{0,6}(\d+(?:\.\d+)?)|(\d+\.\d+)\s*[xX倍]`)

// parseTTSNotesSpeed 从台本 tts_notes 提取全局语速；未标注返回 0。
// 该字段以前只写进 JSON 从不被消费（"语速 0.88x、句尾下收"形同虚设）。
func parseTTSNotesSpeed(notes string) float64 {
	m := reNotesSpeed.FindStringSubmatch(notes)
	if m == nil {
		return 0
	}
	v := m[1]
	if v == "" {
		v = m[2]
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0.5 || f > 2.0 {
		return 0
	}
	return f
}

// emotionNote 日志里展示情绪标注（帮助确认 emotion 已被消费）。
func emotionNote(emotion string) string {
	if strings.TrimSpace(emotion) == "" {
		return ""
	}
	return "，情绪 " + truncate(emotion, 12)
}

// measureShots 测量镜头片段时长并计算绝对时间轴（全部实测，不用计划值）。
// 探测彼此独立且各是一次 ffprobe 子进程调用，批量并行以缩短墙钟时间；
// 结果仍按分镜顺序排布，失败语义与串行版一致（首个探测错误中止）。
func measureShots(runDir string, kit *promspec.Kit) ([]ShotSlot, float64, error) {
	manifest, err := loadManifest(runDir)
	if err != nil {
		return nil, 0, err
	}
	byID := map[string]string{}
	for _, s := range manifest.Shots {
		if s.Status == "done" && s.Video != "" {
			byID[s.ID] = filepath.Join(runDir, filepath.FromSlash(s.Video))
		}
	}
	var present []int // kit.ShotList 中已产出镜头的下标（保持分镜顺序）
	var paths []string
	for i, s := range kit.ShotList {
		if p, ok := byID[s.ID]; ok {
			present = append(present, i)
			paths = append(paths, p)
		}
	}
	durs, errs := ffx.ProbeBatch(paths, 0)
	for i, err := range errs {
		if err != nil {
			return nil, 0, fmt.Errorf("探测镜头 %s 时长失败: %w", kit.ShotList[present[i]].ID, err)
		}
	}
	var slots []ShotSlot
	var t float64
	for j, i := range present {
		s := kit.ShotList[i]
		slots = append(slots, ShotSlot{
			ShotID: s.ID, Start: t, Duration: durs[j], PlanSec: s.DurationSec,
			Characters: s.Characters,
		})
		t += durs[j]
	}
	// 时间轴必须单调递增：排版与混音都依赖它
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Start < slots[j].Start })
	return slots, t, nil
}

// ---------------------------------------------------------------------------
// 载入产物
// ---------------------------------------------------------------------------

type narrationLine struct {
	ShotID  string `json:"shot_id"`
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	Emotion string `json:"emotion"`
	// Speed 该句的语速覆盖（0.5-2；0=不覆盖）。voiceover 阶段可按句标注，
	// 如急促的对话 1.1、沉痛的独白 0.85。
	Speed float64 `json:"speed,omitempty"`
}

type voiceoverDoc struct {
	Narration []narrationLine `json:"narration"`
	TTSNotes  string          `json:"tts_notes"`
}

func loadVoiceover(runDir string) (*voiceoverDoc, error) {
	var vo voiceoverDoc
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "voiceover.json"), &vo); err != nil {
		return nil, fmt.Errorf("读取配音台本失败: %w", err)
	}
	if len(vo.Narration) == 0 {
		return nil, fmt.Errorf("配音台本为空")
	}
	return &vo, nil
}

// validateVoiceoverSpeakers 台本说话人与镜头在场角色的一致性校验。
// 角色表为空的镜头无法校验，跳过；旁白句跳过；名字做双向包含匹配，
// 容忍"杨戬" vs "杨戬（青年）"这类同角色异写。
func validateVoiceoverSpeakers(kit *promspec.Kit, vo *voiceoverDoc) error {
	charOf := map[string][]string{}
	for _, s := range kit.ShotList {
		charOf[s.ID] = s.Characters
	}
	var bad []string
	for _, n := range vo.Narration {
		if promspec.IsNarrator(n.Speaker) {
			continue
		}
		chars := charOf[n.ShotID]
		if len(chars) == 0 || speakerInScene(n.Speaker, chars) {
			continue
		}
		bad = append(bad, fmt.Sprintf("  [%s] 说话人 %q 不在镜头角色（%s）中：%s",
			n.ShotID, n.Speaker, strings.Join(chars, "、"), truncate(n.Text, 20)))
	}
	if len(bad) > 0 {
		return fmt.Errorf("配音台本与分镜不一致（说话人不在镜头在场角色中，成片会变成\"这张嘴配那个人的声音\"）：\n%s\n修正 voiceover.json 的 shot_id/speaker 对应关系后重跑",
			strings.Join(bad, "\n"))
	}
	return nil
}

// speakerInScene 说话人是否在场景角色列表中（双向包含匹配）。
func speakerInScene(speaker string, chars []string) bool {
	for _, c := range chars {
		if c == speaker || strings.Contains(c, speaker) || strings.Contains(speaker, c) {
			return true
		}
	}
	return false
}

func loadKit(runDir string) (*promspec.Kit, error) {
	var kit promspec.Kit
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "shotspec.json"), &kit); err != nil {
		return nil, fmt.Errorf("读取生成规格失败: %w", err)
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
	path := filepath.Join(runDir, "output", "render_manifest.json")
	if err := fsx.ReadJSON(path, &m); err != nil {
		return nil, fmt.Errorf("读取渲染清单失败（需先运行 studio render）: %w", err)
	}
	return &m, nil
}

// SlotsForSubtitle 把配音计划转换成字幕排版需要的槽位与行。
// 入点与时长都用**实测值**，这是字幕与声音对齐的关键。
func SlotsForSubtitle(plan *Plan) ([]subtitle.Slot, []subtitle.Line) {
	slots := make([]subtitle.Slot, 0, len(plan.Shots))
	// 每镜的声音提前量 = 该镜首句的 J-cut 提前量（字幕需同步提前）
	leadOf := map[string]float64{}
	for _, l := range plan.Lines {
		if l.JCutSec > leadOf[l.ShotID] {
			leadOf[l.ShotID] = l.JCutSec
		}
	}
	for _, s := range plan.Shots {
		slots = append(slots, subtitle.Slot{
			ShotID: s.ShotID, Start: s.Start, Dur: s.Duration, ActualDur: s.Duration,
			LeadInSec: leadOf[s.ShotID],
		})
	}
	lines := make([]subtitle.Line, 0, len(plan.Lines))
	for _, l := range plan.Lines {
		lines = append(lines, subtitle.Line{
			ShotID: l.ShotID, Speaker: l.Speaker, Text: l.Text,
			StartSec: l.AbsStartSec, DurSec: l.MeasuredSec,
		})
	}
	return slots, lines
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// measure 测量音频时长；离线模式下音频为占位内容，按文本长度估算。
func measure(path string, mock bool) (float64, error) {
	if d, err := audio.ProbeDuration(path); err == nil {
		return d, nil
	}
	if mock {
		return 1.5, nil
	}
	return 0, fmt.Errorf("无法读取音频时长: %s", filepath.Base(path))
}

func speedNote(speed float64, truncated bool) string {
	switch {
	case truncated:
		return "，已压至加速上限（仍超出槽位）"
	case speed > 1.01:
		return fmt.Sprintf("，加速 %.2fx", speed)
	default:
		return ""
	}
}

// checkTTSDuration 校验合成音频时长与文本体量是否匹配。
// 期望值取 4 字/秒（中文），允许 0.45x~2.2x 的宽区间以容纳语速差异；
// 超出该区间说明 TTS 返回的不是这段文本的音频。
func checkTTSDuration(text string, dur float64, mock bool) error {
	if mock {
		return nil
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	expect := float64(len(runes)) / 4.0
	if expect < 0.6 {
		expect = 0.6
	}
	ratio := dur / expect
	if ratio > 2.2 || ratio < 0.45 {
		return fmt.Errorf("TTS 音频时长异常：%.2fs（按 %d 字估算应约 %.2fs，比值 %.2f）。"+
			"这通常意味着合成失败、被截断或串了别的文本，请检查音色与文本难度后重试",
			dur, len(runes), expect, ratio)
	}
	return nil
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// lineFingerprint 台词合成要素的内容指纹：TTS 模型、文本、音色、语速、情绪。
// 用它做缓存键而不是行序号——台本插句、调序不影响指纹；改任一影响
// 合成结果的要素则指纹变化，旧缓存自然失效。1f 分隔符防止字段拼接歧义。
func lineFingerprint(model, text, voice string, speed float64, emotion string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		model,
		text,
		voice,
		strconv.FormatFloat(speed, 'f', 3, 64),
		emotion,
	}, "\x1f")))
	return hex.EncodeToString(sum[:6])
}

// windowFitBudget 口型窗口的时长适配决策（纯函数便于单测）：
// 音频比窗口长、且所需加速比 rawSec/win 在上限内 → 返回 (窗长, true)，
// 台词压进"闭嘴前说完"；装不下 → 返回 (0, false)，起点仍对齐窗口、
// 溢出如实标注（画外延续），不做无谓加速。
func windowFitBudget(rawSec float64, w *mouth.Window, maxSpeedUp float64) (float64, bool) {
	if w == nil || !w.Valid() || rawSec <= 0 {
		return 0, false
	}
	win := w.End - w.Start
	if win <= 0.6 || rawSec <= win || rawSec/win > maxSpeedUp {
		return 0, false
	}
	return win, true
}
