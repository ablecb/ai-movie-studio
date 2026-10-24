package deliver

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"aistudio/internal/ai"
	"aistudio/internal/audio"
	"aistudio/internal/config"
	"aistudio/internal/fsx"
	"aistudio/internal/shotkit"
)

// ---------------------------------------------------------------------------
// BGM 自动化："不会主动配背景音乐"的解药
//
// 现状链路里混音器、侧链闪避、响度归一都是现成的，缺的只是"把 cue 变成音频"：
// music.json 产出 Cue Sheet 后，配乐全靠人工往 output/music/ 丢文件，没文件
// 就出干片。这里补上自动化两级供给：
//
//  1. 本地曲库自动选曲（零成本、离线可用）：library/music/ 下的音频文件
//     按**文件名标签**索引（悬疑_克制_60bpm_弦乐.wav → tags+tempo），
//     按 cue 的 mood/instruments/section 匹配打分，最优者复制为
//     output/music/<cue_id>.<ext>，混音链自动拾取。
//  2. 音乐生成 API（可选）：曲库无匹配且 music.generate=true 时，
//     按 cue 描述（情绪/节奏/配器）调用 providers.music 生成纯音乐。
//     生成失败只告警、该 cue 保持缺失——BGM 是增强项，绝不阻断交付。
// ---------------------------------------------------------------------------

type musicLibItem struct {
	File        string   `json:"file"` // 绝对或相对曲库目录的路径
	Tags        []string `json:"tags"` // 情绪/配器标签（来自文件名或 index.json）
	Tempo       int      `json:"tempo,omitempty"`
	DurationSec float64  `json:"duration_sec,omitempty"`
}

var reBPMToken = regexp.MustCompile(`^(\d{2,3})\s*bpm$`)

// loadMusicLibrary 载入曲库索引：优先 library/music/index.json（人工整理的
// 标签更准），否则扫描目录并从文件名解析标签（"悬疑_克制_60bpm_弦乐.wav"）。
func loadMusicLibrary(cfg *config.Config) []musicLibItem {
	dir := cfg.Music.LibraryDir
	if dir == "" {
		return nil
	}
	var idx struct {
		Items []musicLibItem `json:"items"`
	}
	if err := fsx.ReadJSON(filepath.Join(dir, "index.json"), &idx); err == nil && len(idx.Items) > 0 {
		return idx.Items
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []musicLibItem
	for _, e := range entries {
		if e.IsDir() || !isAudioFile(e.Name()) {
			continue
		}
		item := musicLibItem{File: filepath.Join(dir, e.Name()), Tags: []string{}}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		for _, tok := range strings.FieldsFunc(base, func(r rune) bool {
			return r == '_' || r == '-' || r == ' ' || r == '，' || r == ','
		}) {
			if m := reBPMToken.FindStringSubmatch(strings.ToLower(tok)); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					item.Tempo = v
					continue
				}
			}
			if strings.TrimSpace(tok) != "" {
				item.Tags = append(item.Tags, tok)
			}
		}
		if d, err := audio.ProbeDuration(item.File); err == nil {
			item.DurationSec = d
		}
		out = append(out, item)
	}
	return out
}

// pickMusicForCue 为一条 cue 打分选曲。score<=0 表示曲库里没有"沾边"的素材。
func pickMusicForCue(items []musicLibItem, mood, section, instruments string, wantSec float64) (musicLibItem, float64) {
	cueText := mood + " " + section + " " + instruments
	wantTempo := 0
	if m := reBPMToken.FindStringSubmatch(strings.ToLower(instruments + "_" + section)); m != nil {
		wantTempo, _ = strconv.Atoi(m[1])
	}
	best := musicLibItem{}
	bestScore := 0.0
	for _, it := range items {
		score := 0.0
		for _, tag := range it.Tags {
			if tag == "" {
				continue
			}
			if strings.Contains(cueText, tag) {
				score += 2 // 曲库标签命中 cue 描述
			}
		}
		if wantTempo > 0 && it.Tempo > 0 && math.Abs(float64(it.Tempo-wantTempo)) <= 10 {
			score += 1
		}
		if score <= 0 {
			continue
		}
		// 时长够用加分（循环铺短素材可听出接缝）；同分取更长者
		if wantSec > 0 && it.DurationSec >= wantSec {
			score += 0.5
		}
		score += math.Min(it.DurationSec/600.0, 0.5)
		if score > bestScore {
			best, bestScore = it, score
		}
	}
	return best, bestScore
}

