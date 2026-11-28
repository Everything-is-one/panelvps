// Package junk 扫描并清理服务器上常见的磁盘垃圾。
//
// 与日志模块同一条原则：只认白名单。接口收的是「类别键」而不是路径，
// 每个类别的位置与删除方式都在包内写死。面板以 root 运行，
// 一个「删任意路径」的清理接口等于把文件管理模块的沙箱整个绕开。
//
// 另一个约束是「不碰正在写的东西」：日志归档只删已轮转的 .gz/.1，
// 临时文件只删 7 天未修改的顶层普通文件，回收站只动面板自己的目录。
// 清理失败不会中断整批：每个类别各自返回结果与错误，前端可以逐条展示。
package junk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Everything-is-one/panelvps/internal/logs"
)

const (
	// 扫描结果缓存：清理页来回切换不该每次重走 /root/.cache。
	cacheTTL = 2 * time.Minute
	// /tmp 里只碰「一周没动过」的文件：正在运行的进程刚写的临时文件
	// 可能只隔几秒，按天算的阈值才不会误删。
	tmpAge = 7 * 24 * time.Hour
	// journald 回收到 50MB，与日志页的「回收 journald」保持同一口径。
	journalTarget = 50 << 20
	// docker CLI 在守护进程卡住时会一直等，给个上限。
	dockerTimeout = 20 * time.Second
)

// Item 是一个垃圾类别的扫描结果。
type Item struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Detail 是给人看的位置说明（可能是多个路径）。
	Detail string `json:"detail"`
	// Hint 说明「清理会删掉什么、不会碰什么」。
	Hint string `json:"hint"`
	// Risk 为 safe / caution：caution 默认不勾选，确认框里额外提示。
	Risk      string `json:"risk"`
	Kind      string `json:"kind"`
	Bytes     int64  `json:"bytes"`
	Files     int    `json:"files"`
	Present   bool   `json:"present"`
	DefaultOn bool   `json:"default_on"`
	// Note 是扫描时的补充说明（Docker 不可用、没有悬空镜像等）。
	Note string `json:"note,omitempty"`
}

// Report 是一次完整扫描的结果。
type Report struct {
	Items     []Item    `json:"items"`
	Total     int64     `json:"total"`
	Files     int       `json:"files"`
	ScannedAt time.Time `json:"scanned_at"`
	ElapsedMS int64     `json:"elapsed_ms"`
}

// Status 是给前端的轮询响应。
type Status struct {
	State     string  `json:"state"` // running | done
	Done      int     `json:"done"`
	Total     int     `json:"total"`
	Current   string  `json:"current,omitempty"`
	ElapsedMS int64   `json:"elapsed_ms"`
	Report    *Report `json:"report,omitempty"`
}

// CleanItem 是单个类别的清理结果。
type CleanItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Freed int64  `json:"freed"`
	Files int    `json:"files"`
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// CleanReport 是一批清理的汇总。
type CleanReport struct {
	Freed int64       `json:"freed"`
	Files int         `json:"files"`
	Items []CleanItem `json:"items"`
}

// Options 是扫描与清理的参数。
type Options struct {
	// DataDir 用于定位面板自己的回收站。
	DataDir string
	// Now 便于测试注入固定时间；零值取 time.Now()。
	Now time.Time
	// roots 覆盖类别的根目录，只给测试用——生产路径不接受外部输入。
	roots map[string][]string
}

func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

func (o Options) rootsOf(key string, def ...string) []string {
	if v, ok := o.roots[key]; ok {
		return v
	}
	return def
}

// unsupported 判断当前是否处在「非 Linux 且没有测试覆盖路径」的环境。
//
// 生产只支持 Linux，但本地开发在 Windows 上跑同一份代码：`/tmp` 会被解释成
// C:\tmp、`/var/log` 变成 C:\var\log，扫描出来的是开发者自己的目录，
// 清理更是可能删掉本机文件。所以非 Linux 一律只报「不可用」，不做任何删除。
// 测试通过 Options.roots 覆盖根目录，属于显式白名单，照常执行。
func unsupported(opt Options) bool {
	return runtime.GOOS != "linux" && len(opt.roots) == 0
}

