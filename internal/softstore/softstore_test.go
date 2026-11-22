package softstore

import (
	"errors"
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

// 装 Nginx 那次失败暴露的洞：preflight 只探 download / path / write_file
// 的落点，纯 apt 清单一个目标都没有，于是「系统目录只读」在预检阶段
// 完全没被看见，一路走到 dpkg unpack 才炸。
func TestPlanTargetsCoversAptSystemDirs(t *testing.T) {
	plan := []Action{{Apt: &AptAction{Names: []string{"nginx"}}}}
	got := planTargets(plan)
	want := map[string]bool{"/etc": false, "/usr": false, "/var": false}
	for _, p := range got {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for dir, found := range want {
		if !found {
			t.Errorf("apt 计划没有探 %s，装包撞只读沙箱时预检会放行：%v", dir, got)
		}
	}
}

// 预检的日志措辞：apt 的目标是三个系统根目录，说成「安装目录」会误导。
func TestPreflightLabel(t *testing.T) {
	aptPlan := []Action{{Apt: &AptAction{Names: []string{"nginx"}}}}
	if got := preflightLabel(aptPlan); !strings.Contains(got, "系统目录") {
		t.Errorf("apt 计划应标注为系统目录，得到 %q", got)
	}
	dlPlan := []Action{{Download: &DownloadAction{URL: "https://example.com/a.tgz", Dest: "/usr/local/bin/a"}}}
	if got := preflightLabel(dlPlan); got != "安装目录" {
		t.Errorf("下载计划应标注为安装目录，得到 %q", got)
	}
}

// systemctl 对「单元根本没加载」的几种措辞都要认出来。
func TestUnitNotLoaded(t *testing.T) {
	yes := []string{
		"Failed to stop nginx.service: Unit nginx.service not loaded.",
		"Failed to disable unit: Unit file nginx.service does not exist.",
		"Unit nginx.service could not be found.",
	}
	for _, s := range yes {
		if !unitNotLoaded(s) {
			t.Errorf("应判定为未加载：%q", s)
		}
	}
	no := []string{
		"Job for nginx.service failed because the control process exited with error code.",
		"",
	}
	for _, s := range no {
		if unitNotLoaded(s) {
			t.Errorf("不应判定为未加载：%q", s)
		}
	}
}

// 只读沙箱的错误必须自带修法，否则用户只看到 "exit status 100"。
func TestAptErrorReadOnlyIsActionable(t *testing.T) {
	out := "dpkg: error processing archive ...\n" +
		"unable to clean up mess surrounding './etc/default/nginx' before installing another version: " +
		"Read-only file system\nE: Sub-process /usr/bin/dpkg returned an error code (1)"
	err := aptError(out, errors.New("exit status 100"))
	if err == nil {
		t.Fatal("应返回错误")
	}
	msg := err.Error()
	for _, want := range []string{"服务单元", "/etc", "/usr", "/var", "apt-get -f install"} {
		if !strings.Contains(msg, want) {
			t.Errorf("提示缺少 %q：%s", want, msg)
		}
	}
}

// 未命中任何已知模式时，不能把原文吞掉。
func TestAptErrorKeepsUnknownOutput(t *testing.T) {
	err := aptError("E: something odd", errors.New("exit status 100"))
	if err == nil || !strings.Contains(err.Error(), "exit status 100") {
		t.Fatalf("未知错误应保留原始错误：%v", err)
	}
}

// 校验必须排在启动之前：先起服务再 nginx -t，80 已被自己占住。
func TestNginxCheckBeforeStart(t *testing.T) {
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Get("nginx")
	if err != nil {
		t.Fatal(err)
	}
	checkAt, startAt := -1, -1
	for i, a := range m.Plan.Install {
		if a.Check != nil && checkAt < 0 {
			checkAt = i
		}
		if a.Service != nil && a.Service.Start && startAt < 0 {
			startAt = i
		}
	}
	if checkAt < 0 || startAt < 0 {
		t.Fatalf("nginx 清单缺少 check 或 start：check=%d start=%d", checkAt, startAt)
	}
	if checkAt > startAt {
		t.Errorf("nginx -t 在启动之后执行（check=%d start=%d），会撞 EADDRINUSE", checkAt, startAt)
	}
}
