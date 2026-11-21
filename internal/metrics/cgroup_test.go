package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

// withCgroup 把 cgroupRoot 指向临时目录，测试结束后还原。
func withCgroup(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", name, err)
		}
	}
	old := cgroupRoot
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = old })
}

func TestCgroupMemV2(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.max":     "536870912\n",
		"memory.current": "104857600\n",
	})
	limit, usage := cgroupMem()
	if limit != 536870912 || usage != 104857600 {
		t.Fatalf("v2 解析错误: limit=%d usage=%d", limit, usage)
	}
}

func TestCgroupMemV2Unlimited(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory.max":     "max\n",
		"memory.current": "1048576\n",
	})
	if limit, usage := cgroupMem(); limit != 0 || usage != 0 {
		t.Fatalf("无限制时应返回 0, 实际 limit=%d usage=%d", limit, usage)
	}
}

func TestCgroupMemV1(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.limit_in_bytes": "268435456\n",
		"memory/memory.usage_in_bytes": "10485760\n",
	})
	limit, usage := cgroupMem()
	if limit != 268435456 || usage != 10485760 {
		t.Fatalf("v1 解析错误: limit=%d usage=%d", limit, usage)
	}
}

func TestCgroupMemV1Unlimited(t *testing.T) {
	withCgroup(t, map[string]string{
		"memory/memory.limit_in_bytes": "9223372036854771712\n",
	})
	if limit, _ := cgroupMem(); limit != 0 {
		t.Fatalf("v1 无限制应返回 0, 实际 %d", limit)
	}
}

func TestCgroupCPUV2WithQuota(t *testing.T) {
	withCgroup(t, map[string]string{
		"cpu.stat": "usage_usec 1234567\nnr_periods 10\n",
		"cpu.max":  "50000 100000\n",
	})
	usage, cores, ok := cgroupCPU()
	if !ok || usage != 1234567 {
		t.Fatalf("v2 usage 解析错误: ok=%v usage=%d", ok, usage)
	}
	if cores != 0.5 {
		t.Fatalf("v2 配额应为 0.5 核, 实际 %v", cores)
	}
}

func TestCgroupCPUV2NoQuota(t *testing.T) {
	withCgroup(t, map[string]string{
		"cpu.stat": "usage_usec 999\n",
		"cpu.max":  "max 100000\n",
	})
	usage, cores, ok := cgroupCPU()
	if !ok || usage != 999 {
		t.Fatalf("v2 usage 解析错误: ok=%v usage=%d", ok, usage)
	}
	if cores != 0 {
		t.Fatalf("无配额时 cores 应为 0, 实际 %v", cores)
	}
}

func TestCgroupCPUV1(t *testing.T) {
	// cpuacct.usage 单位纳秒，1e9 ns = 1e6 usec
	withCgroup(t, map[string]string{
		"cpu,cpuacct/cpuacct.usage":     "1000000000\n",
		"cpu,cpuacct/cpu.cfs_quota_us":  "100000\n",
		"cpu,cpuacct/cpu.cfs_period_us": "100000\n",
	})
	usage, cores, ok := cgroupCPU()
	if !ok || usage != 1000000 {
		t.Fatalf("v1 usage 应为 1000000 usec, 实际 ok=%v usage=%d", ok, usage)
	}
	if cores != 1 {
		t.Fatalf("v1 配额应为 1 核, 实际 %v", cores)
	}
}

func TestCgroupCPUV1UnlimitedQuota(t *testing.T) {
	// -1 表示不限，readUint 解析失败，cores 应为 0
	withCgroup(t, map[string]string{
		"cpu,cpuacct/cpuacct.usage":     "2000000000\n",
		"cpu,cpuacct/cpu.cfs_quota_us":  "-1\n",
		"cpu,cpuacct/cpu.cfs_period_us": "100000\n",
	})
	usage, cores, ok := cgroupCPU()
	if !ok || usage != 2000000 {
		t.Fatalf("v1 usage 应为 2000000 usec, 实际 ok=%v usage=%d", ok, usage)
	}
	if cores != 0 {
		t.Fatalf("-1 配额时 cores 应为 0, 实际 %v", cores)
	}
}

func TestCgroupCPUAbsent(t *testing.T) {
	withCgroup(t, map[string]string{"memory.max": "1024\n"})
	if _, _, ok := cgroupCPU(); ok {
		t.Fatal("没有 cpu 文件时应返回 ok=false")
	}
}
