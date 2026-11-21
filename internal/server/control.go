package server

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/manage"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
	"github.com/Everything-is-one/panelvps/internal/tune"
	"github.com/Everything-is-one/panelvps/internal/version"
)

func (s *Server) handleControlStatus(w http.ResponseWriter, r *http.Request) {
	port := s.panelPort()
	public := !s.cfg.IsLoopback()
	urls := manage.AccessURLs(s.accessHosts(r), port, public)

	account := map[string]any{"username": ""}
	if u := userFrom(r.Context()); u != nil {
		v := toUserView(u)
		account = map[string]any{
			"username":             v.Username,
			"must_change_password": v.MustChangePassword,
			"last_login_at":        v.LastLoginAt,
			"last_login_ip":        v.LastLoginIP,
		}
	}

	fw := firewall.Detect()
	info := sysinfo.Info()

	ok(w, map[string]any{
		"panel": map[string]any{
			"listen":      s.cfg.Listen,
			"port":        port,
			"public":      public,
			"urls":        urls,
			"local_ips":   manage.LocalIPv4s(),
			"public_host": s.publicHost(r),
			"version":     version.Version,
			"config_path": s.cfg.Path(),
			"uptime_sec":  int(time.Since(s.startAt).Seconds()),
		},
		"account": account,
		"bbr":     tune.BBR(),
		"xray": map[string]any{
			"available": s.xr != nil && s.xr.Available(),
			"running":   s.xr != nil && s.xr.Running(),
		},
		"firewall": map[string]any{
			"supported": fw.Supported,
			"backend":   fw.Backend,
			"reason":    fw.Reason,
		},
		"system": map[string]any{
			"hostname": info.Hostname,
			"distro":   info.Distro,
			"kernel":   info.Kernel,
			"arch":     info.Arch,
			"uptime":   info.Uptime,
			"timezone": systemTimezone(),
		},
	})
}

type accessReq struct {
	Port    int    `json:"port"`
	Public  bool   `json:"public"`
	Confirm string `json:"confirm"`
}

// handleControlAccess 切换监听范围或端口。配置落盘后重启面板生效。
func (s *Server) handleControlAccess(w http.ResponseWriter, r *http.Request) {
	var req accessReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if req.Confirm != "ACCESS" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=ACCESS 才能修改访问方式")
		return
	}
	// 网页这条路改完要能不靠人重启，没有 systemd 就别假装能改。
	if !manage.HasSystemd() {
		fail(w, http.StatusBadRequest, codeBadRequest, "当前环境没有 systemd，修改后无法自动重启")
		return
	}
	cfgPath := s.cfg.Path()
	if cfgPath == "" {
		fail(w, http.StatusInternalServerError, codeInternalError, "无法定位配置文件")
		return
	}

	res, err := manage.SetAccess(cfgPath, req.Port, req.Public)
	if err != nil {
		// 端口占用、防火墙回滚都归到「改不了」，按 400 返回给前端
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	urls := manage.AccessURLs(s.accessHosts(r), req.Port, req.Public)
	if !res.Changed {
		ok(w, map[string]any{
			"listen":     res.NewListen,
			"port":       req.Port,
			"public":     req.Public,
			"restarting": false,
			"urls":       urls,
			"warnings":   []string{},
		})
		return
	}

	s.audit(r, "control_access", res.NewListen, fmt.Sprintf("public=%v port=%d", req.Public, req.Port))
	s.restartPanelSoon(1200 * time.Millisecond)

	ok(w, map[string]any{
		"listen":     res.NewListen,
		"port":       req.Port,
		"public":     req.Public,
		"restarting": true,
		"urls":       urls,
		"warnings":   res.Warnings,
	})
}

