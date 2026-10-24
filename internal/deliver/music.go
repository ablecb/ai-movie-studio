package deliver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aistudio/internal/audio"
	"aistudio/internal/config"
	"aistudio/internal/fsx"
	"aistudio/internal/narrate"
	"aistudio/internal/promspec"
)

// sfxRoleMarker 音效文件名前缀：<shot_id>.sfx.wav。
//
// 为什么用显式标记而不是"<shot_id>.wav"：后者会与按 cues 命名的配乐
// （cue01.wav）混淆，一旦撞名就会把配乐当成某个镜头的音效挂上去——
// 这种错误在成片里表现为"某个镜头突然响了不对劲的声"，很难排查。
const sfxRoleMarker = ".sfx"

// scanMusicAndSFX 把音乐音效方案（music.json）与磁盘上已有的素材配对。
//
// 这是原实现缺失的一环：music.json 产出 Cue Sheet 与拟音清单，
// 但后期从不消费它，等于"方案写完就扔"。现在的约定：
//
//	output/music/<cue_id>.wav|mp3|m4a   按 cue 铺在时间轴上（缺文件则该 cue 跳过）
//	output/music/<顺序号>.wav           也接受（cue01/1.wav 两种命名都行）
//	output/sfx/<shot_id>.sfx.wav        挂在对应镜头起点
//
// cue 的落点：music.json 里每条 cue 带 scene_id（音乐技能阶段强制校验过），
// 按实测镜头槽把 cue **对齐到所属场景的起止**——情绪段与画面段重合，
// 转折点的音乐变化正好落在转场上；素材超长时在场景结束处截断，
// 不会漫进下一条 cue 叠成"同时响两首"。cue 没有 scene_id（旧方案文件）
// 时退回从 0 顺序铺（旧行为）。
//
// 音乐总长不足成片时循环铺满（仅单轨时）。系统不会假装能凭空生成音乐：
// 没有素材时明确报告"本次成片无配乐"，而不是静默出一部干片。
func scanMusicAndSFX(runDir string, plan *narrate.Plan, kit *promspec.Kit, cfg *config.Config) ([]audio.Track, []audio.Track) {
	planOf := loadMusicPlan(runDir)

	// ---- 场景 → 实测时间跨度（cue 对齐的依据）----
	// 镜头实测槽位来自配音计划（deliver 已按 ffprobe 实测重排）；
	// 场景 → 镜头的归属来自 shotspec（分镜顺序即场景内镜头顺序）。
	type span struct{ start, end float64 }
	slotOf := map[string]float64{}
	slotEnd := map[string]float64{}
	for _, s := range plan.Shots {
		slotOf[s.ShotID] = s.Start
		slotEnd[s.ShotID] = s.Start + s.Duration
	}
	firstShotOf := map[string]string{}
	lastShotOf := map[string]string{}
	var sceneOrder []string
	if kit != nil {
		for _, sh := range kit.ShotList {
			if _, seen := firstShotOf[sh.SceneID]; !seen {
				firstShotOf[sh.SceneID] = sh.ID
				sceneOrder = append(sceneOrder, sh.SceneID)
			}
			lastShotOf[sh.SceneID] = sh.ID
		}
	}
	sceneSpanOf := func(sceneID string) (span, bool) {
		sceneID = strings.TrimSpace(sceneID)
		if sceneID == "" {
			return span{}, false
		}
		first, okF := firstShotOf[sceneID]
		last, okL := lastShotOf[sceneID]
		if !okF || !okL {
			return span{}, false
		}
		s0, ok0 := slotOf[first]
		e0, ok1 := slotEnd[last]
		if !ok0 || !ok1 || e0 <= s0 {
			return span{}, false
		}
		return span{start: s0, end: e0}, true
	}

	// ---- 配乐：优先按 music.json 的 cue 配对 ----
	var music []audio.Track
	musicDir := filepath.Join(runDir, "output", "music")
	files := listAudio(musicDir)
	at := 0.0
	used := map[string]bool{}
	aligned := 0
	for i, cue := range planOf.CueSheet {
		path := matchCue(files, cue.ID, i, used)
		if path == "" {
			continue
		}
		used[path] = true
		dur, err := audio.ProbeDuration(path)
		if err != nil {
			continue
		}
		tr := audio.Track{Path: path, GainDB: cfg.Audio.BGMGainDB, FadeOutSec: cfg.Audio.CueFadeOutSec}
		if sp, ok := sceneSpanOf(cue.SceneID); ok {
			// 场景对齐：起点=场景首镜实测入点，终点=场景末镜实测出点
			tr.StartSec = sp.start
			tr.EndSec = sp.end
			aligned++
			if sp.end > at {
				at = sp.end // 后续无归属的 cue 排在最后一条已对齐 cue 之后
			}
		} else {
			tr.StartSec = at
			at += dur
		}
		music = append(music, tr)
	}
	if aligned > 0 {
		fmt.Printf("  🎵 %d 条配乐按场景边界对齐（%d 个场景的实测时间槽）\n", aligned, len(sceneOrder))
	}
	// 有素材但 music.json 没有 cue（或全部未匹配）时，按文件名顺序顺序铺
	if len(music) == 0 && len(files) > 0 {
		for _, f := range files {
			dur, err := audio.ProbeDuration(f)
			if err != nil {
				continue
			}
			music = append(music, audio.Track{
				Path: f, StartSec: at, GainDB: cfg.Audio.BGMGainDB,
				Loop: len(files) == 1, // 单轨才循环铺满
			})
			at += dur
			if at >= plan.TotalSec && len(files) > 1 {
				break
			}
		}
	}

	// ---- 音效：<shot_id>.sfx.<ext> → 挂在该镜头起点 ----
	var sfx []audio.Track
	for _, f := range listAudio(filepath.Join(runDir, "output", "sfx")) {
		base := filepath.Base(f)
		name := strings.TrimSuffix(base, filepath.Ext(base))
		// 只接受带 .sfx 标记的文件，避免把配乐误挂成音效
		if !strings.HasSuffix(name, sfxRoleMarker) {
			continue
		}
		id := strings.TrimSuffix(name, sfxRoleMarker)
		start, ok := slotOf[id]
		if !ok {
			continue
		}
		sfx = append(sfx, audio.Track{
			Path: f, StartSec: start, GainDB: cfg.Audio.SFXGainDB,
		})
	}
	return music, sfx
}

