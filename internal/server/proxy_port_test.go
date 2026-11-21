package server

import (
	"testing"

	"github.com/Everything-is-one/panelvps/internal/store"
)

// 设置里的公网地址允许连端口一起填（NAT 机器上公网端口 ≠ 监听端口）。
// 早先直接把这个字符串塞给 joinHostPort，会拼成
// "204.197.163.238:30986:47736" 这种坏链接。
func TestSplitPublicHost(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"203.0.113.9", "203.0.113.9", 0},
		{"203.0.113.9:30986", "203.0.113.9", 30986},
		{"http://203.0.113.9:30986/", "203.0.113.9", 30986},
		{" https://node.example.com:443 ", "node.example.com", 443},
		{"node.example.com", "node.example.com", 0},
		{"[2001:db8::1]:30986", "2001:db8::1", 30986},
		{"2001:db8::1", "2001:db8::1", 0},
		// 端口越界时当没写，链接回落到入站端口，而不是生成 :0 或 :99999
		{"203.0.113.9:99999", "203.0.113.9", 0},
		{"203.0.113.9:0", "203.0.113.9", 0},
	}
	for _, c := range cases {
		host, port := splitPublicHost(c.in)
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("splitPublicHost(%q) = (%q, %d)，期望 (%q, %d)",
				c.in, host, port, c.wantHost, c.wantPort)
		}
	}
}

// 端口优先级：入站对外端口 > 全局端口 > 监听端口。
func TestLinkPortPrecedence(t *testing.T) {
	in := store.Inbound{Port: 47736}
	if got := linkPort(in, 0); got != 0 {
		t.Fatalf("都没填时应返回 0（由 BuildLink 回落到监听端口），得到 %d", got)
	}
	if got := linkPort(in, 30986); got != 30986 {
		t.Fatalf("只有全局端口时应返回 30986，得到 %d", got)
	}
	in.PublicPort = 40000
	if got := linkPort(in, 30986); got != 40000 {
		t.Fatalf("入站对外端口应覆盖全局端口，得到 %d", got)
	}
}
