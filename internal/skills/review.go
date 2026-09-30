package skills

import (
	"fmt"

	"aistudio/internal/promspec"
	"aistudio/internal/skill"
)

// ---------------- S9 AI 制片人审片（质量闭环） ----------------

type Issue struct {
	Stage    string `json:"stage"`    // 问题归属阶段
	Severity string `json:"severity"` // blocker / major / minor
	Note     string `json:"note"`
}

type ReviewReport struct {
	Score     float64  `json:"score"` // 0-10
	Pass      bool     `json:"pass"`
	Strengths []string `json:"strengths"`
	Issues    []Issue  `json:"issues"`
	Feedback  string   `json:"feedback"` // 打回时的返修意见
}

type reviewSkill struct{}

func (reviewSkill) Name() string { return "review" }
func (reviewSkill) Description() string {
	return "AI 制片人审片：多维评分，不达标自动打回"
}

func (reviewSkill) Run(ctx *skill.Context) (any, error) {
	threshold := ctx.ParamFloat("threshold", 8.0)

	// mock 模式：首轮压分触发返修闭环，验证引擎的自我修复路径
	if ctx.AI.Mock() {
		rev := ctx.ParamInt("revision", 0)
		if rev == 0 {
			return &ReviewReport{
				Score: 7.2, Pass: false,
				Strengths: []string{"叙事结构完整", "角色一致性锚点清晰"},
				Issues: []Issue{
					{Stage: "storyboard", Severity: "major", Note: "第二幕缺少反应镜头，情绪转折生硬"},
					{Stage: "shotspec", Severity: "minor", Note: "sh003 负面提示词未覆盖天空纹理伪影"},
				},
				Feedback: "分镜第二幕补充一个女主角的反应镜头；sh003 负面提示词增加'条纹状天空伪影'。",
			}, nil
		}
		return &ReviewReport{
			Score: 8.8, Pass: true,
			Strengths: []string{"返修后情绪曲线完整", "镜头组接节奏达标", "角色跨镜头一致"},
			Issues:    []Issue{},
			Feedback:  "达到专业成片标准，可进入渲染环节。",
		}, nil
	}

	// 真实模式：汇总上游全部产物交给 AI 制片人
	var script Script
	var sb Storyboard
	var vo VoiceoverScript
	var mp MusicPlan
	var kit promspec.Kit
	_ = ctx.DepOf("script", &script)
	_ = ctx.DepOf("storyboard", &sb)
	_ = ctx.DepOf("voiceover", &vo)
	_ = ctx.DepOf("music", &mp)
	_ = ctx.DepOf("shotspec", &kit)

	system := filmBase + `
你是严格的执行制片人，负责终审。基于全部制作产物评分（0-10），schema：
{"score":8.5,"pass":true,"strengths":["…"],"issues":[{"stage":"storyboard|shotspec|voiceover|music|script","severity":"blocker|major|minor","note":"具体问题"}],"feedback":"若不达标，给出具体、可执行的返修意见；达标则给放行结论"}
评分维度：叙事完整(20%)、镜头语言专业度(25%)、角色与画面一致性(20%)、视听节奏(15%)、可生成性/提示词质量(20%)。
专业要求：
- 重点审查 shotspec 的镜头规格：照明是否有画面内动机（无动机光源是最明显的 AI 感）、
  焦段与景别是否匹配、角色锚点是否足够具体到能锁定同一张脸、构图是否给出可执行指令；
- 台词与镜头节拍是否匹配（台词时长不得超过镜头时长）；
- blocker 级问题必须打回；feedback 必须点名具体镜头 ID 并给出可执行的修改方向；
- 只输出 JSON。`
	user := fmt.Sprintf("及格线：%.1f\n剧本：%s\n分镜表：%s\n生成规格：%s\n配音台本：%s\n音乐方案：%s",
		threshold, marshal(script), marshal(sb), marshal(kit), marshal(vo), marshal(mp))
	if rev := ctx.ParamInt("revision", 0); rev > 0 {
		user += fmt.Sprintf("\n注意：这是第 %d 次返修，请重点检查上轮意见是否落实。", rev)
	}

	out := &ReviewReport{}
	if err := callJSON(ctx, "review", system, user, out); err != nil {
		return nil, err
	}
	if out.Score <= 0 && len(out.Strengths) == 0 {
		return nil, fmt.Errorf("审片报告无效：缺少评分与评价")
	}
	out.Pass = out.Pass && out.Score >= threshold
	return out, nil
}
