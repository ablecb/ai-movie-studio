package skills

import (
	"fmt"
	"strings"

	"aistudio/internal/skill"
)

// ---------------- S1 剧本 ----------------

type Scene struct {
	ID        string `json:"id"`
	Location  string `json:"location"`
	TimeOfDay string `json:"time_of_day"`
	// Act 幕号（1/2/3）。声明幕号后引擎可做节拍表校验：三幕占比、转折落位——
	// "三幕结构"不能只靠提示词里的一句话，要有机器下限。
	Act int `json:"act,omitempty"`
	// Tone 本场情绪基调（如 压抑/紧张/温暖/释然）。用于情绪曲线校验：
	// 全片一路同一情绪 = 没有起伏，成片会"平"。
	Tone string `json:"tone,omitempty"`
	// Purpose 本场在叙事中的目的（"建立处境""推动决定""关系转折"…）。
	// 没有目的的场景在成片里就是凑数镜头——结构必须显式建模，不能靠模型自觉。
	Purpose  string `json:"purpose"`
	Conflict string `json:"conflict,omitempty"` // 本场的冲突/障碍（谁要什么，什么拦着）
	// Turn 本场的转折（人物关系/信息/情绪发生了什么变化）。全片至少一场有转折，
	// 否则剧本是"情况的罗列"而不是"故事"。
	Turn     string         `json:"turn,omitempty"`
	Action   string         `json:"action"`
	Dialogue []DialogueLine `json:"dialogue,omitempty"`
}

type DialogueLine struct {
	Character string `json:"character"`
	Line      string `json:"line"`
}

type Script struct {
	Title   string   `json:"title"`
	Logline string   `json:"logline"`
	Genre   string   `json:"genre"`
	Style   string   `json:"style"`
	Themes  []string `json:"themes"`
	Scenes  []Scene  `json:"scenes"`
}

// todConflicts 时段与场景动作的矛盾词表（确定性结构校验）。
// 真实案例：s3 的 action 写"天际泛起微蓝的晨光"，time_of_day 却是"黄昏"——
// 这种矛盾会原样流进分镜与生成，出片即穿帮。词表刻意收窄到"必然矛盾"的组合，
// 避免把闪回/对比修辞误杀。
var todConflicts = map[string][]string{
	"夜":     {"晨光", "清晨", "朝阳", "日出", "曙光", "拂晓", "正午"},
	"深夜":    {"晨光", "清晨", "朝阳", "日出", "曙光", "拂晓", "正午", "夕阳"},
	"日":     {"夜色", "深夜", "月光", "繁星", "星河", "午夜", "夜幕"},
	"白天":    {"夜色", "深夜", "月光", "繁星", "星河", "午夜", "夜幕"},
	"晨":     {"夜色", "深夜", "繁星", "星河", "午夜", "夕阳", "夜幕"},
	"清晨":    {"夜色", "深夜", "繁星", "星河", "午夜", "夕阳", "夜幕"},
	"黄昏":    {"晨光", "清晨", "朝阳", "日出", "曙光", "拂晓"},
	"傍晚":    {"晨光", "清晨", "朝阳", "日出", "曙光", "拂晓"},
	"night": {"dawn", "sunrise", "morning light", "daylight"},
	"dawn":  {"starry", "midnight", "moonlight", "sunset"},
	"day":   {"nightfall", "midnight", "starry"},
	"dusk":  {"dawn", "sunrise", "morning light"},
}

// timeOfDayContradiction 返回该场景"时段 vs 动作描述"的矛盾说明；无矛盾返回空。
func timeOfDayContradiction(tod, action string) string {
	t := strings.ToLower(strings.TrimSpace(tod))
	for key, bads := range todConflicts {
		if !strings.Contains(t, strings.ToLower(key)) {
			continue
		}
		for _, bad := range bads {
			if strings.Contains(action, bad) {
				return fmt.Sprintf("时段=%s 与动作描述中的 %q 矛盾", tod, bad)
			}
		}
	}
	return ""
}

type scriptSkill struct{}

func (scriptSkill) Name() string        { return "script" }
func (scriptSkill) Description() string { return "剧本创作：三幕结构、场景与台词" }

