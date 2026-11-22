package manage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sshd 的 Port 是累加的：只加新端口、注释掉旧的，才叫「改端口」，
// 否则会同时监听两个端口，爆破照样打 22。
func TestCommentPortLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sshd_config")
	body := "# Port 22\nPort 22\nPort 2222\nPermitRootLogin yes\n  Port 2200\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commentPortLines(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	out := string(raw)

	if strings.Contains(out, "\nPort ") || strings.HasPrefix(out, "Port ") {
		t.Fatalf("还有生效的 Port 行:\n%s", out)
	}
	for _, want := range []string{"# panelvps: Port 22", "# panelvps: Port 2222", "# panelvps: Port 2200"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少注释行 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "PermitRootLogin yes") {
		t.Error("不该动其他配置项")
	}
	// 权限必须保持 600（Windows 没有 Unix 权限位，跳过这项）
	if runtime.GOOS != "windows" {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("权限被改成 %o，应为 600", st.Mode().Perm())
		}
	}
}

func TestPrependPort(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(p, []byte("\n\nInclude /etc/ssh/sshd_config.d/*.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prependPort(p, 2222); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	out := string(raw)
	if !strings.HasPrefix(out, "# panelvps:") {
		t.Fatalf("新端口没写在最前面:\n%s", out)
	}
	if !strings.Contains(out, "Port 2222\n") {
		t.Fatalf("缺少 Port 行:\n%s", out)
	}
	// Include 必须在 Port 之后：sshd 取第一个 Port，写在 Include 后会被 drop-in 盖掉
	if strings.Index(out, "Port 2222") > strings.Index(out, "Include") {
		t.Fatalf("Port 落在了 Include 后面:\n%s", out)
	}
}

// 同一行里 Port 出现在别的指令值里（如 AllowUsers 里的用户名）不能被误伤。
func TestCommentPortLinesIgnoresOtherDirectives(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sshd_config")
	body := "AllowUsers port\nSubsystem sftp /usr/lib/openssh/sftp-server\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commentPortLines(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != body {
		t.Fatalf("不该改动无关配置:\n%s", raw)
	}
}
