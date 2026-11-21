// Package server 组装 HTTP 服务、路由与中间件。
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/backup"
	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/filemgr"
	"github.com/Everything-is-one/panelvps/internal/jobs"
	"github.com/Everything-is-one/panelvps/internal/metrics"
	"github.com/Everything-is-one/panelvps/internal/notify"
	"github.com/Everything-is-one/panelvps/internal/relay"
	"github.com/Everything-is-one/panelvps/internal/sites"
	"github.com/Everything-is-one/panelvps/internal/softstore"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/version"
	"github.com/Everything-is-one/panelvps/internal/xray"
	"github.com/Everything-is-one/panelvps/web"
)

const (
	sessionCookie = "pv_session"
	csrfCookie    = "pv_csrf"
	csrfHeader    = "X-CSRF-Token"
)

// 业务错误码，与 PRD §12 对齐。
const (
	codeOK            = 0
	codeUnauthorized  = 1001
	codeNeedVerify    = 1003
	codeBadRequest    = 2001
	codeNotFound      = 2002
	codeTooManyTries  = 2003
	codeInternalError = 5001
)

// Server 持有全部运行时依赖。
type Server struct {
	cfg      *config.Config
	st       *store.Store
	metrics  *metrics.Collector
	limiter  *auth.LoginLimiter
	logger   *slog.Logger
	mux      *http.ServeMux
	static   fs.FS
	startAt  time.Time
	dummyPwd string
	xr       *xray.Manager

	files    *filemgr.Manager
	trash    *filemgr.Trash
	guard    *filemgr.Guard
	sites    *sites.Manager
	appstore *softstore.Store
	backups  *backup.Manager
	notifier *notify.Notifier
	jobs     *jobs.Manager
	relays   *relay.Manager

	termMu     sync.Mutex
	termTicket map[string]time.Time
	termActive int

	updMu   sync.Mutex
	updInfo *updateInfo

	restartMu  sync.Mutex
	restarting bool
}

