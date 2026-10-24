package shotkit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/promspec"
)

// 审核链路回归：参考帧质检不能是黑盒——每轮候选图（含被否掉的）必须归档、
// 核对结论（分数/缺失项/修正指令）必须逐轮落进 RefResult.QCReview。
// 断言的是人工能看到的审计事实："哪张图没过、改成什么样、几轮通过"。
func TestEnsureRefFramesQCReviewChain(t *testing.T) {
	// 视觉核对脚本：第 1 次不通过（缺圆框眼镜），第 2 次通过
	verdicts := []string{
		`{"pass":false,"score":6,"missing":["圆框眼镜"],"drift":[],"issues":["手部畸形"],"fix_hint":"add black round-frame glasses"}`,
		`{"pass":true,"score":8.5,"missing":[],"drift":[],"issues":[],"fix_hint":""}`,
	}
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		if visionCall <= len(verdicts) {
			return 200, verdicts[visionCall-1]
		}
		return 200, verdicts[len(verdicts)-1]
	})
	defer srv.Close()

	runDir := t.TempDir()
	res := runRefChain(t, srv.URL, runDir, 2)

	if *genCount != 2 {
		t.Fatalf("不通过后应重出一轮（共 2 次出图），实际 %d 次", *genCount)
	}
	if *visionCount != 2 {
		t.Fatalf("每轮出图后都应核对（共 2 次），实际 %d 次", *visionCount)
	}
	if len(res) != 1 {
		t.Fatalf("应返回 1 个角色的结果，得到 %d", len(res))
	}
	r := res[0]
	if r.Error != "" {
		t.Fatalf("重出后通过不应有错误: %s", r.Error)
	}
	if r.QCStatus != "corrected" {
		t.Fatalf("重出后通过应记为 corrected，得到 %q", r.QCStatus)
	}
	if len(r.QCReview) != 2 {
		t.Fatalf("审核链路应有 2 轮，得到 %d 轮", len(r.QCReview))
	}

	r0, r1 := r.QCReview[0], r.QCReview[1]
	// 第 0 轮：被否的候选图必须留档（主路径会被重出覆盖）
	if r0.Verdict == nil || r0.Verdict.Pass {
		t.Fatalf("第 0 轮应记录未通过的核对结论: %+v", r0.Verdict)
	}
	if r0.Verdict.Score != 6 || len(r0.Verdict.Missing) == 0 {
		t.Fatalf("第 0 轮结论应带分数与缺失项: %+v", r0.Verdict)
	}
	if r0.Final {
		t.Fatal("被否掉的一轮不应标记为最终采用")
	}
	if r0.FixHintEN == "" || !strings.Contains(r0.FixHintEN, "round-frame glasses") {
		t.Fatalf("被否后应记录追加给出图提示词的修正指令，得到 %q", r0.FixHintEN)
	}
	if r0.Image == "" {
		t.Fatal("第 0 轮候选图应有存档路径")
	}
	archived, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(r0.Image)))
	if err != nil {
		t.Fatalf("第 0 轮候选图存档不存在: %v", err)
	}
	if !strings.Contains(string(archived), "img-1") {
		t.Fatalf("第 0 轮存档应是第 1 张候选图，得到 %q", string(archived))
	}

	// 第 1 轮：通过并标记为最终采用；主路径文件是重出后的图
	if r1.Verdict == nil || !r1.Verdict.Pass || !r1.Final {
		t.Fatalf("第 1 轮应为通过且最终采用: %+v", r1)
	}
	if r1.Image == "" {
		t.Fatal("第 1 轮候选图也应存档（审核链路自包含）")
	}
	final, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(r.Path)))
	if err != nil {
		t.Fatalf("主路径参考帧缺失: %v", err)
	}
	if !strings.Contains(string(final), "img-2") {
		t.Fatalf("主路径应是重出后的第 2 张图，得到 %q", string(final))
	}
	r1Archived, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(r1.Image)))
	if err != nil || string(r1Archived) != string(final) {
		t.Fatalf("第 1 轮存档应与主路径同为最终采用的图: %v", err)
	}

	// 复用路径也必须带回完整链路（断点续跑不丢审计）：
	// 调用方落盘索引（真实流程由 assets 阶段执行），再次运行走复用分支
	ix := LoadAssetIndex(runDir)
	ix.MergeCharacters(res)
	if err := ix.Save(runDir); err != nil {
		t.Fatalf("保存素材索引失败: %v", err)
	}
	res2 := runRefChain(t, srv.URL, runDir, 2)
	if len(res2) != 1 || len(res2[0].QCReview) != 2 || res2[0].QCStatus != "corrected" {
		t.Fatalf("复用既有参考帧应带回审核链路: %+v", res2[0].QCReview)
	}
}

