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
	return nil
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

// IsLoopback 判断当前监听是否只对本机开放。
// 首页与安装脚本据此决定是否提示「面板已暴露在公网」。
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
