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
	// cgroup v2
	if v, ok := readUint(filepath.Join(cgroupRoot, "memory.max")); ok && v > 0 {
		usage, _ = readUint(filepath.Join(cgroupRoot, "memory.current"))
		return v, usage
	}
	// cgroup v1
	if v, ok := readUint(filepath.Join(cgroupRoot, "memory", "memory.limit_in_bytes")); ok && v > 0 {
		if v > 1<<60 {
			return 0, 0 // 无限制
		}
		usage, _ = readUint(filepath.Join(cgroupRoot, "memory", "memory.usage_in_bytes"))
		return v, usage
	}
	return 0, 0
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
