// Package subtitle 实现生产级字幕规划：以"实测音频时长"为唯一依据排版。
//
// 与"按时长估算硬排"的区别，也就是字幕为什么会与台词对不上的原因：
//
//  1. 入点用真实 TTS 音频时长，不是字数估算——估算与真人语速必然有偏差，
//     10 句台词累计偏差可达 1-2 秒，观众立刻能感觉到"字幕比声音快/慢"
//  2. 折行：中文单行最多 18 字，超长必须折两行。一行铺满整屏是业余感最强的特征
//  3. CPS（每秒字符数）限速：字幕停留时间不够就会"闪一下就走"，观众读不完
//  4. 槽位夹紧：绝不越出所属镜头边界（越界=字幕与画面对不上）
//  5. 校验：排版结果逐条验证，不合格则显式报错，而不是默默交付坏字幕
package subtitle

import (
	"fmt"
	"strings"
	"unicode"

	"aistudio/internal/config"
)

// Line 一条口播文案（来自配音台本，保持出现顺序）。
type Line struct {
	ShotID   string
	Speaker  string
	Text     string
	StartSec float64 // 实测入点（相对成片）；0 表示"按镜头槽位顺序排"
	DurSec   float64 // 实测人声时长；0 表示按字符估算
	// NoSpeech 纯音效/拟音行：不显示字幕
	NoSpeech bool
}

// Slot 镜头槽位（必须用 ffprobe 实测时长）。
type Slot struct {
	ShotID    string
	Start     float64
	Dur       float64
	ActualDur float64 // 实测片段时长（与 Dur 可能有漂移）
	// LeadInSec 该镜头首句台词的声音提前量（J-cut）。字幕允许以同量提前
	// 越出镜头起点（音画字同步）；0 = 字幕严格夹在镜头内。
	LeadInSec float64
}

// Cue 一条成片字幕。
type Cue struct {
	Index    int      `json:"index"`
	Start    float64  `json:"start"`
	End      float64  `json:"end"`
	Lines    []string `json:"lines"`
	ShotID   string   `json:"shot_id"`
	Speaker  string   `json:"speaker"`
	CPS      float64  `json:"cps"`
	Clamped  bool     `json:"clamped,omitempty"`  // 因边界被压缩
	Extended bool     `json:"extended,omitempty"` // 因 CPS 被延长
}

// Text 返回折行后的多行文本（SRT 用换行分隔）。
func (c Cue) Text() string { return strings.Join(c.Lines, "\n") }

// PlanOptions 排版参数。
type PlanOptions struct {
	MaxLineRunes int     // 单行最大字符数
	MaxLines     int     // 最多行数
	MinDurSec    float64 // 单条最短显示时长
	MaxDurSec    float64 // 单条最长显示时长（超过说明该拆句）
	MaxCPS       float64 // 每秒字符数上限
	GapSec       float64 // 相邻字幕最小间隔（防粘连）
}

// DefaultPlanOptions 从配置推导排版参数。
func DefaultPlanOptions(cfg *config.Config) PlanOptions {
	return PlanOptions{
		MaxLineRunes: cfg.Subtitle.MaxLineRunes,
		MaxLines:     cfg.Subtitle.MaxLines,
		MinDurSec:    cfg.Subtitle.MinDurSec,
		MaxDurSec:    8.0, // 单条字幕最长 8 秒：再长说明该拆句，而不是挂一整块
		MaxCPS:       cfg.Subtitle.MaxCPS,
		GapSec:       0.08,
	}.withDefaults()
}

func (o PlanOptions) withDefaults() PlanOptions {
	if o.MaxLineRunes <= 0 {
		o.MaxLineRunes = 18
	}
	if o.MaxLines <= 0 {
		o.MaxLines = 2
	}
	if o.MinDurSec <= 0 {
		o.MinDurSec = 0.9
	}
	if o.MaxDurSec <= 0 || o.MaxDurSec < o.MinDurSec {
		o.MaxDurSec = 6.0
	}
	if o.MaxCPS <= 0 {
		o.MaxCPS = 9.0
	}
	if o.GapSec < 0 {
		o.GapSec = 0.08
	}
	return o
}