// category 是一个白名单条目。文件类（kind=files）用 roots + match 描述，
// journal / docker / trash 各有专用实现。
type category struct {
	key, label, detail, hint, risk string
	defaultOn                      bool
	kind                           string
	roots                          []string
	topOnly                        bool
	olderThan                      time.Duration
	skipPrefix                     []string
	match                          func(name string) bool
}

// 轮转归档的命名：syslog.1、dpkg.log.2.gz、nginx/access.log-20260101.gz。
// 只匹配这些后缀，正在写的 access.log / syslog 不动。
// 刻意不含 .bak：那不是 logrotate 的产物，多半是用户手工留的备份。
var reRotated = regexp.MustCompile(`(?i)(\.gz|\.xz|\.bz2|\.old|\.[1-9])$|-[0-9]{8}(\.gz)?$`)

func isRotatedLog(name string) bool {
	return reRotated.MatchString(name)
}

func catalog(opt Options) []category {
	// DataDir 为空时不能拼出相对路径 "trash"：那会跟着面板的工作目录跑，
	// 扫到 /opt/panelvps/trash 还只是误报，清起来就是删错地方。
	trash := ""
	trashDetail := "面板数据目录下的 trash/"
	if opt.DataDir != "" {
		trash = filepath.Join(opt.DataDir, "trash")
		trashDetail = trash
	}
	return []category{
		{
			key: "apt", label: "APT 安装包缓存",
			detail: "/var/cache/apt/archives/*.deb",
			hint:   "只删已下载的安装包，已安装的软件不受影响；下次装包会重新下载。",
			risk:   "safe", defaultOn: true, kind: "files",
			roots: opt.rootsOf("apt", "/var/cache/apt/archives"),
			match: func(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".deb") },
		},
		{
			key: "journal", label: "journald 日志",
			detail: "/var/log/journal（全机共用）",
			hint:   "用 journalctl --vacuum-size=50M 压到 50 MB 以下；其它服务的旧日志会一起被回收。",
			risk:   "caution", kind: "journal",
		},
		{
			key: "rotated", label: "轮转日志归档",
			detail: "/var/log 下的 .gz / .1 / .old 历史归档",
			hint:   "只删已轮转的归档，正在写的 access.log、syslog 不动。",
			risk:   "safe", defaultOn: true, kind: "files",
			roots:   opt.rootsOf("rotated", "/var/log", "/var/log/nginx"),
			topOnly: true, match: isRotatedLog,
		},
		{
			key: "tmp", label: "临时文件",
			detail: "/tmp、/var/tmp 中 7 天未修改的文件",
			hint:   "只删顶层普通文件，跳过 systemd-private-* 与正在使用的套接字。",
			risk:   "safe", defaultOn: true, kind: "files",
			roots:   opt.rootsOf("tmp", "/tmp", "/var/tmp"),
			topOnly: true, olderThan: tmpAge,
			skipPrefix: []string{"systemd-private-", "snap-private-tmp"},
		},
		{
			key: "crash", label: "崩溃转储",
			detail: "/var/crash/*.crash",
			hint:   "应用崩溃时留下的转储文件，删掉不影响运行。",
			risk:   "safe", defaultOn: true, kind: "files",
			roots:   opt.rootsOf("crash", "/var/crash"),
			topOnly: true,
			match:   func(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".crash") },
		},
		{
			key: "devcache", label: "开发工具缓存",
			detail: "/root/.cache（pip / uv / npm / go 编译缓存）",
			hint:   "删的是可重建的缓存：下次构建会慢一点，不会丢代码与依赖声明。",
			risk:   "safe", defaultOn: true, kind: "files",
			roots: opt.rootsOf("devcache",
				"/root/.cache/pip", "/root/.cache/uv", "/root/.cache/go-build",
				"/root/.npm/_cacache", "/root/.cache/yarn", "/root/go/pkg/mod/cache/download"),
		},
		{
			key: "docker", label: "Docker 悬空镜像",
			detail: "docker image prune（未打标签、也未被引用的层）",
			hint:   "只删悬空镜像；正在使用的镜像与容器不受影响。",
			risk:   "caution", kind: "docker",
		},
		{
			key: "trash", label: "面板回收站",
			detail: trashDetail,
			hint:   "清空面板回收站里已删除的文件；这是最后一步，之后无法恢复。",
			risk:   "caution", kind: "trash",
			roots: opt.rootsOf("trash", trash),
		},
	}
}

