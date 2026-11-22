// Package metrics 从 /proc 直接读取系统指标，不依赖 cgo 与第三方库。
//
// 之所以不用 gopsutil：这里只需要 6 个数字，自己读 /proc 更省内存，
// 也避免在低内存机器上多引一层依赖。非 Linux 平台返回零值并标记 Unsupported。
package metrics

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
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
	// MemCache 是「可回收的页缓存」，不计入 MemUsed。cgroup 口径下取
	// memory.stat 的 file 减 tmpfs 的 shmem；宿主机口径下取
	// Cached + Buffers + SReclaimable - Shmem。单独带出来是为了让概览能
	// 写清「已用 + 缓存 + 空闲 = 总量」，不至于把升级时写盘产生的缓存
	// 当成内存泄漏。
	MemCache uint64 `json:"mem_cache"`
	// MemFree 是扣掉缓存之后真正闲置的部分（total - used - cache），
	// 不是 /proc/meminfo 的 MemFree——后者把页缓存也算作空闲，会让人
	// 以为内存一直很宽裕。
	MemFree uint64 `json:"mem_free"`
	// Container 表示内存与 CPU 数据来自 cgroup 限额而非宿主机 /proc。
	// LXC 默认不挂 lxcfs 时，/proc 报的是宿主机数据，必须用 cgroup 纠正。
	Container bool `json:"container"`
	// MemParts 把 MemUsed 拆成能对上账的几块。
	//
	// 存在的理由：概览写「实际 313 MB」，用户往下看前 5 名进程只有 133 MB，
	// 第一反应是统计错了。其实差额是另外几十个小进程 + 内核 slab / 页表 /
	// 内核栈 + tmpfs，这些都不该出现在「进程排行」里，但必须能看到去处，
	// 否则这个数永远像凭空冒出来的。
	MemParts *MemParts `json:"mem_parts,omitempty"`
}

// MemParts 是「实际占用」的构成，四项之和等于 MemUsed。
type MemParts struct {
	// Anon 进程匿名页：堆、栈、Go heap、mmap(MAP_ANON)。所有进程加总，不只前 5。
	Anon uint64 `json:"anon"`
	// Shmem tmpfs 与共享内存。没有 swap 时收不回来，所以算「实际」不算「缓存」。
	Shmem uint64 `json:"shmem"`
	// Kernel 不可回收的内核内存：slab（SUnreclaim）+ 内核栈 + 页表。
	Kernel uint64 `json:"kernel"`
	// Other 余项：per-cpu、vmalloc、驱动缓冲、预留等，通常不大。
	Other uint64 `json:"other"`
}

// Collector 维护上一次的 CPU 计数，用于计算占用率。
type Collector struct {
	mu       sync.Mutex
	lastIdle uint64
	lastAll  uint64
	primed   bool

	// cgroup 口径的 CPU 累计时间（容器里才用）
	cgPrimed bool
	cgLastU  uint64
	cgLastT  time.Time
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

	mem := memInfo()
	s.MemTotal, s.MemUsed, s.MemCache, s.MemFree = mem.Total, mem.Used, mem.Cache, mem.Free
	s.SwapTotal, s.SwapUsed = mem.SwapTotal, mem.SwapUsed
	s.MemParts = mem.parts()

	// 只有 cgroup 限额明显小于物理内存时才认为读到了容器视角。
	// 真机上 memory.max 要么是 "max"，要么等于物理内存，这里不会命中。
	// 用量取「current - 可回收文件页」：memory.current 里含 page cache，
	// 面板更新一次写十几 MB 二进制就会让它涨一大截，直接展示会把
	// 可回收的缓存报成进程占用。
	if lim, use, cache := cgroupMemDetail(); lim > 0 && lim < s.MemTotal {
		s.MemTotal, s.MemUsed, s.MemCache = lim, use, cache
		s.MemFree = memFreeOf(lim, use, cache)
		if use > lim {
			s.MemUsed = lim
		}
		s.Container = true
		// 容器口径下 /proc/meminfo 的 anon/slab 是宿主机的，必须换成
		// 本 cgroup 的 memory.stat。
		s.MemParts = cgroupMemParts(s.MemUsed)
	}

	c.mu.Lock()
	if s.Container {
		if pct, ok := c.cgroupPercentLocked(); ok {
			s.CPUPct = pct
		} else {
			s.CPUPct = c.cpuPercentLocked()
		}
	} else {
		s.CPUPct = c.cpuPercentLocked()
	}
	c.mu.Unlock()

	s.Load1, s.Load5, s.Load15 = loadAvg()
	s.UptimeSec = uptime()
	s.DiskTotal, s.DiskUsed = diskUsage("/")
	return s
}

