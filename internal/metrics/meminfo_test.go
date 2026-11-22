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
	// 可回收 = Buffers + Cached + SReclaimable - Shmem
	//        = 50000 + 700000 + 40000 - 20000 = 770000
	if want := uint64(770000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d（buff/cache 口径）", m.Cache, want)
	}
	// 空闲直接取 MemFree，与 free(1) 的 free 列对齐
	if want := uint64(200000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d（MemFree）", m.Free, want)
	}
	// 实际 = 总量 - 可回收 - 空闲
	if want := uint64(1078000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d（总量 - 缓存 - 空闲）", m.Used, want)
	}
	if m.Used+m.Cache+m.Free != m.Total {
		t.Fatalf("三档之和 %d != 总量 %d", m.Used+m.Cache+m.Free, m.Total)
	}
	// MemAvailable 只做展示，不参与三档
	if want := uint64(1200000) * kb; m.Avail != want {
		t.Fatalf("Avail = %d, want %d", m.Avail, want)
	}
	if m.SwapUsed != uint64(262144)*kb {
		t.Fatalf("SwapUsed = %d, want %d", m.SwapUsed, uint64(262144)*kb)
	}
}

// 3.14 之前的内核没有 MemAvailable，三档不能因此缺一档。
func TestParseMemInfoWithoutAvailable(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemFree:          100000 kB
Buffers:           50000 kB
Cached:           400000 kB
`
	m := parseMemInfo(raw)

	if want := uint64(450000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d", m.Cache, want)
	}
	if want := uint64(100000) * kb; m.Free != want {
		t.Fatalf("Free = %d, want %d", m.Free, want)
	}
	if want := uint64(450000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
	if m.Used+m.Cache+m.Free != m.Total {
		t.Fatalf("三档之和 %d != 总量 %d", m.Used+m.Cache+m.Free, m.Total)
	}
	if m.Avail != 0 {
		t.Fatalf("Avail = %d, want 0（老内核没有这一行）", m.Avail)
	}
}

// 没有 MemFree 的精简 meminfo（部分容器镜像会删掉这一行）：
// 退回以 MemAvailable 为锚，缓存夹到可用量以内，不能算出负数。
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
	if want := uint64(200000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
}

// 缓存不能把「空闲」也吃掉：MemFree 之下没有别的页可算。
func TestParseMemInfoCacheClampedToRemainder(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemFree:          900000 kB
Cached:           800000 kB
Buffers:           50000 kB
SReclaimable:      60000 kB
Shmem:                 0 kB
`
	m := parseMemInfo(raw)
	if m.Cache != m.Total-m.Free {
		t.Fatalf("Cache = %d, want %d", m.Cache, m.Total-m.Free)
	}
	if m.Used != 0 {
		t.Fatalf("Used = %d, want 0", m.Used)
	}
}

// 构造成分能对上账：匿名 + tmpfs + 内核 + 余项 = 实际占用。
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
Percpu:             3000 kB
VmallocUsed:        4000 kB
`
	m := parseMemInfo(raw)
	p := m.parts()

	// 20000 + 640000 + 30000 - 20000
	if want := uint64(670000) * kb; m.Cache != want {
		t.Fatalf("Cache = %d, want %d", m.Cache, want)
	}
	// 1000000 - 100000 - 670000
	if want := uint64(230000) * kb; m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
	if want := uint64(150000) * kb; p.Anon != want {
		t.Fatalf("Anon = %d, want %d", p.Anon, want)
	}
	if want := uint64(20000) * kb; p.Shmem != want {
		t.Fatalf("Shmem = %d, want %d", p.Shmem, want)
	}
	// 20000 + 2000 + 8000 + 3000 + 4000（slab + 内核栈 + 页表 + percpu + vmalloc）
	if want := uint64(37000) * kb; p.Kernel != want {
		t.Fatalf("Kernel = %d, want %d", p.Kernel, want)
	}
	// 230000 - 150000 - 20000 - 37000
	if want := uint64(23000) * kb; p.Other != want {
		t.Fatalf("Other = %d, want %d", p.Other, want)
	}
	if sum := p.Anon + p.Shmem + p.Kernel + p.Other; sum != m.Used {
		t.Fatalf("构成之和 %d != 实际占用 %d", sum, m.Used)
	}
}

// 现场数据（960MB 的 KVM 小机器）：htop 顶部报 Mem 138M/960M。
//
// 上一版用「总量 - MemAvailable」，同一台机器报 291MB，用户拿 htop 一比就发现
// 差了 150MB——那 150MB 全是页缓存。这个用例把两件事钉住：实际占用要与
// htop / free 的 used 同口径；MemAvailable 单独留着，不再参与三档。
func TestParseMemInfoLiveSmallVM(t *testing.T) {
	raw := `MemTotal:         983500 kB
MemFree:          334848 kB
MemAvailable:     684556 kB
Buffers:            1200 kB
Cached:           446500 kB
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

	// 可回收 = 1200 + 446500 + 57506 - 2020 = 503186 kB ≈ 491 MiB
	if got, want := m.Cache/kb/1024, uint64(491); got != want {
		t.Fatalf("Cache = %d MB, want %d MB（buff/cache 口径）", got, want)
	}
	// 空闲 = MemFree = 327 MiB，与 free 的 free 列一致
	if got, want := m.Free/kb/1024, uint64(327); got != want {
		t.Fatalf("Free = %d MB, want %d MB", got, want)
	}
	// 983500 - 334848 - 503186 = 145466 kB ≈ 142 MiB，与 htop 的 138M 同量级
	if got := m.Used / kb / 1024; got < 130 || got > 160 {
		t.Fatalf("Used = %d MB, want 130~160（htop 报 138M）", got)
	}
	if m.Used+m.Cache+m.Free != m.Total {
		t.Fatalf("三档之和 %d != 总量 %d", m.Used+m.Cache+m.Free, m.Total)
	}
	// 最容易复发的一条：按 MemAvailable 算出来的 291MB 不该再出现
	if alt := m.Total - m.Avail; m.Used == alt {
		t.Fatalf("Used 又退回了「总量 - MemAvailable」= %d MB", alt/kb/1024)
	}

	p := m.parts()
	if got, want := p.Anon/kb/1024, uint64(55); got != want {
		t.Fatalf("Anon = %d MB, want %d MB（AnonPages）", got, want)
	}
	if sum := p.Anon + p.Shmem + p.Kernel + p.Other; sum != m.Used {
		t.Fatalf("构成之和 %d != 实际占用 %d", sum, m.Used)
	}
	if m.SwapUsed != 0 {
		t.Fatalf("SwapUsed = %d, want 0", m.SwapUsed)
	}
}

// meminfo 多行分别采样，抖动时成分之和可能超过 Used，必须按比例压回去，
// 否则前端会出现「各分项加起来比总数还大」的假数据。
func TestParseMemInfoPartsClamped(t *testing.T) {
	raw := `MemTotal:        1000000 kB
MemFree:          900000 kB
MemAvailable:     950000 kB
Cached:                0 kB
AnonPages:        200000 kB
Shmem:            100000 kB
SUnreclaim:        50000 kB
`
	m := parseMemInfo(raw)
	p := m.parts()

	if sum := p.Anon + p.Shmem + p.Kernel + p.Other; sum > m.Used {
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
