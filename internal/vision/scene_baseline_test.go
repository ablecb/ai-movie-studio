package vision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aistudio/internal/config"
	"aistudio/internal/promspec"
)

// 场景基准的回归：单靠角色标准像只能查"人没换"，查不出"屋子换了"。
// 生成端拿哪张当环境锚（渲染期逐镜回填），复核端就必须拿哪张当比对基准。

func TestBatchSceneRefsDedupesByPathAndKeepsState(t *testing.T) {
	shotOf := map[string]promspec.ShotSpec{
		"sh001": {ID: "sh001", SceneID: "s4"},
		"sh002": {ID: "sh002", SceneID: "s4"}, // 同场次同状态：共用一张基准，只附一次
		"sh003": {ID: "sh003", SceneID: "s4"}, // 同场次不同状态：另一张基准必须单独附
		"sh004": {ID: "sh004", SceneID: "s5"}, // 无基准：跳过
	}
	baseOf := map[string]shotBaseline{
		"sh001": {Path: "/a/s4.png"},
		"sh002": {Path: "/a/s4.png"},
		"sh003": {Path: "/a/s4-dispersed.png", State: "clouds-dispersed"},
	}
	refs := batchSceneRefs(shotOf, []string{"sh001", "sh002", "sh003", "sh004"}, baseOf)
	if len(refs) != 2 {
		t.Fatalf("应按路径去重为 2 张基准图，实际 %d: %+v", len(refs), refs)
	}
	if refs[0].Name != "s4" || refs[0].Kind != "scene" || refs[0].Path != "/a/s4.png" || refs[0].State != "" {
		t.Fatalf("主基准条目错误: %+v", refs[0])
	}
	if refs[1].Path != "/a/s4-dispersed.png" || refs[1].State != "clouds-dispersed" {
		t.Fatalf("状态基准条目错误: %+v", refs[1])
	}
}

func TestBuildPromptDeclaresSceneBaseline(t *testing.T) {
	shotOf := map[string]promspec.ShotSpec{
		"sh001": {ID: "sh001", SceneID: "s1", Characters: []string{"林"}, Description: "中景"},
	}
	framesOf := map[string][]string{"sh001": {"f1.jpg"}}
	// 角色标准像 + 场次环境基准：两者都要声明图序，且各自有对应的比对段
	refs := []refLabel{
		{Name: "林", Path: "/r/lin.png"},
		{Name: "s1", Path: "/a/s1.png", Kind: "scene"},
	}
	p := buildPrompt(shotOf, []string{"sh001"}, framesOf, refs, 7.5)
	if !strings.Contains(p, "第 1 张图 = 角色「林」的标准像") {
		t.Errorf("缺角色标准像图序声明:\n%s", p)
	}
	if !strings.Contains(p, "第 2 张图 = 场次「s1」的环境基准图") {
		t.Errorf("缺场景基准图序声明:\n%s", p)
	}
	if !strings.Contains(p, "所属场景：s1") {
		t.Errorf("缺镜头所属场次，模型无从匹配对应基准图:\n%s", p)
	}

	// 只有场景基准（环境镜头）：不得要求身份比对——没有标准像却要求比对，
	// 模型会对着镜头帧自己编一个基准出来
	onlyScene := []refLabel{{Name: "s4", Path: "/a/s4.png", Kind: "scene", State: "clouds-dispersed"}}
	p = buildPrompt(shotOf, []string{"sh001"}, framesOf, onlyScene, 7.5)
	if strings.Contains(p, "标准像") {
		t.Errorf("无角色标准像时不应出现身份比对段:\n%s", p)
	}
	if !strings.Contains(p, "环境基准图") {
		t.Errorf("有场景基准时必须写明环境比对要求:\n%s", p)
	}
	// 状态必须写进图序声明：同场次可能有主基准与状态基准两张，不区分会拿错
	if !strings.Contains(p, "状态：clouds-dispersed") {
		t.Errorf("状态基准的图序声明缺少状态标注:\n%s", p)
	}

	// 镜头自带环境状态时要在镜头块里给出，模型才能把镜头与对应的状态基准对上
	shotOf["sh001"] = promspec.ShotSpec{ID: "sh001", SceneID: "s4", SceneState: "clouds-dispersed"}
	p = buildPrompt(shotOf, []string{"sh001"}, framesOf, onlyScene, 7.5)
	if !strings.Contains(p, "所属场景：s4（环境状态：clouds-dispersed）") {
		t.Errorf("镜头块缺少环境状态，模型无从匹配状态基准:\n%s", p)
	}
}