// 质检调用失败（如 HTTP 400）：链路必须留下"哪一轮、什么错误"，
// 且严格模式拒绝继续——"以为查过、其实没查"绝不允许。
func TestEnsureRefFramesQCErrorLeavesTrail(t *testing.T) {
	srv, genCount, _ := newQCChainServer(t, func(visionCall int) (int, string) {
		return 400, `{"error":{"message":"API 调用参数有误，请检查文档。"}}`
	})
	defer srv.Close()

	runDir := t.TempDir()
	cfg, imgc, vision := chainClients(srv.URL, 1)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: "yangjian"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err == nil {
		t.Fatal("严格模式下质检调用失败应让该角色失败")
	}
	if *genCount != 1 {
		t.Fatalf("质检失败不应自动重出（那是另一码事），出图次数 %d", *genCount)
	}
	if len(res) != 1 || !strings.Contains(res[0].Error, "参考帧质检调用失败") {
		t.Fatalf("结果应带质检失败原因: %+v", res)
	}
	if len(res[0].QCReview) != 1 {
		t.Fatalf("审核链路应留下失败的那一轮，得到 %d 轮", len(res[0].QCReview))
	}
	row := res[0].QCReview[0]
	if row.QCError == "" || !strings.Contains(row.QCError, "400") {
		t.Fatalf("链路条目应记录质检调用失败的原因: %+v", row)
	}
	if row.Image == "" {
		t.Fatal("即使质检失败，该轮候选图也应留档（人工要能看图定位问题）")
	}
	if _, err := os.Stat(filepath.Join(runDir, filepath.FromSlash(row.Image))); err != nil {
		t.Fatalf("链路条目指向的候选图不存在: %v", err)
	}
}

// 重出预算用尽仍不合格：链路应逐轮留痕（每轮都有结论），且没有任何一轮标记为采用。
func TestEnsureRefFramesRoundsExhausted(t *testing.T) {
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, `{"pass":false,"score":5.5,"missing":["九齿钉耙"],"drift":["狐裘"],"issues":[],"fix_hint":"restore the nine-toothed rake"}`
	})
	defer srv.Close()

	runDir := t.TempDir()
	cfg, imgc, vision := chainClients(srv.URL, 2) // MaxRounds=2 → 首出 + 2 轮重出 = 3 轮
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "猪八戒", Token: "boar"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err == nil {
		t.Fatal("重出用尽仍不合格应拒绝继续")
	}
	if *genCount != 3 || *visionCount != 3 {
		t.Fatalf("应为首出 + 2 轮重出（3 次出图/核对），得到 出图 %d / 核对 %d", *genCount, *visionCount)
	}
	if len(res) != 1 || res[0].QCStatus != "failed" {
		t.Fatalf("应记 failed: %+v", res)
	}
	rows := res[0].QCReview
	if len(rows) != 3 {
		t.Fatalf("链路应有 3 轮，得到 %d", len(rows))
	}
	for i, row := range rows {
		if row.Verdict == nil || row.Verdict.Pass {
			t.Fatalf("第 %d 轮应记录未通过结论: %+v", i, row)
		}
		if row.Final {
			t.Fatalf("第 %d 轮不应标记为最终采用（没有任何一轮通过）", i)
		}
		if i < len(rows)-1 && row.FixHintEN == "" {
			t.Fatalf("第 %d 轮被否后应留下修正指令（供下一轮重出）", i)
		}
		if row.Image == "" {
			t.Fatalf("第 %d 轮候选图应留档", i)
		}
	}
}

