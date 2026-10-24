// Package selfheal 实现"自我成长 / 自我修复"机制：
//   - Diagnose: 阶段失败后，让 AI 分析错误并给出修复动作（带修复提示重试 / 终止）
//   - Memory:   经验记忆库（memory/lessons.json），记录每次踩坑与解法，
//     后续运行时自动把相关教训注入技能提示词 —— 引擎越用越聪明。
package selfheal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aistudio/internal/ai"
)

// RepairAction 修复动作
type RepairAction struct {
	Action    string `json:"action"`     // retry | abort
	Hint      string `json:"hint"`       // 重试时注入的修复提示
	RootCause string `json:"root_cause"` // 根因分析（人类可读）
}

// DiagnoseInput 诊断上下文
type DiagnoseInput struct {
	SkillName string
	Attempts  int
	Error     string
	Lessons   []string
}

const diagnoseSystem = `你是资深流水线工程师，负责诊断 AI 电影工作流中某个技能阶段的失败原因，并给出可执行的修复方案。
只输出 JSON，格式：
{"action":"retry|abort","hint":"给重试时的具体修复指令，例如：输出必须是纯 JSON，禁止 markdown 代码块；字段 X 必须为字符串数组","root_cause":"一句话根因"}
规则：
- 错误看起来是格式/解析/截断问题 → action=retry，hint 要具体到如何修正输出格式
- 错误是缺少必要输入或依赖 → action=abort
- 不确定时 → action=retry，hint 里给出保守修正建议
- hint 必须可执行、具体，不要空话
- 不存在"跳过"选项：严格模式下阶段失败只能修复重试或中止等待人工介入`

// Diagnose 调用 AI 诊断失败原因，产出修复动作。mock 模式走内置规则。
func Diagnose(client *ai.Client, in DiagnoseInput) RepairAction {
	if client.Mock() {
		return mockDiagnose(in)
	}
	user := fmt.Sprintf("技能: %s\n已尝试次数: %d\n错误信息:\n%s\n相关历史教训:\n- %s",
		in.SkillName, in.Attempts, in.Error, strings.Join(in.Lessons, "\n- "))
	text, err := client.ChatOpts("selfheal", diagnoseSystem, user, ai.CallOpts{Temperature: 0.2})
	if err != nil {
		return RepairAction{Action: "retry", Hint: "请修正输出后重试", RootCause: "诊断器自身调用失败: " + err.Error()}
	}
	raw, err := ai.ExtractJSON(text)
	if err != nil {
		return RepairAction{Action: "retry", Hint: "请修正输出后重试", RootCause: "诊断器输出无法解析"}
	}
	var act RepairAction
	if err := json.Unmarshal(raw, &act); err != nil {
		return RepairAction{Action: "retry", Hint: "请修正输出后重试", RootCause: "诊断器 JSON 字段不合法"}
	}
	switch act.Action {
	case "retry", "abort":
	default:
		act.Action = "retry"
	}
	return act
}

func mockDiagnose(in DiagnoseInput) RepairAction {
	e := strings.ToLower(in.Error)
	switch {
	case strings.Contains(e, "json") || strings.Contains(e, "解析") || strings.Contains(e, "parse") || strings.Contains(e, "unexpected"):
		return RepairAction{
			Action:    "retry",
			Hint:      "上一轮输出不是合法 JSON。请只输出一个 JSON 对象：不要 markdown 代码围栏、不要解释文字；字符串内的引号需转义；数组字段若为空请输出 []。",
			RootCause: "模型输出不符合 JSON Schema（mock 规则：格式类错误→格式化重试）",
		}
	case strings.Contains(e, "依赖") || strings.Contains(e, "missing") || strings.Contains(e, "缺少"):
		return RepairAction{Action: "abort", RootCause: "缺少上游依赖，无法本地修复（mock 规则）"}
	default:
		return RepairAction{
			Action:    "retry",
			Hint:      "收紧输出规模：减少条目数量、缩短单条文本，确保内容完整不被截断。",
			RootCause: "未知错误（mock 规则：默认保守重试）",
		}
	}
}