// cgroupPercentLocked 用 cgroup 的累计 CPU 时间算容器自身的占用率。
// 分母是「墙钟时间 × 配额核数」，所以 1 核配额跑满就是 100%。
func (c *Collector) cgroupPercentLocked() (float64, bool) {
	usage, cores, ok := cgroupCPU()
	if !ok {
		return 0, false
	}
	now := time.Now()
	if !c.cgPrimed {
		c.cgLastU, c.cgLastT, c.cgPrimed = usage, now, true
		return 0, true
	}
	if usage < c.cgLastU {
		// 计数回退（容器迁移 / 计数重置），重新取基线
		c.cgLastU, c.cgLastT = usage, now
		return 0, true
	}
	dU := float64(usage - c.cgLastU)
	dT := now.Sub(c.cgLastT).Seconds() * 1e6
	c.cgLastU, c.cgLastT = usage, now
	if dT <= 0 {
		return 0, true
	}
	if cores <= 0 {
		// 没设 CPU 配额时，用可见核数当分母，口径与真机 /proc/stat 一致
		cores = float64(runtime.NumCPU())
	}
	if cores <= 0 {
		return 0, true
	}
	pct := dU / (dT * cores) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
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

// memStats 是 /proc/meminfo 换算出来的内存三档。单位字节。
type memStats struct {
	Total     uint64
	Used      uint64 // 实际占用：总量 - MemAvailable
	Cache     uint64 // 可回收页缓存
	Free      uint64 // 真正闲置：总量 - 占用 - 缓存
	SwapTotal uint64
	SwapUsed  uint64

	// 以下三项是 Used 的组成部分，从 /proc/meminfo 直接读到。
	Anon   uint64 // AnonPages
	Shmem  uint64 // Shmem（tmpfs / 共享内存）
	Kernel uint64 // SUnreclaim + KernelStack + PageTables
}

// parts 把 memStats 里的构成转成对外结构，余项兜在 Other 里。
func (m memStats) parts() *MemParts {
	p := &MemParts{Anon: m.Anon, Shmem: m.Shmem, Kernel: m.Kernel}
	sum := p.Anon + p.Shmem + p.Kernel
	if sum < m.Used {
		p.Other = m.Used - sum
	}
	return p
}

func memInfo() memStats {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memStats{}
	}
	return parseMemInfo(string(raw))
}

// parseMemInfo 把 /proc/meminfo 的内容拆成「实际占用 / 可回收缓存 / 空闲」。
//
// 为什么不用 MemFree 当空闲：内核会拿空闲内存去做页缓存，MemFree 长期只剩
// 几十 MB，机器明明没事却像要爆了。所以沿用
//
//	已用 = 总量 - MemAvailable
//
// 再把可回收的那部分单独摘出来放在中间一档：
//
//	缓存 = Cached + Buffers + SReclaimable - Shmem
//	空闲 = 总量 - 已用 - 缓存
//
// 减 Shmem 是因为 tmpfs 占住的页在没有 swap 时收不回来，算进「可回收」
// 会让人以为随时能还（与 cgroup 口径的 file - shmem 保持一致）。
func parseMemInfo(raw string) memStats {
	vals := parseMeminfoVals(raw)

	var m memStats
	m.Total = vals["MemTotal"]

	avail, ok := vals["MemAvailable"]
	if !ok {
		// 3.14 之前的内核没有 MemAvailable，退回旧公式
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	if m.Total > avail {
		m.Used = m.Total - avail
	}

	cache := vals["Cached"] + vals["Buffers"] + vals["SReclaimable"]
	if shm := vals["Shmem"]; shm < cache {
		cache -= shm
	} else {
		cache = 0
	}
	// MemAvailable 的估算偏保守，used 有可能把一部分本该算缓存的页也
	// 算进去；不夹一下会出错负数（uint64 下直接变成天文数字）。
	if cache > m.Total-m.Used {
		cache = m.Total - m.Used
	}
	m.Cache = cache
	m.Free = m.Total - m.Used - m.Cache

	// 「实际占用」的构成。四项之和略小于 Used 是正常的：剩余部分是 per-cpu、
	// vmalloc、驱动私有缓冲这类没有独立 meminfo 字段的东西，统一记到 Other。
	m.Anon = vals["AnonPages"]
	m.Shmem = vals["Shmem"]
	m.Kernel = vals["SUnreclaim"] + vals["KernelStack"] + vals["PageTables"]
	if sum := m.Anon + m.Shmem + m.Kernel; sum > m.Used && sum > 0 {
		// MemAvailable 的估算会让 Used 偏小；按比例压到 Used 以内，
		// 否则前端会出现「构成比总数还大」这种一眼假的数据。
		// 最后一项取余数，避免整除截断让四项之和差几个字节。
		m.Anon = m.Used * m.Anon / sum
		m.Shmem = m.Used * m.Shmem / sum
		m.Kernel = m.Used - m.Anon - m.Shmem
	}

	m.SwapTotal = vals["SwapTotal"]
	if m.SwapTotal > vals["SwapFree"] {
		m.SwapUsed = m.SwapTotal - vals["SwapFree"]
	}
	return m
}

// memFreeOf 算限额里还剩多少，同样要防 uint64 下溢。
func memFreeOf(total, used, cache uint64) uint64 {
	if used >= total {
		return 0
	}
	if cache >= total-used {
		return 0
	}
	return total - used - cache
}

// parseMeminfoVals 把 meminfo 的文本读成 键 → 字节数。
func parseMeminfoVals(raw string) map[string]uint64 {
	vals := make(map[string]uint64, 32)
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		rest := strings.Fields(line[i+1:])
		if len(rest) == 0 {
			continue
		}
		v, err := strconv.ParseUint(rest[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo 单位是 kB
		vals[line[:i]] = v * 1024
	}
	return vals
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
