package server

import "testing"

func TestAccessURLs(t *testing.T) {
	got := accessURLs([]string{"1.2.3.4", "1.2.3.4", "example.com"}, 20873, true)
	want := []string{"http://1.2.3.4:20873", "http://example.com:20873"}
	if len(got) != len(want) {
		t.Fatalf("数量不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符: got %q want %q", i, got[i], want[i])
		}
	}

	loop := accessURLs([]string{"1.2.3.4"}, 20873, false)
	if len(loop) != 1 || loop[0] != "http://127.0.0.1:20873" {
		t.Fatalf("仅本机时不应展示公网地址: %v", loop)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("第一行\n第二行", "fallback"); got != "第一行" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine("  \n", "fallback"); got != "fallback" {
		t.Fatalf("got %q", got)
	}
}
