//go:build linux

package metrics

import (
	"bufio"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 进程采样的全局状态。CPU 占用必须做两次采样求差，单次读到的
// utime/stime 是进程生命周期累计值，直接换算只会得到「历史平均值」，
// 在概览里 hover 看排行会全是 0.0% 或者老进程永远排第一。
var (
	topMu    sync.Mutex
	topAt    time.Time
	topItems []TopProc
	topErr   error
)

const (
	topSampleGap = 220 * time.Millisecond
	topCacheTTL  = 2 * time.Second
)

// clockTicks 是 /proc/<pid>/stat 里 utime/stime 的单位：Linux 的 USER_HZ。
// 这是内核 ABI 的一部分，所有架构都固定为 100，与 CPU 频率无关。
// 用常量而不是 sysconf(_SC_CLK_TCK)，避免为此引入 cgo。
const clockTicks = 100

type procSample struct {
	pid   int
	name  string
	rss   uint64
	ticks uint64
}

// TopProcesses 返回按 kind（cpu / mem）排序的前 limit 个进程。
// 整个采样过程串行化并缓存 2 秒：hover 面板时前端可能连续触发，
// 每次都在 /proc 里扫两遍并 sleep 220ms 是浪费。
func (c *Collector) TopProcesses(kind string, limit int) ([]TopProc, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	items, err := sampleProcesses()
	if err != nil {
		return nil, err
	}

	out := make([]TopProc, len(items))
	copy(out, items)
	sort.Slice(out, func(i, j int) bool {
		if kind == "cpu" {
			if out[i].CPUPct != out[j].CPUPct {
				return out[i].CPUPct > out[j].CPUPct
			}
			return out[i].RSS > out[j].RSS
		}
		if out[i].RSS != out[j].RSS {
			return out[i].RSS > out[j].RSS
		}
		return out[i].CPUPct > out[j].CPUPct
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sampleProcesses() ([]TopProc, error) {
	topMu.Lock()
	defer topMu.Unlock()

	if !topAt.IsZero() && time.Since(topAt) < topCacheTTL {
		return topItems, topErr
	}

	first := readAllProcs()
	if len(first) == 0 {
		topErr = ErrProcUnavailable
		topAt = time.Now()
		topItems = nil
		return nil, topErr
	}
	start := time.Now()
	time.Sleep(topSampleGap)
	second := readAllProcs()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = topSampleGap.Seconds()
	}

	memTotal := memInfo().Total
	// 容器里 /proc/meminfo 报的是宿主机内存，占比要用 cgroup 限额做分母
	if lim, _ := cgroupMem(); lim > 0 && lim < memTotal {
		memTotal = lim
	}
	if memTotal == 0 {
		memTotal = 1
	}
	cores := float64(runtime.NumCPU())
	if cores < 1 {
		cores = 1
	}

	out := make([]TopProc, 0, len(second))
	for pid, b := range second {
		a, ok := first[pid]
		if !ok {
			// 采样间隙里新起的进程，分母不完整，跳过
			continue
		}
		// 与概览卡片同口径：除以核数，得到「占整机 CPU 的百分比」。
		// 直接用 top 的单核口径会出现「进程 180% > 整机 45%」的矛盾。
		var pct float64
		if b.ticks >= a.ticks {
			pct = float64(b.ticks-a.ticks) / float64(clockTicks) / elapsed * 100 / cores
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		out = append(out, TopProc{
			PID:    pid,
			Name:   b.name,
			CPUPct: pct,
			RSS:    b.rss,
			MemPct: float64(b.rss) / float64(memTotal) * 100,
		})
	}

	topItems, topAt, topErr = out, time.Now(), nil
	return out, nil
}

// readAllProcs 只解析 /proc/<pid>/stat，不读 cmdline / status：
// 排行只需要名字、RSS 和 CPU 累计时间，多读两个文件会让 200ms
// 的采样窗口在进程多的机器上被拉长。
func readAllProcs() map[int]procSample {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	pageSize := uint64(os.Getpagesize())
	out := make(map[int]procSample, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(raw)
		open := strings.IndexByte(s, '(')
		close := strings.LastIndexByte(s, ')')
		if open < 0 || close <= open {
			continue
		}
		name := s[open+1 : close]
		rest := strings.Fields(s[close+1:])
		if len(rest) < 22 {
			continue
		}
		utime, _ := strconv.ParseUint(rest[11], 10, 64)
		stime, _ := strconv.ParseUint(rest[12], 10, 64)
		rssPages, _ := strconv.ParseUint(rest[21], 10, 64)
		out[pid] = procSample{pid: pid, name: name, rss: rssPages * pageSize, ticks: utime + stime}
	}
	return out
}

// 伪文件系统不算「占磁盘」：tmpfs 用的是内存，proc/sysfs 根本没有实体。
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "ramfs": true, "cgroup": true, "cgroup2": true,
	"securityfs": true, "pstore": true, "bpf": true, "configfs": true,
	"debugfs": true, "tracefs": true, "fusectl": true, "mqueue": true,
	"hugetlbfs": true, "autofs": true, "nsfs": true, "efivarfs": true,
	"binfmt_misc": true, "rpc_pipefs": true, "squashfs": true,
}

// TopMounts 返回按已用空间排序的挂载点。
// 磁盘没有「进程排行」这种说法，占用资源的就是文件系统本身，
// 所以概览的磁盘卡片 hover 出来的是各挂载点的用量与占比。
func TopMounts(limit int) ([]MountUsage, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, ErrProcUnavailable
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []MountUsage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		device, path, fstype := fields[0], unescapeMount(fields[1]), fields[2]
		if pseudoFS[fstype] || strings.HasPrefix(fstype, "fuse.") {
			continue
		}
		if seen[device] {
			continue
		}
		total, used := diskUsage(path)
		if total == 0 {
			continue
		}
		seen[device] = true
		p := 0.0
		if total > 0 {
			p = float64(used) / float64(total) * 100
		}
		out = append(out, MountUsage{
			Path: path, Device: device, Fstype: fstype,
			Total: total, Used: used, Pct: p,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Used > out[j].Used })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// /proc/mounts 里的空格等字符会写成 \040 这样的八进制转义。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
