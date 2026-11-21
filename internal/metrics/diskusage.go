package metrics

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 磁盘占用分析的取舍
//
// 「磁盘用了 80%」只告诉用户一个总数，真正要知道的是谁吃掉的。
// 这就要遍历目录树——代价远高于读一个 statfs：几十万个文件在低配
// VPS 上要跑几秒到几十秒。因此：
//   - 扫描是异步的，HTTP 请求只负责「开始」和「看进度」；
//   - 结果按路径缓存 3 分钟，来回翻同一层目录不必重扫；
//   - 同一路径同时只跑一个任务，重复点击只会看到同一份进度。
const (
	usageCacheTTL = 3 * time.Minute
	usageTimeout  = 3 * time.Minute
	usageMaxFiles = 4 << 20 // 400 万文件后停下，避免在异常挂载上跑到天荒地老
	// 同时最多跑两个扫描：一个扫 /，一个钻子目录，够了。
	// 不限并发的话，连点不同路径就能同时起几十个 walker，把磁盘 IO 打满。
	usageMaxConcurrent = 2
)

// DirUsage 是某个一级子项占用的空间。
type DirUsage struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  uint64 `json:"size"`
	Files int    `json:"files"`
}

// UsageReport 是一次目录占用扫描的结果。
type UsageReport struct {
	Path  string     `json:"path"`
	Total uint64     `json:"total"`
	Files int        `json:"files"`
	Items []DirUsage `json:"items"`
	// Partial 表示扫描被超时或文件数上限截断，数字偏小。
	Partial   bool      `json:"partial"`
	ElapsedMS int64     `json:"elapsed_ms"`
	StartedAt time.Time `json:"started_at"`
}

