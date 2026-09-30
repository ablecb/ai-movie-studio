// Package ai 提供 OpenAI 兼容的 Chat 客户端。
// 支持两种模式：
//   - real: 调用真实大模型（GLM / OpenAI / 任意兼容网关），通过环境变量提供密钥
//   - mock: 无密钥时的离线演示模式，返回确定性的结构化输出
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config 客户端配置（来自 config.json + config.local.json + 环境变量）
type Config struct {
	BaseURL   string `json:"base_url"`    // 例如 https://open.bigmodel.cn/api/paas/v4
	APIKeyEnv string `json:"api_key_env"` // 读取哪个环境变量作为密钥，如 AI_API_KEY
	// APIKey 直接给定的密钥（来自 config.local.json，不入库）。
	// 解析顺序见 Client.Key：环境变量优先，其次本地层。
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"` // 例如 glm-4.6
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	Mock        bool    `json:"mock"` // true 时启用离线演示模式
}

// Key 解析本次调用要用的密钥。环境变量优先于本地配置文件，
// 这样 CI 与临时覆盖永远能压过磁盘上的值。
func (c Config) Key() string {
	if c.APIKeyEnv != "" {
		if v := os.Getenv(c.APIKeyEnv); v != "" {
			return v
		}
	}
	return strings.TrimSpace(c.APIKey)
}

// HasKey 是否具备真实调用条件。
func (c Config) HasKey() bool { return c.Key() != "" }

// CallOpts 单次调用的采样参数覆盖；零值字段回落到全局配置。
// 结构化产出（分镜/提示词/审片）低温防抖动——JSON 解析失败的重试意味着双倍 token；
// 创作类（剧本）保留高温保证多样性。
type CallOpts struct {
	Temperature float64
	MaxTokens   int
}

