// Package config 集中管理生产级参数：模型分层、媒体计费、预算上限、生成与质检阈值。
//
// 设计原则：所有"魔法数字"（重渲次数、质检阈值、字幕 CPS、响度目标、单价）都必须
// 可在 config.json 覆盖并在启动时打印生效值——生产环境出问题时，第一件事是确认
// 运行时到底用了哪些参数，而不是去翻源码。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ModelTier 生成档位：不同环节用不同模型，避免"用最贵的模型做翻译"。
type ModelTier struct {
	Texture   string `json:"texture"`   // 出图/视频：画质优先
	Economy   string `json:"economy"`   // 出图/视频：成本优先
	Vision    string `json:"vision"`    // 视觉复核
	Text      string `json:"text"`      // 文本创作与结构化产出
	Translate string `json:"translate"` // 字幕翻译（低成本即可）
	// TTS 配音模型。语音自然度主要由模型决定：glm-tts（新一代语音大模型，
	// 能按上下文预判情绪与语调）明显优于初代 cogtts，因此默认 glm-tts。
	TTS string `json:"tts"`
}

// Price 计费单价：用于渲染前成本预估与运行中预算熔断。
// LLM 按 1K token 计价，媒体按次/秒/字计价，单位统一为人民币元。
type Price struct {
	PromptPer1K     float64 `json:"prompt_per_1k_tokens"`
	CompletionPer1K float64 `json:"completion_per_1k_tokens"`
	ImagePerShot    float64 `json:"image_per_shot"`
	VideoPerSec     float64 `json:"video_per_sec"`
	TTSPer1KChars   float64 `json:"tts_per_1k_chars"`
	MusicPerCall    float64 `json:"music_per_call"`
}

// Budget 付费资源管控：生产环境的硬门槛，避免批量渲染把余额烧穿。
type Budget struct {
	MaxCostCNY   float64 `json:"max_cost_cny"`    // 0 = 不限制；超限立即停止并保留断点
	WarnRatio    float64 `json:"warn_ratio"`      // 用量达到该比例时告警
	StopOnNoFund bool    `json:"stop_on_no_fund"` // 余额类错误熔断整批（而非逐镜头重试）
}

// VideoGen 视频生成参数。
type VideoGen struct {
	Model          string `json:"model"`           // 默认档位模型
	QualityModel   string `json:"quality_model"`   // --quality quality 时使用的模型
	EconomyModel   string `json:"economy_model"`   // --quality speed 时使用的模型
	DefaultQuality string `json:"default_quality"` // speed | quality
	Size           string `json:"size"`            // 生成分辨率（智谱：16 的整数倍；火山方舟：由它推导 resolution 档位与画幅）
	FPS            int    `json:"fps"`
	Workers        int    `json:"workers"`
	TimeoutMinutes int    `json:"timeout_minutes"`
	ShotRetries    int    `json:"shot_retries"`    // 单镜头生成重试次数
	RerenderRounds int    `json:"rerender_rounds"` // 质检不过 → 换种子重渲的最大轮数
	WithAudio      bool   `json:"with_audio"`      // 交给模型带音频（我们自己做后期，默认关）
	// ImageToImage：出图接口是否支持参考图（角色一致性关键能力）；不支持时自动降级
	ImageToImage bool   `json:"image_to_image"`
	RefSize      string `json:"ref_size"` // 角色参考图分辨率
	// AllowLongTakes 计划时长超过 5.5s 的镜头自动请求 10s 档。
	// CogVideoX-3 官方支持 duration=5/10；不开的话所有镜头都会被服务端压成 ~5s，
	// 8s 长镜头只能靠后期拉伸（慢速/定帧）凑时长，观感明显变差。
	// 10s 档计费约为 5s 的两倍，成本敏感时可关。
	AllowLongTakes bool `json:"allow_long_takes"`
	// ChainSameScene 同场景内上一镜的**末帧**直接作为下一镜的首帧（业界标准的
	// 首尾帧链式工作流）：镜头衔接处动作/光线/机位天然连续，硬切的跳变感消失。
	// 场景切换的镜头仍走分镜首帧，避免跨场景"闪现"。
	ChainSameScene bool `json:"chain_same_scene"`
	// EndFrameAnchor 每镜生成视频时附上"尾帧锚点"（本镜分镜构图），作为
	// CogVideoX-3 首尾帧数组的第二张——运动的终点被钉在分镜构图上，中途不漂离。
	// 有承接的镜头：首帧=上镜末帧、尾帧=本镜构图（首尾夹逼）；
	// 无承接的镜头（片头/定点重渲/跨场景）：首尾两端同为分镜构图，画面锁定更稳。
	// 代价：每镜多一次出图费（约几角钱）。参考图生视频路径（vidu）无尾帧概念，
	// 走该路径的镜头自动跳过锚点。
	// 严格模式：开启后锚点生成/质检失败即镜头失败（不退回单首帧）。
	EndFrameAnchor bool `json:"end_frame_anchor"`
	// ReferenceModel 参考图生视频模型：多角色同框镜头把多张角色参考帧直接喂
	// 视频模型，比"单首帧 + 文字提示"的同框一致性好得多；留空 = 禁用该路径
	// （同框一致性改由多参考图出图 providers.image_edit 保证）。
	// 智谱端点填 vidu2-reference；火山方舟端点填支持参考图角色的 Seedance 模型
	// （参考帧会以 role=reference_image 发送）。
	ReferenceModel string `json:"reference_model"`
	// Watermark 是否在生成的视频上保留平台水印。
	//
	// 智谱该字段默认 true（显式水印 + 隐式数字水印）。成片交付绝不能带可见角标，
	// 因此这里默认 false 并**显式**传 watermark_enabled=false——不依赖服务端默认值，
	// 因为"不传"在官方 spec 里等于 true。
	//
	// 前提：账号需先在 个人中心-安全管理-去水印管理 签署免责声明，否则该参数不生效。
	Watermark bool `json:"watermark"`
}

// ImageGen 出图参数。
type ImageGen struct {
	Model   string `json:"model"`
	Size    string `json:"size"`
	RefSize string `json:"ref_size"`
	// Quality 出图质量档：hd | standard | 留空（沿用服务端默认）。
	//
	// hd 的官方描述是"更精细、整体一致性更高"——一致性正是本项目的命门；
	// 留空是为了不改动既有行为（服务端默认 standard）。
	Quality string `json:"quality"`
	// Watermark 是否在生成的图上保留平台水印；理由同 VideoGen.Watermark。
	// 参考帧带水印会作为参考图/首帧一路传下去，在成片里变成运动鬼影。
	Watermark bool `json:"watermark"`
	// Workers 参考帧生成的并发数。各角色的参考帧彼此独立，天然可并行；
	// 出图接口的限流比视频宽松，3 路并发即可把"多角色等待参考帧"的时间近似按比例缩短。
	Workers int `json:"workers"`
}

