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
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// RefNames 与 RefImages 一一对应的角色名（渲染期随 RefImages 一起回填）。
	// 多模态参考生视频要按位置声明每张参考图的用途（@imageN = 谁的标准像），
	// 没有名字轴对不上号——某角色缺参考帧时 RefImages 会跳过它，
	// 名单必须与图序同源同序，声明才不会张冠李戴。
	RefNames   []string `json:"ref_names,omitempty"`
	RefChars   []string `json:"ref_chars,omitempty"` // 触发参考图模式的角色
	UsedRef    bool     `json:"used_ref_image"`      // 本次生成是否真的用了参考图
	// SceneRefImage 该镜头所属场次的 establishing 基准图（output/assets/scenes/，
	// 相对 run 目录）。渲染期回填，是环境一致性的图像级锚点：没有它，同一场景
	// 跨镜头的"同一个屋子/同一个夜晚"只能靠提示词文本自己维持——文字约束环境
	// 的强度远低于一张参考图。素材阶段未产出的 run 保持为空（未提供 ≠ 降级）。
	SceneRefImage string `json:"scene_ref_image,omitempty"`
	// SceneRefState 该基准图对应的环境状态变体 slug（空 = 场次主基准）。
	// 来自 shotspec 的 scene_state；复核端据此把镜头与它实际使用的基准图对上。
	SceneRefState string `json:"scene_ref_state,omitempty"`
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

// QCRound 一轮参考帧候选的完整档案（候选图 + 审核结论），审核链路的节点。
//
// 为什么逐轮落盘：被否掉的候选图若不留档，就被下一轮重出覆盖，"为什么打回、
// 改完什么样"只剩一句 stdout 日志——审核过程对人工就是黑盒。每轮候选图存到
// output/refs/qc/<token>_round<k>.png，结论（分数/缺失项/添加物/修正指令）随
// 素材索引落盘，Web 控制台据此展示完整的审核→修复→通过链路。
type QCRound struct {
	Round int    `json:"round"`           // 0=首出，1..=第几轮重出
	Image string `json:"image,omitempty"` // 本轮候选图存档路径（相对 run 目录）
	Model string `json:"model,omitempty"`
	Seed  int64  `json:"seed,omitempty"`
	// Verdict 本轮视觉核对结论（未启用质检时为 nil——"没查"绝不表现为"通过"）
	Verdict *RefQCVerdict `json:"verdict,omitempty"`
	// QCError 质检调用失败原因（严格模式下即该角色失败的直接原因）
	QCError string `json:"qc_error,omitempty"`
	// GenError 本轮出图/下载失败原因（重出失败时保留上一轮候选）
	GenError string `json:"gen_error,omitempty"`
	// ArchiveError 候选图存档失败原因（不影响生产，但人工追溯时会缺图）
	ArchiveError string `json:"archive_error,omitempty"`
	// FixHintEN 本轮被否后追加给出图提示词的修正指令（下一轮实际执行的修复）
	FixHintEN string `json:"fix_hint_en,omitempty"`
	// Reused 本轮候选是复用的既有文件（上次运行未完成核对，补查而非重出图）
	Reused bool `json:"reused,omitempty"`
	// Recovered 该轮档案由调用记录回溯恢复（旧流程未留档的历史 run：
	// 候选图来自调用记录归档、结论来自当时的核对调用记录；非本流水线原生记录）
	Recovered bool `json:"recovered,omitempty"`
	// ApprovedCopy 该轮通过核对后的审核存档副本（output/refs/qc/<key>_approved.png）：
	// 主路径被覆盖/误删时由此恢复，绝不重新生成（重出就是换脸）
	ApprovedCopy string `json:"approved_copy,omitempty"`
	// Final 该轮候选即最终采用的帧（渲染锁脸用的就是它）
	Final bool      `json:"final,omitempty"`
	At    time.Time `json:"at,omitempty"`
}

