// Package logs 读取面板、Web 服务、Xray 与认证日志。
//
// 只允许固定来源，不接受任意路径——日志里常有令牌与内网地址，
// 一个「读任意文件」的日志接口等于把文件管理模块的沙箱绕开了。
package logs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrUnknownSource 表示来源不在白名单内。
var ErrUnknownSource = errors.New("未知的日志来源")

// Source 描述一个可读取的日志来源。
type Source struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Kind    string `json:"kind"` // journal | file
	Detail  string `json:"detail"`
	Present bool   `json:"present"`
	// Size 与 ModTime 只有文件来源有：journal 的落盘量由 journald 统一管理，
	// 单个单元占多少没有便宜的查法（要跑一遍 journalctl --output=json），
	// 所以那类来源留空，界面上改显示 journald 的总占用。
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"mtime,omitempty"`
}

// Result 是一次读取结果。
type Result struct {
	Source  string   `json:"source"`
	Label   string   `json:"label"`
	Lines   []string `json:"lines"`
	Note    string   `json:"note,omitempty"`
	HasMore bool     `json:"has_more"`
	// Size 是来源文件此刻的字节数，前端用它实时显示「当前多大」——
	// 每次刷新就能跟着涨，不用再单独请求一次来源列表（那会跑 systemctl show）。
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"mtime,omitempty"`
}

// Options 是读取参数。
type Options struct {
	// DataDir 用于定位面板自身可能落盘的日志。
	DataDir string
	// Lines 期望行数，默认 200，上限 2000。
	Lines int
}

// catalog 是唯一的来源定义处。
func catalog(dataDir string) []struct {
	src  Source
	file string
	unit []string
} {
	// 面板自己写的日志文件优先：服务单元有 ProtectSystem 沙箱，部分容器里
	// 从沙箱内部执行 journalctl -u panelvps 只会得到 "-- No entries --"，
	// 用户看到的就是一个空白的日志页。
	panelSrc := Source{Key: "panel", Label: "面板服务", Kind: "journal", Detail: "journalctl -u panelvps"}
	var panelFile string
	var panelUnit = []string{"panelvps"}
	if logPath := filepath.Join(dataDir, "panel.log"); dataDir != "" {
		if st, err := os.Stat(logPath); err == nil && !st.IsDir() {
			panelSrc.Kind = "file"
			panelSrc.Detail = logPath
			panelFile = logPath
			panelUnit = nil
		}
	}

	// Xray 同理：面板 fork 的是 `xray run` 子进程，不是 systemd 单元，
	// journalctl -u xray 在托管模式下永远为空。有我们写的文件就优先读文件。
	xraySrc := Source{Key: "xray", Label: "Xray 内核", Kind: "journal", Detail: "journalctl -u xray"}
	var xrayFile string
	var xrayUnit = []string{"xray"}
	if logPath := filepath.Join(dataDir, "xray.log"); dataDir != "" {
		if st, err := os.Stat(logPath); err == nil && !st.IsDir() {
			xraySrc.Kind = "file"
			xraySrc.Detail = logPath
			xrayFile = logPath
			xrayUnit = nil
		}
	}

	return []struct {
		src  Source
		file string
		unit []string
	}{
		{panelSrc, panelFile, panelUnit},
		{xraySrc, xrayFile, xrayUnit},
		{Source{Key: "auth", Label: "SSH 登录", Kind: "journal", Detail: "journalctl -u ssh"}, "", []string{"ssh", "sshd"}},
		{Source{Key: "nginx-access", Label: "Nginx 访问", Kind: "file", Detail: "/var/log/nginx/access.log"},
			"/var/log/nginx/access.log", nil},
		{Source{Key: "nginx-error", Label: "Nginx 错误", Kind: "file", Detail: "/var/log/nginx/error.log"},
			"/var/log/nginx/error.log", nil},
		{Source{Key: "syslog", Label: "系统日志", Kind: "journal", Detail: "journalctl -n 200"}, "", nil},
	}
}

