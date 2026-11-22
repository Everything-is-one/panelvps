package config

import "testing"

func TestNormalizeAccessPath(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"abc":         "abc",
		"/abc/":       "abc",
		"  /a-b_c.d/": "a-b_c.d",
		"///":         "",
	}
	for in, want := range cases {
		if got := NormalizeAccessPath(in); got != want {
			t.Errorf("NormalizeAccessPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateAccessPath(t *testing.T) {
	ok := []string{"", "abc", "a1b2c3d4", "my-panel_1", "a.b~c"}
	for _, v := range ok {
		if err := ValidateAccessPath(v); err != nil {
			t.Errorf("ValidateAccessPath(%q) 不应报错: %v", v, err)
		}
	}
	bad := []string{"a", "a b", "a/b", "中文", "a?b", "abc\\def", "x:y"}
	for _, v := range bad {
		if err := ValidateAccessPath(v); err == nil {
			t.Errorf("ValidateAccessPath(%q) 应该报错", v)
		}
	}
}

// 访问后缀会出现在 URL 里，长度也必须被限制，
// 否则一个 500 字符的后缀照样能写进配置。
func TestValidateAccessPathLength(t *testing.T) {
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateAccessPath(string(long)); err == nil {
		t.Error("65 位后缀应该被拒绝")
	}
}
