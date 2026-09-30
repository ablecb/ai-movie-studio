// Package shotkit 把"镜头规格"变成"可执行的逐镜头生成任务"。
//
// 生产级 AI 电影与 Demo 的核心差别在这三件事上，本包逐一落实：
//
//  1. **角色一致性**：先生成每个角色的标准参考帧（锁定身份），后续所有出镜镜头都以
//     该参考图作为图生图输入 + 固定种子。纯文字描述跨镜头必然换脸，这是物理限制。
//
//  2. **逐镜头生成而非整包生成**：提示词按镜头拆分，单个镜头失败只重试那一镜头，
//     而不是让模型重新生成全部镜头（原实现一次解析失败 = 全部提示词重来）。
//
//  3. **可复现**：种子由 (镜头ID, 重渲轮次, 角色) 确定性派生。同一轮次重跑得到同一结果，
//     换轮次才变——这样"重渲"是一个受控操作，而不是抽卡。
package shotkit

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"aistudio/internal/ai"
	"aistudio/internal/conc"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/promspec"
)

// Task 单个镜头的生成任务（渲染前的最终形态，落盘便于人工检查与定点重渲）。
type Task struct {
	ShotID      string `json:"shot_id"`
	SceneID     string `json:"scene_id"`
	ImagePrompt string `json:"image_prompt"`
	VideoPrompt string `json:"video_prompt"`
	Negative    string `json:"negative_prompt"`
	Seed        int64  `json:"seed"`
	// Characters 该镜头的出镜角色名。与 RefChars 的区别：RefChars 只在参考帧路径
	// 已知时才填充（BuildPlan 早于参考帧生成，那时恒为空），Characters 始终记录
	// 出镜角色——渲染管线在 EnsureRefFrames 之后按它回填 RefImage/UsedRef。
	Characters []string `json:"characters,omitempty"`
	RefImage   string   `json:"ref_image,omitempty"` // 角色参考帧路径（相对 run 目录）
	// RefImages 全部出镜角色的参考帧（渲染期回填）。单角色时与 RefImage 相同；
	// 多角色时交给多参考图模型（providers.image_edit）或参考图生视频模型
	// （video.reference_model），解决"同框第二人无图像约束"的老问题。
	RefImages []string `json:"ref_images,omitempty"`
	RefChars  []string `json:"ref_chars,omitempty"` // 触发参考图模式的角色
	UsedRef   bool     `json:"used_ref_image"`      // 本次生成是否真的用了参考图
	// ChainFromPrev 该镜头与上一镜同场景，可承接上镜末帧作为首帧（末帧→首帧
	// 链式衔接）。真正生效还需配置 video.chain_same_scene=true 且上镜已产出。
	ChainFromPrev bool `json:"chain_from_prev,omitempty"`
	// CameraMove 该镜头的运镜枚举（中文）。参考图生视频模型（vidu 系）的
	// movement_amplitude 由此映射：运镜幅度是分镜的戏剧意图，不应交给模型自定。
	CameraMove   string  `json:"camera_move,omitempty"`
	DurationSec  float64 `json:"duration_sec"`
	Priority     int     `json:"priority"`
	Risk         string  `json:"risk"`
	MustHaveFace bool    `json:"must_have_face"` // 该镜头质检时必须检出人脸
	// Night 夜戏镜头（场景 time_of_day 为夜）。客观质检据此放宽亮度/黑帧阈值
	// 并跳过肤色启发式的人脸判定——压黑是光比设计，不是生成失败。
	Night bool `json:"night,omitempty"`
}

// IsNightTimeOfDay 场景时段是否属于"夜"（夜戏质检放行的判据）。
// 黄昏/黎明不算：它们仍有环境光，肤色启发式可用。
func IsNightTimeOfDay(tod string) bool {
	t := strings.ToLower(strings.TrimSpace(tod))
	if t == "" {
		return false
	}
	for _, kw := range []string{"夜", "night", "midnight", "凌晨", "深夜"} {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
}

// Plan 一部片子的完整生成计划。
type Plan struct {
	Style      promspec.Style           `json:"style"`
	Tasks      []Task                   `json:"tasks"`
	Characters []promspec.CharacterSpec `json:"characters"`
	// PrevOf 镜头 → 同场景内的上一镜头（链式衔接的依据；跨场景/片头不存在）。
	PrevOf  map[string]string `json:"prev_of,omitempty"`
	Version int               `json:"version"`
}

// TaskOf 按镜头 ID 取任务。
func (p *Plan) TaskOf(id string) (Task, bool) {
	for _, t := range p.Tasks {
		if t.ID() == id {
			return t, true
		}
	}
	return Task{}, false
}

// ID 兼容 ShotID 命名。
func (t Task) ID() string { return t.ShotID }

// DeriveSeed 由镜头 ID + 轮次 + 角色确定性派生种子。
// 确定性是生产要求：同一轮次重跑必须得到同一张图，否则"重试"会变成不可控的抽卡，
// 也使人工复核无法复现问题。
func DeriveSeed(shotID string, round int, salt string) int64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", shotID, round, salt)))
	v := binary.BigEndian.Uint64(h[:8])
	// 收敛到 32bit 正整数：多数图像接口对种子范围有要求
	return int64(v % (1 << 31))
}

