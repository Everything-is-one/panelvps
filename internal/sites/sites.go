// Package sites 生成并托管 Nginx 站点配置。
//
// 边界：面板只写自己命名的 panelvps-<name>.conf，不碰用户手写的其他配置；
// 站点根目录必须落在 sites_root 之下；域名、站点名、PHP 版本都走严格正则。
package sites

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/bindfail"
)

// ErrInvalid 表示站点参数不合法。
var ErrInvalid = errors.New("站点参数不合法")

// Site 是生成配置所需的字段（与 store.Site 解耦，避免存储层渗进渲染逻辑）。
type Site struct {
	Name       string `json:"name"`
	Domain     string `json:"domain"`
	Root       string `json:"root"`
	Kind       string `json:"kind"` // static | php
	PHPVersion string `json:"php_version"`
	Port       int    `json:"port"`
	SSL        bool   `json:"ssl"`
	CertPath   string `json:"cert_path,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// Manager 绑定一组目录与可执行文件。
type Manager struct {
	ConfDir     string
	SitesRoot   string
	NginxBin    string
	AcmeWebRoot string
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)
	domainRe = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	phpVerRe = regexp.MustCompile(`^\d\.\d$`)
	labelRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// Validate 校验站点参数，并补全默认根目录。
func (m *Manager) Validate(s *Site) error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("%w：站点名只能用小写字母、数字与 -，长度 2–31", ErrInvalid)
	}
	if !validDomain(s.Domain) {
		return fmt.Errorf("%w：域名格式不正确", ErrInvalid)
	}
	switch s.Kind {
	case "static":
		s.PHPVersion = ""
	case "php":
		if !phpVerRe.MatchString(s.PHPVersion) {
			return fmt.Errorf("%w：PHP 版本应形如 8.2", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w：站点类型只能是 static 或 php", ErrInvalid)
	}
	if s.Port == 0 {
		s.Port = 80
	}
	if s.Port != 80 && s.Port != 443 && (s.Port < 1024 || s.Port > 65535) {
		return fmt.Errorf("%w：端口不合法", ErrInvalid)
	}
	if strings.TrimSpace(s.Root) == "" {
		s.Root = filepath.Join(m.SitesRoot, s.Name)
	}
	root := filepath.Clean(s.Root)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%w：网站根目录必须是绝对路径", ErrInvalid)
	}
	rel, err := filepath.Rel(filepath.Clean(m.SitesRoot), root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w：网站根目录必须在 %s 之下", ErrInvalid, m.SitesRoot)
	}
	s.Root = root

	if s.SSL {
		if s.CertPath == "" || s.KeyPath == "" {
			return fmt.Errorf("%w：启用 HTTPS 前必须先签发证书", ErrInvalid)
		}
		for _, p := range []string{s.CertPath, s.KeyPath} {
			if !filepath.IsAbs(p) {
				return fmt.Errorf("%w：证书路径必须是绝对路径", ErrInvalid)
			}
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("%w：证书文件不存在 %s", ErrInvalid, p)
			}
		}
	}
	return nil
}

func validDomain(d string) bool {
	if !domainRe.MatchString(d) || strings.HasPrefix(d, "-") || strings.HasSuffix(d, "-") {
		return false
	}
	for _, part := range strings.Split(d, ".") {
		if !labelRe.MatchString(part) {
			return false
		}
	}
	return true
}

// Render 生成 Nginx 配置。纯函数，便于单测与「预览」功能。
func (m *Manager) Render(s Site) (string, error) {
	if err := m.Validate(&s); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# managed by PanelVPS — site: %s\n", s.Name)
	fmt.Fprintf(&b, "# 任何手工修改都可能被面板覆盖\n\n")

	root := nginxQuote(filepath.ToSlash(s.Root))
	acme := nginxQuote(filepath.ToSlash(m.AcmeWebRoot))

	if s.SSL {
		// 80 端口只保留 ACME 校验，其余跳转 HTTPS
		b.WriteString("server {\n")
		b.WriteString("    listen 80;\n")
		fmt.Fprintf(&b, "    server_name %s;\n", s.Domain)
		b.WriteString("    location /.well-known/acme-challenge/ {\n")
		fmt.Fprintf(&b, "        root %s;\n", acme)
		b.WriteString("    }\n")
		b.WriteString("    location / {\n")
		b.WriteString("        return 301 https://$host$request_uri;\n")
		b.WriteString("    }\n")
		b.WriteString("}\n\n")

		b.WriteString("server {\n")
		fmt.Fprintf(&b, "    listen %d ssl http2;\n", httpsPort(s.Port))
		fmt.Fprintf(&b, "    server_name %s;\n", s.Domain)
		fmt.Fprintf(&b, "    ssl_certificate %s;\n", nginxQuote(s.CertPath))
		fmt.Fprintf(&b, "    ssl_certificate_key %s;\n", nginxQuote(s.KeyPath))
		b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n")
		b.WriteString("    ssl_prefer_server_ciphers off;\n")
		b.WriteString("    ssl_session_cache shared:SSL:10m;\n")
		b.WriteString("    ssl_session_timeout 1d;\n")
	} else {
		b.WriteString("server {\n")
		fmt.Fprintf(&b, "    listen %d;\n", s.Port)
		fmt.Fprintf(&b, "    server_name %s;\n", s.Domain)
	}

	fmt.Fprintf(&b, "    root %s;\n", root)
	b.WriteString("    index index.html index.htm")
	if s.Kind == "php" {
		b.WriteString(" index.php")
	}
	b.WriteString(";\n")
	fmt.Fprintf(&b, "    access_log /var/log/nginx/%s.access.log;\n", s.Name)
	fmt.Fprintf(&b, "    error_log  /var/log/nginx/%s.error.log;\n\n", s.Name)

	b.WriteString("    location /.well-known/acme-challenge/ {\n")
	fmt.Fprintf(&b, "        root %s;\n", acme)
	b.WriteString("    }\n\n")

	if s.Kind == "php" {
		b.WriteString("    location / {\n        try_files $uri $uri/ /index.php?$query_string;\n    }\n\n")
		b.WriteString("    location ~ \\.php$ {\n")
		b.WriteString("        include snippets/fastcgi-php.conf;\n")
		fmt.Fprintf(&b, "        fastcgi_pass unix:/run/php/php%s-fpm.sock;\n", s.PHPVersion)
		b.WriteString("    }\n\n")
	} else {
		b.WriteString("    location / {\n        try_files $uri $uri/ =404;\n    }\n\n")
	}

	// 隐藏文件（.git/.env 等）一律拒绝，ACME 目录已在上面单独放行
	b.WriteString("    location ~ /\\.(?!well-known) {\n        deny all;\n    }\n\n")
	b.WriteString("    location ~* \\.(css|js|png|jpg|jpeg|gif|svg|ico|woff2?)$ {\n")
	b.WriteString("        expires 7d;\n        access_log off;\n    }\n")
	b.WriteString("}\n")
	return b.String(), nil
}

func httpsPort(p int) int {
	if p == 80 || p == 0 {
		return 443
	}
	return p
}

// nginxQuote 用双引号包裹路径，并转义反斜杠与引号。
func nginxQuote(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `"`, `\"`)
	return `"` + p + `"`
}