// New 构造 Server 并注册路由。
func New(cfg *config.Config, st *store.Store, logger *slog.Logger) (*Server, error) {
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}

	// 用于抵御用户名枚举的等时校验：用户不存在时也跑一次哈希。
	dummy, err := auth.HashPassword("panelvps-dummy-password")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:      cfg,
		st:       st,
		metrics:  metrics.New(),
		limiter:  auth.NewLoginLimiter(5, 10*time.Minute, time.Hour),
		logger:   logger,
		mux:      http.NewServeMux(),
		static:   sub,
		startAt:  time.Now(),
		dummyPwd: dummy,
	}

	// Xray 不在也不影响面板启动：代理模块以「不可用」状态呈现，
	// 运维功能照常工作。这比启动即失败更符合低内存机器的现实。
	bin, detectErr := xray.Detect(cfg.XrayBin)
	if detectErr != nil {
		logger.Warn("未探测到 Xray，代理功能不可用", "err", detectErr)
	}
	// 日志同时落盘：面板托管的是 `xray run` 子进程，journald 里没有它，
	// 日志页的「Xray 内核」源只能读我们自己写的那份。
	s.xr = xray.NewManager(bin, cfg.XrayConfDir, cfg.XrayAPIPort, "warning",
		filepath.Join(cfg.DataDir, "xray.log"), logger)

	// 运维模块：任何一个初始化失败都只降级对应功能，不阻止面板启动。
	if fm, err := filemgr.New(cfg.FileRoot); err != nil {
		logger.Warn("文件管理不可用", "root", cfg.FileRoot, "err", err)
	} else {
		s.files = fm
		// 面板跑在 root 下且带 CAP_DAC_OVERRIDE，「用户自己小心点」不成立：
		// 删除守卫与回收站都放在服务端，前端被绕过也删不掉系统目录。
		s.guard = filemgr.NewGuard(
			cfg.DataDir,
			filepath.Dir(cfg.Path()),
			filepath.Join(cfg.DataDir, "trash"),
			filepath.Join(cfg.DataDir, "backups"),
		)
		if tr, err := filemgr.NewTrash(filepath.Join(cfg.DataDir, "trash")); err != nil {
			logger.Warn("回收站不可用，删除将退化为彻底删除", "err", err)
		} else {
			s.trash = tr
			if n := tr.Sweep(7 * 24 * time.Hour); n > 0 {
				logger.Info("回收站已清理过期条目", "count", n)
			}
		}
	}
	s.sites = &sites.Manager{
		ConfDir:     cfg.NginxConfDir,
		SitesRoot:   cfg.SitesRoot,
		NginxBin:    cfg.NginxBin,
		AcmeWebRoot: cfg.AcmeWebRoot,
	}
	if apps, err := softstore.Load(); err != nil {
		logger.Error("软件商店清单加载失败", "err", err)
	} else {
		s.appstore = apps
	}
	if bm, err := backup.New(cfg.BackupDir, cfg.DataDir); err != nil {
		// 配置里的备份目录写不进去（改过 data_dir 的老配置、挂载成只读的外置盘等），
		// 与其让用户在点「立即备份」时才看到 read-only，不如现在退回数据目录下。
		fallback := filepath.Join(cfg.DataDir, "backups")
		if bm2, err2 := backup.New(fallback, cfg.DataDir); err2 == nil {
			logger.Warn("备份目录不可写，已回落到数据目录",
				"configured", cfg.BackupDir, "using", fallback, "reason", err)
			cfg.BackupDir = fallback
			s.backups = bm2
		} else {
			logger.Warn("备份模块不可用", "dir", cfg.BackupDir, "err", err)
		}
	} else {
		s.backups = bm
	}
	if n, err := notify.New(cfg.AlertWebhook, logger); err != nil {
		logger.Warn("告警 Webhook 配置无效，已禁用", "err", err)
	} else {
		s.notifier = n
	}
	s.jobs = jobs.New(60, 2)
	s.termTicket = map[string]time.Time{}

	// 中转规则随面板启动恢复。转发引擎不依赖 Xray，也不依赖任何外部
	// 程序，所以这里可以同步启动：失败只影响对应规则，不会拖住面板。
	s.relays = relay.New(logger)
	s.reloadRelays(context.Background())

	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	s.mux.HandleFunc("GET /api/v1/auth/state", s.handleAuthState)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.requireAuth(s.handleLogout))
	s.mux.HandleFunc("POST /api/v1/auth/password", s.requireAuth(s.handleChangePassword))
	s.mux.HandleFunc("GET /api/v1/system/stat", s.requireAuth(s.handleSystemStat))

	// 代理模块
	s.mux.HandleFunc("GET /api/v1/proxies", s.requireAuth(s.handleProxiesOverview))
	s.mux.HandleFunc("POST /api/v1/proxies/apply", s.requireAuth(s.handleApply))
	s.mux.HandleFunc("POST /api/v1/proxies/traffic/collect", s.requireAuth(s.handleCollectTraffic))
	s.mux.HandleFunc("PUT /api/v1/proxies/preferences", s.requireAuth(s.handleSavePrefs))
	s.mux.HandleFunc("POST /api/v1/proxies/keys/reality", s.requireAuth(s.handleRealityKey))
	s.mux.HandleFunc("POST /api/v1/proxies/inbounds", s.requireAuth(s.handleCreateInbound))
	s.mux.HandleFunc("PATCH /api/v1/proxies/inbounds/{id}", s.requireAuth(s.handleUpdateInbound))
	s.mux.HandleFunc("DELETE /api/v1/proxies/inbounds/{id}", s.requireAuth(s.handleDeleteInbound))
	s.mux.HandleFunc("POST /api/v1/proxies/inbounds/{id}/clients", s.requireAuth(s.handleCreateClient))
	s.mux.HandleFunc("PATCH /api/v1/proxies/clients/{id}", s.requireAuth(s.handleUpdateClient))
	s.mux.HandleFunc("DELETE /api/v1/proxies/clients/{id}", s.requireAuth(s.handleDeleteClient))
	s.mux.HandleFunc("POST /api/v1/proxies/clients/{id}/reset", s.requireAuth(s.handleResetClientTraffic))
	s.mux.HandleFunc("GET /api/v1/proxies/clients/{id}/links", s.requireAuth(s.handleClientLinks))
	s.mux.HandleFunc("GET /api/v1/proxies/clients/{id}/qr", s.requireAuth(s.handleQR))

	// 中转（端口转发）：与被代理的入站是两条独立链路
	s.mux.HandleFunc("GET /api/v1/relays", s.requireAuth(s.handleRelayList))
	s.mux.HandleFunc("POST /api/v1/relays", s.requireAuth(s.handleRelayCreate))
	s.mux.HandleFunc("PATCH /api/v1/relays/{id}", s.requireAuth(s.handleRelayUpdate))
	s.mux.HandleFunc("DELETE /api/v1/relays/{id}", s.requireAuth(s.handleRelayDelete))
	s.mux.HandleFunc("POST /api/v1/relays/{id}/toggle", s.requireAuth(s.handleRelayToggle))

	// 系统运维
	s.mux.HandleFunc("GET /api/v1/system/info", s.requireAuth(s.handleSystemInfo))
	s.mux.HandleFunc("GET /api/v1/system/processes", s.requireAuth(s.handleProcesses))
	s.mux.HandleFunc("GET /api/v1/system/top", s.requireAuth(s.handleSystemTop))
	s.mux.HandleFunc("GET /api/v1/system/services", s.requireAuth(s.handleServices))
	s.mux.HandleFunc("POST /api/v1/system/services/{name}/actions/{action}", s.requireAuth(s.handleServiceAction))
	s.mux.HandleFunc("GET /api/v1/system/ports", s.requireAuth(s.handlePorts))
	s.mux.HandleFunc("POST /api/v1/system/restart", s.requireAuth(s.handleRestart))
	s.mux.HandleFunc("GET /api/v1/system/unit", s.requireAuth(s.handleSystemUnit))
	s.mux.HandleFunc("POST /api/v1/system/unit/sync", s.requireAuth(s.handleSystemUnitSync))
	s.mux.HandleFunc("GET /api/v1/system/jobs", s.requireAuth(s.handleJobs))
	s.mux.HandleFunc("GET /api/v1/system/jobs/{id}", s.requireAuth(s.handleJob))
	s.mux.HandleFunc("GET /api/v1/audit", s.requireAuth(s.handleAuditList))

	// 控制台：访问方式、账号、BBR、重启
	s.mux.HandleFunc("GET /api/v1/control", s.requireAuth(s.handleControlStatus))
	s.mux.HandleFunc("PUT /api/v1/control/access", s.requireAuth(s.handleControlAccess))
	s.mux.HandleFunc("POST /api/v1/control/password", s.requireAuth(s.handleControlPassword))
	s.mux.HandleFunc("POST /api/v1/control/reboot", s.requireAuth(s.handleControlReboot))
	s.mux.HandleFunc("POST /api/v1/control/bbr", s.requireAuth(s.handleControlBBR))
	s.mux.HandleFunc("POST /api/v1/control/xray/restart", s.requireAuth(s.handleControlXrayRestart))

	// 文件
	s.mux.HandleFunc("GET /api/v1/files", s.requireAuth(s.handleFileList))
	s.mux.HandleFunc("GET /api/v1/files/stat", s.requireAuth(s.handleFileStat))
	s.mux.HandleFunc("GET /api/v1/files/content", s.requireAuth(s.handleFileRead))
	s.mux.HandleFunc("PUT /api/v1/files/content", s.requireAuth(s.handleFileWrite))
	s.mux.HandleFunc("GET /api/v1/files/download", s.requireAuth(s.handleFileDownload))
	s.mux.HandleFunc("GET /api/v1/files/batch-download", s.requireAuth(s.handleFileBatchDownload))
	s.mux.HandleFunc("POST /api/v1/files/upload", s.requireAuth(s.handleFileUpload))
	s.mux.HandleFunc("POST /api/v1/files/mkdir", s.requireAuth(s.handleFileMkdir))
	s.mux.HandleFunc("POST /api/v1/files/rename", s.requireAuth(s.handleFileRename))
	s.mux.HandleFunc("POST /api/v1/files/copy", s.requireAuth(s.handleFileCopy))
	s.mux.HandleFunc("POST /api/v1/files/delete", s.requireAuth(s.handleFileDelete))
	s.mux.HandleFunc("GET /api/v1/trash", s.requireAuth(s.handleTrashList))
	s.mux.HandleFunc("POST /api/v1/trash/restore", s.requireAuth(s.handleTrashRestore))
	s.mux.HandleFunc("POST /api/v1/trash/delete", s.requireAuth(s.handleTrashDelete))
	s.mux.HandleFunc("POST /api/v1/files/archive", s.requireAuth(s.handleFileArchive))
	s.mux.HandleFunc("POST /api/v1/files/extract", s.requireAuth(s.handleFileExtract))

	// 终端
	s.mux.HandleFunc("POST /api/v1/terminal/ticket", s.requireAuth(s.handleTerminalTicket))
	s.mux.HandleFunc("GET /api/v1/terminal/ws", s.handleTerminalWS)

	// 防火墙与安全
	s.mux.HandleFunc("GET /api/v1/firewall", s.requireAuth(s.handleFirewallStatus))
	s.mux.HandleFunc("POST /api/v1/firewall/rules", s.requireAuth(s.handleFirewallCreate))
	s.mux.HandleFunc("DELETE /api/v1/firewall/rules/{id}", s.requireAuth(s.handleFirewallDelete))
	s.mux.HandleFunc("POST /api/v1/firewall/apply", s.requireAuth(s.handleFirewallApply))
	s.mux.HandleFunc("POST /api/v1/firewall/bans", s.requireAuth(s.handleBanCreate))
	s.mux.HandleFunc("DELETE /api/v1/firewall/bans/{ip}", s.requireAuth(s.handleBanDelete))
	s.mux.HandleFunc("GET /api/v1/security/ssh", s.requireAuth(s.handleSSHReport))

	// 日志
	s.mux.HandleFunc("GET /api/v1/logs/sources", s.requireAuth(s.handleLogSources))
	s.mux.HandleFunc("GET /api/v1/logs", s.requireAuth(s.handleLogs))

	// 网站
	s.mux.HandleFunc("GET /api/v1/sites", s.requireAuth(s.handleSiteList))
	s.mux.HandleFunc("POST /api/v1/sites", s.requireAuth(s.handleSiteCreate))
	s.mux.HandleFunc("GET /api/v1/sites/{id}/preview", s.requireAuth(s.handleSitePreview))
	s.mux.HandleFunc("PATCH /api/v1/sites/{id}", s.requireAuth(s.handleSiteUpdate))
	s.mux.HandleFunc("DELETE /api/v1/sites/{id}", s.requireAuth(s.handleSiteDelete))
	s.mux.HandleFunc("POST /api/v1/sites/{id}/apply", s.requireAuth(s.handleSiteApply))
	s.mux.HandleFunc("POST /api/v1/sites/{id}/ssl", s.requireAuth(s.handleSiteSSL))

	// 软件商店
	s.mux.HandleFunc("GET /api/v1/store", s.requireAuth(s.handleStoreList))
	s.mux.HandleFunc("GET /api/v1/store/{slug}/plan", s.requireAuth(s.handleStorePlan))
	s.mux.HandleFunc("POST /api/v1/store/{slug}/install", s.requireAuth(s.handleStoreInstall))
	s.mux.HandleFunc("POST /api/v1/store/{slug}/uninstall", s.requireAuth(s.handleStoreUninstall))

	// 备份
	s.mux.HandleFunc("GET /api/v1/backups", s.requireAuth(s.handleBackupList))
	s.mux.HandleFunc("POST /api/v1/backups", s.requireAuth(s.handleBackupCreate))
	s.mux.HandleFunc("GET /api/v1/backups/{name}/download", s.requireAuth(s.handleBackupDownload))
	s.mux.HandleFunc("DELETE /api/v1/backups/{name}", s.requireAuth(s.handleBackupDelete))
	s.mux.HandleFunc("POST /api/v1/backups/{name}/restore", s.requireAuth(s.handleBackupRestore))

	// 设置与更新
	s.mux.HandleFunc("GET /api/v1/settings", s.requireAuth(s.handleSettingsGet))
	s.mux.HandleFunc("PUT /api/v1/settings", s.requireAuth(s.handleSettingsPut))
	s.mux.HandleFunc("POST /api/v1/updates/check", s.requireAuth(s.handleUpdateCheck))
	s.mux.HandleFunc("POST /api/v1/updates/apply", s.requireAuth(s.handleUpdateApply))

	// 兜底：静态资源与 SPA 入口
	s.mux.HandleFunc("GET /", s.handleStatic)
}