// QualityGate 逐项质检阈值。每一项都必须"可通过/可通过占位补足/必须重渲/必须阻断"四态明确。
type QualityGate struct {
	MinSharpness float64 `json:"min_sharpness"`  // 拉普拉斯方差下限，低于此判定模糊
	MinMeanLuma  float64 `json:"min_mean_luma"`  // 平均亮度下限（0-255），防全黑
	MaxMeanLuma  float64 `json:"max_mean_luma"`  // 平均亮度上限，防过曝
	MinStdLuma   float64 `json:"min_std_luma"`   // 亮度标准差下限，防纯色/纯黑
	MaxFaceRatio float64 `json:"max_face_ratio"` // 人脸面积占比上限，防"脸糊满屏幕"畸形
	MinFaceRatio float64 `json:"min_face_ratio"` // 人脸面积占比下限（有角色出场时）
	// MaxMeanSaturation 平均饱和度上限（HSV S，0-1；0=关闭）。糖水片（重散景+
	// 青橙过饱和）是 AI 出图默认审美，"一眼 AI"的最大来源；夜戏自动放宽 0.10。
	MaxMeanSaturation    float64 `json:"max_mean_saturation"`
	MaxSubtitleCPS       float64 `json:"max_subtitle_cps"` // 字幕每秒字符数上限（中文 9 字/秒为舒适区）
	MaxSubtitleLineRunes int     `json:"max_subtitle_line_runes"`
	MaxSubtitleLines     int     `json:"max_subtitle_lines"`
	DurationTolerance    float64 `json:"duration_tolerance"` // 成片时长与镜头和允许偏差（秒）
	MaxCutDriftSec       float64 `json:"max_cut_drift_sec"`  // 单镜头计划/实测时长允许漂移
	LoudnessTarget       float64 `json:"loudness_target_lufs"`
	LoudnessTolerance    float64 `json:"loudness_tolerance_lu"`
	TruePeakDB           float64 `json:"true_peak_db"`
}

// Transitions 剪辑转场参数。editplan 阶段产出的 fade_in/dissolve 方案在这里落地。
//
// 关键设计：叠化**不改变成片时间轴**。实现方式是在边界处用"上一镜尾段 × 下一镜
// 头段"做 xfade，并把下一镜按重叠时长做末帧冻结补长——成片总时长与各镜头实测
// 时长之和严格相等，字幕/配音/音效的实测时间轴因此全部保持有效。
type Transitions struct {
	// Enabled false = 全部硬切（旧行为）
	Enabled bool `json:"enabled"`
	// DissolveSec 场景切换叠化的时长（秒）。0 或过小回落默认。
	DissolveSec float64 `json:"dissolve_sec"`
	// FadeInSec 片头淡入时长（秒）；0 = 不做片头淡入
	FadeInSec float64 `json:"fade_in_sec"`
	// FadeOutSec 片尾淡出时长（秒）；0 = 不做片尾淡出
	FadeOutSec float64 `json:"fade_out_sec"`
	// MatchExposure 镜头间曝光匹配：按各镜头实测平均亮度与全片中位数的差，
	// 在 concat 时逐镜头施加小幅 eq 亮度校正。生成端每镜色调/曝光会漂移，
	// 成片上表现为"镜头间闪一下"——这是"AI 味"的直接来源之一，必须拉齐。
	// 只做温和校正（±0.06，约 ±15 亮度级），不做调色（风格归 prompt 管）。
	// 指针类型：nil = 默认开启（与"显式 false 关闭"区分）。
	MatchExposure *bool `json:"match_exposure"`
	// Scope 画幅交付模式：auto（默认）= 生成端按 16:9 出片，交付时按
	// shotspec 的 Style.Aspect（如 2.39:1）中心裁切并 letterbox 进交付容器——
	// HD scope 的标准交付形态，"宽银幕感"由画幅保证而不是提示词里的一句话。
	// off = 保持生成画幅不裁切。
	Scope string `json:"scope"`
}

// MatchExposureEnabled MatchExposure 的取值口径（nil 视为开启）。
func (t Transitions) MatchExposureEnabled() bool {
	return t.MatchExposure == nil || *t.MatchExposure
}

// ScopeEnabled 宽银幕裁切是否启用（"" 视为 auto）。
func (t Transitions) ScopeEnabled() bool {
	s := strings.ToLower(strings.TrimSpace(t.Scope))
	return s == "" || s == "auto" || s == "on"
}

// VoiceCompressEnabled 对白轻压缩是否启用（nil 视为开启）。
func (a Audio) VoiceCompressEnabled() bool {
	return a.VoiceCompress == nil || *a.VoiceCompress
}

// Music BGM 自动化参数。
type Music struct {
	// LibraryDir 本地免版权曲库目录（相对工作目录）。曲库按文件名打标签：
	// `悬疑_克制_60bpm_弦乐.wav` → tags=[悬疑,克制,弦乐] tempo=60。
	// deliver 阶段按 cue 的 mood/instruments 自动选曲复制到 output/music/。
	LibraryDir string `json:"library_dir"`
	// SFXLibraryDir 音效库目录（相对工作目录），按文件名标签与 music.json 的
	// sound_fx 描述匹配（`雨_屋顶_循环.wav` ← "雨点打在铁皮屋顶上"）。
	// 命中后复制为 output/sfx/<shot_id>.sfx.wav，混音链自动挂到对应镜头。
	SFXLibraryDir string `json:"sfx_library_dir"`
	// Generate 曲库没有可用素材时，是否调用音乐生成 API 生成纯音乐
	// （需配置 providers.music 端点，如 MiniMax Music）。失败只告警不阻断交付。
	Generate bool `json:"generate"`
}

