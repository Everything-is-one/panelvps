package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
	"github.com/Everything-is-one/panelvps/internal/version"
)

const (
	bbrConfPath = "/etc/sysctl.d/99-panelvps.conf"
	bbrUnitName = "panelvps-sysctl.service"
	bbrUnitPath = "/etc/systemd/system/" + bbrUnitName
	bbrMarker   = "# Managed by PanelVPS"
)

type bbrView struct {
	Supported     bool     `json:"supported"`
	Enabled       bool     `json:"enabled"`
	Current       string   `json:"current"`
	Qdisc         string   `json:"qdisc"`
	Available     []string `json:"available"`
	Reason        string   `json:"reason,omitempty"`
	ConfigPath    string   `json:"config_path"`
	ConfigExists  bool     `json:"config_exists"`
	UnitInstalled bool     `json:"unit_installed"`
}

func (s *Server) handleControlStatus(w http.ResponseWriter, r *http.Request) {
	port := s.panelPort()
	public := !s.cfg.IsLoopback()
	urls := accessURLs(s.accessHosts(r), port, public)

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
			"local_ips":   localIPv4s(),
			"public_host": s.publicHost(r),
			"version":     version.Version,
			"config_path": s.cfg.Path(),
			"uptime_sec":  int(time.Since(s.startAt).Seconds()),
		},
		"account": account,
		"bbr":     currentBBR(),
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
	if req.Port < 1 || req.Port > 65535 {
		fail(w, http.StatusBadRequest, codeBadRequest, "端口应在 1–65535 之间")
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "当前环境没有 systemd，修改后无法自动重启")
		return
	}

	oldListen := s.cfg.Listen
	_, oldPortStr, err := net.SplitHostPort(oldListen)
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "当前监听地址不合法: "+oldListen)
		return
	}
	oldPort, _ := strconv.Atoi(oldPortStr)

	host := "127.0.0.1"
	if req.Public {
		host = "0.0.0.0"
	}
	newListen := net.JoinHostPort(host, strconv.Itoa(req.Port))
	if newListen == oldListen {
		ok(w, map[string]any{
			"listen":     newListen,
			"port":       req.Port,
			"public":     req.Public,
			"restarting": false,
			"urls":       accessURLs(s.accessHosts(r), req.Port, req.Public),
			"warnings":   []string{},
		})
		return
	}

	// 换端口时先试绑一次，避免保存完配置才发现端口被占用。
	if req.Port != oldPort {
		ln, err := net.Listen("tcp", newListen)
		if err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest,
				fmt.Sprintf("端口 %d 不可用：%v", req.Port, err))
			return
		}
		_ = ln.Close()
	}

	cfgPath := s.cfg.Path()
	if cfgPath == "" {
		fail(w, http.StatusInternalServerError, codeInternalError, "无法定位配置文件")
		return
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取配置失败: "+err.Error())
		return
	}
	cfg.Listen = newListen
	if err := cfg.Save(cfgPath); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "保存配置失败: "+err.Error())
		return
	}

	warnings := []string{}
	if req.Port < 1024 {
		warnings = append(warnings, "端口低于 1024，需要 root 才能绑定；面板以 root 运行，通常可用")
	}

	// 面板端口变化后，已下发的防火墙规则必须同步，否则会把自己关在门外。
	rows, listErr := s.st.ListFirewallRules(r.Context())
	fw := firewall.Detect()
	if listErr == nil && (len(rows) > 0 || fileExists(firewall.PersistPath())) {
		if !fw.Supported {
			warnings = append(warnings, "防火墙后端当前不可用，规则未同步；请确认新端口在云安全组中已放行")
		} else {
			opt := firewall.ApplyOptions{
				Rules:         toFirewallRules(rows),
				SSHPorts:      s.effectiveSSHPorts(),
				PanelPort:     req.Port,
				PanelLoopback: !req.Public,
			}
			if _, err := firewall.Apply(opt); err != nil {
				// 回滚配置，避免重启后落在「新端口没放行、旧端口已关闭」的状态。
				cfg.Listen = oldListen
				_ = cfg.Save(cfgPath)
				fail(w, http.StatusBadRequest, codeBadRequest, "防火墙规则下发失败，已取消修改: "+err.Error())
				return
			}
		}
	}

	s.audit(r, "control_access", newListen, fmt.Sprintf("public=%v port=%d", req.Public, req.Port))
	s.restartPanelSoon(1200 * time.Millisecond)

	ok(w, map[string]any{
		"listen":     newListen,
		"port":       req.Port,
		"public":     req.Public,
		"restarting": true,
		"urls":       accessURLs(s.accessHosts(r), req.Port, req.Public),
		"warnings":   warnings,
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

	view, err := setBBR(*req.Enabled)
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
	hosts = append(hosts, localIPv4s()...)
	return hosts
}

func accessURLs(hosts []string, port int, public bool) []string {
	if !public {
		return []string{fmt.Sprintf("http://127.0.0.1:%d", port)}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, fmt.Sprintf("http://%s:%d", h, port))
	}
	return out
}