// Handler 返回串联好中间件的处理器。
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.withCSRF(h)
	h = s.withSecurityHeaders(h)
	h = s.withBodyLimit(h)
	h = s.withRecover(h)
	h = s.withAccessLog(h)
	return h
}

// Cleanup 由后台任务周期调用，清过期会话与限流表。
func (s *Server) Cleanup(ctx context.Context) {
	if n, err := s.st.CleanupSessions(ctx); err == nil && n > 0 {
		s.logger.Info("清理过期会话", "count", n)
	}
	s.limiter.Sweep()
}

// ---------- 响应辅助 ----------

type envelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Code: code, Msg: msg, Data: data})
}

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, codeOK, "ok", data)
}

func fail(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, code, msg, nil)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// 拒绝 trailing 数据，避免请求体被拼接利用
	if dec.More() {
		return errors.New("请求体包含多余内容")
	}
	return nil
}

// ---------- 静态资源 ----------

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".json":  "application/json; charset=utf-8",
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if upath == "" || upath == "." {
		upath = "index.html"
	}

	// 目录穿越兜底：Clean 之后仍含 .. 的一律拒绝
	if strings.Contains(upath, "..") {
		fail(w, http.StatusBadRequest, codeBadRequest, "非法路径")
		return
	}

	if _, err := fs.Stat(s.static, upath); err != nil {
		// SPA 回退：未知路径统一交给 index.html，由前端路由处理
		upath = "index.html"
	}

	raw, err := fs.ReadFile(s.static, upath)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "资源不存在")
		return
	}

	ext := strings.ToLower(path.Ext(upath))
	ct, known := contentTypes[ext]
	if !known {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)

	// 首页每次都重新取，并把版本号注入脚本地址：升级后 URL 变化，
	// 浏览器不会继续用上一版缓存的前端脚本（曾出现「升级了但界面没变」）。
	if upath == "index.html" {
		q := []byte("?v=" + version.Version)
		raw = bytes.ReplaceAll(raw, []byte(`/app.js"`), append(append([]byte("/app.js"), q...), '"'))
		raw = bytes.ReplaceAll(raw, []byte(`/app.css"`), append(append([]byte("/app.css"), q...), '"'))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
		return
	}

	// 其余静态资源要求回源校验，配合 ETag 命中返回 304，避免升级后残留旧文件。
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(raw)
}
