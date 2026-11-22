// Package config 负责加载与校验 panelvps 的启动配置。
//
// 设计原则：所有路径都可以被显式覆盖，绝不猜测用户意图；
// 配置缺失时用安全默认值，而不是报错退出。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SecureCookie 控制会话 Cookie 是否带 Secure 标记。
const (
	SecureAuto   = "auto"   // 根据请求是否 TLS 自动判断
	SecureAlways = "always" // 强制（走 Nginx 反代时用）
	SecureNever  = "never"  // 强制关闭（仅本机 http 调试）
)

// Config 是面板的全部可配置项。
type Config struct {
	// Listen 监听地址，默认仅本机回环，公网访问需显式改成 0.0.0.0:端口。
	Listen string `yaml:"listen"`
	// DataDir 数据目录，存放 SQLite 与备份。
	DataDir string `yaml:"data_dir"`
	// DBPath SQLite 文件路径。
	DBPath string `yaml:"db_path"`
	// SessionTTL 会话空闲过期时间，形如 "2h"。
	SessionTTL string `yaml:"session_ttl"`
	// SecureCookie auto|always|never。
	SecureCookie string `yaml:"secure_cookie"`
	// LogLevel debug|info|warn|error。
	LogLevel string `yaml:"log_level"`
	// FileRoot 文件管理模块允许访问的根目录。
	FileRoot string `yaml:"file_root"`
	// TerminalEnabled 是否启用 Web 终端（默认关闭，显式开启）。
	TerminalEnabled bool `yaml:"terminal_enabled"`

	// XrayBin Xray 可执行文件路径；留空则自动探测。
	XrayBin string `yaml:"xray_bin"`
	// XrayConfDir Xray 配置目录，默认为 <data_dir>/xray。
	XrayConfDir string `yaml:"xray_conf_dir"`
	// XrayAPIPort 内部统计接口端口，仅监听 127.0.0.1。
	XrayAPIPort int `yaml:"xray_api_port"`
	// PublicHost 分享链接使用的公网地址或域名；留空时从访问来源推断。
	PublicHost string `yaml:"public_host"`

	// SitesRoot 网站根目录的父目录，所有站点必须落在它下面。
	SitesRoot string `yaml:"sites_root"`
	// NginxConfDir 由面板托管的 Nginx 站点配置目录。
	NginxConfDir string `yaml:"nginx_conf_dir"`
	// NginxBin Nginx 可执行文件，用于 -t 校验与 reload。
	NginxBin string `yaml:"nginx_bin"`
	// AcmeWebRoot ACME HTTP-01 校验目录。
	AcmeWebRoot string `yaml:"acme_webroot"`
	// CertEmail 申请证书用的邮箱。
	CertEmail string `yaml:"cert_email"`

	// BackupDir 备份文件目录，默认为 <data_dir>/backups。
	BackupDir string `yaml:"backup_dir"`
	// AlertWebhook 告警推送地址（POST JSON），留空则不推送。
	AlertWebhook string `yaml:"alert_webhook"`
	// SSHGuardEnabled 是否启用 SSH 爆破检测与自动封禁。
	SSHGuardEnabled bool `yaml:"ssh_guard_enabled"`
	// SSHGuardThreshold 窗口内失败次数阈值。
	SSHGuardThreshold int `yaml:"ssh_guard_threshold"`
	// SSHGuardWindow 统计窗口，如 "10m"。
	SSHGuardWindow string `yaml:"ssh_guard_window"`
	// SSHGuardBan 封禁时长，如 "1h"。
	SSHGuardBan string `yaml:"ssh_guard_ban"`
	// SSHGuardPermaThreshold 窗口内失败次数达到该值时改为永久封禁，0 表示关闭。
	//
	// 临时封禁对分布式爆破够用，但对同一个 IP 反复尝试上万次的场景没有意义：
	// 放出来它还在。所以给一条硬线，过了就长期拉黑，由用户手动解封。
	SSHGuardPermaThreshold int `yaml:"ssh_guard_perma_threshold"`
	// AccessPath 面板访问后缀。非空时所有请求必须带 /<access_path> 前缀，
	// 否则一律 404——扫描器连登录页都看不到，减少被撞库与被扫面的机会。
	//
	// 这是「减少暴露面」而不是认证手段：真正的防线仍是密码、限流与 SSL。
	AccessPath string `yaml:"access_path"`
	// GeoIPEnabled 是否为攻击记录解析 IP 归属地（需要出网，失败只降级不报错）。
	GeoIPEnabled bool `yaml:"geoip_enabled"`
	// SSHPorts 允许的 SSH 端口，防火墙模块会强制放行，避免把自己锁在外面。
	SSHPorts []int `yaml:"ssh_ports"`
	// UpdateRepo 自更新的 GitHub 仓库，形如 owner/repo。
	UpdateRepo string `yaml:"update_repo"`
	// TerminalShell Web 终端使用的 shell。
	TerminalShell string `yaml:"terminal_shell"`

	// path 记录配置文件自身位置，便于回写默认配置。
	path string
}

