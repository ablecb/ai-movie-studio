package shotkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aistudio/internal/ai"
	"aistudio/internal/promspec"
)

// 规划调用的输出预算回归：思考型模型（glm-5.3-flash）的思考内容计入输出预算，
// assets.plan 是"长提示词 + 结构化 JSON"，走全局默认（4096）时思考会把预算耗尽、
// 正文一个字都写不出来（真实事故：两次尝试的 max_tokens 都是 4096，正文为空）。
// 该调用点不在 skills 的 callTune 表内，预算由调用处显式保证——本测试锁住它。
func TestPlanAssetsBudget(t *testing.T) {
	type sentReq struct {
		MaxTokens   int     `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
		Stream      bool    `json:"stream"`
	}
	var got sentReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sentReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体解析失败: %v", err)
		}
		got = req
		body := `{"characters":[{"name":"林夏","frames":[{"variant":"base","purpose":"正面标准像"}]}],"scene_states":[]}`
		chunk, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"delta": map[string]string{"content": body}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 50},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer srv.Close()

	// 全局默认故意设成事故现场的值（4096）：预算必须来自调用点的显式覆盖
	client := ai.New(ai.Config{BaseURL: srv.URL, APIKey: "test-key", Model: "glm-5.3-flash", MaxTokens: 4096})
	plan, err := PlanAssets(client, "测试 brief",
		[]SceneInput{{ID: "s1", Location: "天台", TimeOfDay: "夜", Action: "她站在护栏边"}},
		[]promspec.CharacterSpec{{Name: "林夏", Token: "lin-xia"}}, nil)
	if err != nil {
		t.Fatalf("规划调用不应失败: %v", err)
	}
	if !got.Stream {
		t.Fatalf("chat 调用应走流式（思考可达数分钟，非流式会被空闲超时掐死）")
	}
	if got.MaxTokens < 8192 {
		t.Fatalf("规划调用输出预算过低（max_tokens=%d）：思考型模型的思考会耗尽预算、正文为空", got.MaxTokens)
	}
	if got.Temperature > 0.6 {
		t.Fatalf("结构化产出应低温以减少 JSON 抖动（得到 %.2f）", got.Temperature)
	}
	if len(plan.Characters) != 1 || plan.Characters[0].Name != "林夏" {
		t.Fatalf("规划结果异常: %+v", plan.Characters)
	}
}

func TestFramesForNilPlan(t *testing.T) {
	var plan *AssetPlan
	frames := plan.FramesFor("林夏")
	if len(frames) != 1 || frames[0].Variant != "base" {
		t.Fatalf("nil 计划应返回单张 base，得到 %v", frames)
	}
}

func TestSanitizePlan(t *testing.T) {
	chars := []promspec.CharacterSpec{
		{Name: "林夏", Token: "lin-xia"},
		{Name: "K0-9", Token: "k09"},
		{Name: "路人", Token: "passerby"},
	}
	scenes := []SceneInput{
		{ID: "s1", Location: "天台", TimeOfDay: "夜"},
		{ID: "s2", Location: "天台", TimeOfDay: "夜"},
		{ID: "s3", Location: "仓库", TimeOfDay: "日"},
	}
	raw := &rawPlan{
		Characters: []PlannedChar{
			// 名称宽松匹配 + 非法 slug 丢弃 + 已知变体补默认指令 + 超限裁剪
			{Name: "林", Frames: []RefFrameSpec{
				{Variant: "side", Purpose: "转身镜头"},               // 已知角度，无 directive 应补默认
				{Variant: "Base"},                                // 非法大小写：base 重复，丢弃
				{Variant: "Wardrobe X", DirectiveEN: "raincoat"}, // 非法 slug，丢弃
				{Variant: "wardrobe-rain", DirectiveEN: "wearing a transparent raincoat"},
				{Variant: "state-drunk", DirectiveEN: "drunk, flushed face"}, // 第 4 帧，保留
				{Variant: "closeup", DirectiveEN: "eyes only"},               // 第 5 帧，超限裁剪
			}},
			// 未知变体且无英文指令：丢弃
			{Name: "K0-9", Frames: []RefFrameSpec{{Variant: "floating", Purpose: "悬浮"}}},
			// 路人未出现在规划里 → 补 base
		},
		SceneStates: []PlannedSceneState{
			{SceneIDs: []string{"s1", "s2", "s9"}, State: "Rain Night", DirectiveEN: "heavy rain"}, // 非法 slug + 未知场次
			{SceneIDs: []string{"s3"}, State: "destroyed", Purpose: "被摧毁后"},
			{SceneIDs: []string{"s3"}, State: "destroyed", Purpose: "重复条目"},
		},
	}
	logs := []string{}
	plan := sanitizePlan(raw, scenes, chars, func(f string, a ...any) {
		logs = append(logs, f)
	})

	byName := map[string]PlannedChar{}
	for _, pc := range plan.Characters {
		byName[pc.Name] = pc
	}
	lin := byName["林夏"]
	if len(lin.Frames) != MaxFramesPerChar {
		t.Fatalf("林夏应保留 base + 3 个合法变体（超限裁剪），得到 %d 帧", len(lin.Frames))
	}
	if lin.Frames[0].Variant != "base" {
		t.Fatalf("base 必须在首位，得到 %q", lin.Frames[0].Variant)
	}
	if lin.Frames[1].Variant != "side" || lin.Frames[1].DirectiveEN == "" {
		t.Fatalf("已知角度变体应补默认英文指令，得到 %+v", lin.Frames[1])
	}
	if _, ok := byName["K0-9"]; !ok {
		t.Fatal("规划里名称宽松匹配失败：K0-9 丢失")
	}
	if len(byName["K0-9"].Frames) != 1 || byName["K0-9"].Frames[0].Variant != "base" {
		t.Fatalf("无英文指令的未知变体应被丢弃、只留 base，得到 %+v", byName["K0-9"].Frames)
	}
	if _, ok := byName["路人"]; !ok {
		t.Fatal("规划漏掉的角色应补 base")
	}
	if len(plan.SceneStates) != 1 || plan.SceneStates[0].State != "destroyed" || len(plan.SceneStates[0].SceneIDs) != 1 {
		t.Fatalf("场景状态应清洗为 1 条（非法 slug 丢弃、重复去重），得到 %+v", plan.SceneStates)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "超过") && !strings.Contains(joined, "非法") {
		t.Fatalf("清洗过程应有告警日志，得到: %s", joined)
	}
}

func TestGroupSceneInputs(t *testing.T) {
	scenes := []SceneInput{
		{ID: "s1", Location: "烂尾楼天台", TimeOfDay: "夜"},
		{ID: "s2", Location: "烂尾楼 天台", TimeOfDay: "夜"}, // 空格差异应归并
		{ID: "s3", Location: "烂尾楼天台", TimeOfDay: "日"},  // 时段不同不归并
	}
	groups := groupSceneInputs(scenes)
	if len(groups) != 2 {
		t.Fatalf("同地点同时段应归并为一组，得到 %d 组: %+v", len(groups), groups)
	}
	if groups[0].Rep != 0 || len(groups[0].Members) != 2 {
		t.Fatalf("组代表应是剧本顺序第一个，成员含 s2: %+v", groups[0])
	}
	if CountSceneGroups(scenes) != 2 {
		t.Fatal("CountSceneGroups 与 groupSceneInputs 不一致")
	}
}
