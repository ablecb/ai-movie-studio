package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AI 调用记录（调用账本）：每次模型调用落一条 JSON，供 Web 控制台"调用记录"页
// 展示模型、操作类型、请求/响应内容、首次响应时间与完成时间。
//
// 存储形态：每条记录一个独立 JSON 文件（logs/ai_calls/<unixms>_<pid>_<seq>.json）。
// 不用单文件 JSONL 追加：渲染/素材 worker 是多进程并发（Web 任务派生独立 studio
// 进程），大记录的单次 append 在跨进程场景下不保证原子，按文件写天然安全；
// 文件名以毫秒时间戳开头，目录序即时间序。
//
// 内容边界：请求/响应文本截断到 callTextLimit 字符——记录用于排查与审计，
// 不是数据归档；几百 KB 的提示词原样落盘只会让列表页变慢。

const (
	// KindChat 文本对话（含视觉理解，响应为文本）
	KindChat = "chat"
	// KindImage 图片生成（文生图/参考图生图/多参考图编辑）
	KindImage = "image"
	// KindVideo 视频生成（提交+轮询的整体生命周期）
	KindVideo = "video"

	// callTextLimit 单字段文本上限（字符）；超出截断并追加省略标记
	callTextLimit = 65536
)

// CallRecord 一次 AI 调用的完整记录。
type CallRecord struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // chat | image | video
	Call  string `json:"call"` // 操作类型：业务调用名（script / assets.plan / render.shot_image …）
	Model string `json:"model,omitempty"`
	// Run 关联的 run（调用方通过 SetCallScope 注入；Web 控制台按项目筛选用）
	Run string `json:"run,omitempty"`
	// Mock 离线演示调用（未配密钥时的占位响应）
	Mock bool `json:"mock,omitempty"`

	StartAt time.Time `json:"start_at"`
	// FirstResponseAt 首次响应（HTTP 首字节 / 媒体为提交接口首响应）。
	// 指针 + omitempty：零值时间（未收到任何响应即失败）不能序列化成
	// "0001-01-01"，否则前端会渲染出 1/1/1 这种怪值。
	FirstResponseAt *time.Time `json:"first_response_at,omitempty"`
	EndAt           time.Time  `json:"end_at"`
	FirstResponseMs int64      `json:"first_response_ms,omitempty"`
	TotalMs         int64      `json:"total_ms"`

	Status string `json:"status"` // ok | error
	Error  string `json:"error,omitempty"`

	Request  *CallRequest  `json:"request,omitempty"`
	Response *CallResponse `json:"response,omitempty"`
	Usage    *usageInfo    `json:"usage,omitempty"` // chat 类的 token 用量

	// reqImgs 请求侧图片（role + data URL/http URL 原文）。小写未导出字段不进 JSON；
	// recordCall 落盘后异步归档到 artifacts/，再把相对路径写回 CallRequest 的 *File 字段。
	reqImgs []reqImage
}

// reqImage 一次请求携带的图片：role 标识用途（first_frame / end_frame / ref_image）。
type reqImage struct {
	role string
	ref  string
}

// addRequestImage 登记一张随请求发出的图（调用方在 beginCall 之后、finish 之前调用）。
// 空 ref 忽略；内容不进 JSON，落盘后异步归档。
func addRequestImage(rec *CallRecord, role, ref string) {
	if rec == nil || ref == "" || strings.HasPrefix(ref, "mock://") {
		return
	}
	rec.reqImgs = append(rec.reqImgs, reqImage{role: role, ref: ref})
}

// CallRequest 请求内容。大二进制不落 JSON（只记数量/传递方式），避免记录膨胀；
// 请求侧图片另存 artifacts/ 原文件，路径记在 *File 字段供详情页回看。
type CallRequest struct {
	System         string   `json:"system,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	Temperature    float64  `json:"temperature,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Size           string   `json:"size,omitempty"`
	Quality        string   `json:"quality,omitempty"`
	DurationSec    int      `json:"duration_sec,omitempty"`
	RefImages      int      `json:"ref_images,omitempty"`       // 参考图数量
	FirstFrame     string   `json:"first_frame,omitempty"`      // 首帧传递方式："data-url"（内联）/ http(s) URL / "none"（未传图）；字段缺失 = 该记录早于此字段
	FirstFrameFile string   `json:"first_frame_file,omitempty"` // 首帧存档副本（artifacts/ 下相对路径；详情页回看"当时喂的就是这张图"）
	EndFrameFile   string   `json:"end_frame_file,omitempty"`   // 尾帧存档副本
	EndImage       bool     `json:"end_image,omitempty"`        // 是否带尾帧
	RefImageFiles  []string `json:"ref_image_files,omitempty"`  // 参考图存档（按请求顺序）
}