// 复用帧补查：上次运行质检未完成（被取消/调用失败/旧版生成）的既有参考帧，
// 恢复时必须补核对——"没查过"绝不能表现为"通过"。
// 补查通过：不出图（0 次出图、1 次核对），链路留下复用帧这一轮。
func TestEnsureRefFramesRecheckReusedFrame(t *testing.T) {
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, `{"pass":true,"score":7.5,"missing":[],"drift":[],"issues":[],"fix_hint":""}`
	})
	defer srv.Close()

	runDir := t.TempDir()
	writeExistingRef(t, runDir, "yangjian", "EXISTING-FRAME-BYTES")
	cfg, imgc, vision := chainClients(srv.URL, 2)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: "yangjian"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err != nil {
		t.Fatalf("复用帧补查通过后应继续: %v", err)
	}
	if *genCount != 0 {
		t.Fatalf("补查不应重新出图，实际出图 %d 次", *genCount)
	}
	if *visionCount != 1 {
		t.Fatalf("补查应恰好核对 1 次，实际 %d 次", *visionCount)
	}
	r := res[0]
	if r.QCStatus != "pass" || len(r.QCReview) != 1 {
		t.Fatalf("补查结论应记为 pass 且链路 1 轮: %+v", r)
	}
	row := r.QCReview[0]
	if !row.Reused || !row.Final || row.Verdict == nil || !row.Verdict.Pass {
		t.Fatalf("链路应标记复用补查且为最终采用: %+v", row)
	}
	if row.Image == "" {
		t.Fatal("复用帧也应存档（链路的图证）")
	}
	archived, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(row.Image)))
	if err != nil || !strings.Contains(string(archived), "EXISTING-FRAME-BYTES") {
		t.Fatalf("存档应是既有帧本身: %v %q", err, string(archived))
	}
}

// 复用帧补查不通过 → 从第 1 轮开始换种重出（复用补查占第 0 轮，重出预算不因此打折）。
func TestEnsureRefFramesRecheckThenRegenerate(t *testing.T) {
	verdicts := []string{
		`{"pass":false,"score":6.1,"missing":["圆框眼镜"],"drift":[],"issues":[],"fix_hint":"add round-frame glasses"}`,
		`{"pass":true,"score":8,"missing":[],"drift":[],"issues":[],"fix_hint":""}`,
	}
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, verdicts[visionCall-1]
	})
	defer srv.Close()

	runDir := t.TempDir()
	writeExistingRef(t, runDir, "yangjian", "EXISTING-FRAME-BYTES")
	cfg, imgc, vision := chainClients(srv.URL, 2)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: "yangjian"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err != nil {
		t.Fatalf("补查不通过后重出应能通过: %v", err)
	}
	if *genCount != 1 || *visionCount != 2 {
		t.Fatalf("应为 补查 1 + 重出 1（出图 1 / 核对 2），得到 出图 %d / 核对 %d", *genCount, *visionCount)
	}
	r := res[0]
	if r.QCStatus != "corrected" || len(r.QCReview) != 2 {
		t.Fatalf("应为重出后通过（链路 2 轮）: %+v", r)
	}
	if !r.QCReview[0].Reused || r.QCReview[0].Final || r.QCReview[0].FixHintEN == "" {
		t.Fatalf("第 0 轮应为被否的复用帧（含修正指令）: %+v", r.QCReview[0])
	}
	if !r.QCReview[1].Final || r.QCReview[1].Verdict == nil || !r.QCReview[1].Verdict.Pass {
		t.Fatalf("第 1 轮应为最终采用: %+v", r.QCReview[1])
	}
}

