// Package vision 实现"渲染后的视觉复核"：让模型真的看画面。
//
// 为什么必须有这一步：文本审片（只看 JSON）永远发现不了 AI 电影真正的破绽——
// 角色跨镜头换脸、多余手指、塑料皮肤、运动形变。这些只有看着画面才能判断。
// 技术质检（quality 包）负责确定性缺陷（糊/黑/纯色），本包负责**语义缺陷**：
// "这还是同一个人吗""画面像不像真实拍摄"。
//
// 成本可控设计：
//   - 抽帧缩到 640px 宽：每图 token 数有上限，远低于各家 1024 token/图的封顶
//   - 按镜头分组合并请求：一次请求送 3-4 个镜头的帧，减少往返
//   - MaxReviewShots 可限制复核镜头数（长片抽样）
package vision

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aistudio/internal/ai"
	"aistudio/internal/conc"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/promspec"
)

// Overlay 画面上"后来叠上去"的东西（水印/角标/文字/logo）。
//
// 为什么单独建模：平台水印是交付事故（成片带角标），但它**无法用信号处理判定**——
// 实测（合成对照：有角标 vs 无角标）右下角的帧间时间变化比值仅 0.77 vs 1.04，
// 而真实画面里一块静止的墙角/灯比值接近 0，两者在数值上不可分。只有"看懂画面"
// 才能区分"烧进去的角标"和"画面里本来就有的静止物体"，所以这条只能由视觉模型承担。
type Overlay struct {
	// Present 画面中是否存在叠字/角标/水印
	Present bool `json:"present"`
	// Text 叠字内容（如"AI生成"），看不清则留空
	Text string `json:"text,omitempty"`
	// Location 位置描述（如"右下角"），none 表示无
	Location string `json:"location,omitempty"`
	// Confidence 模型对该判断的信心（0-1）
	Confidence float64 `json:"confidence,omitempty"`
}

// ShotReview 单镜头复核结论。
type ShotReview struct {
	ShotID  string   `json:"shot_id"`
	Score   float64  `json:"score"`
	Pass    bool     `json:"pass"`
	Issues  []string `json:"issues,omitempty"`
	Fix     string   `json:"fix,omitempty"`
	Overlay *Overlay `json:"overlay,omitempty"`
	// 分维度打分（0-10；nil = 模型未给出该维度）。
	// 为什么拆维度：均分会把致命问题平均掉——身份 3 分 + 电影感 9 分的镜头
	// 均分 6 还是 6，但"换脸"是一票否决项，观众不会因为画面漂亮就原谅换了人。
	Identity  *float64 `json:"identity,omitempty"`
	Anatomy   *float64 `json:"anatomy,omitempty"`
	Cinematic *float64 `json:"cinematic,omitempty"`
	// Scene 环境一致性：背景与场次基准图（assets/scenes）是否同一个地点、
	// 同一套陈设与光线氛围。同场戏"换了屋子"和跨镜头换脸一样刺眼，
	// 但它逃过身份/解剖/电影感三项——那时画面本身可以又干净又好看。
	Scene *float64 `json:"scene,omitempty"`
	// Failed 该镜头的复核结论不可用（复核请求失败 / 模型漏答）。
	// "没查出来问题"与"没查成"必须分开：后者不计入均分、不进重渲清单，
	// 否则一次网络抖动就会生成一张误导性的付费重渲清单。
	Failed bool `json:"failed,omitempty"`
}

// Watermark 全片水印汇总结论。
type Watermark struct {
	// Checked 是否真的做过检查（未启用视觉复核时为 false——"没查"不等于"没有"）
	Checked bool `json:"checked"`
	// Suspected 是否有镜头被判定带叠字/角标
	Suspected bool `json:"suspected"`
	// Shots 被判定带叠字的镜头及其内容
	Shots []WatermarkHit `json:"shots,omitempty"`
	// Note 结论与后续动作
	Note string `json:"note,omitempty"`
}

