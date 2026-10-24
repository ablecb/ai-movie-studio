package ai

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonEntries 只返回目录里的 .json 记录文件。
//
// 为什么不能直接数目录条目：请求侧图片的**异步归档**会在同目录建 artifacts/
// 子目录，它何时出现取决于 goroutine 调度——按条目总数断言会随机多出 1 条、
// 甚至读到目录导致 ReadFile 报错（真实踩过的 flake）。
func jsonEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e)
		}
	}
	return out
}

func TestCallLogRecording(t *testing.T) {
	dir := t.TempDir()
	SetCallLogDir(dir)
	t.Cleanup(func() { SetCallLogDir("") })
	SetCallScope("run-test")
	t.Cleanup(func() { SetCallScope("") })

	c := New(Config{Mock: true, Model: "glm-test"})
	if _, err := c.Chat("script", "system prompt", "user prompt"); err != nil {
		t.Fatalf("mock chat 失败: %v", err)
	}
	if _, _, err := c.ImageGenEx("assets.ref_frame", "a portrait", ImageOptions{Model: "cogview-4", Size: "720p"}); err != nil {
		t.Fatalf("mock image 失败: %v", err)
	}
	if _, err := c.VideoGen("render.shot_video", "a shot", "", VideoOptions{Model: "cogvideox-3"}); err != nil {
		t.Fatalf("mock video 失败: %v", err)
	}
	if _, err := c.VideoGen("render.shot_video_ref", "a shot", "data:image/png;base64,AAAA", VideoOptions{Model: "cogvideox-3"}); err != nil {
		t.Fatalf("mock video(带首帧) 失败: %v", err)
	}

	recs := jsonEntries(t, dir)
	if len(recs) != 4 {
		t.Fatalf("应落盘 4 条记录，得到 %d", len(recs))
	}
	byCall := map[string]*CallRecord{}
	for _, e := range recs {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec CallRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("记录不是合法 JSON: %v", err)
		}
		byCall[rec.Call] = &rec
	}
	chat := byCall["script"]
	if chat == nil || chat.Kind != KindChat || chat.Model != "glm-test" || chat.Run != "run-test" {
		t.Fatalf("chat 记录字段不完整: %+v", chat)
	}
	if chat.Status != "ok" || !strings.Contains(chat.Response.Text, "") || chat.Request.System != "system prompt" {
		t.Fatalf("chat 记录内容缺失: %+v", chat)
	}
	if chat.TotalMs < 0 || chat.EndAt.Before(chat.StartAt) {
		t.Fatalf("chat 计时异常: %+v", chat)
	}
	img := byCall["assets.ref_frame"]
	if img == nil || img.Kind != KindImage || img.Response.URL != "mock://image" || !img.Mock {
		t.Fatalf("image 记录不正确: %+v", img)
	}
	vid := byCall["render.shot_video"]
	if vid == nil || vid.Kind != KindVideo || vid.Response.URL != "mock://video" {
		t.Fatalf("video 记录不正确: %+v", vid)
	}
	// 未传图的视频调用必须记为显式 "none"，不能省略字段——否则与
	// "该记录早于此字段"的旧记录无法区分
	if vid.Request == nil || vid.Request.FirstFrame != "none" {
		t.Fatalf("无图视频调用应记 first_frame=none: %+v", vid.Request)
	}
	vidRef := byCall["render.shot_video_ref"]
	if vidRef == nil || vidRef.Request == nil || vidRef.Request.FirstFrame != "data-url" {
		t.Fatalf("带首帧视频调用应记 first_frame=data-url: %+v", vidRef)
	}
}

func TestFirstFrameLog(t *testing.T) {
	if got := firstFrameLog(""); got != "none" {
		t.Fatalf("未传图应记为显式 none，得到 %q", got)
	}
	if got := firstFrameLog("data:image/png;base64,AAAA"); got != "data-url" {
		t.Fatalf("内联 data URL 应记为 data-url，得到 %q", got)
	}
	const u = "https://cdn.example.com/first-frame.png"
	if got := firstFrameLog(u); got != u {
		t.Fatalf("远程 URL 应原样记录，得到 %q", got)
	}
}

