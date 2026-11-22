package softstore

import (
	"strings"
	"testing"
)

func TestLoadBuiltinManifests(t *testing.T) {
	s, err := Load()
	if err != nil {
		t.Fatalf("内置清单加载失败: %v", err)
	}
	list := s.List()
	if len(list) < 8 {
		t.Fatalf("内置清单过少: %d", len(list))
	}
	for _, m := range list {
		if m.Slug == "" || m.Name == "" || m.Category == "" {
			t.Errorf("清单字段不完整: %+v", m)
		}
		if len(m.Plan.Install) == 0 || len(m.Plan.Uninstall) == 0 {
			t.Errorf("%s 缺少安装或卸载计划", m.Slug)
		}
		for _, a := range m.Plan.Install {
			if err := a.validate(); err != nil {
				t.Errorf("%s 的安装步骤非法: %v", m.Slug, err)
			}
		}
	}
}

func TestGetUnknown(t *testing.T) {
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("not-exists"); err != ErrNotFound {
		t.Errorf("未知 slug 应返回 ErrNotFound，实际 %v", err)
	}
}

func TestActionRequiresExactlyOneKind(t *testing.T) {
	both := Action{
		Apt:   &AptAction{Names: []string{"nginx"}},
		Check: &CheckAction{Cmd: []string{"true"}},
	}
	if err := both.validate(); err == nil {
		t.Error("同时声明两种动作应被拒绝")
	}
	none := Action{}
	if err := none.validate(); err == nil {
		t.Error("空动作应被拒绝")
	}
}

func TestActionRejectsInsecureDownload(t *testing.T) {
	cases := []*DownloadAction{
		{URL: "http://example.com/x", Dest: "/tmp/x"},
		{URL: "https://127.0.0.1/x", Dest: "/tmp/x"},
		{URL: "https://localhost/x", Dest: "/tmp/x"},
		{URL: "https://example.com/x", Dest: "relative/x"},
		{URL: "https://example.com/x", Dest: "/tmp/x", SHA256: "abc"},
	}
	for _, d := range cases {
		a := Action{Download: d}
		if err := a.validate(); err == nil {
			t.Errorf("不安全的下载动作被放行: %+v", d)
		}
	}
}

func TestAptRejectsShellMetachars(t *testing.T) {
	a := Action{Apt: &AptAction{Names: []string{"nginx; rm -rf /"}}}
	if err := a.validate(); err == nil {
		t.Fatal("包名中的 shell 元字符应被拒绝")
	}
}

// apt-get 的语法是 apt-get [选项] 命令 [包...]，第一个非选项参数必须是
// 命令。漏掉 install 时 apt-get 会把包名当命令，报
// "E: Invalid operation nginx" 并以 100 退出 —— 全部 apt 类清单都装不上。
//
// 这里按 apt-get 的解析规则校验，而不是简单比较字符串：以后再加参数
// （-o 之类）也不会误判。
func TestAptArgsHasSubcommand(t *testing.T) {
	known := map[string]bool{
		"install": true, "purge": true, "remove": true, "update": true,
		"upgrade": true, "autoremove": true, "clean": true,
	}

	cases := []struct {
		name string
		a    AptAction
		want string // 期望的子命令
	}{
		{"安装", AptAction{Names: []string{"nginx"}}, "install"},
		{"安装多个", AptAction{Names: []string{"nginx", "curl"}}, "install"},
		{"卸载", AptAction{Names: []string{"nginx"}, Purge: true}, "purge"},
	}

	for _, c := range cases {
		args := aptArgs(c.a)
		var cmd string
		for _, s := range args {
			if strings.HasPrefix(s, "-") {
				continue
			}
			cmd = s
			break
		}
		if cmd == "" {
			t.Fatalf("%s: 参数里没有任何子命令: %v", c.name, args)
		}
		if !known[cmd] {
			t.Errorf("%s: 首个非选项参数 %q 不是 apt-get 子命令，apt-get 会把它当成包名并报 Invalid operation。参数: %v",
				c.name, cmd, args)
		}
		if cmd != c.want {
			t.Errorf("%s: 子命令 = %q，期望 %q（参数 %v）", c.name, cmd, c.want, args)
		}
		// 包名必须都在，且不能被子命令吞掉
		for _, p := range c.a.Names {
			found := false
			for _, s := range args {
				if s == p {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: 包名 %q 不在参数里: %v", c.name, p, args)
			}
		}
	}
}

// 回归：曾经 aptArgs 漏了 install，导致首个非选项参数就是包名。
func TestAptArgsRegressionInstallFirst(t *testing.T) {
	args := aptArgs(AptAction{Names: []string{"nginx"}})
	if len(args) == 0 || args[0] != "install" {
		t.Fatalf("aptArgs 的第一个参数应为 install，实际为 %v", args)
	}
}

func TestPlanLines(t *testing.T) {
	s, _ := Load()
	m, err := s.Get("nginx")
	if err != nil {
		t.Fatal(err)
	}
	lines := PlanLines(m, "install")
	if len(lines) == 0 {
		t.Fatal("安装计划不应为空")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"apt install nginx", "服务 nginx"} {
		if !strings.Contains(joined, want) {
			t.Errorf("计划文本缺少 %q：\n%s", want, joined)
		}
	}
	un := PlanLines(m, "uninstall")
	if len(un) == 0 {
		t.Fatal("卸载计划不应为空")
	}
}
