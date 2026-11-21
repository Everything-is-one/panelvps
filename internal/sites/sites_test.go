package sites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	base := t.TempDir()
	return &Manager{
		ConfDir:     filepath.Join(base, "conf"),
		SitesRoot:   filepath.Join(base, "www"),
		NginxBin:    "nginx",
		AcmeWebRoot: filepath.Join(base, "acme"),
	}
}

func TestValidateRejectsRootOutside(t *testing.T) {
	m := newManager(t)
	s := Site{Name: "blog", Domain: "example.com", Kind: "static", Root: "/etc"}
	if err := m.Validate(&s); err == nil {
		t.Fatal("站点根目录在 sites_root 之外应被拒绝")
	}
}

func TestValidateDefaultsRoot(t *testing.T) {
	m := newManager(t)
	s := Site{Name: "blog", Domain: "example.com", Kind: "static", Port: 80}
	if err := m.Validate(&s); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(m.SitesRoot, "blog")
	if s.Root != want {
		t.Errorf("默认根目录错误: got=%s want=%s", s.Root, want)
	}
}

func TestValidateRejectsBadNameAndDomain(t *testing.T) {
	m := newManager(t)
	cases := []Site{
		{Name: "Blog", Domain: "example.com", Kind: "static"},
		{Name: "a", Domain: "example.com", Kind: "static"},
		{Name: "blog", Domain: "bad domain", Kind: "static"},
		{Name: "blog", Domain: "example.com", Kind: "node"},
		{Name: "blog", Domain: "example.com", Kind: "php", PHPVersion: "8"},
	}
	for _, s := range cases {
		if err := m.Validate(&s); err == nil {
			t.Errorf("应拒绝: %+v", s)
		}
	}
}

func TestRenderStatic(t *testing.T) {
	m := newManager(t)
	s := Site{Name: "blog", Domain: "example.com", Kind: "static", Port: 80, Enabled: true}
	conf, err := m.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"listen 80;", "server_name example.com;", "try_files $uri $uri/ =404;",
		"location ~ /\\.(?!well-known)", "PanelVPS",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("配置缺少 %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "fastcgi_pass") {
		t.Error("静态站点不应包含 PHP 配置")
	}
}

func TestRenderPHP(t *testing.T) {
	m := newManager(t)
	s := Site{Name: "app", Domain: "app.example.com", Kind: "php", PHPVersion: "8.2", Port: 80}
	conf, err := m.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "fastcgi_pass unix:/run/php/php8.2-fpm.sock;") {
		t.Errorf("缺少 PHP-FPM 配置:\n%s", conf)
	}
	if !strings.Contains(conf, "/index.php?$query_string") {
		t.Errorf("缺少 PHP 站点的 try_files 回退:\n%s", conf)
	}
}

func TestRenderSSLRedirects(t *testing.T) {
	m := newManager(t)
	dir := t.TempDir()
	cert := filepath.Join(dir, "fullchain.pem")
	key := filepath.Join(dir, "privkey.pem")
	for _, p := range []string{cert, key} {
		if err := os.WriteFile(p, []byte("dummy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := Site{Name: "blog", Domain: "example.com", Kind: "static", Port: 80,
		SSL: true, CertPath: cert, KeyPath: key}
	conf, err := m.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"return 301 https://$host$request_uri;",
		"listen 443 ssl http2;",
		"ssl_certificate ",
		"TLSv1.2 TLSv1.3",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("配置缺少 %q:\n%s", want, conf)
		}
	}
}

func TestSSLRequiresExistingCert(t *testing.T) {
	m := newManager(t)
	s := Site{Name: "blog", Domain: "example.com", Kind: "static",
		SSL: true, CertPath: "/nonexistent/fullchain.pem", KeyPath: "/nonexistent/key.pem"}
	if err := m.Validate(&s); err == nil {
		t.Fatal("证书不存在时应拒绝启用 HTTPS")
	}
}

func TestWriteIsAtomicAndRemovable(t *testing.T) {
	m := newManager(t)
	s := Site{Name: "blog", Domain: "example.com", Kind: "static"}
	path, err := m.Write(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("配置文件未写入: %v", err)
	}
	if err := m.Remove("blog"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("配置未删除")
	}
}

func TestRemoveRejectsBadName(t *testing.T) {
	m := newManager(t)
	if err := m.Remove("../../etc/passwd"); err == nil {
		t.Fatal("非法站点名应被拒绝")
	}
}
