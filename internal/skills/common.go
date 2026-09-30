// Package skills 实现专业级 AI 电影制作流水线的全部技能阶段。
package skills

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"aistudio/internal/ai"
	"aistudio/internal/skill"
)

// filmBase 所有技能共享的专业制作准则
const filmBase = `你是专业级 AI 电影制作流水线中的资深成员。
作品标准：院线级叙事结构、明确的镜头语言（景别/运镜）、角色形象跨镜头严格一致、视听节奏可控。
用户 brief（创作意图）如下，所有产出必须紧扣 brief，不得偏题。`

// callTune 按阶段调用的采样参数（callJSON 按 callName 查表）：
// 结构化产出低温——输出抖动导致 JSON 解析失败的重试是双倍 token；创作类保留高温。
// storyboard/prompts/music 的 JSON 随镜头数线性膨胀（cue 与音效清单逐镜展开），
// MaxTokens 放宽防截断（截断=必然重试）。
var callTune = map[string]ai.CallOpts{
	"script":     {Temperature: 0.8, MaxTokens: 4096},
	"characters": {Temperature: 0.4, MaxTokens: 4096},
	"storyboard": {Temperature: 0.4, MaxTokens: 8192},
	"prompts":    {Temperature: 0.3, MaxTokens: 16384},
	"voiceover":  {Temperature: 0.4, MaxTokens: 4096},
	"music":      {Temperature: 0.5, MaxTokens: 16384},
	"shotspec":   {Temperature: 0.35, MaxTokens: 16384},
	"review":     {Temperature: 0.2, MaxTokens: 4096},
}

// callJSON 调用 AI 并解析 JSON 输出到 out。
// 自动注入：经验教训（自我成长）、修复指令（自我修复）、返修意见（审片闭环）。
func callJSON(ctx *skill.Context, callName, system, user string, out any) error {
	sys := system
	if len(ctx.Lessons) > 0 {
		sys += "\n\n历史经验教训（必须遵守）：\n- " + strings.Join(ctx.Lessons, "\n- ")
	}
	if hint, _ := ctx.Params["repair_hint"].(string); hint != "" {
		sys += "\n\n上次失败的修复指令（必须严格遵守）：\n" + hint
	}
	if fb, _ := ctx.Params["revision_feedback"].(string); fb != "" {
		sys += "\n\n制片人审片返修意见（本轮必须针对性解决）：\n" + fb
	}
	text, err := ctx.AI.ChatOpts(callName, sys, user, callTune[callName])
	if err != nil {
		return err
	}
	raw, err := ai.ExtractJSON(text)
	if err != nil {
		return fmt.Errorf("输出提取 JSON 失败: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("输出 JSON 解析失败: %w", err)
	}
	return nil
}

func marshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// validate 检查必填字段，缺失时返回可自我修复的错误。
// 错误信息即下一轮的修复指令，必须写清"违反了什么、要怎么改"。
func validate(cond bool, msg string) error {
	if !cond {
		return fmt.Errorf("产出校验失败: %s", msg)
	}
	return nil
}

var cjkRe = regexp.MustCompile(`[\p{Han}\p{Hiragana}\p{Katakana}]`)

// hasCJK 是否含中日韩字符（用于拦截"image_prompt 必须为英文"类违规）
func hasCJK(s string) bool { return cjkRe.MatchString(s) }

// nameMatches 宽松人名匹配：台词里的角色名与角色圣经条目互为包含即视为同一人
// （模型常出现"信使·林"/"林"的写法差异，精确匹配会误伤）
func nameMatches(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.Contains(a, b) || strings.Contains(b, a)
}

// tokenRe 一致性锚点格式：小写字母/数字 + 连字符（图像/视频模型锚定的标准形态）
var tokenRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// containsCJKCount 统计 CJK 字符数（台词时长估算）
func cjkCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			n++
		}
	}
	return n
}
