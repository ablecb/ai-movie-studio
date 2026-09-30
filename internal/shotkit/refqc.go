// 参考帧质检：全片一致性的根。
//
// 真实事故：角色圣经写明圆框眼镜，参考帧出图时模型把眼镜丢了，而成片镜头里
// 眼镜又回来了——因为参考帧没有 QC，错误会经图生图放大到每一个出镜镜头，
// 直到交付时视觉复核才发现，那时重渲的已经不是一帧而是一部片。
//
// 做法与选角一样：候选标准像先对照"角色说明书"逐项验收，不合格打回去重拍。
// 视觉模型不可用时不拦生产（fail-open），但结论显式记入 manifest，绝不假装查过。

package shotkit

import (
	"fmt"
	"strings"

	"aistudio/internal/ai"
	"aistudio/internal/promspec"
)

// RefQCVerdict 视觉模型对参考帧的核对结论。
type RefQCVerdict struct {
	Pass  bool    `json:"pass"`
	Score float64 `json:"score"`
	// Missing 圣经有但画面里没有/不符的特征（丢眼镜、发型变了）
	Missing []string `json:"missing"`
	// Drift 画面里有但圣经没有的添加物（AI 自作主张加的胡子/首饰/纹身）
	Drift []string `json:"drift"`
	// Issues 其他问题（畸形手、塑料皮肤等参考帧也会有的生成缺陷）
	Issues  []string `json:"issues"`
	FixHint string   `json:"fix_hint"`
}

// refChecklist 把角色圣经编译成逐项核对清单（给视觉模型的验收标准）。
func refChecklist(c promspec.CharacterSpec) string {
	var b strings.Builder
	add := func(label, v string) {
		if s := strings.TrimSpace(v); s != "" {
			fmt.Fprintf(&b, "- %s: %s\n", label, s)
		}
	}
	add("年龄/族裔/体格", strings.Join([]string{c.Age, c.Ethnicity, c.Build}, "，"))
	add("发型/发色", c.Hair)
	add("五官与骨相", c.Face)
	add("眼睛（含是否戴眼镜及镜框样式）", c.Eyes)
	add("皮肤质感", c.Skin)
	add("服装", c.Wardrobe)
	if len(c.Distinctive) > 0 {
		add("标志性特征（每一项都必须可见）", strings.Join(c.Distinctive, "，"))
	}
	return b.String()
}

const refQCSystem = `你是电影选角导演兼连续性顾问。你会收到一张 AI 生成的角色参考帧（标准像），
请对照给出的"角色说明书清单"逐项核对，判断这张脸是否可以 locked 为全片一致性基准。
只输出 JSON：{"pass":布尔,"score":0-10,"missing":["清单有但画面缺失/不符的项"],"drift":["画面有但清单没有的添加物"],"issues":["其他生成缺陷（畸形/塑料感/三视图两人不一致）"],"fix_hint":"若不合格，给一句可直接追加进出图提示词的英文修正指令"}
判定要严格：missing 或 drift 非空即 pass=false； Score<7 也算不合格。清单里写了眼镜/疤痕/饰品而画面没有，是最严重的失格。`

// QCRefFrame 把参考帧送视觉模型对照角色圣经核对。
func QCRefFrame(vision *ai.Client, imagePath string, c promspec.CharacterSpec) (*RefQCVerdict, error) {
	prompt := "角色说明书（验收清单）：\n" + refChecklist(c) +
		"\n请核对这张参考帧。注意：眼镜有无与镜框形状、发型轮廓、面部标志特征是连续性的生命线。"
	var v RefQCVerdict
	if err := vision.VisionJSON(refQCSystem, prompt, []string{imagePath}, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// qcCorrectionPrompt 把核对结论编译成追加给出图提示词的修正指令。
func qcCorrectionPrompt(v *RefQCVerdict) string {
	var parts []string
	if len(v.Missing) > 0 {
		parts = append(parts, "The previous attempt was missing required identity anchors: "+strings.Join(v.Missing, "; ")+". They MUST be clearly visible.")
	}
	if len(v.Drift) > 0 {
		parts = append(parts, "The previous attempt wrongly added features that are NOT in the character spec, remove them: "+strings.Join(v.Drift, "; ")+".")
	}
	if s := strings.TrimSpace(v.FixHint); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		parts = append(parts, "Regenerate with stricter adherence to the character spec.")
	}
	return "\nCorrections required (a casting review rejected the previous attempt): " + strings.Join(parts, " ")
}
