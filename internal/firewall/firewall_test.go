package firewall

import (
	"strings"
	"testing"
)

func TestValidateRule(t *testing.T) {
	good := []Rule{
		{Action: "accept", Proto: "tcp", Port: "80", Enabled: true},
		{Action: "drop", Proto: "any", Port: "", Source: "203.0.113.0/24", Enabled: true},
		{Action: "accept", Proto: "udp", Port: "8000-8100", Source: "2001:db8::1", Enabled: true},
	}
	for _, r := range good {
		if err := ValidateRule(r); err != nil {
			t.Errorf("合法规则被拒: %+v err=%v", r, err)
		}
	}

	bad := []Rule{
		{Action: "allow", Proto: "tcp", Port: "80"},
		{Action: "accept", Proto: "icmp", Port: "80"},
		{Action: "accept", Proto: "tcp", Port: "70000"},
		{Action: "accept", Proto: "tcp", Port: "900-800"},
		{Action: "accept", Proto: "tcp", Port: "80", Source: "not-an-ip"},
		{Action: "accept", Proto: "tcp", Port: "80", Source: "10.0.0.0/33"},
		{Action: "accept", Proto: "tcp", Port: "80", Comment: strings.Repeat("x", 200)},
	}
	for _, r := range bad {
		if err := ValidateRule(r); err == nil {
			t.Errorf("非法规则被放行: %+v", r)
		}
	}
}

func TestRenderPutsSSHBeforeUserRules(t *testing.T) {
	script, err := Render(ApplyOptions{
		Rules: []Rule{
			{Action: "drop", Proto: "tcp", Port: "*", Source: "198.51.100.7", Enabled: true},
			{Action: "accept", Proto: "tcp", Port: "443", Enabled: true},
		},
		SSHPorts:  []int{22, 2222},
		PanelPort: 20873, PanelLoopback: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	sshPos := strings.Index(script, "tcp dport 22 accept")
	panelPos := strings.Index(script, "tcp dport 20873 accept")
	dropPos := strings.Index(script, "ip saddr 198.51.100.7")
	if sshPos < 0 || panelPos < 0 {
		t.Fatalf("脚本缺少安全放行:\n%s", script)
	}
	if !(sshPos < dropPos && panelPos < dropPos) {
		t.Errorf("SSH/面板端口必须排在用户 drop 规则之前:\n%s", script)
	}
}

func TestRenderWhitelistMode(t *testing.T) {
	script, err := Render(ApplyOptions{
		Rules: []Rule{
			{Action: "accept", Proto: "tcp", Port: "443", Enabled: true},
			{Action: "drop", Proto: "any", Port: "*", Enabled: true},
		},
		SSHPorts: []int{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "counter drop") {
		t.Errorf("白名单模式缺少兜底 drop:\n%s", script)
	}

	script2, _ := Render(ApplyOptions{
		Rules:    []Rule{{Action: "drop", Proto: "tcp", Port: "23", Enabled: true}},
		SSHPorts: []int{22},
	})
	if strings.Contains(script2, "counter drop") {
		t.Errorf("黑名单模式不应出现兜底 drop:\n%s", script2)
	}
}

func TestRenderAnyProtoWithPortUsesL4Match(t *testing.T) {
	script, err := Render(ApplyOptions{
		Rules:    []Rule{{Action: "accept", Proto: "any", Port: "8080", Enabled: true}},
		SSHPorts: []int{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "meta l4proto { tcp, udp } th dport 8080") {
		t.Errorf("未限定协议时必须给出 l4proto 上下文:\n%s", script)
	}
}

func TestRenderSkipsDisabledRules(t *testing.T) {
	script, _ := Render(ApplyOptions{
		Rules:    []Rule{{Action: "accept", Proto: "tcp", Port: "9999", Enabled: false}},
		SSHPorts: []int{22},
	})
	if strings.Contains(script, "9999") {
		t.Errorf("停用规则不应出现在脚本中:\n%s", script)
	}
}

func TestShellSafeStripsCommandSubstitution(t *testing.T) {
	got := shellSafe("$(reboot) `id` ; rm -rf /")
	for _, bad := range []string{"$", "`", ";", "/"} {
		if strings.Contains(got, bad) {
			t.Errorf("危险字符未被清除: %q", got)
		}
	}
}

func TestDomainLooksValid(t *testing.T) {
	for _, d := range []string{"example.com", "a-b.example.co.uk", "xn--fiq228c.cn"} {
		if !DomainLooksValid(d) {
			t.Errorf("合法域名被拒: %s", d)
		}
	}
	for _, d := range []string{"", "-bad.com", "bad-.com", "a..b", "a b.com", strings.Repeat("a", 64) + ".com"} {
		if DomainLooksValid(d) {
			t.Errorf("非法域名被放行: %q", d)
		}
	}
}
