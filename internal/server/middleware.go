package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/store"
)

type ctxKey string

const userCtxKey ctxKey = "user"
const sessCtxKey ctxKey = "session"
const csrfCtxKey ctxKey = "csrf"

// withRecover 捕获 panic，返回 500 而不是让进程崩掉。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered",
					"path", r.URL.Path,
					"err", rec,
					"stack", string(debug.Stack()))
				fail(w, http.StatusInternalServerError, codeInternalError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withSecurityHeaders 下发安全响应头（PRD S-E09）。
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; form-action 'self'; "+
				"frame-ancestors 'none'; object-src 'none'")
		// HSTS 只在确实走 TLS 时下发，避免本地 http 调试被浏览器锁死
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// withBodyLimit 限制请求体大小，防止内存被撑爆（PRD S-E08）。
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	const maxBody = 1 << 20 // 1MB，M0 没有上传接口
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// withCSRF 实现双提交 Cookie 校验。
//
// GET 请求负责投放 Cookie；写请求必须带与 Cookie 一致的请求头。
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(csrfCookie)
		token := ""
		if err == nil {
			token = c.Value
		}

		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			if token == "" {
				token = randomHex(16)
				http.SetCookie(w, &http.Cookie{
					Name:     csrfCookie,
					Value:    token,
					Path:     "/",
					HttpOnly: false, // 前端 JS 需要读取后放进请求头
					SameSite: http.SameSiteLaxMode,
					Secure:   s.useSecureCookie(r),
					MaxAge:   12 * 3600,
				})
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfCtxKey, token)))
			return
		}

		if token == "" || r.Header.Get(csrfHeader) != token {
			fail(w, http.StatusForbidden, codeBadRequest, "CSRF 校验失败，请刷新页面重试")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfCtxKey, token)))
	})
}

// withAccessLog 输出结构化访问日志。刻意不记录查询串，避免令牌泄漏到日志。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"bytes", rw.bytes,
			"dur_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// requireAuth 包装受保护的处理器：未登录直接 401。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, sess, err := s.currentUser(r)
		if err != nil {
			fail(w, http.StatusUnauthorized, codeUnauthorized, "未登录或会话已失效")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, u)
		ctx = context.WithValue(ctx, sessCtxKey, sess)
		next(w, r.WithContext(ctx))
	}
}

// currentUser 从 Cookie 还原登录态。
func (s *Server) currentUser(r *http.Request) (*store.User, *store.Session, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil, store.ErrNotFound
	}
	sess, u, err := s.st.SessionByToken(r.Context(), auth.HashToken(c.Value))
	if err != nil {
		return nil, nil, err
	}
	return u, sess, nil
}

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userCtxKey).(*store.User)
	return u
}

func sessionFrom(ctx context.Context) *store.Session {
	sess, _ := ctx.Value(sessCtxKey).(*store.Session)
	return sess
}

// useSecureCookie 按配置与请求推断是否加 Secure 标记。
func (s *Server) useSecureCookie(r *http.Request) bool {
	switch s.cfg.SecureCookie {
	case "always":
		return true
	case "never":
		return false
	default:
		return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
}

// setSessionCookie 下发会话 Cookie。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.useSecureCookie(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientIP 取真实来源 IP。
//
// 只有当直连方是回环地址时才信任 X-Forwarded-For——否则任何客户端
// 都能伪造请求头绕过登录限流。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if net.ParseIP(first) != nil {
				return first
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(real) != nil {
			return real
		}
	}
	return host
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
