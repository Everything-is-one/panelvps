package server

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/logs"
	"github.com/Everything-is-one/panelvps/internal/notify"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
)

func toFirewallRules(items []store.FirewallRule) []firewall.Rule {
	out := make([]firewall.Rule, 0, len(items))
	for _, r := range items {
		out = append(out, firewall.Rule{
			ID: r.ID, Action: r.Action, Proto: r.Proto, Port: r.Port,
			Source: r.Source, Comment: r.Comment, Enabled: r.Enabled,
		})
	}
	return out
}

func (s *Server) handleFirewallStatus(w http.ResponseWriter, r *http.Request) {
	status := firewall.Detect()
	if status.Supported {
		if bans, err := firewall.ListBans(); err == nil {
			status.Bans = bans
		}
	}
	if status.Bans == nil {
		status.Bans = []firewall.BanEntry{}
	}
	rules, err := s.st.ListFirewallRules(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取规则失败")
		return
	}
	status.Whitelist = false
	for _, rule := range rules {
		if rule.Enabled && rule.Action == "drop" && (rule.Port == "*" || rule.Port == "") && rule.Source == "" {
			status.Whitelist = true
		}
	}
	ok(w, map[string]any{
		"status":       status,
		"rules":        rules,
		"ssh_ports":    s.cfg.SSHPorts,
		"persist_path": firewall.PersistPath(),
	})
}

