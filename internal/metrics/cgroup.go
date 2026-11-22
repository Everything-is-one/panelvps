package metrics

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupRoot 单独提取成变量，便于测试时指向临时目录。
var cgroupRoot = "/sys/fs/cgroup"

// cgroupMem 返回当前环境的 cgroup 内存限额与用量（字节）。
//
// 没设限额时返回 0 —— cgroup v2 用 "max"，v1 用一个接近 int64 上限的数字表示。
// 只有容器里才会拿到一个明显小于物理内存的值，调用方据此判断是否该覆盖
// /proc/meminfo（LXC 默认没开 lxcfs 时，meminfo 报的是宿主机的内存）。
func cgroupMem() (limit, usage uint64) {
	limit, usage, _ = cgroupMemDetail()
	return limit, usage
}

// cgroupMemDetail 同 cgroupMem，另外把「可回收的文件页缓存」单独带出。
//
// memory.current 是 anon + page cache + slab 的总和，其中文件页在内核
// 有回收压力时随时可以丢掉。面板自己更新一次要往数据目录写十几 MB 的
// 新二进制，日志与 sqlite 的 WAL 也在持续追加，于是「升级完面板，
// 概览上的内存从几十 MB 变成一百多 MB」——数字没算错，但它不是进程
// 占住的内存。
//
// 这里不能用 docker stats 的口径（current - inactive_file）：内核只在
// 内存吃紧时才把页从 active 链摘到 inactive 链，机器空闲时 file 全部
// 挂在 active 上，inactive_file 就是 0——现场那台就是这样，
// anon 41.6MB / file 81.4MB / inactive_file 0，照 docker stats 算出来
// 还是 129.7MB，一点缓存都没扣掉。
//
// 正确的做法是按「能不能收回来」分：file 里除了 tmpfs（shmem）之外
// 都是可以丢的，不管它在 active 还是 inactive 链上。所以
// 已用 = current - (file - shmem)，缓存 = file - shmem。
func cgroupMemDetail() (limit, used, cache uint64) {
	// cgroup v2
	if v, ok := readUint(filepath.Join(cgroupRoot, "memory.max")); ok && v > 0 {
		cur, _ := readUint(filepath.Join(cgroupRoot, "memory.current"))
		stat := filepath.Join(cgroupRoot, "memory.stat")
		cache = reclaimableCache(stat, "file", "shmem")
		if cache > cur {
			cache = cur
		}
		return v, cur - cache, cache
	}
	// cgroup v1
	if v, ok := readUint(filepath.Join(cgroupRoot, "memory", "memory.limit_in_bytes")); ok && v > 0 {
		if v > 1<<60 {
			return 0, 0, 0 // 无限制
		}
		cur, _ := readUint(filepath.Join(cgroupRoot, "memory", "memory.usage_in_bytes"))
		stat := filepath.Join(cgroupRoot, "memory", "memory.stat")
		// v1 的键名带 total_ 前缀（含子 cgroup 汇总），老内核可能没有
		cache = reclaimableCache(stat, "total_cache", "total_shmem")
		if cache == 0 {
			cache = reclaimableCache(stat, "cache", "shmem")
		}
		if cache > cur {
			cache = cur
		}
		return v, cur - cache, cache
	}
	return 0, 0, 0
}

// cgroupMemParts 在容器口径下把「实际占用」拆成匿名 / tmpfs / 内核。
//
// 容器里不能读 /proc/meminfo 的 AnonPages / Slab：那是宿主机的数字。
// memory.stat 才有本 cgroup 的 anon、shmem、slab_unreclaimable。
// 拿不到就返回 nil，前端会退回只显示总数。
func cgroupMemParts(used uint64) *MemParts {
	path := filepath.Join(cgroupRoot, "memory.stat")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(cgroupRoot, "memory", "memory.stat")
	}
	anon := readStatKey(path, "anon")
	if anon == 0 {
		anon = readStatKey(path, "total_rss") // cgroup v1
	}
	shmem := readStatKey(path, "shmem")
	if shmem == 0 {
		shmem = readStatKey(path, "total_shmem")
	}
	slab := readStatKey(path, "slab_unreclaimable")
	if slab == 0 {
		slab = readStatKey(path, "total_slab_unreclaimable")
	}
	if anon == 0 && shmem == 0 && slab == 0 {
		return nil
	}
	if sum := anon + shmem + slab; sum > used && sum > 0 {
		anon = used * anon / sum
		shmem = used * shmem / sum
		slab = used - anon - shmem // 取余数，四项之和才能正好等于 used
	}
	p := &MemParts{Anon: anon, Shmem: shmem, Kernel: slab}
	if sum := p.Anon + p.Shmem + p.Kernel; sum < used {
		p.Other = used - sum
	}
	return p
}

// reclaimableCache 返回能还给内核的那部分页缓存。
//
// tmpfs / 共享内存（shmem）在没开 swap 时收不回来，必须从 file 里减掉，
// 否则容器里挂一块 tmpfs 就会被报成「可回收缓存」，真到 OOM 才发现不对。
func reclaimableCache(statPath, fileKey, shmemKey string) uint64 {
	file := readStatKey(statPath, fileKey)
	if file == 0 {
		return 0
	}
	shmem := readStatKey(statPath, shmemKey)
	if shmem > file {
		shmem = file
	}
	return file - shmem
}

// readStatKey 从 memory.stat 里取一个键的字节数。文件是每行
// "<key> <bytes>"，键不存在时返回 0（老内核没有个别统计项）。
func readStatKey(path, key string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != key {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

// cgroupCPU 返回容器累计消耗的 CPU 时间（微秒）与可用核数。
//
// cores 为 0 表示没设 quota（不限制），调用方自己挑分母。
func cgroupCPU() (usageUsec uint64, cores float64, ok bool) {
	// cgroup v2：cpu.stat 里是 usage_usec
	if b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.stat")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "usage_usec" {
				if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					return v, cgroupCoresV2(), true
				}
			}
		}
	}

	// cgroup v1：cpuacct.usage 单位是纳秒，且挂载点在不同发行版下名字不一
	for _, base := range []string{"cpu,cpuacct", "cpuacct", "cpu"} {
		dir := filepath.Join(cgroupRoot, base)
		if v, ok := readUint(filepath.Join(dir, "cpuacct.usage")); ok && v > 0 {
			return v / 1000, cgroupCoresV1(dir), true
		}
	}
	return 0, 0, false
}

func cgroupCoresV2() float64 {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.max"))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
		return 0
	}
	return quota / period
}

func cgroupCoresV1(dir string) float64 {
	quota, ok1 := readUint(filepath.Join(dir, "cpu.cfs_quota_us"))
	period, ok2 := readUint(filepath.Join(dir, "cpu.cfs_period_us"))
	if !ok1 || !ok2 || period == 0 || quota == 0 {
		return 0
	}
	return float64(quota) / float64(period)
}

func readUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