func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	set := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			set[ip.String()] = true
		}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
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

// ---------- BBR ----------

func currentBBR() bbrView {
	v := bbrView{
		ConfigPath: bbrConfPath,
		Available:  []string{},
	}
	if runtime.GOOS != "linux" {
		v.Reason = "当前系统不是 Linux，BBR 配置仅在服务器上可用"
		return v
	}
	v.Current = readTrim("/proc/sys/net/ipv4/tcp_congestion_control")
	v.Qdisc = readTrim("/proc/sys/net/core/default_qdisc")
	v.Available = strings.Fields(readTrim("/proc/sys/net/ipv4/tcp_available_congestion_control"))
	v.Enabled = v.Current == "bbr"
	v.ConfigExists = fileExists(bbrConfPath)
	v.UnitInstalled = fileExists(bbrUnitPath)

	switch {
	case slices.Contains(v.Available, "bbr") || v.Current == "bbr":
		v.Supported = true
	case exec.Command("modprobe", "-n", "tcp_bbr").Run() == nil:
		// 模块存在但尚未加载；开启时会由 helper 负责 modprobe。
		v.Supported = true
		v.Reason = "内核提供 tcp_bbr 模块，开启时自动加载"
	default:
		v.Reason = "内核未提供 tcp_bbr 模块（或 modprobe 不可用）"
	}
	return v
}

func setBBR(enable bool) (bbrView, error) {
	before := currentBBR()
	if enable && !before.Supported {
		return before, errors.New("当前内核不支持 BBR：" + before.Reason)
	}

	prev, prevErr := os.ReadFile(bbrConfPath)
	if prevErr == nil && !strings.Contains(string(prev), bbrMarker) {
		return before, fmt.Errorf("%s 已存在且不是面板生成的，请手动处理后再试", bbrConfPath)
	}
	if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
		return before, fmt.Errorf("读取 %s 失败: %w", bbrConfPath, prevErr)
	}

	body := bbrMarker + "\nnet.ipv4.tcp_congestion_control=cubic\n"
	if enable {
		body = bbrMarker + "\nnet.core.default_qdisc=fq\nnet.ipv4.tcp_congestion_control=bbr\n"
	}
	if err := os.MkdirAll(filepath.Dir(bbrConfPath), 0o755); err != nil {
		return before, fmt.Errorf("创建 sysctl 目录失败: %w", err)
	}
	if err := writeFileAtomic(bbrConfPath, []byte(body), 0o644); err != nil {
		return before, fmt.Errorf("写入 sysctl 配置失败（安装脚本可能未放行 /etc/sysctl.d）: %w", err)
	}

	if err := applySysctl(); err != nil {
		// 回滚文件；已经生效的内核参数留到下次重启自然恢复。
		if prevErr == nil {
			_ = writeFileAtomic(bbrConfPath, prev, 0o644)
		} else {
			_ = os.Remove(bbrConfPath)
		}
		return currentBBR(), err
	}

	after := currentBBR()
	if enable && after.Current != "bbr" {
		return after, fmt.Errorf("配置已写入，但内核当前仍是 %s", after.Current)
	}
	if !enable && after.Current == "bbr" {
		return after, errors.New("配置已写入，但内核仍在使用 bbr")
	}
	return after, nil
}

// applySysctl 通过特权 helper 应用配置。
//
// 主服务开启了 ProtectSystem / ProtectKernelTunables，自己写 /proc/sys 会被拒绝；
// helper 是独立的 oneshot 单元，只执行 modprobe 与 sysctl -p 本文件。
func applySysctl() error {
	if fileExists(bbrUnitPath) {
		out, err := exec.Command("systemctl", "restart", bbrUnitName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("应用 sysctl 失败: %s", firstLine(string(out), err.Error()))
		}
		return nil
	}
	// 没有 helper（例如手工在终端里跑二进制）时直接应用。
	p, err := exec.LookPath("sysctl")
	if err != nil {
		return errors.New("系统没有 sysctl，且未安装 panelvps-sysctl.service")
	}
	_ = exec.Command("modprobe", "tcp_bbr").Run()
	out, err := exec.Command(p, "-p", bbrConfPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("应用 sysctl 失败: %s", firstLine(string(out), err.Error()))
	}
	return nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".panelvps-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