func (scriptSkill) Run(ctx *skill.Context) (any, error) {
	s := &Script{}
	if ctx.AI.Mock() {
		s = mockScript()
	} else {
		system := filmBase + `
你是编剧。产出电影剧本 JSON，schema：
{"title":"片名","logline":"一句话故事","genre":"类型","style":"整体视觉风格与画幅","themes":["主题"],
 "scenes":[{"id":"s1","act":1,"tone":"压抑","location":"场景地点","time_of_day":"日/夜/黄昏","purpose":"本场在叙事中的目的","conflict":"本场的冲突或障碍","turn":"本场结尾发生的转折（信息/关系/情绪变化）","action":"本场动作与情绪描述","dialogue":[{"character":"角色","line":"台词"}]}]}
要求：
1. 场景数与 brief 体量匹配（默认 3-6 场）；三幕结构清晰——第一幕建立处境与欲望，第二幕冲突升级，第三幕给变化与余味；每场标注 act（1/2/3）与 tone（本场情绪基调，如 压抑/紧张/温暖/释然）；
2. 每场必须写 purpose（这场为什么存在），相邻两场的 purpose 不得雷同；全片至少一场有明确的 turn（转折），没有转折的剧本只是情况的罗列；情绪要有起伏——tone 最好有对比（如压抑→释放）；
3. 台词克制、有潜台词；每个场景的 action 必须具体可分镜；
4. time_of_day 与 action 严禁自相矛盾（例如夜场写"晨光"）；
5. 画面表达走含蓄暗示路线：不要直写血腥、伤口、尸体、兵器、束缚、注射等具象负面元素（如 血迹/流淌/撕裂/勒痕），冲突与创伤用光影、道具、留白与表演来暗示——场景 action 会原样流进分镜与图像提示词，直白的负面词汇会触发生成平台内容审核拦截，整镜报废；只输出 JSON。`
		user := "创作意图：" + ctx.Brief + "\n片长目标(分钟)：" + ctx.ParamStr("duration_min", "3") +
			"\n风格要求：" + ctx.ParamStr("style", "由你根据 brief 决定")
		if err := callJSON(ctx, "script", system, user, s); err != nil {
			return nil, err
		}
	}
	if err := validate(s.Title != "" && len(s.Scenes) > 0, "缺少片名或场景"); err != nil {
		return nil, err
	}
	// 专业校验：场景 ID 唯一（下游分镜/剪辑以 ID 关联），动作描述非空
	seenScene := map[string]bool{}
	for _, sc := range s.Scenes {
		if sc.ID == "" {
			return nil, fmt.Errorf("产出校验失败: 存在缺少 id 的场景，每个场景必须有唯一 id（s1、s2…）")
		}
		if seenScene[sc.ID] {
			return nil, fmt.Errorf("产出校验失败: 场景 id 重复（%s），请重新编号保证唯一", sc.ID)
		}
		seenScene[sc.ID] = true
		if strings.TrimSpace(sc.Action) == "" {
			return nil, fmt.Errorf("产出校验失败: 场景 %s 缺少 action 动作描述，无法进入分镜，请补写", sc.ID)
		}
		if strings.TrimSpace(sc.Purpose) == "" && !ctx.AI.Mock() {
			return nil, fmt.Errorf("产出校验失败: 场景 %s 缺少 purpose（本场为什么存在）。没有叙事目的的场景在成片里只是凑数镜头，请为每场补写目的", sc.ID)
		}
		if c := timeOfDayContradiction(sc.TimeOfDay, sc.Action); c != "" {
			return nil, fmt.Errorf("产出校验失败: 场景 %s %s（时段与画面描述冲突会原样流进生成，出片即穿帮）；请对齐 time_of_day 与 action", sc.ID, c)
		}
	}
	// 结构下限：全片至少一场有转折（≥2 场时才检查，单片场景没有"转折"可言）
	if !ctx.AI.Mock() && len(s.Scenes) >= 2 {
		hasTurn := false
		for _, sc := range s.Scenes {
			if strings.TrimSpace(sc.Turn) != "" {
				hasTurn = true
				break
			}
		}
		if !hasTurn {
			return nil, fmt.Errorf("产出校验失败: 全片没有任何一场写了 turn（转折）。没有转折的故事是情况的罗列，请至少为一场补写结尾发生的信息/关系/情绪变化")
		}
	}
	checkNarrativeArc(ctx, s)
	return s, nil
}

