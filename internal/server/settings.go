package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/manage"
	"github.com/Everything-is-one/panelvps/internal/notify"
	"github.com/Everything-is-one/panelvps/internal/selfupdate"
	"github.com/Everything-is-one/panelvps/internal/version"
)

type updateInfo struct {
	*selfupdate.Info
	CheckedAt time.Time `json:"checked_at"`
	Repo      string    `json:"repo"`
}

func (s *Server) settingOr(ctx context.Context, key, def string) string {
	v, err := s.st.GetSetting(ctx, key)
	if err != nil || v == "" {
		return def
	}
	return v
}

// loadRuntimeSettings 把设置表里的运行期开关套到内存。
//
// 这些值原先只在设置页展示、保存后改内存，重启就从配置文件回默认值——
// 用户改过的阈值重启后悄悄变回去，比没有这个开关更糟。启动时统一回填一次。
func (s *Server) loadRuntimeSettings(ctx context.Context) {
	if v, err := s.st.GetSetting(ctx, "ssh_guard_enabled"); err == nil && v != "" {
		s.cfg.SSHGuardEnabled = v == "1"
	}
	if v, err := s.st.GetSetting(ctx, "ssh_guard_threshold"); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= 3 && n <= 100 {
			s.cfg.SSHGuardThreshold = n
		}
	}
	if v, err := s.st.GetSetting(ctx, "ssh_guard_perma_threshold"); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.permaThreshold.Store(int32(n))
		}
	}
	if v, err := s.st.GetSetting(ctx, "geoip"); err == nil && v != "" {
		s.geoOn.Store(v == "1")
	}
}

func boolFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	guardOn := s.cfg.SSHGuardEnabled
	if v, err := s.st.GetSetting(ctx, "ssh_guard_enabled"); err == nil && v != "" {
		guardOn = v == "1"
	}
	threshold := s.cfg.SSHGuardThreshold
	if v, err := s.st.GetSetting(ctx, "ssh_guard_threshold"); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			threshold = n
		}
	}
	// 访问方式与账号原先在「控制」页，那里为了拿 BBR 状态要跑 modprobe，
	// 整个页面要等一两秒。设置页不需要 BBR，这里只算地址。
	listenPort := 0
	if _, p, err := net.SplitHostPort(s.cfg.Listen); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			listenPort = n
		}
	}
	public := !s.cfg.IsLoopback()
	access := map[string]any{
		"port":      listenPort,
		"public":    public,
		"listen":    s.cfg.Listen,
		"urls":      manage.AccessURLs(s.accessHosts(r), listenPort, public),
		"local_ips": manage.LocalIPv4s(),
	}
	account := map[string]any{}
	if u, _, err := s.currentUser(r); err == nil && u != nil {
		v := toUserView(u)
		account = map[string]any{
			"username":      v.Username,
			"last_login_at": v.LastLoginAt,
			"last_login_ip": v.LastLoginIP,
		}
	}

	ok(w, map[string]any{
		"public_host":   s.publicHostSetting(r),
		"alert_webhook": s.cfg.AlertWebhook,
		"access_path":   s.currentAccessPath(),
		"geoip":         s.geoOn.Load(),
		"access":        access,
		"account":       account,
		"ssh_guard": map[string]any{
			"enabled":         guardOn,
			"threshold":       threshold,
			"perma_threshold": int(s.permaThreshold.Load()),
			"window":          s.cfg.SSHGuardWindow,
			"ban":             s.cfg.SSHGuardBan,
			"ssh_ports":       s.effectiveSSHPorts(),
		},
		"panel": map[string]any{
			"version":          version.Version,
			"listen":           s.cfg.Listen,
			"loopback":         s.cfg.IsLoopback(),
			"data_dir":         s.cfg.DataDir,
			"db_path":          s.cfg.DBPath,
			"file_root":        s.cfg.FileRoot,
			"sites_root":       s.cfg.SitesRoot,
			"nginx_conf":       s.cfg.NginxConfDir,
			"backup_dir":       s.cfg.BackupDir,
			"cert_email":       s.cfg.CertEmail,
			"terminal_enabled": s.cfg.TerminalEnabled,
			"update_repo":      s.cfg.UpdateRepo,
			"config_path":      s.cfg.Path(),
		},
		"notify_enabled": s.notifier != nil && s.notifier.Enabled(),
		"update":         s.cachedUpdate(),
	})
}