type resetPwdReq struct {
	Mode     string `json:"mode"`
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

// handleControlPassword 直接重置当前账号密码，并踢掉所有会话。
func (s *Server) handleControlPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPwdReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if req.Confirm != "RESET" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=RESET 才能重置密码")
		return
	}
	u := userFrom(r.Context())
	if u == nil {
		fail(w, http.StatusUnauthorized, codeUnauthorized, "未登录")
		return
	}

	var plain string
	switch req.Mode {
	case "random":
		pwd, err := auth.RandomPassword(20)
		if err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "生成密码失败")
			return
		}
		plain = pwd
	case "custom":
		if err := auth.ValidatePassword(req.Password, u.Username); err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		plain = req.Password
	default:
		fail(w, http.StatusBadRequest, codeBadRequest, "mode 只能是 random 或 custom")
		return
	}

	hash, err := auth.HashPassword(plain)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err := s.st.UpdatePassword(r.Context(), u.ID, hash); err != nil {
		s.logger.Error("重置密码失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "内部错误")
		return
	}
	_ = s.st.DeleteUserSessions(r.Context(), u.ID)
	s.clearSessionCookie(w)
	s.audit(r, "password_reset", u.Username, req.Mode+" 重置")

	data := map[string]any{"relogin": true}
	if req.Mode == "random" {
		data["password"] = plain
	}
	ok(w, data)
}

func (s *Server) handleControlReboot(w http.ResponseWriter, r *http.Request) {
	var req confirmReq
	if err := decodeJSON(w, r, &req); err != nil || req.Confirm != "REBOOT" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=REBOOT 才能重启服务器")
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "当前环境没有 systemd，无法重启服务器")
		return
	}
	out, err := exec.Command("systemctl", "--no-block", "reboot").CombinedOutput()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError,
			"重启失败: "+firstLine(string(out), err.Error()))
		return
	}
	s.audit(r, "server_reboot", "host", "用户触发服务器重启")
	ok(w, map[string]any{"rebooting": true})
}

func (s *Server) handleControlXrayRestart(w http.ResponseWriter, r *http.Request) {
	if s.xr == nil || !s.xr.Available() {
		fail(w, http.StatusBadRequest, codeBadRequest, "没有探测到 Xray，无法重启")
		return
	}
	if err := s.xr.Restart(r.Context()); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "重启 Xray 失败: "+err.Error())
		return
	}
	s.audit(r, "xray_restart", "xray", "用户触发重启")
	ok(w, map[string]any{"running": s.xr.Running()})
}

type bbrReq struct {
	Enabled *bool  `json:"enabled"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleControlBBR(w http.ResponseWriter, r *http.Request) {
	var req bbrReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if req.Confirm != "BBR" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=BBR")
		return
	}
	if req.Enabled == nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "缺少 enabled 字段")
		return
	}
	if runtime.GOOS != "linux" {
		fail(w, http.StatusNotImplemented, codeBadRequest, "BBR 只能在 Linux 上配置")
		return
	}

	view, err := tune.SetBBR(*req.Enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, codeBadRequest, err.Error(), map[string]any{"bbr": view})
		return
	}
	s.audit(r, "bbr_toggle", "sysctl", fmt.Sprintf("enabled=%v", *req.Enabled))
	ok(w, map[string]any{"bbr": view})
}

// ---------- 访问地址 ----------

// accessHosts 收集可用于展示的访问主机名。请求 Host 优先，其次是本机网卡地址。
func (s *Server) accessHosts(r *http.Request) []string {
	hosts := []string{}
	if h := s.publicHost(r); h != "" && h != "127.0.0.1" && h != "localhost" && h != "::1" {
		hosts = append(hosts, h)
	}
	hosts = append(hosts, manage.LocalIPv4s()...)
	return hosts
}

func systemTimezone() string {
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	return time.Local.String()
}

func firstLine(out, fallback string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return fallback
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return strings.TrimSpace(out)
}

// restartPanelSoon 延迟重启面板自身：先把 HTTP 响应发出去，再让 systemd 重启。
func (s *Server) restartPanelSoon(delay time.Duration) {
	s.restartMu.Lock()
	if s.restarting {
		s.restartMu.Unlock()
		return
	}
	s.restarting = true
	s.restartMu.Unlock()

	go func() {
		time.Sleep(delay)
		// --no-block：把重启任务交给 systemd 后立刻返回。这个进程本身就在被重启的
		// 单元里，等任务完成只会等到自己被 SIGTERM 杀掉。
		if err := exec.Command("systemctl", "--no-block", "restart", "panelvps").Run(); err != nil {
			s.logger.Error("重启面板失败", "err", err)
			s.restartMu.Lock()
			s.restarting = false
			s.restartMu.Unlock()
		}
	}()
}