// UsageStatus 是给前端的轮询响应。
type UsageStatus struct {
	State     string       `json:"state"` // running | done | failed
	Path      string       `json:"path"`
	Scanned   int64        `json:"scanned"`
	Bytes     uint64       `json:"bytes"`
	Current   string       `json:"current"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Report    *UsageReport `json:"report,omitempty"`
	Error     string       `json:"error,omitempty"`
}

type usageEntry struct {
	report *UsageReport
	at     time.Time
}

// UsageJob 是一次正在进行的扫描。
type UsageJob struct {
	id        string
	path      string
	startedAt time.Time

	mu      sync.Mutex
	scanned int64
	bytes   uint64
	current string
	done    bool
	report  *UsageReport
	err     error
}

// DiskScanner 负责调度目录占用扫描。
type DiskScanner struct {
	mu      sync.Mutex
	seq     uint64
	jobs    map[string]*UsageJob
	running map[string]*UsageJob // path -> 正在跑的 job
	cache   map[string]usageEntry
}

// NewDiskScanner 创建扫描器。
func NewDiskScanner() *DiskScanner {
	return &DiskScanner{
		jobs:    map[string]*UsageJob{},
		running: map[string]*UsageJob{},
		cache:   map[string]usageEntry{},
	}
}

// Clean 把用户输入的路径规范化，并挡掉相对路径与 ..
// 面板本身以 root 运行，这里不做 chroot 式的限制——能登进面板的人
// 本来就能用「文件」页面翻整个磁盘；但明确拒绝带 .. 的写法，
// 免得前端拼接时出现越界路径。
func cleanUsagePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		// "/" 单独放行：Windows 上 filepath.IsAbs("/") 是 false，
		// 但服务端的根目录永远写作 "/"。
		return "/", nil
	}
	// filepath.IsAbs 而不是判断前导 "/"：生产环境是 Linux，但本地开发
	// 在 Windows 上跑同一份代码，用平台自带的判定两边都能过。
	if !filepath.IsAbs(p) {
		return "", errors.New("路径必须是绝对路径")
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("路径非法")
	}
	return clean, nil
}

// Start 开始（或复用）一次扫描。
//
// refresh 为 false 时优先返回缓存：概览的 hover 每几秒就可能触发一次，
// 每次都重扫会把 IO 打满。
func (d *DiskScanner) Start(path string, refresh bool) (*UsageStatus, error) {
	clean, err := cleanUsagePath(path)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	if j, ok := d.running[clean]; ok {
		d.mu.Unlock()
		return j.status(), nil
	}

	var stale *UsageStatus
	if !refresh {
		if e, ok := d.cache[clean]; ok {
			stale = &UsageStatus{
				State: "done", Path: clean, Report: e.report,
				Scanned: int64(e.report.Files), Bytes: e.report.Total,
				ElapsedMS: e.report.ElapsedMS,
			}
			if time.Since(e.at) < usageCacheTTL {
				d.mu.Unlock()
				return stale, nil
			}
			// 过期：先把旧结果交出去，下面顺手起一个后台重扫，
			// 用户从别的页面切回来时不至于盯着空进度条。
		}
	}

	d.seq++
	j := &UsageJob{
		id:        "du-" + time.Now().Format("20060102T150405") + "-" + itoa64(d.seq),
		path:      clean,
		startedAt: time.Now(),
	}
	if len(d.running) >= usageMaxConcurrent {
		d.mu.Unlock()
		if stale != nil {
			return stale, nil
		}
		return nil, errors.New("已有目录在统计中，请稍后再试")
	}
	d.jobs[j.id] = j
	d.running[clean] = j
	d.evictLocked()
	d.mu.Unlock()

	go d.run(j)
	if stale != nil {
		return stale, nil
	}
	return j.status(), nil
}

func (d *DiskScanner) run(j *UsageJob) {
	ctx, cancel := context.WithTimeout(context.Background(), usageTimeout)
	defer cancel()

	report, err := scanTree(ctx, j)

	j.mu.Lock()
	j.done = true
	j.report, j.err = report, err
	j.current = ""
	j.mu.Unlock()

	d.mu.Lock()
	delete(d.running, j.path)
	if err == nil && report != nil {
		d.cache[j.path] = usageEntry{report: report, at: time.Now()}
	}
	d.mu.Unlock()
}

// Status 按 job id 查询进度；id 为空时按路径查最近一次。
func (d *DiskScanner) Status(id string) (*UsageStatus, bool) {
	d.mu.Lock()
	j, ok := d.jobs[id]
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	return j.status(), true
}

// Cached 返回某路径最近一次扫描结果（可能已过期）。
func (d *DiskScanner) Cached(path string) (*UsageReport, bool) {
	clean, err := cleanUsagePath(path)
	if err != nil {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.cache[clean]; ok {
		return e.report, true
	}
	return nil, false
}

// evictLocked 只保留最近 40 个 job，避免反复下钻把内存撑大。
func (d *DiskScanner) evictLocked() {
	const keep = 40
	if len(d.jobs) <= keep {
		return
	}
	type pair struct {
		id string
		at time.Time
	}
	all := make([]pair, 0, len(d.jobs))
	for id, j := range d.jobs {
		all = append(all, pair{id, j.startedAt})
	}
	sort.Slice(all, func(a, b int) bool { return all[a].at.After(all[b].at) })
	for i := keep; i < len(all); i++ {
		delete(d.jobs, all[i].id)
	}
}

func (j *UsageJob) progress(n int64, bytes uint64, current string) {
	j.mu.Lock()
	j.scanned += n
	j.bytes += bytes
	if current != "" {
		j.current = current
	}
	j.mu.Unlock()
}

func (j *UsageJob) status() *UsageStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := &UsageStatus{
		State:     "running",
		Path:      j.path,
		Scanned:   j.scanned,
		Bytes:     j.bytes,
		Current:   j.current,
		ElapsedMS: time.Since(j.startedAt).Milliseconds(),
	}
	if j.done {
		if j.err != nil {
			st.State = "failed"
			st.Error = j.err.Error()
			return st
		}
		st.State = "done"
		st.Report = j.report
	}
	return st
}

func itoa64(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