// Sources 返回来源清单及可用性。
func Sources(dataDir string) ([]Source, error) {
	var out []Source
	for _, item := range catalog(dataDir) {
		s := item.src
		if s.Kind == "file" {
			if st, err := os.Stat(item.file); err == nil && !st.IsDir() {
				s.Present = true
				s.Size = st.Size()
				s.ModTime = st.ModTime().UTC().Format(time.RFC3339)
			}
		} else {
			s.Present = hasJournalctl()
			if s.Present && len(item.unit) > 0 {
				// 单元不存在时 journalctl 会报错，用 show 判断更准确
				args := append([]string{"show"}, item.unit[0])
				if err := exec.Command("systemctl", args...).Run(); err != nil {
					s.Present = false
				}
			}
		}
		out = append(out, s)
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		for i := range out {
			if out[i].Kind == "journal" {
				out[i].Present = false
			}
		}
	}
	return out, nil
}

func hasJournalctl() bool {
	_, err := exec.LookPath("journalctl")
	return err == nil
}

// Read 读取指定来源的末尾若干行。
func Read(source string, opt Options) (*Result, error) {
	if opt.Lines <= 0 {
		opt.Lines = 200
	}
	if opt.Lines > 2000 {
		opt.Lines = 2000
	}

	for _, item := range catalog(opt.DataDir) {
		if item.src.Key != source {
			continue
		}
		res := &Result{Source: item.src.Key, Label: item.src.Label, Lines: []string{}}
		if item.src.Kind == "file" {
			if st, err := os.Stat(item.file); err == nil && !st.IsDir() {
				res.Size = st.Size()
				res.ModTime = st.ModTime().UTC().Format(time.RFC3339)
			}
			lines, more, err := tailFile(item.file, opt.Lines)
			if err != nil {
				return nil, err
			}
			res.Lines = lines
			res.HasMore = more
			return res, nil
		}

		lines, note, err := readJournal(item.unit, opt.Lines)
		if err != nil {
			return nil, err
		}
		if lines == nil {
			lines = []string{} // 保持 JSON 里是 []，不是 null
		}
		res.Lines = lines
		res.Note = note
		res.HasMore = len(lines) >= opt.Lines
		return res, nil
	}
	return nil, ErrUnknownSource
}

func readJournal(units []string, lines int) ([]string, string, error) {
	if !hasJournalctl() {
		return nil, "当前系统没有 journalctl，无法读取该来源", nil
	}
	args := []string{"--no-pager", "-n", itoa(lines), "-o", "short-iso"}
	for _, u := range units {
		args = append(args, "-u", u)
	}
	cmd := exec.Command("journalctl", args...)
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, "读取失败：" + msg, nil
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return []string{}, "该来源暂无日志", nil
	}
	return strings.Split(text, "\n"), "", nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// tailFile 用「seek 到末尾 512KB」的方式读大文件，不把整个日志读进内存。
func tailFile(path string, lines int) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	const window = 512 << 10
	start := int64(0)
	hasMore := false
	if fi.Size() > window {
		start = fi.Size() - window
		hasMore = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var all []string
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	if start > 0 && len(all) > 0 {
		all = all[1:] // 丢掉可能被截断的首行
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
		hasMore = true
	}
	return all, hasMore, nil
}

// ---------- SSH 失败分析 ----------

// FailEvent 是一次认证失败。
type FailEvent struct {
	Time    string `json:"time"`
	IP      string `json:"ip"`
	User    string `json:"user"`
	Message string `json:"message"`
}

// SSHFailures 解析最近窗口内的 SSH 失败事件，并按 IP 汇总。
func SSHFailures(window time.Duration, limit int) ([]FailEvent, map[string]int, error) {
	if !hasJournalctl() {
		return nil, nil, errors.New("当前系统没有 journalctl")
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	since := time.Now().Add(-window).Format("2006-01-02 15:04:05")
	cmd := exec.Command("journalctl", "--no-pager", "-o", "short-iso",
		"--since", since, "-u", "ssh", "-u", "sshd", "-n", itoa(limit))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, nil, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, nil, err
	}

	var events []FailEvent
	counts := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		ip, user, ok := parseSSHFailure(line)
		if !ok {
			continue
		}
		counts[ip]++
		if len(events) < 200 {
			events = append(events, FailEvent{
				Time:    firstField(line),
				IP:      ip,
				User:    user,
				Message: clip(line, 200),
			})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time > events[j].Time })
	return events, counts, sc.Err()
}

var (
	reFailedPassword = regexp.MustCompile(`Failed password for (?:invalid user )?(\S+) from ([0-9a-fA-F:.]+)`)
	reInvalidUser    = regexp.MustCompile(`Invalid user (\S+) from ([0-9a-fA-F:.]+)`)
	reFailedPubkey   = regexp.MustCompile(`Failed publickey for (?:invalid user )?(\S+) from ([0-9a-fA-F:.]+)`)
)

