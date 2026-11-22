package server

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
)

// 防火墙的两种默认策略。
//
// 面板此前只能靠"手工加一条 port=* 的 drop 规则"间接进入白名单模式，
// 这个入口藏得太深，绝大多数人一辈子都不会点到 —— 于是规则列表里空空
// 如也，机器却对所有端口开放，用户一直以为自己有防火墙。
//
// whitelist：链尾 drop，只放行已列出的端口。
// blacklist：链尾 accept，只拦已列出的端口（默认策略）。
const (
	modeWhitelist = "whitelist"
	modeBlacklist = "blacklist"
)

type firewallModeReq struct {
	Mode string `json:"mode"`
	// AdoptListening 为 true 时，切白名单会先把当前所有非回环监听端口
	// 逐条放行。默认 true：不这么做，点下按钮的瞬间用户的站点、宝塔、
	// Docker 业务端口全部断掉，而这些都是本机已经在跑的。
	AdoptListening *bool `json:"adopt_listening"`
}

type firewallModeResult struct {
	Mode      string   `json:"mode"`
	Adopted   []string `json:"adopted"`   // 本次自动放行的端口
	Applied   bool     `json:"applied"`   // 是否已下发内核
	Whitelist bool     `json:"whitelist"` // 切换后的实际状态
}

func (s *Server) handleFirewallMode(w http.ResponseWriter, r *http.Request) {
	var req firewallModeReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误: "+err.Error())
		return
	}
	wantWL := true
	switch strings.TrimSpace(req.Mode) {
	case modeWhitelist:
		wantWL = true
	case modeBlacklist:
		wantWL = false
	default:
		fail(w, http.StatusBadRequest, codeBadRequest, "mode 只能是 whitelist 或 blacklist")
		return
	}
	adopt := true
	if req.AdoptListening != nil {
		adopt = *req.AdoptListening
	}

	res, err := s.switchFirewallMode(r.Context(), wantWL, adopt)
	if err != nil {
		if err == firewall.ErrUnsupported {
			fail(w, http.StatusServiceUnavailable, codeBadRequest, err.Error())
			return
		}
		s.audit(r, "firewall_mode_failed", req.Mode, store.TrimDetail(err.Error()))
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	action := "firewall_mode_blacklist"
	if wantWL {
		action = "firewall_mode_whitelist"
	}
	s.audit(r, action, req.Mode, "自动放行 "+strconv.Itoa(len(res.Adopted))+" 个在监端口")
	ok(w, res)
}

// switchFirewallMode 切换默认策略。
//
// 顺序刻意是「先下发内核、成功后写库」：反过来一旦内核下发失败，库里却
// 留着那条 port=* 的 drop，面板下次重启自动 apply 时就把 SSH 和面板一起
// 锁在门外了。先内核后库，最坏情况是内核已切、库里没记，重启退回旧策略，
// 至少不失联。
func (s *Server) switchFirewallMode(ctx context.Context, wantWL, adopt bool) (firewallModeResult, error) {
	res := firewallModeResult{Applied: true}

	rows, err := s.st.ListFirewallRules(ctx)
	if err != nil {
		return res, err
	}

	// 找出所有的兜底 drop（port 为空或 *、且不限来源）
	var fallbackIDs []int64
	var keep []firewall.Rule
	cur := toFirewallRules(rows)
	for _, r := range cur {
		if r.Action == "drop" && (r.Port == "" || r.Port == "*") && strings.TrimSpace(r.Source) == "" {
			fallbackIDs = append(fallbackIDs, r.ID)
			continue
		}
		keep = append(keep, r)
	}

	if !wantWL {
		res.Mode, res.Whitelist = modeBlacklist, false
		if len(fallbackIDs) == 0 {
			return res, nil // 本来就是黑名单，不用下发
		}
		if err := s.applyWithRules(ctx, keep); err != nil {
			return res, err
		}
		for _, id := range fallbackIDs {
			_ = s.st.DeleteFirewallRule(ctx, id)
		}
		return res, nil
	}

	res.Mode, res.Whitelist = modeWhitelist, true

	// 已有 accept 规则覆盖了哪些 proto/port，避免重复添加
	var adds []store.FirewallRule
	if adopt {
		ports, _ := sysinfo.ListeningPorts()
		adds = planWhitelistAdds(rows, ports)
		for _, a := range adds {
			res.Adopted = append(res.Adopted, a.Proto+"/"+a.Port)
		}
		sort.Strings(res.Adopted)
	}

	// 组装将要下发的规则集：现有 + 新增 + 兜底 drop
	next := make([]firewall.Rule, 0, len(keep)+len(adds)+1)
	next = append(next, keep...)
	for i, a := range adds {
		next = append(next, firewall.Rule{
			ID:     -int64(i + 1), // 负数占位：还没入库，渲染用不到 ID
			Action: a.Action, Proto: a.Proto, Port: a.Port,
			Comment: a.Comment, Enabled: true,
		})
	}
	if len(fallbackIDs) == 0 {
		next = append(next, firewall.Rule{
			ID: -9999, Action: "drop", Proto: "any", Port: "*",
			Comment: "默认拒绝（白名单模式）", Enabled: true,
		})
	}

	if err := s.applyWithRules(ctx, next); err != nil {
		return res, err
	}

	for i := range adds {
		if err := s.st.CreateFirewallRule(ctx, &adds[i]); err != nil {
			return res, err
		}
	}
	if len(fallbackIDs) == 0 {
		row := store.FirewallRule{
			Action: "drop", Proto: "any", Port: "*",
			Comment: "默认拒绝（白名单模式）", Enabled: true,
		}
		if err := s.st.CreateFirewallRule(ctx, &row); err != nil {
			return res, err
		}
	}
	return res, nil
}

