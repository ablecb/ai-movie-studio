package skills

import (
	"strings"
	"testing"

	"aistudio/internal/skill"
)

// 结构工程回归：剧本/分镜的确定性校验是"故事成立"的机器下限。
// 真实案例：s3 的 action 写"晨光"、time_of_day 是"黄昏"——矛盾流进生成即穿帮。

func TestTimeOfDayContradiction(t *testing.T) {
	cases := []struct {
		tod, action string
		want        bool
	}{
		{"黄昏", "雨停，天际泛起微蓝的晨光", true}, // 真实事故案例
		{"夜", "窗外的晨光透过玻璃洒进来", true},  // 夜场写晨光
		{"日", "夜色笼罩着街道", true},       // 日场写夜色
		{"夜", "霓虹灯在雨夜里明明灭灭", false},  // 提到"夜"但一致
		{"黄昏", "夕阳把影子拉得很长", false},   // 一致
		{"", "任意描述", false},          // 缺时段不判
	}
	for _, c := range cases {
		got := timeOfDayContradiction(c.tod, c.action)
		if (got != "") != c.want {
			t.Errorf("tod=%q action=%q: 矛盾=%v（期望 %v）%s", c.tod, c.action, got != "", c.want, got)
		}
	}
}

func TestDurationTargetSec(t *testing.T) {
	ctx := &skill.Context{Params: map[string]any{"duration_min": "3"}}
	if got := durationTargetSec(ctx); got != 180 {
		t.Fatalf("3 分钟应为 180 秒，实际 %.0f", got)
	}
	ctx = &skill.Context{Params: map[string]any{"duration_min": "2.5"}}
	if got := durationTargetSec(ctx); got != 150 {
		t.Fatalf("2.5 分钟应为 150 秒，实际 %.0f", got)
	}
	// 非法/缺失：返回 0 = 不做预算检查
	for _, params := range []map[string]any{nil, {"duration_min": ""}, {"duration_min": "abc"}, {"duration_min": "0"}} {
		if got := durationTargetSec(&skill.Context{Params: params}); got != 0 {
			t.Fatalf("params=%v 应返回 0，实际 %.0f", params, got)
		}
	}
}

func TestCameraMoveEnumCoversPromspec(t *testing.T) {
	// promspec 的运镜表是镜头语言的"确定性下限"：分镜枚举必须覆盖它的每一个键，
	// 否则模型输出该运镜时拿不到英文运动描述与运动幅度映射
	for _, move := range []string{"固定", "缓推", "缓拉", "横移", "摇", "手持跟随", "航拍", "升降", "环绕"} {
		if !cameraMoveEnum[move] {
			t.Errorf("运镜 %q 在分镜枚举中缺失（promspec 有对应编译规则）", move)
		}
	}
}

func TestScriptSceneCarriesStructureFields(t *testing.T) {
	// mock 产物必须满足新校验（purpose/turn/矛盾），否则离线演示模式跑不通
	s := mockScript()
	for _, sc := range s.Scenes {
		if strings.TrimSpace(sc.Purpose) == "" {
			t.Errorf("场景 %s 缺少 purpose", sc.ID)
		}
		if c := timeOfDayContradiction(sc.TimeOfDay, sc.Action); c != "" {
			t.Errorf("场景 %s mock 数据时段矛盾: %s", sc.ID, c)
		}
	}
	hasTurn := false
	for _, sc := range s.Scenes {
		if strings.TrimSpace(sc.Turn) != "" {
			hasTurn = true
		}
	}
	if !hasTurn {
		t.Fatal("mock 剧本应至少有一场转折")
	}
}
