package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/Everything-is-one/panelvps/internal/sites"
	"github.com/Everything-is-one/panelvps/internal/store"
)

func toSiteManagerSite(x store.Site) sites.Site {
	return sites.Site{
		Name: x.Name, Domain: x.Domain, Root: x.Root, Kind: x.Kind,
		PHPVersion: x.PHPVersion, Port: x.Port, SSL: x.SSL,
		CertPath: x.CertPath, KeyPath: x.KeyPath, Enabled: x.Enabled,
	}
}

type siteView struct {
	store.Site
	ConfigPath string `json:"config_path"`
	CertExpiry string `json:"cert_expiry,omitempty"`
	CertIssuer string `json:"cert_issuer,omitempty"`
}

func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListSites(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取站点失败")
		return
	}
	views := make([]siteView, 0, len(rows))
	for _, x := range rows {
		v := siteView{Site: x, ConfigPath: s.sites.ConfPath(x.Name)}
		if x.SSL && x.CertPath != "" {
			if na, issuer, err := sites.CertInfo(x.CertPath); err == nil {
				v.CertExpiry = na.Format("2006-01-02")
				v.CertIssuer = issuer
			}
		}
		views = append(views, v)
	}

	testMsg, testErr := s.sites.Test()
	nginxOK := testErr == nil
	ok(w, map[string]any{
		"items": views,
		"nginx": map[string]any{
			"ok":      nginxOK,
			"message": strings.TrimSpace(testMsg),
			"conf":    s.cfg.NginxConfDir,
			"root":    s.cfg.SitesRoot,
			"email":   s.cfg.CertEmail,
		},
	})
}

type siteReq struct {
	Name       string `json:"name"`
	Domain     string `json:"domain"`
	Root       string `json:"root"`
	Kind       string `json:"kind"`
	PHPVersion string `json:"php_version"`
	Port       int    `json:"port"`
	CreateRoot bool   `json:"create_root"`
	Enabled    *bool  `json:"enabled"`
}

func (s *Server) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	var req siteReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if req.Kind == "" {
		req.Kind = "static"
	}
	if req.Port == 0 {
		req.Port = 80
	}
	site := sites.Site{
		Name: strings.ToLower(strings.TrimSpace(req.Name)), Domain: strings.TrimSpace(req.Domain),
		Root: strings.TrimSpace(req.Root), Kind: req.Kind, PHPVersion: req.PHPVersion,
		Port: req.Port, Enabled: true,
	}
	if err := s.sites.Validate(&site); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if _, err := s.st.SiteByNameOrDomain(r.Context(), site.Name, site.Domain); err == nil {
		fail(w, http.StatusConflict, codeBadRequest, "站点名或域名已存在")
		return
	}
	if req.CreateRoot {
		if err := s.sites.EnsureRoot(site); err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, "创建站点目录失败: "+err.Error())
			return
		}
	}

	row := store.Site{
		Name: site.Name, Domain: site.Domain, Root: site.Root, Kind: site.Kind,
		PHPVersion: site.PHPVersion, Port: site.Port, Enabled: true,
	}
	if err := s.applySite(r.Context(), &row, false); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err := s.st.CreateSite(r.Context(), &row); err != nil {
		_ = s.sites.Remove(row.Name)
		fail(w, http.StatusInternalServerError, codeInternalError, "保存站点失败")
		return
	}
	s.audit(r, "site_create", row.Name, row.Domain+" → "+row.Root)
	ok(w, row)
}

func (s *Server) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "站点 ID 非法")
		return
	}
	old, err := s.st.SiteByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "站点不存在")
		return
	}
	var req siteReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}

	updated := *old
	if req.Domain != "" {
		updated.Domain = strings.TrimSpace(req.Domain)
	}
	if req.Root != "" {
		updated.Root = strings.TrimSpace(req.Root)
	}
	if req.Kind != "" {
		updated.Kind = req.Kind
	}
	if req.PHPVersion != "" {
		updated.PHPVersion = req.PHPVersion
	}
	if req.Port != 0 {
		updated.Port = req.Port
	}
	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	if err := s.applySite(r.Context(), &updated, true); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err := s.st.UpdateSite(r.Context(), &updated); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "保存站点失败")
		return
	}
	s.audit(r, "site_update", updated.Name, updated.Domain)
	ok(w, updated)
}

