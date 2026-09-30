package render

import (
	"strings"
	"testing"

	"aistudio/internal/config"
)

// 负面词内嵌：官方 API 没有 negative_prompt 参数，不内嵌就等于白写。
func TestWithNegatives(t *testing.T) {
	got := withNegatives("a girl in a field", "extra fingers, deformed hands")
	if !strings.HasSuffix(got, "avoid: extra fingers, deformed hands") {
		t.Fatalf("负面词未内嵌: %q", got)
	}
	if got == withNegatives(got, "more") {
		// 装不下时应保主提示词（幂等且不超限）
	}
	huge := strings.Repeat("字", 995)
	if out := withNegatives(huge, "bad stuff"); strings.Contains(out, "avoid:") {
		t.Fatalf("超限时应放弃内嵌负面词")
	}
}

// 换种重渲的提示词变体：只换种子对 cogvideox 无效，必须换提示词。
func TestVariantFor(t *testing.T) {
	base := "a girl in a field"
	if v := variantFor(base, 0, 990); v != base {
		t.Fatalf("round 0 不应变体")
	}
	v1 := variantFor(base, 1, 990)
	v2 := variantFor(base, 2, 990)
	if v1 == base || v2 == base || v1 == v2 {
		t.Fatalf("round 1/2 应产生不同变体: %q / %q", v1, v2)
	}
	if out := variantFor(strings.Repeat("x", 505), 1, 500); !strings.HasPrefix(out, "xxxxx") || out != strings.Repeat("x", 505) {
		t.Fatalf("超限应原样返回")
	}
}

// 10s 档位升级。
func TestVideoDurationFor(t *testing.T) {
	cfg := config.Default()
	if got := videoDurationFor(5.0, cfg); got != 0 {
		t.Fatalf("5s 镜头不应显式传 duration")
	}
	if got := videoDurationFor(8.0, cfg); got != 10 {
		t.Fatalf("8s 镜头应请求 10s 档")
	}
	cfg.Video.AllowLongTakes = false
	if got := videoDurationFor(8.0, cfg); got != 0 {
		t.Fatalf("关闭 allow_long_takes 后不应升级档位")
	}
}

func TestAspectOf(t *testing.T) {
	cases := map[string]string{
		"1920x1080": "16:9", "1080x1920": "9:16", "1024x1024": "1:1", "bogus": "16:9",
	}
	for in, want := range cases {
		if got := aspectOf(in); got != want {
			t.Errorf("aspectOf(%q)=%q want %q", in, got, want)
		}
	}
}
