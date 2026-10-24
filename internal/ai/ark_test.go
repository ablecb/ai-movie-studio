package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsArkEndpoint(t *testing.T) {
	for url, want := range map[string]bool{
		"https://ark.cn-beijing.volces.com":    true,
		"https://ARK.cn-beijing.volces.com/v3": true,
		"https://ark.example.com":              true,
		"https://open.bigmodel.cn/api/paas/v4": false,
		"https://api.deepseek.com":             false,
	} {
		if got := isArkEndpoint(url); got != want {
			t.Errorf("isArkEndpoint(%q) = %v, want %v", url, got, want)
		}
	}
}

// base_url 两种写法都要拼出正确路径：漏 /api/v3 会打到网关根路径，
// 只得到"空响应体"404（真实踩过的坑）。
func TestArkAPIPath(t *testing.T) {
	for _, c := range []struct{ base, want string }{
		{"https://ark.cn-beijing.volces.com/api/v3", "/images/generations"},
		{"https://ark.cn-beijing.volces.com/api/v3/", "/images/generations"},
		{"https://ark.cn-beijing.volces.com", "/api/v3/images/generations"},
	} {
		if got := arkAPIPath(c.base, "/images/generations"); got != c.want {
			t.Errorf("arkAPIPath(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

// 模型未开通时的报错要带可执行提示（客户端只透传 message，没有错误码）。
func TestArkModelHint(t *testing.T) {
	raw := errors.New("The model or endpoint doubao-seedream-4-0-250828 does not exist or you do not have access to it")
	got := arkModelHint(raw).Error()
	if !strings.Contains(got, "开通管理") {
		t.Fatalf("模型未开通的报错应带控制台开通提示: %s", got)
	}
	other := errors.New("rate limit exceeded")
	if arkModelHint(other).Error() != other.Error() {
		t.Fatal("其他错误不应被改写")
	}
}

// Seedream 出图请求体：去水印必须显式 false、参考图是 image、
// 不出现 Seedream 没有的字段（ref_strength/quality/watermark_enabled → 400）。
func TestArkImageBody(t *testing.T) {
	const ref = "data:image/png;base64,AAA"
	opt := ImageOptions{Size: "1248x704", Watermark: false, RefImage: ref, RefStrength: 0.6}
	var body map[string]any
	if err := json.Unmarshal(arkImageBody("prompt", opt, opt.RefImage, "doubao-seedream-4-0-250828"), &body); err != nil {
		t.Fatal(err)
	}
	if body["watermark"] != false {
		t.Fatalf("去水印必须显式写 watermark=false: %v", body["watermark"])
	}
	for _, absent := range []string{"ref_strength", "quality", "watermark_enabled"} {
		if _, ok := body[absent]; ok {
			t.Fatalf("Seedream 请求体不应出现 %s（未知字段会 400）", absent)
		}
	}
	if body["image"] != ref {
		t.Fatalf("参考图应放在 image 字段: %v", body["image"])
	}
	if body["size"] != "2496x1408" {
		t.Fatalf("低于像素下限的尺寸应被翻译到合法区间: %v", body["size"])
	}
	if body["response_format"] != "url" {
		t.Fatalf("应显式要 url: %v", body["response_format"])
	}
}

func TestArkImageSize(t *testing.T) {
	for in, want := range map[string]string{
		"2K":        "2K",
		"1.5k":      "1.5K",
		"1024x1024": "1024x1024", // 1.05M ≥ 下限：原样
		"1248x704":  "2496x1408", // 87.9 万 < 下限：等比放大
		"5120x2880": "2560x1440", // 超单边上限：缩回
	} {
		if got := arkImageSize(in); got != want {
			t.Errorf("arkImageSize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Seedance 任务请求体：参数是顶层 JSON 字段；首帧无 role、尾帧 role=last_frame。
func TestArkVideoTaskBody(t *testing.T) {
	const first, last = "data:image/png;base64,AAA", "data:image/png;base64,BBB"
	opt := VideoOptions{
		Duration: 10, Size: "1920x1080", Watermark: false,
		CameraFixed: true, EndImage: last,
	}
	var body map[string]any
	if err := json.Unmarshal(arkVideoTaskBody("p", first, opt, "doubao-seedance-1-0-pro-250528"), &body); err != nil {
		t.Fatal(err)
	}
	if d, _ := body["duration"].(float64); d != 10 {
		t.Fatalf("duration 应为顶层字段: %v", body["duration"])
	}
	if body["resolution"] != "1080p" || body["ratio"] != "16:9" {
		t.Fatalf("分辨率/画幅推导错误: res=%v ratio=%v", body["resolution"], body["ratio"])
	}
	if body["watermark"] != false || body["camera_fixed"] != true {
		t.Fatalf("watermark/camera_fixed 缺失: %v", body)
	}
	if _, ok := body["generate_audio"]; ok {
		t.Fatal("1.x 系不认识 generate_audio，发了会 400")
	}
	content, _ := body["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("应为 [文本, 首帧, 尾帧]: %v", content)
	}
	firstItem, _ := content[1].(map[string]any)
	if _, hasRole := firstItem["role"]; hasRole {
		t.Fatalf("首帧不带 role（官方语义：无 role = 首帧）: %v", firstItem)
	}
	lastItem, _ := content[2].(map[string]any)
	if lastItem["role"] != "last_frame" {
		t.Fatalf("尾帧应标 role=last_frame: %v", lastItem)
	}
}

// 2.x 系才发 generate_audio；参考图路径只发 reference_image（不与首/尾帧混用），
// 并显式声明任务类型为 reference（不写会被模型按提示词自判，误判成编辑/延长会异步报错）。
func TestArkVideoTaskBodyReferenceImages(t *testing.T) {
	opt := VideoOptions{
		Duration: 5, Size: "1920x1080", Watermark: false,
		RefImages: []string{"data:image/png;base64,A", "data:image/png;base64,B"},
		EndImage:  "data:image/png;base64,C", // 参考图路径下应被忽略（不能混用）
	}
	var body map[string]any
	if err := json.Unmarshal(arkVideoTaskBody("p", "", opt, "doubao-seedance-2-0-260128"), &body); err != nil {
		t.Fatal(err)
	}
	if ga, ok := body["generate_audio"].(bool); !ok || ga {
		t.Fatalf("2.x 应显式关闭模型自带音频（本项目自己做后期）: %v", body["generate_audio"])
	}
	if body["omni_reference_task_type"] != "reference" {
		t.Fatalf("2.x 参考任务应显式声明 omni_reference_task_type=reference: %v", body["omni_reference_task_type"])
	}
	content, _ := body["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("应为 [文本, 参考图, 参考图]: %v", content)
	}
	for i, it := range content[1:] {
		item, _ := it.(map[string]any)
		if item["role"] != "reference_image" {
			t.Fatalf("第 %d 张参考图应标 role=reference_image: %v", i+1, item)
		}
	}

	// 1.x 没有该字段（未知字段 400）
	body1x := map[string]any{}
	if err := json.Unmarshal(arkVideoTaskBody("p", "", opt, "doubao-seedance-1-0-pro-250528"), &body1x); err != nil {
		t.Fatal(err)
	}
	if _, ok := body1x["omni_reference_task_type"]; ok {
		t.Fatal("1.x 系不认识 omni_reference_task_type，发了会 400")
	}
}

// 2.x 首帧/首尾帧任务：首帧显式标 role=first_frame，ratio 必须为 adaptive
//（显式画幅会触发 InvalidParameter.TaskTypeConstraint 异步报错）。
func TestArkVideoTaskBodyFirstLast2x(t *testing.T) {
	const first, last = "data:image/png;base64,AAA", "data:image/png;base64,BBB"
	opt := VideoOptions{
		Duration: 5, Size: "1920x1080", Watermark: false, EndImage: last,
	}
	var body map[string]any
	if err := json.Unmarshal(arkVideoTaskBody("p", first, opt, "doubao-seedance-2-5-260628"), &body); err != nil {
		t.Fatal(err)
	}
	if body["ratio"] != "adaptive" {
		t.Fatalf("2.x 首帧/首尾帧任务 ratio 必须为 adaptive: %v", body["ratio"])
	}
	if _, ok := body["omni_reference_task_type"]; ok {
		t.Fatal("首尾帧任务不是全模态参考任务，不应带 omni_reference_task_type")
	}
	content, _ := body["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("应为 [文本, 首帧, 尾帧]: %v", content)
	}
	firstItem, _ := content[1].(map[string]any)
	if firstItem["role"] != "first_frame" {
		t.Fatalf("2.x 首帧应显式标 role=first_frame: %v", firstItem)
	}
	lastItem, _ := content[2].(map[string]any)
	if lastItem["role"] != "last_frame" {
		t.Fatalf("尾帧应标 role=last_frame: %v", lastItem)
	}
}

// Seedance 轮询：GET 任务端点，succeeded 返回 content.video_url。
func TestArkVideoWait(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write([]byte(`{"id":"t1","status":"succeeded","content":{"video_url":"https://cdn.example.com/v.mp4"}}`))
	}))
	defer srv.Close()
	old := arkPollInterval
	arkPollInterval = 5 * time.Millisecond
	defer func() { arkPollInterval = old }()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	url, err := c.arkVideoWait(context.Background(), "t1", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if url != "https://cdn.example.com/v.mp4" {
		t.Fatalf("未取回视频 URL: %q", url)
	}
	if len(paths) != 1 || paths[0] != "/api/v3/contents/generations/tasks/t1" {
		t.Fatalf("轮询路径错误（httptest 的 base 不含 /api/v3，应被自动补上）: %v", paths)
	}
}

// Seedance 失败状态：带服务端原因的显式错误（不静默、不重试到超时）。
func TestArkVideoWaitFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"t1","status":"failed","error":{"code":"ContentPolicy","message":"输入图片包含敏感内容"}}`))
	}))
	defer srv.Close()
	old := arkPollInterval
	arkPollInterval = 5 * time.Millisecond
	defer func() { arkPollInterval = old }()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := c.arkVideoWait(context.Background(), "t1", 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "敏感内容") {
		t.Fatalf("失败任务应带服务端原因返回错误: %v", err)
	}
}
