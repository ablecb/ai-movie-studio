package skills

import (
	"fmt"
	"strings"
	"testing"

	"aistudio/internal/promspec"
)

// 口型风险门禁的回归：生成端无声、配音后期叠加，口型与台词必然对不上——
// "嘴清晰可见地在说话"是穿帮的最坏形态（尤其正对嘴部的特写），
// "看不见嘴"的构图天然规避。声明缺失或 visible+特写都必须在创作阶段拦下。

func mouthShot(size string, beats ...promspec.Beat) *promspec.ShotSpec {
	return &promspec.ShotSpec{ID: "sh007", ShotSize: size, DurationSec: 5, Beats: beats}
}

func onScreenLine() []promspec.Beat {
	return []promspec.Beat{{Kind: "dialogue", Speaker: "林夏", Text: "这一战，不算奉陪。", AtSec: 0.5}}
}

func TestApplyMouthVisibilityRules(t *testing.T) {
	cases := []struct {
		name     string
		shot     *promspec.ShotSpec
		declared string
		want     string // 期望归一后的取值（未报错时）
		wantErr  string // 期望错误包含的片段；空 = 不应报错
	}{
		{"无台词留空", mouthShot("中景"), "", "", ""},
		{"无台词却声明→清空", mouthShot("中景"), "visible", "", ""},
		{"旁白不算画面内台词→清空", mouthShot("中景",
			promspec.Beat{Kind: "narration", Speaker: "narrator", Text: "深空第七年。", AtSec: 0}), "visible", "", ""},
		{"有台词未声明→拒绝", mouthShot("中景", onScreenLine()...), "", "",
			"却没有声明 mouth_visibility"},
		{"非法取值→拒绝", mouthShot("中景", onScreenLine()...), "面朝镜头", "",
			"不合法"},
		{"visible+特写→拒绝", mouthShot("特写", onScreenLine()...), "visible", "",
			"正对嘴部的特写是口型穿帮的最坏形态"},
		{"visible+大特写→拒绝", mouthShot("大特写", onScreenLine()...), "visible", "",
			"正对嘴部的特写是口型穿帮的最坏形态"},
		{"visible+中景→放行", mouthShot("中景", onScreenLine()...), "visible", "visible", ""},
		{"covered+特写→放行", mouthShot("特写", onScreenLine()...), "covered", "covered", ""},
		{"offscreen+大特写→放行", mouthShot("大特写", onScreenLine()...), "offscreen", "offscreen", ""},
		{"大写归一", mouthShot("中景", onScreenLine()...), " VISIBLE ", "visible", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			err := applyMouthVisibility(tc.shot, tc.declared, logf)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				if tc.shot.MouthVisibility != tc.want {
					t.Fatalf("归一结果应为 %q，实际 %q", tc.want, tc.shot.MouthVisibility)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应包含 %q，实际: %v", tc.wantErr, err)
			}
		})
	}
}

// 清空"无台词却声明"的字段必须留下告警：静默清空等于让"模型理解错了"
// 这件事不可见（下一轮还可能继续错）。
func TestApplyMouthVisibilityWarnsOnUselessDeclaration(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	if err := applyMouthVisibility(mouthShot("中景"), "covered", logf); err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), "已清空") {
		t.Fatalf("应告警说明字段被清空，实际日志: %v", logs)
	}
}

// 景别修正：只在枚举内、由代码重算英文描述与焦段——
// 只改英文描述而留着中文枚举会得到"英文说中景、焦段按特写算"的自相矛盾规格。
func TestResolveShotSize(t *testing.T) {
	spec := &promspec.ShotSpec{ID: "sh007", ShotSize: "特写"}
	spec.ShotSizeEn = promspec.ShotSizeEn("特写")
	spec.Lens = promspec.LensFor("特写", 0)

	// 留空 / 相同：不改
	before := spec.Lens
	if err := resolveShotSize(spec, ""); err != nil || spec.ShotSize != "特写" {
		t.Fatalf("留空不应改动: %v %q", err, spec.ShotSize)
	}
	if err := resolveShotSize(spec, "特写"); err != nil || spec.Lens != before {
		t.Fatalf("相同景别不应改动: %v", err)
	}
	// 非法枚举：拒绝
	if err := resolveShotSize(spec, "超特写"); err == nil {
		t.Fatal("枚举外的景别应被拒绝")
	}
	// 合法修正：中文枚举、英文描述、焦段三者同步更新
	if err := resolveShotSize(spec, "中景"); err != nil {
		t.Fatal(err)
	}
	if spec.ShotSize != "中景" || spec.ShotSizeEn != promspec.ShotSizeEn("中景") {
		t.Fatalf("景别与英文描述未同步: %q / %q", spec.ShotSize, spec.ShotSizeEn)
	}
	if spec.Lens != promspec.LensFor("中景", 0) {
		t.Fatalf("焦段未按新景别重算: %+v", spec.Lens)
	}
}