// Audio 声音后期参数。
type Audio struct {
	SampleRate    int               `json:"sample_rate"`
	Channels      int               `json:"channels"`
	Voice         string            `json:"voice"`     // 默认 TTS 音色
	VoiceMap      map[string]string `json:"voice_map"` // 角色名 → 音色（取代关键词猜音调）
	NarratorVoice string            `json:"narrator_voice"`
	// SkipNarrator 全片关闭旁白/独白：配音只出角色对白（短剧对白驱动风格）。
	// 过滤在合成入口完成，台词索引保持原序——对白句的缓存文件名不受影响，
	// 已付费的合成资产照常复用。
	SkipNarrator bool `json:"skip_narrator,omitempty"`
	// MouthAlign 口型对齐（段落级）：检测镜头里角色的张嘴窗口，把台词
	// 入点对到窗口起点、时长适配到窗口长度。检测走视觉模型（付费，结果
	// 缓存 artifacts/mouth_windows.json），失败/低置信度回落普通入点。
	MouthAlign bool    `json:"mouth_align,omitempty"`
	BGMGainDB  float64 `json:"bgm_gain_db"`
	// DuckDB 人声出现时 BGM 的压低量（dB，负值，典型 -9）。
	//
	// 实现说明：sidechaincompress 没有"压低 N dB"这样的直接参数，只有阈值与压缩比。
	// 本项目把 DuckDB 映射为 (threshold=0.02, ratio≈2^(-duck/3))，
	// 实测（ffmpeg 3.0，配乐 -27dBFS 素材）duck=-9 时实际压低约 10dB。
	// 阈值取低值是为了保证对白一出现就可靠触发——阈值高于配乐电平时会完全不触发
	// （实测 threshold=0.3 时压低量为 0dB，即"配了参数等于没做"）。
	DuckDB     float64 `json:"duck_db"`
	SFXGainDB  float64 `json:"sfx_gain_db"`
	MaxSpeedUp float64 `json:"max_speed_up"` // 超槽加速上限（>1.2 起可闻"赶"感，越大越机械）
	LineGapSec float64 `json:"line_gap_sec"` // 相邻台词的静音间隔
	LeadInSec  float64 `json:"lead_in_sec"`  // 台词相对镜头起点的入点偏移
	// JCutSec 声音提前切入上限（秒，0=关闭）。前一镜完全无台词时，下一镜首句
	// 台词可提前越过硬切点——J-cut 是电影最常用的转场语法（"声音带观众进下一镜"）。
	// 字幕与音频按同一提前量排版，音画字保持同步；与台词/槽位冲突时自动退化为普通排麦。
	JCutSec float64 `json:"jcut_sec"`
	// CueFadeOutSec 配乐在场景边界收尾的淡出时长（秒，0=不淡出）。
	// 戛然而止的配乐是"AI 味"的听觉特征之一；淡出让音乐转折落在切换点上。
	CueFadeOutSec float64 `json:"cue_fade_out_sec"`
	// VoiceCompress 对白总线轻压缩。指针：nil = 默认开启。
	VoiceCompress  *bool   `json:"voice_compress"`
	TargetLoudness float64 `json:"target_loudness_lufs"`
	// TTSSpeed 服务端合成语速（glm-tts 支持 0.5-2，1.0 = 服务端默认）。
	// 服务端语速是"说话人就真的这么说"，比本地 atempo 拉伸自然得多；
	// 本地 atempo 只作为超槽兜底。1.0 时不向接口传 speed。
	TTSSpeed float64 `json:"tts_speed"`
	// TTSEmotion 是否把台本里每句的 emotion 标注映射为 TTS 情感参数。
	// glm-tts 无 emotion 入参（能力上限，只能靠标点驱动韵律）；
	// 支持 emotion 的供应商（如 MiniMax speech-2.6+）会真正吃到该参数。
	TTSEmotion bool `json:"tts_emotion"`
}

// Subtitle 字幕参数。
type Subtitle struct {
	Font        string   `json:"font"`
	FontSize    int      `json:"font_size"`
	Languages   []string `json:"languages"` // 首个为原语言；其余为外挂/烧录译文
	MarginV     int      `json:"margin_v"`
	MaxCPS      float64  `json:"max_cps"`
	MinDurSec   float64  `json:"min_dur_sec"`
	BurnPrimary bool     `json:"burn_primary"` // 是否把主语言烧进画面
	// 折行约束：中文单行 16-18 字是舒适区，超过就需要折行
	MaxLineRunes int `json:"max_line_runes"`
	MaxLines     int `json:"max_lines"`
	OutlineWidth int `json:"outline_width"` // 描边宽度：保证在亮背景上仍可读
}

