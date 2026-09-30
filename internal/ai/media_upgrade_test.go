package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// 官方支持 duration=5/10：计划时长 >5.5s 的镜头必须真正把 10 发出去。
func TestVideoBody_Duration(t *testing.T) {
	body := ExportVideoBody("p", "img", VideoOptions{Model: "cogvideox-3", Duration: 10}, "cogvideox-3")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["duration"] != float64(10) {
		t.Fatalf("duration=10 未随请求发出: %s", body)
	}
}

// vidu 系模型：image_url 为参考图数组，且不带 size/fps/quality/duration。
func TestVideoBody_ViduReference(t *testing.T) {
	body := ExportVideoBody("p", "first-frame", VideoOptions{
		Model:             "vidu2-reference",
		RefImages:         []string{"refA", "refB", "refC"},
		AspectRatio:       "16:9",
		MovementAmplitude: "auto",
		Size:              "1920x1080", FPS: 30, Quality: "quality", Duration: 10,
	}, "vidu2-reference")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	arr, ok := m["image_url"].([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("vidu 参考图应为 3 元素数组: %s", body)
	}
	for _, k := range []string{"size", "fps", "quality", "duration"} {
		if _, exists := m[k]; exists {
			t.Fatalf("vidu 请求不应携带 %s: %s", k, body)
		}
	}
	if m["aspect_ratio"] != "16:9" || m["movement_amplitude"] != "auto" {
		t.Fatalf("vidu 专属参数缺失: %s", body)
	}
}

// 首尾帧数组（链式衔接）：EndImage 非空时 CogVideoX 的 image_url 必须是
// [首帧, 尾帧] 两元素数组——场景衔接的官方能力；没有尾帧时保持单字符串。
func TestVideoBody_FirstLastFrames(t *testing.T) {
	body := ExportVideoBody("p", "first-frame", VideoOptions{
		Model: "cogvideox-3", EndImage: "last-frame",
	}, "cogvideox-3")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	arr, ok := m["image_url"].([]any)
	if !ok || len(arr) != 2 || arr[0] != "first-frame" || arr[1] != "last-frame" {
		t.Fatalf("首尾帧应为 [首帧,尾帧] 数组: %s", body)
	}

	// 无尾帧：兼容旧行为（单字符串），否则服务端可能按数组语义误读
	body = ExportVideoBody("p", "first-frame", VideoOptions{Model: "cogvideox-3"}, "cogvideox-3")
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if s, ok := m["image_url"].(string); !ok || s != "first-frame" {
		t.Fatalf("无尾帧时 image_url 应保持字符串: %s", body)
	}
}

// 情绪映射：中文标注 → MiniMax 枚举；未命中返回空（模型自动识别）。
func TestMapEmotion(t *testing.T) {
	cases := map[string]string{
		"低沉，几乎像自言自语": "sad",
		"急切":         "", // 急促靠语速表达，不硬映射
		"愤怒地喊":       "angry",
		"平静克制":       "neutral",
		"开心地笑":       "happy",
		"恐惧":         "fearful",
		"惊讶":         "surprised",
		"happy":      "happy",
		"":           "",
	}
	for in, want := range cases {
		if got := MapEmotion(in); got != want {
			t.Errorf("MapEmotion(%q)=%q, want %q", in, got, want)
		}
	}
}

// MiniMax TTS 请求体：emotion/pitch 正确进入 voice_setting。
func TestMinimaxTTSBody(t *testing.T) {
	b := ExportMinimaxTTSBody("你好。", TTSOptions{
		Voice: "female-shaonv", Speed: 0.9, Emotion: "低沉", Pitch: 1.05, Model: "speech-2.6-hd",
	})
	if b.Model != "speech-2.6-hd" || b.VoiceSetting.Emotion != "sad" {
		t.Fatalf("emotion 未映射: %+v", b.VoiceSetting)
	}
	if b.VoiceSetting.Pitch != 1 {
		t.Fatalf("1.05 比值应换算为 +1 半音，实际 %d", b.VoiceSetting.Pitch)
	}
	if b.AudioSetting.Format != "mp3" {
		t.Fatalf("音频格式应为 mp3: %+v", b.AudioSetting)
	}
}

// 响应音频解码：hex 与 base64 都要能解。
func TestDecodeAudioPayload(t *testing.T) {
	if b, err := decodeAudioPayload("48656c6c6f"); err != nil || string(b) != "Hello" {
		t.Fatalf("hex 解码失败: %v %q", err, b)
	}
	if b, err := decodeAudioPayload("SGVsbG8="); err != nil || string(b) != "Hello" {
		t.Fatalf("base64 解码失败: %v %q", err, b)
	}
	if _, err := decodeAudioPayload("!!!"); err == nil {
		t.Fatal("非法负载应报错")
	}
}

// 多参考图：dashscope / ark 的宽容解析。
func TestParseEditImageURL(t *testing.T) {
	if u := parseEditImageURL([]byte(`{"data":[{"url":"https://x/a.png"}]}`)); u != "https://x/a.png" {
		t.Fatalf("ark 结构解析失败: %s", u)
	}
	if u := parseEditImageURL([]byte(`{"output":{"choices":[{"message":{"content":[{"image":"https://x/b.png"}]}}]}}`)); u != "https://x/b.png" {
		t.Fatalf("dashscope 结构解析失败: %s", u)
	}
	if u := parseEditImageURL([]byte(`{"weird":{"image":"https://x/c.png","other":1}}`)); u != "https://x/c.png" {
		t.Fatalf("正则兜底失败: %s", u)
	}
}

// 非法提示词防呆：文本规范化保留（既有行为回归）。
func TestNormalizeTTSTextStillStripsStageDirections(t *testing.T) {
	got := normalizeTTSText("（低声）我回来了")
	if strings.Contains(got, "低声") {
		t.Fatalf("舞台提示应被剔除: %q", got)
	}
}
