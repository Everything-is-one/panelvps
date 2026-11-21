// Package metrics 从 /proc 直接读取系统指标，不依赖 cgo 与第三方库。
//
// 之所以不用 gopsutil：这里只需要 6 个数字，自己读 /proc 更省内存，
// 也避免在 1GB 机器上多引一层依赖。非 Linux 平台返回零值并标记 Unsupported。
package metrics

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Snapshot 是一次采样结果。所有容量单位为字节。
type Snapshot struct {
	CPUPct     float64 `json:"cpu_pct"`
	MemTotal   uint64  `json:"mem_total"`
	MemUsed    uint64  `json:"mem_used"`
	SwapTotal  uint64  `json:"swap_total"`
	SwapUsed   uint64  `json:"swap_used"`
	DiskTotal  uint64  `json:"disk_total"`
	DiskUsed   uint64  `json:"disk_used"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`
	UptimeSec  uint64  `json:"uptime_sec"`
	Supported  bool    `json:"supported"`
	PanelRSS   uint64  `json:"panel_rss"`
	Goroutines int     `json:"goroutines"`
}

// Collector 维护上一次的 CPU 计数，用于计算占用率。
type Collector struct {
	mu       sync.Mutex
	lastIdle uint64
	lastAll  uint64
	primed   bool
}

// New 创建采集器。
func New() *Collector { return &Collector{} }

// Collect 采集一次。CPU 占用需要两次采样之间的差值，
// 首次调用返回 0（并记录基线），之后的调用才有意义。
func (c *Collector) Collect() Snapshot {
	s := Snapshot{
		Goroutines: runtime.NumGoroutine(),
		PanelRSS:   panelRSS(),
	}
	if runtime.GOOS != "linux" {
		return s
	}
	s.Supported = true

	c.mu.Lock()
	s.CPUPct = c.cpuPercentLocked()
	c.mu.Unlock()

	s.MemTotal, s.MemUsed, s.SwapTotal, s.SwapUsed = memInfo()
	s.Load1, s.Load5, s.Load15 = loadAvg()
	s.UptimeSec = uptime()
	s.DiskTotal, s.DiskUsed = diskUsage("/")
	return s
}

func (c *Collector) cpuPercentLocked() float64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 8 || fields[0] != "cpu" {
		return 0
	}

	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0
		}
		total += v
		// idle + iowait 视为空闲
		if i == 4 || i == 5 {
			idle += v
		}
	}

	if !c.primed {
		c.lastAll, c.lastIdle, c.primed = total, idle, true
		return 0
	}

	dAll := float64(total - c.lastAll)
	dIdle := float64(idle - c.lastIdle)
	c.lastAll, c.lastIdle = total, idle
	if dAll <= 0 {
		return 0
	}
	pct := (dAll - dIdle) / dAll * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func memInfo() (total, used, swapTotal, swapUsed uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()

	vals := make(map[string]uint64, 8)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := line[:i]
		rest := strings.Fields(line[i+1:])
		if len(rest) == 0 {
			continue
		}
		v, err := strconv.ParseUint(rest[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo 单位是 kB
		vals[key] = v * 1024
	}

	total = vals["MemTotal"]
	avail, ok := vals["MemAvailable"]
	if !ok {
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	if total > avail {
		used = total - avail
	}
	swapTotal = vals["SwapTotal"]
	if swapTotal > vals["SwapFree"] {
		swapUsed = swapTotal - vals["SwapFree"]
	}
	return
}

func loadAvg() (a, b, c float64) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return
	}
	a, _ = strconv.ParseFloat(fields[0], 64)
	b, _ = strconv.ParseFloat(fields[1], 64)
	c, _ = strconv.ParseFloat(fields[2], 64)
	return
}

func uptime() uint64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// panelRSS 读取面板自身常驻内存，用于验证「空载 ≤ 50MB」这条硬指标。
func panelRSS() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return v * 1024
	}
	return 0
}
