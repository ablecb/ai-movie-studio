package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/selfheal"
	"aistudio/internal/skill"
)

func testConfig() *config.Config { return config.Default() }

func mockClient() *ai.Client {
	return ai.New(ai.Config{Mock: true})
}

func testMemory(t *testing.T) *selfheal.Memory {
	t.Helper()
	mem, err := selfheal.LoadMemory(filepath.Join(t.TempDir(), "lessons.json"))
	if err != nil {
		t.Fatalf("构造经验记忆库失败: %v", err)
	}
	return mem
}

func testWorkflow() *Workflow {
	wf, err := LoadWorkflow("../../workflows/movie.json")
	if err != nil {
		panic(err) // 仓库内工作流损坏时直接 panic，让所有测试一起红
	}
	return wf
}

// Resume 必须为"工作流里有、state.json 里没有"的阶段补种状态：
// 否则 readyStages 对 nil 解引用，断点续跑直接 panic（真实事故场景：
// 工作流加了新阶段后对旧 run 执行 resume）。
func TestResumeSeedsMissingStages(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()

	// 造一份"只有 script 阶段"的旧状态文件（模拟老版本 state.json）
	old := RunState{
		ID: "run-test", Workflow: wf.Name, Brief: "测试", Mock: true,
		Stages: map[string]*StageState{
			wf.Stages[0].ID: {Status: StatusDone, OutputFile: "artifacts/script.json"},
		},
	}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// script 标记 done 就必须有产物（否则下游依赖加载失败是正确行为）
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptJSON := `{"title":"t","logline":"l","scenes":[
		{"id":"s1","location":"a","time_of_day":"夜","purpose":"p","action":"a","dialogue":[{"character":"信使·林","line":"你好"}]},
		{"id":"s2","location":"b","time_of_day":"日","purpose":"p2","action":"b","dialogue":[{"character":"信使·林","line":"再见"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "artifacts", "script.json"), []byte(scriptJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	e, err := Resume(dir, wf, mockClient(), testConfig(), testMemory(t))
	if err != nil {
		t.Fatal(err)
	}
	// 关键验证：Run 全程不 panic，且除已完成的首阶段外全部可推进到终态
	if err := e.Run(); err != nil {
		t.Fatalf("Resume 后 Run 不应失败: %v", err)
	}
	for i, s := range wf.Stages {
		st := e.State.Stages[s.ID]
		if st == nil {
			t.Fatalf("阶段 %s 仍未补种状态", s.ID)
		}
		if i == 0 {
			continue
		}
		if st.Status != StatusDone && st.Status != StatusSkipped {
			t.Fatalf("阶段 %s 状态异常: %s", s.ID, st.Status)
		}
	}
}

// 审片产物损坏（非法 JSON）→ 重跑审片阶段本身，绝不把零值结论
// 当作"0 分不达标"烧光返修轮数。
func TestHandleReviewCorruptArtifact(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()
	e, err := NewRun(wf, mockClient(), testConfig(), testMemory(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	var review *Stage
	for i := range wf.Stages {
		if wf.Stages[i].Skill == "review" {
			review = &wf.Stages[i]
		}
	}
	if review == nil {
		t.Fatal("工作流缺少 review 阶段")
	}
	e.State.Stages[review.ID] = &StageState{Status: StatusDone, OutputFile: "artifacts/" + review.ID + ".json", Revisions: 0}
	if err := os.MkdirAll(filepath.Join(e.Dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Dir, "artifacts", review.ID+".json"), []byte("{broken json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.handleReview(review, e.State.Stages[review.ID]); err != nil {
		t.Fatalf("损坏产物应被妥善处理而非报错: %v", err)
	}
	st := e.State.Stages[review.ID]
	if st.Status != StatusPending {
		t.Fatalf("损坏产物应触发审片重跑（pending），实际 %s", st.Status)
	}
	if st.Revisions != 0 {
		t.Fatalf("损坏产物不应消耗返修轮数，实际已消耗 %d", st.Revisions)
	}
}

// 审片产物为空对象（解析成功但零值）同样不可信：真实的不达标审片必然带分数与意见。
func TestHandleReviewEmptyArtifact(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()
	e, err := NewRun(wf, mockClient(), testConfig(), testMemory(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	var review *Stage
	for i := range wf.Stages {
		if wf.Stages[i].Skill == "review" {
			review = &wf.Stages[i]
		}
	}
	e.State.Stages[review.ID] = &StageState{Status: StatusDone, OutputFile: "artifacts/" + review.ID + ".json"}
	if err := os.MkdirAll(filepath.Join(e.Dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Dir, "artifacts", review.ID+".json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.handleReview(review, e.State.Stages[review.ID]); err != nil {
		t.Fatal(err)
	}
	st := e.State.Stages[review.ID]
	if st.Status != StatusPending || st.Revisions != 0 {
		t.Fatalf("空审片产物应重跑审片且不消耗返修轮数，实际 status=%s revisions=%d", st.Status, st.Revisions)
	}
}

// 审片不达标 → 定向打回：受影响阶段置回 pending、返修意见登记、轮数+1。
func TestHandleReviewDirectsRevision(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()
	e, err := NewRun(wf, mockClient(), testConfig(), testMemory(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	var review *Stage
	reviewIdx := -1
	for i := range wf.Stages {
		if wf.Stages[i].Skill == "review" {
			review, reviewIdx = &wf.Stages[i], i
		}
	}
	for i := 0; i <= reviewIdx; i++ {
		e.State.Stages[wf.Stages[i].ID] = &StageState{Status: StatusDone, OutputFile: "artifacts/" + wf.Stages[i].ID + ".json"}
	}
	if err := os.MkdirAll(filepath.Join(e.Dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := map[string]any{
		"score": 6.5, "pass": false, "feedback": "第二幕缺反应镜头",
		"issues": []map[string]any{{"stage": "storyboard", "severity": "major", "note": "缺反应镜头"}},
	}
	raw, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(e.Dir, "artifacts", review.ID+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.handleReview(review, e.State.Stages[review.ID]); err != nil {
		t.Fatal(err)
	}
	st := e.State.Stages[review.ID]
	if st.Revisions != 1 {
		t.Fatalf("返修轮数应为 1，实际 %d", st.Revisions)
	}
	// 打回范围应扩大到意见点名的 storyboard（比默认 target 更早则取更早者）
	sbSt := e.State.Stages["storyboard"]
	if sbSt == nil || sbSt.Status != StatusPending {
		t.Fatalf("storyboard 应被打回重做")
	}
	if fb := e.feedback["storyboard"]; !strings.Contains(fb, "反应镜头") {
		t.Fatalf("返修意见未登记到 storyboard: %q", fb)
	}
}

// 全链路冒烟：mock 模式下完整跑一遍工作流（含 mock 审片首轮压分 → 返修 → 二轮放行）。
// 这是 engine 唯一能证明"调度/重试/返修/级联跳过串起来是对的"的测试。
func TestRunMockWorkflowEndToEnd(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()
	e, err := NewRun(wf, mockClient(), testConfig(), testMemory(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatalf("mock 全流程不应失败: %v", err)
	}
	for _, s := range wf.Stages {
		st := e.State.Stages[s.ID]
		if st.Status != StatusDone {
			t.Fatalf("mock 模式下阶段 %s 应完成，实际 %s（%s）", s.ID, st.Status, st.Error)
		}
		if _, err := os.Stat(filepath.Join(e.Dir, filepath.FromSlash(st.OutputFile))); err != nil {
			t.Fatalf("阶段 %s 产物缺失: %v", s.ID, err)
		}
	}
	// mock 审片首轮必失败：返修闭环应真实发生
	if e.State.Stages["review"].Revisions < 1 {
		t.Fatalf("mock 审片首轮压分应触发返修，实际返修轮数 %d", e.State.Stages["review"].Revisions)
	}
}

// 状态快照在 Run 中途即落盘（每个阶段完成后），保证断点续跑不丢已完成阶段。
func TestStatePersisted(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()
	e, err := NewRun(wf, mockClient(), testConfig(), testMemory(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	var st RunState
	if err := json.Unmarshal(mustRead(t, filepath.Join(e.Dir, "state.json")), &st); err != nil {
		t.Fatalf("state.json 应可反序列化: %v", err)
	}
	if len(st.Stages) != len(wf.Stages) {
		t.Fatalf("state.json 应包含全部阶段状态")
	}
	if st.UpdatedAt.IsZero() {
		t.Fatal("state.json 缺少 updated_at")
	}
}

// 注册表装配完整（工作流引用的技能全部已注册）。
func TestRegistryCoversWorkflow(t *testing.T) {
	reg := skill.NewRegistry()
	// RegisterAll 由 engine.buildRegistry 装配；这里直接验证工作流引用
	e := &Engine{Reg: buildRegistry()}
	for _, s := range testWorkflow().Stages {
		if _, ok := e.Reg.Get(s.Skill); !ok {
			t.Fatalf("工作流引用的技能 %s 未注册", s.Skill)
		}
	}
	_ = reg
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// RerunFrom：全流程跑完后人工重跑某阶段。窗口内（≤until）的下游本次直接
// 排队重跑；窗口外的已完成下游标记为"需重做"；上游与无关分支不受牵连。
func TestRerunFromWindowSemantics(t *testing.T) {
	dir := t.TempDir()
	wf := testWorkflow()
	e, err := NewRun(wf, mockClient(), testConfig(), testMemory(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(); err != nil {
		t.Fatalf("mock 全流程不应失败: %v", err)
	}
	for _, s := range wf.Stages {
		if st := e.State.Stages[s.ID]; st.Status != StatusDone {
			t.Fatalf("前置条件：阶段 %s 应为 done，实际 %s", s.ID, st.Status)
		}
	}

	// 场景一：重跑单阶段（from == until）——该阶段排队，下游标记需重做
	if err := e.RerunFrom("characters", "characters"); err != nil {
		t.Fatal(err)
	}
	if st := e.State.Stages["script"]; st.Status != StatusDone {
		t.Fatalf("上游 script 不应被牵连，实际 %s", st.Status)
	}
	if st := e.State.Stages["characters"]; st.Status != StatusPending {
		t.Fatalf("characters 应重置为 pending，实际 %s", st.Status)
	}
	for _, id := range []string{"assets", "storyboard", "voiceover", "music", "shotspec", "subtitles", "editplan", "review"} {
		st := e.State.Stages[id]
		if st.Status != StatusSkipped || !strings.Contains(st.RepairNote, "上游 characters 已重跑") {
			t.Fatalf("窗口外下游 %s 应标记需重做，实际 %s / %q", id, st.Status, st.RepairNote)
		}
	}
	// 单阶段重跑后 Run：只重做 characters，窗口外下游保持"需重做"等下次续跑
	if err := e.Run(); err != nil {
		t.Fatalf("单阶段重跑不应失败: %v", err)
	}
	if st := e.State.Stages["characters"]; st.Status != StatusDone {
		t.Fatalf("characters 重跑后应为 done，实际 %s", st.Status)
	}
	if st := e.State.Stages["assets"]; st.Status != StatusSkipped {
		t.Fatalf("窗口外下游本次不应被重跑，实际 %s", st.Status)
	}

	// 场景二：无 until（窗口到末尾）——下游全部排队，一次 Run 全量重做
	if err := e.RerunFrom("characters", ""); err != nil {
		t.Fatal(err)
	}
	if st := e.State.Stages["assets"]; st.Status != StatusPending {
		t.Fatalf("窗口内下游 assets 应排队重跑，实际 %s", st.Status)
	}
	if err := e.Run(); err != nil {
		t.Fatalf("全量重跑不应失败: %v", err)
	}
	for _, s := range wf.Stages {
		if st := e.State.Stages[s.ID]; st.Status != StatusDone {
			t.Fatalf("全量重跑后阶段 %s 应回到 done，实际 %s", s.ID, st.Status)
		}
	}

	// 场景三：下游闭包不越界——重跑 music 只波及 music 与依赖它的 review，
	// 平行分支 subtitles/editplan 保持 done
	if err := e.RerunFrom("music", ""); err != nil {
		t.Fatal(err)
	}
	if st := e.State.Stages["subtitles"]; st.Status != StatusDone {
		t.Fatalf("无关分支 subtitles 不应被重置，实际 %s", st.Status)
	}
	if st := e.State.Stages["editplan"]; st.Status != StatusDone {
		t.Fatalf("无关分支 editplan 不应被重置，实际 %s", st.Status)
	}
	if st := e.State.Stages["review"]; st.Status != StatusPending {
		t.Fatalf("依赖 music 的 review 应排队重跑，实际 %s", st.Status)
	}
}
