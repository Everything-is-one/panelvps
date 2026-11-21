package server

import "testing"

func TestFirstLine(t *testing.T) {
	if got := firstLine("第一行\n第二行", "fallback"); got != "第一行" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine("  \n", "fallback"); got != "fallback" {
		t.Fatalf("got %q", got)
	}
}