func TestLoadShotSceneBaselines(t *testing.T) {
	dir := t.TempDir()
	if got := loadShotSceneBaselines(dir); len(got) != 0 {
		t.Fatalf("无 shotplan.json 时应返回空映射，实际 %v", got)
	}

	base := filepath.ToSlash(filepath.Join("output", "assets", "scenes", "s4.png"))
	abs := filepath.Join(dir, filepath.FromSlash(base))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(dir, "output", "shotplan.json"), map[string]any{
		"shots": []map[string]any{
			{"shot_id": "sh036", "scene_ref_image": base},
			{"shot_id": "sh045", "scene_ref_image": base, "scene_ref_state": "clouds-dispersed"},
			{"shot_id": "sh046", "scene_ref_image": "output/assets/scenes/missing.png"}, // 文件不在盘：跳过
			{"shot_id": "sh047"}, // 未锚定（旧 run / 未生成素材）：跳过
		},
	})
	got := loadShotSceneBaselines(dir)
	if len(got) != 2 {
		t.Fatalf("应映射文件在盘上的逐镜基准，实际 %v", got)
	}
	if got["sh036"].Path != abs || got["sh036"].State != "" {
		t.Fatalf("主基准条目错误: %+v", got["sh036"])
	}
	// 状态随基准一起带给复核端：镜头块与图序说明靠它对上
	if got["sh045"].Path != abs || got["sh045"].State != "clouds-dispersed" {
		t.Fatalf("状态条目错误: %+v", got["sh045"])
	}
	if _, ok := got["sh046"]; ok {
		t.Fatalf("文件缺失的镜头不应进入比对基准（比对会指向不存在的图）: %v", got)
	}
}

func TestMergeResponseSceneDimensionVetoes(t *testing.T) {
	// 用真实 JSON 走一遍解析：字段名与 schema 的一致性也一并锁住
	var resp response
	if err := json.Unmarshal([]byte(`{"score":8.8,"shots":[
	  {"id":"sh001","score":8.8,"pass":true,"identity":9.0,"anatomy":9.0,"cinematic":9.0,"scene":5.0,"issues":["背景与场景基准不符"],"fix":"按场景基准图重渲"}
	]}`), &resp); err != nil {
		t.Fatal(err)
	}
	rep := &Report{}
	mergeResponse(rep, resp, []string{"sh001"}, 7.5)
	if len(rep.Shots) != 1 {
		t.Fatalf("应有 1 条结论: %+v", rep.Shots)
	}
	if rep.Shots[0].Pass {
		t.Fatal("场景分 5.0 低于达标线，总分再高也不能 pass——屋子换了不能进成片")
	}
	if rep.Shots[0].Scene == nil || *rep.Shots[0].Scene != 5.0 {
		t.Fatalf("场景分未透传: %+v", rep.Shots[0])
	}
}

func TestFinalizeSceneAvgAndCalibrationNote(t *testing.T) {
	scene, cinematic := 6.0, 8.6
	rep := &Report{Shots: []ShotReview{
		{ShotID: "sh001", Score: 6.8, Pass: false, Scene: &scene, Cinematic: &cinematic},
		{ShotID: "sh002", Score: 6.9, Pass: false, Scene: &scene, Cinematic: &cinematic},
	}}
	finalize(rep, config.Default(), 0)
	if rep.SceneAvg == nil || *rep.SceneAvg != scene {
		t.Fatalf("场景维度均分应聚合，实际 %v", rep.SceneAvg)
	}
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "环境一致性均分") && strings.Contains(n, "assets/scenes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("环境维度显著垫底时应给出优先核对场景基准图的校准线索: %v", rep.Notes)
	}
}
