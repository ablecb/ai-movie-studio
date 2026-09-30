package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
	"aistudio/internal/skill"
	"aistudio/internal/srt"
)

// ---------------- S5 配音/旁白台本 ----------------

type NarrationLine struct {
	ShotID  string `json:"shot_id"`
	Speaker string `json:"speaker"` // 角色名 或 "narrator"
	Text    string `json:"text"`
	Emotion string `json:"emotion"`
	// Speed 该句语速（0.5-2；缺省=全局语速）。
	// 急促对话 1.05-1.15，沉痛独白 0.85-0.95；这是"读出来的表演"，逐句标注
	// 比全局语速更能去掉机器感。0 表示不覆盖。
	Speed float64 `json:"speed,omitempty"`
}

type VoiceoverScript struct {
	Narration []NarrationLine `json:"narration"`
	TTSNotes  string          `json:"tts_notes"`
}

type voiceoverSkill struct{}

func (voiceoverSkill) Name() string { return "voiceover" }
func (voiceoverSkill) Description() string {
	return "配音台本：旁白/台词/情绪标注/TTS 参数"
}

func (voiceoverSkill) Run(ctx *skill.Context) (any, error) {
	var script Script
	var sb Storyboard
	if err := ctx.DepOf("script", &script); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("storyboard", &sb); err != nil {
		return nil, err
	}
	out := &VoiceoverScript{}
	if ctx.AI.Mock() {
		out = &VoiceoverScript{
			Narration: []NarrationLine{
				{ShotID: "sh001", Speaker: "narrator", Text: "深空第七年，一束信号撕开了寂静。", Emotion: "低沉悬念"},
				{ShotID: "sh002", Speaker: "信使·林", Text: "信号源……是麦田？调转航向！", Emotion: "急切"},
				{ShotID: "sh003", Speaker: "narrator", Text: "穿越三百个日夜，云层之下是答案。", Emotion: "辽阔温暖"},
				{ShotID: "sh004", Speaker: "信使·林", Text: "我回来了。", Emotion: "哽咽释然"},
			},
			TTSNotes: "narrator 用低音男声，林用角色音色；语速 0.95x；48kHz，输出 WAV",
		}
	} else {
		system := filmBase + `
你是声音导演。基于剧本与分镜产出配音台本 JSON，schema：
{"narration":[{"shot_id":"sh001","speaker":"角色名或narrator","text":"台词/旁白","emotion":"情绪标注","speed":1.0}],"tts_notes":"整体 TTS 制作说明（音色、语速、采样率、格式）"}
专业要求：每句口播时长必须与所属镜头 duration_sec 匹配（中文约 4 字/秒），宁可删词不可超时；台词沿用剧本对白可轻度润色；旁白承担叙事缝合计；emotion 从 {平静, 开心, 悲伤, 愤怒, 恐惧, 惊讶, 厌恶, 低语} 中选词并可附短说明（如"悲伤，句尾下收"），该标注会映射为 TTS 情感参数；需要表演感差别的句子用 speed 标注逐句语速（0.5-2，急促 1.1、沉痛 0.9）；tts_notes 里可写全局语速如"语速 0.95x"；只输出 JSON。`
		user := "剧本：" + marshal(script) + "\n分镜表：" + marshal(sb)
		if err := callJSON(ctx, "voiceover", system, user, out); err != nil {
			return nil, err
		}
	}
	if err := validate(len(out.Narration) > 0, "配音台本为空"); err != nil {
		return nil, err
	}

	// 专业校验：镜头槽位存在性 + 台词时长预算。
	// TTS 音频超槽只能靠加速硬压（有 1.25x 上限），超了就会挤压后续台词连锁后移，
	// 所以必须在台本层就拦住，而不是等混音时听天由命。
	durOf := map[string]float64{}
	for _, sh := range sb.Shots {
		durOf[sh.ID] = sh.DurationSec
	}
	// 无对白的纯视觉镜头是正常电影语法：模型偶尔给无台词镜头补一条空条目，
	// 直接丢弃而不是判死整个阶段（全空仍会被上面的「配音台本为空」拦住）。
	// 修复指令「请删除该条」实测模型不遵守，确定性过滤才是下限。
	narr := make([]NarrationLine, 0, len(out.Narration))
	for _, n := range out.Narration {
		if strings.TrimSpace(n.Text) != "" {
			narr = append(narr, n)
		}
	}
	out.Narration = narr

	load := map[string]float64{} // 镜头 → 累计口播秒数
	for _, n := range out.Narration {
		if _, ok := durOf[n.ShotID]; !ok {
			return nil, fmt.Errorf("产出校验失败: 台词 [%s] 的 shot_id=%q 不存在于分镜，请修正 shot_id 或删除该句", n.Speaker, n.ShotID)
		}
		if n.Speed != 0 && (n.Speed < 0.5 || n.Speed > 2) {
			return nil, fmt.Errorf("产出校验失败: 台词 [%s] 的 speed=%.2f 超出 0.5-2 范围，请修正或删除该字段", n.Speaker, n.Speed)
		}
		load[n.ShotID] += srt.EstimateSpeakSec(n.Text)
		if !isNarrator(n.Speaker) && !scriptHasSpeaker(script, n.Speaker) {
			return nil, fmt.Errorf("产出校验失败: 台词 [%s] 说话人 %q 既不是 narrator 也不在剧本角色中，请修正 speaker", n.ShotID, n.Speaker)
		}
	}
	for shotID, spoken := range load {
		if budget := durOf[shotID] * 1.5; spoken > budget { // 1.5x=混音端 1.25x 加速仍能装下的上限
			return nil, fmt.Errorf("产出校验失败: 镜头 %s 台词总量约 %.1f 秒，超出镜头时长 %.1f 秒可容纳的口播预算，请精简该镜头台词（中文约 4 字/秒）", shotID, spoken, durOf[shotID])
		}
	}
	return out, nil
}