// CallResponse 响应内容：chat 是文本；image/video 是产物 URL 与实际命中的模型。
// LocalPath 是调用方把产物下载到本地后的路径（相对 run 目录）；ArchivePath 是
// 记录器自己异步存档的副本（logs/ai_calls/artifacts/，run 目录被删后记录仍可看）。
// 平台 URL 会过期，详情页按 本地文件 → 存档副本 → 平台URL 的顺序渲染媒体。
type CallResponse struct {
	Text        string `json:"text,omitempty"`
	URL         string `json:"url,omitempty"`
	Model       string `json:"model,omitempty"`
	LocalPath   string `json:"local_path,omitempty"`
	ArchivePath string `json:"archive_path,omitempty"`
}

// callLogState 记录器全局状态：目录 + 进程内序号。
var callLogState struct {
	mu  sync.Mutex
	dir string
	seq int64
}

// callScope 当前进程的 run 作用域（操作类型之外的关联维度）。
var callScope atomic.Value // string

// SetCallScope 标注后续所有调用记录所属的 run（CLI 按 --run 注入）。
func SetCallScope(run string) { callScope.Store(run) }

// callLogDir 记录目录：由 EnableByEnv / SetCallLogDir 显式设置；空 = 不记录
// （单测里的 mock 调用不会产生文件）。
func callLogDir() string {
	callLogState.mu.Lock()
	defer callLogState.mu.Unlock()
	return callLogState.dir
}

// SetCallLogDir 设置记录目录（空串停用记录）。
func SetCallLogDir(dir string) {
	callLogState.mu.Lock()
	callLogState.dir = dir
	callLogState.mu.Unlock()
}

// EnableByEnv 按环境变量启用调用记录：AI_CALL_LOG=off 停用，未设置用默认
// logs/ai_calls（相对进程 cwd——Web 任务派生的 studio 子进程与服务器均以工作区
// 根为 cwd，因此全局一份账本）。CLI 在 newClient 处统一调用。
func EnableByEnv() {
	dir := os.Getenv("AI_CALL_LOG")
	switch {
	case dir == "off":
		SetCallLogDir("")
	case dir == "":
		SetCallLogDir("logs/ai_calls")
	default:
		SetCallLogDir(dir)
	}
}

// recordCall 落盘一条记录；任何失败都静默降级——记录器绝不能影响生产调用。
func recordCall(r *CallRecord) {
	dir := callLogDir()
	if dir == "" || r == nil {
		return
	}
	if r.StartAt.IsZero() {
		r.StartAt = time.Now()
	}
	if r.EndAt.IsZero() {
		r.EndAt = time.Now()
	}
	if r.TotalMs <= 0 {
		r.TotalMs = r.EndAt.Sub(r.StartAt).Milliseconds()
	}
	// 离线演示调用没有 HTTP 首字节：响应即时，首响 = 完成时间，避免前端出现空首响
	if r.Mock && r.FirstResponseAt == nil {
		t := r.EndAt
		r.FirstResponseAt = &t
	}
	if r.FirstResponseMs <= 0 && r.FirstResponseAt != nil && !r.FirstResponseAt.IsZero() {
		r.FirstResponseMs = r.FirstResponseAt.Sub(r.StartAt).Milliseconds()
	}
	if r.Status == "" {
		r.Status = "ok"
	}
	if r.Run == "" {
		if v, ok := callScope.Load().(string); ok {
			r.Run = v
		}
	}
	seq := atomic.AddInt64(&callLogState.seq, 1)
	r.ID = fmt.Sprintf("%013d_%d_%04d", r.StartAt.UnixMilli(), os.Getpid(), seq)
	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return
	}
	callLogState.mu.Lock()
	defer callLogState.mu.Unlock()
	// 目录懒创建（唯一文件名 + 原子写：多进程并发下无需追加锁）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = atomicWrite(filepath.Join(dir, r.ID+".json"), data)
	// 记住产物 URL → 记录 ID 的映射：调用方下载完成后用 AttachCallArtifact 回填本地路径
	if r.Response != nil && r.Response.URL != "" && (r.Kind == KindImage || r.Kind == KindVideo) {
		callArtifacts.Store(r.Response.URL, r.ID)
		// 媒体产物异步存档：平台签名 URL 会过期，记录层自己留一份可回看的副本
		//（run 目录被删、调用方下载失败都不影响回看）；失败静默
		if strings.HasPrefix(r.Response.URL, "http") {
			go archiveMediaArtifact(r.ID, r.Response.URL, r.Kind, dir)
		}
	}
	// 请求侧图片异步归档：详情页要能回看"当时喂给模型的到底是哪张图"——
	// 这是排查一致性/衔接问题的第一现场。失败静默，绝不影响生产调用。
	if len(r.reqImgs) > 0 {
		go archiveRequestImages(r.ID, dir, r.reqImgs)
	}
}