func parseSSHFailure(line string) (ip, user string, ok bool) {
	for _, re := range []*regexp.Regexp{reFailedPassword, reInvalidUser, reFailedPubkey} {
		if m := re.FindStringSubmatch(line); len(m) == 3 {
			return m[2], m[1], true
		}
	}
	return "", "", false
}

func firstField(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------- 清理 ----------

// CleanResult 是一次清理的结果。
type CleanResult struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Before int64  `json:"before"`
	After  int64  `json:"after"`
	Freed  int64  `json:"freed"`
	Note   string `json:"note"`
}

// minJournalKeep 是 journald 回收的下限。再往下压会连当次会话的记录一起删掉，
// 出问题时反而无从下手；50MB 上下的日志对 960MB 的机器也不算负担。
const minJournalKeep = 8 << 20

// Clean 清理指定来源。
//
// 文件来源就地截断成 0 字节：nginx 与面板都还拿着这个 inode 在写，
// 删文件之后它们会往一个谁也没法再打开的幽灵文件里写，日志从此消失。
//
// journal 来源交给 journald 自己回收（--vacuum-size），target 单位字节，
// 只对 journal 有效，<=0 时取 50MB。
func Clean(source string, opt Options, target int64) (*CleanResult, error) {
	for _, item := range catalog(opt.DataDir) {
		if item.src.Key != source {
			continue
		}
		res := &CleanResult{Source: item.src.Key, Kind: item.src.Kind}
		if item.src.Kind == "file" {
			st, err := os.Stat(item.file)
			if err != nil {
				return nil, errors.New("日志文件不存在：" + item.file)
			}
			if st.IsDir() {
				return nil, errors.New("来源路径是目录，拒绝清理")
			}
			res.Before = st.Size()
			if err := os.Truncate(item.file, 0); err != nil {
				return nil, errors.New("清空失败：" + err.Error())
			}
			res.Freed = res.Before
			if res.Before == 0 {
				res.Note = "文件本来就是空的"
			} else {
				res.Note = "已清空，文件保留（写入方不用重启）"
			}
			return res, nil
		}

		if !hasJournalctl() {
			return nil, errors.New("当前系统没有 journalctl，无法清理该来源")
		}
		if target <= 0 {
			target = 50 << 20
		}
		if target < minJournalKeep {
			target = minJournalKeep
		}
		res.Before = JournalUsage()
		out, err := vacuumJournal(target)
		if err != nil {
			return nil, err
		}
		res.After = JournalUsage()
		if res.Before > res.After {
			res.Freed = res.Before - res.After
		}
		res.Note = "journald 已回收到 " + mbText(res.After) +
			"，只保留最近的部分；当前活动文件要先轮转才参与回收"
		if out != "" {
			res.Note += "。journalctl：" + out
		}
		return res, nil
	}
	return nil, ErrUnknownSource
}

// JournalUsage 统计 journald 落盘占用的字节数。
//
// 不用 `journalctl --disk-usage`：它输出的是给人看的一句话（"take up 47.3M"），
// 带本地化，解析起来比遍历目录脆得多。/var/log/journal 是持久化目录，
// /run/log/journal 是 Storage=volatile 时的目录，两个都算。
func JournalUsage() int64 {
	var total int64
	for _, root := range []string{"/var/log/journal", "/run/log/journal"} {
		_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		})
	}
	return total
}

// vacuumJournal 把 journald 压到 target 字节以下。
func vacuumJournal(target int64) (string, error) {
	// 先轮转：正在写的活动文件不参与回收，不轮转基本等于什么都没做。
	// 部分容器里没有权限轮转，这里失败不算错，后面的 vacuum 照样能删归档段。
	_ = exec.Command("journalctl", "--rotate").Run()

	sizeArg := fmt.Sprintf("--vacuum-size=%dM", (target+(1<<20)-1)>>20)
	out, err := exec.Command("journalctl", sizeArg).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		msg := text
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New("journalctl 回收失败：" + clip(msg, 200))
	}
	// 输出是若干行统计（Vacuuming done, freed …），只留最后一行当提示。
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return clip(text, 160), nil
}

func mbText(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// DefaultNginxLogDir 便于前端提示日志位置。
func DefaultNginxLogDir() string { return filepath.Dir("/var/log/nginx/access.log") }