func isNarrator(speaker string) bool {
	s := strings.ToLower(strings.TrimSpace(speaker))
	return s == "narrator" || strings.Contains(speaker, "旁白")
}

func scriptHasSpeaker(script Script, speaker string) bool {
	for _, sc := range script.Scenes {
		for _, d := range sc.Dialogue {
			if nameMatches(d.Character, speaker) {
				return true
			}
		}
	}
	return false
}

// ---------------- S6 音乐与音效 Cue Sheet ----------------

type Cue struct {
	ID string `json:"id"`
	// Section 剧情段落的人类可读标签（"第一幕·深空"）。
	Section string `json:"section"`
	// SceneID 所属剧本场景 id（对应分镜的 scene_id）。deliver 按该场景的
	// 实测镜头槽把 cue 对齐到场景起止——没有它，配乐只能从 0 顺序铺，
	// 情绪段会漂离它为之而写的那场戏。
	SceneID     string  `json:"scene_id"`
	Mood        string  `json:"mood"`
	Tempo       string  `json:"tempo"`
	Instruments string  `json:"instruments"`
	DurationSec float64 `json:"duration_sec"`
}

type SFX struct {
	ShotID      string `json:"shot_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type MusicPlan struct {
	CueSheet []Cue `json:"cue_sheet"`
	SoundFX  []SFX `json:"sound_fx"`
}

type musicSkill struct{}

func (musicSkill) Name() string { return "music" }
func (musicSkill) Description() string {
	return "音乐音效：Cue Sheet / 情绪曲线 / 拟音清单"
}

func (musicSkill) Run(ctx *skill.Context) (any, error) {
	var script Script
	var sb Storyboard
	if err := ctx.DepOf("script", &script); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("storyboard", &sb); err != nil {
		return nil, err
	}
	out := &MusicPlan{}
	if ctx.AI.Mock() {
		out = &MusicPlan{
			CueSheet: []Cue{
				{ID: "cue01", Section: "第一幕·深空", SceneID: "s1", Mood: "悬疑克制", Tempo: "60 BPM", Instruments: "低音弦乐+电子脉冲", DurationSec: 10},
				{ID: "cue02", Section: "第二幕·归途", SceneID: "s2", Mood: "坚定上扬", Tempo: "90 BPM", Instruments: "弦乐群+圆号", DurationSec: 13},
			},
			SoundFX: []SFX{
				{ShotID: "sh001", Name: "信号蜂鸣", Description: "短促电子蜂鸣，逐渐清晰"},
				{ShotID: "sh003", Name: "破云风声", Description: "高速风噪过渡到开阔混响"},
			},
		}
	} else {
		system := filmBase + `
你是声音设计总监。产出配乐与音效方案 JSON，schema：
{"cue_sheet":[{"id":"cue01","section":"对应剧情段落","scene_id":"s1","mood":"情绪","tempo":"BPM","instruments":"配器","duration_sec":10}],"sound_fx":[{"shot_id":"sh001","name":"音效名","description":"音效描述与触发时机"}]}
专业要求：cue 与剧本情绪曲线对齐、cue 总时长覆盖全片；scene_id 必须引用分镜表中的场景 id（配乐会精确对齐到该场景的时间段）；关键转折点必须有音乐变化；镜头级关键音效（脚步/机械/环境）齐全；只输出 JSON。`
		user := "剧本：" + marshal(script) + "\n分镜表：" + marshal(sb)
		if err := callJSON(ctx, "music", system, user, out); err != nil {
			return nil, err
		}
	}
	if err := validate(len(out.CueSheet) > 0, "cue sheet 为空"); err != nil {
		return nil, err
	}
	// 专业校验：音效必须挂在真实镜头上；cue 必须绑定真实场景（deliver 按场景
	// 的实测时间槽落 cue，绑定错了配乐就会压在别的戏上）；cue 总时长须覆盖全片
	shotIDs := map[string]bool{}
	sceneIDs := map[string]bool{}
	for _, sh := range sb.Shots {
		shotIDs[sh.ID] = true
		sceneIDs[sh.SceneID] = true
	}
	for _, fx := range out.SoundFX {
		if !shotIDs[fx.ShotID] {
			return nil, fmt.Errorf("产出校验失败: 音效 %q 挂载的 shot_id=%q 不存在于分镜，请修正", fx.Name, fx.ShotID)
		}
	}
	for _, c := range out.CueSheet {
		if !sceneIDs[strings.TrimSpace(c.SceneID)] {
			return nil, fmt.Errorf("产出校验失败: 配乐 %q 的 scene_id=%q 不存在于分镜，请从分镜表的 scene_id 中选取", c.ID, c.SceneID)
		}
	}
	var cueTotal float64
	for _, c := range out.CueSheet {
		cueTotal += c.DurationSec
	}
	if cueTotal < sb.TotalDurationSec*0.6 {
		return nil, fmt.Errorf("产出校验失败: cue 总时长 %.1f 秒未覆盖全片 %.1f 秒的 60%%，请补齐中段配乐避免情绪断层", cueTotal, sb.TotalDurationSec)
	}
	return out, nil
}

// ---------------- S7 字幕（镜头槽位制，机械逻辑零 AI 调用） ----------------

type SubtitleResult struct {
	SRTFile   string `json:"srt_file"`
	LineCount int    `json:"line_count"`
}

type subtitlesSkill struct{}

func (subtitlesSkill) Name() string        { return "subtitles" }
func (subtitlesSkill) Description() string { return "字幕生成：按镜头时序产出 SRT 文件" }

func (subtitlesSkill) Run(ctx *skill.Context) (any, error) {
	var sb Storyboard
	var vo VoiceoverScript
	if err := ctx.DepOf("storyboard", &sb); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("voiceover", &vo); err != nil {
		return nil, err
	}

	// 计划槽位（渲染完成后会用实测镜头时长重排，见 render 包 retimeSubtitles）
	var slots []srt.Slot
	var t float64
	for _, sh := range sb.Shots {
		slots = append(slots, srt.Slot{ShotID: sh.ID, Start: t, Dur: sh.DurationSec})
		t += sh.DurationSec
	}
	var lines []srt.Line
	for _, n := range vo.Narration {
		lines = append(lines, srt.Line{ShotID: n.ShotID, Text: n.Text})
	}
	cues := srt.Plan(slots, lines)

	outDir := filepath.Join(ctx.WorkDir, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	srtPath := filepath.Join(outDir, "subtitles.srt")
	if err := fsx.AtomicWrite(srtPath, []byte(srt.Render(cues)), 0o644); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(ctx.WorkDir, srtPath)
	return &SubtitleResult{SRTFile: rel, LineCount: len(cues)}, nil
}

// ---------------- S8 剪辑决策与渲染方案 ----------------

type Cut struct {
	ShotID     string  `json:"shot_id"`
	StartSec   float64 `json:"start_sec"`
	EndSec     float64 `json:"end_sec"`
	Transition string  `json:"transition"` // fade_in / cut / dissolve
}

type EditPlan struct {
	Timeline      []Cut  `json:"timeline"`
	FFmpegCommand string `json:"ffmpeg_command"`
	RenderNotes   string `json:"render_notes"`
}

type editplanSkill struct{}

func (editplanSkill) Name() string { return "editplan" }
func (editplanSkill) Description() string {
	return "剪辑方案：时间线/转场/ffmpeg 渲染脚本"
}

func (editplanSkill) Run(ctx *skill.Context) (any, error) {
	var sb Storyboard
	var kit promspec.Kit
	if err := ctx.DepOf("storyboard", &sb); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("shotspec", &kit); err != nil {
		return nil, err
	}
	if len(kit.ShotList) != len(sb.Shots) {
		return nil, fmt.Errorf("生成规格(%d)与分镜(%d)镜头数不一致，无法排时间线", len(kit.ShotList), len(sb.Shots))
	}

	out := &EditPlan{Timeline: []Cut{}}
	var t float64
	lastScene := ""
	for i, sh := range sb.Shots {
		tr := "cut"
		switch {
		case i == 0:
			tr = "fade_in" // 影片开场淡入
		case sh.SceneID != lastScene:
			tr = "dissolve" // 场景切换叠化
		}
		out.Timeline = append(out.Timeline, Cut{
			ShotID:     sh.ID,
			StartSec:   t,
			EndSec:     t + sh.DurationSec,
			Transition: tr,
		})
		lastScene = sh.SceneID
		t += sh.DurationSec
	}
	out.FFmpegCommand = "见 output/render.sh（bash）/ output/render.ps1（PowerShell）"
	out.RenderNotes = "转场策略：开场淡入、场景切换叠化、片尾淡出，其余硬切；" +
		"成片规格 H.264 CRF18 / AAC 192k；渲染链路 studio render → studio deliver（配音/字幕/混音/校验）"

	// 生成可直接执行的生产脚本：生成 → 交付（配音/字幕/混音/成片校验）
	// 每一步都断点续跑：已付费的产物不会被重买
	var cmds strings.Builder
	cmds.WriteString(`#!/usr/bin/env bash
# AI 电影生产脚本（editplan 自动生成）——依赖 studio CLI、ffmpeg/ffprobe
# 用法: bash output/render.sh   （或 STUDIO=/path/to/studio bash output/render.sh）
set -euo pipefail
cd "$(dirname "$0")/.."   # 切到 run 根目录

STUDIO="${STUDIO:-studio}"

# 步骤 0：先看成本，再决定要不要花钱（--dry-run 不产生任何付费调用）
"$STUDIO" render --run . --dry-run

# 步骤 1：逐镜头生成（角色参考帧锁定身份 + 客观质检 + 缺陷镜头换种重渲）
#   缺镜头会以非零退出码结束，避免残缺成片被当成成品
"$STUDIO" render --run .

# 步骤 2：配音（逐句实测时长）+ 字幕（按实测重排）+ 混音（响度归一化）
#         + 合成 + 成片校验，全部由 deliver 一次完成
"$STUDIO" deliver --run .

# 步骤 3：复核交付报告
"$STUDIO" verify --run .

echo "生产完成: output/final_release.mp4"
`)

	outDir := filepath.Join(ctx.WorkDir, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := fsx.AtomicWrite(filepath.Join(outDir, "render.sh"), []byte(cmds.String()), 0o755); err != nil {
		return nil, err
	}
	// render.ps1：同一生产流程的 PowerShell 版——本项目大量用户在 Windows 上，
	// bash 脚本对他们不可用（"生成的脚本自己都跑不了"是 obviously 的穿帮）
	var ps strings.Builder
	ps.WriteString(`# AI 电影生产脚本（editplan 自动生成）——依赖 studio CLI、ffmpeg/ffprobe
# 用法: powershell -File output/render.ps1   （或 $env:STUDIO="D:\path\studio.exe"; .\output\render.ps1）
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

if (-not $env:STUDIO) { $env:STUDIO = "studio" }

# 步骤 0：先看成本，再决定要不要花钱（--dry-run 不产生任何付费调用）
& $env:STUDIO render --run . --dry-run
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

# 步骤 1：逐镜头生成（角色参考帧锁定身份 + 客观质检 + 缺陷镜头换种重渲）
& $env:STUDIO render --run .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

# 步骤 2：配音 + 字幕 + 混音 + 合成 + 成片校验
& $env:STUDIO deliver --run .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

# 步骤 3：复核交付报告
& $env:STUDIO verify --run .

Write-Host "生产完成: output/final_release.mp4"
`)
	if err := fsx.AtomicWrite(filepath.Join(outDir, "render.ps1"), []byte(ps.String()), 0o644); err != nil {
		return nil, err
	}
	// concat.txt：计划时间线清单（实际合成由 deliver 按实测产物完成）
	var ct strings.Builder
	for _, c := range out.Timeline {
		fmt.Fprintf(&ct, "file 'shots/%s.mp4'\nduration %v\n", c.ShotID, c.EndSec-c.StartSec)
	}
	if err := fsx.AtomicWrite(filepath.Join(outDir, "concat.txt"), []byte(ct.String()), 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------- 注册 ----------------
