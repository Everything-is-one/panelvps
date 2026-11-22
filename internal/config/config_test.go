package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// 改 ssh_ports 必须替换整块。
//
// 这是 0.4.8 那次「升级完面板起不来」的直接原因：yaml.Marshal 把 []int{22}
// 写成两行（ssh_ports: / - 22），PatchScalar 只换了第一行，留下孤儿列表项，
// 整份配置变成非法 YAML。写入当时不报错——进程还在跑、配置在内存里，
// 直到下次重启才炸，日志里只有「yaml: line 24: did not find expected key」。
func TestPatchScalarRewritesListBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	c := Default()
	body, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ssh_ports:\n    - 22") {
		t.Fatalf("前置条件变了，marshal 结果里没有多行列表：\n%s", body)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PatchScalar(path, "ssh_ports", "[2222]"); err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "- 22") {
		t.Errorf("旧列表项没被清掉：\n%s", raw)
	}

	var back Config
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("改写后无法解析: %v\n%s", err, raw)
	}
	if len(back.SSHPorts) != 1 || back.SSHPorts[0] != 2222 {
		t.Fatalf("ssh_ports = %v，期望 [2222]", back.SSHPorts)
	}
}

// 配置文件本身已经坏了时，PatchScalar 必须拒绝写入并原样保留文件。
// 否则「改个设置」会把最后的现场也毁掉。
func TestPatchScalarRefusesBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	broken := "listen: 127.0.0.1:20873\nssh_ports: [2222]\n    - 22\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PatchScalar(path, "terminal_enabled", "true"); err == nil {
		t.Fatal("对已损坏的配置应当拒绝写入")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Fatalf("文件被改动了：\n%s", raw)
	}
}

// 键不存在时追加到末尾，且不破坏已有的嵌套结构。
func TestPatchScalarAppendsMissingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:20873\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PatchScalar(path, "access_path", `"secret"`); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "access_path: \"secret\"") {
		t.Fatalf("没写进去：\n%s", raw)
	}
	var back Config
	if err := yaml.Unmarshal(raw, &back); err != nil {
		t.Fatalf("追加后无法解析: %v", err)
	}
	if back.AccessPath != "secret" {
		t.Fatalf("AccessPath = %q", back.AccessPath)
	}
}

// 解析失败要把出错行和邻居打出来，而不是只给一个行号。
func TestLoadParseErrorShowsContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	body := strings.Join([]string{
		"listen: 127.0.0.1:20873",
		"data_dir: ./data",
		"ssh_ports: [2222]",
		"    - 22",
		"update_repo: Everything-is-one/panelvps",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("非法配置应当报错")
	}
	msg := err.Error()
	for _, want := range []string{"第 3 行附近", "→ ", "ssh_ports: [2222]", "- 22", "孤儿"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错里缺少 %q：\n%s", want, msg)
		}
	}
}

// Check 不创建文件、不改文件，只回答「能不能启动」。
func TestCheckDoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	if err := Check(path); err == nil {
		t.Fatal("文件不存在时 Check 应当报错")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Check 不应创建配置文件")
	}
}