// 复用帧补查时，既有链路必须接续（历史轮次保留、新轮次从其后编号），
// 不能因为补查把历史审核记录清掉。
func TestEnsureRefFramesRecheckContinuesChain(t *testing.T) {
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, `{"pass":true,"score":8.2,"missing":[],"drift":[],"issues":[],"fix_hint":""}`
	})
	defer srv.Close()

	runDir := t.TempDir()
	writeExistingRef(t, runDir, "yangjian", "EXISTING-FRAME-BYTES")
	// 预置既有链路（模拟回溯恢复/上次中断前的记录）：第 0、1 轮
	ix := &AssetIndex{Characters: []RefResult{{
		Name: "杨戬", Token: "yangjian", Path: "output/refs/yangjian.png",
		QCReview: []QCRound{
			{Round: 0, Image: "output/refs/qc/yangjian_round0.png", Recovered: true,
				Verdict: &RefQCVerdict{Pass: false, Score: 6}},
			{Round: 1, Image: "output/refs/qc/yangjian_round1.png", Recovered: true,
				QCError: "/chat/completions 失败 (HTTP 400, bad_request)"},
		},
	}}}
	if err := ix.Save(runDir); err != nil {
		t.Fatal(err)
	}

	cfg, imgc, vision := chainClients(srv.URL, 2)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: "yangjian"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err != nil {
		t.Fatalf("补查通过后应继续: %v", err)
	}
	if *genCount != 0 || *visionCount != 1 {
		t.Fatalf("应为 补查 1 次、不出图（出图 %d / 核对 %d）", *genCount, *visionCount)
	}
	rows := res[0].QCReview
	if len(rows) != 3 {
		t.Fatalf("链路应接续为 3 轮（历史 2 + 补查 1），得到 %d", len(rows))
	}
	if rows[0].Round != 0 || rows[1].Round != 1 || !rows[0].Recovered {
		t.Fatalf("历史轮次应原样保留: %+v", rows[:2])
	}
	if rows[2].Round != 2 || !rows[2].Reused || !rows[2].Final {
		t.Fatalf("补查轮应从第 2 轮编号、标记复用补查且为最终采用: %+v", rows[2])
	}
	if !strings.Contains(rows[2].Image, "round2") {
		t.Fatalf("补查轮归档名应继续编号（不得覆盖历史轮次的图证）: %s", rows[2].Image)
	}
}

// 长 token 的文件名保护：NTFS 单文件名分量上限 255 字符，244 字符的 token
// 加变体后缀就写不进去（真实事故）。命名键必须截断、确定、不撞名。
func TestFrameFileKeyCapsLongTokens(t *testing.T) {
	long := realLongToken()
	k1 := frameFileKey(long)
	if len(k1) > 60 {
		t.Fatalf("文件名键必须截断到 ≤60 字符，得到 %d: %s", len(k1), k1)
	}
	if k2 := frameFileKey(long); k2 != k1 {
		t.Fatalf("同 token 必须得到同名字（确定性）: %s != %s", k1, k2)
	}
	other := frameFileKey(long + "x")
	if other == k1 {
		t.Fatal("不同 token 不应撞名")
	}
	if short := frameFileKey("yangjian"); short != "yangjian" {
		t.Fatalf("短 token 保持原样: %s", short)
	}
}

// 长 token 的完整生成链路：文件名必须短到能落盘（含 .part 临时名与 qc 存档）。
func TestEnsureRefFramesLongTokenWritesShortNames(t *testing.T) {
	srv, _, _ := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, `{"pass":true,"score":8,"missing":[],"drift":[],"issues":[],"fix_hint":""}`
	})
	defer srv.Close()

	long := realLongToken()
	runDir := t.TempDir()
	cfg, imgc, vision := chainClients(srv.URL, 2)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: long}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err != nil {
		t.Fatalf("长 token 应能正常生成: %v", err)
	}
	r := res[0]
	for _, p := range []string{r.Path, r.QCReview[0].Image} {
		if p == "" {
			t.Fatal("路径不能为空")
		}
		base := filepath.Base(filepath.FromSlash(p))
		if len(base) > 200 {
			t.Fatalf("文件名分量过长（NTFS 上限 255，含 .part 无余量）: %d 字符", len(base))
		}
		if _, err := os.Stat(filepath.Join(runDir, filepath.FromSlash(p))); err != nil {
			t.Fatalf("文件应存在: %v", err)
		}
	}
	if filepath.Base(filepath.FromSlash(r.Path)) == safeName(long)+".png" {
		t.Fatal("长 token 不应直接用作文件名")
	}
}

