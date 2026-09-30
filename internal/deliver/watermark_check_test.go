package deliver

import (
	"strings"
	"testing"

	"aistudio/internal/vision"
)

// 水印校验的三态判定。
//
// 这个测试存在的原因：水印是"每一帧都在"的交付硬伤，但它不会让任何技术指标变差——
// 画面清晰度、时长、响度、字幕全部照常通过。也就是说除这条检查外，
// 整条交付链路上没有任何东西会发现成片带着角标。
func TestWatermarkCheckThreeStates(t *testing.T) {
	checked := func(w vision.Watermark) *vision.Report {
		return &vision.Report{Watermark: w}
	}

	cases := []struct {
		name       string
		report     *vision.Report
		wantOK     bool
		wantSubstr string
	}{
		{
			name:       "未启用视觉复核：必须是未检查而不是通过",
			report:     nil,
			wantOK:     false,
			wantSubstr: "未检查",
		},
		{
			name:       "复核跑了但模型没按 schema 答 overlay：同样是未检查",
			report:     checked(vision.Watermark{Checked: false}),
			wantOK:     false,
			wantSubstr: "未检查",
		},
		{
			name: "确认无水印",
			report: checked(vision.Watermark{
				Checked: true, Suspected: false, Note: "未检出水印/叠字",
			}),
			wantOK:     true,
			wantSubstr: "未检出",
		},
		{
			name: "疑似水印：必须报出并给出镜头与内容",
			report: checked(vision.Watermark{
				Checked: true, Suspected: true,
				Shots: []vision.WatermarkHit{{ShotID: "sh002", Text: "AI生成", Location: "右下角", Confidence: 0.95}},
				Note:  "建议抽帧确认",
			}),
			wantOK:     false,
			wantSubstr: "sh002(AI生成)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, detail := watermarkCheck(c.report)
			if ok != c.wantOK {
				t.Errorf("ok = %v，期望 %v（detail: %s）", ok, c.wantOK, detail)
			}
			if !strings.Contains(detail, c.wantSubstr) {
				t.Errorf("说明中应包含 %q，实际: %s", c.wantSubstr, detail)
			}
		})
	}
}

// 水印命中时不能把"未检查"和"疑似"混为一谈——前者要人开复核，后者要人重渲。
func TestWatermarkCheckDistinguishesUncheckedFromSuspected(t *testing.T) {
	_, unchecked := watermarkCheck(nil)
	_, suspected := watermarkCheck(&vision.Report{Watermark: vision.Watermark{
		Checked: true, Suspected: true,
		Shots: []vision.WatermarkHit{{ShotID: "sh001", Confidence: 0.9}},
	}})
	if unchecked == suspected {
		t.Fatal("未检查与疑似必须给出不同说明，否则处理动作无从判断")
	}
	if !strings.Contains(suspected, "重渲") {
		t.Errorf("疑似水印应给出可执行的后续动作（重渲），实际: %s", suspected)
	}
}
