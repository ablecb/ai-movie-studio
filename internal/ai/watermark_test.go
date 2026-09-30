package ai

import (
	"encoding/json"
	"testing"
)

// 回归测试：去水印必须"显式传 false"，不能靠"不传字段"。
//
// 背景（官方 OpenAPI spec）：图片与视频请求的 watermark_enabled 默认值是 true
// ——"默认启用 AI 生成的显式水印及隐式数字水印"。也就是说省略该字段等于加水印。
// 成片带可见角标是交付事故，且参考帧的水印会作为参考图/首帧一路传下去变成运动鬼影，
// 所以这里把"请求体里到底有没有 watermark_enabled=false"钉成断言。
func TestImageBodyDisablesWatermark(t *testing.T) {
	body := ExportImageBody("a serene mountain landscape", ImageOptions{
		Model: "cogview-4", Size: "1280x720", Quality: "hd",
	}, "", "cogview-4")

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, body)
	}

	v, ok := got["watermark_enabled"]
	if !ok {
		t.Fatalf("出图请求缺少 watermark_enabled 字段；服务端默认 true，等于全片带水印: %s", body)
	}
	if v != false {
		t.Errorf("watermark_enabled 应为 false，实际 %#v", v)
	}
	if got["quality"] != "hd" {
		t.Errorf("quality 应透传 hd，实际 %#v", got["quality"])
	}
}

func TestVideoBodyDisablesWatermark(t *testing.T) {
	body := ExportVideoBody("slow dolly in", "data:image/png;base64,AAA", VideoOptions{
		Quality: "quality", Size: "1920x1080", FPS: 30, Duration: 10,
	}, "cogvideox-3")

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, body)
	}

	v, ok := got["watermark_enabled"]
	if !ok {
		t.Fatalf("视频请求缺少 watermark_enabled 字段；服务端默认 true，等于全片带水印: %s", body)
	}
	if v != false {
		t.Errorf("watermark_enabled 应为 false，实际 %#v", v)
	}
	// duration 只有显式设置时才发：智谱只接受 5/10，传其它值会被拒
	if got["duration"] != float64(10) {
		t.Errorf("duration 应透传 10，实际 %#v", got["duration"])
	}
}

// 降级路径（去掉参考图重试）也必须保留去水印设置——
// 这条路径是重新构造请求体的，最容易漏字段。
func TestImageBodyKeepsWatermarkOffOnRefFallback(t *testing.T) {
	opt := ImageOptions{Model: "cogview-4", Size: "1024x1024", Watermark: false}

	withRef := ExportImageBody("p", opt, "data:image/png;base64,AAA", "cogview-4")
	withoutRef := ExportImageBody("p", opt, "", "cogview-4")

	for name, body := range map[string][]byte{"带参考图": withRef, "降级去掉参考图": withoutRef} {
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: 请求体不是合法 JSON: %v", name, err)
		}
		if got["watermark_enabled"] != false {
			t.Errorf("%s: watermark_enabled 应为 false，实际 %#v", name, got["watermark_enabled"])
		}
	}

	var got map[string]any
	_ = json.Unmarshal(withoutRef, &got)
	if _, ok := got["image"]; ok {
		t.Error("降级路径不应再带 image 字段")
	}
}

// 显式要求保留水印时不应发 watermark_enabled=false（该场景仅用于对照/调试）。
func TestImageBodyExplicitWatermarkOmitsFlag(t *testing.T) {
	body := ExportImageBody("p", ImageOptions{Model: "cogview-4", Watermark: true}, "", "cogview-4")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if _, ok := got["watermark_enabled"]; ok {
		t.Error("Watermark=true 时不应发送 watermark_enabled（留给服务端默认 true）")
	}
}

// 未设置 duration 时不能凭空发一个 0——智谱只接受 5/10，0 会被拒。
func TestVideoBodyOmitsDurationWhenUnset(t *testing.T) {
	body := ExportVideoBody("p", "data:image/png;base64,AAA", VideoOptions{Quality: "speed"}, "cogvideox-3")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if _, ok := got["duration"]; ok {
		t.Errorf("未设置 duration 时不应发送该字段，实际 %#v", got["duration"])
	}
}
