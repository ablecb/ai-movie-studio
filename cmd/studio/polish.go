// polish —— 视觉复核闭环：复核 → 定点重渲 → 重新交付 → 再复核，直到达标或轮数用尽。
//
// 此前视觉复核只给"建议重渲清单"，闭环的最后一步（真正去重渲）靠人手动执行。
// polish 把这最后一步接上：它复用 render 的定点重渲（--shots 即换种重渲）与
// deliver 的实测重排，把"看片 → 返修 → 再看片"变成一个命令。
//
// 每一轮的账目是透明的：复核花视觉调用的钱，重渲只花不及格镜头的视频钱，
// 重新交付是本地 ffmpeg。预算熔断（budget.max_cost_cny）全程生效。
package main

import (
	"flag"
	"fmt"
	"strings"

	"aistudio/internal/deliver"
	"aistudio/internal/ffx"
	"aistudio/internal/fsx"
	"aistudio/internal/render"
	"aistudio/internal/vision"
)

func cmdPolish(args []string) error {
	fs := flag.NewFlagSet("polish", flag.ExitOnError)
	runDir := fs.String("run", "", "运行目录 runs/run-xxx（必填）")
	rounds := fs.Int("rounds", 2, "最多打磨轮数（每轮：视觉复核→定点重渲→重新交付）")
	passScore := fs.Float64("pass-score", 0, "单镜头达标分（0=沿用 config.vision_review.pass_score）")
	noBurn := fs.Bool("no-burn", false, "不烧录字幕")
	workers := fs.Int("workers", 0, "配音并发数")
	configPath := fs.String("config", "config.json", "AI 与生产参数配置")
	fs.Parse(args)
	if *runDir == "" {
		return fmt.Errorf("缺少 --run 参数")
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
	visionClient, err := newVisionClient(cfg)
	if err != nil {
		return err
	}
	if visionClient.Mock() {
		return fmt.Errorf("polish 需要视觉端点与密钥（providers.vision，环境变量 %s）", cfg.Providers.Vision.APIKeyEnv)
	}
	if err := visionClient.Ping(); err != nil {
		return fmt.Errorf("视觉能力探测失败: %w", err)
	}
	if *passScore > 0 {
		cfg.VisionReview.PassScore = *passScore
	}
	// polish 的存在意义就是视觉复核，无视全局 enabled 开关
	cfg.VisionReview.Enabled = true

	burn := !*noBurn
	for round := 1; round <= *rounds; round++ {
		fmt.Printf("\n🔍 打磨第 %d/%d 轮：视觉复核…\n", round, *rounds)
		rep, err := vision.Run(cfg, vision.Options{RunDir: *runDir, Client: visionClient})
		if err != nil {
			return fmt.Errorf("视觉复核失败: %w", err)
		}
		fmt.Printf("  👁 均分 %.1f（身份 %s / 解剖 %s / 电影感 %s / 环境 %s）\n", rep.Score,
			avgStr(rep.IdentityAvg), avgStr(rep.AnatomyAvg), avgStr(rep.CinematicAvg), avgStr(rep.SceneAvg))
		for _, n := range rep.Notes {
			fmt.Printf("  ℹ %s\n", n)
		}
		if len(rep.Rerender) == 0 {
			fmt.Println("✓ 所有镜头均达标，打磨完成")
			break
		}
		if client.OverBudget() {
			fmt.Println("⛔ 已达预算上限，停止打磨（未达标镜头见上；处理问题后可重新 polish）")
			break
		}
		fmt.Printf("↻ 定点重渲 %d 个镜头: %s\n", len(rep.Rerender), strings.Join(rep.Rerender, ","))
		if _, err := render.Run(client, cfg, render.Options{
			RunDir: *runDir, ShotIDs: rep.Rerender, Workers: cfg.Video.Workers,
		}); err != nil {
			return fmt.Errorf("重渲失败（已重渲的镜头保留断点，可重新执行 polish）: %w", err)
		}
		// 重新交付：重渲后的镜头实测时长必然漂移，字幕/配音/混音必须按实测重排
		fmt.Println("📦 重新交付（按新镜头实测时长重排字幕与声音）…")
		if _, err := deliver.Run(cfg, deliver.Options{
			RunDir: *runDir, BurnSubtitle: &burn, KeepMaster: true,
			Workers: *workers, Client: client, VisionClient: visionClient, VisualCheck: false,
		}); err != nil {
			return fmt.Errorf("重新交付失败: %w", err)
		}
	}

	// 收尾：最后一次完整交付带视觉复核，保证 delivery_report.json 的 visual_qa
	// 描述的就是当前成片——报告与成片不同步比不打磨更糟
	fmt.Println("\n📦 最终交付（含视觉复核，写入交付报告）…")
	_, err = deliver.Run(cfg, deliver.Options{
		RunDir: *runDir, BurnSubtitle: &burn, KeepMaster: true,
		Workers: *workers, Client: client, VisionClient: visionClient, VisualCheck: true,
	})
	return err
}

func avgStr(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *p)
}