// Default 返回安全默认值。
//
// 注意 listen 默认是 127.0.0.1：这是本项目最重要的一条安全默认值，
// 安装后面板不应该能被公网扫到。
func Default() *Config {
	return &Config{
		Listen:          "127.0.0.1:20873",
		DataDir:         "./data",
		DBPath:          "",
		SessionTTL:      "2h",
		SecureCookie:    SecureAuto,
		LogLevel:        "info",
		FileRoot:        "/",
		TerminalEnabled: false,
		XrayBin:         "",
		XrayAPIPort:     10085,
		PublicHost:      "",

		SitesRoot:         "/var/www",
		NginxConfDir:      "/etc/nginx/conf.d",
		NginxBin:          "nginx",
		AcmeWebRoot:       "/var/www/_acme",
		CertEmail:         "",
		BackupDir:         "",
		AlertWebhook:      "",
		SSHGuardEnabled:   true,
		SSHGuardThreshold: 5,
		SSHGuardWindow:    "10m",
		SSHGuardBan:       "1h",
		SSHGuardPermaThreshold: 10,
		GeoIPEnabled:      true,
		SSHPorts:          []int{22},
		UpdateRepo:        "Everything-is-one/panelvps",
		TerminalShell:     "/bin/bash",
	}
}

// Load 读取配置文件。文件不存在时返回默认值并写出一份样例，
// 这样用户安装完可以直接编辑，而不是面对一个空目录。
func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.path = path

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := cfg.Save(path); err != nil {
			return nil, fmt.Errorf("配置文件不存在且无法创建默认配置: %w", err)
		}
	default:
		return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	// db_path 是相对路径时，基于 data_dir 解析（而不是进程工作目录）。
	// 默认只写文件名，最终落在 <data_dir>/panel.db。
	if c.DBPath == "" {
		c.DBPath = "panel.db"
	}
	if c.SessionTTL == "" {
		c.SessionTTL = "2h"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.SecureCookie == "" {
		c.SecureCookie = SecureAuto
	}
	if c.FileRoot == "" {
		c.FileRoot = "/"
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:20873"
	}
	if c.XrayAPIPort <= 0 || c.XrayAPIPort > 65535 {
		c.XrayAPIPort = 10085
	}
	if c.XrayConfDir == "" {
		c.XrayConfDir = "xray"
	}
	if c.SitesRoot == "" {
		c.SitesRoot = "/var/www"
	}
	if c.NginxConfDir == "" {
		c.NginxConfDir = "/etc/nginx/conf.d"
	}
	if c.NginxBin == "" {
		c.NginxBin = "nginx"
	}
	if c.AcmeWebRoot == "" {
		c.AcmeWebRoot = "/var/www/_acme"
	}
	if c.BackupDir == "" {
		c.BackupDir = "backups"
	}
	if c.SSHGuardThreshold <= 0 {
		c.SSHGuardThreshold = 5
	}
	if c.SSHGuardWindow == "" {
		c.SSHGuardWindow = "10m"
	}
	if c.SSHGuardBan == "" {
		c.SSHGuardBan = "1h"
	}
	if c.SSHGuardPermaThreshold < 0 {
		c.SSHGuardPermaThreshold = 0
	}
	c.AccessPath = NormalizeAccessPath(c.AccessPath)
	if len(c.SSHPorts) == 0 {
		c.SSHPorts = []int{22}
	}
	if c.TerminalShell == "" {
		c.TerminalShell = "/bin/bash"
	}
	if c.UpdateRepo == "" {
		c.UpdateRepo = "Everything-is-one/panelvps"
	}
}

