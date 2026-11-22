package server

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
)

func TestIsLoopbackBind(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true,
		"::1":       true,
		"[::1]":     true,
		"localhost": true,
		"0.0.0.0":   false,
		"":          false, // 空代表所有地址
		"10.0.0.5":  false,
		"::":        false,
		"1.2.3.4":   false,
	}
	for addr, want := range cases {
		if got := isLoopbackBind(addr); got != want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestRelayProtos(t *testing.T) {
	cases := map[string][]string{
		"tcp":  {"tcp"},
		"udp":  {"udp"},
		"both": {"tcp", "udp"},
		"":     {"tcp"}, // 缺省按 tcp
	}
	for nw, want := range cases {
		got := relayProtos(nw)
		if len(got) != len(want) {
			t.Fatalf("relayProtos(%q) = %v, want %v", nw, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("relayProtos(%q) = %v, want %v", nw, got, want)
			}
		}
	}
}

// 切白名单时，只有「对外监听且还没放行」的端口才该新增规则。
func TestPlanWhitelistAdds(t *testing.T) {
	existing := []store.FirewallRule{
		// 已经放行过 80，不该重复加
		{Action: "accept", Proto: "tcp", Port: "80", Enabled: true},
		// 停用的规则不算已覆盖
		{Action: "accept", Proto: "tcp", Port: "8080", Enabled: false},
	}
	ports := []sysinfo.Port{
		{Proto: "tcp", Port: 80, Expose: true},    // 已放行 → 跳过
		{Proto: "tcp", Port: 443, Expose: true},   // 新增
		{Proto: "tcp", Port: 8080, Expose: true},  // 规则被停用 → 新增
		{Proto: "tcp", Port: 8899, Expose: false}, // 只听回环 → 跳过
		{Proto: "udp", Port: 443, Expose: true},   // tcp/udp 各一条
		{Proto: "tcp6", Port: 443, Expose: true},  // 与 tcp/443 重复 → 跳过
		{Proto: "udp6", Port: 5300, Expose: true}, // 归一到 udp
		{Proto: "tcp", Port: 0, Expose: true},     // 非法端口 → 跳过
	}

	adds := planWhitelistAdds(existing, ports)
	got := map[string]bool{}
	for _, a := range adds {
		if a.Action != "accept" || !a.Enabled || a.Source != "" {
			t.Errorf("新增规则字段不正确: %+v", a)
		}
		got[a.Proto+"/"+a.Port] = true
	}

	want := []string{"tcp/443", "tcp/8080", "udp/443", "udp/5300"}
	if len(got) != len(want) {
		t.Fatalf("新增 %d 条，期望 %d 条：%v", len(got), len(want), got)
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("缺少应放行的端口 %s，实际 %v", k, got)
		}
	}
}

// any 协议的规则等于把 tcp 与 udp 一起放行了，不该再补。
func TestPlanWhitelistAddsAnyProtoCoversBoth(t *testing.T) {
	existing := []store.FirewallRule{
		{Action: "accept", Proto: "any", Port: "443", Enabled: true},
	}
	ports := []sysinfo.Port{
		{Proto: "tcp", Port: 443, Expose: true},
		{Proto: "udp", Port: 443, Expose: true},
	}
	if adds := planWhitelistAdds(existing, ports); len(adds) != 0 {
		t.Fatalf("any 规则已覆盖，不该新增，实际: %+v", adds)
	}
}

// 切白名单的顺序是「先下发内核，成功后写库」。反过来的话，一旦下发失败，
// 库里会留着那条 chain 尾部 drop —— 面板下次重启自动 apply 时就把 SSH
// 和面板一起锁在门外了。这个用例盯的就是这个顺序。
func TestSwitchModeDoesNotPersistWhenApplyFails(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("需要没有 nft/iptables 后端的环境才能稳定触发下发失败")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := &Server{st: st, cfg: &config.Config{Listen: "127.0.0.1:8899", SSHPorts: []int{22}}}

	if _, err := s.switchFirewallMode(context.Background(), true, true); err == nil {
		t.Fatal("没有防火墙后端时切换应失败")
	}

	rows, err := st.ListFirewallRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("下发失败后不应写库，实际写入了 %d 条规则", len(rows))
	}
}
