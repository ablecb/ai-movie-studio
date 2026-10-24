package config

import (
	"strings"
	"testing"
)

// 视觉模型白名单必须按服务商分家。
//
// 为什么值得一个测试：配错视觉模型不会让任何步骤报错——deliver 探活失败后
// 只打印一行警告就继续交付，于是"水印复核"这类只能靠看图发现的检查会被静默跳过。
// 也就是说这个错误的表现形式是"看起来一切正常"，必须在开工前的自检里拦住。
func TestValidateRejectsVisionModelWithoutImageSupport(t *testing.T) {
	cfg := Default()
	cfg.VisionReview.Enabled = true
	cfg.Models.Vision = "glm-4v-plus" // 历史默认值，已不在支持列表内

	err := cfg.Validate()
	if err == nil {
		t.Fatal("视觉模型不支持图片输入时必须自检失败，而不是等到交付时静默跳过复核")
	}
	if !strings.Contains(err.Error(), "glm-4v-plus") {
		t.Errorf("错误信息应包含出错的模型名，实际: %v", err)
	}
}

func TestValidateAcceptsSupportedVisionModelsPerProvider(t *testing.T) {
	// 智谱端点
	zhipu := Default()
	zhipu.VisionReview.Enabled = true
	zhipu.Models.Vision = "glm-5.3-flash"
	if err := zhipu.Validate(); err != nil {
		t.Errorf("智谱视觉模型 glm-5.3-flash 应通过自检: %v", err)
	}

	// DeepSeek 端点：同一个模型名不在它的白名单里
	ds := Default()
	ds.VisionReview.Enabled = true
	ds.Providers.Vision.BaseURL = "https://api.deepseek.com"
	ds.Providers.Vision.APIKeyEnv = "DEEPSEEK_API_KEY"
	ds.Models.Vision = "deepseek-flash"
	if err := ds.Validate(); err != nil {
		t.Errorf("DeepSeek 视觉模型 deepseek-flash 应通过自检: %v", err)
	}

	// 反向：DeepSeek 端点配智谱模型名必须被拦下
	ds.Models.Vision = "glm-5.3-flash"
	if err := ds.Validate(); err == nil {
		t.Error("DeepSeek 端点配 glm 模型应被拦下（该端点不认这个模型名）")
	}
}

// 未启用视觉复核时不应因为视觉模型名而拦住整条流水线——
// 用户可能只想跑创作/渲染，此时视觉模型根本不会被调用。
func TestValidateIgnoresVisionModelWhenReviewDisabled(t *testing.T) {
	cfg := Default()
	cfg.VisionReview.Enabled = false
	cfg.Models.Vision = "some-legacy-name"
	if err := cfg.Validate(); err != nil {
		t.Errorf("未启用视觉复核时不应因视觉模型名报错: %v", err)
	}
}

// 默认配置必须开箱可用：默认值指向的视觉模型要是真实存在的。
func TestDefaultConfigIsSelfConsistent(t *testing.T) {
	cfg := Default()
	cfg.VisionReview.Enabled = true
	if !cfg.VisionModelSupported() {
		t.Errorf("默认视觉模型 %q 不在支持列表内，默认配置本身就不可用", cfg.Models.Vision)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("默认配置应通过自检: %v", err)
	}
	// 首尾帧锁定是默认能力：每镜视频都带尾帧锚点（分镜构图）。
	// 默认关会让"衔接不自然"的回归悄悄发生——这里把它钉死。
	if !cfg.Video.EndFrameAnchor {
		t.Error("尾帧锚点应默认开启（每镜首尾帧锁定）")
	}
	if !cfg.Video.ChainSameScene {
		t.Error("链式衔接应默认开启")
	}
}
