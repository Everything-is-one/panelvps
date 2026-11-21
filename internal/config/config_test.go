package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 0.3.0 的 SetAccess 会把整份配置写回文件，backup_dir 从此固化成
// <曾经的 data_dir>/backups。用户把 data_dir 改回 ./data 之后，
// 备份仍指向 /root/data/backups，在服务沙箱里是只读的。
func TestValidateFallsBackLegacyBackupDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到用户目录")
	}
	c := Default()
	c.DataDir = "./data"
	c.BackupDir = filepath.Join(home, "data", "backups")

	if err := c.Validate(); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	want := filepath.Join(c.DataDir, "backups")
	if c.BackupDir != want {
		t.Fatalf("遗留的 backup_dir 应回落，得到 %q，期望 %q", c.BackupDir, want)
	}
}

// data_dir 本来就是 $HOME/data 时，那个绝对路径是对的，不能动。
func TestValidateKeepsBackupDirUnderHomeData(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到用户目录")
	}
	c := Default()
	c.DataDir = filepath.Join(home, "data")
	c.BackupDir = filepath.Join(home, "data", "backups")

	if err := c.Validate(); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if c.BackupDir != filepath.Join(home, "data", "backups") {
		t.Fatalf("合法配置被改写: %q", c.BackupDir)
	}
}

// 自定义到别处的备份目录要保留。
func TestValidateKeepsCustomBackupDir(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "backup")
	c := Default()
	c.DataDir = "./data"
	c.BackupDir = custom

	if err := c.Validate(); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if c.BackupDir != custom {
		t.Fatalf("自定义备份目录被改写: %q", c.BackupDir)
	}
}

// PatchScalar 只改一行，其余内容（注释、顺序、相对路径）必须原样保留。
func TestPatchScalar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	origin := "# 顶部注释\nlisten: 127.0.0.1:20873\ndata_dir: ./data\nterminal_enabled: false\nxray:\n  listen: keep-me\n"
	if err := os.WriteFile(path, []byte(origin), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PatchScalar(path, "terminal_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"# 顶部注释",
		"data_dir: ./data",
		"terminal_enabled: true",
		"  listen: keep-me",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "terminal_enabled: false") {
		t.Errorf("旧值没有被替换：\n%s", got)
	}
}
