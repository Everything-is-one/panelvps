package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 下拉框的箭头是 CSS 里的 data:image/svg+xml。CSP 只写 default-src 'self'
// 时浏览器会静默拦掉它，页面上只剩一个没有箭头、看不出能点的白框——
// 这种回归肉眼很难发现，所以把 img-src 锁进测试。
func TestSecurityHeadersAllowInlineSvgImages(t *testing.T) {
	s := &Server{}
	h := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("缺少 Content-Security-Policy")
	}
	if !strings.Contains(csp, "img-src 'self' data:") {
		t.Fatalf("CSP 未放行 data: 图片，下拉箭头会被拦掉：%s", csp)
	}
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("CSP 基线被破坏：%s", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("基础安全头缺失")
	}
}

// 访问后缀是「收窄暴露面」：不带前缀的路径必须一律 404，
// 且 404 不能泄露「这里有个面板」（不能是跳转或 403）。
func TestAccessPathGate(t *testing.T) {
	var s Server
	s.setAccessPath("pv-abc123")

	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	})
	h := s.withAccessPath(echo)

	cases := []struct {
		path string
		code int
		body string
	}{
		{"/", http.StatusNotFound, ""},
		{"/api/v1/auth/login", http.StatusNotFound, ""},
		{"/.env", http.StatusNotFound, ""},
		{"/healthz", http.StatusOK, "/healthz"},
		{"/pv-abc123", http.StatusOK, "/"},
		{"/pv-abc123/", http.StatusOK, "/"},
		{"/pv-abc123/api/v1/system/stat", http.StatusOK, "/api/v1/system/stat"},
		// 前缀必须以完整路径段匹配，/pv-abc1234 不能当成带前缀的请求
		{"/pv-abc1234/", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s: 状态码 %d，期望 %d", c.path, rec.Code, c.code)
			continue
		}
		if c.body != "" && rec.Body.String() != c.body {
			t.Errorf("%s: 转发路径 %q，期望 %q", c.path, rec.Body.String(), c.body)
		}
	}
}

// 没启用后缀时行为必须与以前完全一致。
func TestAccessPathDisabledPassesThrough(t *testing.T) {
	var s Server
	s.setAccessPath("")
	h := s.withAccessPath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/stat", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "/api/v1/system/stat" {
		t.Fatalf("未启用后缀时不应改写请求: code=%d body=%q", rec.Code, rec.Body.String())
	}
}
