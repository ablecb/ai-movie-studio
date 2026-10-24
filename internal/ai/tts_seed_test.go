package ai

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestSeedEmotion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"happy", "happy"},
		{"angry", "angry"},
		{"sad", "sad"},
		{"fearful", "fear"},
		{"surprised", "surprise"},
		{"disgusted", ""}, // 豆包无该枚举：不传参数，交给上下文预测
		{"unknown-enum", ""},
	}
	for _, c := range cases {
		if got := seedEmotion(c.in); got != c.want {
			t.Errorf("seedEmotion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSeedTTSRequestBody(t *testing.T) {
	body := seedTTSRequestBody("雷霆万钧！", TTSOptions{
		Voice: "zh_male_test_bigtts", Speed: 0.88, Emotion: "愤怒，冷声压火",
	}, "my-appid", "seed-tts-1.1")

	if body.App.AppID != "my-appid" {
		t.Errorf("appid = %q, want my-appid", body.App.AppID)
	}
	if body.App.Cluster != "volcano_tts" {
		t.Errorf("cluster = %q, want volcano_tts", body.App.Cluster)
	}
	if body.App.Token == "" {
		t.Errorf("token 占位值不能为空（官方要求非空字符串）")
	}
	if body.Audio.VoiceType != "zh_male_test_bigtts" {
		t.Errorf("voice = %q", body.Audio.VoiceType)
	}
	if body.Audio.Encoding != "wav" || body.Audio.Rate != 24000 {
		t.Errorf("encoding/rate = %s/%d, want wav/24000", body.Audio.Encoding, body.Audio.Rate)
	}
	if body.Audio.SpeedRatio != 0.88 {
		t.Errorf("speed = %v, want 0.88", body.Audio.SpeedRatio)
	}
	if body.Audio.Emotion != "angry" || !body.Audio.EnableEmotion {
		t.Errorf("emotion = %q enable=%v, want angry/true", body.Audio.Emotion, body.Audio.EnableEmotion)
	}
	if body.Request.Operation != "query" || body.Request.TextType != "plain" {
		t.Errorf("operation/text_type = %s/%s", body.Request.Operation, body.Request.TextType)
	}
	if body.Request.Model != "seed-tts-1.1" {
		t.Errorf("model = %q", body.Request.Model)
	}
	if body.Request.ReqID == "" || len(body.Request.ReqID) != 32 {
		t.Errorf("reqid 应为 32 位 hex，got %q", body.Request.ReqID)
	}
}

func TestSeedTTSRequestBodyDefaults(t *testing.T) {
	// 未指定音色/情绪/语速：默认音色、speed 归一为 1.0、无情绪参数（交给模型预测）
	body := seedTTSRequestBody("旁白一句", TTSOptions{}, "appid", "")
	if body.Audio.VoiceType != "zh_male_M392_conversation_wvae_bigtts" {
		t.Errorf("default voice = %q", body.Audio.VoiceType)
	}
	if body.Audio.SpeedRatio != 1.0 {
		t.Errorf("speed = %v, want 1.0", body.Audio.SpeedRatio)
	}
	if body.Audio.Emotion != "" || body.Audio.EnableEmotion {
		t.Errorf("无情绪标注时不应传 emotion 参数")
	}
	if body.Request.Model != "" {
		t.Errorf("model 空时应由服务端默认，got %q", body.Request.Model)
	}
}

func TestSeedEmotionScale(t *testing.T) {
	if got := seedEmotionScale("愤怒，战吼炸裂"); got != 5 {
		t.Errorf("战吼强度 = %d, want 5", got)
	}
	if got := seedEmotionScale("平静，低沉铺陈"); got != 4 {
		t.Errorf("平静强度 = %d, want 4", got)
	}
}

func TestIsSeedTTSEndpoint(t *testing.T) {
	for _, u := range []string{"https://openspeech.bytedance.com", "https://openspeech.bytedance.com/"} {
		if !isSeedTTSEndpoint(u) {
			t.Errorf("%q 应识别为火山语音端点", u)
		}
	}
	for _, u := range []string{"https://open.bigmodel.cn/api/paas/v4", "https://api.minimax.chat", ""} {
		if isSeedTTSEndpoint(u) {
			t.Errorf("%q 不应识别为火山语音端点", u)
		}
	}
}

func TestSeedTTSBodyJSON(t *testing.T) {
	// 序列化必须能被官方 API 解析：关键字段名稳定（appid/cluster/voice_type/...）
	raw, err := json.Marshal(seedTTSRequestBody("hi", TTSOptions{Voice: "v1"}, "a", "m"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, key := range []string{`"appid"`, `"cluster"`, `"voice_type"`, `"text_type"`, `"operation"`} {
		if !strings.Contains(s, key) {
			t.Errorf("请求体缺少字段 %s：%s", key, s)
		}
	}
}

func TestSeedV3RequestBody(t *testing.T) {
	body := seedV3RequestBody("雷霆万钧！", "zh_male_test_bigtts",
		TTSOptions{Speed: 1.5, Emotion: "愤怒，战吼炸裂"}, "seed-tts-1.0")
	if body.ReqParams.Speaker != "zh_male_test_bigtts" {
		t.Errorf("speaker = %q", body.ReqParams.Speaker)
	}
	if body.ReqParams.AudioParms.Format != "wav" || body.ReqParams.AudioParms.SampleRate != 24000 {
		t.Errorf("format/sample_rate = %s/%d", body.ReqParams.AudioParms.Format, body.ReqParams.AudioParms.SampleRate)
	}
	// speed 1.5 → speech_rate 50
	if body.ReqParams.AudioParms.SpeechRate != 50 {
		t.Errorf("speech_rate = %d, want 50", body.ReqParams.AudioParms.SpeechRate)
	}
	if body.ReqParams.AudioParms.Emotion != "angry" || body.ReqParams.AudioParms.EmotionScale != 5 {
		t.Errorf("emotion = %q/%d, want angry/5", body.ReqParams.AudioParms.Emotion, body.ReqParams.AudioParms.EmotionScale)
	}

	// 2.0 代际：不传 emotion（模型上下文预测）；语速未指定归一 1.0 → speech_rate 0
	body2 := seedV3RequestBody("旁白", "zh_male_m191_uranus_bigtts", TTSOptions{}, "seed-tts-2.0")
	if body2.ReqParams.AudioParms.Emotion != "" {
		t.Errorf("2.0 代际不应传 emotion")
	}
	if body2.ReqParams.AudioParms.SpeechRate != 0 {
		t.Errorf("默认语速 speech_rate = %d, want 0", body2.ReqParams.AudioParms.SpeechRate)
	}
}

func TestAudioGenRespAudio(t *testing.T) {
	// 成功：audio 非空即成功（code 可能缺省）
	got, err := audioGenRespAudio([]byte(`{"audio":"` + base64.StdEncoding.EncodeToString([]byte("mp3bytes")) + `","duration":5.9}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mp3bytes" {
		t.Errorf("audio = %q", got)
	}
	// 失败：code != 0 带出 message
	if _, err := audioGenRespAudio([]byte(`{"code":3001,"message":"service not granted"}`)); err == nil {
		t.Errorf("错误响应应返回 error")
	} else if !strings.Contains(err.Error(), "service not granted") {
		t.Errorf("错误应含 message: %v", err)
	}
	// 空响应
	if _, err := audioGenRespAudio([]byte(`{}`)); err == nil {
		t.Errorf("空音频应报错")
	}
}

func TestSeedV3ParseResponse(t *testing.T) {
	// 正常：两片 base64 顺序拼接
	a := base64.StdEncoding.EncodeToString([]byte("hello "))
	b := base64.StdEncoding.EncodeToString([]byte("world"))
	ndjson := `{"code":0,"message":"ok","data":"` + a + `"}` + "\n" +
		`{"code":0,"message":"ok","data":"` + b + `"}` + "\n"
	got, err := seedV3ParseResponse([]byte(ndjson))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Errorf("拼接结果 = %q, want %q", got, "hello world")
	}

	// 错误分片：code != 0 带出 message
	if _, err := seedV3ParseResponse([]byte(`{"code":3001,"message":"grant not found","data":""}`)); err == nil {
		t.Errorf("code!=0 应返回错误")
	} else if !strings.Contains(err.Error(), "grant not found") {
		t.Errorf("错误信息应含 message，got %v", err)
	}

	// 数据与结束码同行：先取数据再结束
	mixed := `{"code":20000000,"message":"OK","data":"` + base64.StdEncoding.EncodeToString([]byte("done")) + `"}`
	got2, err := seedV3ParseResponse([]byte(mixed))
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != "done" {
		t.Errorf("同行结束码取数 = %q, want done", got2)
	}

	// 空响应 / 无分片
	if _, err := seedV3ParseResponse(nil); err == nil {
		t.Errorf("空响应应报错")
	}
	if _, err := seedV3ParseResponse([]byte("{\"code\":0}\n")); err == nil {
		t.Errorf("无音频分片应报错")
	}
}