// ensureMusic 保证 output/music/ 里每个 cue 都有素材：曲库自动选曲 →
// （可选）生成 API。返回需要上报的告警（曲库缺口）与供给过程本身的错误。
//
// 严格模式：生成调用失败/复制失败/落盘失败都是错误（直接中止交付）；
// "曲库无匹配素材"是缺输入不是错误——如实上报为"该 cue 无配乐"。
func ensureMusic(client *ai.Client, cfg *config.Config, runDir string) ([]string, error) {
	plan := loadMusicPlan(runDir)
	if len(plan.CueSheet) == 0 {
		return nil, nil
	}
	musicDir := filepath.Join(runDir, "output", "music")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		return nil, fmt.Errorf("BGM 自动化失败（无法创建目录 %s）: %w", musicDir, err)
	}
	existing := map[string]bool{}
	for _, f := range listAudio(musicDir) {
		existing[strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))] = true
	}

	var lib []musicLibItem
	var warnings []string
	var musicClient *ai.Client
	if cfg.Music.Generate {
		ep := cfg.Providers.Music
		if cfg.Mock {
			// 离线演示：生成走占位音频（MusicGen 的 mock 通道），不校验端点
			if client != nil && ep.BaseURL != "" && ep.Model != "" {
				musicClient = client.For(ai.CapMusic, ai.Endpoint{
					BaseURL: ep.BaseURL, APIKeyEnv: ep.APIKeyEnv,
					APIKey: cfg.APIKeyFor(ep), Model: ep.Model,
				})
			}
		} else {
			// 严格模式：开启生成就必须配置完整且可用，不允许"配了等于没配"
			if ep.BaseURL == "" || ep.Model == "" || client == nil {
				return nil, fmt.Errorf("music.generate=true 但 providers.music 端点未配置完整（需要 base_url 与 model）")
			}
			mc, err := shotkit.CapClient(client, cfg, ai.CapMusic, ep, ep.Model)
			if err != nil {
				return nil, err
			}
			musicClient = mc
		}
	}

	for i, cue := range plan.CueSheet {
		if existing[cue.ID] || existing["cue"+pad2(i+1)] || existing[pad2(i+1)] {
			continue // 已有素材（人工投喂或上次生成），不重复供给
		}
		if len(lib) == 0 {
			lib = loadMusicLibrary(cfg)
		}
		// 一级：曲库自动选曲
		if item, score := pickMusicForCue(lib, cue.Mood, cue.Section, cue.Instruments, cue.DurationSec); score > 0 {
			dst := filepath.Join(musicDir, cue.ID+strings.ToLower(filepath.Ext(item.File)))
			if err := copyFile(item.File, dst); err != nil {
				return nil, fmt.Errorf("cue %s 曲库复制失败: %w", cue.ID, err)
			}
			existing[cue.ID] = true
			fmt.Printf("  🎵 cue %s ← 曲库选曲 %s（%s）\n", cue.ID,
				filepath.Base(item.File), cue.Mood)
			continue
		}
		// 二级：生成 API（严格模式：生成失败即失败）
		if musicClient != nil {
			prompt := fmt.Sprintf("纯音乐，无人声。情绪：%s。节奏：%s。配器：%s。影视配乐质感。",
				cue.Mood, cue.Tempo, cue.Instruments)
			audioData, err := musicClient.MusicGen(prompt, int(math.Max(10, cue.DurationSec)))
			if err != nil {
				return nil, fmt.Errorf("cue %s 音乐生成失败: %w", cue.ID, err)
			}
			dst := filepath.Join(musicDir, cue.ID+".mp3")
			if err := fsx.AtomicWrite(dst, audioData, 0o644); err != nil {
				return nil, fmt.Errorf("cue %s 生成音频落盘失败: %w", cue.ID, err)
			}
			existing[cue.ID] = true
			fmt.Printf("  🎵 cue %s ← 生成（%s）\n", cue.ID, cue.Mood)
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"cue %s（%s）无配乐素材：往 %s 放置带情绪标签的音频文件（如 %s_%s_60bpm_弦乐.wav），"+
				"或在 config.json 开启 music.generate 走生成 API",
			cue.ID, cue.Mood, cfg.Music.LibraryDir, cue.Mood, cue.Instruments))
	}
	return warnings, nil
}