// ---------- 文件遍历 ----------

// walk 遍历一个文件类别的全部根目录。
//
// present 表示至少有一个根目录存在；readErrors 是读不了的子目录/文件数。
// 单个目录没权限不会中断整轮扫描（低配机器上 /var/log 里常有不可读的子目录）。
func walk(c category, now time.Time, fn func(path string, info fs.FileInfo) error) (present bool, readErrors int) {
	for _, root := range c.roots {
		if root == "" {
			continue
		}
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}
		present = true

		if c.topOnly {
			entries, err := os.ReadDir(root)
			if err != nil {
				readErrors++
				continue
			}
			for _, de := range entries {
				if de.IsDir() {
					continue
				}
				info, err := de.Info()
				if err != nil {
					readErrors++
					continue
				}
				name := de.Name()
				if !c.accept(now, name, info) {
					continue
				}
				if err := fn(filepath.Join(root, name), info); err != nil {
					readErrors++
				}
			}
			continue
		}

		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				readErrors++
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil {
				readErrors++
				return nil
			}
			if !c.accept(now, d.Name(), info) {
				return nil
			}
			if err := fn(path, info); err != nil {
				readErrors++
			}
			return nil
		})
		if err != nil {
			readErrors++
		}
	}
	return present, readErrors
}

// accept 决定一个文件是否属于该类别。
// 只认普通文件：符号链接、设备、FIFO、套接字都不碰——它们的大小没有意义，
// 删掉还可能让正在运行的服务直接失联。
func (c category) accept(now time.Time, name string, info fs.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	if c.olderThan > 0 && now.Sub(info.ModTime()) < c.olderThan {
		return false
	}
	for _, p := range c.skipPrefix {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if c.match != nil && !c.match(name) {
		return false
	}
	return true
}

func scanFiles(c category, opt Options) (bytes int64, files int, present bool, note string) {
	present, readErrors := walk(c, opt.now(), func(_ string, info fs.FileInfo) error {
		bytes += info.Size()
		files++
		return nil
	})
	if readErrors > 0 {
		note = "部分目录读不了（权限不足），数字偏小"
	}
	return
}

func cleanFiles(c category, opt Options) (freed int64, files int, note string, err error) {
	failed := 0
	_, _ = walk(c, opt.now(), func(path string, info fs.FileInfo) error {
		if rerr := os.Remove(path); rerr != nil {
			if !os.IsNotExist(rerr) {
				failed++
			}
			return nil
		}
		freed += info.Size()
		files++
		return nil
	})
	if failed > 0 {
		note = fmt.Sprintf("%d 个文件删除失败（权限或占用）", failed)
	}
	return
}

func cleanTrash(c category, opt Options) (freed int64, files int, note string, err error) {
	bytes, n, present, _ := scanFiles(c, opt)
	if !present {
		return 0, 0, "回收站不存在", nil
	}
	for _, root := range c.roots {
		if root == "" {
			continue
		}
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			return 0, 0, "", fmt.Errorf("读取回收站失败：%v", rerr)
		}
		for _, de := range entries {
			if rerr := os.RemoveAll(filepath.Join(root, de.Name())); rerr != nil {
				return 0, 0, "", fmt.Errorf("清空回收站失败：%v", rerr)
			}
		}
	}
	if bytes == 0 {
		note = "回收站本来就是空的"
	} else {
		note = "回收站已清空"
	}
	return bytes, n, note, nil
}

// ---------- journald ----------

func scanJournal() (bytes int64, files int, present bool, note string) {
	bytes = logs.JournalUsage()
	present = dirExists("/var/log/journal") || dirExists("/run/log/journal") || hasCmd("journalctl")
	if bytes == 0 && present {
		note = "journald 没有落盘占用"
	}
	return bytes, 0, present, note
}

