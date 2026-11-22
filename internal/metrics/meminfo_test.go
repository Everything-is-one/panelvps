package metrics

import "testing"

// 单位换算统一按 kB → 字节，测试里也用 kB 写，读起来和真实文件一致。
const kb = 1024

func TestParseMemInfoSplitsCacheAndFree(t *testing.T) {
	raw := `MemTotal:        2048000 kB
MemFree:          200000 kB
MemAvailable:    1200000 kB
Buffers:           50000 kB
Cached:           700000 kB
SReclaimable:      40000 kB
Shmem:             20000 kB
SwapTotal:        524288 kB
SwapFree:         262144 kB
`
	m := parseMemInfo(raw)

	if want := uint64(2048000) * kb; m.Total != want {
		t.Fatalf("Total = %d, want %d", m.Total, want)
	}
	// 2048000 - 1200000
	if want := uint64(848000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d（总量 - MemAvailable）", m.Used, want)
	}
	// 700000 + 50000 + 40000 - 20000
	if want := uint64(770000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d（Cached+Buffers+SReclaimable-Shmem）", m.Cache, want)
	}
	// 2048000 - 848000 - 770000
	if want := uint64(430000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d（总量 - 占用 - 缓存）", m.Free, want)
	}
	if m.Used+m.Cache+m.Free != m.Total {
		t.Fatalf("三档之和 %d != 总量 %d", m.Used+m.Cache+m.Free, m.Total)
	}
	if m.SwapUsed != uint64(262144)*kb {
		t.Fatalf("SwapUsed = %d, want %d", m.SwapUsed, uint64(262144)*kb)
	}
}

// 3.14 之前的内核没有 MemAvailable，要退回 MemFree+Buffers+Cached。
func TestParseMemInfoWithoutAvailable(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemFree:          100000 kB
Buffers:           50000 kB
Cached:           400000 kB
`
	m := parseMemInfo(raw)

	// 可用 = 100000 + 50000 + 400000
	if want := uint64(450000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
	if want := uint64(450000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d", m.Cache, want)
	}
	// 1000000 - 450000 - 450000
	if want := uint64(100000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d", m.Free, want)
	}
}

// MemAvailable 估算偏保守时，缓存有可能算得比「剩余」还多。
// 不夹一下，free = total - used - cache 会下溢成一个天文数字。
// 这里刻意给一组自相矛盾的数值（可用只有 100MB，缓存却报了 850MB），
// 只为覆盖这个防御分支。
func TestParseMemInfoClampsCache(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemAvailable:     100000 kB
Cached:           800000 kB
Buffers:           50000 kB
`
	m := parseMemInfo(raw)

	if want := uint64(900000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
	if m.Cache != m.Total-m.Used {
		t.Fatalf("Cache = %d, want 夹到剩余 %d", m.Cache, m.Total-m.Used)
	}
	if m.Free != 0 {
		t.Fatalf("Free = %d, want 0", m.Free)
	}
}

// tmpfs 挂得多时 Shmem 可能比 Cached 大，不能算出负的缓存。
func TestParseMemInfoShmemLargerThanCached(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemAvailable:     800000 kB
Cached:           100000 kB
Buffers:               0 kB
Shmem:            300000 kB
`
	m := parseMemInfo(raw)
	if m.Cache != 0 {
		t.Fatalf("Cache = %d, want 0", m.Cache)
	}
	if want := uint64(800000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d", m.Free, want)
	}
}

func TestMemFreeOf(t *testing.T) {
	cases := []struct{ total, used, cache, want uint64 }{
		{100, 40, 30, 30},
		{100, 100, 0, 0},
		{100, 90, 20, 0}, // 缓存把剩余吃超了
		{0, 0, 0, 0},
	}
	for _, c := range cases {
		if got := memFreeOf(c.total, c.used, c.cache); got != c.want {
			t.Fatalf("memFreeOf(%d,%d,%d) = %d, want %d", c.total, c.used, c.cache, got, c.want)
		}
	}
}
