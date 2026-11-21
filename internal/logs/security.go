package logs

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errNoJournal = errors.New("当前系统没有 journalctl，读不到 SSH 日志")
	errNoWebLog  = errors.New("没找到 Nginx / Apache 访问日志")
)

// Attacker 是按来源 IP 聚合后的攻击记录。
//
// 概览页只报「封禁数」看不出发生了什么；这里把 SSH 爆破、面板撞库、
// Web 扫描合成一个口径，用户一眼能看到「谁在打、打了多少次、打的是什么」。
type Attacker struct {
	IP     string `json:"ip"`
	Count  int    `json:"count"`
	Last   string `json:"last"`
	Detail string `json:"detail"`
	// Banned 由上层按防火墙封禁表回填，不在日志层判断。
	Banned bool `json:"banned"`
}

// sshWindowLines 取指定窗口内的 SSH 日志行。
// 单独抽出来是因为概览的「实时失败数」与「攻击来源排行」用的是同一份数据，
// 分两次调 journalctl 在低配机器上能明显感觉到卡。
func sshWindowLines(window time.Duration, limit int) ([]string, error) {
	if !hasJournalctl() {
		return nil, errNoJournal
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	if limit <= 0 || limit > 20000 {
		limit = 5000
	}
	since := time.Now().Add(-window).Format("2006-01-02 15:04:05")
	out, err := exec.Command("journalctl", "--no-pager", "-o", "short-iso",
		"--since", since, "-u", "ssh", "-u", "sshd", "-n", itoa(limit)).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	lines := make([]string, 0, 256)
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// SSHAttackers 汇总窗口内的 SSH 认证失败，按 IP 排序。
func SSHAttackers(window time.Duration, top int) ([]Attacker, int, error) {
	lines, err := sshWindowLines(window, 5000)
	if err != nil {
		return nil, 0, err
	}
	type acc struct {
		count int
		last  string
		users map[string]bool
	}
	m := map[string]*acc{}
	total := 0
	for _, line := range lines {
		ip, user, ok := parseSSHFailure(line)
		if !ok {
			continue
		}
		total++
		a := m[ip]
		if a == nil {
			a = &acc{users: map[string]bool{}}
			m[ip] = a
		}
		a.count++
		if ts := firstField(line); ts > a.last {
			a.last = ts
		}
		if user != "" && len(a.users) < 4 {
			a.users[user] = true
		}
	}
	out := make([]Attacker, 0, len(m))
	for ip, a := range m {
		names := make([]string, 0, len(a.users))
		for u := range a.users {
			names = append(names, u)
		}
		sort.Strings(names)
		detail := "认证失败"
		if len(names) > 0 {
			detail = "尝试用户 " + strings.Join(names, "、")
		}
		out = append(out, Attacker{IP: ip, Count: a.count, Last: a.last, Detail: detail})
	}
	sortAttackers(out)
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out, total, nil
}

// ---------- Web 访问日志里的扫描探测 ----------

// 扫描器最常敲的路径。命中一条就足以说明对方不是正常访客。
var scanPaths = []string{
	"/.env", "/.git/", "/.svn/", "/.aws/", "/.ssh/",
	"/wp-login.php", "/wp-admin", "/wp-content/plugins", "/xmlrpc.php",
	"/phpmyadmin", "/phpMyAdmin", "/pma/", "/myadmin",
	"/vendor/phpunit", "/actuator", "/solr/", "/console/",
	"/config.json", "/config.php.bak", "/web.config", "/server-status",
	"/cgi-bin/", "/HNAP1", "/boaform/", "/manager/html",
	"/owa/", "/autodiscover/", "/ecp/", "/remote/login",
	"/shell", "/cmd.exe", "/../../", "/etc/passwd",
}

// hardScanPaths 是正常用户永远不会访问的路径：哪怕返回 200 也算探测。
// 其余路径（/wp-admin、/manager/html 等）真的可能有站长在用，
// 只有吃到 4xx 才计入——否则会把自家 WordPress 后台当成攻击。
var hardScanPaths = map[string]bool{
	"/.env": true, "/.git/": true, "/.svn/": true, "/.aws/": true, "/.ssh/": true,
	"/phpmyadmin": true, "/phpMyAdmin": true, "/pma/": true,
	"/xmlrpc.php": true, "/HNAP1": true, "/boaform/": true, "/cgi-bin/": true,
	"/actuator": true, "/solr/": true, "/console/": true,
	"/vendor/phpunit": true, "/cmd.exe": true, "/../../": true, "/etc/passwd": true,
	"/shell": true, "/config.php.bak": true, "/server-status": true,
}

// 扫描器的 UA 特征。只在对端已经吃到 4xx 时才作为佐证，
// 否则 curl / python-requests 这类正常调试也会被算成攻击。
var scanUA = []string{
	"zgrab", "masscan", "nmap", "sqlmap", "nikto", "dirbuster", "gobuster",
	"nuclei", "censys", "shodan", "netsparker", "acunetix", "nmap scripting",
	"python-requests", "go-http-client", "curl/", "wget/", "libwww-perl",
}

var reAccessLine = regexp.MustCompile(`^(\S+) \S+ \S+ \[([^\]]+)\] "([A-Z]+) (\S+)[^"]*" (\d{3}) \S+ "([^"]*)" "([^"]*)"`)

// accessHit 是一条解析后的访问日志。
type accessHit struct {
	IP     string
	Time   time.Time
	Method string
	Path   string
	UA     string
	Code   int
}

// parseAccessLine 解析 combined 格式。解析不出来（日志格式改过、
// 自定义 log_format）就返回 false，直接跳过——不猜。
func parseAccessLine(line string) (accessHit, bool) {
	mt := reAccessLine.FindStringSubmatch(line)
	if mt == nil {
		return accessHit{}, false
	}
	ts, err := time.Parse("02/Jan/2006:15:04:05 -0700", mt[2])
	if err != nil {
		return accessHit{}, false
	}
	code, _ := strconv.Atoi(mt[5])
	return accessHit{IP: mt[1], Time: ts, Method: mt[3], Path: mt[4], Code: code, UA: mt[7]}, true
}

// detectScan 判断这条访问是不是扫描 / 探测，返回命中的路径特征与 UA 特征。
// 扫描器 UA 一定算；命中扫描路径要求「4xx 或属于正常用户绝不会访问的路径」，
// 否则会把站长登录 WordPress 后台（/wp-admin 返回 200）误记成攻击。
func detectScan(h accessHit) (hitPath, hitUA string, ok bool) {
	lowPath := strings.ToLower(h.Path)
	for _, p := range scanPaths {
		if strings.Contains(lowPath, strings.ToLower(p)) {
			hitPath = p
			break
		}
	}
	if h.Code >= 400 {
		lowUA := strings.ToLower(h.UA)
		for _, u := range scanUA {
			if strings.Contains(lowUA, u) {
				hitUA = u
				break
			}
		}
	}
	if hitUA != "" {
		return hitPath, hitUA, true
	}
	if hitPath != "" && (h.Code >= 400 || hardScanPaths[hitPath]) {
		return hitPath, hitUA, true
	}
	return "", "", false
}

// webLogCandidates 是常见发行版 / 宝塔 / 军哥 LNMP 的访问日志位置。
func webLogCandidates() []string {
	var out []string
	out = append(out,
		"/var/log/nginx/access.log",
		"/www/wwwlogs/access.log",
		"/var/log/httpd/access_log",
		"/var/log/apache2/access.log",
	)
	// 宝塔把每个站点写成 /www/wwwlogs/<域名>.log
	if items, err := filepath.Glob("/www/wwwlogs/*.log"); err == nil && len(items) > 0 {
		if len(items) > 20 {
			items = items[:20]
		}
		out = append(out, items...)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range out {
		if seen[p] {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

// WebScans 从访问日志里挑出扫描 / 探测请求，按来源 IP 聚合。
// 返回的 files 是实际读到的日志，便于前端说明数据来源。
func WebScans(window time.Duration, top int) ([]Attacker, []string, int, error) {
	files := webLogCandidates()
	if len(files) == 0 {
		return nil, nil, 0, errNoWebLog
	}
	if window <= 0 {
		window = 24 * time.Hour
	}
	since := time.Now().Add(-window)

	type acc struct {
		count int
		last  time.Time
		paths map[string]bool
		kinds map[string]bool
	}
	m := map[string]*acc{}
	total := 0

	for _, f := range files {
		lines, _, err := tailFile(f, 4000)
		if err != nil {
			continue
		}
		for _, line := range lines {
			h, ok := parseAccessLine(line)
			if !ok || h.Time.Before(since) {
				continue
			}
			hitPath, _, suspicious := detectScan(h)
			if !suspicious {
				continue
			}
			ip, ts, method, lowPath := h.IP, h.Time, h.Method, strings.ToLower(h.Path)
			total++
			a := m[ip]
			if a == nil {
				a = &acc{paths: map[string]bool{}, kinds: map[string]bool{}}
				m[ip] = a
			}
			a.count++
			if ts.After(a.last) {
				a.last = ts
			}
			if hitPath != "" && len(a.paths) < 4 {
				a.paths[hitPath] = true
			}
			switch {
			case strings.Contains(lowPath, ".env"):
				a.kinds["探测 .env"] = true
			case strings.Contains(lowPath, "wp-") || strings.Contains(lowPath, "xmlrpc"):
				a.kinds["WordPress 扫描"] = true
			case strings.Contains(lowPath, "phpmyadmin") || strings.Contains(lowPath, "pma/"):
				a.kinds["探测 phpMyAdmin"] = true
			case strings.Contains(lowPath, ".."):
				a.kinds["路径穿越"] = true
			default:
				a.kinds[method+" 探测"] = true
			}
		}
	}

	out := make([]Attacker, 0, len(m))
	for ip, a := range m {
		var paths, kinds []string
		for p := range a.paths {
			paths = append(paths, p)
		}
		for k := range a.kinds {
			kinds = append(kinds, k)
		}
		sort.Strings(paths)
		sort.Strings(kinds)
		detail := strings.Join(kinds, "、")
		if len(paths) > 0 {
			detail += "（" + strings.Join(paths, " ") + "）"
		}
		last := ""
		if !a.last.IsZero() {
			last = a.last.Format("2006-01-02 15:04:05")
		}
		out = append(out, Attacker{IP: ip, Count: a.count, Last: last, Detail: detail})
	}
	sortAttackers(out)
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out, files, total, nil
}

func sortAttackers(items []Attacker) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Last > items[j].Last
	})
}