// ------------------------------------------------------------------------------
// 经验记忆库
// ------------------------------------------------------------------------------

// Lesson 一条经验教训
type Lesson struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Skill     string    `json:"skill"`     // 发生在哪个技能阶段
	Signature string    `json:"signature"` // 错误特征（用于检索）
	Lesson    string    `json:"lesson"`    // 提炼后的教训
	Source    string    `json:"source"`    // selfheal | review | manual
}

// Memory 经验记忆库（JSON 文件持久化，并发安全）
type Memory struct {
	mu      sync.Mutex
	Path    string
	Lessons []Lesson `json:"lessons"`
}

// LoadMemory 从磁盘加载记忆库（不存在则返回空库）
func LoadMemory(path string) (*Memory, error) {
	m := &Memory{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("记忆库文件损坏 (%s): %w", path, err)
	}
	return m, nil
}

// Save 持久化记忆库
func (m *Memory) Save() error {
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.Path, data, 0o644)
}

// Add 追加一条教训并落盘。同一（技能+特征+内容）只保留一条，重复出现则刷新时间——
// 防止同类踩坑把记忆库刷成重复流水账，稀释检索质量。
func (m *Memory) Add(skill, signature, lesson, source string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.Lessons {
		l := &m.Lessons[i]
		if l.Skill == skill && l.Signature == signature && l.Lesson == lesson {
			l.Time = time.Now()
			l.Source = source
			return m.Save()
		}
	}
	m.Lessons = append(m.Lessons, Lesson{
		ID:        fmt.Sprintf("L%03d", len(m.Lessons)+1),
		Time:      time.Now(),
		Skill:     skill,
		Signature: signature,
		Lesson:    lesson,
		Source:    source,
	})
	// 记忆库上限：只保留最近 500 条，防止无限膨胀
	if len(m.Lessons) > 500 {
		m.Lessons = m.Lessons[len(m.Lessons)-500:]
	}
	return m.Save()
}

// Recall 检索与某技能相关的教训（最近优先，最多 n 条）
func (m *Memory) Recall(skill string, n int) []string {
	return m.RecallFor(skill, "", n)
}

// RecallFor 检索教训：先返回与错误特征（去数字比对）匹配的条目，再以该技能最近
// 的教训补足。教训多起来之后，"相关"比"最近"更重要——无关教训只会稀释提示词。
func (m *Memory) RecallFor(skill, errHint string, n int) []string {
	if n <= 0 {
		n = 5
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	type scored struct {
		l     *Lesson
		score int
	}
	var all []scored
	for i := range m.Lessons {
		l := &m.Lessons[i]
		if l.Skill != skill && l.Skill != "*" {
			continue
		}
		score := 0
		if errHint != "" && l.Signature != "" {
			score = overlapScore(l.Signature, Signature(errHint))
		}
		all = append(all, scored{l, score})
	}
	// 相关性优先，同级按时间新→旧
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].score > all[i].score ||
				(all[j].score == all[i].score && all[j].l.Time.After(all[i].l.Time)) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range all {
		if len(out) >= n {
			break
		}
		if seen[s.l.Lesson] {
			continue
		}
		seen[s.l.Lesson] = true
		out = append(out, fmt.Sprintf("[%s] %s", s.l.ID, s.l.Lesson))
	}
	return out
}

// Signature 提取错误特征：去数字、压缩空白、截断——同型错误归一到可比对的特征串
func Signature(err string) string {
	s := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return -1
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, err)
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(strings.TrimSpace(s), 80)
}

// overlapScore 两个特征串的 3-gram 重合度（轻量相似度，够用于教训匹配）
func overlapScore(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	grams := func(s string) map[string]bool {
		r := []rune(s)
		m := map[string]bool{}
		for i := 0; i+3 <= len(r); i++ {
			m[string(r[i:i+3])] = true
		}
		return m
	}
	ga, gb := grams(a), grams(b)
	n := 0
	for g := range ga {
		if gb[g] {
			n++
		}
	}
	return n
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
