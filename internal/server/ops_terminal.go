package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/terminal"
)

const maxTerminalSessions = 2

// handleTerminalTicket 发放一次性连接票据。
//
// 用票据而不是把会话 Cookie 放进 WebSocket URL：URL 会进访问日志与
// 浏览器历史，票据两分钟失效且只能使用一次。
func (s *Server) handleTerminalTicket(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.TerminalEnabled {
		fail(w, http.StatusForbidden, codeBadRequest,
			"Web 终端未启用：请在配置中设置 terminal_enabled: true 后重启面板")
		return
	}
	if !terminal.Supported() {
		fail(w, http.StatusBadRequest, codeBadRequest, terminal.ErrUnsupported.Error())
		return
	}

	ticket := randomHex(24)
	if ticket == "" {
		fail(w, http.StatusInternalServerError, codeInternalError, "生成票据失败")
		return
	}
	now := time.Now()
	s.termMu.Lock()
	for k, exp := range s.termTicket {
		if now.After(exp) {
			delete(s.termTicket, k)
		}
	}
	s.termTicket[ticket] = now.Add(2 * time.Minute)
	s.termMu.Unlock()

	ok(w, map[string]any{"ticket": ticket, "expires_in": 120})
}

func (s *Server) consumeTicket(ticket string) bool {
	if ticket == "" {
		return false
	}
	now := time.Now()
	s.termMu.Lock()
	defer s.termMu.Unlock()
	exp, ok2 := s.termTicket[ticket]
	if !ok2 {
		return false
	}
	delete(s.termTicket, ticket) // 一次性
	return now.Before(exp)
}

var termUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 8192,
	// 只接受同源页面发起的连接，防止跨站 WebSocket 劫持
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // 非浏览器客户端（如 curl）没有 Origin
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	},
}

type termMsg struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// handleTerminalWS 处理 WebSocket 终端。
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.TerminalEnabled {
		http.Error(w, "terminal disabled", http.StatusForbidden)
		return
	}
	if !terminal.Supported() {
		http.Error(w, "terminal unsupported", http.StatusBadRequest)
		return
	}
	if !s.consumeTicket(r.URL.Query().Get("ticket")) {
		http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
		return
	}

	s.termMu.Lock()
	if s.termActive >= maxTerminalSessions {
		s.termMu.Unlock()
		http.Error(w, "too many terminal sessions", http.StatusTooManyRequests)
		return
	}
	s.termActive++
	s.termMu.Unlock()
	defer func() {
		s.termMu.Lock()
		s.termActive--
		s.termMu.Unlock()
	}()

	cols := parseUint16(r.URL.Query().Get("cols"), 120)
	rows := parseUint16(r.URL.Query().Get("rows"), 32)

	conn, err := termUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)

	sess, err := terminal.Start(s.cfg.TerminalShell, cols, rows)
	if err != nil {
		_ = conn.WriteJSON(map[string]any{"type": "error", "data": err.Error()})
		return
	}
	defer sess.Close()

	user := "-"
	if u := userFrom(r.Context()); u != nil {
		user = u.Username
	}
	_ = s.st.Audit(r.Context(), user, "terminal_open", "shell", s.cfg.TerminalShell, clientIP(r))
	defer func() {
		_ = s.st.Audit(r.Context(), user, "terminal_close", "shell", "", clientIP(r))
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 8192)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "exit"})
				return
			}
		}
	}()

	conn.SetReadDeadline(time.Now().Add(30 * time.Minute))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(30 * time.Minute))
	})

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Minute))
		if mt == websocket.BinaryMessage {
			_, _ = sess.Write(data)
			continue
		}
		var msg termMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "input":
			_, _ = sess.Write([]byte(msg.Data))
		case "resize":
			_ = sess.Resize(msg.Cols, msg.Rows)
		case "ping":
			// 客户端心跳，忽略
		}
	}
}

func parseUint16(s string, def uint16) uint16 {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 60000 {
		return def
	}
	return uint16(n)
}

// handleAuditList 供未来「审计日志」页面使用。
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB().QueryContext(r.Context(),
		`SELECT id, username, action, target, detail, ip, created_at
		   FROM audit_logs ORDER BY id DESC LIMIT 200`)
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取审计失败")
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, created int64
		var user, action, target, detail, ip string
		if err := rows.Scan(&id, &user, &action, &target, &detail, &ip, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "username": user, "action": action, "target": target,
			"detail": store.TrimDetail(detail), "ip": ip,
			"at": time.Unix(created, 0),
		})
	}
	ok(w, map[string]any{"items": out})
}
