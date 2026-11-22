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

// 相对路径按「配置文件所在的安装前缀」解析，而不是按进程工作目录。
//
// 这是 0.4.3 修的那个坑：服务的工作目录是 /opt/panelvps，人敲 CLI 时却在
// /root，同一份 ./data 解析成两个目录，于是 reset-password 改的是影子库，
// Web 端永远登不上，而且两边都不报错。
func TestLoadResolvesDataDirAgainstPrefix(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(prefix, "conf", "panel.yaml")
	if err := os.WriteFile(confPath, []byte("listen: 127.0.0.1:20873\ndata_dir: ./data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 换到一个完全无关的工作目录，模拟「root 登录后在 /root 敲命令」。
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	cfg, err := Load(confPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	want := filepath.Join(prefix, "data")
	if cfg.DataDir != want {
		t.Fatalf("data_dir 应按安装前缀解析为 %q，得到 %q", want, cfg.DataDir)
	}
	if wantDB := filepath.Join(want, "panel.db"); cfg.DBPath != wantDB {
		t.Fatalf("db_path 应落在 data_dir 下 %q，得到 %q", wantDB, cfg.DBPath)
	}
}

// 配置文件不在 conf/ 子目录时，基准退成它自己所在的目录。
func TestLoadResolvesDataDirAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "panel.yaml")
	if err := os.WriteFile(confPath, []byte("data_dir: ./data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	cfg, err := Load(confPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if want := filepath.Join(dir, "data"); cfg.DataDir != want {
		t.Fatalf("期望 %q，得到 %q", want, cfg.DataDir)
	}
}

// 绝对路径的 data_dir 一律原样保留——升级不能把别人的数据目录挪走。
func TestLoadKeepsAbsoluteDataDir(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "mydata")
	dir := t.TempDir()
	confPath := filepath.Join(dir, "panel.yaml")
	if err := os.WriteFile(confPath, []byte("data_dir: "+custom+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	cfg, err := Load(confPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.DataDir != custom {
		t.Fatalf("绝对路径被改写: %q", cfg.DataDir)
	}
}

// 历史布局兜底：新位置不存在、老位置（按工作目录）确实有目录时，继续用老位置。
// 没有这条，某些把配置放在 <prefix>/conf 之外的安装会在升级后突然换库。
func TestLoadFallsBackToLegacyDataDir(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(prefix, "conf", "panel.yaml")
	if err := os.WriteFile(confPath, []byte("data_dir: ./data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 老位置：工作目录下的 data/ 确实存在（新位置 <prefix>/data 不存在）。
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	cfg, err := Load(confPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if want := filepath.Join(work, "data"); cfg.DataDir != want {
		t.Fatalf("应回落到老位置 %q，得到 %q", want, cfg.DataDir)
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