// Validate 做启动前校验，宁可启动失败也不要带病运行。
func (c *Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen 格式应为 host:port，当前为 %q: %w", c.Listen, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen 端口非法: %q", port)
	}
	_ = host

	if _, err := c.SessionTTLDuration(); err != nil {
		return err
	}

	switch c.SecureCookie {
	case SecureAuto, SecureAlways, SecureNever:
	default:
		return fmt.Errorf("secure_cookie 只能是 auto/always/never，当前为 %q", c.SecureCookie)
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level 只能是 debug/info/warn/error，当前为 %q", c.LogLevel)
	}

	if !filepath.IsAbs(c.DataDir) {
		abs, err := filepath.Abs(c.DataDir)
		if err != nil {
			return fmt.Errorf("data_dir 无法解析为绝对路径: %w", err)
		}
		c.DataDir = abs
	}
	if !filepath.IsAbs(c.DBPath) {
		c.DBPath = filepath.Join(c.DataDir, c.DBPath)
	}
	if !filepath.IsAbs(c.XrayConfDir) {
		c.XrayConfDir = filepath.Join(c.DataDir, c.XrayConfDir)
	}
	// 0.3.0 的 SetAccess 曾把 Validate 之后的整份配置写回文件，把
	// backup_dir 从相对值 "backups" 固化成 <旧 data_dir>/backups 的绝对路径。
	// 用户后来把 data_dir 改回 ./data，备份却仍指向旧目录（典型是
	// /root/data/backups），而 /root 在服务沙箱里只读，一备份就报
	// "read-only file system"。这里识别这种残留并纠正回相对值。
	if filepath.IsAbs(c.BackupDir) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			legacy := filepath.Join(home, "data", "backups")
			current := filepath.Join(home, "data")
			if filepath.Clean(c.BackupDir) == legacy &&
				c.DataDir != current &&
				!strings.HasPrefix(c.DataDir, current+string(filepath.Separator)) {
				c.BackupDir = "backups"
			}
		}
	}
	if !filepath.IsAbs(c.BackupDir) {
		c.BackupDir = filepath.Join(c.DataDir, c.BackupDir)
	}
	if !filepath.IsAbs(c.SitesRoot) {
		abs, err := filepath.Abs(c.SitesRoot)
		if err != nil {
			return fmt.Errorf("sites_root 无法解析: %w", err)
		}
		c.SitesRoot = abs
	}
	for _, p := range c.SSHPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("ssh_ports 含非法端口: %d", p)
		}
	}
	for _, item := range []struct{ name, val string }{
		{"ssh_guard_window", c.SSHGuardWindow},
		{"ssh_guard_ban", c.SSHGuardBan},
	} {
		d, err := time.ParseDuration(item.val)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s 解析失败: %q", item.name, item.val)
		}
	}
	if c.UpdateRepo != "" {
		parts := strings.Split(c.UpdateRepo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("update_repo 应形如 owner/repo，当前为 %q", c.UpdateRepo)
		}
	}
	if err := ValidateAccessPath(c.AccessPath); err != nil {
		return err
	}
	return nil
}

// SSHGuardDurations 返回解析后的窗口与封禁时长。
func (c *Config) SSHGuardDurations() (window, ban time.Duration, err error) {
	window, err = time.ParseDuration(c.SSHGuardWindow)
	if err != nil {
		return 0, 0, err
	}
	ban, err = time.ParseDuration(c.SSHGuardBan)
	if err != nil {
		return 0, 0, err
	}
	return window, ban, nil
}

// SessionTTLDuration 把字符串形式的 TTL 解析为 time.Duration。
func (c *Config) SessionTTLDuration() (time.Duration, error) {
	d, err := time.ParseDuration(c.SessionTTL)
	if err != nil {
		return 0, fmt.Errorf("session_ttl 解析失败: %w", err)
	}
	if d < 5*time.Minute {
		return 0, errors.New("session_ttl 不应小于 5m")
	}
	if d > 30*24*time.Hour {
		return 0, errors.New("session_ttl 不应大于 720h")
	}
	return d, nil
}

// Save 把当前配置写回文件（用于首次生成样例）。
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// Path 返回配置文件自身路径，备份时用于一并打包。
func (c *Config) Path() string { return c.path }

// PatchScalar 只改配置文件里某个顶层键的值，其余内容原样保留。
//
// 不要用 Save 做运行期的小改动：Save 写的是内存里的 cfg，而 Validate 已经把
// data_dir、backup_dir 这些相对路径解析成绝对路径，写回去就会把「./data」
// 固化成「/root/data」——0.3.0 那次线上故障就是这么来的。
func PatchScalar(path, key, literal string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	prefix := key + ":"
	found := false
	for i, line := range lines {
		// 只替换顶层键：缩进行属于嵌套结构，不能碰
		if strings.HasPrefix(line, prefix) {
			lines[i] = key + ": " + literal
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+": "+literal)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// IsLoopback 判断当前监听是否只对本机开放。// 首页与安装脚本据此决定是否提示「面板已暴露在公网」。
func (c *Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