// RefResult 角色参考帧生成结果。
type RefResult struct {
	Name   string `json:"name"`
	Token  string `json:"token"`
	Path   string `json:"path"` // 相对 run 目录
	Seed   int64  `json:"seed"`
	Error  string `json:"error,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// Variant 帧变体：""（base，主锚点帧，渲染锁脸用）/ side / back / full_body /
	// closeup / wardrobe-xxx / state-xxx。同一角色可有多个条目，base 的文件名与
	// 旧版完全一致（token.png），保证渲染管线与既有索引向后兼容。
	Variant string `json:"variant,omitempty"`
	// Purpose 规划理由（中文，展示用）。
	Purpose string `json:"purpose,omitempty"`
	// QCStatus 参考帧质检结论：pass（核对通过）/ corrected（重出后通过）/
	// failed（重出预算耗尽仍不合格，保留最佳候选并告警）/ skipped（视觉模型不可用）/
	// ""（未启用质检）。结论随 manifest 落盘，"没查"绝不表现为"通过"。
	QCStatus string `json:"qc_status,omitempty"`
	// QCIssues 质检发现的问题（failed/skipped 时非空，供人工核对与修圣经）
	QCIssues []string `json:"qc_issues,omitempty"`
	// QCReview 审核链路：逐轮候选（含被否掉的）的图片存档与核对结论。
	// 未启用质检时只有首出一轮（Verdict 为空 = 未查）；旧版生成的帧为空数组。
	QCReview []QCRound `json:"qc_review,omitempty"`
}

// EnsureRefFrames 生成/复用角色参考帧（按素材规划，每角色可多帧）。
//
// 为什么必须有这一步：跨镜头一致性的唯一可靠工程手段是"有一张确定的脸作为参考图"。
// 参考帧一旦生成就复用（断点续跑），只有明确要求重刷时才重新生成——
// 重新生成参考帧会改变全片角色的长相，属于重大操作，必须显式触发。
//
// 帧序列来自素材规划（plan 为 nil 时每角色仅 base，与旧行为一致）：base 是渲染
// 锁脸的唯一主锚点，文件名/种子与旧版完全一致；变体帧（侧面/全身/换装…）以
// base 为图生图输入派生，同一张脸由参考图保证。质检关卡只把守 base——变体的
// 身份由 base 传递，重复质检是双倍视觉模型花费。
//
// 各角色的参考帧彼此独立（独立的出图请求 + 下载），按 image.workers 有界并行；
// 失败语义与串行版一致：余额/鉴权类致命错误立即停批（不再派发新任务），
// 单角色失败只记录在该角色的结果里，不拖垮其他角色。
// results 始终按角色表顺序返回，只包含已处理（复用/成功/失败）的帧。
//
// vision 非空且 cfg.RefQC 开启时，新生成的 base 参考帧先经视觉模型对照角色圣经
// 逐项核对，不合格换种子重出（最多 cfg.RefQC.MaxRounds 轮）；复用的既有参考帧
// 不重查（它已在当初生成时验收过，重查只会重复花钱）。
//
// 审核链路全程留痕：每轮候选图归档到 output/refs/qc/、核对结论（分数/缺失项/
// 修正指令）记入 RefResult.QCReview 并落盘素材索引——"哪张被否、改了什么、
// 几轮后通过"对人工完全可追溯，审核不是黑盒。
// 未完成核对的复用帧会先补查（不重新出图），补查结果接续在既有链路之后。
func EnsureRefFrames(client *ai.Client, cfg *config.Config, runDir string, kit *promspec.Kit,
	plan *AssetPlan, force bool, vision *ai.Client) ([]RefResult, error) {
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
	frames := make([][]RefFrameSpec, len(kit.Characters))
	for i := range kit.Characters {
		frames[i] = plan.FramesFor(kit.Characters[i].Name)
	}
	// 既有质检结论按 token+变体留存：复用帧时带回，避免合并索引时把
	// 上次质检的 pass/failed 结论覆盖成空（前端会误显示为 unknown）
	prevQC := map[string]RefResult{}
	for _, r := range LoadAssetIndex(runDir).Characters {
		prevQC[r.Token+"\x00"+r.Variant] = r
	}
	resPer := make([][]RefResult, len(kit.Characters))
	done := make([]bool, len(kit.Characters))
	var fatalErr error
	var failed atomic.Bool
	var mu sync.Mutex // 保护 fatalErr 的并发写入
	conc.ForEachIdx(len(kit.Characters), workers, func(i int) {
		if failed.Load() {
			return // 已致命失败：不再派发新的付费出图（在跑的让它自然结束）
		}
		c := &kit.Characters[i] // 每个 goroutine 只写自己的角色，无共享竞争

		// ---- base 主锚点帧 ----
		baseRel := filepath.ToSlash(filepath.Join("output", "refs", frameFileKey(c.Token)+".png"))
		// 兼容既有 run 的老命名（完整 token 当文件名）：老文件在、新命名不在 → 继续用老文件。
		// 一致性基准绝不能因为改名而重新生成（那就是换脸）。
		if legacyRel := filepath.ToSlash(filepath.Join("output", "refs", safeName(c.Token)+".png")); legacyRel != baseRel &&
			!fsx.FileExists(filepath.Join(runDir, filepath.FromSlash(baseRel))) &&
			fsx.FileExists(filepath.Join(runDir, filepath.FromSlash(legacyRel))) {
			baseRel = legacyRel
		}
		baseAbs := filepath.Join(runDir, filepath.FromSlash(baseRel))
		genBase := func() (RefResult, bool) {
			res := RefResult{Name: c.Name, Token: c.Token, Path: baseRel, Seed: c.RefSeed}
			qcRounds := 0
			if vision != nil && cfg.RefQCEnabled() {
				qcRounds = cfg.RefQC.MaxRounds
			}
			// 复用判定：文件在、非强制。复用帧不重新出图——这是全片一致性的基准，
			// 绝不能无意中换脸。
			reused := fsx.FileExists(baseAbs) && !force
			if !reused && !force {
				// 主文件缺失但审核存档还在（已通过核对的帧被误删/移动）：
				// 从审核存档恢复，而不是重新生成——重出就是换脸。
				if prev, ok := prevQC[c.Token+"\x00"]; ok &&
					(prev.QCStatus == "pass" || prev.QCStatus == "corrected") {
					if err := restoreApprovedRef(runDir, c.Token, baseAbs); err == nil {
						fmt.Printf("  ♻ 主锚点帧缺失，已从审核存档恢复 [%s] → %s\n", c.Name, baseRel)
						reused = true
					}
				}
			}
			if reused {
				// 历史质检结论与审核链路一并带回，防止索引合并时丢失
				if prev, ok := prevQC[c.Token+"\x00"]; ok {
					res.QCStatus = prev.QCStatus
					res.QCIssues = prev.QCIssues
					res.Prompt = prev.Prompt
					res.QCReview = prev.QCReview
				}
				// 已有完整核对结论（pass/corrected）或在未启用质检时直接复用：
				// 重查只会重复花钱。
				if qcRounds == 0 || res.QCStatus == "pass" || res.QCStatus == "corrected" {
					c.RefImage = baseRel
					return res, true
				}
				// 质检开启但既有帧从未完成过核对（上次运行被取消/质检调用失败/
				// 旧版生成）：对既有文件补一次核对。地基没查完就开工，错误会经
				// 图生图放大到每个出镜镜头——"没查过"绝不能表现为"通过"。
				// 既有链路接续下去（轮次继续编号），补查不清空历史轮次。
				fmt.Printf("  🔍 复用帧补查核对 [%s]（既有帧未完成过核对）\n", c.Name)
				res.QCStatus, res.QCIssues = "", nil
			}
			basePrompt := RefPromptFor(kit.Style, *c, frames[i][0]) // 人工覆盖优先，否则按圣经+风格编译
			res.Prompt = basePrompt
			var review []QCRound // 审核链路：每轮候选都进档，被否掉的也不丢
			if reused {
				review = append(review, res.QCReview...) // 接续既有历史（回溯档案/上次中断前已记录的轮次）
			}
			baseRound := 0 // 本轮链路的首个轮次编号（接续历史时从历史末轮之后继续）
			if n := len(review); n > 0 {
				baseRound = review[n-1].Round + 1
			}
			var fixHint string                         // 上一轮核对给出的修正指令（重出时追加）
			commit := func() { res.QCReview = review } // 每个返回点都带上已产生的链路
			// firstGen：首个需要出图的轮次。复用补查时第 0 轮用既有文件（不花钱），
			// 核对不通过再从第 1 轮开始换种重出。
			firstGen := 0
			if reused {
				firstGen = 1
				c.RefImage = baseRel
			}
			for round := 0; ; round++ {
				var model string
				if round < firstGen {
					// 复用候选：既有文件即本轮候选，只核对不出图
				} else {
					prompt := basePrompt
					if round > 0 {
						prompt = basePrompt + fixHint
					}
					url, model2, err := client.ImageGenEx("assets.ref_frame", prompt, ai.ImageOptions{
						Model:     cfg.Image.Model,
						Size:      cfg.Image.RefSize,
						Quality:   cfg.Image.Quality,
						Watermark: cfg.Image.Watermark,
					})
					if err != nil {
						review = append(review, QCRound{Round: baseRound + round, Model: model2, GenError: err.Error(), At: time.Now()})
						commit()
						// 重出失败：上一轮候选留在磁盘上（未被删除，Download 是原子改名），
						// 但它是一张被核对否掉的帧——严格模式下绝不允许带病继续
						// （"参考帧没眼镜、成片处处眼镜"正是要防的事故）。
						// 显式失败 + 保留候选：下次运行会重新核对这张候选，而不是重新抽卡。
						// 注意：重出只发生在核对未通过之后，所以"保留的候选"必然是未通过的。
						if round > 0 && fsx.FileExists(baseAbs) {
							res.QCStatus = "failed"
							res.QCIssues = append(res.QCIssues, "重出失败（候选未通过核对，仅保留供下次重新核对）: "+err.Error())
							res.Error = fmt.Sprintf("参考帧重出失败，候选未通过核对（%.1f 分）: %v",
								lastScore(review), err)
						} else {
							res.Error = err.Error()
						}
						if ai.IsFatal(err) {
							mu.Lock()
							fatalErr = err
							mu.Unlock()
							failed.Store(true) // 余额/鉴权：立即停，别继续烧钱
						}
						return res, false
					}
					if err := Download(url, baseAbs); err != nil {
						review = append(review, QCRound{Round: baseRound + round, Model: model2,
							GenError: "下载参考帧失败: " + err.Error(), At: time.Now()})
						commit()
						res.Error = "下载参考帧失败: " + err.Error()
						return res, false
					}
					ai.AttachCallArtifact(url, baseRel) // 调用记录回填本地路径（平台 URL 会过期）
					res.Seed = DeriveSeed("ref", baseRound+round, c.Token)
					c.RefSeed = res.Seed
					c.RefImage = baseRel
					model = model2
					if round == 0 {
						fmt.Printf("  🧑 角色参考帧已就绪 [%s] 模型=%s → %s\n", c.Name, model, baseRel)
					} else {
						fmt.Printf("  🧑 参考帧第 %d 轮重出完成 [%s] 模型=%s → %s\n", baseRound+round, c.Name, model, baseRel)
					}
				}

				// 候选图存档 + 进链路：下一轮重出会把主路径覆盖，先保住这一轮的图。
				// 归档名用轮次标签（接续历史时继续编号），避免覆盖历史轮次的图证。
				entry := QCRound{Round: baseRound + round, Model: model, Seed: res.Seed, At: time.Now()}
				if round < firstGen {
					entry.Reused = true
				}
				if rel, aerr := archiveRefCandidate(runDir, c.Token, baseRound+round, baseAbs); aerr == nil {
					entry.Image = rel
				} else {
					entry.Image = baseRel
					entry.ArchiveError = aerr.Error()
				}
				review = append(review, entry)
				cur := &review[len(review)-1]

				if qcRounds == 0 {
					cur.Final = true
					commit()
					return res, true
				}
				// 质检关卡：对照角色圣经逐项核对，不合格换种重出
				v, err := QCRefFrame(vision, baseAbs, *c)
				if err != nil {
					// 严格模式：质检已启用就必须查完——核对调用失败即失败，
					// 不允许"以为查过、其实没查"地继续
					cur.QCError = err.Error()
					commit()
					res.Error = "参考帧质检调用失败: " + err.Error()
					fmt.Printf("  ✗ 参考帧质检调用失败 [%s]: %v\n", c.Name, err)
					return res, false
				}
				cur.Verdict = v
				if v.Pass {
					cur.Final = true
					// 通过核对的帧另存一份审核存档：一致性基准是全片的地基，
					// 主路径被覆盖/误删时还能从这里恢复（真实事故：一张 8.0 分
					// 通过的帧在后续操作中消失，只能重新生成——那就是换脸）。
					if rel, aerr := archiveApprovedRef(runDir, c.Token, baseAbs); aerr == nil {
						cur.ApprovedCopy = rel
					} else {
						cur.ArchiveError = aerr.Error()
					}
					commit()
					res.QCStatus = "pass"
					switch {
					case round >= firstGen && round > 0:
						res.QCStatus = "corrected"
						fmt.Printf("  ✓ 参考帧重出后通过核对 [%s]（%.1f 分，共 %d 轮）\n", c.Name, v.Score, round+1)
					case reused:
						fmt.Printf("  ✓ 复用帧核对通过 [%s]（%.1f 分）\n", c.Name, v.Score)
					}
					return res, true
				}
				issues := append(append([]string{}, v.Missing...), v.Drift...)
				issues = append(issues, v.Issues...)
				if round >= firstGen+qcRounds {
					// 严格模式：重出轮数用尽仍不合格 → 失败，不保留"最佳候选"带病开工
					commit()
					res.QCStatus = "failed"
					res.QCIssues = issues
					res.Error = fmt.Sprintf("参考帧核对未通过（%.1f 分，重出 %d 轮仍不合格）: %s",
						v.Score, qcRounds, strings.Join(issues, "；"))
					fmt.Printf("  ✗ 参考帧核对未通过 [%s]（%.1f 分，重出 %d 轮仍不合格）: %s\n",
						c.Name, v.Score, qcRounds, strings.Join(issues, "；"))
					fmt.Printf("    建议：修正角色圣经后 studio render --run … --force-ref\n")
					return res, false
				}
				fmt.Printf("  ↻ 参考帧核对未通过 [%s]（%.1f 分）：%s → 换种重出\n",
					c.Name, v.Score, strings.Join(issues, "；"))
				cur.FixHintEN = qcCorrectionPrompt(v)
				commit()
				fixHint = cur.FixHintEN
			}
		}
		baseRes, baseReady := genBase()
		list := []RefResult{baseRes}

		// ---- 变体帧：以 base 为图生图输入派生；base 未就绪则跳过（没有锚点就没有变体）----
		if nVar := len(frames[i]) - 1; nVar > 0 {
			if !baseReady {
				fmt.Printf("  ⚠ %s: 主锚点帧未就绪，跳过 %d 张变体帧\n", c.Name, nVar)
			} else {
				genVariant := func(fr RefFrameSpec) RefResult {
					rel := filepath.ToSlash(filepath.Join("output", "refs",
						frameFileKey(c.Token)+"_"+safeName(fr.Variant)+".png"))
					// 老命名兼容（同上：既有文件不因改名而重出）
					if legacyRel := filepath.ToSlash(filepath.Join("output", "refs",
						safeName(c.Token)+"_"+safeName(fr.Variant)+".png")); legacyRel != rel &&
						!fsx.FileExists(filepath.Join(runDir, filepath.FromSlash(rel))) &&
						fsx.FileExists(filepath.Join(runDir, filepath.FromSlash(legacyRel))) {
						rel = legacyRel
					}
					abs := filepath.Join(runDir, filepath.FromSlash(rel))
					res := RefResult{Name: c.Name, Token: c.Token, Variant: fr.Variant, Purpose: fr.Purpose,
						Path: rel, Seed: DeriveSeed("ref-"+fr.Variant, 0, c.Token), Prompt: RefPromptFor(kit.Style, *c, fr)}
					if fsx.FileExists(abs) && !force {
						if prev, ok := prevQC[c.Token+"\x00"+fr.Variant]; ok {
							res.QCStatus = prev.QCStatus
							res.QCIssues = prev.QCIssues
						}
						return res
					}
					opt := ai.ImageOptions{
						Model:     cfg.Image.Model,
						Size:      cfg.Image.RefSize,
						Quality:   cfg.Image.Quality,
						Watermark: cfg.Image.Watermark,
					}
					// 图生图锁脸：变体的"同一张脸"由 base 参考图保证，纯文字描述做不到
					if ref, err := ReadAsDataURL(baseAbs); err == nil {
						opt.RefImage = ref
					} else {
						fmt.Printf("  ⚠ 变体帧无法读取主锚点（退化为纯文字生成）[%s·%s]: %v\n", c.Name, fr.Variant, err)
					}
					url, _, err := client.ImageGenEx("assets.ref_variant", res.Prompt, opt)
					if err != nil {
						res.Error = err.Error()
						if ai.IsFatal(err) {
							mu.Lock()
							fatalErr = err
							mu.Unlock()
							failed.Store(true)
						}
						return res
					}
					if err := Download(url, abs); err != nil {
						res.Error = "下载变体帧失败: " + err.Error()
						return res
					}
					ai.AttachCallArtifact(url, rel)
					fmt.Printf("  🧑 变体帧已就绪 [%s·%s] → %s\n", c.Name, fr.Variant, rel)
					return res
				}
				for _, fr := range frames[i][1:] {
					if failed.Load() {
						break
					}
					list = append(list, genVariant(fr))
				}
			}
		}
		resPer[i], done[i] = list, true
	})
	// 按角色表顺序收敛结果（与串行版一致）；致命错误时未派发的角色不进结果
	var out []RefResult
	var fails []string
	for i := range resPer {
		if done[i] {
			out = append(out, resPer[i]...)
		}
	}
	for _, r := range out {
		if r.Error != "" {
			label := r.Name
			if r.Variant != "" && r.Variant != "base" {
				label += "·" + r.Variant
			}
			fails = append(fails, label+": "+r.Error)
		}
	}
	if fatalErr != nil {
		persistReviewOnFailure(runDir, out)
		return out, fatalErr
	}
	// 严格模式：参考帧是全片一致性的根，任何一张生成/核对失败都拒绝继续——
	// 缺参考帧的镜头会静默退化成纯文生图（跨镜头换脸），这是最贵的错误
	if len(fails) > 0 {
		persistReviewOnFailure(runDir, out)
		return out, fmt.Errorf("参考帧未全部就绪，拒绝继续（%d 张失败）：\n  - %s",
			len(fails), strings.Join(fails, "\n  - "))
	}
	return out, nil
}

// persistReviewOnFailure 阶段失败前把已产生的审核链路落入素材索引。
//
// 图片与档案已在磁盘上，但索引不更新的话，"哪张被否、几轮中断、错误是什么"
// 在 Web 上就消失了（阶段失败后调用方不会保存索引）。断点续跑时还能据此
// 接续轮次编号、避免重复核对。只被本函数在失败路径调用——成功路径由调用方
// 统一合并保存（这里写会与调用方的保存意图重复）。
func persistReviewOnFailure(runDir string, out []RefResult) {
	if len(out) == 0 {
		return
	}
	ix := LoadAssetIndex(runDir)
	ix.MergeCharacters(out)
	_ = ix.Save(runDir)
}

// archiveRefCandidate 把一轮候选帧归档到 output/refs/qc/（审核链路的图证）。
//
// 主路径（output/refs/<token>.png）会被下一轮重出覆盖，且渲染管线只认它——
// 被否掉的候选必须另存一份，人工才能在 Web 上看清"哪张图没过、改成什么样"。
// 存档失败不中断生产（审核链路缺图 ≠ 参考帧不可用），错误记入链路条目。
func archiveRefCandidate(runDir, token string, round int, src string) (string, error) {
	rel := filepath.ToSlash(filepath.Join("output", "refs", "qc",
		fmt.Sprintf("%s_round%d.png", frameFileKey(token), round)))
	abs := filepath.Join(runDir, filepath.FromSlash(rel))
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	return rel, fsx.AtomicWrite(abs, data, 0o644)
}

// QCSummary 审核结论 + 链路轮数的一句话摘要（CLI/任务日志用；完整链路看 QCReview）。
func (r RefResult) QCSummary() string {
	status := r.QCStatus
	if status == "" {
		status = "未质检"
	}
	if len(r.QCReview) == 0 {
		return status
	}
	repairs := 0
	for _, row := range r.QCReview {
		if row.Round > 0 {
			repairs++
		}
	}
	if repairs > 0 {
		return fmt.Sprintf("%s，审核 %d 轮（重出 %d 次）", status, len(r.QCReview), repairs)
	}
	return fmt.Sprintf("%s，审核 %d 轮", status, len(r.QCReview))
}

// lastScore 链路里最近一轮的核对分数（0 = 没有结论）。
func lastScore(review []QCRound) float64 {
	for i := len(review) - 1; i >= 0; i-- {
		if v := review[i].Verdict; v != nil {
			return v.Score
		}
	}
	return 0
}

// archiveApprovedRef 把通过核对的帧另存为审核存档（覆盖式：永远保留最近一次通过的）。
func archiveApprovedRef(runDir, token, src string) (string, error) {
	rel := filepath.ToSlash(filepath.Join("output", "refs", "qc", frameFileKey(token)+"_approved.png"))
	abs := filepath.Join(runDir, filepath.FromSlash(rel))
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	return rel, fsx.AtomicWrite(abs, data, 0o644)
}

// restoreApprovedRef 主锚点帧缺失时从审核存档恢复（已通过核对的帧被误删/移动；
// 重新生成 = 换脸，绝不允许）。
func restoreApprovedRef(runDir, token, dst string) error {
	src := filepath.Join(runDir, "output", "refs", "qc", frameFileKey(token)+"_approved.png")
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return fsx.AtomicWrite(dst, data, 0o644)
}

// Download 原子下载：先写同目录临时文件再改名。
// 半截文件一旦被断点续跑当成"已完成"跳过，就会静默混进成片——这比显式失败昂贵得多。
// 临时名不用 path+".part"：文件名接近 NTFS 单分量上限（255 字符）时
// "目标名 + 后缀" 直接写不进去（真实事故：244 字符 token 的变体帧全部下载失败）。
func Download(url, path string) error {
	sum := sha1.Sum([]byte(filepath.Base(path)))
	tmp := filepath.Join(filepath.Dir(path), ".dl-"+hex.EncodeToString(sum[:6])+".part")
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

// frameFileKey 参考帧的文件名键：safeName(token)，超长时截断 + 内容哈希后缀。
//
// 为什么必须截断：NTFS 单文件名分量上限 255 字符，而角色一致性 token 可能很长
// （真实事故：244 字符的 token，基础帧 token.png 勉强写进，变体帧
// token_closeup.png 与质检候选存档全部写入失败，assets 阶段整体失败——
// 中文提示词生成的 token 是任意长度，不能依赖它"恰好够短"）。
// 截断是确定性的（同 token 同名字），哈希后缀保证不同 token 不会撞名；
// 老命名的既有文件由调用方做存在性兼容，绝不因改名重出（那就是换脸）。
func frameFileKey(token string) string {
	s := safeName(token)
	const maxLen = 48
	if len(s) <= maxLen {
		return s
	}
	sum := sha1.Sum([]byte(token))
	return s[:maxLen] + "-" + hex.EncodeToString(sum[:4]) // 48 + 1 + 8 = 57 字符
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