func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "站点 ID 非法")
		return
	}
	x, err := s.st.SiteByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "站点不存在")
		return
	}
	if r.URL.Query().Get("confirm") != "1" {
		fail(w, http.StatusBadRequest, codeBadRequest, "删除站点需要 confirm=1（网站文件不会被删除）")
		return
	}
	if err := s.sites.Remove(x.Name); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "删除配置失败: "+err.Error())
		return
	}
	if _, err := s.sites.Reload(); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "Nginx 重载失败: "+err.Error())
		return
	}
	if err := s.st.DeleteSite(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "删除记录失败")
		return
	}
	s.audit(r, "site_delete", x.Name, x.Domain+"（保留网站文件）")
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleSiteApply(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "站点 ID 非法")
		return
	}
	x, err := s.st.SiteByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "站点不存在")
		return
	}
	if err := s.applySite(r.Context(), x, true); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "site_apply", x.Name, "配置已下发")
	ok(w, map[string]any{"ok": true, "config_path": s.sites.ConfPath(x.Name)})
}

func (s *Server) handleSitePreview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "站点 ID 非法")
		return
	}
	x, err := s.st.SiteByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "站点不存在")
		return
	}
	body, err := s.sites.Render(toSiteManagerSite(*x))
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"config": body, "path": s.sites.ConfPath(x.Name)})
}

// applySite 写入配置 → nginx -t → reload；失败时恢复原文件。
func (s *Server) applySite(ctx context.Context, row *store.Site, allowDisable bool) error {
	mgrSite := toSiteManagerSite(*row)
	if err := s.sites.Validate(&mgrSite); err != nil {
		return err
	}
	row.Root = mgrSite.Root

	confPath := s.sites.ConfPath(row.Name)
	prev, prevErr := os.ReadFile(confPath)

	if !row.Enabled && allowDisable {
		if err := s.sites.Remove(row.Name); err != nil {
			return err
		}
		if _, err := s.sites.Reload(); err != nil {
			return err
		}
		return nil
	}

	if _, err := s.sites.Write(mgrSite); err != nil {
		return fmt.Errorf("写入 Nginx 配置失败: %w", err)
	}
	if msg, err := s.sites.Test(); err != nil {
		// 回滚到写入前的内容，避免留下坏配置导致 Nginx 无法 reload
		if prevErr == nil {
			_ = os.WriteFile(confPath, prev, 0o644)
		} else {
			_ = os.Remove(confPath)
		}
		// nginx 不存在时 msg 为空，必须回落到 err，否则前端只能看到一句空话
		reason := strings.TrimSpace(msg)
		if reason == "" {
			reason = err.Error()
		}
		return fmt.Errorf("Nginx 配置校验失败，已回滚：%s", reason)
	}
	if _, err := s.sites.Reload(); err != nil {
		return err
	}
	return nil
}

// handleSiteSSL 异步申请证书并启用 HTTPS。
func (s *Server) handleSiteSSL(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "站点 ID 非法")
		return
	}
	x, err := s.st.SiteByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "站点不存在")
		return
	}
	if s.jobs == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "任务执行器不可用")
		return
	}

	job, err := s.jobs.Start("ssl", x.Domain, func(ctx context.Context, log func(string)) error {
		log("开始为 " + x.Domain + " 申请证书")
		log("ACME 校验目录: " + s.cfg.AcmeWebRoot)
		cert, key, out, err := s.sites.IssueCert(x.Domain, s.cfg.CertEmail)
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) != "" {
				log(line)
			}
		}
		if err != nil {
			return err
		}
		log("证书已签发: " + cert)

		updated := *x
		updated.SSL = true
		updated.CertPath = cert
		updated.KeyPath = key
		if err := s.applySite(ctx, &updated, true); err != nil {
			return err
		}
		if err := s.st.UpdateSite(ctx, &updated); err != nil {
			return err
		}
		log("已启用 HTTPS 并重载 Nginx")
		return nil
	})
	if err != nil {
		fail(w, http.StatusTooManyRequests, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "site_ssl_start", x.Name, x.Domain)
	ok(w, job)
}
