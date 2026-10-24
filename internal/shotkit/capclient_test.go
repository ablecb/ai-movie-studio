package shotkit

import (
	"strings"
	"testing"

	"aistudio/internal/ai"
	"aistudio/internal/config"
)

// CapClient 的能力路由与缺密钥阻塞——严格模式的核心保证：
// "配了独立端点就必须真的用、且必须可用"。
func TestCapClientRoutesAndBlocksKeyless(t *testing.T) {
	cfg := config.Default()
	cfg.Mock = false
	cfg.BaseURL = "https://top.example/v1"
	cfg.APIKeyEnv = "STUDIO_TEST_TOP_KEY" // 该变量不会真的设置：密钥只从字面量取
	cfg.APIKeyLiteral = "top-key"
	cfg.Model = "top-model"
	base := ai.New(ai.Config{
		BaseURL: cfg.BaseURL, APIKeyEnv: cfg.APIKeyEnv, APIKey: cfg.APIKey(),
		Model: cfg.Model, Mock: cfg.Mock,
	})

	// 1) providers.image 指向独立端点且带密钥 → 请求真的路由到该端点与模型
	cfg.Providers.Image = config.Endpoint{
		BaseURL: "https://img.example/v1", APIKeyEnv: "STUDIO_TEST_IMG_KEY",
		APIKeyLiteral: "img-key", Model: "img-model",
	}
	c, err := CapClient(base, cfg, ai.CapImage, cfg.Providers.Image, "img-model")
	if err != nil {
		t.Fatalf("独立端点 + 密钥应派生成功: %v", err)
	}
	if info := c.EndpointInfo(); !strings.Contains(info, "img.example") || !strings.Contains(info, "img-model") {
		t.Fatalf("未路由到出图端点: %s", info)
	}

	// 2) 独立端点但无密钥 → 拒绝（不允许静默退回离线模式）
	cfg.Providers.Image = config.Endpoint{BaseURL: "https://img.example/v1", APIKeyEnv: "STUDIO_TEST_IMG_KEY_MISSING"}
	if _, err := CapClient(base, cfg, ai.CapImage, cfg.Providers.Image, "img-model"); err == nil {
		t.Fatal("独立端点缺密钥必须报错（严格模式）")
	}

	// 3) 全局离线演示（cfg.Mock=true）不受严格检查影响——那是显式模式，不是降级
	cfg.Mock = true
	if _, err := CapClient(base, cfg, ai.CapImage, cfg.Providers.Image, "img-model"); err != nil {
		t.Fatalf("mock 模式不应因缺密钥报错: %v", err)
	}
}
