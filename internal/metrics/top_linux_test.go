//go:build linux

package metrics

import "testing"

func TestUnescapeMount(t *testing.T) {
	cases := map[string]string{
		"/mnt/data":        "/mnt/data",
		`/mnt/my\040disk`:  "/mnt/my disk",
		`/mnt/a\134b`:      `/mnt/a\b`,
		`/mnt/tab\011name`: "/mnt/tab\tname",
		`/mnt/bad\09`:      `/mnt/bad\09`, // 不足三位，原样保留
	}
	for in, want := range cases {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

// 本机跑测试时至少根分区是可读的，顺带验证一下排序方向与过滤逻辑。
func TestTopMounts(t *testing.T) {
	items, err := TopMounts(3)
	if err != nil {
		t.Skipf("当前环境没有 /proc/mounts: %v", err)
	}
	if len(items) == 0 {
		t.Skip("没有可统计的挂载点")
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].Used < items[i].Used {
			t.Fatalf("结果没有按已用量倒序：%d < %d", items[i-1].Used, items[i].Used)
		}
	}
	for _, it := range items {
		if it.Total == 0 || it.Used > it.Total {
			t.Errorf("挂载点 %s 的用量不合法: used=%d total=%d", it.Path, it.Used, it.Total)
		}
	}
}

func TestTopProcesses(t *testing.T) {
	c := New()
	items, err := c.TopProcesses("mem", 3)
	if err != nil {
		t.Skipf("当前环境无法采样进程: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("应至少返回一个进程")
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].RSS < items[i].RSS {
			t.Fatalf("内存排行没有倒序")
		}
	}
	// 第二次调用命中 2 秒缓存，必须立即返回（若真的重新采样会 sleep 220ms）
	if got, err := c.TopProcesses("cpu", 3); err != nil || len(got) == 0 {
		t.Fatalf("缓存路径异常: %v", err)
	}
}
