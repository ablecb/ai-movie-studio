package deliver

import (
	"strings"
	"testing"

	"aistudio/internal/config"
)

func trMap() map[string]string {
	return map[string]string{
		"sh001": "fade_in", "sh002": "cut", "sh003": "dissolve", "sh004": "dissolve",
	}
}

func clips4() []shotClip {
	return []shotClip{
		{ID: "sh001", Dur: 5.0}, {ID: "sh002", Dur: 5.0},
		{ID: "sh003", Dur: 5.0}, {ID: "sh004", Dur: 5.0},
	}
}

// 无转场配置时必须产出与旧版等价的纯 concat 图。
func TestBuildConcatFilter_NoTransitions(t *testing.T) {
	g, err := buildConcatFilter(clips4(), nil, 1920, 1080, 24, config.Transitions{Enabled: false}, true, true, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g, "xfade") || strings.Contains(g, "tpad") {
		t.Fatalf("禁用转场时不应出现 xfade/tpad:\n%s", g)
	}
	if !strings.Contains(g, "concat=n=4:v=1:a=0[vcat]") {
		t.Fatalf("缺少 concat 节点:\n%s", g)
	}
}

// 单处叠化：出现 xfade + B 侧减速补长，且 concat 段数 = 镜头数 + 边界数。
func TestBuildConcatFilter_SingleDissolve(t *testing.T) {
	tc := config.Transitions{Enabled: true, DissolveSec: 0.6, FadeInSec: 0.8, FadeOutSec: 1.0}
	g, err := buildConcatFilter(clips4(), trMap(), 1920, 1080, 24, tc, true, true, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(g, "xfade=transition=fade") != 1 {
		t.Fatalf("应恰好 1 处叠化（sh004 的边界退化）:\n%s", g)
	}
	// B 侧用减速补回叠化占用的 N 秒（5s 镜头、0.6s 叠化 → 5/4.4 ≈ 1.1364 倍），
	// 而不是末帧冻结——冻结会在剪入下一镜前产生可见停顿
	if !strings.Contains(g, "setpts=1.13636*PTS") {
		t.Fatalf("B 侧剩余段应减速补长（因子 %.4f）:\n%s", 5.0/4.4, g)
	}
	if strings.Contains(g, "tpad") {
		t.Fatalf("不应再出现末帧冻结:\n%s", g)
	}
	if !strings.Contains(g, "concat=n=5:v=1:a=0[vcat]") {
		t.Fatalf("concat 段数应为 镜头4+边界1=5:\n%s", g)
	}
	// 时间轴守恒的关键：A 侧尾段从 dur-N 开始，B 侧正段从 N 开始
	if !strings.Contains(g, "trim=start=4.400") || !strings.Contains(g, "trim=end=0.600") {
		t.Fatalf("叠化的 A/B 侧裁剪点不符合守恒设计:\n%s", g)
	}
	// 片头淡入与片尾淡出挂在 concat 之后（总长 20s）
	if !strings.Contains(g, "fade=t=in:st=0:d=0.80") || !strings.Contains(g, "fade=t=out:st=19.00:d=1.00") {
		t.Fatalf("缺少片头淡入/片尾淡出:\n%s", g)
	}
}

// 相邻两处叠化：第二处自动降级为硬切（避免同一视频流被双重消费）。
func TestBuildConcatFilter_ConsecutiveDissolveDegrades(t *testing.T) {
	g, err := buildConcatFilter(clips4(), trMap(), 1920, 1080, 24,
		config.Transitions{Enabled: true, DissolveSec: 0.6}, true, true, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(g, "xfade=transition=fade"); n != 1 {
		t.Fatalf("连续叠化应只保留第一处，实际 %d 处:\n%s", n, g)
	}
}

// 镜头过短时叠化退化为硬切（减速因子过大不可接受）。
func TestBuildConcatFilter_ShortClipDegrades(t *testing.T) {
	clips := []shotClip{{ID: "sh001", Dur: 5.0}, {ID: "sh002", Dur: 2.2}}
	g, err := buildConcatFilter(clips, map[string]string{"sh002": "dissolve"}, 1920, 1080, 24,
		config.Transitions{Enabled: true, DissolveSec: 0.6}, true, true, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g, "xfade") {
		t.Fatalf("B 侧短于 4N+0.2 应退化为硬切:\n%s", g)
	}
}

// ffmpeg 缺少 xfade 滤镜时全部硬切。
func TestBuildConcatFilter_MissingFilters(t *testing.T) {
	g, err := buildConcatFilter(clips4(), trMap(), 1920, 1080, 24,
		config.Transitions{Enabled: true, DissolveSec: 0.6}, false, false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g, "xfade") || strings.Contains(g, "tpad") {
		t.Fatalf("滤镜缺失时不应生成叠化:\n%s", g)
	}
}

// 宽银幕：Style.Aspect 宽于交付容器时按画幅裁切 + letterbox。
func TestBuildConcatFilter_ScopeCrop(t *testing.T) {
	g, err := buildConcatFilter(clips4(), nil, 1920, 1080, 24,
		config.Transitions{Enabled: false}, true, true, nil, 804)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g, "scale=1920:804:force_original_aspect_ratio=increase,crop=1920:804") {
		t.Fatalf("应先放大裁切到 scope 画幅:\n%s", g)
	}
	if !strings.Contains(g, "pad=1920:1080") {
		t.Fatalf("裁切后应 letterbox 回交付容器:\n%s", g)
	}
}

// scopeCropHeight：解析画幅字符串，越界/无意义裁切返回 0。
func TestScopeCropHeight(t *testing.T) {
	cases := []struct {
		aspect string
		w, h   int
		want   int
	}{
		{"2.39:1", 1920, 1080, 802}, // 1920/2.39=803.4 → 取偶 802
		{"2.35:1", 1920, 1080, 816},
		{"1.85:1", 1920, 1080, 1038}, // 42px 黑边，标准 1.85 flatten
		{"16:9", 1920, 1080, 0},      // 不宽于容器
		{"", 1920, 1080, 0},          // 未声明
		{"abc", 1920, 1080, 0},       // 解析失败
	}
	for _, c := range cases {
		if got := scopeCropHeight(c.aspect, c.w, c.h); got != c.want {
			t.Errorf("scopeCropHeight(%q) = %d, want %d", c.aspect, got, c.want)
		}
	}
}

// loadTransitions：editplan.json 缺失时返回空表（全部硬切，兼容旧 run）。
func TestLoadTransitions_Missing(t *testing.T) {
	if m := loadTransitions(t.TempDir()); m != nil {
		t.Fatalf("缺失 editplan.json 应返回 nil，实际 %v", m)
	}
}
