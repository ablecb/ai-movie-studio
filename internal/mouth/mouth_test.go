package mouth

import "testing"

// Valid 的置信度门槛曾只有 0.15：12 帧里 2 帧误判（16.7%）也能过线，
// 表情变化造成的假窗会把声音锚到错误位置。Speaking≥3 叠加后，
// "几乎没人说话"的镜头回落普通入点而不是硬对齐。
func TestWindowValid(t *testing.T) {
	good := &Window{Start: 1.0, End: 2.0, Confidence: 0.5, Frames: 12, Speaking: 6}
	if !good.Valid() {
		t.Error("半数帧说话的正常窗口应有效")
	}
	fluke := &Window{Start: 1.0, End: 2.0, Confidence: 0.167, Frames: 12, Speaking: 2}
	if fluke.Valid() {
		t.Error("12 帧里仅 2 帧说话（疑似表情误判）不应算有效窗口")
	}
	tooShort := &Window{Start: 1.0, End: 1.2, Confidence: 0.9, Frames: 12, Speaking: 10}
	if tooShort.Valid() {
		t.Error("窗口长度 <0.4s 不应有效")
	}
	var nilWin *Window
	if nilWin.Valid() {
		t.Error("nil 窗口应无效")
	}
}