// BuildPlan 从 Kit + 重渲轮次构造生成计划。
// rerenderIDs 为需要重渲的镜头集合（空=全新生成）；rerenderRound 决定种子变化。
func BuildPlan(kit *promspec.Kit, rerenderIDs map[string]bool, rerenderRound int) *Plan {
	plan := &Plan{Style: kit.Style, Characters: kit.Characters, Version: kit.Version, PrevOf: map[string]string{}}
	prevScene := ""
	prevID := ""
	for _, s := range kit.ShotList {
		if s.SceneID == prevScene && prevID != "" {
			plan.PrevOf[s.ID] = prevID
		}
		prevScene, prevID = s.SceneID, s.ID
	}
	for _, s := range kit.ShotList {
		if len(rerenderIDs) > 0 && !rerenderIDs[s.ID] {
			continue
		}
		var chars []promspec.CharacterSpec
		var charNames []string
		var refChars []string
		var refImage string
		for _, name := range s.Characters {
			c, ok := kit.CharOf(name)
			if !ok {
				continue
			}
			chars = append(chars, c)
			charNames = append(charNames, name)
			if c.RefImage != "" {
				refChars = append(refChars, name)
				if refImage == "" {
					refImage = c.RefImage
				}
			}
		}
		// 种子：有角色出镜时以主角锚点定种，保证换镜头不换脸的概率更高
		salt := ""
		if len(chars) > 0 {
			salt = chars[0].Token
		}
		seed := s.Seed
		if seed == 0 || rerenderRound > 0 {
			seed = DeriveSeed(s.ID, rerenderRound, salt)
		}
		_, chained := plan.PrevOf[s.ID]
		t := Task{
			ShotID:        s.ID,
			SceneID:       s.SceneID,
			ImagePrompt:   promspec.ShotPrompt(kit.Style, s, chars),
			VideoPrompt:   promspec.VideoPrompt(kit.Style, s),
			Negative:      promspec.NegativesFor(s),
			Seed:          seed,
			Characters:    charNames,
			RefChars:      refChars,
			RefImage:      refImage,
			ChainFromPrev: chained,
			CameraMove:    s.CameraMove,
			DurationSec:   s.DurationSec,
			Priority:      s.Priority,
			Risk:          s.Risk,
			MustHaveFace:  len(chars) > 0 && s.ShotSize != "大远景" && s.ShotSize != "远景",
			Night:         IsNightTimeOfDay(s.TimeOfDay),
		}
		plan.Tasks = append(plan.Tasks, t)
	}
	return plan
}

