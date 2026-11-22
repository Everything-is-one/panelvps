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
	// 空闲直接取 MemFree，与 free(1) 的 free 列对齐
	if want := uint64(200000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d（MemFree）", m.Free, want)
	}
	// 可回收 = MemAvailable - MemFree = 1200000 - 200000
	if want := uint64(1000000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d（可用 - 空闲）", m.Cache, want)
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
	if want := uint64(100000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d", m.Free, want)
	}
	if m.Used+m.Cache+m.Free != m.Total {
		t.Fatalf("三档之和 %d != 总量 %d", m.Used+m.Cache+m.Free, m.Total)
	}
}

// 自相矛盾的 meminfo（可用只有 100MB，缓存却报了 850MB）不能算出负数：
// uint64 下溢会直接变成天文数字。
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

// 「实际 313 MB、前 5 名进程才 133 MB」是用户最常问的问题。
// 验证构成能对上账：匿名 + tmpfs + 内核 + 冷缓存 + 余项 = 实际占用。
func TestParseMemInfoParts(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemFree:          100000 kB
MemAvailable:     700000 kB
Buffers:           20000 kB
Cached:           640000 kB
SReclaimable:      30000 kB
Shmem:             20000 kB
AnonPages:        150000 kB
SUnreclaim:        20000 kB
KernelStack:        2000 kB
PageTables:         8000 kB
`
	m := parseMemInfo(raw)
	p := m.parts()

	// 1000000 - 700000
	if want := uint64(300000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
	// 可用 700000 - 空闲 100000
	if want := uint64(600000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d", m.Cache, want)
	}
	// 全量缓存 640000 + 20000 + 30000 - 20000 = 670000，其中 600000 被算作
	// 可用，差的 70000 只能落在「实际」里。
	if want := uint64(70000) * kb; p.ColdCache != want {
		t.Fatalf("ColdCache = %d, want %d", p.ColdCache, want)
	}
	if want := uint64(150000) * kb; p.Anon != want {
		t.Fatalf("Anon = %d, want %d", p.Anon, want)
	}
	if want := uint64(20000) * kb; p.Shmem != want {
		t.Fatalf("Shmem = %d, want %d", p.Shmem, want)
	}
	// 20000 + 2000 + 8000
	if want := uint64(30000) * kb; p.Kernel != want {
		t.Fatalf("Kernel = %d, want %d", p.Kernel, want)
	}
	// 300000 - 150000 - 20000 - 30000 - 70000
	if want := uint64(30000) * kb; p.Other != want {
		t.Fatalf("Other = %d, want %d", p.Other, want)
	}
	if sum := p.Anon + p.Shmem + p.Kernel + p.ColdCache + p.Other; sum != m.Used {
		t.Fatalf("构成之和 %d != 实际占用 %d", sum, m.Used)
	}
}

// 现场数据（960MB 的 KVM 小机器）：用户拿 free -m 和面板对不上账。
// 钉住两点：实际/空闲与 free 的 used/free 列一致；冷缓存不能吞掉差额。
func TestParseMemInfoLiveSmallVM(t *testing.T) {
	raw := `MemTotal:         983500 kB
MemFree:          334848 kB
MemAvailable:     684556 kB
Buffers:            1200 kB
Cached:           440000 kB
SReclaimable:      57506 kB
Shmem:              2020 kB
AnonPages:         56568 kB
SUnreclaim:        22572 kB
KernelStack:        1524 kB
PageTables:         1224 kB
SwapTotal:       1047552 kB
SwapFree:        1047552 kB
`
	m := parseMemInfo(raw)

	// free -m 报 used 291 / free 327 / available 668
	if got, want := m.Used/kb/1024, uint64(291); got != want {
		t.Fatalf("Used = %d MB, want %d MB", got, want)
	}
	if got, want := m.Free/kb/1024, uint64(327); got != want {
		t.Fatalf("Free = %d MB, want %d MB（free 的 free 列）", got, want)
	}
	if got, want := m.Cache/kb/1024, uint64(341); got != want {
		t.Fatalf("Cache = %d MB, want %d MB（可用 - 空闲）", got, want)
	}
	if m.Used+m.Cache+m.Free != m.Total {
		t.Fatalf("三档之和 %d != 总量 %d", m.Used+m.Cache+m.Free, m.Total)
	}

	p := m.parts()
	// 全量缓存约 496MB，只有 341MB 被算作可用，剩下 150MB 是冷缓存
	if got := p.ColdCache / kb / 1024; got < 100 {
		t.Fatalf("ColdCache = %d MB，太小，说明差额又被兜进了 Other", got)
	}
	if got, want := p.Anon/kb/1024, uint64(55); got != want {
		t.Fatalf("Anon = %d MB, want %d MB（AnonPages）", got, want)
	}
	if sum := p.Anon + p.Shmem + p.Kernel + p.ColdCache + p.Other; sum != m.Used {
		t.Fatalf("构成之和 %d != 实际占用 %d", sum, m.Used)
	}
	// 剩下的「其它」不该再是个大块头
	if got := p.Other / kb / 1024; got > 100 {
		t.Fatalf("Other = %d MB，余项过大，说明还有没被识别的成分", got)
	}
	if m.SwapUsed != 0 {
		t.Fatalf("SwapUsed = %d, want 0", m.SwapUsed)
	}
}

// MemAvailable 偏保守时构成之和可能超过 Used，必须按比例压回去，
// 否则前端会出现「各分项加起来比总数还大」的假数据。
func TestParseMemInfoPartsClamped(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemAvailable:     900000 kB
Cached:                0 kB
AnonPages:        200000 kB
Shmem:            100000 kB
SUnreclaim:        50000 kB
`
	m := parseMemInfo(raw)
	p := m.parts()

	if sum := p.Anon + p.Shmem + p.Kernel + p.ColdCache + p.Other; sum > m.Used {
		t.Fatalf("构成之和 %d 超过实际占用 %d", sum, m.Used)
	}
	if p.Other != 0 {
		t.Fatalf("压回之后不该还有余项，得到 %d", p.Other)
	}
	if !(p.Anon > p.Shmem && p.Shmem > p.Kernel) {
		t.Fatalf("比例没保住：anon=%d shmem=%d kernel=%d", p.Anon, p.Shmem, p.Kernel)
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
