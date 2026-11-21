package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/version"
)

type userView struct {
	ID                 int64      `json:"id"`
	Username           string     `json:"username"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	LastLoginIP        string     `json:"last_login_ip,omitempty"`
}

func toUserView(u *store.User) *userView {
	v := &userView{
		ID:                 u.ID,
		Username:           u.Username,
		MustChangePassword: u.MustChangePassword,
		LastLoginIP:        u.LastLoginIP,
	}
	if u.LastLoginAt.Valid {
		t := u.LastLoginAt.Time
		v.LastLoginAt = &t
	}
	return v
}

// handleHealth 供 systemd/外部探活使用，不需要登录。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := s.metrics.Collect()
	ok(w, map[string]any{
		"status":     "ok",
		"version":    version.Version,
		"uptime_sec": int(time.Since(s.startAt).Seconds()),
		"panel_rss":  snap.PanelRSS,
		"time":       time.Now().Format(time.RFC3339),
	})
}

// handleAuthState 返回前端启动所需的最小状态：是否初始化、是否已登录、CSRF 令牌。
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	token, _ := r.Context().Value(csrfCtxKey).(string)

	n, err := s.st.CountUsers(r.Context())
	if err != nil {
		s.logger.Error("查询账号数量失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "内部错误")
		return
	}

	resp := map[string]any{
		"initialized":   n > 0,
		"authenticated": false,
		"csrf":          token,
	}

	if u, _, err := s.currentUser(r); err == nil {
		resp["authenticated"] = true
		resp["user"] = toUserView(u)
	}
	ok(w, resp)
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "用户名和密码不能为空")
		return
	}

	ip := clientIP(r)
	if d := s.limiter.Blocked(ip); d > 0 {
		fail(w, http.StatusTooManyRequests, codeTooManyTries,
			fmt.Sprintf("尝试过于频繁，请在 %s后重试", humanDuration(d)))
		return
	}

	u, err := s.st.UserByUsername(r.Context(), req.Username)
	if err != nil {
		// 用户不存在时也跑一次哈希，避免通过响应时间判断账号是否存在
		_, _ = auth.VerifyPassword(req.Password, s.dummyPwd)
		s.onLoginFail(w, r, req.Username, ip, "账号或密码错误")
		return
	}

	passOK, err := auth.VerifyPassword(req.Password, u.PasswordHash)
	if err != nil {
		s.logger.Error("密码哈希校验失败", "user", u.Username, "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "内部错误")
		return
	}
	if !passOK {
		s.onLoginFail(w, r, req.Username, ip, "账号或密码错误")
		return
	}

	ttl, err := s.cfg.SessionTTLDuration()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "配置错误")
		return
	}

	token, hash, err := auth.NewSessionToken()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "内部错误")
		return
	}
	if err := s.st.CreateSession(r.Context(), hash, u.ID, ip, r.UserAgent(), time.Now().Add(ttl)); err != nil {
		s.logger.Error("创建会话失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "内部错误")
		return
	}

	s.limiter.Reset(ip)
	s.setSessionCookie(w, r, token, ttl)
	_ = s.st.TouchLogin(r.Context(), u.ID, ip)
	_ = s.st.Audit(r.Context(), u.Username, "login_success", "panel", store.TrimDetail(r.UserAgent()), ip)

	ok(w, map[string]any{
		"user":                 toUserView(u),
		"must_change_password": u.MustChangePassword,
	})
}

func (s *Server) onLoginFail(w http.ResponseWriter, r *http.Request, username, ip, msg string) {
	blocked := s.limiter.Fail(ip)
	_ = s.st.Audit(r.Context(), username, "login_failed", "panel", "密码错误", ip)
	if blocked {
		_ = s.st.Audit(r.Context(), username, "login_blocked", "panel", "连续失败触发封禁", ip)
	}
	fail(w, http.StatusUnauthorized, codeUnauthorized, msg)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	sess := sessionFrom(r.Context())
	if sess != nil {
		_ = s.st.DeleteSession(r.Context(), sess.ID)
	}
	s.clearSessionCookie(w)
	if u != nil {
		_ = s.st.Audit(r.Context(), u.Username, "logout", "panel", "", clientIP(r))
	}
	ok(w, map[string]any{"ok": true})
}

type changePasswordReq struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}

	u := userFrom(r.Context())
	if u == nil {
		fail(w, http.StatusUnauthorized, codeUnauthorized, "未登录")
		return
	}

	passOK, err := auth.VerifyPassword(req.Current, u.PasswordHash)
	if err != nil || !passOK {
		_ = s.st.Audit(r.Context(), u.Username, "password_change_failed", "user", "当前密码错误", clientIP(r))
		fail(w, http.StatusBadRequest, codeBadRequest, "当前密码不正确")
		return
	}

	if err := auth.ValidatePassword(req.New, u.Username); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	hash, err := auth.HashPassword(req.New)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	if err := s.st.UpdatePassword(r.Context(), u.ID, hash); err != nil {
		s.logger.Error("更新密码失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "内部错误")
		return
	}

	// 改密后踢掉所有旧会话，再为当前浏览器建立新会话
	_ = s.st.DeleteUserSessions(r.Context(), u.ID)
	ttl, _ := s.cfg.SessionTTLDuration()
	token, tokenHash, err := auth.NewSessionToken()
	if err == nil {
		if err := s.st.CreateSession(r.Context(), tokenHash, u.ID, clientIP(r), r.UserAgent(), time.Now().Add(ttl)); err == nil {
			s.setSessionCookie(w, r, token, ttl)
		}
	}

	_ = s.st.Audit(r.Context(), u.Username, "password_changed", "user", "密码已更新，其他会话已失效", clientIP(r))
	ok(w, map[string]any{"ok": true})
}

// handleSystemStat 返回首页需要的系统指标与面板信息。
func (s *Server) handleSystemStat(w http.ResponseWriter, r *http.Request) {
	snap := s.metrics.Collect()
	sess := sessionFrom(r.Context())

	auditCount, err := s.st.AuditCount(r.Context())
	if err != nil {
		s.logger.Warn("读取审计条数失败", "err", err)
	}

	var expires *time.Time
	if sess != nil {
		t := sess.ExpiresAt
		expires = &t
	}

	ok(w, map[string]any{
		"metrics":            snap,
		"listen":             s.cfg.Listen,
		"loopback":           s.cfg.IsLoopback(),
		"data_dir":           s.cfg.DataDir,
		"db_path":            s.cfg.DBPath,
		"audit_count":        auditCount,
		"version":            version.Version,
		"session_expires_at": expires,
	})
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}
