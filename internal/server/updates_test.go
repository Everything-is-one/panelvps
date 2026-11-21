package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/selfupdate"
)

// 自动检查必须复用 10 分钟内的结果，否则每次打开设置页都会打一次 GitHub，
// 未认证接口按 IP 限流，多开几个标签页就 403 了。
func TestUpdateCheckReusesFreshCache(t *testing.T) {
	checked := time.Now().Add(-2 * time.Minute)
	s := &Server{
		cfg: &config.Config{},
		updInfo: &updateInfo{
			Info:      &selfupdate.Info{Current: "0.3.13", Latest: "0.3.13", Available: false},
			CheckedAt: checked,
			Repo:      "Everything-is-one/panelvps",
		},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/updates/check", strings.NewReader(`{}`))
	s.handleUpdateCheck(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（不该回源）: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int        `json:"code"`
		Data updateInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !env.Data.CheckedAt.Equal(checked) {
		t.Errorf("返回的检查时间 = %v，期望复用缓存 %v", env.Data.CheckedAt, checked)
	}
}

// 空 body（老前端、curl 手测）不能被当成格式错误。
func TestUpdateCheckEmptyBodyUsesCache(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		updInfo: &updateInfo{
			Info:      &selfupdate.Info{Current: "0.3.13", Latest: "0.3.13"},
			CheckedAt: time.Now(),
		},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/updates/check", nil)
	s.handleUpdateCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("空 body 状态码 = %d: %s", rec.Code, rec.Body.String())
	}
}

// 未知字段应拒绝，避免前端把拼错的参数当成默认值静默通过。
func TestUpdateCheckRejectsUnknownField(t *testing.T) {
	s := &Server{cfg: &config.Config{}, updInfo: &updateInfo{
		Info: &selfupdate.Info{Current: "0.3.13"}, CheckedAt: time.Now(),
	}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/updates/check", strings.NewReader(`{"forse":true}`))
	s.handleUpdateCheck(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
}

// 状态接口只读缓存，不发网络请求；没有缓存时返回 null。
func TestUpdateStateEmpty(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	s.handleUpdateState(rec, httptest.NewRequest(http.MethodGet, "/api/v1/updates/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var env struct {
		Data struct {
			Update *updateInfo `json:"update"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if env.Data.Update != nil {
		t.Errorf("没有检查过时应返回 null，实际 %+v", env.Data.Update)
	}
}