func cleanJournal() (freed int64, files int, note string, err error) {
	before, after, note, err := logs.Vacuum(journalTarget)
	if err != nil {
		return 0, 0, "", err
	}
	if before > after {
		freed = before - after
	}
	if freed == 0 {
		note = "journald 已经在 50 MB 以下，没有可回收的段"
	}
	return freed, 0, note, nil
}

// ---------- Docker ----------

var reDockerSize = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgtp]?i?b)\s*$`)

// parseDockerSize 解析 docker 输出的容量（1.2GB / 800MB / 0B）。
// docker 用十进制单位；带 i 的（GiB）按 1024 算。
func parseDockerSize(s string) (int64, bool) {
	m := reDockerSize.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	var mult float64
	switch strings.ToLower(m[2]) {
	case "b":
		mult = 1
	case "kb":
		mult = 1e3
	case "mb":
		mult = 1e6
	case "gb":
		mult = 1e9
	case "tb":
		mult = 1e12
	case "pb":
		mult = 1e15
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	default:
		return 0, false
	}
	return int64(v * mult), true
}

func scanDocker() (bytes int64, files int, present bool, note string) {
	if !hasCmd("docker") {
		return 0, 0, false, "未安装 Docker"
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "ls",
		"--filter", "dangling=true", "--format", "{{.ID}}\t{{.Size}}").Output()
	if err != nil {
		return 0, 0, false, "Docker 不可用：" + clip(errText(err), 80)
	}
	present = true
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		files++
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			if n, ok := parseDockerSize(parts[1]); ok {
				bytes += n
			}
		}
	}
	if files == 0 {
		note = "没有悬空镜像"
	}
	return bytes, files, present, note
}

func cleanDocker() (freed int64, files int, note string, err error) {
	bytes, count, present, why := scanDocker()
	if !present {
		if why == "" {
			why = "Docker 不可用"
		}
		return 0, 0, "", errors.New(why)
	}
	if count == 0 {
		return 0, 0, "没有悬空镜像，未删除任何内容", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, cerr := exec.CommandContext(ctx, "docker", "image", "prune", "-f").CombinedOutput()
	if cerr != nil {
		return 0, 0, "", errors.New("docker image prune 失败：" + clip(errText2(cerr, out), 160))
	}
	return bytes, count, "已清理 " + strconv.Itoa(count) + " 个悬空镜像", nil
}

// ---------- 扫描调度 ----------

// Scanner 负责调度扫描与执行清理。
type Scanner struct {
	opt Options

	mu      sync.Mutex
	cur     *scanJob
	cache   *Report
	cacheAt time.Time
	// gen 用来让「清理前开始的扫描」不再写回缓存，否则清理完看到的还是旧数字。
	gen uint64
}

type scanJob struct {
	startedAt time.Time
	total     int
	gen       uint64

	mu       sync.Mutex
	done     int
	current  string
	report   *Report
	finished bool
}

// NewScanner 创建扫描器。
func NewScanner(opt Options) *Scanner {
	return &Scanner{opt: opt}
}

// Start 开始（或复用）一次扫描。refresh=false 时命中 2 分钟缓存直接返回。
func (s *Scanner) Start(refresh bool) *Status {
	s.mu.Lock()
	if s.cur != nil {
		st := s.cur.status()
		s.mu.Unlock()
		return st
	}
	if !refresh && s.cache != nil && time.Since(s.cacheAt) < cacheTTL {
		rep := s.cache
		s.mu.Unlock()
		return &Status{
			State: "done", Done: len(rep.Items), Total: len(rep.Items),
			Report: rep, ElapsedMS: rep.ElapsedMS,
		}
	}
	j := &scanJob{startedAt: time.Now(), total: len(catalog(s.opt)), gen: s.gen}
	s.cur = j
	s.mu.Unlock()

	go func() {
		rep := scanAll(s.opt, func(done int, current string) {
			j.mu.Lock()
			j.done, j.current = done, current
			j.mu.Unlock()
		})
		j.mu.Lock()
		j.finished, j.report, j.current = true, rep, ""
		j.mu.Unlock()

		s.mu.Lock()
		if s.gen == j.gen {
			s.cache, s.cacheAt = rep, time.Now()
		}
		if s.cur == j {
			s.cur = nil
		}
		s.mu.Unlock()
	}()
	return j.status()
}

func (j *scanJob) status() *Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := &Status{
		State:     "running",
		Done:      j.done,
		Total:     j.total,
		Current:   j.current,
		ElapsedMS: time.Since(j.startedAt).Milliseconds(),
	}
	if j.finished {
		st.State = "done"
		st.Report = j.report
		st.Current = ""
	}
	return st
}

func scanAll(opt Options, progress func(done int, current string)) *Report {
	start := time.Now()
	cats := catalog(opt)
	rep := &Report{Items: make([]Item, 0, len(cats)), ScannedAt: start}
	for i, c := range cats {
		if progress != nil {
			progress(i, c.label)
		}
		it := Item{
			Key: c.key, Label: c.label, Detail: c.detail, Hint: c.hint,
			Risk: c.risk, Kind: c.kind, DefaultOn: c.defaultOn,
		}
		if unsupported(opt) {
			it.Note = "清理功能仅支持 Linux"
			rep.Items = append(rep.Items, it)
			continue
		}
		switch c.kind {
		case "journal":
			it.Bytes, it.Files, it.Present, it.Note = scanJournal()
		case "docker":
			it.Bytes, it.Files, it.Present, it.Note = scanDocker()
		case "trash":
			it.Bytes, it.Files, it.Present, it.Note = scanFiles(c, opt)
		default:
			it.Bytes, it.Files, it.Present, it.Note = scanFiles(c, opt)
		}
		rep.Items = append(rep.Items, it)
		rep.Total += it.Bytes
		rep.Files += it.Files
	}
	if progress != nil {
		progress(len(cats), "")
	}
	// 大的排前面：用户打开这一页最先想知道「哪个最值得清」。
	sort.SliceStable(rep.Items, func(i, j int) bool { return rep.Items[i].Bytes > rep.Items[j].Bytes })
	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep
}

// Clean 按类别键执行清理。未知键直接拒绝，不做任何删除。
func (s *Scanner) Clean(keys []string) (*CleanReport, error) {
	if len(keys) == 0 {
		return nil, errors.New("请至少选择一项")
	}
	if unsupported(s.opt) {
		return nil, errors.New("清理功能仅在 Linux 上可用")
	}
	byKey := map[string]category{}
	for _, c := range catalog(s.opt) {
		byKey[c.key] = c
	}
	seen := map[string]bool{}
	var picked []category
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		c, ok := byKey[k]
		if !ok {
			return nil, fmt.Errorf("未知的清理类别：%s", k)
		}
		seen[k] = true
		picked = append(picked, c)
	}
	if len(picked) == 0 {
		return nil, errors.New("请至少选择一项")
	}

	rep := &CleanReport{Items: make([]CleanItem, 0, len(picked))}
	for _, c := range picked {
		res := CleanItem{Key: c.key, Label: c.label}
		freed, files, note, err := cleanCategory(c, s.opt)
		if err != nil {
			res.Error = err.Error()
		}
		res.Freed, res.Files, res.Note = freed, files, note
		rep.Freed += freed
		rep.Files += files
		rep.Items = append(rep.Items, res)
	}

	s.mu.Lock()
	s.gen++
	s.cache, s.cacheAt = nil, time.Time{}
	s.mu.Unlock()
	return rep, nil
}

func cleanCategory(c category, opt Options) (freed int64, files int, note string, err error) {
	switch c.kind {
	case "journal":
		return cleanJournal()
	case "docker":
		return cleanDocker()
	case "trash":
		return cleanTrash(c, opt)
	default:
		return cleanFiles(c, opt)
	}
}

// ---------- 小工具 ----------

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func errText(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return msg
		}
	}
	return err.Error()
}

func errText2(err error, out []byte) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return msg
	}
	return errText(err)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
