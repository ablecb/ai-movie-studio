// Package skill 定义工作流技能（Skill）的接口与注册表。
// 每个制作环节（剧本、分镜、审片……）都是一个 Skill。
package skill

import (
	"encoding/json"
	"fmt"
	"sync"

	"aistudio/internal/ai"
	"aistudio/internal/config"
)

// Context 一次技能执行的上下文
type Context struct {
	RunID   string
	WorkDir string // 当前 run 的目录（runs/<id>）
	Brief   string // 用户的创作意图 / 故事梗概

	// Params: 阶段参数 + 引擎注入（如 revision 次数、审片返修意见）
	Params map[string]any

	// Deps: 依赖阶段的输出（key = 阶段 ID）
	Deps map[string]json.RawMessage

	AI *ai.Client
	// Cfg 生产参数（媒体生成/质检阈值/预算）。纯 LLM 创作阶段可忽略；
	// 媒体类技能（素材生成等）必须依赖它，而不是自己猜配置。
	Cfg     *config.Config
	Lessons []string // 从经验记忆库检索到的相关教训，注入提示词

	Log func(format string, args ...any)
}

// Param helpers ----------------------------------------------------------------

// ParamStr 读取字符串参数，缺省返回 def
func (c *Context) ParamStr(key, def string) string {
	if v, ok := c.Params[key].(string); ok && v != "" {
		return v
	}
	return def
}

// ParamInt 读取整数参数
func (c *Context) ParamInt(key string, def int) int {
	if v, ok := c.Params[key].(float64); ok {
		return int(v)
	}
	if v, ok := c.Params[key].(int); ok {
		return v
	}
	return def
}

// ParamFloat 读取浮点参数
func (c *Context) ParamFloat(key string, def float64) float64 {
	if v, ok := c.Params[key].(float64); ok && v > 0 {
		return v
	}
	return def
}

// DepOf 获取依赖阶段输出并解码
func (c *Context) DepOf(stageID string, out any) error {
	raw, ok := c.Deps[stageID]
	if !ok {
		return fmt.Errorf("缺少依赖阶段 %s 的输出", stageID)
	}
	return json.Unmarshal(raw, out)
}

// Skill 技能接口：输入上下文，输出可 JSON 序列化的产物
type Skill interface {
	Name() string
	Description() string
	Run(ctx *Context) (output any, err error)
}

// Registry 技能注册表
type Registry struct {
	mu sync.RWMutex
	m  map[string]Skill
}

// NewRegistry 创建注册表
func NewRegistry() *Registry { return &Registry{m: map[string]Skill{}} }

// Register 注册技能
func (r *Registry) Register(s Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[s.Name()] = s
}

// Get 按名称取技能
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.m[name]
	return s, ok
}

// Names 已注册技能名列表
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.m))
	for k := range r.m {
		names = append(names, k)
	}
	return names
}
