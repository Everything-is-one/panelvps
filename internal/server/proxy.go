package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/xray"
)

// 支持的协议与传输组合。把组合约束放在一处，前后端用同一套判断。
var (
	supportedProtocols = map[string]bool{
		"vless": true, "vmess": true, "trojan": true, "shadowsocks": true,
	}
	supportedNetworks = map[string]bool{"tcp": true, "ws": true, "grpc": true}
	supportedSecurity = map[string]bool{"none": true, "tls": true, "reality": true}
	supportedSSMethod = map[string]bool{
		"2022-blake3-aes-128-gcm":       true,
		"2022-blake3-aes-256-gcm":       true,
		"2022-blake3-chacha20-poly1305": true,
		"aes-128-gcm":                   true,
		"aes-256-gcm":                   true,
		"chacha20-ietf-poly1305":        true,
	}
)

// ---------- 视图 ----------

type clientView struct {
	ID         int64      `json:"id"`
	InboundID  int64      `json:"inbound_id"`
	Email      string     `json:"email"`
	Secret     string     `json:"secret"`
	Flow       string     `json:"flow,omitempty"`
	Level      int        `json:"level"`
	Enabled    bool       `json:"enabled"`
	Up         int64      `json:"up"`
	Down       int64      `json:"down"`
	TotalBytes int64      `json:"total_bytes"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Active     bool       `json:"active"`
	Link       string     `json:"link,omitempty"`
}

type inboundView struct {
	ID         int64        `json:"id"`
	Tag        string       `json:"tag"`
	Remark     string       `json:"remark"`
	Protocol   string       `json:"protocol"`
	Listen     string       `json:"listen"`
	Port       int          `json:"port"`
	PublicPort int          `json:"public_port"`
	Network    string       `json:"network"`
	Security   string       `json:"security"`
	Options    xray.Options `json:"options"`
	Enabled    bool         `json:"enabled"`
	Clients    []clientView `json:"clients"`
	Up         int64        `json:"up"`
	Down       int64        `json:"down"`
}

// handleProxiesOverview 一次性返回页面需要的全部数据，避免前端多次往返。
func (s *Server) handleProxiesOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inbounds, err := s.st.ListInbounds(ctx)
	if err != nil {
		s.logger.Error("读取入站失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "读取入站失败")
		return
	}
	ids := make([]int64, len(inbounds))
	for i, in := range inbounds {
		ids[i] = in.ID
	}
	clients, err := s.st.ClientsByInboundIDs(ctx, ids)
	if err != nil {
		s.logger.Error("读取用户失败", "err", err)
		fail(w, http.StatusInternalServerError, codeInternalError, "读取用户失败")
		return
	}

	host, globalPort := s.publicTarget(r)
	now := time.Now()
	views := make([]inboundView, 0, len(inbounds))
	for _, in := range inbounds {
		opts, _ := xray.ParseOptions(in.Options)
		// 私钥绝不下发到前端：前端只需要公钥拼链接
		opts.PrivateKey = ""

		v := inboundView{
			ID: in.ID, Tag: in.Tag, Remark: in.Remark, Protocol: in.Protocol,
			Listen: in.Listen, Port: in.Port, PublicPort: in.PublicPort, Network: in.Network,
			Security: in.Security, Options: opts, Enabled: in.Enabled,
			Clients: []clientView{},
		}
		for _, c := range clients[in.ID] {
			cv := clientView{
				ID: c.ID, InboundID: c.InboundID, Email: c.Email, Secret: c.Secret,
				Flow: c.Flow, Level: c.Level, Enabled: c.Enabled,
				Up: c.Up, Down: c.Down, TotalBytes: c.TotalBytes,
				Active: c.Active(now),
			}
			if c.ExpiresAt.Valid {
				t := c.ExpiresAt.Time
				cv.ExpiresAt = &t
			}
			if host != "" {
				target := xray.LinkTarget{Host: host, Port: linkPort(in, globalPort)}
				if link, err := xray.BuildLink(in, c, target); err == nil {
					cv.Link = link
				}
			}
			v.Up += c.Up
			v.Down += c.Down
			v.Clients = append(v.Clients, cv)
		}
		views = append(views, v)
	}

	ok(w, map[string]any{
		"status":      s.xr.Status(),
		"host":        host,
		"public_port": globalPort,
		"inbounds":    views,
		"protocols":   []string{"vless", "vmess", "trojan", "shadowsocks"},
		"networks":    []string{"tcp", "ws", "grpc"},
		"securities":  []string{"none", "tls", "reality"},
		"ss_methods": []string{
			"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm",
			"2022-blake3-chacha20-poly1305",
		},
		"reality_targets": []string{
			"www.cloudflare.com:443", "www.apple.com:443", "www.microsoft.com:443",
			"dl.google.com:443", "www.amazon.com:443",
		},
	})
}

// ---------- 入站 ----------

type inboundReq struct {
	Remark   string `json:"remark"`
	Protocol string `json:"protocol"`
	Listen   string `json:"listen"`
	Port     int    `json:"port"`
	// PublicPort 是 NAT 映射出来的对外端口，0 表示与监听端口相同。
	PublicPort int          `json:"public_port"`
	Network    string       `json:"network"`
	Security   string       `json:"security"`
	Options    xray.Options `json:"options"`
	Enabled    *bool        `json:"enabled"`
}

func (s *Server) handleCreateInbound(w http.ResponseWriter, r *http.Request) {
	var req inboundReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}

	in, err := s.buildInbound(r.Context(), req, nil)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	id, err := s.st.CreateInbound(r.Context(), in)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "创建失败: "+err.Error())
		return
	}
	in.ID = id

	// 顺手配一个默认用户：否则界面显示「暂无用户」，还得再点一次「+ 用户」
	// 才能拿到连接链接，第一眼看上去像坏了。
	clientID, cerr := s.ensureDefaultClient(r.Context(), in)
	if cerr != nil {
		s.logger.Warn("创建默认用户失败", "inbound", in.Tag, "err", cerr)
	}

	s.audit(r, "inbound_create", in.Tag, fmt.Sprintf("%s:%d/%s", in.Listen, in.Port, in.Protocol))
	apply := s.applyXray(r.Context())
	s.logger.Info("创建入站", "tag", in.Tag, "port", in.Port, "protocol", in.Protocol)
	out := map[string]any{"id": id, "apply": apply}
	if clientID > 0 {
		out["client_id"] = clientID
	}
	if cerr != nil {
		out["client_error"] = cerr.Error()
	}
	ok(w, out)
}

// ensureDefaultClient 给新入站配一个默认用户，返回其 ID。
//
// email 是 Xray 的统计维度，必须全局唯一，所以重名时加序号。
func (s *Server) ensureDefaultClient(ctx context.Context, in *store.Inbound) (int64, error) {
	email := "default"
	for i := 2; i <= 20; i++ {
		if _, err := s.st.ClientByEmail(ctx, email); err != nil {
			break
		}
		email = fmt.Sprintf("default-%d", i)
	}
	c, err := s.buildClient(ctx, clientReq{Email: email}, in, nil)
	if err != nil {
		return 0, err
	}
	return s.st.CreateClient(ctx, c)
}

func (s *Server) handleUpdateInbound(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "入站 ID 非法")
		return
	}
	cur, err := s.st.InboundByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "入站不存在")
		return
	}

	var req inboundReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}
	next, err := s.buildInbound(r.Context(), req, cur)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	next.ID = id
	if err := s.st.UpdateInbound(r.Context(), next); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "更新失败: "+err.Error())
		return
	}
	s.audit(r, "inbound_update", next.Tag, fmt.Sprintf("port=%d", next.Port))
	ok(w, map[string]any{"apply": s.applyXray(r.Context())})
}

func (s *Server) handleDeleteInbound(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "入站 ID 非法")
		return
	}
	in, err := s.st.InboundByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "入站不存在")
		return
	}
	if err := s.st.DeleteInbound(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "删除失败")
		return
	}
	s.audit(r, "inbound_delete", in.Tag, fmt.Sprintf("port=%d", in.Port))
	ok(w, map[string]any{"apply": s.applyXray(r.Context())})
}

// buildInbound 校验并补全入站参数。prev 非空表示更新，用于继承未提交的字段。
// pickPort 在 20000–59999 里挑一个没被占用的端口。
func (s *Server) pickPort(ctx context.Context, listen string) (int, error) {
	for i := 0; i < 64; i++ {
		p := 20000 + rand.IntN(40000)
		if s.portInUse(ctx, listen, p) {
			continue
		}
		return p, nil
	}
	return 0, errors.New("连续 64 次随机端口都被占用，请手工指定一个端口")
}

// portInUse 判断端口是否已被面板、Xray 内部接口或其它入站占用。
func (s *Server) portInUse(ctx context.Context, listen string, p int) bool {
	if _, port, err := net.SplitHostPort(s.cfg.Listen); err == nil {
		if v, _ := strconv.Atoi(port); v == p {
			return true
		}
	}
	if p == s.cfg.XrayAPIPort {
		return true
	}
	if other, err := s.st.InboundByPort(ctx, listen, p, 0); err == nil && other != nil {
		return true
	}
	return false
}

func (s *Server) buildInbound(ctx context.Context, req inboundReq, prev *store.Inbound) (*store.Inbound, error) {
	in := &store.Inbound{}
	if prev != nil {
		*in = *prev
	}
	if req.Remark != "" || prev == nil {
		in.Remark = strings.TrimSpace(req.Remark)
	}
	if req.Protocol != "" {
		in.Protocol = strings.ToLower(req.Protocol)
	}
	if !supportedProtocols[in.Protocol] {
		return nil, fmt.Errorf("不支持的协议 %q", in.Protocol)
	}
	if req.Listen == "" && prev == nil {
		in.Listen = "0.0.0.0"
	} else if req.Listen != "" {
		in.Listen = req.Listen
	}
	if net.ParseIP(in.Listen) == nil {
		return nil, errors.New("监听地址必须是 IP（0.0.0.0 表示全部）")
	}
	if req.Port != 0 {
		in.Port = req.Port
	}
	// 没指定端口就随机挑一个：固定 443 会让第二个入站必撞端口，
	// 也让扫描器一眼认出这是面板生成的节点。
	if in.Port == 0 && prev == nil {
		p, err := s.pickPort(ctx, in.Listen)
		if err != nil {
			return nil, err
		}
		in.Port = p
	}
	if req.PublicPort != 0 {
		if req.PublicPort < 1 || req.PublicPort > 65535 {
			return nil, errors.New("对外端口必须在 1–65535 之间（留空表示与监听端口相同）")
		}
		in.PublicPort = req.PublicPort
	} else if prev == nil {
		in.PublicPort = 0
	}
	if in.Port < 1 || in.Port > 65535 {
		return nil, errors.New("端口必须在 1–65535 之间")
	}
	if req.Network != "" {
		in.Network = strings.ToLower(req.Network)
	}
	if in.Network == "" {
		in.Network = "tcp"
	}
	if !supportedNetworks[in.Network] {
		return nil, fmt.Errorf("不支持的传输方式 %q", in.Network)
	}
	if req.Security != "" {
		in.Security = strings.ToLower(req.Security)
	}
	if in.Security == "" {
		in.Security = "none"
	}
	if !supportedSecurity[in.Security] {
		return nil, fmt.Errorf("不支持的安全类型 %q", in.Security)
	}

	// 协议与安全性的组合约束
	switch in.Protocol {
	case "vmess":
		if in.Security == "reality" {
			return nil, errors.New("VMess 不支持 REALITY，请改用 VLESS")
		}
	case "shadowsocks":
		if in.Security != "none" {
			return nil, errors.New("Shadowsocks 不支持 TLS/REALITY")
		}
		if in.Network != "tcp" {
			return nil, errors.New("Shadowsocks 仅支持 TCP 传输")
		}
	}
	if in.Security == "reality" {
		if in.Protocol != "vless" {
			return nil, errors.New("REALITY 仅适用于 VLESS")
		}
		if in.Network != "tcp" && in.Network != "grpc" {
			return nil, errors.New("REALITY 仅支持 tcp / grpc 传输")
		}
	}

	// 端口冲突：面板自身、Xray API 端口、其他入站
	if host, port, err := net.SplitHostPort(s.cfg.Listen); err == nil && host != "" {
		if p, _ := strconv.Atoi(port); p == in.Port {
			return nil, fmt.Errorf("端口 %d 已被面板自身占用", in.Port)
		}
	}
	if in.Port == s.cfg.XrayAPIPort {
		return nil, fmt.Errorf("端口 %d 是 Xray 内部接口端口", in.Port)
	}
	exclude := int64(0)
	if prev != nil {
		exclude = prev.ID
	}
	if other, err := s.st.InboundByPort(ctx, in.Listen, in.Port, exclude); err == nil && other != nil {
		return nil, fmt.Errorf("端口 %d 已被入站 %s 占用", in.Port, other.Tag)
	}

	// 合并 options：未提交的字段沿用旧值，避免误清空私钥这类不可再生数据
	opts := req.Options
	if prev != nil {
		old, err := xray.ParseOptions(prev.Options)
		if err != nil {
			return nil, err
		}
		opts = mergeOptions(old, opts)
	}
	opts.Dest = strings.TrimSpace(opts.Dest)
	if opts.Dest != "" && !strings.Contains(opts.Dest, ":") {
		opts.Dest += ":443"
	}

	switch in.Security {
	case "reality":
		if opts.PrivateKey == "" {
			priv, pub, err := xray.NewRealityKeypair()
			if err != nil {
				return nil, fmt.Errorf("生成 REALITY 密钥失败: %w", err)
			}
			opts.PrivateKey, opts.PublicKey = priv, pub
		} else if opts.PublicKey == "" {
			pub, err := xray.PublicFromPrivate(opts.PrivateKey)
			if err != nil {
				return nil, err
			}
			opts.PublicKey = pub
		}
		if opts.Dest == "" {
			opts.Dest = "www.cloudflare.com:443"
		}
		if len(opts.ServerNames) == 0 {
			host := strings.Split(opts.Dest, ":")[0]
			opts.ServerNames = []string{host}
		}
		if len(opts.ShortIDs) == 0 {
			sid, err := xray.NewShortID()
			if err != nil {
				return nil, err
			}
			opts.ShortIDs = []string{sid}
		}
	case "tls":
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, errors.New("TLS 需要填写证书与私钥路径")
		}
	case "none":
		opts.PrivateKey, opts.PublicKey = "", ""
	}

	if in.Protocol == "shadowsocks" {
		if opts.Method == "" {
			opts.Method = "2022-blake3-aes-128-gcm"
		}
		if !supportedSSMethod[opts.Method] {
			return nil, fmt.Errorf("不支持的 Shadowsocks 加密方式 %q", opts.Method)
		}
	}

	raw, err := marshalOptions(opts)
	if err != nil {
		return nil, err
	}
	in.Options = raw

	// tag 由协议与端口推导，保证唯一且可读
	if prev == nil || prev.Port != in.Port || prev.Protocol != in.Protocol {
		in.Tag = s.uniqueTag(ctx, fmt.Sprintf("%s-%d", in.Protocol, in.Port), in.ID)
	}
	if in.Tag == "" {
		in.Tag = fmt.Sprintf("%s-%d", in.Protocol, in.Port)
	}

	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	} else if prev == nil {
		in.Enabled = true
	}
	return in, nil
}

func (s *Server) uniqueTag(ctx context.Context, base string, excludeID int64) string {
	existing, err := s.st.ListInbounds(ctx)
	if err != nil {
		return base
	}
	used := map[string]bool{}
	for _, in := range existing {
		if in.ID != excludeID {
			used[in.Tag] = true
		}
	}
	if !used[base] {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !used[cand] {
			return cand
		}
	}
	return fmt.Sprintf("%s-%d", base, time.Now().Unix())
}

// ---------- 客户端 ----------

type clientReq struct {
	Email      string `json:"email"`
	Secret     string `json:"secret"`
	Flow       string `json:"flow"`
	Level      int    `json:"level"`
	Enabled    *bool  `json:"enabled"`
	TotalBytes int64  `json:"total_bytes"`
	// ExpiresAt 用 RFC3339；空串表示不改动，ClearExpiry 为真才清除。
	ExpiresAt   string `json:"expires_at"`
	ClearExpiry bool   `json:"clear_expiry"`
	InboundID   int64  `json:"inbound_id"`
}

func (s *Server) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	inboundID, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "入站 ID 非法")
		return
	}
	in, err := s.st.InboundByID(r.Context(), inboundID)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "入站不存在")
		return
	}

	var req clientReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}

	c, err := s.buildClient(r.Context(), req, in, nil)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	id, err := s.st.CreateClient(r.Context(), c)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "创建用户失败: "+err.Error())
		return
	}
	s.audit(r, "client_create", c.Email, in.Tag)
	ok(w, map[string]any{"id": id, "apply": s.applyXray(r.Context())})
}

func (s *Server) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "用户 ID 非法")
		return
	}
	cur, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return
	}
	in, err := s.st.InboundByID(r.Context(), cur.InboundID)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "入站不存在")
		return
	}

	var req clientReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}
	next, err := s.buildClient(r.Context(), req, in, cur)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	next.ID = id
	if err := s.st.UpdateClient(r.Context(), next); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "更新失败")
		return
	}
	s.audit(r, "client_update", next.Email, in.Tag)
	ok(w, map[string]any{"apply": s.applyXray(r.Context())})
}

func (s *Server) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "用户 ID 非法")
		return
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return
	}
	if err := s.st.DeleteClient(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "删除失败")
		return
	}
	s.audit(r, "client_delete", c.Email, "")
	ok(w, map[string]any{"apply": s.applyXray(r.Context())})
}

func (s *Server) handleResetClientTraffic(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "用户 ID 非法")
		return
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return
	}
	if err := s.st.ResetClientTraffic(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "重置失败")
		return
	}
	s.audit(r, "client_reset", c.Email, "")
	ok(w, nil)
}

func (s *Server) buildClient(ctx context.Context, req clientReq, in *store.Inbound, prev *store.Client) (*store.Client, error) {
	c := &store.Client{InboundID: in.ID, Level: 0, Enabled: true}
	if prev != nil {
		*c = *prev
	}

	if req.Email != "" || prev == nil {
		c.Email = strings.TrimSpace(req.Email)
	}
	if c.Email == "" {
		return nil, errors.New("必须填写用户标识（email）")
	}
	if len(c.Email) > 64 {
		return nil, errors.New("用户标识不能超过 64 字符")
	}
	if strings.ContainsAny(c.Email, " \t\r\n") {
		return nil, errors.New("用户标识不能包含空白字符")
	}
	// Xray 用 email 作为统计维度，跨入站重名会导致流量合并计到同一人
	if other, err := s.st.ClientByEmail(ctx, c.Email); err == nil && (prev == nil || other.ID != prev.ID) {
		return nil, fmt.Errorf("用户标识 %s 已被使用（全局唯一）", c.Email)
	}

	if req.Secret != "" {
		c.Secret = strings.TrimSpace(req.Secret)
	}
	if c.Secret == "" {
		secret, err := defaultSecret(in, c.Email)
		if err != nil {
			return nil, err
		}
		c.Secret = secret
	}
	if err := validateSecret(in, c.Secret); err != nil {
		return nil, err
	}

	if req.Flow != "" {
		c.Flow = req.Flow
	}
	// xtls-rprx-vision 只跑在 TCP 上。ws / grpc 带这个 flow，Xray 会
	// 在握手阶段拒绝连接（配置能过 -test，用户只会看到"连不上"），
	// 所以这里必须按传输方式卡住，不能只看 security。
	if in.Protocol == "vless" && in.Network == "tcp" && (in.Security == "reality" || in.Security == "tls") && c.Flow == "" {
		c.Flow = "xtls-rprx-vision"
	}
	if in.Protocol != "vless" || in.Network != "tcp" {
		c.Flow = ""
	}

	c.Level = req.Level
	if req.TotalBytes >= 0 {
		c.TotalBytes = req.TotalBytes
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			return nil, errors.New("到期时间格式应为 RFC3339")
		}
		c.ExpiresAt = sqlNullTime(t)
	} else if req.ClearExpiry {
		c.ExpiresAt = sqlNullTime(time.Time{})
	}
	return c, nil
}

func defaultSecret(in *store.Inbound, email string) (string, error) {
	switch in.Protocol {
	case "vless", "vmess":
		return xray.NewUUID()
	case "trojan":
		return xray.NewPassword(24)
	case "shadowsocks":
		opts, _ := xray.ParseOptions(in.Options)
		method := opts.Method
		if method == "" {
			method = "2022-blake3-aes-128-gcm"
		}
		if xray.IsSS2022(method) {
			return xray.NewSSPassword(method)
		}
		return xray.NewPassword(24)
	}
	return "", errors.New("未知协议")
}

func validateSecret(in *store.Inbound, secret string) error {
	switch in.Protocol {
	case "vless", "vmess":
		if !xray.IsUUID(secret) {
			return errors.New("UUID 格式不正确")
		}
	case "trojan":
		if len(secret) < 8 {
			return errors.New("密码至少 8 位")
		}
	case "shadowsocks":
		opts, _ := xray.ParseOptions(in.Options)
		if xray.IsSS2022(opts.Method) && !xray.IsSSKeyValid(opts.Method, secret) {
			return fmt.Errorf("%s 要求 %d 字节的 base64 密钥", opts.Method, xray.SSKeyLen(opts.Method))
		}
	}
	return nil
}

// handleCollectTraffic 手动触发一次采样，供页面「刷新」按钮使用。
func (s *Server) handleCollectTraffic(w http.ResponseWriter, r *http.Request) {
	if err := s.CollectTraffic(r.Context()); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"status": s.xr.Status()})
}

// handleClientLinks 返回该用户全部可用链接（当前只属于一个入站）。
func (s *Server) handleClientLinks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "用户 ID 非法")
		return
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return
	}
	in, err := s.st.InboundByID(r.Context(), c.InboundID)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "入站不存在")
		return
	}
	host, globalPort := s.publicTarget(r)
	if host == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "请先在设置里填写服务器公网地址")
		return
	}
	link, err := xray.BuildLink(*in, *c, xray.LinkTarget{Host: host, Port: linkPort(*in, globalPort)})
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"link": link, "email": c.Email})
}

// handleQR 返回链接的二维码 PNG。
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "用户 ID 非法")
		return
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "用户不存在")
		return
	}
	in, err := s.st.InboundByID(r.Context(), c.InboundID)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "入站不存在")
		return
	}
	h, gp := s.publicTarget(r)
	link, err := xray.BuildLink(*in, *c, xray.LinkTarget{Host: h, Port: linkPort(*in, gp)})
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	png, err := xray.QRPNG(link, 320)
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "生成二维码失败")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// ---------- 密钥 / 设置 / 应用 ----------

// handleRealityKey 生成一对 REALITY 密钥，供前端「一键生成」使用。
func (s *Server) handleRealityKey(w http.ResponseWriter, r *http.Request) {
	priv, pub, err := xray.NewRealityKeypair()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "生成密钥失败")
		return
	}
	ok(w, map[string]any{"private_key": priv, "public_key": pub})
}

type prefsReq struct {
	PublicHost string `json:"public_host"`
}

func (s *Server) handleSavePrefs(w http.ResponseWriter, r *http.Request) {
	var req prefsReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	host := strings.TrimSpace(req.PublicHost)
	if host != "" && strings.ContainsAny(host, " /?#") {
		fail(w, http.StatusBadRequest, codeBadRequest, "公网地址不能包含空格或路径")
		return
	}
	if err := s.st.SetSetting(r.Context(), "public_host", host); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "保存失败")
		return
	}
	s.audit(r, "prefs_update", "public_host", host)
	ok(w, map[string]any{"host": s.publicHost(r)})
}

// handleApply 手动触发一次「重建配置 + 重启」。
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	result := s.applyXray(r.Context())
	s.audit(r, "xray_apply", "config", result.Message)
	ok(w, result)
}

// applyResult 是给前端的应用结果。
type applyResult struct {
	OK      bool   `json:"ok"`
	Changed bool   `json:"changed"`
	Restart bool   `json:"restarted"`
	Message string `json:"message"`
	Output  string `json:"output,omitempty"`
}

// applyXray 从数据库重建配置并应用。
//
// 这里刻意吞掉错误并转成结构体：用户正在改配置时，
// 「保存成功但下发失败」必须如实告诉他，而不是整个请求 500。
func (s *Server) applyXray(ctx context.Context) applyResult {
	if !s.xr.Available() {
		return applyResult{OK: false, Message: "未安装 Xray，配置已保存但未下发"}
	}

	inbounds, err := s.st.ListInbounds(ctx)
	if err != nil {
		return applyResult{OK: false, Message: "读取入站失败: " + err.Error()}
	}
	ids := make([]int64, len(inbounds))
	for i, in := range inbounds {
		ids[i] = in.ID
	}
	clients, err := s.st.ClientsByInboundIDs(ctx, ids)
	if err != nil {
		return applyResult{OK: false, Message: "读取用户失败: " + err.Error()}
	}

	cfg, err := xray.BuildConfig(xray.Plan{
		Inbounds: inbounds,
		Clients:  clients,
		APIPort:  s.cfg.XrayAPIPort,
		LogLevel: "warning",
		Now:      time.Now(),
	})
	if err != nil {
		return applyResult{OK: false, Message: "生成配置失败: " + err.Error()}
	}

	res, err := s.xr.Apply(ctx, cfg)
	if err != nil {
		s.logger.Error("应用 Xray 配置失败", "err", err)
		out := ""
		if res != nil {
			out = res.TestOutput
		}
		return applyResult{OK: false, Message: err.Error(), Output: out}
	}
	msg := "配置未变化"
	if res.Changed {
		msg = "配置已下发并重启"
	}
	return applyResult{OK: true, Changed: res.Changed, Restart: res.Restarted, Message: msg, Output: res.TestOutput}
}

// BypassXray 在启动阶段恢复 Xray：配置存在且可用时直接接管。
func (s *Server) StartXray(ctx context.Context) {
	if !s.xr.Available() {
		s.logger.Warn("未找到 xray 可执行文件，代理功能不可用（安装后重启面板即可）")
		return
	}
	s.xr.EnsureVersion(ctx)
	res := s.applyXray(ctx)
	if !res.OK {
		s.logger.Warn("Xray 启动失败", "msg", res.Message)
		return
	}
	s.logger.Info("Xray 已就绪", "pid", s.xr.Status().PID)
}

// StopXray 在面板退出时收掉子进程，避免留下孤儿进程占端口。
func (s *Server) StopXray() {
	if s.xr != nil && s.xr.Available() {
		s.xr.Stop()
	}
}

// CollectTraffic 从 Xray 拉取统计并累加到数据库。
//
// 计数器是「自进程启动以来」的累计值，因此要用游标做差：
// 进程重启后计数归零，差值会变成负数，此时直接以当前值作为增量。
func (s *Server) CollectTraffic(ctx context.Context) error {
	if !s.xr.Available() || !s.xr.Running() {
		return nil
	}
	stats, err := s.xr.QueryStats(ctx)
	if err != nil {
		return err
	}

	type delta struct{ up, down int64 }
	byEmail := map[string]*delta{}
	for _, st := range stats {
		kind, tag, dir, ok := xray.ParseStatName(st.Name)
		if !ok || kind != "user" {
			continue
		}
		cursorName := "user:" + tag + ":" + dir
		prev, err := s.st.TrafficCursor(ctx, cursorName)
		if err != nil {
			continue
		}
		diff := st.Value - prev
		if diff < 0 {
			diff = st.Value
		}
		if diff == 0 {
			continue
		}
		if err := s.st.SetTrafficCursor(ctx, cursorName, st.Value); err != nil {
			continue
		}
		d := byEmail[tag]
		if d == nil {
			d = &delta{}
			byEmail[tag] = d
		}
		if dir == "uplink" {
			d.up += diff
		} else {
			d.down += diff
		}
	}

	for email, d := range byEmail {
		c, err := s.st.ClientByEmail(ctx, email)
		if err != nil {
			continue
		}
		if err := s.st.AddClientTraffic(ctx, c.ID, d.up, d.down); err != nil {
			s.logger.Warn("累加流量失败", "email", email, "err", err)
			continue
		}
		if err := s.st.AddTrafficSample(ctx, c.ID, d.up, d.down); err != nil {
			s.logger.Warn("写入采样失败", "err", err)
		}
	}

	// 超额/过期用户自动禁用；有变化就重建配置，让限制真正生效
	if n, err := s.st.AutoDisableExhausted(ctx); err == nil && n > 0 {
		s.logger.Info("自动禁用超额用户", "count", n)
		s.applyXray(ctx)
	}
	return nil
}

// publicTarget 返回分享链接该指向的地址。
//
// 设置里允许填 "IP:端口"——NAT / 端口映射的机器上，容器内监听 47736，
// 商家把公网 30986 转到 47736，链接必须写 30986。这里把端口拆出来，
// 否则 joinHostPort 会拼成 "204.197.163.238:30986:47736"（IPv6 形态还
// 会被加上方括号），链接直接坏掉。
//
// 返回的 port 为 0 表示「没有全局端口覆盖」，由入站自己的 public_port
// 或监听端口决定。
func (s *Server) publicTarget(r *http.Request) (string, int) {
	raw := ""
	if v, err := s.st.GetSetting(r.Context(), "public_host"); err == nil && v != "" {
		raw = v
	} else if s.cfg.PublicHost != "" {
		raw = s.cfg.PublicHost
	} else {
		raw = r.Header.Get("X-Forwarded-Host")
		if raw == "" {
			raw = r.Host
		}
	}
	raw = strings.TrimSpace(raw)
	host, port := splitPublicHost(raw)
	return host, port
}

// splitPublicHost 把用户填的「地址」统一成 (主机, 端口)。
//
// 容忍这几种写法：203.0.113.9 / 203.0.113.9:30986 / http://203.0.113.9:30986/ /
// node.example.com / [2001:db8::1]:30986。返回值 port=0 表示没写端口。
func splitPublicHost(raw string) (string, int) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
	raw = strings.TrimSuffix(raw, "/")
	if h, p, err := net.SplitHostPort(raw); err == nil {
		if n, _ := strconv.Atoi(p); n > 0 && n < 65536 {
			return h, n
		}
		// 端口写错的（0、99999）当作没写，别把整条链接弄坏
		return h, 0
	}
	return raw, 0
}

// linkPort 决定某个入站的链接端口：入站对外端口 > 全局端口 > 监听端口。
func linkPort(in store.Inbound, globalPort int) int {
	if in.PublicPort > 0 {
		return in.PublicPort
	}
	if globalPort > 0 {
		return globalPort
	}
	return 0
}

// publicHost 只返回主机名，给「访问方式」这类展示用。
func (s *Server) publicHost(r *http.Request) string {
	h, _ := s.publicTarget(r)
	return h
}

func (s *Server) audit(r *http.Request, action, target, detail string) {
	u := userFrom(r.Context())
	name := ""
	if u != nil {
		name = u.Username
	}
	_ = s.st.Audit(r.Context(), name, action, target, store.TrimDetail(detail), clientIP(r))
}

func pathID(r *http.Request, name string) (int64, error) {
	v := r.PathValue(name)
	if v == "" {
		return 0, errors.New("缺少 ID")
	}
	return strconv.ParseInt(v, 10, 64)
}