// checkNarrativeArc 节拍表校验：幕结构、三幕占比、转折落位、情绪曲线。
// 模型声明了 act（≥3 场且全部标了幕号）才启用——有结构才有结构校验，
// 缺幕号时不拦（老剧本/单片场仍按 turn 存在性兜底）。
// 硬失败留给"结构确实断了"的情况（幕倒退/整幕缺失），软问题打警告不重烧剧本。
func checkNarrativeArc(ctx *skill.Context, s *Script) {
	if ctx.AI.Mock() || len(s.Scenes) < 3 {
		return
	}
	allLabeled := true
	for _, sc := range s.Scenes {
		if sc.Act < 1 || sc.Act > 3 {
			allLabeled = false
			break
		}
	}
	if !allLabeled {
		return
	}

	// 幕序必须非降（1→2→3 可以原地多场，不能倒回去）——倒退意味着叙事顺序乱了
	prev := 0
	for _, sc := range s.Scenes {
		if sc.Act < prev {
			failNarrative("场景 %s 的幕号从 %d 倒退回 %d，叙事顺序断裂；请按 1→2→3 重新编排场景", sc.ID, prev, sc.Act)
		}
		prev = sc.Act
	}

	// 体量占比：以"动作描述+台词"的字符量近似每场体量（剧本阶段还没有实测时长）。
	// 经典三幕约 25/50/25；整幕缺失是结构断裂，直接失败；比例失衡只警告——
	// 节奏是创作判断，误杀的代价是整条剧本链白烧。
	weight := func(sc Scene) int {
		n := len([]rune(sc.Action))
		for _, d := range sc.Dialogue {
			n += len([]rune(d.Line))
		}
		if n <= 0 {
			n = 1
		}
		return n
	}
	total := 0
	actWeight := map[int]int{}
	actScenes := map[int]int{}
	for _, sc := range s.Scenes {
		w := weight(sc)
		total += w
		actWeight[sc.Act] += w
		actScenes[sc.Act]++
	}
	for act := 1; act <= 3; act++ {
		if actScenes[act] == 0 {
			failNarrative("第 %d 幕一场都没有（幕号只有 1/2/3，缺哪幕哪幕的戏剧任务就没人完成）；请补齐该幕或调整幕号", act)
		}
	}
	if total > 0 {
		share := func(act int) float64 { return float64(actWeight[act]) / float64(total) * 100 }
		if share(2) < 20 {
			ctx.Log("⚠ 结构警告：第二幕只占 %.0f%% 体量——中段是冲突升级的引擎，太薄会让片子直接从铺垫跳结局；建议扩充第二幕的对抗与代价", share(2))
		}
		if share(1) > 55 {
			ctx.Log("⚠ 结构警告：第一幕占 %.0f%% 体量——铺垫过长，观众还没等到冲突就划走了；建议压缩开场建立段", share(1))
		}
	}

	// 转折落位：转折全部堆在第一幕 = 把"变化"讲在了冲突开始之前
	firstActTurns := 0
	turnCount := 0
	for _, sc := range s.Scenes {
		if strings.TrimSpace(sc.Turn) != "" {
			turnCount++
			if sc.Act == 1 {
				firstActTurns++
			}
		}
	}
	if turnCount > 0 && firstActTurns == turnCount {
		ctx.Log("⚠ 结构警告：全部 %d 处转折都落在第一幕——中后段没有变化，第三幕的结局会失去支撑；至少把一处转折放进第二或第三幕", turnCount)
	}

	// 情绪曲线：≥70% 场戏同一基调 = 全片一个情绪走到底，成片必然"平"
	toneCount := map[string]int{}
	labeled := 0
	for _, sc := range s.Scenes {
		if t := strings.TrimSpace(sc.Tone); t != "" {
			toneCount[t]++
			labeled++
		}
	}
	for t, n := range toneCount {
		if labeled >= 3 && float64(n)/float64(labeled) >= 0.7 {
			ctx.Log("⚠ 结构警告：%d 场里 %d 场的情绪基调都是「%s」——情绪一条道走到黑没有起伏；建议让 tone 出现对比（压抑→释放、紧张→松弛）", labeled, n, t)
			break
		}
	}

	// 相邻场景 purpose 雷同：两场戏干同一件事，必有一场是凑数
	for i := 1; i < len(s.Scenes); i++ {
		a, b := strings.TrimSpace(s.Scenes[i-1].Purpose), strings.TrimSpace(s.Scenes[i].Purpose)
		if a != "" && a == b {
			ctx.Log("⚠ 结构警告：场景 %s 与 %s 的 purpose 完全相同（%q）——两场戏在干同一件事，请合并或让后一场推进新信息", s.Scenes[i-1].ID, s.Scenes[i].ID, a)
		}
	}
}

// failNarrative 结构断裂类校验失败（触发带修复指引的重试）
func failNarrative(format string, args ...any) error {
	return fmt.Errorf("产出校验失败: "+format, args...)
}

// ---------------- S2 角色圣经（跨镜头一致性关键） ----------------

type Character struct {
	Name       string `json:"name"`
	Appearance string `json:"appearance"` // 外观描述
	Wardrobe   string `json:"wardrobe"`   // 服装
	Voice      string `json:"voice"`      // 音色设定
	// TTSVoice 实际音色名（从音色池里选，一个角色一个）。这是"一开口就知道
	// 是谁"的落地字段：narrate 按它路由 TTS 音色，配音阶段打印音色表可核对。
	TTSVoice          string `json:"tts_voice,omitempty"`
	ConsistencyTokens string `json:"consistency_tokens"` // 一致性标识（喂给图像/视频模型的锚定描述）
}

type CharacterBible struct {
	Characters []Character `json:"characters"`
}

type charactersSkill struct{}

func (charactersSkill) Name() string { return "characters" }
func (charactersSkill) Description() string {
	return "角色圣经：外观/服装/音色/一致性锚点"
}

// defaultVoicePool 智谱 GLM-TTS 的官方预置音色（docs.bigmodel.cn 文本转语音 API 的
// voice 枚举）。tongtong 是平台默认音色——旁白默认也用它，因此分配时排在最后，
// 先把更有个性的音色留给角色，旁白才能天然区分开。
// 工作流参数 voice_pool 可整体替换；单个角色仍可被 config.audio.voice_map 覆盖。
var defaultVoicePool = []string{"tongtong", "chuichui", "xiaochen", "jam"}