type firewallRuleReq struct {
	Action  string `json:"action"`
	Proto   string `json:"proto"`
	Port    string `json:"port"`
	Source  string `json:"source"`
	Comment string `json:"comment"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) handleFirewallCreate(w http.ResponseWriter, r *http.Request) {
	var req firewallRuleReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rule := firewall.Rule{
		Action: strings.TrimSpace(req.Action), Proto: strings.TrimSpace(req.Proto),
		Port: strings.TrimSpace(req.Port), Source: strings.TrimSpace(req.Source),
		Comment: strings.TrimSpace(req.Comment), Enabled: enabled,
	}
	if rule.Proto == "" {
		rule.Proto = "tcp"
	}
	if err := firewall.ValidateRule(rule); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	row := store.FirewallRule{
		Action: rule.Action, Proto: rule.Proto, Port: rule.Port,
		Source: rule.Source, Comment: rule.Comment, Enabled: rule.Enabled,
	}
	if err := s.st.CreateFirewallRule(r.Context(), &row); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "保存规则失败")
		return
	}
	s.audit(r, "firewall_rule_create", strconv.FormatInt(row.ID, 10),
		row.Action+" "+row.Proto+" "+row.Port+" "+row.Source)
	ok(w, row)
}

func (s *Server) handleFirewallDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "规则 ID 非法")
		return
	}
	if err := s.st.DeleteFirewallRule(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "删除失败")
		return
	}
	s.audit(r, "firewall_rule_delete", strconv.FormatInt(id, 10), "")
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleFirewallApply(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListFirewallRules(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取规则失败")
		return
	}
	opt := firewall.ApplyOptions{
		Rules:         toFirewallRules(rows),
		SSHPorts:      s.effectiveSSHPorts(),
		PanelPort:     s.panelPort(),
		PanelLoopback: s.cfg.IsLoopback(),
	}
	script, err := firewall.Apply(opt)
	if err != nil {
		s.audit(r, "firewall_apply_failed", "nftables", store.TrimDetail(err.Error()))
		status := http.StatusBadRequest
		if err == firewall.ErrUnsupported {
			status = http.StatusServiceUnavailable
		}
		fail(w, status, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "firewall_apply", "nftables", "规则 "+strconv.Itoa(len(opt.Rules))+" 条")
	ok(w, map[string]any{
		"script":    script,
		"backend":   firewall.Detect().Backend,
		"applied":   true,
		"ssh_ports": opt.SSHPorts,
	})
}

// effectiveSSHPorts 合并配置与 sshd_config，宁可多放行也不要锁死自己。
func (s *Server) effectiveSSHPorts() []int {
	set := map[int]bool{}
	for _, p := range s.cfg.SSHPorts {
		set[p] = true
	}
	for _, p := range sysinfo.FindSSHPorts() {
		set[p] = true
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func (s *Server) panelPort() int {
	_, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

type banReq struct {
	IP      string `json:"ip"`
	Minutes int    `json:"minutes"`
	// Permanent 为 true 时忽略 minutes，直接长期封禁（十年，见 firewall.permaTTL）。
	Permanent bool `json:"permanent"`
}

func (s *Server) handleBanCreate(w http.ResponseWriter, r *http.Request) {
	var req banReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if net.ParseIP(strings.TrimSpace(req.IP)) == nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "IP 非法")
		return
	}
	ttl := time.Duration(0) // 0 表示永久
	if !req.Permanent {
		minutes := req.Minutes
		if minutes <= 0 || minutes > 43200 {
			minutes = 60
		}
		ttl = time.Duration(minutes) * time.Minute
	}
	if err := firewall.Ban(req.IP, ttl); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	detail := "永久"
	if ttl > 0 {
		detail = ttl.String()
	}
	s.audit(r, "firewall_ban", req.IP, detail)
	ok(w, map[string]any{"ok": true, "permanent": ttl == 0})
}

func (s *Server) handleBanDelete(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if net.ParseIP(ip) == nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "IP 非法")
		return
	}
	if err := firewall.Unban(ip); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "firewall_unban", ip, "")
	ok(w, map[string]any{"ok": true})
}

// handleSSHReport 返回最近窗口内的 SSH 失败事件与按 IP 汇总。
func (s *Server) handleSSHReport(w http.ResponseWriter, r *http.Request) {
	minutes := 10
	if v := r.URL.Query().Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1440 {
			minutes = n
		}
	}
	events, counts, err := logs.SSHFailures(time.Duration(minutes)*time.Minute, 1000)
	if err != nil {
		ok(w, map[string]any{
			"supported":   false,
			"reason":      err.Error(),
			"events":      []any{},
			"counts":      []any{},
			"threshold":   s.cfg.SSHGuardThreshold,
			"guard":       s.cfg.SSHGuardEnabled,
			"window_min":  minutes,
			"ban_minutes": s.banMinutes(),
		})
		return
	}

	type stat struct {
		IP    string `json:"ip"`
		Count int    `json:"count"`
	}
	var list []stat
	for ip, n := range counts {
		list = append(list, stat{IP: ip, Count: n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Count > list[j].Count })

	ok(w, map[string]any{
		"supported":   true,
		"events":      events,
		"counts":      list,
		"threshold":   s.cfg.SSHGuardThreshold,
		"guard":       s.cfg.SSHGuardEnabled,
		"window_min":  minutes,
		"ban_minutes": s.banMinutes(),
	})
}

func (s *Server) banMinutes() int {
	if _, ban, err := s.cfg.SSHGuardDurations(); err == nil {
		return int(ban.Minutes())
	}
	return 60
}

// SSHGuardTick 由后台任务每分钟调用：统计失败次数并自动封禁。
func (s *Server) SSHGuardTick(ctx context.Context) {
	if !s.cfg.SSHGuardEnabled {
		return
	}
	if !firewall.Detect().Supported {
		return
	}
	window, banDur, err := s.cfg.SSHGuardDurations()
	if err != nil {
		return
	}
	_, counts, err := logs.SSHFailures(window, 1000)
	if err != nil {
		return
	}
	existing := map[string]bool{}
	if bans, err := firewall.ListBans(); err == nil {
		for _, b := range bans {
			existing[b.IP] = true
		}
	}
	for ip, n := range counts {
		if n < s.cfg.SSHGuardThreshold || existing[ip] {
			continue
		}
		// 过了硬线就直接长期拉黑：临时封禁对「试完一万次再等一小时」的
		// 扫描器没有意义，放出来它还会接着打。
		permaLine := int(s.permaThreshold.Load())
		permanent := permaLine > 0 && n >= permaLine
		ttl := banDur
		if permanent {
			ttl = 0
		}
		if err := firewall.Ban(ip, ttl); err != nil {
			s.logger.Warn("SSH 自动封禁失败", "ip", ip, "err", err)
			continue
		}
		label := banDur.String()
		if permanent {
			label = "永久"
		}
		s.logger.Warn("SSH 爆破达到阈值，已自动封禁", "ip", ip, "fails", n, "ban", label)
		_ = s.st.Audit(ctx, "system", "ssh_guard_ban", ip,
			"窗口内失败 "+strconv.Itoa(n)+" 次，封禁 "+label, "firewall")
		if s.notifier != nil {
			s.notifier.Send(notify.Event{
				Level:  "warn",
				Title:  "SSH 爆破自动封禁",
				Detail: ip + " 在 " + window.String() + " 内失败 " + strconv.Itoa(n) + " 次，已封禁 " + label,
			})
		}
	}
}