// applyWithRules 用给定规则集下发内核。与 handleFirewallApply 共用同一套
// 放行参数（SSH 端口、面板端口），保证切换过程中不会把面板自己挡在外面。
func (s *Server) applyWithRules(ctx context.Context, rules []firewall.Rule) error {
	_, err := firewall.Apply(firewall.ApplyOptions{
		Rules:         rules,
		SSHPorts:      s.effectiveSSHPorts(),
		PanelPort:     s.panelPort(),
		PanelLoopback: s.cfg.IsLoopback(),
	})
	return err
}

// ensurePortAllowed 给某个端口补一条放行规则并下发，返回是否新增。
//
// 入站与中转建好之后，端口本来靠"默认放行"就能通，所以以前没这一步也
// 没问题 —— 但这恰好是白名单模式的坑：用户切到默认拒绝以后，新建的节点
// 在防火墙里没有对应规则，节点建好了却连不上，表现很像"节点坏了"。
// 这里统一补上，让两种模式下的行为一致。
func (s *Server) ensurePortAllowed(ctx context.Context, port int, proto, comment string) (bool, error) {
	if port <= 0 || port > 65535 {
		return false, nil
	}
	if proto == "" {
		proto = "tcp"
	}
	rows, err := s.st.ListFirewallRules(ctx)
	if err != nil {
		return false, err
	}
	ps := strconv.Itoa(port)
	for _, r := range rows {
		// 同一个端口不限来源的 accept 已经存在就跳过
		if r.Enabled && r.Action == "accept" && r.Port == ps &&
			(r.Proto == proto || r.Proto == "any") && strings.TrimSpace(r.Source) == "" {
			return false, nil
		}
	}
	row := store.FirewallRule{
		Action: "accept", Proto: proto, Port: ps,
		Comment: comment, Enabled: true,
	}
	if err := s.st.CreateFirewallRule(ctx, &row); err != nil {
		return false, err
	}
	// 只有白名单模式下需要立刻下发；黑名单模式默认就通，避免每次建节点
	// 都重建一遍整张表。
	if s.firewallIsWhitelist(ctx) {
		after, err := s.st.ListFirewallRules(ctx)
		if err != nil {
			return true, err
		}
		if err := s.applyWithRules(ctx, toFirewallRules(after)); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (s *Server) firewallIsWhitelist(ctx context.Context) bool {
	rows, err := s.st.ListFirewallRules(ctx)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.Enabled && r.Action == "drop" && (r.Port == "" || r.Port == "*") &&
			strings.TrimSpace(r.Source) == "" {
			return true
		}
	}
	return false
}

// planWhitelistAdds 算出让当前在监端口「不被切断」所需新增的放行规则。
//
// 抽成纯函数是因为这段最容易出错：漏一个端口，用户点完按钮业务就断；
// 多算一个，防火墙就白开了。输入输出都是普通数据，可以直接喂用例。
func planWhitelistAdds(existing []store.FirewallRule, ports []sysinfo.Port) []store.FirewallRule {
	have := map[string]bool{}
	for _, r := range existing {
		if r.Enabled && r.Action == "accept" {
			have[r.Proto+"/"+r.Port] = true
			// any 覆盖了具体协议，两种都算已覆盖
			if r.Proto == "any" {
				have["tcp/"+r.Port] = true
				have["udp/"+r.Port] = true
			}
		}
	}

	var adds []store.FirewallRule
	seen := map[string]bool{}
	for _, p := range ports {
		if !p.Expose { // 只听 127.0.0.1 的端口不需要对外放行
			continue
		}
		if p.Port <= 0 || p.Port > 65535 {
			continue
		}
		proto := "tcp"
		if strings.HasPrefix(p.Proto, "udp") {
			proto = "udp"
		}
		key := proto + "/" + strconv.Itoa(p.Port)
		if have[key] || seen[key] {
			continue
		}
		seen[key] = true
		adds = append(adds, store.FirewallRule{
			Action: "accept", Proto: proto, Port: strconv.Itoa(p.Port),
			Comment: "切换白名单时已在监听", Enabled: true,
		})
	}
	return adds
}

// autoPortMark 标记由面板自动派生的端口放行规则。
//
// 删除节点时只回收带这个前缀的规则，用户手工加的一条都不动 —— 这是
// 自动清理能安全存在的前提。备注前缀也让人一眼看出这条规则从哪来。
const autoPortMark = "[auto] "

// releasePort 回收某个节点的自动放行规则。
//
// kind 形如「入站」「中转」，同时用作备注前缀的一部分，避免删入站时
// 顺手把中转的规则也带走。
func (s *Server) releasePort(ctx context.Context, port int, kind string) error {
	if port <= 0 {
		return nil
	}
	rows, err := s.st.ListFirewallRules(ctx)
	if err != nil {
		return err
	}
	ps := strconv.Itoa(port)
	prefix := autoPortMark + kind
	removed := false
	for _, r := range rows {
		if r.Port != ps || !strings.HasPrefix(r.Comment, prefix) {
			continue
		}
		if err := s.st.DeleteFirewallRule(ctx, r.ID); err != nil {
			return err
		}
		removed = true
	}
	if !removed || !s.firewallIsWhitelist(ctx) {
		return nil
	}
	after, err := s.st.ListFirewallRules(ctx)
	if err != nil {
		return err
	}
	return s.applyWithRules(ctx, toFirewallRules(after))
}

// isLoopbackBind 判断监听地址是否只绑回环。空串代表 0.0.0.0，是对外的。
func isLoopbackBind(addr string) bool {
	switch addr {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	return ip != nil && ip.IsLoopback()
}
