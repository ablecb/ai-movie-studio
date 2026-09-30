package deliver

import (
	"fmt"
	"path/filepath"
	"strings"

	"aistudio/internal/config"
	"aistudio/internal/fsx"
)

// ---------------------------------------------------------------------------
// 转场（editplan 方案的真正落地处）
//
// editplan 阶段一直会产出 fade_in/dissolve 的转场方案，但 deliver 的 concat
// 从不消费它——"方案写完就扔"，成片永远是硬切。这里把它变成真实画面。
//
// 关键设计：**叠化不改变成片时间轴**。若直接用 xfade 链，每处叠化会让总时长
// 缩短 N 秒，而字幕/配音/音效的入点全部按"各镜头实测时长之和"排版——时间轴
// 一缩，后面全部错位。因此每个叠化边界实现为：
//
//	上一镜尾段(真画面 N 秒) × 下一镜头段(真画面 N 秒) → xfade 叠化
//	下一镜剩余段轻微放慢，把叠化占用的 N 秒还回时间轴
//
// 每个边界都严格守恒：seg_prev(dur-N) + joint(N) + seg_next(减速补长后 = dur)
// = dur_prev + dur_next。成片总时长与镜头实测之和完全一致，下游所有实测
// 时间轴（narrate 排轨、字幕、音效挂点）原样有效。
//
// 为什么用"减速"而不是"末帧冻结"：冻结会在剪入下一镜前"卡一下"（每个叠化的
// B 侧片尾都是可见停顿）；轻微减速（≤1.2×时长）让运动自然收尾，观感是
// "这镜缓缓停下"，这正是剪辑里叠化的惯用节奏。减速比例上限由边界判定保证。
//
// 约束：不允许连续两个叠化边界（上一镜同时充当 A/B 两侧会双重消费视频流）。
// editplan 相邻边界都标 dissolve 时，后者自动降级为硬切。
// ---------------------------------------------------------------------------

// loadTransitions 读取 editplan.json 的转场方案：shotID → 该镜头起始处的转场类型
// （fade_in / dissolve / cut）。文件缺失（老 run / 跳过 editplan 阶段）返回空表，
// 全部按硬切处理，行为与旧版一致。
func loadTransitions(runDir string) map[string]string {
	var doc struct {
		Timeline []struct {
			ShotID     string `json:"shot_id"`
			Transition string `json:"transition"`
		} `json:"timeline"`
	}
	if err := fsx.ReadJSON(filepath.Join(runDir, "artifacts", "editplan.json"), &doc); err != nil {
		return nil
	}
	out := map[string]string{}
	for _, c := range doc.Timeline {
		out[c.ShotID] = strings.ToLower(strings.TrimSpace(c.Transition))
	}
	return out
}

// scopeCropHeight 按 Style.Aspect 计算宽银幕裁切后的可视高度（0 = 不裁切）。
// "2.39:1" 在 1920x1080 容器里 → 804（偶数对齐），上下各 letterbox 138px——
// HD scope 交付的标准做法；字幕烧录在 letterbox 之后，落在下黑边里（影院式字幕位）。
// 画幅不宽于 16:9、或裁切量不足 40px（肉眼不可辨）时返回 0。
func scopeCropHeight(aspect string, w, h int) int {
	aspect = strings.TrimSpace(aspect)
	if aspect == "" {
		return 0
	}
	parts := strings.Split(aspect, ":")
	if len(parts) != 2 {
		return 0
	}
	var aw, ah float64
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%g", &aw); err != nil || aw <= 0 {
		return 0
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%g", &ah); err != nil || ah <= 0 {
		return 0
	}
	ratio := aw / ah
	if ratio <= float64(w)/float64(h)+0.01 { // 不宽于交付容器：无从裁起
		return 0
	}
	scopeH := int(float64(w)/ratio + 0.5)
	if scopeH%2 == 1 {
		scopeH--
	}
	if scopeH <= 0 || h-scopeH < 40 {
		return 0
	}
	return scopeH
}