// RefResult 角色参考帧生成结果。
type RefResult struct {
	Name   string `json:"name"`
	Token  string `json:"token"`
	Path   string `json:"path"` // 相对 run 目录
	Seed   int64  `json:"seed"`
	Error  string `json:"error,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// QCStatus 参考帧质检结论：pass（核对通过）/ corrected（重出后通过）/
	// failed（重出预算耗尽仍不合格，保留最佳候选并告警）/ skipped（视觉模型不可用）/
	// ""（未启用质检）。结论随 manifest 落盘，"没查"绝不表现为"通过"。
	QCStatus string `json:"qc_status,omitempty"`
	// QCIssues 质检发现的问题（failed/skipped 时非空，供人工核对与修圣经）
	QCIssues []string `json:"qc_issues,omitempty"`
}

// EnsureRefFrames 生成/复用角色参考帧。
//
// 为什么必须有这一步：跨镜头一致性的唯一可靠工程手段是"有一张确定的脸作为参考图"。
// 参考帧一旦生成就复用（断点续跑），只有明确要求重刷时才重新生成——
// 重新生成参考帧会改变全片角色的长相，属于重大操作，必须显式触发。
//
// 各角色的参考帧彼此独立（独立的出图请求 + 下载），按 image.workers 有界并行；
// 失败语义与串行版一致：余额/鉴权类致命错误立即停批（不再派发新任务），
// 单角色失败只记录在该角色的结果里，不拖垮其他角色。
// results 始终按角色表顺序返回，只包含已处理（复用/成功/失败）的角色。
//
// vision 非空且 cfg.RefQC 开启时，新生成的参考帧先经视觉模型对照角色圣经逐项核对，
// 不合格换种子重出（最多 cfg.RefQC.MaxRounds 轮）；复用的既有参考帧不重查
// （它已在当初生成时验收过，重查只会重复花钱）。
func EnsureRefFrames(client *ai.Client, cfg *config.Config, runDir string, kit *promspec.Kit, force bool, vision *ai.Client) ([]RefResult, error) {
	dir := filepath.Join(runDir, "output", "refs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	workers := cfg.Image.Workers
	if workers <= 0 {
		workers = 3
	}
	// 离线模式下参考帧本身是占位物料，视觉核对没有对象，直接视为未启用
	if vision != nil && vision.Mock() {
		vision = nil
	}
	if vision != nil && cfg.RefQCEnabled() {
		fmt.Printf("  🔍 参考帧质检已启用（strict，最多重出 %d 轮）：模型 %s\n", cfg.RefQC.MaxRounds, vision.EndpointInfo())
	}
	results := make([]RefResult, len(kit.Characters))
	done := make([]bool, len(kit.Characters))
	var fatalErr error
	var failed atomic.Bool
	var mu sync.Mutex // 保护 fatalErr 的并发写入
	conc.ForEachIdx(len(kit.Characters), workers, func(i int) {
		if failed.Load() {
			return // 已致命失败：不再派发新的付费出图（在跑的让它自然结束）
		}
		c := &kit.Characters[i] // 每个 goroutine 只写自己的角色，无共享竞争
		rel := filepath.ToSlash(filepath.Join("output", "refs", safeName(c.Token)+".png"))
		abs := filepath.Join(runDir, rel)
		res := RefResult{Name: c.Name, Token: c.Token, Path: rel, Seed: c.RefSeed}
		if fsx.FileExists(abs) && !force {
			// 复用既有参考帧：这是全片一致性的基准，绝不能无意中换脸
			c.RefImage = rel
			results[i], done[i] = res, true
			return
		}
		basePrompt := promspec.RefPrompt(kit.Style, *c)
		res.Prompt = basePrompt
		qcRounds := 0
		if vision != nil && cfg.RefQCEnabled() {
			qcRounds = cfg.RefQC.MaxRounds
		}
		var fixHint string // 上一轮核对给出的修正指令（重出时追加）
		for round := 0; ; round++ {
			prompt := basePrompt
			if round > 0 {
				prompt = basePrompt + fixHint
			}
			url, model, err := client.ImageGenEx(prompt, ai.ImageOptions{
				Model:     cfg.Image.Model,
				Size:      cfg.Image.RefSize,
				Quality:   cfg.Image.Quality,
				Watermark: cfg.Image.Watermark,
			}, cfg.Image.FallbackModels)
			if err != nil {
				// 重出失败时保留上一轮已落盘的候选（旧帧未被删除，Download 是原子改名）
				if round > 0 && fsx.FileExists(abs) {
					res.QCStatus = "failed"
					res.QCIssues = append(res.QCIssues, "重出失败，保留上一轮候选: "+err.Error())
				} else {
					res.Error = err.Error()
				}
				results[i], done[i] = res, true
				if ai.IsFatal(err) {
					mu.Lock()
					fatalErr = err
					mu.Unlock()
					failed.Store(true) // 余额/鉴权：立即停，别继续烧钱
				}
				return
			}
			if err := Download(url, abs); err != nil {
				res.Error = "下载参考帧失败: " + err.Error()
				results[i], done[i] = res, true
				return
			}
			res.Seed = DeriveSeed("ref", round, c.Token)
			c.RefSeed = res.Seed
			c.RefImage = rel
			if round == 0 {
				fmt.Printf("  🧑 角色参考帧已就绪 [%s] 模型=%s → %s\n", c.Name, model, rel)
			} else {
				fmt.Printf("  🧑 参考帧第 %d 轮重出完成 [%s] 模型=%s → %s\n", round, c.Name, model, rel)
			}

			if qcRounds == 0 {
				results[i], done[i] = res, true
				return
			}
			// 质检关卡：对照角色圣经逐项核对，不合格换种重出
			v, err := QCRefFrame(vision, abs, *c)
			if err != nil {
				res.QCStatus = "skipped"
				res.QCIssues = []string{"视觉核对不可用（不拦生产，但需人工核对参考帧）: " + err.Error()}
				fmt.Printf("  ⚠ 参考帧质检跳过 [%s]: %v\n", c.Name, err)
				results[i], done[i] = res, true
				return
			}
			if v.Pass {
				res.QCStatus = "pass"
				if round > 0 {
					res.QCStatus = "corrected"
					fmt.Printf("  ✓ 参考帧重出后通过核对 [%s]（%.1f 分）\n", c.Name, v.Score)
				}
				results[i], done[i] = res, true
				return
			}
			issues := append(append([]string{}, v.Missing...), v.Drift...)
			issues = append(issues, v.Issues...)
			if round >= qcRounds {
				res.QCStatus = "failed"
				res.QCIssues = issues
				fmt.Printf("  ⚠ 参考帧核对未通过 [%s]（%.1f 分，重出 %d 轮仍不合格，保留最佳候选）: %s\n",
					c.Name, v.Score, qcRounds, strings.Join(issues, "；"))
				fmt.Printf("    建议：修正角色圣经后 studio render --run … --force-ref，或核对 output/refs/ 后人工放行\n")
				results[i], done[i] = res, true
				return
			}
			fmt.Printf("  ↻ 参考帧核对未通过 [%s]（%.1f 分）：%s → 换种重出\n",
				c.Name, v.Score, strings.Join(issues, "；"))
			fixHint = qcCorrectionPrompt(v)
		}
	})
	// 按角色表顺序收敛结果（与串行版一致）；致命错误时未派发的角色不进结果
	var out []RefResult
	for i := range results {
		if done[i] {
			out = append(out, results[i])
		}
	}
	return out, fatalErr
}

// Download 原子下载：先写 .part 再改名。
// 半截文件一旦被断点续跑当成"已完成"跳过，就会静默混进成片——这比显式失败昂贵得多。
func Download(url, path string) error {
	tmp := path + ".part"
	if strings.HasPrefix(url, "mock://") {
		// 离线演示：占位内容也要走原子写，保持与真实路径一致的语义
		if err := fsx.AtomicWrite(tmp, []byte("mock-asset "+url), 0o644); err != nil {
			return err
		}
	} else {
		if err := httpDownload(url, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "char"
	}
	return out
}

func copyIfMissing(src, dst string) error {
	if fsx.FileExists(dst) {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return fsx.AtomicWrite(dst, data, 0o644)
}

// ValidatePlan 生成前自检：把"必然失败"的问题在付费之前拦下来。
func ValidatePlan(plan *Plan, cfg *config.Config) []string {
	var problems []string
	if len(plan.Tasks) == 0 {
		problems = append(problems, "生成计划为空：没有可渲染的镜头")
	}
	for _, t := range plan.Tasks {
		if strings.TrimSpace(t.ImagePrompt) == "" {
			problems = append(problems, fmt.Sprintf("镜头 %s 缺少 image_prompt", t.ShotID))
		}
		if strings.TrimSpace(t.VideoPrompt) == "" {
			problems = append(problems, fmt.Sprintf("镜头 %s 缺少 video_prompt", t.ShotID))
		}
		if t.DurationSec <= 0 {
			problems = append(problems, fmt.Sprintf("镜头 %s 时长非法（%.2f）", t.ShotID, t.DurationSec))
		}
		if len(t.RefChars) > 0 && t.RefImage == "" {
			problems = append(problems, fmt.Sprintf("镜头 %s 需要角色参考图但未提供（角色一致性无法保证）", t.ShotID))
		}
	}
	if cfg.Video.Workers > 4 {
		problems = append(problems, fmt.Sprintf("并发 %d 过高，视频接口限流概率大", cfg.Video.Workers))
	}
	return problems
}

// ProbeDuration 测量片段时长（统一走 ffx，保证与质检口径一致）。
func ProbeDuration(path string) (float64, error) {
	p, err := ffx.ProbeFile(path)
	if err != nil {
		return 0, err
	}
	return p.DurationSec, nil
}