// ensureSFX 音效曲库自动供给：music.json 的 sound_fx 描述与 library/sfx/
// 的文件名标签匹配（`雨_屋顶_循环.wav` ← "雨点打在铁皮屋顶上"），
// 命中即复制为 output/sfx/<shot_id>.sfx.wav，混音链自动挂到对应镜头起点。
// 此前音效清单是"写完就扔"的方案——SFX 只有在人工逐个丢文件时才会发声。
func ensureSFX(cfg *config.Config, runDir string) []string {
	plan := loadMusicPlan(runDir)
	if len(plan.SoundFX) == 0 {
		return nil
	}
	sfxDir := filepath.Join(runDir, "output", "sfx")
	if err := os.MkdirAll(sfxDir, 0o755); err != nil {
		return []string{"音效库供给失败（无法创建目录）: " + err.Error()}
	}
	existing := map[string]bool{}
	for _, f := range listAudio(sfxDir) {
		base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		if strings.HasSuffix(base, sfxRoleMarker) {
			existing[strings.TrimSuffix(base, sfxRoleMarker)] = true
		}
	}
	lib := loadSFXLibrary(cfg)
	var warnings []string
	for _, fx := range plan.SoundFX {
		if fx.ShotID == "" || strings.TrimSpace(fx.Name) == "" {
			continue
		}
		if existing[fx.ShotID] {
			continue
		}
		if item, score := pickSFXForCue(lib, fx.Name); score > 0 {
			dst := filepath.Join(sfxDir, fx.ShotID+".sfx"+strings.ToLower(filepath.Ext(item.File)))
			if err := copyFile(item.File, dst); err != nil {
				warnings = append(warnings, fmt.Sprintf("音效 %s 曲库复制失败: %v", fx.ShotID, err))
				continue
			}
			existing[fx.ShotID] = true
			fmt.Printf("  🔊 [%s] 音效 ← 曲库 %s（%s）\n", fx.ShotID, filepath.Base(item.File), fx.Name)
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"音效 [%s]（%s）无素材：往 output/sfx/ 放置 %s.sfx.wav，"+
				"或在 %s/ 放置带描述标签的音频文件（如 雨_屋顶_循环.wav）自动匹配",
			fx.ShotID, fx.Name, fx.ShotID, cfg.Music.SFXLibraryDir))
	}
	return warnings
}

// loadSFXLibrary 扫描音效库：文件名下划线/空格分词即标签。
func loadSFXLibrary(cfg *config.Config) []musicLibItem {
	dir := cfg.Music.SFXLibraryDir
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []musicLibItem
	for _, e := range entries {
		if e.IsDir() || !isAudioFile(e.Name()) {
			continue
		}
		item := musicLibItem{File: filepath.Join(dir, e.Name()), Tags: []string{}}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		for _, tok := range strings.FieldsFunc(base, func(r rune) bool {
			return r == '_' || r == '-' || r == ' ' || r == '，' || r == ','
		}) {
			if tok = strings.TrimSpace(tok); tok != "" {
				item.Tags = append(item.Tags, tok)
			}
		}
		out = append(out, item)
	}
	return out
}

// pickSFXForCue 按"音效描述里的词命中曲库标签"打分；同分取先者。
func pickSFXForCue(items []musicLibItem, desc string) (musicLibItem, float64) {
	best := musicLibItem{}
	bestScore := 0.0
	for _, it := range items {
		score := 0.0
		for _, tag := range it.Tags {
			if tag != "" && strings.Contains(desc, tag) {
				score += 2
			}
		}
		if score > bestScore {
			best, bestScore = it, score
		}
	}
	return best, bestScore
}

func copyFile(src, dst string) error {
	if fsx.FileExists(dst) {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return fsx.AtomicWrite(dst, data, 0o644)
}

// sortMusicLib 稳定输出（供测试）。
func sortMusicLib(items []musicLibItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].File < items[j].File })
}