// buildConcatFilter 生成带转场的 filter_complex（纯函数，可单测）。
//
// 输出图结构（以 3 镜、镜 2 起始处叠化 N 秒为例）：
//
//	[0:v]scale…fps…format[n0];[n0]split=2[n0s][n0t]
//	[n0t]trim=start=…,setpts=PTS-STARTPTS[t1]
//	[1:v]scale…fps…format[n1];[n1]split=2[n1h][n1r]
//	[n1h]trim=end=N,setpts=PTS-STARTPTS[h1]
//	[n1r]trim=start=N,setpts=PTS-STARTPTS,setpts=减速因子*PTS,fps[r1]
//	[t1][h1]xfade=transition=fade:duration=N:offset=0[j1]
//	[n0s][j1][r1][n2]concat=n=4[vcat]
//	[vcat]fade=t=in…,fade=t=out…[vout]
func buildConcatFilter(clips []shotClip, tr map[string]string, w, h, fps int, tc config.Transitions,
	hasXfade, hasTpad bool, lumaAdj []float64, scopeH int) (string, error) {

	if len(clips) == 0 {
		return "", fmt.Errorf("没有任何可合成的镜头片段")
	}
	dissolveN := tc.DissolveSec
	// 叠化的减速偿还要求 B 侧足够长：durB ≥ 4N+0.2 时减速因子 ≤1.2（观感自然），
	// 更短的镜头退回硬切——为 0.6s 叠化把 1.5s 镜头放慢 40% 是本末倒置
	dissolvable := tc.Enabled && hasXfade && dissolveN > 0.05

	// 第一遍：判定每个边界是否真的做叠化
	// boundary[i] = 镜头 i 起始处与镜头 i-1 之间的叠化
	boundary := make([]bool, len(clips))
	for i := 1; i < len(clips); i++ {
		if tr[clips[i].ID] != "dissolve" {
			continue
		}
		if !dissolvable ||
			clips[i-1].Dur < 2*dissolveN+0.2 || clips[i].Dur < 4*dissolveN+0.2 ||
			boundary[i-1] { // 连续叠化：上一镜已是被消费的 B 侧，不能再当 A 侧
			continue
		}
		boundary[i] = true
	}

	// 第二遍：组装滤镜图。每个 n_i 只被消费一次：
	//   有出边叠化（boundary[i+1]）→ split 出 seg+tail；有入边叠化 → split 出 head+rest。
	var f []string
	segs := make([]string, len(clips))
	for i, c := range clips {
		// 宽银幕：先放大裁掉上下（scope 构图），eq 亮度校正插在裁切与补边之间，
		// letterbox 区域不会被抬成灰边
		prelude := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,", w, h)
		if scopeH > 0 && scopeH < h {
			prelude = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,", w, scopeH, w, scopeH)
		}
		eq := ""
		if i < len(lumaAdj) && lumaAdj[i] != 0 {
			eq = fmt.Sprintf("eq=brightness=%.4f,", lumaAdj[i])
		}
		f = append(f, fmt.Sprintf(
			"[%d:v]%s%spad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p[n%d]",
			i, prelude, eq, w, h, fps, i))

		outDissolve := i+1 < len(clips) && boundary[i+1]
		inDissolve := boundary[i]

		switch {
		case outDissolve:
			// 本镜是下一处叠化的 A 侧：独立段（截短）+ 尾段（真画面，进 xfade）
			f = append(f, fmt.Sprintf("[n%d]split=2[n%ds][n%dt]", i, i, i))
			f = append(f, fmt.Sprintf("[n%ds]trim=end=%.3f,setpts=PTS-STARTPTS[s%d]", i, c.Dur-dissolveN, i))
			f = append(f, fmt.Sprintf("[n%dt]trim=start=%.3f,setpts=PTS-STARTPTS[t%d]", i, c.Dur-dissolveN, i+1))
			segs[i] = fmt.Sprintf("[s%d]", i)
		case inDissolve:
			// 本镜是叠化的 B 侧：真头段进 xfade，剩余段减速补回 N 秒
			slow := c.Dur / (c.Dur - dissolveN)
			f = append(f, fmt.Sprintf("[n%d]split=2[n%dh][n%dr]", i, i, i))
			f = append(f, fmt.Sprintf("[n%dh]trim=end=%.3f,setpts=PTS-STARTPTS[h%d]", i, dissolveN, i))
			f = append(f, fmt.Sprintf(
				"[n%dr]trim=start=%.3f,setpts=PTS-STARTPTS,setpts=%.5f*PTS,fps=%d[r%d]", i, dissolveN, slow, fps, i))
			f = append(f, fmt.Sprintf(
				"[t%d][h%d]xfade=transition=fade:duration=%.3f:offset=0[j%d]", i, i, dissolveN, i))
			segs[i] = fmt.Sprintf("[r%d]", i)
		default:
			segs[i] = fmt.Sprintf("[n%d]", i)
		}
	}

	var chain strings.Builder
	segCount := 0
	for i := range clips {
		chain.WriteString(segs[i])
		segCount++
		if i+1 < len(clips) && boundary[i+1] {
			chain.WriteString(fmt.Sprintf("[j%d]", i+1))
			segCount++ // 叠化节点也是 concat 的一个输入段
		}
	}
	f = append(f, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[vcat]", chain.String(), segCount))

	// 片头淡入 / 片尾淡出（只调透明度，不改时长）
	var total float64
	for _, c := range clips {
		total += c.Dur
	}
	var post []string
	if tc.Enabled && tc.FadeInSec > 0.05 {
		post = append(post, fmt.Sprintf("fade=t=in:st=0:d=%.2f", tc.FadeInSec))
	}
	if tc.Enabled && tc.FadeOutSec > 0.05 {
		post = append(post, fmt.Sprintf("fade=t=out:st=%.2f:d=%.2f", total-tc.FadeOutSec, tc.FadeOutSec))
	}
	if len(post) > 0 {
		f = append(f, fmt.Sprintf("[vcat]%s[vout]", strings.Join(post, ",")))
	} else {
		f = append(f, "[vcat]null[vout]")
	}
	return strings.Join(f, ";"), nil
}