// archiveRequestImages 把请求侧图片落成 artifacts/<id>_req_<role>[_n].<ext>，
// 并把相对路径写回记录的 CallRequest。逐张顺序处理：ref_image 按请求顺序追加。
func archiveRequestImages(id, dir string, imgs []reqImage) {
	for i, im := range imgs {
		name := id + "_req_" + im.role
		if im.role == "ref_image" {
			n := 0
			for _, prev := range imgs[:i] {
				if prev.role == "ref_image" {
					n++
				}
			}
			name = fmt.Sprintf("%s_req_ref_%d", id, n)
		}
		ext, data, err := fetchRequestImage(im.ref)
		if err != nil {
			return // 本张取不到：后续同源引用大概率同样失败，静默放弃
		}
		artDir := filepath.Join(dir, "artifacts")
		if err := os.MkdirAll(artDir, 0o755); err != nil {
			return
		}
		path := filepath.Join(artDir, name+ext)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return
		}
		rel := filepath.ToSlash(filepath.Join("artifacts", name+ext))
		updateCallRecordFile(dir, id, func(rec *CallRecord) {
			if rec.Request == nil {
				rec.Request = &CallRequest{}
			}
			switch im.role {
			case "first_frame":
				rec.Request.FirstFrameFile = rel
			case "end_frame":
				// 尾帧降级重试会把 EndImage 标记清掉：降级后发出的请求不再带尾帧，
				// 存档跟着标记走，避免"记录说没带尾帧、页面却展示尾帧"的自相矛盾
				if rec.Request.EndImage {
					rec.Request.EndFrameFile = rel
				}
			case "ref_image":
				rec.Request.RefImageFiles = append(rec.Request.RefImageFiles, rel)
			}
		})
	}
}

// fetchRequestImage 取回请求图片的原始字节与扩展名。
// data URL 直接解码；http(s) 下载（上限 32MB，防异常端点拖垮记录器）。
func fetchRequestImage(ref string) (string, []byte, error) {
	if strings.HasPrefix(ref, "data:") {
		comma := strings.IndexByte(ref, ',')
		if comma < 0 {
			return "", nil, fmt.Errorf("data URL 缺少逗号分隔")
		}
		meta := ref[5:comma] // image/png;base64
		payload := ref[comma+1:]
		var data []byte
		var err error
		if strings.Contains(meta, ";base64") {
			data, err = base64.StdEncoding.DecodeString(payload)
			if err != nil {
				data, err = base64.RawStdEncoding.DecodeString(payload)
			}
			if err != nil {
				return "", nil, fmt.Errorf("base64 解码失败: %w", err)
			}
		} else {
			return "", nil, fmt.Errorf("仅支持 base64 data URL")
		}
		return sniffImageExt(strings.Split(meta, ";")[0], data), data, nil
	}
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		return "", nil, fmt.Errorf("不支持的图片引用形式")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("下载请求图片失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", nil, err
	}
	return sniffImageExt(resp.Header.Get("Content-Type"), data), data, nil
}

// sniffImageExt 按内容魔数判定扩展名（内容优先，其次 MIME，默认 .png）。
// 请求图是给详情页 <img> 用的，判定错了顶多显示不出，不影响生产链路。
func sniffImageExt(mime string, data []byte) string {
	switch {
	case len(data) >= 8 && data[0] == 0x89 && data[1] == 'P':
		return ".png"
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8:
		return ".jpg"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp"
	case len(data) >= 3 && string(data[0:3]) == "GIF":
		return ".gif"
	}
	m := strings.ToLower(mime)
	switch {
	case strings.Contains(m, "jpeg"), strings.Contains(m, "jpg"):
		return ".jpg"
	case strings.Contains(m, "webp"):
		return ".webp"
	case strings.Contains(m, "gif"):
		return ".gif"
	}
	return ".png"
}

// atomicWrite 原子写：先写临时文件再改名，避免并发读到半截 JSON。
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// updateCallRecordFile 读-改-写一条记录（全局锁串行，避免并发更新互相覆盖字段）。
// 不要求 Response 非空：请求侧图片的回填发生在响应到达之前，失败的调用
// （无响应）恰恰也需要回看"当时传了什么"。
func updateCallRecordFile(dir, id string, fn func(*CallRecord)) {
	callLogState.mu.Lock()
	defer callLogState.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return
	}
	var rec CallRecord
	if json.Unmarshal(data, &rec) != nil {
		return
	}
	fn(&rec)
	if out, err := json.MarshalIndent(&rec, "", " "); err == nil {
		_ = atomicWrite(filepath.Join(dir, id+".json"), out)
	}
}

