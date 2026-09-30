package narrate

import "testing"

// 台本 tts_notes 里的语速曾经只写进 JSON 从不被消费（"语速 0.88x"形同虚设）。
func TestParseTTSNotesSpeed(t *testing.T) {
	cases := []struct {
		notes string
		want  float64
	}{
		{"narrator 用低音男声，林用角色音色；语速 0.95x；48kHz，输出 WAV", 0.95},
		{"语速为0.88x，句尾下收", 0.88},
		{"整体语速 1.1 倍", 1.1},
		{"语速稍慢", 0},
		{"音色列表与采样率", 0},
		{"", 0},
		{"语速 0.3x（超出范围，忽略）", 0},
	}
	for _, c := range cases {
		if got := parseTTSNotesSpeed(c.notes); got != c.want {
			t.Errorf("parseTTSNotesSpeed(%q)=%v want %v", c.notes, got, c.want)
		}
	}
}