// WatermarkHit 单个镜头的水印命中。
type WatermarkHit struct {
	ShotID     string  `json:"shot_id"`
	Text       string  `json:"text,omitempty"`
	Location   string  `json:"location,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// Report 整片视觉复核报告。
type Report struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Model       string       `json:"model"`
	Endpoint    string       `json:"endpoint"`
	FramesTotal int          `json:"frames_total"`
	Shots       []ShotReview `json:"shots"`
	Score       float64      `json:"score"`
	Pass        bool         `json:"pass"`
	Rerender    []string     `json:"rerender_shots,omitempty"`
	Feedback    string       `json:"feedback,omitempty"`
	Notes       []string     `json:"notes,omitempty"`
	Frames      []FrameRef   `json:"frames,omitempty"`
	// 分维度均分（仅统计有效结论；nil = 该维度没有任何模型给出过）。
	// 用于校准：如果 identity 均分显著低于其他维度，优先怀疑参考帧链路；
	// scene 垫底则优先怀疑场景基准图（assets/scenes）。
	IdentityAvg  *float64 `json:"identity_avg,omitempty"`
	AnatomyAvg   *float64 `json:"anatomy_avg,omitempty"`
	CinematicAvg *float64 `json:"cinematic_avg,omitempty"`
	SceneAvg     *float64 `json:"scene_avg,omitempty"`
	// Watermark 水印/叠字检测结论（由视觉模型逐镜头判定后汇总）
	Watermark Watermark `json:"watermark"`
}

// FrameRef 记录"哪个镜头的第几帧来自哪个时间点"，便于人工按报告核查。
type FrameRef struct {
	ShotID string  `json:"shot_id"`
	AtSec  float64 `json:"at_sec"`
	File   string  `json:"file"`
}

// response 视觉模型的 JSON 输出结构。
type response struct {
	Score float64 `json:"score"`
	Pass  bool    `json:"pass"`
	Shots []struct {
		ID     string   `json:"id"`
		Score  float64  `json:"score"`
		Pass   bool     `json:"pass"`
		Issues []string `json:"issues"`
		Fix    string   `json:"fix"`
		// 分维度分（旧模型可能不返回，指针容忍缺失）
		Identity  *float64 `json:"identity"`
		Anatomy   *float64 `json:"anatomy"`
		Cinematic *float64 `json:"cinematic"`
		Scene     *float64 `json:"scene"`
		// Overlay 叠字/角标判定（缺失时视为未检出，不发散、不误报）
		Overlay *Overlay `json:"overlay"`
	} `json:"shots"`
	Feedback string `json:"feedback"`
}

const systemPrompt = `你是电影后期质检师，负责判断 AI 生成的镜头是否可以进入成片。
你的判断标准是"观众能否看出这是 AI 生成的"以及"是否像真实拍摄的电影"。

对每个镜头逐项检查：
1. **身份一致性**：若请求中附有角色标准像，请把镜头帧中的人物与**对应角色的标准像**
   逐项比对（面部骨相、五官比例、发型、标志性特征）——不是"看起来像"，而是"就是同一个人"。
   没有附标准像时，跨镜头比对同一角色在不同镜头帧中的面部是否一致（跨镜头换脸是最严重的问题）
2. **解剖正确性**：手指数量与形态、肢体连接、牙齿、耳朵、眼睛对称性
3. **皮肤与质感**：是否塑料感/蜡像感/过度磨皮，是否有真实皮肤纹理
4. **运动与形变**：是否有面部或肢体形变、融化感、像素级抖动
5. **光影真实性**：光源是否有画面内来源，是否像真实拍摄的打光（无来源的均匀补光会暴露 AI）
6. **构图与镜头语言**：是否符合电影构图，是否出现裁头/裁手
7. **画面完整性**：是否有黑帧、卡帧、明显的重复纹理
8. **环境一致性**：若请求中附有场次的环境基准图，把镜头帧的背景与**所属场次**的基准图
   比对——地点特征、陈设布置、光线方向与氛围是否一致；对不上就是场景漂移。没有基准图时，
   跨镜头比对同一场次的背景是否漂移（同一场戏里"换了屋子"是最刺眼的 AI 痕迹之一）
9. **叠字与角标（水印）**：画面四角与下缘有没有"后来叠上去"的文字、logo、半透明条块
   （例如"AI生成""图片由AI生成"、厂商 logo）。判据是**它是否像叠加的图形**——
   边缘锐利、贴边固定、文字清晰可读；而画面里本来就有的静止物体（墙、灯、窗外景色）
   不算叠字。这一项必须逐镜头明确回答，不能省略或含糊。

评分：每个镜头给总分 0-10，以及四个**分维度分**（同样 0-10）：
- identity 身份一致性：人物与标准像（或跨镜头）是否"就是同一个人"
- anatomy 解剖与形变：手/肢体/牙齿/面部是否正确无畸变
- cinematic 真实感与电影感：皮肤质感、光影动机、构图、是否像真实拍摄
- scene 环境一致性：背景与场次基准图（或同场次其他镜头）是否同一个地点、同一套陈设与光线氛围
任一维度不及格，总分再高也不能 pass——观众不会因为画面漂亮就原谅换了人，也不会因为换了人没被看出来就原谅换了屋子。
只输出 JSON，不要任何解释文字。`

const schemaHint = `输出格式：
{"score":8.5,"pass":true,
 "shots":[{"id":"sh001","score":8.6,"pass":true,"identity":8.8,"anatomy":9.0,"cinematic":8.2,"scene":8.5,"issues":[],"fix":"",
           "overlay":{"present":false,"text":"","location":"none","confidence":0.9}},
          {"id":"sh002","score":5.2,"pass":false,"identity":3.5,"anatomy":4.0,"cinematic":6.8,"scene":6.0,"issues":["右手出现六根手指","面部与参考角色不一致"],"fix":"换种子重新生成该镜头，避免手部入画","overlay":{"present":true,"text":"AI生成","location":"右下角","confidence":0.95}}],
 "feedback":"整体评价与最需要优先处理的问题"}

identity/anatomy/cinematic/scene 四个分维度分逐镜头必填（0-10）。
overlay 逐镜头必填：present(是否有叠字/角标/水印)、text(内容，看不清留空)、
location(位置，如"右下角"；没有填 "none")、confidence(0-1 信心)`

// watermarkConfidenceLine 判定"确认存在角标"的信心门槛。
//
// 取 0.7 而非更低：模型偶尔会把画面里本就存在的招牌/店招/窗贴误认成角标，
// 而这条结论会写进交付报告并提示复核。宁可漏报让人肉眼复核，也不要刷屏式误报。
const watermarkConfidenceLine = 0.7

// Options 复核选项。
type Options struct {
	RunDir string
	// Client 必须是已配置为视觉能力的客户端（见 ai.Client.For(ai.CapVision, ...)）
	Client *ai.Client
}

// Run 执行视觉复核。
func Run(cfg *config.Config, opt Options) (*Report, error) {
	if opt.Client == nil {
		return nil, fmt.Errorf("视觉复核需要视觉客户端")
	}
	if !cfg.VisionReview.Enabled {
		return nil, fmt.Errorf("视觉复核未启用（config.vision_review.enabled=false）")
	}
	kit, err := loadKit(opt.RunDir)
	if err != nil {
		return nil, err
	}
	man, err := loadManifest(opt.RunDir)
	if err != nil {
		return nil, err
	}
	// 角色标准像映射：把出镜角色的参考帧与镜头帧一起送检。
	// "是不是同一个人"由此从跨镜头印象变成与确定性基准的比对——
	// 参考帧是全片一致性的锚，复核时看不到它等于让质检师凭记忆认人。
	refOf := loadCharRefs(opt.RunDir)
	// 场景基准图映射（逐镜头，含环境状态）：单靠角色标准像只能查"人没换"，
	// 查不出"屋子换了"。
	baseOf := loadShotSceneBaselines(opt.RunDir)

	// 只复核已生成的镜头；缺失镜头由交付门禁负责，不在这里重复报错
	type item struct {
		shot  promspec.ShotSpec
		video string
		dur   float64
	}
	var items []item
	for _, s := range kit.ShotList {
		path, ok := man.videoOf(s.ID)
		if !ok {
			continue
		}
		items = append(items, item{shot: s, video: path, dur: man.durationOf(s.ID)})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("没有可复核的镜头片段（先运行 studio render）")
	}
	if cfg.VisionReview.MaxReviewShots > 0 && len(items) > cfg.VisionReview.MaxReviewShots {
		// 抽样：优先复核"有角色出镜"的镜头——一致性问题是视觉复核的核心价值
		sort.SliceStable(items, func(i, j int) bool {
			return len(items[i].shot.Characters) > len(items[j].shot.Characters)
		})
		items = items[:cfg.VisionReview.MaxReviewShots]
		fmt.Printf("  · 视觉复核抽样 %d 个镜头（优先含角色的镜头）\n", len(items))
	}

	// 抽帧
	frameDir := filepath.Join(opt.RunDir, "output", "review_frames")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return nil, err
	}
	rep := &Report{
		GeneratedAt: time.Now(),
		Model:       opt.Client.ModelName(),
		Endpoint:    opt.Client.EndpointInfo(),
	}
	// 抽帧并行：每个镜头的抽帧是独立的 ffmpeg 子进程调用。
	// 结果与失败记录仍按分镜顺序收敛，报告内容与串行版一致。
	framesList := make([][]string, len(items))
	extractNotes := make([]string, len(items))
	conc.ForEachIdx(len(items), frameWorkers, func(i int) {
		it := items[i]
		fs, err := ExtractFrames(it.video, frameDir, it.shot.ID, it.dur,
			cfg.VisionReview.SampleFrames, cfg.VisionReview.FrameWidth)
		if err != nil {
			extractNotes[i] = fmt.Sprintf("镜头 %s 抽帧失败（已跳过复核）: %v", it.shot.ID, err)
			return
		}
		framesList[i] = fs
	})
	framesOf := map[string][]string{}
	var extractFails []string
	for i, it := range items {
		if extractNotes[i] != "" {
			rep.Notes = append(rep.Notes, extractNotes[i])
			extractFails = append(extractFails, it.shot.ID)
			continue
		}
		framesOf[it.shot.ID] = framesList[i]
		rep.FramesTotal += len(framesList[i])
		for _, f := range framesList[i] {
			rep.Frames = append(rep.Frames, FrameRef{ShotID: it.shot.ID, File: rel(opt.RunDir, f)})
		}
	}
	if rep.FramesTotal == 0 {
		return nil, fmt.Errorf("未能从任何镜头抽取到帧")
	}
	// 严格模式：抽帧失败 = 该镜头根本没被复核——不允许"跳过"后继续交付
	if len(extractFails) > 0 {
		return rep, fmt.Errorf("有 %d 个镜头抽帧失败，复核无法完成（不允许跳过）: %s",
			len(extractFails), strings.Join(extractFails, ","))
	}

	// 分批送检：一次请求包含多个镜头，减少往返。
	// 批次彼此独立（各自的请求、各自的帧），按 vision_review.workers 有界并行；
	// 合并阶段仍按批次顺序进行，保证报告/Notes/Feedback 与串行版逐字一致。
	batch := cfg.VisionReview.MaxShotsPerRequest
	if batch <= 0 {
		batch = 4
	}
	shotOf := map[string]promspec.ShotSpec{}
	for _, it := range items {
		shotOf[it.shot.ID] = it.shot
	}
	var order []string
	for _, it := range items {
		if len(framesOf[it.shot.ID]) > 0 {
			order = append(order, it.shot.ID)
		}
	}
	var groups [][]string
	for i := 0; i < len(order); i += batch {
		end := i + batch
		if end > len(order) {
			end = len(order)
		}
		groups = append(groups, order[i:end])
	}
	type batchOutcome struct {
		resp response
		err  error
	}
	outcomes := make([]batchOutcome, len(groups))
	batchWorkers := cfg.VisionReview.Workers
	if batchWorkers <= 0 {
		batchWorkers = 2
	}
	conc.ForEachIdx(len(groups), batchWorkers, func(bi int) {
		group := groups[bi]
		// 图序：角色标准像 → 场次环境基准图 → 镜头帧。
		// buildPrompt 的图序说明必须与这里逐张对应，错位会让模型拿错基准判分。
		refs := append(batchRefs(shotOf, group, refOf), batchSceneRefs(shotOf, group, baseOf)...)
		// 标准像在前、镜头帧在后：顺序与 buildPrompt 里的图序说明严格对应
		var images []ai.ImageRef
		for _, r := range refs {
			images = append(images, ai.ImageRef{Path: r.Path})
		}
		for _, id := range group {
			for _, f := range framesOf[id] {
				images = append(images, ai.ImageRef{Path: f})
			}
		}
		prompt := buildPrompt(shotOf, group, framesOf, refs, cfg.VisionReview.PassScore)
		var resp response
		err := opt.Client.VisionJSONEx(systemPrompt, prompt, images, "original", &resp)
		outcomes[bi] = batchOutcome{resp: resp, err: err}
	})
	if len(refOf) > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("已附 %d 张角色标准像作为身份基准（与镜头帧逐人比对）", len(refOf)))
	} else {
		for _, it := range items {
			if len(it.shot.Characters) > 0 {
				rep.Notes = append(rep.Notes,
					"角色标准像不可用（缺 output/shotplan.json 或参考帧缺失）：身份复核退化为跨镜头对比")
				break
			}
		}
	}
	if len(baseOf) > 0 {
		distinct := map[string]bool{}
		for _, b := range baseOf {
			distinct[b.Path] = true
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"已附 %d 张场景基准图作为环境基准（%d 个镜头逐镜绑定，与镜头帧比对地点/陈设/光线氛围）",
			len(distinct), len(baseOf)))
	} else {
		for _, it := range items {
			if it.shot.SceneID != "" {
				rep.Notes = append(rep.Notes,
					"场景基准图不可用（未生成素材库）：环境复核退化为跨镜头对比；在「素材库」生成场景图后重跑复核更可靠")
				break
			}
		}
	}
	failedBatches := 0
	var failedShots []string
	for bi, oc := range outcomes {
		group := groups[bi]
		if oc.err != nil {
			// 单批失败：记录"不可用"结论（不进均分、不进重渲清单），
			// 但严格模式下这不是"可以继续"的理由——最后统一报错。
			failedBatches++
			rep.Notes = append(rep.Notes, fmt.Sprintf("复核批次 %v 失败: %v", group, oc.err))
			for _, id := range group {
				failedShots = append(failedShots, id)
				rep.Shots = append(rep.Shots, ShotReview{
					ShotID: id, Pass: false, Failed: true,
					Issues: []string{"该镜头的复核请求失败: " + oc.err.Error()},
					Fix:    "修复视觉端点后重跑，或人工查看该镜头",
				})
			}
			continue
		}
		mergeResponse(rep, oc.resp, group, cfg.VisionReview.PassScore)
	}

	finalize(rep, cfg, failedBatches)
	// 严格模式：复核请求失败 → 直接失败。"没查"绝不能表现为"查过"，
	// 更不允许带着"未复核"的画面进入交付（确需跳过请显式 --no-visual）。
	if failedBatches > 0 {
		return rep, fmt.Errorf("视觉复核有 %d/%d 个批次请求失败（涉及镜头 %s）：复核未完成，拒绝继续",
			failedBatches, len(groups), strings.Join(failedShots, ","))
	}
	return rep, nil
}

// frameWorkers 抽帧并发数。抽帧是本地 ffmpeg 调用（每帧约百毫秒级），
// 不受接口限流约束，4 路已足够让抽帧阶段不再成为复核的等待大头。
const frameWorkers = 4

// refLabel 一张随请求附送的基准图：角色标准像（Kind 为空）或场次环境基准图（Kind=scene）。
type refLabel struct {
	Name string
	Path string
	// Kind 基准类型："scene" = 场次环境基准图（establishing）；其余（空）= 角色标准像。
	Kind string
	// State 环境状态变体 slug（仅 Kind=scene 时有意义；空 = 场次主基准）。
	// 提示词里要与镜头块的"环境状态"对上，模型才知道哪张基准对应哪些镜头。
	State string
}

// batchRefs 收集本批次出镜角色的标准像（按首次出现排序、去重）。
func batchRefs(shotOf map[string]promspec.ShotSpec, group []string, refOf map[string]string) []refLabel {
	seen := map[string]bool{}
	var out []refLabel
	for _, id := range group {
		for _, name := range shotOf[id].Characters {
			p, ok := refOf[name]
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, refLabel{Name: name, Path: p})
		}
	}
	return out
}

// batchSceneRefs 收集本批次镜头实际使用的环境基准图（按图片路径去重、按批次内首次出现排序）。
// 角色标准像解决"是不是同一个人"，它解决"是不是同一个地方"——两者缺一，
// 复核就只剩一半的一致性判断。同场次不同状态的镜头会带不同的基准图（路径去重，
// 状态不同的两张都要附），同状态多镜头只附一次。
func batchSceneRefs(shotOf map[string]promspec.ShotSpec, group []string, baseOf map[string]shotBaseline) []refLabel {
	seen := map[string]bool{}
	var out []refLabel
	for _, id := range group {
		b, ok := baseOf[id]
		if !ok || seen[b.Path] {
			continue
		}
		seen[b.Path] = true
		out = append(out, refLabel{Name: shotOf[id].SceneID, Path: b.Path, Kind: "scene", State: b.State})
	}
	return out
}

// shotPlanDoc 只取视觉复核需要的字段（shotplan.json 由渲染阶段落盘，
// characters[].ref_image 与 shots[].scene_ref_image 都在渲染期回填）。
type shotPlanDoc struct {
	Characters []struct {
		Name     string `json:"name"`
		RefImage string `json:"ref_image"`
	} `json:"characters"`
	Shots []struct {
		ID            string `json:"shot_id"`
		SceneRefImage string `json:"scene_ref_image"`
		SceneRefState string `json:"scene_ref_state"`
	} `json:"shots"`
}

// loadCharRefs 从 shotplan.json 建立角色名 → 标准像绝对路径的映射。
// 旧 run / 缺产物时返回空映射（身份复核退化为跨镜头对比，不报错）。
func loadCharRefs(runDir string) map[string]string {
	var doc shotPlanDoc
	if err := readJSON(filepath.Join(runDir, "output", "shotplan.json"), &doc); err != nil {
		return nil
	}
	out := map[string]string{}
	for _, c := range doc.Characters {
		if c.Name == "" || c.RefImage == "" {
			continue
		}
		p := filepath.Join(runDir, filepath.FromSlash(c.RefImage))
		if _, err := os.Stat(p); err == nil {
			out[c.Name] = p
		}
	}
	return out
}

// shotBaseline 一个镜头生成时实际使用的环境基准图。
type shotBaseline struct {
	Path  string
	State string // 环境状态变体 slug（空 = 场次主基准）
}

// loadShotSceneBaselines 镜头 → 环境基准图绝对路径（渲染期回填进 shotplan.json）。
//
// 与出图端消费的是同一批文件、同一份逐镜头绑定——生成时拿哪张当环境锚，
// 复核时就必须拿哪张当比对基准。从素材索引重新推导"这一镜该用哪张"是另一套
// 逻辑（状态变体怎么绑），两边会各挑各的，复核就开始在错的基准上判分。
// 旧 run（无该字段）/ 未生成素材时返回空映射（环境复核退化为跨镜头对比，不报错）。
func loadShotSceneBaselines(runDir string) map[string]shotBaseline {
	var doc shotPlanDoc
	if err := readJSON(filepath.Join(runDir, "output", "shotplan.json"), &doc); err != nil {
		return nil
	}
	out := map[string]shotBaseline{}
	for _, s := range doc.Shots {
		if s.ID == "" || s.SceneRefImage == "" {
			continue
		}
		p := filepath.Join(runDir, filepath.FromSlash(s.SceneRefImage))
		if _, err := os.Stat(p); err == nil {
			out[s.ID] = shotBaseline{Path: p, State: s.SceneRefState}
		}
	}
	return out
}

// refNamesOfKind 按类型挑出基准图名（Kind="" = 角色标准像；"scene" = 场次环境基准）。
// buildPrompt 据此决定写不写对应的比对段：没有该类基准却要求模型比对，
// 只会让它对着镜头帧自己编一个基准出来。
func refNamesOfKind(refs []refLabel, kind string) []string {
	var out []string
	for _, r := range refs {
		if r.Kind == kind {
			out = append(out, r.Name)
		}
	}
	return out
}

// buildPrompt 构造送检提示词：把每个镜头的身份/场次信息与画面要求一并给出，
// 否则模型无从判断"这是不是同一个人、是不是同一个地方、动作对不对"。
// refs 非空时先声明基准图的图序——模型看到的图片列表是基准图在前、帧在后。
func buildPrompt(shotOf map[string]promspec.ShotSpec, group []string, framesOf map[string][]string, refs []refLabel, passScore float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "以下是从成片中按镜头顺序抽取的画面帧。请逐个镜头复核。达标线 %.1f 分。\n\n", passScore)
	if len(refs) > 0 {
		b.WriteString("本次请求同时附上了**基准图**（比对基准），图片顺序如下：\n")
		for i, r := range refs {
			if r.Kind == "scene" {
				if r.State != "" {
					fmt.Fprintf(&b, "  第 %d 张图 = 场次「%s」的环境基准图（状态：%s；establishing 图：地点/陈设/光线氛围）\n", i+1, r.Name, r.State)
				} else {
					fmt.Fprintf(&b, "  第 %d 张图 = 场次「%s」的环境基准图（establishing 图：地点/陈设/光线氛围）\n", i+1, r.Name)
				}
			} else {
				fmt.Fprintf(&b, "  第 %d 张图 = 角色「%s」的标准像\n", i+1, r.Name)
			}
		}
		b.WriteString("基准图之后的图片才是镜头帧。\n")
		if len(refNamesOfKind(refs, "")) > 0 {
			b.WriteString("身份：把镜头帧中出现的人物与**该镜头出场角色**的标准像逐项比对")
			b.WriteString("（骨相、五官比例、发型、标志性特征），判断是否为同一人。\n")
		}
		if len(refNamesOfKind(refs, "scene")) > 0 {
			b.WriteString("环境：把镜头帧的背景与**所属场次**的环境基准图比对")
			b.WriteString("（地点特征、陈设布置、光线方向与氛围是否一致；对不上即场景漂移）。\n")
		}
		b.WriteString("\n")
	}
	for _, id := range group {
		s := shotOf[id]
		fmt.Fprintf(&b, "【镜头 %s】共 %d 帧\n", id, len(framesOf[id]))
		if s.Description != "" {
			fmt.Fprintf(&b, "  画面要求：%s\n", s.Description)
		}
		if s.SceneID != "" {
			if st := strings.TrimSpace(s.SceneState); st != "" {
				fmt.Fprintf(&b, "  所属场景：%s（环境状态：%s）\n", s.SceneID, st)
			} else {
				fmt.Fprintf(&b, "  所属场景：%s\n", s.SceneID)
			}
		}
		if len(s.Characters) > 0 {
			fmt.Fprintf(&b, "  出场角色：%s\n", strings.Join(s.Characters, "、"))
		}
		fmt.Fprintf(&b, "  景别/运镜：%s / %s\n", s.ShotSize, s.CameraMove)
		for _, bt := range s.DialogueBeats() {
			fmt.Fprintf(&b, "  台词（%s）：%s\n", bt.Speaker, bt.Text)
		}
		b.WriteString("\n")
	}
	b.WriteString(schemaHint)
	return b.String()
}

func mergeResponse(rep *Report, resp response, group []string, passScore float64) {
	got := map[string]ShotReview{}
	for _, s := range resp.Shots {
		if s.ID == "" {
			continue
		}
		// 分维度一票否决：任一给出的维度低于达标线，总分再高也不算过——
		// 均分会把"身份 3 分 + 电影感 9 分"平均成 6 分的灰色地带，而换脸不可原谅；
		// 场景漂移同理（"又一个漂亮的、但不是同一个屋子的镜头"不能进成片）。
		pass := s.Pass && s.Score >= passScore
		for _, d := range []*float64{s.Identity, s.Anatomy, s.Cinematic, s.Scene} {
			if d != nil && *d < passScore {
				pass = false
			}
		}
		got[s.ID] = ShotReview{ShotID: s.ID, Score: s.Score, Pass: pass,
			Issues: s.Issues, Fix: s.Fix, Overlay: s.Overlay,
			Identity: s.Identity, Anatomy: s.Anatomy, Cinematic: s.Cinematic, Scene: s.Scene,
		}
	}
	for _, id := range group {
		if r, ok := got[id]; ok {
			rep.Shots = append(rep.Shots, r)
			continue
		}
		// 模型漏答该镜头：标记为"结论不可用"而不是"通过"——漏答不等于达标，
		// 但也不等于不合格，不进均分、不进重渲清单
		rep.Shots = append(rep.Shots, ShotReview{
			ShotID: id, Score: 0, Pass: false, Failed: true,
			Issues: []string{"视觉模型未返回该镜头的复核结论"},
			Fix:    "重新运行复核，或人工查看该镜头",
		})
	}
	if resp.Feedback != "" {
		if rep.Feedback != "" {
			rep.Feedback += " "
		}
		rep.Feedback += resp.Feedback
	}
}

func finalize(rep *Report, cfg *config.Config, failedBatches int) {
	// 按分镜顺序排列，便于人工按序核查
	sort.SliceStable(rep.Shots, func(i, j int) bool { return rep.Shots[i].ShotID < rep.Shots[j].ShotID })
	summarizeWatermark(rep)
	var sum float64
	var reviewed int
	pass := true
	var idSum, anSum, ciSum, scSum float64
	var idN, anN, ciN, scN int
	for _, s := range rep.Shots {
		if s.Failed {
			// 复核请求失败/漏答：结论不可用——不计分、不进重渲清单
			continue
		}
		reviewed++
		sum += s.Score
		if s.Identity != nil {
			idSum += *s.Identity
			idN++
		}
		if s.Anatomy != nil {
			anSum += *s.Anatomy
			anN++
		}
		if s.Cinematic != nil {
			ciSum += *s.Cinematic
			ciN++
		}
		if s.Scene != nil {
			scSum += *s.Scene
			scN++
		}
		if !s.Pass {
			pass = false
			rep.Rerender = append(rep.Rerender, s.ShotID)
		}
	}
	if reviewed == 0 {
		rep.Pass = false
		rep.Notes = append(rep.Notes, "没有任何镜头完成复核")
		return
	}
	rep.Score = sum / float64(reviewed)
	rep.IdentityAvg, rep.AnatomyAvg, rep.CinematicAvg = avgPtr(idSum, idN), avgPtr(anSum, anN), avgPtr(ciSum, ciN)
	rep.SceneAvg = avgPtr(scSum, scN)
	// 校准线索：身份维度显著垫底时，问题多半不在镜头而在参考帧链路——
	// 提示去查参考帧而不是盲目全片重渲
	if idN > 0 && ciN > 0 {
		if avgPtr(idSum, idN) != nil && avgPtr(ciSum, ciN) != nil &&
			*rep.IdentityAvg+1.5 < *rep.CinematicAvg {
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"身份一致性均分（%.1f）显著低于电影感均分（%.1f）：优先核对角色参考帧（output/refs/）与角色圣经是否一致，必要时 --force-ref 重出参考帧，而不是逐镜头重渲",
				*rep.IdentityAvg, *rep.CinematicAvg))
		}
	}
	// 环境维度垫底同理：先查场景基准图（assets/scenes）有没有、对不对，
	// 再谈逐镜头重渲——环境漂移是"基准缺失/选错"的典型症状
	if scN > 0 && ciN > 0 && rep.SceneAvg != nil && rep.CinematicAvg != nil &&
		*rep.SceneAvg+1.5 < *rep.CinematicAvg {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"环境一致性均分（%.1f）显著低于电影感均分（%.1f）：优先核对场景基准图（output/assets/scenes/）是否覆盖对应场次（素材库 → 场景图），补齐后定点重渲相关镜头，而不是逐镜头调提示词",
			*rep.SceneAvg, *rep.CinematicAvg))
	}
	// 部分批次失败只降低结论可信度，不等于"画面不合格"——
	// 两者必须分开表达，否则一次网络抖动就会被当成"片子不行"。
	if failedBatches > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"有 %d 个复核批次请求失败，该部分镜头的结论不可用（不等于画面不合格）", failedBatches))
	}
	rep.Pass = pass && failedBatches == 0
}

func avgPtr(sum float64, n int) *float64 {
	if n == 0 {
		return nil
	}
	v := sum / float64(n)
	return &v
}

// ExtractFrames 均匀抽帧并缩放到指定宽度（takes 对比与外部评分共用）。
// 避开首尾各 8%：视频模型的首尾常有过渡伪影，抽到会误判。
func ExtractFrames(video, dir, shotID string, dur float64, n, width int) ([]string, error) {
	if n <= 0 {
		n = 3
	}
	if width <= 0 {
		width = 640
	}
	if dur <= 0 {
		var err error
		if dur, err = probeDuration(video); err != nil {
			return nil, err
		}
	}
	var out []string
	times := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		t := dur * (0.08 + 0.84*float64(i)/float64(maxInt(1, n-1)))
		times = append(times, t)
	}
	for i, t := range times {
		fp := filepath.Join(dir, fmt.Sprintf("%s_%02d.jpg", shotID, i+1))
		// 用 jpg 而不是 png：体积小、各家视觉接口都支持，省请求体与流量
		if _, err := ffx.Run(60*time.Second, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%.3f", t), "-i", video, "-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:-2", width), "-q:v", "3", fp); err != nil {
			continue
		}
		if st, err := os.Stat(fp); err != nil || st.Size() == 0 {
			continue
		}
		out = append(out, fp)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("抽帧全部失败")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 载入
// ---------------------------------------------------------------------------

type manifest struct {
	Shots []struct {
		ID     string  `json:"id"`
		Status string  `json:"status"`
		Video  string  `json:"video"`
		Actual float64 `json:"actual_duration_sec"`
	} `json:"shots"`
}

func (m *manifest) videoOf(id string) (string, bool) {
	for _, s := range m.Shots {
		if s.ID == id && s.Status == "done" && s.Video != "" {
			return s.Video, true
		}
	}
	return "", false
}

func (m *manifest) durationOf(id string) float64 {
	for _, s := range m.Shots {
		if s.ID == id {
			return s.Actual
		}
	}
	return 0
}

func loadKit(runDir string) (*promspec.Kit, error) {
	var kit promspec.Kit
	if err := readJSON(filepath.Join(runDir, "artifacts", "shotspec.json"), &kit); err != nil {
		return nil, fmt.Errorf("读取生成规格失败: %w", err)
	}
	if len(kit.ShotList) == 0 {
		return nil, fmt.Errorf("生成规格中没有镜头")
	}
	return &kit, nil
}

func loadManifest(runDir string) (*manifest, error) {
	var m manifest
	if err := readJSON(filepath.Join(runDir, "output", "render_manifest.json"), &m); err != nil {
		return nil, fmt.Errorf("读取渲染清单失败（需先运行 studio render）: %w", err)
	}
	// 清单里的 video 是相对 run 目录的路径，这里转成绝对路径
	for i := range m.Shots {
		if m.Shots[i].Video != "" && !filepath.IsAbs(m.Shots[i].Video) {
			m.Shots[i].Video = filepath.Join(runDir, filepath.FromSlash(m.Shots[i].Video))
		}
	}
	return &m, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func probeDuration(p string) (float64, error) {
	pr, err := ffx.ProbeFile(p)
	if err != nil {
		return 0, err
	}
	return pr.DurationSec, nil
}

func rel(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// summarizeWatermark 汇总逐镜头的水印/叠字判定。
//
// 为什么必须存在这一步：平台水印一旦发生（换了模型、服务端改了默认值、账号侧声明过期），
// 成片会**每一帧**都带着角标，这是不可交付的硬伤；但它不会让任何技术指标变差，
// 所以常规质检永远发现不了。判定只能靠"看懂画面"，因此结论来自视觉模型，
// 并且只作为**告警级**结论呈现——不阻断交付（画面语义是模型的主观判断），
// 但会明确写进交付报告并给出复核动作。
func summarizeWatermark(rep *Report) {
	rep.Watermark.Checked = len(rep.Shots) > 0
	var hits []WatermarkHit
	for _, s := range rep.Shots {
		if s.Overlay == nil || !s.Overlay.Present {
			continue
		}
		if s.Overlay.Confidence < watermarkConfidenceLine {
			continue // 低信心判定不进结论：避免把画面里的招牌/窗贴当成水印
		}
		hits = append(hits, WatermarkHit{
			ShotID:     s.ShotID,
			Text:       s.Overlay.Text,
			Location:   s.Overlay.Location,
			Confidence: s.Overlay.Confidence,
		})
	}
	if len(hits) == 0 {
		rep.Watermark.Note = "未检出水印/叠字"
		return
	}
	rep.Watermark.Suspected = true
	rep.Watermark.Shots = hits
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ShotID)
	}
	rep.Watermark.Note = fmt.Sprintf(
		"%d 个镜头疑似带叠字/角标（%s）。建议：① 抽帧人工确认（帧已存 output/review_frames/）；"+
			"② 确认账号已在智谱控制台签署去水印免责声明，且出图/视频请求确实带了 watermark_enabled=false；"+
			"③ 确认后就这些镜头执行 studio render --run <run> --shots %s 重渲。",
		len(hits), strings.Join(ids, ","), strings.Join(ids, ","))
	rep.Notes = append(rep.Notes, "⚠ "+rep.Watermark.Note)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