type settingsReq struct {
	PublicHost      *string `json:"public_host"`
	AlertWebhook    *string `json:"alert_webhook"`
	SSHGuard        *bool   `json:"ssh_guard_enabled"`
	SSHThreshold    *int    `json:"ssh_guard_threshold"`
	SSHPermaThresh  *int    `json:"ssh_guard_perma_threshold"`
	TerminalEnabled *bool   `json:"terminal_enabled"`
	// AccessPath 访问后缀，空字符串表示关闭。
	AccessPath *string `json:"access_path"`
	GeoIP      *bool   `json:"geoip"`
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var req settingsReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	ctx := r.Context()
	changed := []string{}

	if req.PublicHost != nil {
		host := strings.TrimSpace(*req.PublicHost)
		if err := s.checkPublicHost(host); err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		if err := s.st.SetSetting(ctx, "public_host", host); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
			return
		}
		changed = append(changed, "public_host")
	}

	if req.AlertWebhook != nil {
		raw := strings.TrimSpace(*req.AlertWebhook)
		n, err := notify.New(raw, s.logger)
		if err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.notifier = n
		s.cfg.AlertWebhook = raw
		if err := s.st.SetSetting(ctx, "alert_webhook", raw); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
			return
		}
		changed = append(changed, "alert_webhook")
	}

	if req.SSHGuard != nil {
		v := "0"
		if *req.SSHGuard {
			v = "1"
		}
		s.cfg.SSHGuardEnabled = *req.SSHGuard
		if err := s.st.SetSetting(ctx, "ssh_guard_enabled", v); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
			return
		}
		changed = append(changed, "ssh_guard_enabled")
	}

	if req.SSHThreshold != nil {
		n := *req.SSHThreshold
		if n < 3 || n > 100 {
			fail(w, http.StatusBadRequest, codeBadRequest, "失败次数阈值应在 3–100 之间")
			return
		}
		s.cfg.SSHGuardThreshold = n
		if err := s.st.SetSetting(ctx, "ssh_guard_threshold", strconv.Itoa(n)); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
			return
		}
		changed = append(changed, "ssh_guard_threshold")
	}

	if req.SSHPermaThresh != nil {
		n := *req.SSHPermaThresh
		// 0 表示关闭永久封禁；不为负，也不允许低于普通阈值（那样所有人都是永久）
		if n < 0 || n > 100000 {
			fail(w, http.StatusBadRequest, codeBadRequest, "永久封禁阈值不合法")
			return
		}
		if n > 0 && n < s.cfg.SSHGuardThreshold {
			fail(w, http.StatusBadRequest, codeBadRequest, "永久封禁阈值不能小于普通阈值")
			return
		}
		s.permaThreshold.Store(int32(n))
		if err := s.st.SetSetting(ctx, "ssh_guard_perma_threshold", strconv.Itoa(n)); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
			return
		}
		changed = append(changed, "ssh_guard_perma_threshold")
	}

	// 访问后缀会改变所有页面的入口，必须写回配置文件，否则重启就丢。
	if req.AccessPath != nil {
		v := config.NormalizeAccessPath(*req.AccessPath)
		if err := config.ValidateAccessPath(v); err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		if s.cfg.Path() == "" {
			fail(w, http.StatusInternalServerError, codeInternalError, "无法定位配置文件")
			return
		}
		// 用 %q 写成带引号的 YAML 标量：空串也写成 ""，语义明确
		if err := config.PatchScalar(s.cfg.Path(), "access_path", strconv.Quote(v)); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "写入配置文件失败: "+err.Error())
			return
		}
		s.setAccessPath(v)
		changed = append(changed, "access_path")
	}

	if req.GeoIP != nil {
		s.geoOn.Store(*req.GeoIP)
		if err := s.st.SetSetting(ctx, "geoip", boolFlag(*req.GeoIP)); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
			return
		}
		changed = append(changed, "geoip")
	}

	if req.TerminalEnabled != nil {
		if s.cfg.Path() == "" {
			fail(w, http.StatusInternalServerError, codeInternalError, "无法定位配置文件")
			return
		}
		if err := config.PatchScalar(s.cfg.Path(), "terminal_enabled",
			strconv.FormatBool(*req.TerminalEnabled)); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "写入配置文件失败: "+err.Error())
			return
		}
		// 终端开关是每次请求现读 s.cfg，改完立即生效，不需要重启面板
		s.cfg.TerminalEnabled = *req.TerminalEnabled
		changed = append(changed, "terminal_enabled")
	}

	s.audit(r, "settings_update", strings.Join(changed, ","), "")
	ok(w, map[string]any{"changed": changed})
}
func (s *Server) cachedUpdate() *updateInfo {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	return s.updInfo
}

