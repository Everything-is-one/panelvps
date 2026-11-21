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