// 老命名（完整 token 当文件名）的既有帧必须被兼容复用：改名绝不重出（那就是换脸）。
func TestEnsureRefFramesKeepsLegacyLongName(t *testing.T) {
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, `{"pass":true,"score":8,"missing":[],"drift":[],"issues":[],"fix_hint":""}`
	})
	defer srv.Close()

	long := realLongToken()
	runDir := t.TempDir()
	// 预置老命名的既有帧 + 已通过核对的索引条目
	legacyRel := filepath.ToSlash(filepath.Join("output", "refs", safeName(long)+".png"))
	if err := os.MkdirAll(filepath.Join(runDir, "output", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, filepath.FromSlash(legacyRel)), []byte("LEGACY-FRAME"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := &AssetIndex{Characters: []RefResult{{Name: "杨戬", Token: long, Path: legacyRel, QCStatus: "pass"}}}
	if err := ix.Save(runDir); err != nil {
		t.Fatal(err)
	}

	cfg, imgc, vision := chainClients(srv.URL, 2)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: long}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err != nil {
		t.Fatalf("复用既有帧不应失败: %v", err)
	}
	if *genCount != 0 || *visionCount != 0 {
		t.Fatalf("已通过核对的既有帧不应重出/重查（出图 %d / 核对 %d）", *genCount, *visionCount)
	}
	if res[0].Path != legacyRel {
		t.Fatalf("应继续沿用老命名文件: %s != %s", res[0].Path, legacyRel)
	}
}

// 通过核对的帧必须有审核存档；主文件被误删/覆盖后从存档恢复，
// 而不是重新生成——重出就是换脸（真实事故：8.0 分通过的帧消失）。
func TestEnsureRefFramesApprovedSnapshotAndRestore(t *testing.T) {
	srv, genCount, visionCount := newQCChainServer(t, func(visionCall int) (int, string) {
		return 200, `{"pass":true,"score":8,"missing":[],"drift":[],"issues":[],"fix_hint":""}`
	})
	defer srv.Close()

	runDir := t.TempDir()
	res := runRefChain(t, srv.URL, runDir, 2)
	if res[0].QCStatus != "pass" {
		t.Fatalf("应一次通过: %+v", res[0])
	}
	snap := res[0].QCReview[0].ApprovedCopy
	if snap == "" {
		t.Fatal("通过核对的帧应留下审核存档路径")
	}
	final, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(res[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	snapData, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(snap)))
	if err != nil || string(snapData) != string(final) {
		t.Fatalf("审核存档内容应与通过帧一致: %v", err)
	}
	// 落盘索引后模拟"主文件消失"（真实事故场景）
	ix := LoadAssetIndex(runDir)
	ix.MergeCharacters(res)
	if err := ix.Save(runDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runDir, filepath.FromSlash(res[0].Path))); err != nil {
		t.Fatal(err)
	}

	before := *genCount
	res2 := runRefChain(t, srv.URL, runDir, 2)
	if *genCount != before {
		t.Fatalf("恢复不应触发重新出图（出图次数 %d → %d）", before, *genCount)
	}
	if *visionCount != 1 {
		t.Fatalf("已通过的帧不应重复核对（核对次数 %d）", *visionCount)
	}
	restored, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(res[0].Path)))
	if err != nil || string(restored) != string(final) {
		t.Fatalf("主文件应已从审核存档恢复: %v", err)
	}
	if res2[0].QCStatus != "pass" || len(res2[0].QCReview) != 1 {
		t.Fatalf("恢复后应保持原结论: %+v", res2[0])
	}
}

