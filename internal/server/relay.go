package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Everything-is-one/panelvps/internal/relay"
	"github.com/Everything-is-one/panelvps/internal/store"
)

// ---------- 中转 ----------

type relayReq struct {
	Name       string `json:"name"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`
	Target     string `json:"target"`
	Network    string `json:"network"`
	Enabled    *bool  `json:"enabled"`
}

type relayView struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Listen     string      `json:"listen"`
	ListenPort int         `json:"listen_port"`
	Target     string      `json:"target"`
	Network    string      `json:"network"`
	Enabled    bool        `json:"enabled"`
	Stats      relay.Stats `json:"stats"`
}

func (s *Server) handleRelayList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListRelays(r.Context())
	if err != nil {
		s.logger.Error("读取中转规则失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "读取中转规则失败")
		return
	}
	stats := s.relays.Snapshot()
	views := make([]relayView, 0, len(rows))
	for _, it := range rows {
		views = append(views, relayView{
			ID: it.ID, Name: it.Name, Listen: it.Listen, ListenPort: it.ListenPort,
			Target: it.Target, Network: it.Network, Enabled: it.Enabled,
			Stats: stats[it.ID],
		})
	}
	ok(w, map[string]any{
		"rules":    views,
		"networks": []string{"tcp", "udp", "both"},
	})
}

func (s *Server) handleRelayCreate(w http.ResponseWriter, r *http.Request) {
	var req relayReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}
	rule, err := s.buildRelay(r.Context(), req, nil)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	id, err := s.st.CreateRelay(r.Context(), rule)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "创建失败: "+err.Error())
		return
	}
	s.audit(r, "relay_create", rule.Name, fmt.Sprintf("%s -> %s", relayAddr(rule), rule.Target))
	s.reloadRelays(r.Context())
	ok(w, map[string]any{"id": id})
}

func (s *Server) handleRelayUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "规则 ID 非法")
		return
	}
	cur, err := s.st.RelayByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "规则不存在")
		return
	}
	var req relayReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}
	next, err := s.buildRelay(r.Context(), req, cur)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	next.ID = id
	if err := s.st.UpdateRelay(r.Context(), next); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "更新失败: "+err.Error())
		return
	}
	s.audit(r, "relay_update", next.Name, fmt.Sprintf("%s -> %s", relayAddr(next), next.Target))
	s.reloadRelays(r.Context())
	ok(w, map[string]any{"id": id})
}

func (s *Server) handleRelayDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "规则 ID 非法")
		return
	}
	cur, err := s.st.RelayByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "规则不存在")
		return
	}
	if err := s.st.DeleteRelay(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "删除失败")
		return
	}
	s.audit(r, "relay_delete", cur.Name, relayAddr(cur))
	s.reloadRelays(r.Context())
	ok(w, nil)
}

func (s *Server) handleRelayToggle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "规则 ID 非法")
		return
	}
	cur, err := s.st.RelayByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "规则不存在")
		return
	}
	cur.Enabled = !cur.Enabled
	if err := s.st.UpdateRelay(r.Context(), cur); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "更新失败")
		return
	}
	s.audit(r, "relay_toggle", cur.Name, fmt.Sprintf("enabled=%v", cur.Enabled))
	s.reloadRelays(r.Context())
	ok(w, map[string]any{"enabled": cur.Enabled})
}

// reloadRelays 把库里的规则同步到转发引擎。
//
// 所有写操作之后都调它，而不是在 handler 里直接起停单条：数据库才是唯一
// 事实来源，任何"只改内存"的分支迟早会和库里对不上。
func (s *Server) reloadRelays(ctx context.Context) {
	rows, err := s.st.ListRelays(ctx)
	if err != nil {
		s.logger.Error("同步中转规则失败", "err", err)
		return
	}
	rules := make([]relay.Rule, 0, len(rows))
	for _, it := range rows {
		rules = append(rules, relay.Rule{
			ID: it.ID, Name: it.Name, Listen: it.Listen, ListenPort: it.ListenPort,
			Target: it.Target, Network: it.Network, Enabled: it.Enabled,
		})
	}
	s.relays.Apply(rules)
}

func (s *Server) buildRelay(ctx context.Context, req relayReq, prev *store.Relay) (*store.Relay, error) {
	out := &store.Relay{Listen: "0.0.0.0", Network: "tcp", Enabled: true}
	if prev != nil {
		*out = *prev
	}

	if name := strings.TrimSpace(req.Name); name != "" || prev == nil {
		out.Name = name
	}
	if listen := strings.TrimSpace(req.Listen); listen != "" {
		out.Listen = listen
	}
	if out.Listen == "" {
		out.Listen = "0.0.0.0"
	}
	if net.ParseIP(out.Listen) == nil {
		return nil, fmt.Errorf("监听地址必须是 IP（0.0.0.0 表示全部）")
	}

	if req.ListenPort != 0 {
		out.ListenPort = req.ListenPort
	}
	if out.ListenPort < 1 || out.ListenPort > 65535 {
		return nil, fmt.Errorf("监听端口必须在 1–65535 之间")
	}

	if target := strings.TrimSpace(req.Target); target != "" || prev == nil {
		out.Target = target
	}
	host, port, err := net.SplitHostPort(out.Target)
	if err != nil || host == "" {
		return nil, fmt.Errorf("目标要写成 主机:端口，例如 1.2.3.4:8443")
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("目标端口必须在 1–65535 之间")
	}

	if nw := strings.ToLower(strings.TrimSpace(req.Network)); nw != "" {
		switch nw {
		case "tcp+udp", "udp+tcp", "all":
			nw = "both"
		}
		out.Network = nw
	}
	switch out.Network {
	case "tcp", "udp", "both":
	default:
		return nil, fmt.Errorf("协议只支持 tcp / udp / both")
	}

	if req.Enabled != nil {
		out.Enabled = *req.Enabled
	}

	// 端口冲突：面板自身、Xray 内部接口、已装软件占的端口这里不查，
	// 但要挡住面板端口、Xray API 与自己的其它规则——这三个是最常见的
	// "面板突然打不开"和"转发时好时坏"的来源。
	if _, p, err := net.SplitHostPort(s.cfg.Listen); err == nil {
		if v, _ := strconv.Atoi(p); v == out.ListenPort {
			return nil, fmt.Errorf("端口 %d 已被面板自身占用", out.ListenPort)
		}
	}
	if out.ListenPort == s.cfg.XrayAPIPort {
		return nil, fmt.Errorf("端口 %d 是 Xray 内部接口端口", out.ListenPort)
	}
	exclude := int64(0)
	if prev != nil {
		exclude = prev.ID
	}
	if other, err := s.st.RelayByAddr(ctx, out.Listen, out.ListenPort, exclude); err == nil && other != nil {
		return nil, fmt.Errorf("端口 %d 已被中转规则「%s」占用", out.ListenPort, other.Name)
	}
	if in, err := s.st.InboundByPort(ctx, out.Listen, out.ListenPort, 0); err == nil && in != nil {
		return nil, fmt.Errorf("端口 %d 已被入站 %s 占用", out.ListenPort, in.Tag)
	}

	if out.Name == "" {
		out.Name = "中转-" + strconv.Itoa(out.ListenPort)
	}
	return out, nil
}

// relayAddr 拼接规则的监听地址，日志与审计里用。
func relayAddr(r *store.Relay) string {
	return net.JoinHostPort(r.Listen, strconv.Itoa(r.ListenPort))
}
