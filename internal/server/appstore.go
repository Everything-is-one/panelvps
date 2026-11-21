package server

import (
	"context"
	"net/http"
	"runtime"

	"github.com/Everything-is-one/panelvps/internal/notify"
	"github.com/Everything-is-one/panelvps/internal/softstore"
)

func (s *Server) handleStoreList(w http.ResponseWriter, r *http.Request) {
	if s.appstore == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "软件商店清单未加载")
		return
	}
	installed, err := s.st.ListInstalledApps(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取安装状态失败")
		return
	}

	type item struct {
		*softstore.Manifest
		Installed    bool   `json:"installed"`
		InstalledVer string `json:"installed_version,omitempty"`
		InstalledAt  string `json:"installed_at,omitempty"`
		Status       string `json:"status,omitempty"`
		Detail       string `json:"detail,omitempty"`
	}
	items := make([]item, 0, len(s.appstore.List()))
	for _, m := range s.appstore.List() {
		it := item{Manifest: m}
		if v, ok := softstore.Installed(m); ok {
			it.Installed = true
			it.InstalledVer = v
		}
		if rec, ok := installed[m.Slug]; ok {
			it.Status = rec.Status
			it.Detail = rec.Detail
			it.InstalledAt = rec.InstalledAt.Format("2006-01-02 15:04")
			if it.InstalledVer == "" {
				it.Installed = rec.Status == "installed"
				it.InstalledVer = rec.Version
			}
		}
		items = append(items, it)
	}

	ok(w, map[string]any{
		"items":     items,
		"supported": runtime.GOOS == "linux",
		"reason":    map[bool]string{true: "", false: "软件商店仅能在 Linux 服务器上执行安装"}[runtime.GOOS == "linux"],
	})
}

func (s *Server) handleStorePlan(w http.ResponseWriter, r *http.Request) {
	if s.appstore == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "软件商店清单未加载")
		return
	}
	m, err := s.appstore.Get(r.PathValue("slug"))
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "软件不存在")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "uninstall" {
		mode = "install"
	}
	ok(w, map[string]any{
		"manifest": m,
		"mode":     mode,
		"steps":    softstore.PlanLines(m, mode),
	})
}

type installReq struct {
	Force bool `json:"force"`
}

func (s *Server) handleStoreInstall(w http.ResponseWriter, r *http.Request) {
	if s.appstore == nil || s.jobs == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "软件商店不可用")
		return
	}
	// 非 Linux 直接拒绝：清单里全是 apt/服务动作，起一个注定失败的任务
	// 只会让用户对着满屏报错猜原因。
	if runtime.GOOS != "linux" {
		fail(w, http.StatusNotImplemented, codeBadRequest, softstore.ErrUnsupported.Error())
		return
	}
	slug := r.PathValue("slug")
	m, err := s.appstore.Get(slug)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "软件不存在")
		return
	}
	var req installReq
	_ = decodeJSON(w, r, &req) // 允许空请求体

	job, err := s.jobs.Start("install-"+slug, m.Name, func(ctx context.Context, log func(string)) error {
		if err := s.appstore.Install(ctx, slug, log, req.Force); err != nil {
			_ = s.st.MarkAppInstalled(ctx, slug, m.Version, "failed", err.Error())
			s.notifierSend(notify.Event{
				Level: "error", Title: "软件安装失败: " + m.Name, Detail: err.Error(),
			})
			return err
		}
		_ = s.st.MarkAppInstalled(ctx, slug, m.Version, "installed", "")
		s.notifierSend(notify.Event{
			Level: "info", Title: "软件已安装: " + m.Name, Detail: "版本 " + m.Version,
		})
		return nil
	})
	if err != nil {
		fail(w, http.StatusTooManyRequests, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "store_install", slug, m.Name)
	ok(w, job)
}

func (s *Server) handleStoreUninstall(w http.ResponseWriter, r *http.Request) {
	if s.appstore == nil || s.jobs == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "软件商店不可用")
		return
	}
	if runtime.GOOS != "linux" {
		fail(w, http.StatusNotImplemented, codeBadRequest, softstore.ErrUnsupported.Error())
		return
	}
	slug := r.PathValue("slug")
	m, err := s.appstore.Get(slug)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "软件不存在")
		return
	}
	job, err := s.jobs.Start("uninstall-"+slug, m.Name, func(ctx context.Context, log func(string)) error {
		if err := s.appstore.Uninstall(ctx, slug, log); err != nil {
			return err
		}
		return s.st.MarkAppRemoved(ctx, slug)
	})
	if err != nil {
		fail(w, http.StatusTooManyRequests, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "store_uninstall", slug, m.Name)
	ok(w, job)
}

// notifierSend 收敛空指针判断，避免每个调用点都写 if。
func (s *Server) notifierSend(ev notify.Event) {
	if s.notifier != nil {
		s.notifier.Send(ev)
	}
}
