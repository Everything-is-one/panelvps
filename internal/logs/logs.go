// Package logs 读取面板、Web 服务、Xray 与认证日志。
//
// 只允许固定来源，不接受任意路径——日志里常有令牌与内网地址，
// 一个「读任意文件」的日志接口等于把文件管理模块的沙箱绕开了。
package logs

import (
	"bufio"
	"errors"
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
}

// Result 是一次读取结果。
type Result struct {
	Source  string   `json:"source"`
	Label   string   `json:"label"`
	Lines   []string `json:"lines"`
	Note    string   `json:"note,omitempty"`
	HasMore bool     `json:"has_more"`
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

// DefaultNginxLogDir 便于前端提示日志位置。
func DefaultNginxLogDir() string { return filepath.Dir("/var/log/nginx/access.log") }