// updateCacheTTL 是自动检查的复用窗口。
//
// 打开设置页就会自动查一次，但 GitHub 未认证接口按 IP 限流（每小时 60 次），
// 多开几个标签页、来回切几次就用完了。窗口内直接返回上一次的结果，
// 手动点「检查更新」仍然会真正回源（force=true）。
const updateCacheTTL = 10 * time.Minute

// handleUpdateState 只读缓存，不发网络请求。
// 前端启动时先问一次，有结果就先画出来，避免设置页空着等人点。
func (s *Server) handleUpdateState(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"update": s.cachedUpdate()})
}

type updateCheckReq struct {
	Force bool `json:"force"`
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	var req updateCheckReq
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
			return
		}
	}
	if !req.Force {
		if cached := s.cachedUpdate(); cached != nil && !cached.CheckedAt.IsZero() &&
			time.Since(cached.CheckedAt) < updateCacheTTL {
			ok(w, cached)
			return
		}
	}
	// 自动检查是页面打开时顺手做的，不能让用户对着「检查中…」等 20 秒。
	// 手动检查保持原来的耐心，网络慢时也能等到结果。
	ctx := r.Context()
	if !req.Force {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
	}
	info, err := selfupdate.Check(ctx, s.cfg.UpdateRepo, version.Version)
	if err != nil {
		msg := "检查更新失败: " + err.Error()
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") {
			msg = "检查更新失败：连接 GitHub 超时，服务器可能无法访问 api.github.com"
		}
		fail(w, http.StatusBadGateway, codeBadRequest, msg)
		return
	}
	cached := &updateInfo{Info: info, CheckedAt: time.Now(), Repo: s.cfg.UpdateRepo}
	s.updMu.Lock()
	s.updInfo = cached
	s.updMu.Unlock()
	s.audit(r, "update_check", s.cfg.UpdateRepo, info.Latest)
	ok(w, cached)
}

type updateApplyReq struct {
	Tag string `json:"tag"`
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "任务执行器不可用")
		return
	}
	var req updateApplyReq
	_ = decodeJSON(w, r, &req)

	cached := s.cachedUpdate()
	if cached == nil || cached.Info == nil || !cached.Available {
		fail(w, http.StatusBadRequest, codeBadRequest, "请先检查更新，且确认有新版本可用")
		return
	}
	if req.Tag != "" && req.Tag != cached.Latest {
		fail(w, http.StatusBadRequest, codeBadRequest, "版本不一致，请重新检查更新")
		return
	}

	info := cached.Info
	target, err := os.Executable()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "无法定位当前二进制: "+err.Error())
		return
	}

	job, err := s.jobs.Start("update", info.Latest, func(ctx context.Context, log func(string)) error {
		tmp, err := os.MkdirTemp("", "panelvps-update-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		newBin := filepath.Join(tmp, info.AssetName)

		log("下载 " + info.AssetName)
		if err := selfupdate.Download(ctx, info.AssetURL, newBin); err != nil {
			return err
		}
		if info.ChecksumURL != "" {
			log("校验 sha256")
			okSum, err := selfupdate.Verify(ctx, info.ChecksumURL, info.AssetName, newBin)
			var skip *selfupdate.ChecksumSkip
			switch {
			case errors.As(err, &skip):
				// 校验和拿不到不该把人卡在旧版本上：来源仍是 GitHub 的 https，
				// 后面 Apply 还会检查 ELF 头。真算出来对不上才中止。
				log(skip.Reason + "，已跳过校验")
			case err != nil:
				return err
			case !okSum:
				log("发布未提供可用的校验和，已跳过")
			}
		} else {
			log("发布未附带 checksums.txt，跳过校验（仍会检查 ELF 头）")
		}

		backup, err := selfupdate.Apply(newBin, target)
		if err != nil {
			return err
		}
		log("已替换二进制，旧版本保留在 " + backup)
		log("请点击「重启面板」完成切换")
		return nil
	})
	if err != nil {
		fail(w, http.StatusTooManyRequests, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "update_apply", info.Latest, info.AssetName)
	ok(w, job)
}
