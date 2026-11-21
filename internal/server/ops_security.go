package server

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/logs"
)

// securityRecord 是「安全记录」表格的一行：一个来源 IP 在某个入口上的攻击汇总。
type securityRecord struct {
	Source string `json:"source"` // ssh / panel / web
	IP     string `json:"ip"`
	Count  int    `json:"count"`
	Last   string `json:"last"`
	Detail string `json:"detail"`
	Banned bool   `json:"banned"`
}

type sourceStat struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Count     int    `json:"count"`
	Extra     string `json:"extra,omitempty"`
}

// handleSecurityRecords 汇总三个入口的攻击痕迹：
//
//	ssh   —— journald 里的认证失败（爆破）
//	panel —— 审计表里的登录失败 / 触发限流（撞库）
//	web   —— Nginx/Apache 访问日志里的扫描器探测
//
// 三个数据源互相独立，任何一个不可用都不影响其余两个，
// 所以每个源单独带 supported/reason，前端按源显示「不可用」而不是整页报错。
func (s *Server) handleSecurityRecords(w http.ResponseWriter, r *http.Request) {
	minutes := 1440
	if v := r.URL.Query().Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 10080 {
			minutes = n
		}
	}
	window := time.Duration(minutes) * time.Minute
	now := time.Now()

	records := make([]securityRecord, 0, 64)
	resp := map[string]any{
		"window_min":  minutes,
		"threshold":   s.cfg.SSHGuardThreshold,
		"guard":       s.cfg.SSHGuardEnabled,
		"ban_minutes": s.banMinutes(),
	}

	// --- SSH 爆破 ---
	sshStat := sourceStat{}
	if items, total, err := logs.SSHAttackers(window, 50); err != nil {
		sshStat.Reason = err.Error()
	} else {
		sshStat.Supported = true
		sshStat.Count = total
		for _, it := range items {
			records = append(records, securityRecord{
				Source: "ssh", IP: it.IP, Count: it.Count, Last: it.Last, Detail: it.Detail,
			})
		}
	}
	resp["ssh"] = sshStat

	// --- 面板撞库 ---
	panelStat := sourceStat{Supported: true}
	if rows, err := s.st.SecurityLogins(r.Context(), now.Add(-window)); err != nil {
		panelStat.Supported = false
		panelStat.Reason = err.Error()
	} else {
		for _, a := range rows {
			if a.IP == "" {
				continue
			}
			detail := "密码错误 " + strconv.Itoa(a.Failed) + " 次"
			if a.Blocked > 0 {
				detail += "，触发限流 " + strconv.Itoa(a.Blocked) + " 次"
			}
			panelStat.Count += a.Failed + a.Blocked
			last := ""
			if a.LastUnix > 0 {
				last = time.Unix(a.LastUnix, 0).Format("2006-01-02 15:04:05")
			}
			records = append(records, securityRecord{
				Source: "panel", IP: a.IP, Count: a.Failed + a.Blocked, Last: last, Detail: detail,
			})
		}
	}
	resp["panel"] = panelStat

	// --- Web 扫描探测 ---
	webStat := sourceStat{}
	if items, files, total, err := logs.WebScans(window, 50); err != nil {
		webStat.Reason = err.Error()
	} else {
		webStat.Supported = true
		webStat.Count = total
		webStat.Extra = strconv.Itoa(len(files)) + " 个访问日志"
		for _, it := range items {
			records = append(records, securityRecord{
				Source: "web", IP: it.IP, Count: it.Count, Last: it.Last, Detail: it.Detail,
			})
		}
	}
	resp["web"] = webStat

	// 已封禁的 IP 打标，前端就不用再点一次「封禁」才发现已经封过
	banned := map[string]bool{}
	if bans, err := firewall.ListBans(); err == nil {
		for _, b := range bans {
			banned[b.IP] = true
		}
	}
	for i := range records {
		records[i].Banned = banned[records[i].IP]
	}

	// 按次数排；次数相同按时间近的在前
	sort.Slice(records, func(i, j int) bool {
		if records[i].Count != records[j].Count {
			return records[i].Count > records[j].Count
		}
		return records[i].Last > records[j].Last
	})
	resp["records"] = records
	ok(w, resp)
}