// ConfPath 返回该站点的配置文件路径。
func (m *Manager) ConfPath(name string) string {
	return filepath.Join(m.ConfDir, "panelvps-"+name+".conf")
}

// EnsureRoot 创建站点根目录，并在空目录里放一个占位首页。
func (m *Manager) EnsureRoot(s Site) error {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	index := filepath.Join(s.Root, "index.html")
	if _, err := os.Stat(index); errors.Is(err, os.ErrNotExist) {
		body := fmt.Sprintf(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>%s</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;background:#0f1115;color:#e6e8ee}
code{color:#7ee787}</style></head>
<body><main><h1>%s</h1><p>站点已由 PanelVPS 创建。把文件放进 <code>%s</code> 即可。</p></main></body></html>
`, s.Domain, s.Domain, s.Root)
		return os.WriteFile(index, []byte(body), 0o644)
	}
	return nil
}

// Write 渲染并原子写入配置文件。
func (m *Manager) Write(s Site) (string, error) {
	body, err := m.Render(s)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.ConfDir, 0o755); err != nil {
		return "", err
	}
	dest := m.ConfPath(s.Name)
	tmp, err := os.CreateTemp(m.ConfDir, ".panelvps-*.conf")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Remove 删除站点配置（不删网站文件，避免误删数据）。
func (m *Manager) Remove(name string) error {
	if !nameRe.MatchString(name) {
		return ErrInvalid
	}
	err := os.Remove(m.ConfPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Test 执行 nginx -t。
//
// 这个命令同时验两件事：配置语法，以及每个 listen 能不能 bind。后者在
// 面板里几乎必然失败，且与配置好坏无关：
//
//   - 缺 CAP_NET_BIND_SERVICE（服务单元旧版本）→ EACCES(13)；
//   - nginx 已经跑着，80/443 就是它自己占的 → EADDRINUSE(98)。
//
// 直接把这些判成「配置错误」，会让新建站点、启用站点一律失败在最后一步，
// 而写入的配置其实是对的。所以按 errno 分开处理：权限问题直接放行；
// 占用问题只在「nginx 确实在跑」时放行 —— 没在跑还占着，那是真的冲突。
func (m *Manager) Test() (string, error) {
	bin := m.NginxBin
	if bin == "" {
		bin = "nginx"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", errors.New("未安装 Nginx，无法校验配置")
	}
	out, err := exec.Command(bin, "-t").CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		switch bindfail.Classify(msg) {
		case bindfail.PermDenied, bindfail.MixedBind:
			return msg + "\n（端口无法试绑：面板进程缺 CAP_NET_BIND_SERVICE，配置语法本身通过。到「设置 → 服务单元」用内置版本覆盖并重启面板）", nil
		case bindfail.AddrInUse:
			if nginxRunning() {
				return msg + "\n（端口已被运行中的 nginx 占用，属预期，配置语法本身通过）", nil
			}
		}
		return msg, fmt.Errorf("Nginx 配置校验失败: %s", msg)
	}
	return msg, nil
}

// nginxRunning 判断是否已有 nginx 主进程在跑。
//
// 用来区分「端口被 nginx 自己占着」（正常）与「端口被别的程序占着」
// （真冲突）。systemctl 不可用时退化为看 pid 文件。
func nginxRunning() bool {
	if err := exec.Command("systemctl", "is-active", "--quiet", "nginx").Run(); err == nil {
		return true
	}
	_, err := os.Stat("/run/nginx.pid")
	return err == nil
}

// Reload 校验通过后热加载。
func (m *Manager) Reload() (string, error) {
	if msg, err := m.Test(); err != nil {
		return msg, err
	}
	// nginx 没在跑时 nginx -s reload 只会报 open() /run/nginx.pid failed，
	// 那句话对用户毫无指向性。配置已经写好了，缺的是把 nginx 起起来。
	if !nginxRunning() {
		return "", errors.New("Nginx 未运行，配置已写入但尚未生效：到「软件商店」确认 Nginx 已安装并启动，再点一次「应用」")
	}
	bin := m.NginxBin
	if bin == "" {
		bin = "nginx"
	}
	out, err := exec.Command(bin, "-s", "reload").CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		return msg, fmt.Errorf("Nginx 重载失败: %s", msg)
	}
	return msg, nil
}

// CertPaths 返回某域名的证书路径。
func CertPaths(domain string) (cert, key string) {
	base := filepath.Join("/etc/letsencrypt/live", domain)
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "privkey.pem")
}

// IssueCert 调用 certbot 的 webroot 模式签发证书。
//
// 要求 80 端口已能访问 AcmeWebRoot，否则 HTTP-01 校验必然失败。
func (m *Manager) IssueCert(domain, email string) (cert, key, output string, err error) {
	if !validDomain(domain) {
		return "", "", "", fmt.Errorf("%w：域名格式不正确", ErrInvalid)
	}
	if email != "" && !strings.Contains(email, "@") {
		return "", "", "", fmt.Errorf("%w：邮箱格式不正确", ErrInvalid)
	}
	bin, err := exec.LookPath("certbot")
	if err != nil {
		return "", "", "", errors.New("未安装 certbot：请在软件商店安装，或执行 apt install certbot")
	}
	if err := os.MkdirAll(m.AcmeWebRoot, 0o755); err != nil {
		return "", "", "", err
	}

	args := []string{
		"certonly", "--webroot", "-w", m.AcmeWebRoot,
		"-d", domain, "--non-interactive", "--agree-tos",
		"--keep-until-expiring", "--expand",
	}
	if email != "" {
		args = append(args, "-m", email)
	} else {
		args = append(args, "--register-unsafely-without-email")
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	output = strings.TrimSpace(string(out))
	if err != nil {
		return "", "", output, fmt.Errorf("证书签发失败: %s", clip(output, 600))
	}

	cert, key = CertPaths(domain)
	for _, p := range []string{cert, key} {
		if _, err := os.Stat(p); err != nil {
			return "", "", output, fmt.Errorf("certbot 返回成功但证书不存在: %s", p)
		}
	}
	return cert, key, output, nil
}

// CertInfo 读取证书到期时间与签发者。
func CertInfo(certPath string) (notAfter time.Time, issuer string, err error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, "", err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return time.Time{}, "", errors.New("证书中没有 PEM 数据")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, "", err
		}
		return c.NotAfter, c.Issuer.CommonName, nil
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