type musicPlanDoc struct {
	CueSheet []struct {
		ID          string  `json:"id"`
		Section     string  `json:"section"`
		SceneID     string  `json:"scene_id"` // 所属剧本场景（cue 对齐场景边界的依据；旧文件没有则顺序铺）
		Mood        string  `json:"mood"`
		Tempo       string  `json:"tempo"`
		Instruments string  `json:"instruments"`
		DurationSec float64 `json:"duration_sec"`
	} `json:"cue_sheet"`
	SoundFX []struct {
		ShotID      string `json:"shot_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"sound_fx"`
}

func loadMusicPlan(runDir string) *musicPlanDoc {
	var doc musicPlanDoc
	_ = fsx.ReadJSON(filepath.Join(runDir, "artifacts", "music.json"), &doc)
	return &doc
}

// matchCue 为一条 cue 找到素材：先按 cue id，再按顺序号（cue01 / 1 / 01）。
func matchCue(files []string, cueID string, order int, used map[string]bool) string {
	candidates := []string{
		cueID,
		"cue" + pad2(order+1),
		pad2(order + 1),
	}
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		for _, f := range files {
			base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
			if used[f] {
				continue
			}
			if strings.EqualFold(base, cand) {
				return f
			}
		}
	}
	return ""
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// listAudio 列出目录下的音频文件（按名称排序，保证铺轨顺序确定）。
func listAudio(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !isAudioFile(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}