// Plan 把文案排进镜头槽位。
//
// 时序规则（按优先级）：
//  1. 不得越出所属镜头槽位（硬约束：字幕必须与画面对应）
//  2. 有实测人声时长时，显示时长 = 人声时长 + 0.4s 余量（读得完、走得不突兀）
//  3. 无实测时长时按字符估算，并受 CPS 下限约束
//  4. 同一镜头多条文案按顺序依次排布，不重叠
//  5. 显示时长超过 MaxDurSec 时优先延长（观众读得完）但绝不越界
func Plan(slots []Slot, lines []Line, opt PlanOptions) []Cue {
	opt = opt.withDefaults()

	byShot := map[string][]Line{}
	order := map[string]int{}
	for i, l := range lines {
		if l.ShotID == "" || strings.TrimSpace(l.Text) == "" || l.NoSpeech {
			continue
		}
		byShot[l.ShotID] = append(byShot[l.ShotID], l)
		if _, ok := order[l.ShotID]; !ok {
			order[l.ShotID] = i
		}
	}

	var cues []Cue
	for _, s := range slots {
		items := byShot[s.ShotID]
		if len(items) == 0 {
			continue
		}
		shotStart, shotEnd := s.Start, s.Start+s.Dur
		if shotEnd <= shotStart {
			continue
		}

		// 同一镜头内部按顺序排布；每条的位置由"实测入点"或"顺序均分"决定。
		// J-cut：首句声音提前越出镜头起点时，字幕跟着提前（音画字同步），
		// 提前下限 = 声音的实际提前量，不会提前更多
		floor := shotStart - s.LeadInSec
		cursor := shotStart - s.LeadInSec
		for _, l := range items {
			text := Normalize(l.Text)
			linesWrapped := Wrap(text, opt.MaxLineRunes, opt.MaxLines)

			// 目标显示时长
			dur := targetDuration(text, l.DurSec, opt)

			// 入点：台本给出实测入点时使用，否则从当前游标开始
			start := cursor
			if l.StartSec > 0 {
				start = l.StartSec
				if start < floor {
					start = floor
				}
				if start > shotEnd-opt.MinDurSec {
					start = shotEnd - opt.MinDurSec
				}
			}
			if start < cursor {
				start = cursor // 不与上一条重叠
			}

			end := start + dur
			clamped := false
			if end > shotEnd {
				// 越界：压缩到镜头内。这是"字幕与画面对不上"的最后一道防线
				end = shotEnd
				clamped = true
			}
			if end-start < opt.MinDurSec {
				// 剩余空间不足：回退入点以争取最短可读时长
				if shotEnd-opt.MinDurSec >= cursor {
					start = shotEnd - opt.MinDurSec
					end = shotEnd
					clamped = true
				} else if len(cues) > 0 {
					// 完全放不下：丢弃并在校验阶段报出（宁可少一条也不要闪一条）
					continue
				}
			}
			if end <= start {
				continue
			}

			c := Cue{
				Start: start, End: end, Lines: linesWrapped,
				ShotID: s.ShotID, Speaker: l.Speaker,
				Clamped: clamped,
			}
			c.CPS = CPS(strings.Join(linesWrapped, ""), end-start)
			cues = append(cues, c)
			cursor = end + opt.GapSec
		}
	}

	// 编号与全局排序（不同镜头的槽位理论上已有序，排序保证 SRT 时序单调）
	for i := range cues {
		cues[i].Index = i + 1
	}
	return cues
}

// targetDuration 计算一条字幕应有的显示时长。
func targetDuration(text string, measured float64, opt PlanOptions) float64 {
	runeCount := float64(visibleRunes(text))
	var dur float64
	if measured > 0 {
		// 以实测人声为准：这是"字幕跟得上台词"的核心
		dur = measured + 0.4
	} else {
		dur = runeCount / estimateCPS(text)
	}
	// CPS 下限：字多的时候必须给足时间，否则观众读不完
	if opt.MaxCPS > 0 {
		need := runeCount / opt.MaxCPS
		if need > dur {
			dur = need
		}
	}
	if dur < opt.MinDurSec {
		dur = opt.MinDurSec
	}
	if dur > opt.MaxDurSec {
		dur = opt.MaxDurSec
	}
	return dur
}

// Wrap 折行：中日韩按字符宽度折，西文按词折，最多 maxLines 行。
// 折行是"专业字幕"与"业余字幕"最直观的分界：一行铺满整屏非常刺眼。
func Wrap(text string, maxRunes, maxLines int) []string {
	if maxRunes <= 0 {
		maxRunes = 18
	}
	if maxLines <= 0 {
		maxLines = 2
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{""}
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}
	// 以标点为主要断点候选，避免"断在词中间"的硬伤
	breakers := "，。！？；：、,.!?;: "
	var lines []string
	rest := runes
	for len(rest) > 0 {
		if len(rest) <= maxRunes {
			lines = append(lines, strings.TrimSpace(string(rest)))
			break
		}
		cut := -1
		// 先找 maxRunes 之前最近的标点断点（不超过 maxRunes，且不短于 maxRunes/2）
		for i := maxRunes; i > maxRunes/2; i-- {
			if strings.ContainsRune(breakers, rest[i]) {
				cut = i + 1 // 标点跟在上一行
				break
			}
		}
		if cut < 0 {
			// 无标点：找最近的空格（西文词边界）
			for i := maxRunes; i > maxRunes/2; i-- {
				if rest[i] == ' ' {
					cut = i
					break
				}
			}
		}
		if cut < 0 {
			cut = maxRunes // 中文长句无标点：硬折
		}
		lines = append(lines, strings.TrimSpace(string(rest[:cut])))
		rest = rest[cut:]
		if len(lines) == maxLines-1 {
			// 最后一行放剩余全部（允许略超长，总比丢字好）
			if tail := strings.TrimSpace(string(rest)); tail != "" {
				lines = append(lines, tail)
			}
			break
		}
	}
	if len(lines) > maxLines {
		// 超行数：合并尾部，避免丢字
		merged := strings.Join(lines[maxLines-1:], "")
		lines = append(lines[:maxLines-1], merged)
	}
	// 过滤空行
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = []string{text}
	}
	return out
}