func TestCallLogArchivesRequestImages(t *testing.T) {
	dir := t.TempDir()
	SetCallLogDir(dir)
	t.Cleanup(func() { SetCallLogDir("") })

	// 本地 HTTP 服务模拟远程参考图（JPEG 魔数开头，验证按内容判扩展名）
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("\xFF\xD8\xFF-jpegdata"))
	})}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	// 一条带首帧（data URL）+ 尾帧 + 参考图（远程 URL）的视频记录
	pngBytes := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fakepng")...)
	jpgBytes := append([]byte("\xFF\xD8\xFF"), []byte("fakejpeg")...)
	rec := &CallRecord{
		Kind: KindVideo, Call: "render.shot_video_ref", Model: "cogvideox-3",
		Request: &CallRequest{FirstFrame: "data-url", EndImage: true},
	}
	addRequestImage(rec, "first_frame", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes))
	addRequestImage(rec, "end_frame", "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(jpgBytes))
	addRequestImage(rec, "ref_image", "http://"+ln.Addr().String()+"/ref.jpg")
	recordCall(rec)

	// 归档是异步的：轮询等待三张图落盘 + 路径回填
	deadline := time.Now().Add(3 * time.Second)
	var got CallRecord
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		found := 0
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if json.Unmarshal(data, &got) != nil || got.Request == nil {
				continue
			}
			if got.Request.FirstFrameFile != "" {
				found++
			}
			if got.Request.EndFrameFile != "" {
				found++
			}
			if len(got.Request.RefImageFiles) > 0 {
				found++
			}
		}
		if found == 3 {
			// 校验文件内容与扩展名
			check := func(rel, wantExt string, wantPrefix []byte) {
				t.Helper()
				if !strings.HasSuffix(rel, wantExt) {
					t.Fatalf("存档扩展名应为 %s: %s", wantExt, rel)
				}
				data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
				if err != nil || string(data[:len(wantPrefix)]) != string(wantPrefix) {
					t.Fatalf("存档内容不正确: %v", err)
				}
			}
			check(got.Request.FirstFrameFile, ".png", []byte("\x89PNG"))
			check(got.Request.EndFrameFile, ".jpg", []byte("\xFF\xD8"))
			check(got.Request.RefImageFiles[0], ".jpg", []byte("\xFF\xD8"))
			return // 通过
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("3 秒内请求图片未全部归档回填: first=%q end=%q refs=%v",
		got.Request.FirstFrameFile, got.Request.EndFrameFile, got.Request.RefImageFiles)
}

func TestCallLogErrorPath(t *testing.T) {
	dir := t.TempDir()
	SetCallLogDir(dir)
	t.Cleanup(func() { SetCallLogDir("") })

	// 指向必然拒绝连接的端口：传输层错误应记录为 error 而非中断测试进程
	c := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m"})
	if _, err := c.Chat("probe", "s", "u"); err == nil {
		t.Fatal("坏端点应当返回错误")
	}
	entries := jsonEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("错误调用也应落盘 1 条，得到 %d", len(entries))
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var rec CallRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != "error" || rec.Error == "" {
		t.Fatalf("错误记录字段不完整: %+v", rec)
	}
}