// voicePoolOf 解析音色池：工作流参数 voice_pool（逗号分隔字符串或数组）> 默认池。
func voicePoolOf(ctx *skill.Context) []string {
	raw, ok := ctx.Params["voice_pool"]
	if !ok {
		return defaultVoicePool
	}
	switch v := raw.(type) {
	case string:
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	case []any:
		var out []string
		for _, it := range v {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return defaultVoicePool
}

// assignDistinctVoices 保证"一个角色一个音色"（模型给偏好，代码给下限）：
//   - 模型选的音色在池内且未被占用 → 保留（模型的性别/气质匹配判断优先）
//   - 缺失、重复或越池的 → 按池序补齐；tongtong（旁白默认音色）排最后分配
//   - 池不够用 → 重复者清空，narrate 层回落默认音色并用变调区分，显式告警
//
// 返回需要上报的说明（无可分配音色等）。
func assignDistinctVoices(chars []Character, pool []string) []string {
	// 分配顺序：非默认音色在前，tongtong（旁白默认）最后
	ordered := make([]string, 0, len(pool))
	for _, v := range pool {
		if v != "tongtong" {
			ordered = append(ordered, v)
		}
	}
	for _, v := range pool {
		if v == "tongtong" {
			ordered = append(ordered, v)
		}
	}
	inPool := func(v string) bool {
		for _, p := range pool {
			if p == v {
				return true
			}
		}
		return false
	}
	used := map[string]bool{}
	var notes []string
	for i := range chars {
		v := strings.TrimSpace(chars[i].TTSVoice)
		if v != "" && inPool(v) && !used[v] {
			used[v] = true
			continue
		}
		chars[i].TTSVoice = ""
	}
	for i := range chars {
		if chars[i].TTSVoice != "" {
			continue
		}
		picked := ""
		for _, v := range ordered {
			if !used[v] {
				picked = v
				break
			}
		}
		if picked == "" {
			notes = append(notes, fmt.Sprintf(
				"音色池不足以区分角色 %q：将与其他角色共用音色，配音时靠变调区分（可在 config.audio.voice_map 为其单独指定）",
				chars[i].Name))
			continue
		}
		chars[i].TTSVoice = picked
		used[picked] = true
	}
	return notes
}

func (charactersSkill) Run(ctx *skill.Context) (any, error) {
	var script Script
	if err := ctx.DepOf("script", &script); err != nil {
		return nil, err
	}
	voicePool := voicePoolOf(ctx)
	out := &CharacterBible{}
	if ctx.AI.Mock() {
		out = mockCharacters()
	} else {
		system := filmBase + `
你是角色设计总监。基于剧本产出角色圣经 JSON，schema：
{"characters":[{"name":"角色名","appearance":"外貌细节（发型/五官/年龄/体态/标志性特征）","wardrobe":"服装描述","voice":"音色与语速","tts_voice":"从音色池里为该角色选的实际音色名","consistency_tokens":"小写英文连字符的一致性锚点串，供图像/视频模型锁定角色"}]}
要求：appearance 必须具体到可用于文生图的一致性控制；tts_voice 必须从音色池 ` + strings.Join(voicePool, "/") +
			` 中选取、每个角色的 tts_voice 互不相同（这是"一个角色一个声音"的硬性要求），按角色性别/年龄/气质匹配；所有有台词或特写的角色都要覆盖；只输出 JSON。`
		user := "剧本：" + marshal(script) + "\n风格：" + script.Style
		if err := callJSON(ctx, "characters", system, user, out); err != nil {
			return nil, err
		}
	}
	if err := validate(len(out.Characters) > 0, "角色列表为空"); err != nil {
		return nil, err
	}
	// 音色去重是确定性兜底：模型漏选/重复/越池都在这里修正，不靠重跑碰运气
	for _, note := range assignDistinctVoices(out.Characters, voicePool) {
		if ctx.Log != nil {
			ctx.Log("  ⚠ %s", note)
		}
	}
	// 专业校验：锚点格式与唯一性——这是跨镜头角色一致性的根基，错一个字符就锁定失败
	tokens := map[string]bool{}
	for _, c := range out.Characters {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Appearance) == "" {
			return nil, fmt.Errorf("产出校验失败: 角色 %q 缺少 name 或 appearance，appearance 必须具体到可用于文生图", c.Name)
		}
		ct := strings.TrimSpace(c.ConsistencyTokens)
		if !tokenRe.MatchString(ct) {
			return nil, fmt.Errorf("产出校验失败: 角色 %q 的 consistency_tokens=%q 不合规，必须为小写英文/数字 + 连字符（如 lin-female35-short-hair），请重写", c.Name, ct)
		}
		if tokens[ct] {
			return nil, fmt.Errorf("产出校验失败: 多个角色共用 consistency_tokens=%q，锚点必须一人一串，请为每个角色重写唯一锚点", ct)
		}
		tokens[ct] = true
	}
	// 专业校验：剧本里有台词的角色必须入册，否则后续配音/提示词无锚可引
	var missing []string
	for _, sc := range script.Scenes {
		for _, d := range sc.Dialogue {
			covered := false
			for _, c := range out.Characters {
				if nameMatches(d.Character, c.Name) {
					covered = true
					break
				}
			}
			if !covered {
				missing = append(missing, d.Character)
			}
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("产出校验失败: 剧本中有台词的角色 %s 未进入角色圣经，请为这些角色补充条目（含 consistency_tokens）", strings.Join(unique(missing), "、"))
	}
	return out, nil
}

// ---------------- S3 分镜表（镜头语言） ----------------

type Shot struct {
	ID          string   `json:"id"`
	SceneID     string   `json:"scene_id"`
	Description string   `json:"description"`
	ShotSize    string   `json:"shot_size"`   // 大远景/远景/中景/近景/特写/大特写
	CameraMove  string   `json:"camera_move"` // 推拉摇移/固定/手持/航拍…
	DurationSec float64  `json:"duration_sec"`
	Location    string   `json:"location"`
	Characters  []string `json:"characters"`
}

type Storyboard struct {
	Shots            []Shot  `json:"shots"`
	TotalDurationSec float64 `json:"total_duration_sec"`
}

var shotSizeEnum = map[string]bool{
	"大远景": true, "远景": true, "全景": true, "中景": true, "近景": true, "特写": true, "大特写": true,
}

// cameraMoveEnum 与 promspec.cameraMoveToEn 严格同步（那边有的这边必须收）：
// 枚举外的运镜拿不到确定性的英文运动描述与运动幅度映射，等于把镜头语言
// 交还给模型即兴发挥——"电影感有确定下限"就破了。
var cameraMoveEnum = map[string]bool{
	"固定": true, "缓推": true, "缓拉": true, "横移": true, "摇": true,
	"手持跟随": true, "航拍": true, "升降": true, "环绕": true,
}

type storyboardSkill struct{}

func (storyboardSkill) Name() string { return "storyboard" }
func (storyboardSkill) Description() string {
	return "分镜设计：景别/运镜/时长/场景衔接"
}

func (storyboardSkill) Run(ctx *skill.Context) (any, error) {
	var script Script
	if err := ctx.DepOf("script", &script); err != nil {
		return nil, err
	}
	out := &Storyboard{}
	if ctx.AI.Mock() {
		out = mockStoryboard()
	} else {
		// 时长预算 → 镜头数指引：分镜不知道目标片长就会随机涌现片长
		// （brief 要 3 分钟、成片 87 秒就是这么来的）。给模型明确的镜头数区间。
		shotGuidance := ""
		if target := durationTargetSec(ctx); target > 0 {
			lo := int(target / 8.0) // 全部 8 秒长镜的最少数量
			hi := int(target / 2.5) // 全部 2.5 秒短镜的最多数量
			if hi < lo+2 {
				hi = lo + 2
			}
			if hi > 80 {
				hi = 80
			}
			shotGuidance = fmt.Sprintf("\n片长目标 %.0f 秒：镜头总数应在 %d-%d 个之间（按平均 4-5 秒/镜估算，请把总时长做进这个区间，不许只拍一半就收）", target, lo, hi)
		}
		system := filmBase + `
你是分镜师。基于剧本产出分镜表 JSON，schema：
{"shots":[{"id":"sh001","scene_id":"s1","description":"画面内容与表演调度","shot_size":"大远景|远景|全景|中景|近景|特写|大特写","camera_move":"固定|缓推|缓拉|横移|摇|手持跟随|航拍|升降|环绕","duration_sec":4.0,"location":"地点","characters":["出场角色"]}],"total_duration_sec":总计秒数}
专业要求：景别成组（建立镜头→反应镜头→细节镜头，同一景别不连续超过 2 镜）；镜头时长 2-8 秒（AI 视频生成友好）；场景切换有视觉过渡设计；每个场景至少 2 个镜头；把剧本里每场的 purpose/turn 落实为具体的镜头设计（转折点要给到镜头级：让谁在什么景别里完成变化）；有台词的镜头注意口型风险——AI 生成的口型与配音对不上，说台词的镜头优先用"看不见嘴"的电影语言承载：听者反应镜头、过肩、侧脸/背身、前景遮挡、手部或道具大特写、说话人出画（画外音），避免正面正对说话人嘴部的 特写/大特写；camera_move 必须严格使用枚举值；只输出 JSON。` + shotGuidance
		user := "剧本：" + marshal(script)
		if fb, _ := ctx.Params["revision_feedback"].(string); fb != "" {
			user += "\n返修重点：" + fb
		}
		if err := callJSON(ctx, "storyboard", system, user, out); err != nil {
			return nil, err
		}
	}
	if err := validate(len(out.Shots) > 0, "分镜为空"); err != nil {
		return nil, err
	}

	sceneIDs := map[string]bool{}
	for _, sc := range script.Scenes {
		sceneIDs[sc.ID] = true
	}
	shotIDs := map[string]bool{}
	for _, sh := range out.Shots {
		if sh.ID == "" {
			return nil, fmt.Errorf("产出校验失败: 存在缺少 id 的镜头，请按 sh001 起重新编号")
		}
		if shotIDs[sh.ID] {
			return nil, fmt.Errorf("产出校验失败: 镜头 id 重复（%s），请重新编号保证唯一", sh.ID)
		}
		shotIDs[sh.ID] = true
		if !sceneIDs[sh.SceneID] {
			return nil, fmt.Errorf("产出校验失败: 镜头 %s 的 scene_id=%q 不存在于剧本，scene_id 必须引用剧本中的场景 id", sh.ID, sh.SceneID)
		}
		if sh.DurationSec < 2 || sh.DurationSec > 8 {
			return nil, fmt.Errorf("产出校验失败: 镜头 %s 时长 %.1f 秒超出 2-8 秒区间（AI 视频生成友好区间），请调整拆分", sh.ID, sh.DurationSec)
		}
		if !shotSizeEnum[sh.ShotSize] {
			return nil, fmt.Errorf("产出校验失败: 镜头 %s 的 shot_size=%q 不在枚举内（大远景/远景/全景/中景/近景/特写/大特写），请修正", sh.ID, sh.ShotSize)
		}
		if !cameraMoveEnum[sh.CameraMove] {
			return nil, fmt.Errorf("产出校验失败: 镜头 %s 的 camera_move=%q 不在枚举内（固定/缓推/缓拉/横移/摇/手持跟随/航拍/升降/环绕）。枚举外的运镜拿不到确定性的镜头语言编译，请改用枚举值", sh.ID, sh.CameraMove)
		}
	}
	// 场景覆盖 + 每场至少 2 镜：单镜的"场景"在成片里只是一张照片，
	// 建立与反应没有画面载体，叙事靠不住。
	shotsOfScene := map[string][]Shot{}
	for _, sh := range out.Shots {
		shotsOfScene[sh.SceneID] = append(shotsOfScene[sh.SceneID], sh)
	}
	var uncovered, tooSparse []string
	for _, sc := range script.Scenes {
		shots := shotsOfScene[sc.ID]
		if len(shots) == 0 {
			uncovered = append(uncovered, sc.ID)
			continue
		}
		if len(shots) < 2 && !ctx.AI.Mock() {
			tooSparse = append(tooSparse, fmt.Sprintf("%s(%d镜)", sc.ID, len(shots)))
		}
		// 景别成组的确定性下限：同场景连续 3 镜同景别 = 没有"建立→反应→细节"
		// 的覆盖设计，观众看的是三张构图雷同的照片
		runLen, runSize := 1, shots[0].ShotSize
		for _, sh := range shots[1:] {
			if sh.ShotSize == runSize {
				runLen++
				continue
			}
			runLen, runSize = 1, sh.ShotSize
		}
		if runLen >= 3 {
			return nil, fmt.Errorf("产出校验失败: 场景 %s 有连续 %d 个镜头同为 %q，缺少景别变化（建立→反应→细节）。请调整景别或合并镜头", sc.ID, runLen, runSize)
		}
	}
	if len(uncovered) > 0 {
		return nil, fmt.Errorf("产出校验失败: 剧本场景 %s 没有任何镜头覆盖，请为这些场景补拍分镜", strings.Join(uncovered, "、"))
	}
	if len(tooSparse) > 0 {
		return nil, fmt.Errorf("产出校验失败: 场景 %s 只有 1 个镜头（每场至少 2 镜：建立与反应都需要画面载体），请补充分镜", strings.Join(tooSparse, "、"))
	}
	out.TotalDurationSec = 0 // 总时长永远本地重算，不信任模型自报
	for _, sh := range out.Shots {
		out.TotalDurationSec += sh.DurationSec
	}
	// 时长预算门禁：成片时长是"镜头数 × 时长"涌现出来的，不设门禁就会
	// brief 要 3 分钟、成片 87 秒。低于 55% 判失败（叙事一定被砍没了），
	// 高于 140% 同样判失败（体量失控）；55%-80% 放行但显式提醒。
	if !ctx.AI.Mock() {
		if target := durationTargetSec(ctx); target > 0 {
			ratio := out.TotalDurationSec / target
			switch {
			case ratio < 0.55:
				return nil, fmt.Errorf("产出校验失败: 分镜总时长 %.0f 秒，只有片长目标 %.0f 秒的 %.0f%%——按当前分镜出片会严重短于承诺。请增加镜头（含反应镜头与细节镜头）或延长镜头内动作", out.TotalDurationSec, target, ratio*100)
			case ratio > 1.4:
				return nil, fmt.Errorf("产出校验失败: 分镜总时长 %.0f 秒，超出片长目标 %.0f 秒的 %.0f%%——体量失控会拉高生成成本且节奏拖沓，请压缩", out.TotalDurationSec, target, ratio*100)
			case ratio < 0.8 && ctx.Log != nil:
				ctx.Log("  ⚠ 分镜总时长 %.0f 秒，为片长目标的 %.0f%%（低于 80%）：如非刻意留白建议补足", out.TotalDurationSec, ratio*100)
			}
		}
	}
	return out, nil
}

// durationTargetSec 片长目标（秒）。读工作流参数 duration_min（分钟，可带小数）；
// 未提供或非法返回 0（不做预算检查）。
func durationTargetSec(ctx *skill.Context) float64 {
	raw := strings.TrimSpace(ctx.ParamStr("duration_min", ""))
	if raw == "" {
		return 0
	}
	var min float64
	if _, err := fmt.Sscanf(raw, "%f", &min); err != nil || min <= 0 {
		return 0
	}
	return min * 60
}

// ---------------- S4 生成提示词包 ----------------

type ShotPrompts struct {
	ID             string   `json:"id"`
	ImagePrompt    string   `json:"image_prompt"`    // 首帧图像提示词（英文，喂文生图）
	NegativePrompt string   `json:"negative_prompt"` // 负面提示词
	VideoPrompt    string   `json:"video_prompt"`    // 图生视频运动提示词
	CharacterRefs  []string `json:"character_refs"`  // 一致性锚点引用
}

type PromptPack struct {
	Shots []ShotPrompts `json:"shots"`
}

type promptsSkill struct{}

func (promptsSkill) Name() string { return "prompts" }
func (promptsSkill) Description() string {
	return "提示词工程：首帧/运镜/负面提示词/角色锚定"
}

func (promptsSkill) Run(ctx *skill.Context) (any, error) {
	var sb Storyboard
	var bible CharacterBible
	if err := ctx.DepOf("storyboard", &sb); err != nil {
		return nil, err
	}
	if err := ctx.DepOf("characters", &bible); err != nil {
		return nil, err
	}
	out := &PromptPack{}
	if ctx.AI.Mock() {
		out = mockPrompts(&sb)
	} else {
		system := filmBase + `
你是 AI 视觉提示词工程师。基于分镜表与角色圣经，产出每个镜头的提示词包 JSON，schema：
{"shots":[{"id":"sh001","image_prompt":"英文首帧提示词：主体+环境+光线+色调+镜头+质感，含画幅","negative_prompt":"中文负面提示词：低分辨率/多余手指/水印/扭曲面部等","video_prompt":"图生视频运动描述（对应 camera_move 与表演）","character_refs":["从角色圣经引用的 consistency_tokens"]}]}
专业要求：image_prompt 用英文、工业级细节密度；同一角色跨镜头必须引用相同 consistency_tokens；负面提示词覆盖常见 AI 伪影；只输出 JSON。`
		user := "分镜表：" + marshal(sb) + "\n角色圣经：" + marshal(bible)
		if err := callJSON(ctx, "prompts", system, user, out); err != nil {
			return nil, err
		}
	}
	// 专业校验：与分镜逐镜头对齐（顺序+ID），渲染/剪辑按位取用，错位即错片
	if len(out.Shots) != len(sb.Shots) {
		return nil, fmt.Errorf("提示词镜头数(%d)与分镜数(%d)不一致，必须一一对应，请按分镜顺序逐镜头补齐", len(out.Shots), len(sb.Shots))
	}
	validTokens := map[string]bool{}
	for _, c := range bible.Characters {
		validTokens[strings.TrimSpace(c.ConsistencyTokens)] = true
	}
	for i, p := range out.Shots {
		if p.ID != sb.Shots[i].ID {
			return nil, fmt.Errorf("提示词第 %d 条 id=%q 与分镜第 %d 个镜头 id=%q 错位，请严格按分镜顺序输出", i+1, p.ID, i+1, sb.Shots[i].ID)
		}
		if strings.TrimSpace(p.ImagePrompt) == "" {
			return nil, fmt.Errorf("镜头 %s 缺少 image_prompt，请补写英文首帧提示词", p.ID)
		}
		if hasCJK(p.ImagePrompt) {
			return nil, fmt.Errorf("镜头 %s 的 image_prompt 含中文，文生图模型要求英文工业级提示词，请整句重写为英文", p.ID)
		}
		if strings.TrimSpace(p.VideoPrompt) == "" {
			return nil, fmt.Errorf("镜头 %s 缺少 video_prompt 运动描述，请补写", p.ID)
		}
		if strings.TrimSpace(p.NegativePrompt) == "" {
			return nil, fmt.Errorf("镜头 %s 缺少 negative_prompt，请补写覆盖 AI 伪影的负面提示词", p.ID)
		}
		for _, ref := range p.CharacterRefs {
			if !validTokens[strings.TrimSpace(ref)] {
				return nil, fmt.Errorf("镜头 %s 引用了一致性锚点 %q，但角色圣经中没有该锚点；character_refs 必须逐字引用圣经中的 consistency_tokens", p.ID, ref)
			}
		}
	}
	return out, nil
}

// ---------------- 工具 ----------------

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---------------- Mock 数据 ----------------

func mockScript() *Script {
	return &Script{
		Title:   "归途",
		Logline: "深空第七年，星际信使收到一束来自地球麦田的信号，决定赌上一切返航。",
		Genre:   "科幻/剧情",
		Style:   "电影感胶片质感，2.39:1 宽画幅，冷青与暖金对比，颗粒感",
		Themes:  []string{"归乡", "希望", "孤独"},
		Scenes: []Scene{
			{ID: "s1", Act: 1, Tone: "孤独压抑", Location: "废弃空间站驾驶舱", TimeOfDay: "夜",
				Purpose:  "建立处境：深空孤独与归乡欲望的触发点",
				Conflict: "信号真假难辨，燃料只够一次转向",
				Turn:     "林决定相信信号，调转航向——故事从漂泊变为归途",
				Action:   "控制台突然亮起微弱信号，林确认坐标来自地球，调转航向。",
				Dialogue: []DialogueLine{{Character: "信使·林", Line: "信号源……是麦田？"}}},
			{ID: "s2", Act: 3, Tone: "温暖释然", Location: "地球麦田上空", TimeOfDay: "黄昏",
				Purpose:  "兑现欲望并给情绪落点：归途的终点",
				Conflict: "七年航行换来的重逢只有一瞬",
				Turn:     "孤独被麦田的金色化解，恐惧转为释然",
				Action:   "飞船破云而出，金色麦田在夕阳下铺展到地平线。",
				Dialogue: []DialogueLine{{Character: "信使·林", Line: "我回来了。"}}},
		},
	}
}

func mockCharacters() *CharacterBible {
	return &CharacterBible{Characters: []Character{
		{Name: "信使·林", Appearance: "35 岁亚洲女性，利落短发，左眉浅疤，眼神坚定", Wardrobe: "做旧深灰航天服，胸前挂黄铜罗盘", Voice: "低沉平静，语速偏慢", ConsistencyTokens: "lin-female35-short-hair-scar-brass-compass"},
		{Name: "舱载AI·拾音", Appearance: "驾驶舱内环形青色全息光纹，随语音流动", Wardrobe: "无实体", Voice: "合成女声，温暖克制", ConsistencyTokens: "ai-ring-hologram-cyan"},
	}}
}

func mockStoryboard() *Storyboard {
	return &Storyboard{Shots: []Shot{
		{ID: "sh001", SceneID: "s1", Description: "大特写：控制台信号灯闪烁，倒映在林的眼中", ShotSize: "大特写", CameraMove: "缓推", DurationSec: 4, Location: "废弃空间站驾驶舱", Characters: []string{"信使·林"}},
		{ID: "sh002", SceneID: "s1", Description: "中景：林猛地抬头，抓住操纵杆调转航向", ShotSize: "中景", CameraMove: "手持跟随", DurationSec: 6, Location: "废弃空间站驾驶舱", Characters: []string{"信使·林"}},
		{ID: "sh003", SceneID: "s2", Description: "大远景：飞船穿出云层，金色麦田铺满大地", ShotSize: "大远景", CameraMove: "航拍", DurationSec: 8, Location: "地球麦田上空", Characters: []string{}},
		{ID: "sh004", SceneID: "s2", Description: "特写：林含泪微笑，阳光掠过侧脸", ShotSize: "特写", CameraMove: "固定", DurationSec: 5, Location: "地球麦田上空", Characters: []string{"信使·林"}},
	}, TotalDurationSec: 23}
}

func mockPrompts(sb *Storyboard) *PromptPack {
	neg := "低分辨率, 多余手指, 扭曲面部, 文字水印, 画面抖动伪影"
	m := map[string]ShotPrompts{
		"sh001": {ID: "sh001", ImagePrompt: "cinematic extreme close-up, blinking amber signal lights on a worn control console reflected in a woman's determined eyes, teal shadows, film grain, 2.39:1", NegativePrompt: neg, VideoPrompt: "缓慢推近，信号灯闪烁渐强，瞳孔微缩", CharacterRefs: []string{"lin-female35-short-hair-scar-brass-compass"}},
		"sh002": {ID: "sh002", ImagePrompt: "cinematic medium shot, female pilot in worn grey spacesuit gripping control yoke turning the ship, cockpit holographic cyan ring glowing, cold light, film grain, 2.39:1", NegativePrompt: neg, VideoPrompt: "手持跟随，身体急转，全息光纹波动", CharacterRefs: []string{"lin-female35-short-hair-scar-brass-compass", "ai-ring-hologram-cyan"}},
		"sh003": {ID: "sh003", ImagePrompt: "epic aerial wide shot, small spacecraft breaking through golden clouds above endless wheat field at sunset, warm gold light, volumetric god rays, 2.39:1", NegativePrompt: neg, VideoPrompt: "航拍横移，云层向后掠过，麦田随风起伏", CharacterRefs: []string{}},
		"sh004": {ID: "sh004", ImagePrompt: "cinematic close-up, woman's tearful smile lit by golden sunset, short hair strands moving in wind, shallow depth of field, film grain, 2.39:1", NegativePrompt: neg, VideoPrompt: "固定机位，风拂发丝，泪光闪动，微笑渐展", CharacterRefs: []string{"lin-female35-short-hair-scar-brass-compass"}},
	}
	shots := make([]ShotPrompts, 0, len(sb.Shots))
	for _, sh := range sb.Shots {
		if p, ok := m[sh.ID]; ok {
			shots = append(shots, p)
		}
	}
	return &PromptPack{Shots: shots}
}
