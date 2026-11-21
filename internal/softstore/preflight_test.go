package softstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlanTargets(t *testing.T) {
	plan := []Action{
		{Download: &DownloadAction{URL: "x", Dest: "/tmp/panelvps-xray.zip"}},
		{Extract: &ExtractAction{Src: "/tmp/a.zip", Dest: "/usr/local/bin"}},
		{Extract: &ExtractAction{Src: "/tmp/a.zip", Dest: "/usr/local/share/xray"}},
		{Path: &PathAction{Path: "/usr/local/share/xray", State: "directory"}},
		{Path: &PathAction{Path: "/tmp/panelvps-xray.zip", State: "absent"}},
		{WriteFile: &WriteFileAction{Path: "/etc/systemd/system/x.service", From: "x"}},
	}
	got := planTargets(plan)
	want := []string{"/tmp", "/usr/local/bin", "/usr/local/share/xray", "/etc/systemd/system"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planTargets = %v, want %v", got, want)
	}
}

func TestPreflightCreatesMissingDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "apps", "xray")
	plan := []Action{{Extract: &ExtractAction{Src: "x", Dest: target}}}

	var logs []string
	if err := preflight(plan, func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatalf("preflight 失败: %v", err)
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() {
		t.Fatalf("目录没有被创建: %v", err)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "检查安装目录") {
		t.Fatalf("缺少检查日志: %v", logs)
	}
}

func TestPreflightReportsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上没有等价的只读目录语义")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，用例无意义")
	}
	base := t.TempDir()
	ro := filepath.Join(base, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	plan := []Action{{Extract: &ExtractAction{Src: "x", Dest: filepath.Join(ro, "xray")}}}
	err := preflight(plan, func(string) {})
	if err == nil {
		t.Fatal("只读目录应当失败")
	}
	if !strings.Contains(err.Error(), "不可写") || !strings.Contains(err.Error(), "systemctl restart panelvps") {
		t.Fatalf("错误信息缺少可操作提示: %v", err)
	}
}