// archiveMediaArtifact 把平台返回的媒体产物下载到记录目录的 artifacts/ 下，
// 并把相对路径写回记录的 archive_path。URL 不可达时静默放弃。
func archiveMediaArtifact(id, rawURL, kind, dir string) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req.WithContext(ctx))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return
	}
	ext := ".png"
	if kind == KindVideo {
		ext = ".mp4"
	}
	if u, perr := neturl.Parse(rawURL); perr == nil {
		switch {
		case strings.HasSuffix(strings.ToLower(u.Path), ".jpg"):
			ext = ".jpg"
		case strings.HasSuffix(strings.ToLower(u.Path), ".webp"):
			ext = ".webp"
		case strings.HasSuffix(strings.ToLower(u.Path), ".mp4"):
			ext = ".mp4"
		}
	}
	artDir := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return
	}
	f, err := os.Create(filepath.Join(artDir, id+ext))
	if err != nil {
		return
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return
	}
	rel := filepath.ToSlash(filepath.Join("artifacts", id+ext))
	updateCallRecordFile(dir, id, func(rec *CallRecord) {
		if rec.Response != nil {
			rec.Response.ArchivePath = rel
		}
	})
}

// callArtifacts 最近媒体产物的 URL → 记录 ID 映射（进程内；同进程的调用方
// 下载产物后回填本地路径用）。并发渲染 worker 各自调用，map 用 sync.Map。
var callArtifacts sync.Map // string -> string

// AttachCallArtifact 产物落盘后回填：把本地路径（相对 run 目录）写进对应记录，
// 详情页即可从本地文件渲染图片/视频，不依赖会过期的平台 URL。
// url 未知或离线占位（mock://）时静默忽略——记录器永远不影响生产路径。
func AttachCallArtifact(url, localPath string) {
	if url == "" || localPath == "" || strings.HasPrefix(url, "mock://") {
		return
	}
	idV, ok := callArtifacts.Load(url)
	if !ok {
		return
	}
	id, _ := idV.(string)
	dir := callLogDir()
	if dir == "" {
		return
	}
	updateCallRecordFile(dir, id, func(rec *CallRecord) {
		if rec.Response != nil {
			rec.Response.LocalPath = filepath.ToSlash(localPath)
		}
	})
}

// truncateCall 文本字段落盘前的截断（按 rune，保留完整 UTF-8）。
func truncateCall(s string) string {
	r := []rune(s)
	if len(r) <= callTextLimit {
		return s
	}
	return string(r[:callTextLimit]) + "\n…(记录截断，共 " + fmt.Sprintf("%d", len(r)) + " 字符)"
}

// firstFrameLog 首帧"传没传、怎么传"的落账值：不落图片内容，只落传递方式。
// data URL 内联记为 "data-url"，远程 URL 原样记录（可直接回看），
// 未传图记为显式的 "none" 而不是省略字段——字段缺失表示"该记录早于此字段"，
// 与"确认没传"必须可区分（"没查"不能表现为"没传"，反之亦然）。
func firstFrameLog(imageRef string) string {
	switch {
	case imageRef == "":
		return "none"
	case strings.HasPrefix(imageRef, "data:"):
		return "data-url"
	default:
		return truncateCall(imageRef)
	}
}

// callSpan 一次调用的计时跨度：请求发出前创建，首字节与结束时间写回。
// 通过 context 传递给内部 HTTP 调用，跨 reservePost/post/get 的多层嵌套仍能归位。
type callSpan struct {
	first atomic.Int64 // 首字节 unixMilli；0 = 未收到
}

func (s *callSpan) ctx(base context.Context) context.Context {
	return httptrace.WithClientTrace(base, &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			s.first.CompareAndSwap(0, time.Now().UnixMilli())
		},
	})
}

func (s *callSpan) firstTime() time.Time {
	if ms := s.first.Load(); ms > 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

// beginCall 创建记录骨架并返回 span 与完成回调（ok / error 二选一收尾）。
func beginCall(kind, callName, model string) (*callSpan, *CallRecord, func(*CallResponse), func(error)) {
	span := &callSpan{}
	start := time.Now()
	rec := &CallRecord{
		Kind: kind, Call: callName, Model: model,
		StartAt: start, Status: "ok",
	}
	done := func(resp *CallResponse) {
		rec.Response = resp
		if t := span.firstTime(); !t.IsZero() {
			rec.FirstResponseAt = &t
		}
		recordCall(rec)
	}
	fail := func(err error) {
		rec.Status = "error"
		rec.Error = truncateCall(err.Error())
		if t := span.firstTime(); !t.IsZero() {
			rec.FirstResponseAt = &t
		}
		recordCall(rec)
	}
	return span, rec, done, fail
}
