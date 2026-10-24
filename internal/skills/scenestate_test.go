package skills

import (
	"strings"
	"testing"
)

// 环境状态绑定的回归：scene_state 是"这一镜用哪张环境基准图"的开关——
// 没有声明状态清单时必须留空（渲染退回主基准），填了就必须命中清单。
// 真实代价：错的 slug 在渲染期要么硬失败，要么静默换一张基准图带偏整镜画面。
func TestValidateSceneState(t *testing.T) {
	states := map[string][]string{"s4": {"clouds-dispersed"}}

	cases := []struct {
		name    string
		state   string
		want    string
		wantErr string // 期望错误包含的片段；空 = 不应报错
	}{
		{"留空=初始状态", "", "", ""},
		{"空白视为留空", "  ", "", ""},
		{"命中声明状态", "clouds-dispersed", "clouds-dispersed", ""},
		{"两侧空白归一", " clouds-dispersed ", "clouds-dispersed", ""},
		{"越清单", "rain-night", "", "不在场次 s4 声明的状态清单内"},
		{"该场次无声明", "clouds-dispersed", "", "没有为场次 s1 声明任何状态变体"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sceneID := "s4"
			if tc.name == "该场次无声明" {
				sceneID = "s1"
			}
			got, err := validateSceneState("sh045", sceneID, tc.state, states)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				if got != tc.want {
					t.Fatalf("归一结果应为 %q，实际 %q", tc.want, got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应包含 %q，实际: %v", tc.wantErr, err)
			}
		})
	}

	// 无状态清单（未跑素材阶段）：任何非空值都必须被拒——渲染没有变体可绑
	if _, err := validateSceneState("sh001", "s4", "clouds-dispersed", nil); err == nil {
		t.Fatal("没有素材规划时非空 scene_state 应被拒绝")
	}
}
