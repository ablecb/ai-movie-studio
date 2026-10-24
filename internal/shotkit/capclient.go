package shotkit

import (
	"fmt"

	"aistudio/internal/ai"
	"aistudio/internal/config"
)

// CapClient 派生面向某能力的客户端：端点取 cfg.Providers 的对应配置
// （normalize 已把空字段补齐到顶层），密钥走 cfg.APIKeyFor 的统一口径。
//
// base 只用于继承计费/预算/批次上下文——ai.Client.For 让所有派生客户端
// **共享同一花费账本**，因此文本/图像/视频可以分别指向不同供应商而账目仍然统一。
//
// 严格模式：显式配了独立端点却拿不到密钥时，For() 会静默退回离线模式——
// 这里把它变成显式错误，拒绝"配了等于没配"。全局 mock（离线演示）不受影响：
// 那是显式模式，不是降级。
func CapClient(base *ai.Client, cfg *config.Config, capability ai.Capability, ep config.Endpoint, model string) (*ai.Client, error) {
	// 独立端点（自带密钥环境变量且与顶层不同）必须解析出自己的密钥。
	// 不能只依赖 For() 的兜底：For 只在覆盖值非空时才写入，空值会**继承基础
	// 客户端的顶层密钥**——等于把智谱的 key 发给另一个供应商（401 且账单错位）。
	independent := ep.APIKeyEnv != "" && ep.APIKeyEnv != cfg.APIKeyEnv
	key := cfg.APIKeyFor(ep)
	if independent && key == "" && !cfg.Mock {
		return nil, fmt.Errorf("providers.%s 指向独立端点 %s 但缺少密钥（环境变量 %s）", capability, ep.BaseURL, ep.APIKeyEnv)
	}
	c := base.For(capability, ai.Endpoint{
		BaseURL: ep.BaseURL, APIKeyEnv: ep.APIKeyEnv,
		APIKey: key, Model: model,
	})
	if c.Mock() && !cfg.Mock {
		return nil, fmt.Errorf("providers.%s 指向端点 %s 但不可用（无可用密钥）", capability, ep.BaseURL)
	}
	return c, nil
}