func TestCallLogAttachArtifact(t *testing.T) {
	dir := t.TempDir()
	SetCallLogDir(dir)
	t.Cleanup(func() { SetCallLogDir("") })

	// 直接落一条图片记录（真实 URL），随后回填本地路径
	recordCall(&CallRecord{
		Kind: KindImage, Call: "assets.scene_base", Model: "cogview-4",
		Response: &CallResponse{URL: "https://cdn.example.com/img.png"},
	})
	AttachCallArtifact("https://cdn.example.com/img.png", "output/assets/scenes/s1.png")

	entries := jsonEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("应只有 1 条记录，得到 %d", len(entries))
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var rec CallRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Response.LocalPath != "output/assets/scenes/s1.png" {
		t.Fatalf("本地路径未回填: %+v", rec.Response)
	}

	// mock:// 占位不允许回填（占位文件不是真实图片）
	AttachCallArtifact("mock://image", "output/refs/fake.png")
	data, _ = os.ReadFile(filepath.Join(dir, entries[0].Name()))
	_ = json.Unmarshal(data, &rec)
	if rec.Response.LocalPath != "output/assets/scenes/s1.png" {
		t.Fatalf("mock URL 不应触发回填: %+v", rec.Response)
	}

	// 未知 URL 静默忽略
	AttachCallArtifact("https://unknown.example.com/x.png", "a/b.png")
}

func TestCallLogArchiveArtifact(t *testing.T) {
	dir := t.TempDir()
	SetCallLogDir(dir)
	t.Cleanup(func() { SetCallLogDir("") })

	// 本地 HTTP 服务模拟平台返回的图片 URL（含扩展名，验证按后缀存档）
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("PNGDATA"))
	})}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	imgURL := "http://" + ln.Addr().String() + ("/gen/abc.png")

	recordCall(&CallRecord{
		Kind: KindImage, Call: "assets.scene_base", Model: "m",
		Response: &CallResponse{URL: imgURL},
	})

	// 存档是异步的：轮询等待落盘 + archive_path 回填
	deadline := time.Now().Add(3 * time.Second)
	var rec CallRecord
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if json.Unmarshal(data, &rec) == nil && rec.Response != nil && rec.Response.ArchivePath != "" {
				artPath := filepath.Join(dir, filepath.FromSlash(rec.Response.ArchivePath))
				if data, err := os.ReadFile(artPath); err != nil || string(data) != "PNGDATA" {
					t.Fatalf("存档内容不正确: %v %q", err, data)
				}
				if !strings.HasSuffix(rec.Response.ArchivePath, ".png") {
					t.Fatalf("应按 URL 后缀存档为 .png: %s", rec.Response.ArchivePath)
				}
				return // 通过
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("3 秒内未见媒体存档回填")
}

func TestChatStreaming(t *testing.T) {
	dir := t.TempDir()
	SetCallLogDir(dir)
	t.Cleanup(func() { SetCallLogDir("") })

	// SSE 流式服务：两个内容块 + 最终 usage 块 + [DONE]
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\",世界。\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
	})}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c := New(Config{BaseURL: "http://" + ln.Addr().String(), APIKey: "k", Model: "glm-test"})
	text, err := c.Chat("script", "sys", "user")
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if text != "你好,世界。" {
		t.Fatalf("流式内容拼装不正确: %q", text)
	}
	snap := c.UsageSnapshot()
	if st := snap["script"]; st.CompletionTokens != 5 || st.PromptTokens != 10 {
		t.Fatalf("流式 usage 未记账: %+v", st)
	}
	// 调用记录应落盘且带正文
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		var rec CallRecord
		if json.Unmarshal(data, &rec) == nil && rec.Call == "script" {
			found = true
			if rec.Response == nil || rec.Response.Text != "你好,世界。" {
				t.Fatalf("记录正文不正确: %+v", rec.Response)
			}
		}
	}
	if !found {
		t.Fatal("调用记录未落盘")
	}
}

func TestCallLogTruncation(t *testing.T) {
	huge := strings.Repeat("字", callTextLimit+5000)
	got := truncateCall(huge)
	if len([]rune(got)) > callTextLimit+100 {
		t.Fatalf("截断失效：长度 %d", len([]rune(got)))
	}
	if !strings.Contains(got, "记录截断") {
		t.Fatal("截断应带省略标记")
	}
	if truncateCall("短文本") != "短文本" {
		t.Fatal("短文本不应被改写")
	}
}