// 严格模式缺口回归：候选未通过核对后重出又失败（如内容审核拦截），
// 绝不允许"保留候选、阶段照常成功"——那会把一张被否的帧带进渲染。
// 必须显式失败，同时把候选留在磁盘供下次重新核对。
func TestEnsureRefFramesRegenFailureIsHard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/generations"):
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"The request failed because the input text may contain sensitive information."}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				`{"pass":false,"score":7,"missing":["武器形制不符"],"drift":[],"issues":[],"fix_hint":"fix the blade"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	runDir := t.TempDir()
	writeExistingRef(t, runDir, "yangjian", strings.Repeat("FRAME", 300))
	cfg, imgc, vision := chainClients(srv.URL, 2)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: "yangjian"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err == nil {
		t.Fatal("重出失败 + 候选未通过 → 必须让阶段失败（严格模式）")
	}
	r := res[0]
	if r.QCStatus != "failed" || !strings.Contains(r.Error, "重出失败") {
		t.Fatalf("应记录重出失败与未通过结论: %+v", r)
	}
	if _, serr := os.Stat(filepath.Join(runDir, filepath.FromSlash(r.Path))); serr != nil {
		t.Fatalf("候选必须保留在磁盘（供下次重新核对，而非重新抽卡）: %v", serr)
	}
	if len(r.QCReview) != 2 || r.QCReview[1].GenError == "" {
		t.Fatalf("链路应留下重出失败那一轮: %+v", r.QCReview)
	}
	if r.QCReview[0].Verdict == nil || r.QCReview[0].Verdict.Pass {
		t.Fatalf("第 0 轮应为未通过的复用补查: %+v", r.QCReview[0])
	}
	// 失败也必须落盘链路：否则 UI 上"哪张被否、几轮中断"会消失
	saved := LoadAssetIndex(runDir)
	if len(saved.Characters) != 1 || len(saved.Characters[0].QCReview) != 2 {
		t.Fatalf("阶段失败后审核链路应已落盘: %+v", saved.Characters)
	}
	if saved.Characters[0].QCStatus != "failed" {
		t.Fatalf("落盘的状态应为 failed: %+v", saved.Characters[0])
	}
}

// ---- 测试脚手架 ----

// realLongToken 真实事故里的角色一致性 token（244 字符，NTFS 上限 255 的危险区）。
func realLongToken() string {
	return "young-male-celestial-general-handsome-sharp-cold-eyes-pale-gold-iris-silver-long-hair-high-ponytail-bangs-vertical-third-eye-gold-seam-on-forehead-silver-gold-layered-armor-cloud-thunder-engraving-black-gold-cape-three-pointed-double-edge-blade"
}

func writeExistingRef(t *testing.T, runDir, token, content string) {
	t.Helper()
	dir := filepath.Join(runDir, "output", "refs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, token+".png"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newQCChainServer 起一个假端点：出图返回递增编号的图片，视觉核对按脚本应答。
// 返回出图/视觉调用计数（指针，供断言实际发生的轮次）。
func newQCChainServer(t *testing.T, visionScript func(visionCall int) (int, string)) (*httptest.Server, *int, *int) {
	t.Helper()
	var mu sync.Mutex
	gen, vision := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("/images/generations", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gen++
		n := gen
		mu.Unlock()
		fmt.Fprintf(w, `{"data":[{"url":"http://%s/img/img-%d"}]}`, r.Host, n)
	})
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		// 下载层拒绝 <1024 字节的响应（疑似错误页），这里补齐并埋入可断言的标记
		marker := "PNG-BYTES-" + strings.TrimPrefix(r.URL.Path, "/img/")
		fmt.Fprint(w, marker, strings.Repeat(".", 2048-len(marker)))
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		vision++
		n := vision
		mu.Unlock()
		status, verdict := visionScript(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != 200 {
			fmt.Fprint(w, verdict)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, verdict)
	})
	return httptest.NewServer(mux), &gen, &vision
}

// chainClients 构造出图客户端 + 视觉客户端（指向同一个假端点的不同路径）。
func chainClients(baseURL string, maxRounds int) (*config.Config, *ai.Client, *ai.Client) {
	cfg := config.Default()
	cfg.Mock = false
	cfg.RefQC = config.RefQCConfig{Mode: "strict", MaxRounds: maxRounds}
	cfg.Image.Model = "img-model"
	cfg.Image.RefSize = "1024x1024"
	imgc := ai.New(ai.Config{BaseURL: baseURL, APIKey: "test-key", Model: "img-model"})
	vision := ai.New(ai.Config{BaseURL: baseURL, APIKey: "test-key", Model: "vision-model"})
	return cfg, imgc, vision
}

func runRefChain(t *testing.T, baseURL, runDir string, maxRounds int) []RefResult {
	t.Helper()
	cfg, imgc, vision := chainClients(baseURL, maxRounds)
	kit := &promspec.Kit{Characters: []promspec.CharacterSpec{{Name: "杨戬", Token: "yangjian"}}}
	res, err := EnsureRefFrames(imgc, cfg, runDir, kit, nil, false, vision)
	if err != nil {
		t.Fatalf("EnsureRefFrames 失败: %v", err)
	}
	return res
}
