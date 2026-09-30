// takes —— 关键镜头多 take 对比。
//
// 电影工业里"一条过"是例外，"多条选一"是常态。AI 生成同理：与其让重渲
// 直接顶掉旧 take（旧 take 退进 invalid/ 只留证据），不如把两个 take 并排
// 放在一起比——并排预览给人看，视觉评分给机器意见，选择权留给导演。
//
// 流程：
//  1. 现有 take 备份为 takeA（绝不丢已付费资产）
//  2. 定点重渲产出 takeB（换种子 + 提示词变体，与重渲同一机制）
//  3. ffmpeg 并排拼接对比片 + 视觉模型对两个 take 分别打分并给出推荐
//  4. 默认保留 takeA（不改变现状）；--pick B 才切换（切完需重跑 deliver 重排时间轴）
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aistudio/internal/ai"
	"aistudio/internal/config"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/render"
	"aistudio/internal/vision"
)

type takeVerdict struct {
	Winner string  `json:"winner"` // "A" | "B" | "tie"
	ScoreA float64 `json:"score_a"`
	ScoreB float64 `json:"score_b"`
	Reason string  `json:"reason"`
}

func cmdTakes(args []string) error {
	fs := flag.NewFlagSet("takes", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	shots := fs.String("shots", "", "参与对比的镜头 ID（逗号分隔，必填）")
	pick := fs.String("pick", "", "选定 take（A=恢复旧 take；B=采用新 take；空=只对比不切换）")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)
	if *runDir == "" || strings.TrimSpace(*shots) == "" {
		return fmt.Errorf("缺少 --run 或 --shots 参数")
	}
	release, err := fsx.LockRun(*runDir)
	if err != nil {
		return err
	}
	defer release()
	cfg := mustConfig(*configPath)
	if err := ffx.Preflight(); err != nil {
		return err
	}
	client := newClient(cfg)

	ids := splitIDs(*shots)
	takeDir := filepath.Join(*runDir, "output", "takes")
	if err := os.MkdirAll(takeDir, 0o755); err != nil {
		return err
	}

	// --pick：只做切换，不重新生成
	if *pick != "" {
		return applyPick(*runDir, takeDir, ids, strings.ToUpper(*pick))
	}

	for _, id := range ids {
		if err := makeTake(client, cfg, *runDir, takeDir, id); err != nil {
			return err
		}
	}
	return nil
}

// makeTake 为单个镜头生成备选 take 并对比。
func makeTake(client *ai.Client, cfg *config.Config, runDir, takeDir, id string) error {
	cur := filepath.Join(runDir, "output", "shots", id+".mp4")
	if !fsx.FileExists(cur) {
		return fmt.Errorf("镜头 %s 尚无成片（先跑 studio render）", id)
	}
	takeA := filepath.Join(takeDir, id+".takeA.mp4")
	takeB := filepath.Join(takeDir, id+".takeB.mp4")
	fmt.Printf("🎬 [%s] 备份现有 take → %s\n", id, filepath.Base(takeA))
	if err := copyFile(cur, takeA); err != nil {
		return fmt.Errorf("备份 takeA 失败: %w", err)
	}

	fmt.Printf("🎬 [%s] 换种重渲生成 takeB…\n", id)
	if _, err := render.Run(client, cfg, render.Options{RunDir: runDir, ShotIDs: []string{id}, Workers: cfg.Video.Workers}); err != nil {
		return fmt.Errorf("takeB 生成失败（takeA 完好保留）: %w", err)
	}
	if !fsx.FileExists(cur) {
		return fmt.Errorf("takeB 渲染未产出 %s", cur)
	}
	if err := copyFile(cur, takeB); err != nil {
		return fmt.Errorf("保存 takeB 失败: %w", err)
	}

	// 并排对比片：等高缩放后 hstack，同起同播
	compare := filepath.Join(takeDir, id+".compare.mp4")
	fmt.Printf("🎬 [%s] 拼接对比片 → %s\n", id, filepath.Base(compare))
	if err := hstack(takeA, takeB, compare); err != nil {
		fmt.Printf("  ⚠ 对比片拼接失败（不影响 take 产物）: %v\n", err)
	}

	// 视觉模型给推荐意见（失败不拦：选择权本来就在人）
	verdict := judgeTakes(cfg, runDir, takeA, takeB, id)
	vd := struct {
		Shot    string       `json:"shot"`
		Verdict *takeVerdict `json:"verdict"`
		TakeA   string       `json:"take_a"`
		TakeB   string       `json:"take_b"`
		Compare string       `json:"compare"`
	}{Shot: id, Verdict: verdict, TakeA: rel(runDir, takeA), TakeB: rel(runDir, takeB), Compare: rel(runDir, compare)}
	if err := fsx.AtomicWriteJSON(filepath.Join(takeDir, id+".takes.json"), vd); err != nil {
		fmt.Printf("  ⚠ 评分结果写盘失败: %v\n", err)
	}

	// 默认恢复 takeA：对比流程不得悄悄改变成片现状，切换必须显式 --pick B
	fmt.Printf("🎬 [%s] 恢复 takeA 为当前成片（切换请执行 --pick B）\n", id)
	if err := copyFile(takeA, cur); err != nil {
		return fmt.Errorf("恢复 takeA 失败: %w", err)
	}
	return nil
}

