package ai

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// 错误分类
//
// 生产环境里"失败"不是一个东西：鉴权失败重试一万次也没用，限流退避几秒就好了，
// 余额不足则应该**立刻停掉整批付费调用**。把它们混成一个 error 字符串，
// 就会既浪费钱又浪费时间。这里在 HTTP 层就完成分类。
// ---------------------------------------------------------------------------

// Kind 错误类别。
type Kind int

const (
	KindUnknown     Kind = iota
	KindAuth             // 鉴权/密钥错误：不可恢复
	KindNoFund           // 余额/配额不足：立即熔断整批
	KindRateLimit        // 限流：退避重试
	KindNetwork          // 网络类：直接重试
	KindBadRequest       // 请求不合法（模型名/参数/内容审核）：换参数或人工介入
	KindTimeout          // 超时
	KindUnsupported      // 接口不支持该能力（如出图不支持参考图）：走降级路径
)

func (k Kind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindNoFund:
		return "no_fund"
	case KindRateLimit:
		return "rate_limit"
	case KindNetwork:
		return "network"
	case KindBadRequest:
		return "bad_request"
	case KindTimeout:
		return "timeout"
	case KindUnsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

// APIError 带分类的 API 错误。
type APIError struct {
	Kind       Kind
	StatusCode int
	Message    string
	Op         string // 操作名（images.generations / videos.generations …）
}

func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s 失败 (HTTP %d, %s): %s", e.Op, e.StatusCode, e.Kind, e.Message)
	}
	return fmt.Sprintf("%s 失败 (%s): %s", e.Op, e.Kind, e.Message)
}

// IsFatal 该错误是否应立即中止整批（继续调用只会继续烧钱或继续失败）。
func IsFatal(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Kind == KindAuth || ae.Kind == KindNoFund
	}
	return false
}

// IsNoFund 是否余额/配额不足。
func IsNoFund(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Kind == KindNoFund
}

