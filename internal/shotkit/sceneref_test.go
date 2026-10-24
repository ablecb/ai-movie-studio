package shotkit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 跨包 JSON 契约：渲染写 shotplan.json、视觉复核按这些键名读。
// 键名改动必须在这里显式暴露——否则复核会静默地"找不到基准图"退回跨镜头对比，
// 而报告上只表现为少一条说明。
func TestTaskSceneRefJSONKeys(t *testing.T) {
	b, err := json.Marshal(Task{ShotID: "sh045", SceneRefImage: "output/assets/scenes/s4_clouds-dispersed.png", SceneRefState: "clouds-dispersed"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{`"shot_id"`, `"scene_ref_image"`, `"scene_ref_state"`} {
		if !strings.Contains(s, key) {
			t.Errorf("shotplan.json 契约键 %s 缺失: %s", key, s)
		}
	}
}

// 场景基准图选择的回归：同地点同时段归并共享的那张（Variant 为空）才是
// "跨镜头同一个环境"的锚。状态变体（雨夜/被毁…）属于特定镜头的美术状态，
// 由分镜决定用哪一张——渲染管线替它挑错比不挑更坏。
func TestBaselineOfPrefersBaseVariant(t *testing.T) {
	ix := &AssetIndex{Scenes: []SceneAsset{
		{SceneID: "s1", Variant: "rain-night", File: "output/assets/scenes/s1-rain.png"},
		{SceneID: "s1", File: "output/assets/scenes/s1.png"},
		{SceneID: "s2", Variant: "destroyed", File: "output/assets/scenes/s2-destroyed.png"},
		{SceneID: "s3", File: ""}, // 失败条目（无文件）：不能当基准
	}}
	if got := ix.BaselineOf("s1"); got != "output/assets/scenes/s1.png" {
		t.Errorf("应选主基准（Variant 为空），实际 %q", got)
	}
	if got := ix.BaselineOf("s2"); got != "output/assets/scenes/s2-destroyed.png" {
		t.Errorf("只有状态变体时应退回该场次第一个带文件的条目，实际 %q", got)
	}
	if got := ix.BaselineOf("s3"); got != "" {
		t.Errorf("无文件的条目不能作为基准，实际 %q", got)
	}
	if got := ix.BaselineOf("s9"); got != "" {
		t.Errorf("未知场次应返回空，实际 %q", got)
	}
	if got := ix.BaselineOf(""); got != "" {
		t.Errorf("空场次应返回空，实际 %q", got)
	}
}

// 状态变体查找：shotspec 声明的 scene_state 必须精确命中该场次的变体文件——
// 命中失败由调用方显式报错（静默回退主基准 = 锚到另一个状态）。
func TestVariantOf(t *testing.T) {
	ix := &AssetIndex{Scenes: []SceneAsset{
		{SceneID: "s4", File: "output/assets/scenes/s4.png"},
		{SceneID: "s4", Variant: "clouds-dispersed", File: "output/assets/scenes/s4_clouds-dispersed.png"},
		{SceneID: "s4", Variant: "sky-cleared", File: ""}, // 无文件的条目不作数
	}}
	if got := ix.VariantOf("s4", "clouds-dispersed"); got != "output/assets/scenes/s4_clouds-dispersed.png" {
		t.Errorf("应命中状态变体，实际 %q", got)
	}
	if got := ix.VariantOf("s4", "main"); got != "" {
		t.Errorf("主基准不是变体（Variant 为空），应返回空，实际 %q", got)
	}
	if got := ix.VariantOf("s4", "sky-cleared"); got != "" {
		t.Errorf("无文件的条目不能命中，实际 %q", got)
	}
	if got := ix.VariantOf("s9", "clouds-dispersed"); got != "" {
		t.Errorf("未知场次应返回空，实际 %q", got)
	}
	if got := ix.VariantOf("", "clouds-dispersed"); got != "" {
		t.Errorf("空场次应返回空，实际 %q", got)
	}
}