// TokenStat 一次运行的 token 用量（按调用名聚合，用于成本核算）
type TokenStat struct {
	Calls            int   `json:"calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// clientShared 跨派生客户端共享的可变状态。
// For() 派生出的能力客户端（视觉/媒体/音乐）与母客户端共用同一份 usage、
// 预算在途预留与批次上下文——它们必须共用同一把锁，否则并发记账就是数据竞争。
type clientShared struct {
	mu    sync.Mutex
	usage map[string]*TokenStat
	// warned 80% 预算告警只发一次（跨全部派生客户端去重）
	warned bool
	// inflight 已提交未过账的预估花费（预算预留）：并发 worker 提交付费任务
	// 与花费实际过账之间有时间差，不预留就等于 N 个 worker 能同时过闸击穿预算
	inflight float64
	// parent 批次级上下文：取消时正在排队的付费调用一并终止
	parent context.Context
}

// Client AI 客户端
type Client struct {
	cfg  Config
	http *http.Client
	// shared 派生客户端间共享的可变状态（见 clientShared）
	shared *clientShared

	// 成本与预算：所有付费调用（含媒体）都从这里过账，支持运行中熔断。
	// budget/warnRatio 是装配期写入的配置值，运行期只读。
	ledger    *ledger
	prices    map[string]Price
	budget    float64
	warnRatio float64
}

// New 根据配置创建客户端。若配置了密钥环境变量且存在，则自动使用真实模式。
func New(cfg Config) *Client {
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "AI_API_KEY"
	}
	if cfg.HasKey() {
		cfg.Mock = false
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.Temperature <= 0 {
		cfg.Temperature = 0.8
	}
	return &Client{
		cfg:       cfg,
		http:      &http.Client{Timeout: 600 * time.Second},
		shared:    &clientShared{usage: map[string]*TokenStat{}, parent: context.Background()},
		ledger:    newLedger(),
		prices:    map[string]Price{},
		warnRatio: 0.8,
	}
}

// SetPrices 注入单价表（来自 config）。必须在任何付费调用之前调用。
func (c *Client) SetPrices(p map[string]Price) {
	if p == nil {
		return
	}
	c.prices = p
}

// SetWarnRatio 用量告警阈值。
func (c *Client) SetWarnRatio(r float64) {
	if r > 0 && r <= 1 {
		c.warnRatio = r
	}
}

// SetParentContext 设置批次级上下文；取消会让所有后续请求立即返回，
// 是"用户按 Ctrl-C 不该继续烧钱"的实现基础。
// 写入共享状态：无论派生客户端先于还是晚于本调用创建，取消都对其生效。
func (c *Client) SetParentContext(ctx context.Context) {
	if ctx == nil {
		return
	}
	c.shared.mu.Lock()
	c.shared.parent = ctx
	c.shared.mu.Unlock()
}

// Context 返回批次级上下文（供媒体长任务继承）。
func (c *Client) Context() context.Context {
	c.shared.mu.Lock()
	defer c.shared.mu.Unlock()
	if c.shared.parent == nil {
		return context.Background()
	}
	return c.shared.parent
}

// Mock 是否为离线演示模式
func (c *Client) Mock() bool { return c.cfg.Mock }

// ModelName 当前使用的模型名
func (c *Client) ModelName() string { return c.cfg.Model }

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usageInfo struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *usageInfo `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat 发起一次对话（使用全局默认采样参数）。callName 用于用量记账与日志标识。
func (c *Client) Chat(callName, system, user string) (string, error) {
	return c.ChatOpts(callName, system, user, CallOpts{})
}

// ChatOpts 发起一次对话，按调用覆盖采样参数并记录 token 用量。
func (c *Client) ChatOpts(callName, system, user string, opts CallOpts) (string, error) {
	return c.ChatModel(c.ModelName(), callName, system, user, opts)
}

// ChatModel 指定模型发起对话。模型分层（创作/结构化/翻译/视觉）都走这里，
// 保证无论用哪个模型，预算门禁、错误分类、用量记账都一致生效。
func (c *Client) ChatModel(model, callName, system, user string, opts CallOpts) (string, error) {
	if c.cfg.Mock {
		return MockRespond(callName), nil
	}
	if err := c.guardBudget("chat:" + callName); err != nil {
		return "", err
	}
	if model == "" {
		model = c.cfg.Model
	}
	temp, maxTokens := opts.Temperature, opts.MaxTokens
	if temp <= 0 {
		temp = c.cfg.Temperature
	}
	if maxTokens <= 0 {
		maxTokens = c.cfg.MaxTokens
	}
	reqBody, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: temp,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(c.Context(), http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", ClassifyTransport("chat:"+callName, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", ClassifyTransport("chat:"+callName, readErr)
	}
	if resp.StatusCode >= 300 {
		return "", ClassifyHTTP("chat:"+callName, resp.StatusCode, string(body))
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("解析响应失败 (HTTP %d): %w", resp.StatusCode, err)
	}
	if cr.Error != nil {
		return "", ClassifyHTTP("chat:"+callName, resp.StatusCode, cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", ClassifyHTTP("chat:"+callName, resp.StatusCode, "响应中没有 choices（可能被内容审核拦截）")
	}
	c.recordUsage(callName, cr.Usage)
	c.recordTokens(costKindFor(callName), c.priceOf(costKindFor(callName)), cr.Usage)
	return cr.Choices[0].Message.Content, nil
}

// costKindFor 把调用名映射到计费类别，保证视觉/翻译走各自的单价。
func costKindFor(callName string) string {
	switch callName {
	case "vision", "review.visual":
		return CostVision
	case "translate":
		return CostTranslate
	default:
		return CostText
	}
}

// priceOf 取单价（内部用，无记录时返回零价）。
func (c *Client) priceOf(kind string) Price {
	p, _ := c.priceFor(kind)
	return p
}

func (c *Client) recordUsage(callName string, u *usageInfo) {
	c.shared.mu.Lock()
	defer c.shared.mu.Unlock()
	st := c.shared.usage[callName]
	if st == nil {
		st = &TokenStat{}
		c.shared.usage[callName] = st
	}
	st.Calls++
	st.PromptTokens += u.PromptTokens
	st.CompletionTokens += u.CompletionTokens
}

// UsageSnapshot 返回用量快照（调用方持有副本，可安全序列化）
func (c *Client) UsageSnapshot() map[string]TokenStat {
	c.shared.mu.Lock()
	defer c.shared.mu.Unlock()
	out := make(map[string]TokenStat, len(c.shared.usage))
	for k, v := range c.shared.usage {
		out[k] = *v
	}
	return out
}

var jsonBlockRe = regexp.MustCompile(`(?s)\{.*\}`)

// ExtractJSON 从模型输出中提取第一个 JSON 对象（模型常带 markdown 代码围栏）。
func ExtractJSON(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			return []byte(text[i : j+1]), nil
		}
	}
	if jsonBlockRe.MatchString(text) {
		return []byte(jsonBlockRe.FindString(text)), nil
	}
	return nil, fmt.Errorf("输出中未找到 JSON 对象")
}

// ---------- 通用 HTTP 助手（图像/视频等媒体 API 共用） ----------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(method, path string, body []byte) ([]byte, error) {
	if err := c.guardBudget(method + " " + path); err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.Context(), method, url, rd)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ClassifyTransport(method+" "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ClassifyTransport(method+" "+path, err)
	}
	if resp.StatusCode >= 300 {
		var ae struct {
			Error *apiError `json:"error"`
		}
		if json.Unmarshal(data, &ae) == nil && ae.Error != nil {
			return nil, ClassifyHTTP(path, resp.StatusCode, ae.Error.Message)
		}
		return nil, ClassifyHTTP(path, resp.StatusCode, string(data))
	}
	return data, nil
}

func (c *Client) post(path string, body []byte) ([]byte, error) {
	return c.do(http.MethodPost, path, body)
}

func (c *Client) get(path string) ([]byte, error) {
	return c.do(http.MethodGet, path, nil)
}

func truncateForLog(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