// Config 顶层配置。
type Config struct {
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	Mock      bool   `json:"mock"`

	// APIKeyLiteral 直接写死的密钥，JSON 键为 "api_key"。
	// **只应出现在 config.local.json（已 .gitignore）**。
	//
	// 为什么需要：只靠环境变量时，每个新终端/每次重启会话都要重新 export 一次，
	// 实践上会把人逼回"把 Key 贴进 config.json"——而 config.json 是入库文件，
	// 一次提交就永久泄露。给一个被忽略的本地层，比反复叮嘱"别忘了 export"有效。
	// 密钥解析顺序见 APIKey()。
	APIKeyLiteral string `json:"api_key"`

	// LLM 默认采样（可被技能级 callTune 覆盖）
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`

	Models   ModelTier        `json:"models"`
	Pricing  map[string]Price `json:"pricing"`
	Budget   Budget           `json:"budget"`
	Video    VideoGen         `json:"video"`
	Image    ImageGen         `json:"image"`
	Quality  QualityGate      `json:"quality"`
	Audio    Audio            `json:"audio"`
	Subtitle Subtitle         `json:"subtitle"`
	// Transitions 剪辑转场（editplan 方案的真正落地处）
	Transitions Transitions `json:"transitions"`
	// Music BGM 自动化（曲库选曲 + 可选生成）
	Music Music `json:"music"`

	// Providers 按能力覆盖服务端点/密钥环境变量。
	//
	// 为什么需要：生成端（出图/视频/TTS）与复核端常常不在同一家——
	// 例如画面用智谱 GLM-Image/CogVideoX，而视觉复核用 DeepSeek deepseek-flash。
	// 没有这个配置就只能二选一，等于放弃复核能力。
	Providers Providers `json:"providers"`

	// VisionReview 渲染后的视觉复核（让模型真的"看"画面）。
	VisionReview VisionReview `json:"vision_review"`

	// RefQC 角色参考帧质检（渲染前的一致性关卡）。
	// 参考帧是全片一致性的根：参考帧一旦与角色圣经不符（眼镜丢了、发型变了），
	// 错误会经图生图放大到每一个出镜镜头，事后无法挽回。
	// 严格模式：质检启用（strict）但视觉端点不可用/核对不通过 → 直接失败。
	RefQC RefQCConfig `json:"ref_qc"`

	// LocalFile 实际生效的本地覆盖层路径（"" 表示没有）。
	// 打印在启动信息里，避免"改了配置却没生效"这类最难查的问题。
	LocalFile string `json:"-"`
	// localSet 本地层显式出现过的键路径（如 "mock"、"video.workers"）。
	// 用于区分布尔字段的"没写"与"显式写了 false"——Go 的 bool 零值是 false，
	// 只靠反序列化无法区分，会把"没写"误判成"关掉"。
	localSet map[string]bool
}

// LocalConfigName 分层配置的本地覆盖层文件名，与基础配置同目录。
//
// 用途：放密钥与本机专属参数。**约定该文件必须进 .gitignore。**
const LocalConfigName = "config.local.json"

// Providers 按能力的端点覆盖（空值 = 沿用顶层 base_url / api_key_env）。
type Providers struct {
	Text   Endpoint `json:"text"`
	Vision Endpoint `json:"vision"`
	Image  Endpoint `json:"image"`
	Video  Endpoint `json:"video"`
	TTS    Endpoint `json:"tts"`
	// ImageEdit 图像编辑/多参考图端点（可选）。角色一致性的终局方案：
	// 把多张角色参考帧一起喂给支持多图参考的模型（如 Seedream 4.0 /
	// Qwen-Image-Edit）直接生成"两人同框"的首帧，绕开"单参考帧只锁一张脸"的限制。
	// 留空 = 不启用，走单参考帧降级路径。
	ImageEdit Endpoint `json:"image_edit"`
	// Music 音乐生成端点（可选，如 MiniMax Music）。曲库缺素材时按 cue 生成纯音乐。
	Music Endpoint `json:"music"`
}

// Endpoint 一个服务的接入点。
type Endpoint struct {
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	// APIKeyLiteral 端点专属密钥（本地层，不入库）。为空时回落到顶层 api_key。
	APIKeyLiteral string `json:"api_key"`
	// Model 可选：覆盖该能力的模型名（视觉复核建议显式写，避免选到不支持视觉的模型）
	Model string `json:"model,omitempty"`
	// AppIDEnv/AppIDLiteral 二级凭据（双凭据供应商用，如火山语音的 appid +
	// access token）。解析顺序与密钥一致：环境变量 > 本地层。
	AppIDEnv     string `json:"appid_env,omitempty"`
	AppIDLiteral string `json:"appid,omitempty"`
}

// VisionReview 视觉复核参数。
type VisionReview struct {
	Enabled bool `json:"enabled"`
	// SampleFrames 每个镜头抽几帧送检（3 帧足以发现卡帧/黑帧/形变）
	SampleFrames int `json:"sample_frames"`
	// PassScore 单镜头达标分（0-10）
	PassScore float64 `json:"pass_score"`
	// MaxShotsPerRequest 每次请求送检的镜头数（控制请求体大小）
	MaxShotsPerRequest int `json:"max_shots_per_request"`
	// MaxReviewShots 最多复核多少镜头（0=全部）；长片可抽样控制成本
	MaxReviewShots int `json:"max_review_shots"`
	// FrameWidth 抽帧宽度（越小越省 token；640 足够判断一致性）
	FrameWidth int `json:"frame_width"`
	// Workers 复核批次并发数。批次彼此独立，多模态请求单次常达十几秒，
	// 2 路并发可把复核墙钟时间近似减半；再高容易触发视觉接口限流。
	Workers int `json:"workers"`
}

// RefQC 角色参考帧质检参数。参考帧与角色圣经不符时，错误会经图生图
// 放大到每个出镜镜头（参考帧没眼镜、成片处处眼镜）——必须在开工前拦住。
type RefQCConfig struct {
	// Mode strict（默认）：参考帧出图后由视觉模型对照角色圣经逐项核对，
	// 不合格自动重出（换种子 + 追加修复提示）；off：跳过核对。
	// 严格模式：无视觉密钥/探活失败时拒绝开工（不允许静默跳过核对）。
	Mode string `json:"mode"`
	// MaxRounds 不合格重出的最大轮数（不含首次生成）。
	MaxRounds int `json:"max_rounds"`
}

// RefQCEnabled 质检是否启用。
func (c *Config) RefQCEnabled() bool { return c.RefQC.Mode != "off" }

// Default 返回生产默认值。所有阈值都取"宁可拦下来也别放行"的保守侧。
func Default() *Config {
	return &Config{
		BaseURL:     "https://open.bigmodel.cn/api/paas/v4",
		APIKeyEnv:   "AI_API_KEY",
		Model:       "glm-4.6",
		Temperature: 0.8,
		MaxTokens:   4096,
		Mock:        true,
		Models: ModelTier{
			Texture:   "cogvideox-3",
			Economy:   "cogvideox-3",
			Vision:    "glm-5.3-flash",
			Text:      "glm-4.6",
			Translate: "glm-4-flash",
			TTS:       "glm-tts",
		},
		Pricing: map[string]Price{
			"text":      {PromptPer1K: 0.001, CompletionPer1K: 0.002},
			"vision":    {PromptPer1K: 0.004, CompletionPer1K: 0.004},
			"translate": {PromptPer1K: 0.0001, CompletionPer1K: 0.0001},
			"image":     {ImagePerShot: 0.10},
			"video":     {VideoPerSec: 0.20},
			"tts":       {TTSPer1KChars: 0.10},
			"music":     {MusicPerCall: 1.00},
		},
		Budget: Budget{MaxCostCNY: 0, WarnRatio: 0.8, StopOnNoFund: true},
		Video: VideoGen{
			Model:          "cogvideox-3",
			QualityModel:   "cogvideox-3",
			EconomyModel:   "cogvideox-3",
			DefaultQuality: "quality",
			Size:           "1920x1080",
			FPS:            30,
			Workers:        2,
			TimeoutMinutes: 20,
			ShotRetries:    3,
			RerenderRounds: 2,
			WithAudio:      false,
			ImageToImage:   true,
			RefSize:        "1024x1024",
			Watermark:      false,
			// 10s 档：CogVideoX-3 官方支持，长镜头不再被压成 5s
			AllowLongTakes: true,
			// 同场景末帧→首帧链式衔接：官方 image_url 数组第二张即尾帧的能力前提
			ChainSameScene: true,
			// 每镜都带尾帧锚点（首尾帧夹逼），不再只限链式承接镜头
			EndFrameAnchor: true,
			// vidu2-reference：智谱平台上的参考图生视频模型（1-3 张参考图，
			// 专为一致主体设计），多角色同框镜头的一致性显著优于单参考帧图生图
			ReferenceModel: "vidu2-reference",
		},
		Image: ImageGen{
			Model: "glm-image",
			// glm-image 要求尺寸在 512-2048 内且为 32 的整数倍，1280x720 不合法；
			// 1248x704 是最接近 16:9 的合规值。
			Size:      "1248x704",
			RefSize:   "1024x1024",
			Quality:   "", // glm-image 没有 quality 参数，传 hd 会被拒
			Watermark: false,
			Workers:   3,
		},
		Quality: QualityGate{
			MinSharpness:         55,
			MinMeanLuma:          18,
			MaxMeanLuma:          238,
			MinStdLuma:           14,
			MaxFaceRatio:         0.55,
			MinFaceRatio:         0.020,
			MaxMeanSaturation:    0.58,
			MaxSubtitleCPS:       9.0,
			MaxSubtitleLineRunes: 18,
			MaxSubtitleLines:     2,
			DurationTolerance:    0.35,
			MaxCutDriftSec:       1.5,
			LoudnessTarget:       -14.0,
			LoudnessTolerance:    1.5,
			TruePeakDB:           -1.0,
		},
		Audio: Audio{
			// 48k：交付容器（AAC 48kHz）与混音总线一致，避免 24k 总线再上采样
			SampleRate:    48000,
			Channels:      2,
			Voice:         "tongtong",
			VoiceMap:      map[string]string{},
			NarratorVoice: "tongtong",
			BGMGainDB:     -18,
			DuckDB:        -9,
			SFXGainDB:     -14,
			// 1.15 是"听不出赶"的经验上限：再高宁可让台词溢出到下一镜头
			// （字幕跟随实测时间），也不做明显失真的加速。
			MaxSpeedUp:     1.15,
			LineGapSec:     0.28,
			LeadInSec:      0.18,
			JCutSec:        0.3,
			CueFadeOutSec:  2.0,
			TargetLoudness: -14,
			TTSSpeed:       1.0,
			TTSEmotion:     true,
		},
		// 转场默认全开：editplan 的 fade_in/dissolve 方案从"死数据"变成真实画面
		Transitions: Transitions{
			Enabled:     true,
			DissolveSec: 0.6,
			FadeInSec:   0.8,
			FadeOutSec:  1.0,
		},
		Music: Music{
			LibraryDir:    "library/music",
			SFXLibraryDir: "library/sfx",
			Generate:      false,
		},
		Subtitle: Subtitle{
			Font:         "Source Han Sans SC Medium",
			FontSize:     22,
			Languages:    []string{"zh"},
			MarginV:      42,
			MaxCPS:       9.0,
			MinDurSec:    0.9,
			BurnPrimary:  false,
			MaxLineRunes: 18,
			MaxLines:     2,
			OutlineWidth: 2,
		},
		// 视觉复核默认关闭：它需要额外的视觉模型（可能来自另一家服务商），
		// 开启前应确认端点与模型都支持图片输入。
		// 严格模式：一旦启用（或 --visual），复核不可用（无密钥/探活失败）即报错，
		// 不再静默跳过——要跳过请显式 --no-visual。
		VisionReview: VisionReview{
			Enabled:            false,
			SampleFrames:       3,
			PassScore:          7.5,
			MaxShotsPerRequest: 4,
			MaxReviewShots:     0,
			FrameWidth:         640,
			Workers:            2,
		},
		// 参考帧质检默认开：它防的是"全片换脸"这种最贵的错误，
		// 且只在渲染开头花几次视觉调用；无视觉密钥时自动跳过（显式可见）。
		RefQC: RefQCConfig{
			Mode:      "strict",
			MaxRounds: 2,
		},
	}
}

// DeepSeekPreset 生成一份 DeepSeek 接入预设。
//
// 用途：文本创作/复核用 DeepSeek，画面生成仍用智谱——
//
//	cfg := config.DeepSeekPreset(); cfg.Providers.Text = config.Endpoint{BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY", Model: "deepseek-flash"}
//
// 注意：DeepSeek 当前只有 deepseek-flash 支持图片输入（deepseek-v4-pro 不支持 Vision）。
func DeepSeekPreset() *Config {
	c := Default()
	c.Providers.Text = Endpoint{
		BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY", Model: "deepseek-flash",
	}
	c.Providers.Vision = Endpoint{
		BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY", Model: "deepseek-flash",
	}
	c.Models.Text = "deepseek-flash"
	c.Models.Vision = "deepseek-flash"
	c.Models.Translate = "deepseek-flash"
	c.VisionReview.Enabled = true
	return c
}

// Load 读取配置文件并补齐缺省值。文件不存在时返回默认配置（不报错，便于离线演示）。
//
// 分层：config.json（入库，团队共享）→ 同目录 config.local.json（**必须 .gitignore**，
// 放密钥与本机参数）→ 环境变量（最高优先级，CI 用）。本地层缺失不算错误。
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
		}
		// 基础配置缺失也继续：便于纯本地/纯环境变量的用法
	} else if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("配置 %s 解析失败: %w", path, err)
	}
	cfg.localSet = map[string]bool{}

	if err := mergeLocal(cfg, filepath.Join(filepath.Dir(path), LocalConfigName)); err != nil {
		return nil, err
	}
	cfg.normalize()
	return cfg, nil
}

// mergeLocal 把本地覆盖层合并进 cfg，并记录显式出现过的键路径。
func mergeLocal(cfg *Config, localPath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取本地配置 %s 失败: %w", localPath, err)
	}

	var keys map[string]any
	if err := json.Unmarshal(data, &keys); err != nil {
		return fmt.Errorf("本地配置 %s 解析失败: %w", localPath, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("本地配置 %s 应用失败: %w", localPath, err)
	}

	cfg.localSet = flattenKeys("", keys)
	cfg.LocalFile = localPath
	return nil
}

// flattenKeys 把嵌套的 JSON 对象拍平成"a.b.c" 形式的键路径集合。
func flattenKeys(prefix string, v map[string]any) map[string]bool {
	out := map[string]bool{}
	for k, val := range v {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		out[path] = true
		if sub, ok := val.(map[string]any); ok {
			for kk := range flattenKeys(path, sub) {
				out[kk] = true
			}
		}
	}
	return out
}

// WasSetLocally 判断某键路径是否在本地层被显式设置过。
func (c *Config) WasSetLocally(path string) bool {
	return c.localSet[path]
}

// APIKey 解析顶层密钥，顺序为：
//
//	① 环境变量（CI 与临时覆盖用，优先级最高）
//	② config.local.json 里的 api_key（日常开发最省事的一层）
//
// 显式拒绝从 config.json 取密钥：那是入库文件，写进去就是永久泄露。
func (c *Config) APIKey() string {
	if c.APIKeyEnv != "" {
		if v := os.Getenv(c.APIKeyEnv); v != "" {
			return v
		}
	}
	return strings.TrimSpace(c.APIKeyLiteral)
}

// APIKeyFor 解析指定能力的密钥，端点未指定环境变量时回落到顶层解析。
func (c *Config) APIKeyFor(ep Endpoint) string {
	if ep.APIKeyEnv == "" {
		// 端点直填的本地层密钥优先于顶层——像 providers.music 指向豆包
		// （与 TTS 共用一把 key 直填 api_key 字段）这种"单凭据独立端点"，
		// 回落顶层会把智谱的 key 发给另一家（401 且账单错位）。
		if v := strings.TrimSpace(ep.APIKeyLiteral); v != "" {
			return v
		}
		return c.APIKey()
	}
	if v := os.Getenv(ep.APIKeyEnv); v != "" {
		return v
	}
	// 端点自带 env 名但没配到：仍允许本地层兜底，避免"配了端点就丢了密钥"
	if ep.APIKeyEnv == c.APIKeyEnv {
		return c.APIKey()
	}
	return strings.TrimSpace(ep.APIKeyLiteral)
}

// AppIDFor 解析指定端点的二级凭据（appid）。双凭据供应商（火山语音等）用；
// 解析顺序与密钥一致：环境变量 > 端点本地层。顶层无 appid 概念，不回落。
func (c *Config) AppIDFor(ep Endpoint) string {
	if v := os.Getenv(ep.AppIDEnv); v != "" {
		return v
	}
	return strings.TrimSpace(ep.AppIDLiteral)
}

// normalize 补齐零值并做一致性校正，保证运行期不会出现"阈值为 0 导致全部误判"。
func (c *Config) normalize() {
	d := Default()
	if c.BaseURL == "" {
		c.BaseURL = d.BaseURL
	}
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = d.APIKeyEnv
	}
	if c.Model == "" {
		c.Model = d.Model
	}
	if c.Models.Text == "" {
		c.Models.Text = c.Model
	}
	for _, kv := range []struct {
		p *string
		v string
	}{
		{&c.Models.Texture, d.Models.Texture},
		{&c.Models.Economy, d.Models.Economy},
		{&c.Models.Vision, d.Models.Vision},
		{&c.Models.Translate, d.Models.Translate},
		{&c.Models.TTS, d.Models.TTS},
	} {
		if *kv.p == "" {
			*kv.p = kv.v
		}
	}
	if len(c.Pricing) == 0 {
		c.Pricing = d.Pricing
	}
	for k, v := range d.Pricing {
		if _, ok := c.Pricing[k]; !ok {
			c.Pricing[k] = v
		}
	}
	if c.Video.Workers <= 0 {
		c.Video.Workers = d.Video.Workers
	}
	if c.Video.TimeoutMinutes <= 0 {
		c.Video.TimeoutMinutes = d.Video.TimeoutMinutes
	}
	if c.Video.ShotRetries <= 0 {
		c.Video.ShotRetries = d.Video.ShotRetries
	}
	if c.Video.RerenderRounds < 0 {
		c.Video.RerenderRounds = d.Video.RerenderRounds
	}
	if c.Video.Size == "" {
		c.Video.Size = d.Video.Size
	}
	if c.Video.FPS <= 0 {
		c.Video.FPS = d.Video.FPS
	}
	if c.Video.DefaultQuality == "" {
		c.Video.DefaultQuality = d.Video.DefaultQuality
	}
	if c.Video.EconomyModel == "" {
		c.Video.EconomyModel = c.Video.Model
	}
	if c.Video.QualityModel == "" {
		c.Video.QualityModel = c.Video.Model
	}
	if c.Video.RefSize == "" {
		c.Video.RefSize = d.Video.RefSize
	}
	if c.Image.Model == "" {
		c.Image.Model = d.Image.Model
	}
	if c.Image.Size == "" {
		c.Image.Size = d.Image.Size
	}
	if c.Image.RefSize == "" {
		c.Image.RefSize = d.Image.RefSize
	}
	if c.Image.Workers <= 0 {
		c.Image.Workers = d.Image.Workers
	}
	if c.Budget.WarnRatio <= 0 || c.Budget.WarnRatio > 1 {
		c.Budget.WarnRatio = d.Budget.WarnRatio
	}
	if c.Audio.SampleRate <= 0 {
		c.Audio.SampleRate = d.Audio.SampleRate
	}
	if c.Audio.Channels <= 0 {
		c.Audio.Channels = d.Audio.Channels
	}
	if c.Audio.Voice == "" {
		c.Audio.Voice = d.Audio.Voice
	}
	if c.Audio.NarratorVoice == "" {
		c.Audio.NarratorVoice = d.Audio.NarratorVoice
	}
	if c.Audio.VoiceMap == nil {
		c.Audio.VoiceMap = map[string]string{}
	}
	if c.Audio.MaxSpeedUp <= 1 {
		c.Audio.MaxSpeedUp = d.Audio.MaxSpeedUp
	}
	if c.Audio.TTSSpeed <= 0 {
		c.Audio.TTSSpeed = d.Audio.TTSSpeed
	}
	if c.Audio.LineGapSec < 0 {
		c.Audio.LineGapSec = d.Audio.LineGapSec
	}
	if c.Audio.LeadInSec < 0 {
		c.Audio.LeadInSec = d.Audio.LeadInSec
	}
	if c.Audio.JCutSec < 0 {
		c.Audio.JCutSec = d.Audio.JCutSec
	}
	if c.Audio.JCutSec > 0.6 {
		c.Audio.JCutSec = 0.6 // 提前量过大观众会以为声音错位了
	}
	if c.Audio.CueFadeOutSec <= 0 {
		c.Audio.CueFadeOutSec = d.Audio.CueFadeOutSec
	}
	if c.Transitions.Scope == "" {
		c.Transitions.Scope = "auto"
	}
	if c.Audio.BGMGainDB == 0 {
		c.Audio.BGMGainDB = d.Audio.BGMGainDB
	}
	if c.Audio.SFXGainDB == 0 {
		c.Audio.SFXGainDB = d.Audio.SFXGainDB
	}
	if c.Audio.DuckDB == 0 {
		c.Audio.DuckDB = d.Audio.DuckDB
	}
	if c.Subtitle.FontSize <= 0 {
		c.Subtitle.FontSize = d.Subtitle.FontSize
	}
	if c.Subtitle.Font == "" {
		c.Subtitle.Font = d.Subtitle.Font
	}
	if len(c.Subtitle.Languages) == 0 {
		c.Subtitle.Languages = d.Subtitle.Languages
	}
	if c.Subtitle.MaxCPS <= 0 {
		c.Subtitle.MaxCPS = c.Quality.MaxSubtitleCPS
	}
	if c.Subtitle.MinDurSec <= 0 {
		c.Subtitle.MinDurSec = d.Subtitle.MinDurSec
	}
	// 转场与 BGM 自动化：负值视为未设置（0 有"显式关闭"的合法语义，如关闭片头淡入）
	if c.Transitions.DissolveSec < 0 {
		c.Transitions.DissolveSec = d.Transitions.DissolveSec
	}
	if c.Transitions.FadeInSec < 0 {
		c.Transitions.FadeInSec = d.Transitions.FadeInSec
	}
	if c.Transitions.FadeOutSec < 0 {
		c.Transitions.FadeOutSec = d.Transitions.FadeOutSec
	}
	if c.Music.LibraryDir == "" {
		c.Music.LibraryDir = d.Music.LibraryDir
	}
	if c.Music.SFXLibraryDir == "" {
		c.Music.SFXLibraryDir = d.Music.SFXLibraryDir
	}
	// 质检阈值：任何为 0 的项回落到默认，否则"下限=0"会让门禁形同虚设
	if c.Quality.MinSharpness <= 0 {
		c.Quality.MinSharpness = d.Quality.MinSharpness
	}
	if c.Quality.MaxMeanSaturation <= 0 {
		c.Quality.MaxMeanSaturation = d.Quality.MaxMeanSaturation
	}
	if c.Quality.MinMeanLuma <= 0 {
		c.Quality.MinMeanLuma = d.Quality.MinMeanLuma
	}
	if c.Quality.MaxMeanLuma <= 0 {
		c.Quality.MaxMeanLuma = d.Quality.MaxMeanLuma
	}
	if c.Quality.MinStdLuma <= 0 {
		c.Quality.MinStdLuma = d.Quality.MinStdLuma
	}
	if c.Quality.MaxSubtitleCPS <= 0 {
		c.Quality.MaxSubtitleCPS = d.Quality.MaxSubtitleCPS
	}
	if c.Quality.MaxSubtitleLineRunes <= 0 {
		c.Quality.MaxSubtitleLineRunes = d.Quality.MaxSubtitleLineRunes
	}
	if c.Quality.MaxSubtitleLines <= 0 {
		c.Quality.MaxSubtitleLines = d.Quality.MaxSubtitleLines
	}
	if c.Quality.DurationTolerance <= 0 {
		c.Quality.DurationTolerance = d.Quality.DurationTolerance
	}
	if c.Quality.LoudnessTarget == 0 {
		c.Quality.LoudnessTarget = d.Quality.LoudnessTarget
	}
	// 视觉复核参数（0 值一律回落默认，避免"抽 0 帧"这类静默失效）
	if c.VisionReview.SampleFrames <= 0 {
		c.VisionReview.SampleFrames = d.VisionReview.SampleFrames
	}
	if c.VisionReview.SampleFrames > 6 {
		c.VisionReview.SampleFrames = 6 // 单镜头送太多帧会稀释注意力且浪费 token
	}
	if c.VisionReview.PassScore <= 0 {
		c.VisionReview.PassScore = d.VisionReview.PassScore
	}
	if c.VisionReview.MaxShotsPerRequest <= 0 {
		c.VisionReview.MaxShotsPerRequest = d.VisionReview.MaxShotsPerRequest
	}
	if c.VisionReview.FrameWidth <= 0 {
		c.VisionReview.FrameWidth = d.VisionReview.FrameWidth
	}
	if c.VisionReview.Workers <= 0 {
		c.VisionReview.Workers = d.VisionReview.Workers
	}
	// 参考帧质检：mode 只认 strict/off（含大小写与空串），轮数夹在 1-4
	c.RefQC.Mode = strings.ToLower(strings.TrimSpace(c.RefQC.Mode))
	switch c.RefQC.Mode {
	case "", "strict":
		c.RefQC.Mode = "strict"
	case "off", "false", "none":
		c.RefQC.Mode = "off"
	default:
		c.RefQC.Mode = "strict"
	}
	if c.RefQC.MaxRounds <= 0 {
		c.RefQC.MaxRounds = d.RefQC.MaxRounds
	}
	if c.RefQC.MaxRounds > 4 {
		c.RefQC.MaxRounds = 4 // 重出太多次说明该角色圣经本身有问题，该人工介入
	}
	// 端点：任一能力未显式配置则沿用顶层，避免"配了一半"导致请求打到错的服务
	for _, ep := range []*Endpoint{
		&c.Providers.Text, &c.Providers.Vision,
		&c.Providers.Image, &c.Providers.Video, &c.Providers.TTS,
	} {
		if ep.BaseURL == "" {
			ep.BaseURL = c.BaseURL
		}
		if ep.APIKeyEnv == "" {
			ep.APIKeyEnv = c.APIKeyEnv
		}
	}
	if c.Providers.Text.Model == "" {
		c.Providers.Text.Model = c.Models.Text
	}
	if c.Providers.Vision.Model == "" {
		c.Providers.Vision.Model = c.Models.Vision
	}
	if c.Providers.Image.Model == "" {
		c.Providers.Image.Model = c.Image.Model
	}
	if c.Providers.Video.Model == "" {
		c.Providers.Video.Model = c.VideoModel(c.Video.DefaultQuality)
	}
	if c.Providers.TTS.Model == "" {
		c.Providers.TTS.Model = c.Models.TTS
	}
}

// ModelFor 按档位名返回模型名。档位名未知时回落到文本模型。
func (c *Config) ModelFor(tier string) string {
	switch tier {
	case "texture":
		return c.Models.Texture
	case "economy":
		return c.Models.Economy
	case "vision":
		return c.Models.Vision
	case "translate":
		return c.Models.Translate
	default:
		return c.Models.Text
	}
}

// VideoModel 按画质档位返回视频模型。
func (c *Config) VideoModel(quality string) string {
	if strings.EqualFold(quality, "speed") || strings.EqualFold(quality, "economy") {
		if c.Video.EconomyModel != "" {
			return c.Video.EconomyModel
		}
	}
	if c.Video.QualityModel != "" {
		return c.Video.QualityModel
	}
	return c.Video.Model
}

// PriceFor 取某档位单价，缺省返回零价（不阻塞，但成本统计会少算，启动时会告警）。
func (c *Config) PriceFor(kind string) Price {
	if p, ok := c.Pricing[kind]; ok {
		return p
	}
	return Price{}
}

// 视觉模型白名单：**按服务商分家**。
//
// 依据各家官方文档里 chat/completions 视觉请求的 model 枚举（核对后更新）。
// 为什么要在这里写死：**配错视觉模型的代价是静默跳过复核**——
// deliver 会探测一次（Ping），失败只打印一行警告就继续交付。结果是
// "以为查过了、其实没查"，而水印/换脸恰恰只能靠这一步发现。
// 早一步在配置自检里拦住，比在交付时悄悄降级好得多。
var (
	// zhipuVisionModels 智谱支持的图片输入模型。
	zhipuVisionModels = []string{
		"glm-5.3-flashx",
		"glm-5.3-flash",
		"glm-5v-turbo",
		"glm-4.6v",
		"glm-4.6v-flash",
		"glm-4.6v-flashx",
		"glm-4v-flash",
		"glm-4.1v-thinking-flashx",
		"glm-4.1v-thinking-flash",
		"autoglm-phone",
	}
	// deepseekVisionModels DeepSeek 当前只有 Flash 支持图片输入
	// （v4-pro / reasoner 不支持，配错会在复核时才失败）。
	deepseekVisionModels = []string{"deepseek-flash"}
)

// SupportedVisionModels 返回该视觉端点下可用的模型编码。
//
// 服务商由 base_url 判定：视觉复核常常与画面生成不是同一家
// （画面智谱、复核 DeepSeek），白名单必须跟着端点走，否则合法的配置会被自检拦死。
func (c *Config) SupportedVisionModels() []string {
	if strings.Contains(strings.ToLower(c.Providers.Vision.BaseURL), "deepseek") {
		return deepseekVisionModels
	}
	return zhipuVisionModels
}

// VisionModelSupported 判断当前配置的视觉模型是否为该端点支持的模型。
func (c *Config) VisionModelSupported() bool {
	for _, m := range c.SupportedVisionModels() {
		if m == c.Models.Vision {
			return true
		}
	}
	return false
}

// Validate 启动自检：把"配置看起来不对"变成显式错误，而不是运行到一半才发现。
func (c *Config) Validate() error {
	var problems []string
	if c.Video.Workers > 8 {
		problems = append(problems, fmt.Sprintf("video.workers=%d 过高，易触发限流（建议 ≤4）", c.Video.Workers))
	}
	// 视觉复核一旦启用，模型名必须是真实支持图片输入的那个：
	// 配错只会在交付阶段静默跳过复核，等于"以为查过、其实没查"。
	if c.VisionReview.Enabled && !c.VisionModelSupported() {
		problems = append(problems, fmt.Sprintf(
			"models.vision=%q 不是该视觉端点支持的图片输入模型（端点 %s）。\n"+
				"    可选: %s\n"+
				"    注意旧值 glm-4v-plus / glm-4v 已不在智谱支持列表内，请求会直接失败并使复核被静默跳过。",
			c.Models.Vision, c.Providers.Vision.BaseURL, strings.Join(c.SupportedVisionModels(), ", ")))
	}
	if c.Quality.MinSharpness <= 0 {
		problems = append(problems, "quality.min_sharpness 未设置，模糊镜头会被放行")
	}
	if c.Subtitle.MaxCPS <= 0 {
		problems = append(problems, "subtitle.max_cps 未设置，字幕可能快到看不清")
	}
	if c.Audio.MaxSpeedUp > 1.5 {
		problems = append(problems, fmt.Sprintf("audio.max_speed_up=%.2f 过高，台词会明显失真", c.Audio.MaxSpeedUp))
	}
	if c.Quality.MinMeanLuma >= c.Quality.MaxMeanLuma {
		problems = append(problems, "quality 亮度上下限颠倒，所有镜头都会被判定异常")
	}
	if len(problems) > 0 {
		return fmt.Errorf("配置自检未通过:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// String 打印生效配置摘要（启动时输出，便于生产排障）。
func (c *Config) String() string {
	return fmt.Sprintf(
		"文本模型=%s 视觉复核=%s 视频=%s(%s) 出图=%s%s 水印=图:%s 视频:%s 并发=%d 单镜头重试=%d 重渲轮数=%d 预算=%s 转场=%v 链式衔接=%v 尾帧锚点=%v 参考视频=%s",
		c.Models.Text, c.Models.Vision, c.VideoModel(c.Video.DefaultQuality), c.Video.DefaultQuality,
		c.Image.Model, c.imageQualityText(),
		watermarkText(c.Image.Watermark), watermarkText(c.Video.Watermark),
		c.Video.Workers, c.Video.ShotRetries, c.Video.RerenderRounds, c.budgetText(),
		c.Transitions.Enabled, c.Video.ChainSameScene, c.Video.EndFrameAnchor, orDefault(c.Video.ReferenceModel, "关"))
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func (c *Config) imageQualityText() string {
	if c.Image.Quality == "" {
		return ""
	}
	return "(" + c.Image.Quality + ")"
}

// watermarkText 输出水印开关状态。
//
// 去水印必须显示在生效配置里：它依赖服务端参数 + 账号侧免责声明两处生效，
// 只看代码无法判断线上到底有没有角标，出片后才发现代价很大。
func watermarkText(on bool) string {
	if on {
		return "带水印"
	}
	return "去水印"
}

func (c *Config) budgetText() string {
	if c.Budget.MaxCostCNY <= 0 {
		return "不限制（建议生产环境设置上限）"
	}
	return fmt.Sprintf("≤%.2f 元", c.Budget.MaxCostCNY)
}
