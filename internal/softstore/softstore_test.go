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
