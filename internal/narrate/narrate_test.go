package narrate

import (
	"testing"

	"aistudio/internal/promspec"
)

// TestPitchFor 同音色兜底的音高区分：只有"角色与旁白共用同一 TTS 音色"时
// 变调才启用（音色表能区分角色时调用方直接跳过变调，保持原声最自然）。
func TestPitchFor(t *testing.T) {
	kit := &promspec.Kit{
		Characters: []promspec.CharacterSpec{{Name: "林深"}},
	}
	cases := []struct {
		speaker string
		want    float64
	}{
		{"narrator", 1.0}, // 旁白保持原声
		{"旁白", 1.0},       // 中文旁白称谓同上
		{"林深", 1.05},      // 角色：略抬形成层次
		{"路人", 1.03},      // 未登记角色：默认微抬
		{"系统", 0.92},      // 非人声角色：压低做出机械感（刻意保留）
		{"广播", 0.92},
	}
	for _, c := range cases {
		if got := pitchFor(kit, c.speaker); got != c.want {
			t.Errorf("pitchFor(%q) = %.2f, 期望 %.2f", c.speaker, got, c.want)
		}
	}
}
