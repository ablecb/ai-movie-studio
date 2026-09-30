package deliver

import (
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/config"
)

// 曝光匹配回归：镜头间亮度漂移是生成端逐镜独立产物的直接后果，
// concat 阶段的确定性校正是"AI 味"消除的一环。

func TestLumaMedian(t *testing.T) {
	if got := lumaMedian([]float64{10, 20, 30, 200}); got != 25 {
		// 中位数必须抗离群值：单帧纯黑转场不能把全片基准拉暗
		t.Fatalf("中位数应为 25（(20+30)/2），实际 %.1f", got)
	}
	if got := lumaMedian([]float64{40}); got != 40 {
		t.Fatalf("单值中位数应为自身，实际 %.1f", got)
	}
	if got := lumaMedian(nil); got != 0 {
		t.Fatalf("空集应为 0，实际 %.1f", got)
	}
}

func TestBuildConcatFilterInsertsExposureCorrection(t *testing.T) {
	clips := []shotClip{
		{ID: "sh001", Dur: 4}, {ID: "sh002", Dur: 4},
	}
	// sh001 偏暗（+0.05 校正），sh002 无偏差（不插滤镜）
	g, err := buildConcatFilter(clips, nil, 1920, 1080, 24,
		config.Transitions{Enabled: false}, false, false, []float64{0.05, 0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g, "eq=brightness=0.0500") {
		t.Fatalf("偏暗镜头应插入 eq 亮度校正:\n%s", g)
	}
	// eq 必须插在 scale 与 pad 之间：letterbox 黑边在校正后添加，不会被抬成灰边
	seg := g[strings.Index(g, "[0:v]"):]
	if !strings.Contains(seg, "scale=") || strings.Index(seg, "eq=") > strings.Index(seg, "pad=") {
		t.Fatalf("eq 应位于 scale 之后、pad 之前:\n%s", seg)
	}
}

func TestBuildConcatFilterWithoutExposure(t *testing.T) {
	clips := []shotClip{{ID: "sh001", Dur: 4}, {ID: "sh002", Dur: 4}}
	g, err := buildConcatFilter(clips, nil, 1920, 1080, 24,
		config.Transitions{Enabled: false}, false, false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g, "eq=") {
		t.Fatalf("未提供校正表时不应插入 eq 滤镜:\n%s", g)
	}
}

// 门禁 fail-closed 回归：narration.json 缺失时"字幕未越出所属镜头"绝不能显示通过。
func TestCountOutOfShotCuesFailsClosed(t *testing.T) {
	dir := t.TempDir()
	_, err := countOutOfShotCues(dir)
	if err == nil {
		t.Fatal("narration.json 缺失必须返回错误（无证据不能视为通过）")
	}
	if !strings.Contains(err.Error(), "narration.json") {
		t.Fatalf("错误信息应指出缺失的核验依据: %v", err)
	}
}

func TestMatchExposureDefaultOn(t *testing.T) {
	var tc config.Transitions
	if !tc.MatchExposureEnabled() {
		t.Fatal("MatchExposure 未配置时应默认开启")
	}
	off := false
	tc.MatchExposure = &off
	if tc.MatchExposureEnabled() {
		t.Fatal("显式 false 应能关闭曝光匹配")
	}
}

func TestPeakDetailOnFailure(t *testing.T) {
	d := peakDetail(errTest{}, -12.3, -1)
	if !strings.Contains(d, "峰值测量失败") {
		t.Fatalf("测量失败的详情必须显式说明，实际: %s", d)
	}
	d = peakDetail(nil, -12.3, -1)
	if !strings.Contains(d, "-12.3") {
		t.Fatalf("正常详情应含实测值: %s", d)
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom" }

// 防止 filepath 导入裁剪告警（TempDir 返回值用于路径拼接断言）
var _ = filepath.Join
