package metrics

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCleanUsagePath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "/", false},
		// "/" 被单独放行（Windows 上 filepath.IsAbs("/") 为 false），
		// 其余用 filepath.Clean 做期望值，两边都能跑同一份测试。
		{"/", "/", false},
		{"/var/log", filepath.Clean("/var/log"), false},
		{"/var//log/", filepath.Clean("/var/log"), false},
		{"var/log", "", true},
		{"../etc", "", true},
	}
	for _, c := range cases {
		// Windows 上 "/var/log" 不算绝对路径（需要盘符），这几条只在 Linux 校验
		if runtime.GOOS == "windows" && strings.HasPrefix(c.in, "/") && c.in != "/" {
			continue
		}
		got, err := cleanUsagePath(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("cleanUsagePath(%q) 期望报错，实际得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("cleanUsagePath(%q) 报错: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("cleanUsagePath(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestScanTreeAggregates(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, n int) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("big/a.bin", 4096)
	mustWrite("big/nested/b.bin", 2048)
	mustWrite("small/c.txt", 100)
	mustWrite("rootfile.log", 512)

	j := &UsageJob{path: root, startedAt: time.Now()}
	rep, err := scanTree(context.Background(), j)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}

	if rep.Files != 4 {
		t.Fatalf("文件数 = %d, 期望 4", rep.Files)
	}
	if rep.Total != 4096+2048+100+512 {
		t.Fatalf("总大小 = %d, 期望 %d", rep.Total, 4096+2048+100+512)
	}
	if len(rep.Items) != 3 {
		t.Fatalf("一级子项 = %d 个, 期望 3（big / small / rootfile.log）", len(rep.Items))
	}
	// 按大小降序：big 6144 > rootfile.log 512 > small 100
	if rep.Items[0].Name != "big" || !rep.Items[0].IsDir || rep.Items[0].Size != 6144 {
		t.Fatalf("第一项 = %+v, 期望 big/目录/6144", rep.Items[0])
	}
	if rep.Items[0].Files != 2 {
		t.Fatalf("big 下文件数 = %d, 期望 2", rep.Items[0].Files)
	}
	if rep.Items[2].Name != "small" || rep.Items[2].Size != 100 {
		t.Fatalf("最后一项 = %+v, 期望 small/100", rep.Items[2])
	}
	// 直接躺在根下的文件也要作为一项出现，且 IsDir=false
	found := false
	for _, it := range rep.Items {
		if it.Name == "rootfile.log" {
			found = true
			if it.IsDir {
				t.Fatalf("rootfile.log 被标成了目录")
			}
		}
	}
	if !found {
		t.Fatalf("根目录下的文件没有出现在结果里")
	}
}

func TestDiskScannerLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	sc := NewDiskScanner()
	st, err := sc.Start(root, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for st.State == "running" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		st, err = sc.Start(root, false)
		if err != nil {
			t.Fatalf("轮询: %v", err)
		}
	}
	if st.State != "done" {
		t.Fatalf("扫描没有完成，状态 %q（%s）", st.State, st.Error)
	}
	if st.Report == nil || st.Report.Total != 1024 {
		t.Fatalf("结果不对: %+v", st.Report)
	}

	// 命中缓存：第二次调用立刻是 done，且不会重新计时
	cached, err := sc.Start(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if cached.State != "done" || cached.Report == nil {
		t.Fatalf("缓存未命中: %+v", cached)
	}
	if _, ok := sc.Cached(root); !ok {
		t.Fatalf("Cached 应当返回结果")
	}

	// refresh 强制重扫，此时应回到 running
	refreshed, err := sc.Start(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != "running" && refreshed.State != "done" {
		t.Fatalf("强制刷新状态异常: %q", refreshed.State)
	}
}

func TestDiskScannerRejectsRelative(t *testing.T) {
	sc := NewDiskScanner()
	if _, err := sc.Start("var/log", false); err == nil {
		t.Fatal("相对路径应当被拒绝")
	}
}