// judgeTakes 让视觉模型对比两个 take，返回推荐（不可用时返回 nil，只留并排片）。
func judgeTakes(cfg *config.Config, runDir, takeA, takeB, shotID string) *takeVerdict {
	vc := newVisionClient(cfg)
	if vc.Mock() {
		fmt.Println("  ⚠ 未配置视觉端点，跳过机器评分（人工看对比片决定）")
		return nil
	}
	aFrames, err := takeFrames(takeA, shotID)
	if err != nil {
		fmt.Printf("  ⚠ takeA 抽帧失败，跳过机器评分: %v\n", err)
		return nil
	}
	bFrames, err := takeFrames(takeB, shotID)
	if err != nil {
		fmt.Printf("  ⚠ takeB 抽帧失败，跳过机器评分: %v\n", err)
		return nil
	}
	images := append(append([]ai.ImageRef{}, aFrames...), bFrames...)
	prompt := fmt.Sprintf(`同一个镜头的两个备选 take。图片顺序：第 1-%d 张是 take A，第 %d-%d 张是 take B。
请分别打分（0-10，标准：角色一致性、解剖正确性、真实感/电影感、与分镜意图的贴合），并指出你推荐哪个 take、为什么。
只输出 JSON：{"winner":"A或B或tie","score_a":8.2,"score_b":7.6,"reason":"一句话理由"}`, len(aFrames), len(aFrames)+1, len(aFrames)+len(bFrames))
	var v takeVerdict
	if err := vc.VisionJSON("你是导演，为同一个镜头挑选更好的 take。", prompt, pathsOf(images), &v); err != nil {
		fmt.Printf("  ⚠ 机器评分失败（人工看对比片决定）: %v\n", err)
		return nil
	}
	fmt.Printf("  🤖 机器推荐 take %s（A %.1f vs B %.1f）：%s\n", v.Winner, v.ScoreA, v.ScoreB, v.Reason)
	return &v
}

func applyPick(runDir, takeDir string, ids []string, pick string) error {
	if pick != "A" && pick != "B" {
		return fmt.Errorf("--pick 只接受 A 或 B")
	}
	for _, id := range ids {
		src := filepath.Join(takeDir, id+".take"+pick+".mp4")
		if !fsx.FileExists(src) {
			return fmt.Errorf("镜头 %s 没有 take%s 的存档（先跑不带 --pick 的 takes 对比）", id, pick)
		}
		dst := filepath.Join(runDir, "output", "shots", id+".mp4")
		if err := copyFile(src, dst); err != nil {
			return err
		}
		fmt.Printf("✓ [%s] 已切换为 take%s。镜头时长可能变化，请重跑 studio deliver 重排字幕与声音\n", id, pick)
	}
	return nil
}

// hstack 两条视频等高并排。
func hstack(a, b, out string) error {
	args := []string{"-y", "-i", a, "-i", b, "-filter_complex",
		"[0:v]scale=-2:540,setsar=1[a];[1:v]scale=-2:540,setsar=1[b];[a][b]hstack=inputs=2,format=yuv420p[v]",
		"-map", "[v]", "-an", "-c:v", "libx264", "-preset", "fast", "-crf", "23", out}
	_, err := ffx.Run(10*60, "ffmpeg", args...)
	return err
}

// takeFrames 为 take 抽 3 帧用于机器评分（复用 vision 的抽帧，避开首尾伪影）。
func takeFrames(video, shotID string) ([]ai.ImageRef, error) {
	dir, err := os.MkdirTemp("", "takes-"+shotID)
	if err != nil {
		return nil, err
	}
	files, err := vision.ExtractFrames(video, dir, shotID, 0, 3, 640)
	if err != nil {
		return nil, err
	}
	refs := make([]ai.ImageRef, 0, len(files))
	for _, f := range files {
		refs = append(refs, ai.ImageRef{Path: f})
	}
	return refs, nil
}

func pathsOf(refs []ai.ImageRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Path)
	}
	return out
}

func splitIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return fsx.AtomicWrite(dst, data, 0o644)
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}
