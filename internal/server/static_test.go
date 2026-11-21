package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Everything-is-one/panelvps/internal/version"
	"github.com/Everything-is-one/panelvps/web"
)

func newStaticServer(t *testing.T) *Server {
	t.Helper()
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		t.Fatalf("子文件系统: %v", err)
	}
	return &Server{static: sub}
}

// 首页要把版本号带进脚本地址，且不允许缓存，否则升级后界面仍是旧的。
func TestStaticIndexVersioned(t *testing.T) {
	s := newStaticServer(t)
	rec := httptest.NewRecorder()
	s.handleStatic(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	body := rec.Body.String()
	want := "/app.js?v=" + version.Version
	if !strings.Contains(body, want) {
		t.Fatalf("首页未注入版本号，期望包含 %q", want)
	}
	if !strings.Contains(body, "/app.css?v="+version.Version) {
		t.Fatalf("样式表未注入版本号")
	}
}

// 脚本走回源校验：带 ETag，命中时返回 304。
func TestStaticAssetETag(t *testing.T) {
	s := newStaticServer(t)
	rec := httptest.NewRecorder()
	s.handleStatic(rec, httptest.NewRequest(http.MethodGet, "/app.js?v="+version.Version, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("Cache-Control = %q", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺少 ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	s.handleStatic(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("命中 ETag 时应返回 304，实际 %d", rec2.Code)
	}
}