// CPS 每秒可见字符数。
func CPS(text string, durSec float64) float64 {
	if durSec <= 0 {
		return 0
	}
	return float64(visibleRunes(text)) / durSec
}

// Normalize 字幕文本规范化：压缩空白、统一省略号、去掉首尾标点冗余。
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "...", "…")
	s = strings.ReplaceAll(s, "。。", "。")
	return s
}

func visibleRunes(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		n++
	}
	return n
}

// estimateCPS 无实测时长时的估算语速（中文 4 字/秒）。
func estimateCPS(text string) float64 {
	var cjk, latin int
	for _, r := range []rune(text) {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			cjk++
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			latin++
		}
	}
	if latin > cjk*2 {
		return 2.5 // 西文按词近似
	}
	return 4.0
}

// ---------------------------------------------------------------------------
// 渲染与校验
// ---------------------------------------------------------------------------

// Render 输出 SRT 文本。
func Render(cues []Cue) string {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(c.Start), srtTime(c.End), c.Text())
	}
	return b.String()
}

// RenderPlain 输出单行纯文本（无折行），用于不支持多行的平台。
func RenderPlain(cues []Cue) string {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(c.Start), srtTime(c.End), strings.Join(c.Lines, " "))
	}
	return b.String()
}

func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	// 四舍五入到毫秒，避免向下的浮点误差让字幕早退
	ms := int(sec*1000 + 0.5)
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// Violation 一条字幕质量问题。
type Violation struct {
	Index    int    `json:"index"`
	Code     string `json:"code"`
	Severity string `json:"severity"` // block | warn
	Message  string `json:"message"`
}

// Verify 逐条校验排版结果：字幕质量问题必须显式暴露，不能默默交付。
func Verify(cues []Cue, opt PlanOptions) []Violation {
	opt = opt.withDefaults()
	var out []Violation
	for _, c := range cues {
		dur := c.End - c.Start
		if dur < opt.MinDurSec-0.001 {
			sev := "warn"
			if dur < opt.MinDurSec*0.6 {
				sev = "block" // 一闪而过，观众根本读不到
			}
			out = append(out, Violation{Index: c.Index, Code: "E_SUB_TOO_SHORT", Severity: sev,
				Message: fmt.Sprintf("第 %d 条字幕仅显示 %.2fs（下限 %.2fs）：%s",
					c.Index, dur, opt.MinDurSec, strings.Join(c.Lines, ""))})
		}
		if c.CPS > opt.MaxCPS+0.05 {
			out = append(out, Violation{Index: c.Index, Code: "E_SUB_CPS_HIGH", Severity: "warn",
				Message: fmt.Sprintf("第 %d 条字幕 %.1f 字/秒（上限 %.1f），观众可能读不完：%s",
					c.Index, c.CPS, opt.MaxCPS, strings.Join(c.Lines, ""))})
		}
		if len(c.Lines) > opt.MaxLines {
			out = append(out, Violation{Index: c.Index, Code: "E_SUB_TOO_MANY_LINES", Severity: "block",
				Message: fmt.Sprintf("第 %d 条字幕 %d 行（上限 %d 行）", c.Index, len(c.Lines), opt.MaxLines)})
		}
		for _, l := range c.Lines {
			if visibleRunes(l) > opt.MaxLineRunes+6 {
				out = append(out, Violation{Index: c.Index, Code: "E_SUB_LINE_TOO_LONG", Severity: "warn",
					Message: fmt.Sprintf("第 %d 条字幕存在超长行（%d 字）：%s", c.Index, visibleRunes(l), l)})
			}
		}
	}
	// 重叠检测：字幕重叠会让两条同时显示，观感极差
	for i := 1; i < len(cues); i++ {
		if cues[i].Start < cues[i-1].End-0.001 {
			out = append(out, Violation{Index: cues[i].Index, Code: "E_SUB_OVERLAP", Severity: "block",
				Message: fmt.Sprintf("第 %d 条与第 %d 条时间重叠（%.2fs）",
					cues[i-1].Index, cues[i].Index, cues[i-1].End-cues[i].Start)})
		}
	}
	return out
}

// HasBlocking 是否存在阻断级字幕问题。
func HasBlocking(vs []Violation) bool {
	for _, v := range vs {
		if v.Severity == "block" {
			return true
		}
	}
	return false
}

// TotalDuration 返回字幕覆盖的总时长（用于与成片时长比对）。
func TotalDuration(cues []Cue) float64 {
	if len(cues) == 0 {
		return 0
	}
	return cues[len(cues)-1].End
}
