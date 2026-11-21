package selfupdate

import (
	"strings"
	"testing"
)

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.3.5", 1},
		{"0.3.0", "0.3.0", 0},
		{"0.2.9", "0.3.0", -1},
		{"v1.0.0", "0.9.9", 1},
		{"1.2", "1.2.0", 0},
		{"1.10.0", "1.9.9", 1},
		{"0.4.0-rc1", "0.4.0", 0},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetNameFor(t *testing.T) {
	got := AssetNameFor()
	if !strings.HasPrefix(got, "panelvps-") {
		t.Errorf("资产名异常: %s", got)
	}
}
