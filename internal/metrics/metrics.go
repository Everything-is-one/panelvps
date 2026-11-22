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
	// MemCache 是「内核认为能收回来」的那部分，不计入 MemUsed。
	//
	// 宿主机口径取 MemAvailable - MemFree，不是 meminfo 的
	// Cached + Buffers + SReclaimable - Shmem。两者差得可能很远：页缓存里
	// 有一部分（块设备元数据页、被映射钉住的页）内核不会算进 available，
	// 于是它既在「实际」里、又在全量缓存里，同一块内存被记两次，
	// 「空闲」被压掉一大截（实测少报 150MB）。
	//
	// cgroup 口径取 memory.stat 的 file 减 shmem。
	MemCache uint64 `json:"mem_cache"`
	// MemFree 是真正闲置的页，直接取 meminfo 的 MemFree，与 free(1) 的
	// free 列一致。三档满足 实际 + 缓存 + 空闲 = 总量。
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

// MemParts 是「实际占用」的构成，各项之和等于 MemUsed。
type MemParts struct {
	// Anon 进程匿名页：堆、栈、Go heap、mmap(MAP_ANON)。所有进程加总，不只前 5。
	Anon uint64 `json:"anon"`
	// Shmem tmpfs 与共享内存。没有 swap 时收不回来，所以算「实际」不算「缓存」。
	Shmem uint64 `json:"shmem"`
	// Kernel 不可回收的内核内存：slab（SUnreclaim）+ 内核栈 + 页表。
	Kernel uint64 `json:"kernel"`
	// ColdCache 全量页缓存里内核没算进 MemAvailable 的部分。
	//
	// 它是「实际占用」的常客：实测一台 960MB 的小机器上 300MB 实际占用里
	// 有 150MB 是这块，比所有进程加起来还多。不单列出来的话，这些量全被
	// 兜进 Other，而 Other 的说明是 per-cpu / vmalloc——对不上账。
	ColdCache uint64 `json:"cold_cache"`
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
	Cache     uint64 // 可回收：MemAvailable - MemFree
	Free      uint64 // 真正闲置：MemFree
	SwapTotal uint64
	SwapUsed  uint64

	// 以下三项是 Used 的组成部分，从 /proc/meminfo 直接读到。
	Anon   uint64 // AnonPages
	Shmem  uint64 // Shmem（tmpfs / 共享内存）
	Kernel uint64 // SUnreclaim + KernelStack + PageTables
	// Cold 全量页缓存里没被算进 MemAvailable 的差额，同样落在 Used 里。
	Cold uint64
}

// parts 把 memStats 里的构成转成对外结构，余项兜在 Other 里。
func (m memStats) parts() *MemParts {
	anon, shmem, kernel, cold := m.Anon, m.Shmem, m.Kernel, m.Cold

	if sum := anon + shmem + kernel + cold; sum > m.Used && sum > 0 {
		// MemAvailable 的估算会让 Used 偏小；按比例压到 Used 以内，
		// 否则前端会出现「构成比总数还大」这种一眼假的数据。
		// 最后一项取余数，避免整除截断让各项之和差几个字节。
		anon = m.Used * anon / sum
		shmem = m.Used * shmem / sum
		kernel = m.Used - anon - shmem
		cold = 0 // 数据自相矛盾时宁可少报一项，也不凭空编数
	}

	p := &MemParts{Anon: anon, Shmem: shmem, Kernel: kernel, ColdCache: cold}
	if sum := anon + shmem + kernel + cold; sum < m.Used {
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

// parseMemInfo 把 /proc/meminfo 的内容拆成「实际占用 / 可回收 / 空闲」。
//
// 三档都以 MemAvailable 为锚，而不是自己拿 Cached 去拼：
//
//	实际 = 总量 - MemAvailable
//	空闲 = MemFree
//	可回收 = 总量 - 实际 - 空闲  （即 MemAvailable - MemFree）
//
// 早先「缓存」取 Cached + Buffers + SReclaimable - Shmem、「空闲」用减法推，
// 两者会打架：页缓存里有一部分内核不认（块设备元数据页、被映射钉住的页），
// 它既算在「实际」里、又被全量缓存算一遍，同一块内存记了两次，空闲被压掉
// 一大截。现场一台 960MB 的机器上，free(1) 报空闲 327MB，面板只敢报 171MB。
//
// 现在空闲直接取 MemFree（与 free 的 free 列一致），可回收取两者之间余下
// 的部分，必然满足 实际 + 可回收 + 空闲 = 总量。全量缓存与它的差额另存
// Cold，由 parts() 放到「实际」的构成里，不再凭空消失。
func parseMemInfo(raw string) memStats {
	vals := parseMeminfoVals(raw)

	var m memStats
	m.Total = vals["MemTotal"]

	avail, ok := vals["MemAvailable"]
	if !ok {
		// 3.14 之前的内核没有 MemAvailable，退回旧公式
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	if avail > m.Total {
		avail = m.Total
	}
	m.Used = m.Total - avail

	// 全部页缓存（含可回收 slab）。tmpfs 在没有 swap 时收不回来，
	// 所以减掉 Shmem，与 cgroup 口径的 file - shmem 保持一致。
	cacheAll := vals["Cached"] + vals["Buffers"] + vals["SReclaimable"]
	if shm := vals["Shmem"]; shm < cacheAll {
		cacheAll -= shm
	} else {
		cacheAll = 0
	}

	if free, ok := vals["MemFree"]; ok {
		// 空闲不能超过「可用」——超过说明 meminfo 自相矛盾。
		if free > m.Total-m.Used {
			free = m.Total - m.Used
		}
		m.Free = free
		m.Cache = m.Total - m.Used - m.Free
		// 缓存里内核不认的那部分：它就是 Used 的一部分，不可能超过 Used。
		if cacheAll > m.Cache {
			cold := cacheAll - m.Cache
			if cold > m.Used {
				cold = m.Used
			}
			m.Cold = cold
		}
	} else {
		// 裁剪过的 meminfo 没有 MemFree：退回减法，冷缓存无从判断。
		if cacheAll > m.Total-m.Used {
			cacheAll = m.Total - m.Used
		}
		m.Cache = cacheAll
		m.Free = m.Total - m.Used - m.Cache
	}

	// 「实际占用」的构成，余项由 parts() 兜到 Other。
	m.Anon = vals["AnonPages"]
	m.Shmem = vals["Shmem"]
	m.Kernel = vals["SUnreclaim"] + vals["KernelStack"] + vals["PageTables"]

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