// ClassifyHTTP 按状态码与响应体分类错误。
func ClassifyHTTP(op string, status int, body string) *APIError {
	low := strings.ToLower(body)
	kind := KindUnknown
	msg := strings.TrimSpace(body)

	switch {
	case status == 401 || status == 403 ||
		containsAny(low, "invalid api key", "unauthorized", "authentication", "鉴权", "密钥无效"):
		kind = KindAuth
	case containsAny(low, "余额", "欠费", "insufficient balance", "no available resource", "quota exceeded", "billing", "无可用资源包"):
		kind = KindNoFund
	case status == 429 || containsAny(low, "rate limit", "too many requests", "限流", "请求过快"):
		// 智谱的 429 同时用于"限流"和"余额不足"，必须先判余额（上面已判）
		kind = KindRateLimit
	case status == 404 || containsAny(low, "model not found", "not support", "不支持", "unknown model"):
		kind = KindUnsupported
	case status == 400 || status == 422:
		kind = KindBadRequest
	case status >= 500:
		kind = KindNetwork
	case containsAny(low, "timeout", "deadline exceeded"):
		kind = KindTimeout
	}
	if msg == "" {
		msg = "(空响应体)"
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return &APIError{Kind: kind, StatusCode: status, Message: msg, Op: op}
}

// ClassifyTransport 分类传输层错误（无 HTTP 状态码）。
func ClassifyTransport(op string, err error) *APIError {
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	kind := KindNetwork
	if containsAny(low, "timeout", "deadline exceeded", "context deadline") {
		kind = KindTimeout
	}
	return &APIError{Kind: kind, Message: err.Error(), Op: op}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 成本记账
// ---------------------------------------------------------------------------

// CostKind 花费类别。
const (
	CostText      = "text"
	CostVision    = "vision"
	CostTranslate = "translate"
	CostImage     = "image"
	CostVideo     = "video"
	CostTTS       = "tts"
	CostMusic     = "music"
)

// PriceBook 单价表（元）。由 config 注入，AI 包不关心具体数值来源。
type PriceBook struct {
	Text      Price
	Vision    Price
	Translate Price
	Image     Price
	Video     Price
	TTS       Price
}

// Price 计费单价（元）。
type Price struct {
	PromptPer1K     float64
	CompletionPer1K float64
	ImagePerShot    float64
	VideoPerSec     float64
	TTSPer1KChars   float64
	MusicPerCall    float64
}

// Spend 按类别的用量与金额。
type Spend struct {
	Calls int     `json:"calls"`
	Units float64 `json:"units"` // 秒/张/字符/token 合计，按类别含义不同
	CNY   float64 `json:"cny"`
}

// ledger 花费账本（并发安全）。
type ledger struct {
	mu     sync.Mutex
	byKind map[string]*Spend
	total  float64
}

func newLedger() *ledger { return &ledger{byKind: map[string]*Spend{}} }

func (l *ledger) add(kind string, units, cny float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.byKind[kind]
	if s == nil {
		s = &Spend{}
		l.byKind[kind] = s
	}
	s.Calls++
	s.Units += units
	s.CNY += cny
	l.total += cny
}

func (l *ledger) snapshot() (map[string]Spend, float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Spend, len(l.byKind))
	for k, v := range l.byKind {
		out[k] = *v
	}
	return out, l.total
}

func (l *ledger) totalCNY() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// recordTokens 记录 LLM token 花费。
func (c *Client) recordTokens(kind string, p Price, u *usageInfo) {
	if u == nil {
		return
	}
	cny := float64(u.PromptTokens)/1000*p.PromptPer1K + float64(u.CompletionTokens)/1000*p.CompletionPer1K
	c.ledger.add(kind, float64(u.PromptTokens+u.CompletionTokens), cny)
}

// recordMedia 记录媒体调用花费（出图按张、视频按秒、TTS 按字符）。
func (c *Client) recordMedia(kind string, units, cny float64) {
	c.ledger.add(kind, units, cny)
}

// Spend 返回当前花费快照（按类别 + 总额）。
func (c *Client) Spend() (map[string]Spend, float64) { return c.ledger.snapshot() }

// TotalSpend 当前累计花费（元）。
func (c *Client) TotalSpend() float64 { return c.ledger.totalCNY() }

// EstimateVideoCost 渲染前成本预估：视频按秒、出图按张、TTS 按字符、LLM 按经验值。
// 预估值必须**在付费调用之前**给出，这是用户决定"要不要跑"的唯一依据。
func (c *Client) EstimateVideoCost(shots int, totalSec float64, ttsChars int, llmCalls int) CostEstimate {
	est := CostEstimate{}
	if p, ok := c.priceFor(CostVideo); ok {
		est.VideoCNY = totalSec * p.VideoPerSec
	}
	if p, ok := c.priceFor(CostImage); ok {
		est.ImageCNY = float64(shots*2) * p.ImagePerShot // 含角色参考帧
	}
	if p, ok := c.priceFor(CostTTS); ok {
		est.TTSCNY = float64(ttsChars) / 1000 * p.TTSPer1KChars
	}
	if p, ok := c.priceFor(CostText); ok {
		est.LLMCNY = float64(llmCalls) * 3000 / 1000 * (p.PromptPer1K + p.CompletionPer1K)
	}
	est.TotalCNY = est.VideoCNY + est.ImageCNY + est.TTSCNY + est.LLMCNY
	est.BudgetCNY = c.budget
	est.SpentCNY = c.TotalSpend()
	return est
}

// CostEstimate 成本预估结果。
type CostEstimate struct {
	ImageCNY  float64 `json:"image_cny"`
	VideoCNY  float64 `json:"video_cny"`
	TTSCNY    float64 `json:"tts_cny"`
	LLMCNY    float64 `json:"llm_cny"`
	TotalCNY  float64 `json:"total_cny"`
	SpentCNY  float64 `json:"spent_cny"`
	BudgetCNY float64 `json:"budget_cny"`
}

// String 打印预估明细（渲染前必须展示）。
func (e CostEstimate) String() string {
	s := fmt.Sprintf("预估花费：出图 %.2f + 视频 %.2f + 配音 %.2f + 文本 %.2f = %.2f 元",
		e.ImageCNY, e.VideoCNY, e.TTSCNY, e.LLMCNY, e.TotalCNY)
	if e.BudgetCNY > 0 {
		s += fmt.Sprintf("（已花费 %.2f / 预算 %.2f 元）", e.SpentCNY, e.BudgetCNY)
	} else if e.SpentCNY > 0 {
		s += fmt.Sprintf("（本次会话已花费 %.2f 元）", e.SpentCNY)
	}
	return s
}

// OverBudget 是否已超出预算。超预算时所有付费调用都会拒绝执行。
func (c *Client) OverBudget() bool {
	if c.budget <= 0 {
		return false
	}
	return c.ledger.totalCNY()+c.inflight() >= c.budget
}

// inflight 当前在途预留（已提交未过账的预估花费）。
func (c *Client) inflight() float64 {
	c.shared.mu.Lock()
	defer c.shared.mu.Unlock()
	return c.shared.inflight
}

// guardBudget 付费调用前的预算门禁。宁可拒绝调用也不要静默超支。
// 判定口径 = 已过账花费 + 在途预留：并发 worker 提交任务与花费过账之间
// 有时间差，不算在途就等于 N 个 worker 同时过闸（check-then-act 超卖）。
func (c *Client) guardBudget(op string) error {
	return c.reserve(op, 0)
}

// reserve 预算门禁 + 在途预留。est>0 时把预估花费记为"在途"参与判定，
// 调用完成后必须 settle 释放（实际花费由 recordMedia/recordTokens 单点过账）。
// 返回 error 时调用方必须放弃本次付费调用。
func (c *Client) reserve(op string, est float64) error {
	if c.budget <= 0 {
		return nil
	}
	c.shared.mu.Lock()
	defer c.shared.mu.Unlock()
	spent := c.ledger.totalCNY()
	committed := spent + c.shared.inflight
	if spent >= c.budget ||
		(est > 0 && committed+est > c.budget) || // 本次调用需要真实额度余量
		(est == 0 && committed >= c.budget) { // 在途预留已吃满预算：不再加新调用
		return &APIError{
			Kind: KindNoFund, Op: op, StatusCode: 0,
			Message: fmt.Sprintf("已达预算上限 %.2f 元（已花费 %.2f 元，在途 %.2f 元）：停止付费调用以保护账户，调整 budget.max_cost_cny 或充值后 resume 续跑",
				c.budget, spent, c.shared.inflight),
		}
	}
	if est > 0 {
		c.shared.inflight += est
	}
	// 接近上限时告警一次（跨全部派生客户端去重）
	if !c.shared.warned && committed >= c.budget*c.warnRatio {
		c.shared.warned = true
		fmt.Printf("  ⚠ 花费已达预算的 %.0f%%（%.2f / %.2f 元）\n", committed/c.budget*100, committed, c.budget)
	}
	return nil
}

// settle 结算预留：退还在途预估。无论调用成败都必须调用，否则预留泄漏
// 会把后续调用永久挡在预算门外。实际花费由 ledger 单点过账，这里不记账。
func (c *Client) settle(est float64) {
	if c.budget <= 0 || est <= 0 {
		return
	}
	c.shared.mu.Lock()
	c.shared.inflight -= est
	if c.shared.inflight < 0 {
		c.shared.inflight = 0
	}
	c.shared.mu.Unlock()
}

// reservePost 带预算预留的媒体请求：预留 → 请求 → 无论成败结算预留。
// est 为本次调用的预估花费（0 = 只做门禁检查不预留）。
func (c *Client) reservePost(op, path string, body []byte, est float64) ([]byte, error) {
	if err := c.reserve(op, est); err != nil {
		return nil, err
	}
	data, err := c.post(path, body)
	c.settle(est)
	return data, err
}

// estImageCost 单次出图的预估花费（未配单价返回 0 = 不预留只检查）。
func (c *Client) estImageCost() float64 {
	if p, ok := c.priceFor(CostImage); ok {
		return p.ImagePerShot
	}
	return 0
}

// estVideoCost 一段视频的预估花费（按请求档位时长估算，实测后由 ledger 校正）。
func (c *Client) estVideoCost(durationSec float64) float64 {
	if durationSec <= 0 {
		durationSec = 5
	}
	if p, ok := c.priceFor(CostVideo); ok {
		return durationSec * p.VideoPerSec
	}
	return 0
}

// estTTSCost 单句合成的预估花费。
func (c *Client) estTTSCost(chars int) float64 {
	if p, ok := c.priceFor(CostTTS); ok {
		return float64(chars) / 1000 * p.TTSPer1KChars
	}
	return 0
}

// SetBudget 设置预算上限（来自 config）。
func (c *Client) SetBudget(cny float64) { c.budget = cny }

// priceFor 取单价；未配置时返回 false（调用方按零价处理，不阻塞但统计偏低）。
func (c *Client) priceFor(kind string) (Price, bool) {
	if c.prices == nil {
		return Price{}, false
	}
	p, ok := c.prices[kind]
	return p, ok
}
