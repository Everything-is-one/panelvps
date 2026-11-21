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

// allocSize 按扫描器自己的口径算占用：Linux 是块数 × 512（与 du 一致），
// 其他平台退回文件长度。断言不能写死字节数——100 字节的小文件在 Linux 上
// 占 4096，写死就会在 CI 上挂（第一版就是这么炸的）。
func allocSize(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fileBlocks(fi)
}

func TestScanTreeAggregates(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string, n int) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	aBin := mustWrite("big/a.bin", 4096)
	bBin := mustWrite("big/nested/b.bin", 2048)
	cTxt := mustWrite("small/c.txt", 100)
	rootLog := mustWrite("rootfile.log", 512)

	wantTotal := allocSize(t, aBin) + allocSize(t, bBin) + allocSize(t, cTxt) + allocSize(t, rootLog)
	wantBig := allocSize(t, aBin) + allocSize(t, bBin)

	j := &UsageJob{path: root, startedAt: time.Now()}
	rep, err := scanTree(context.Background(), j)
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}

	if rep.Files != 4 {
		t.Fatalf("文件数 = %d, 期望 4", rep.Files)
	}
	if rep.Total != wantTotal {
		t.Fatalf("总大小 = %d, 期望 %d", rep.Total, wantTotal)
	}
	if len(rep.Items) != 3 {
		t.Fatalf("一级子项 = %d 个, 期望 3（big / small / rootfile.log）", len(rep.Items))
	}
	if rep.Items[0].Name != "big" || !rep.Items[0].IsDir || rep.Items[0].Size != wantBig {
		t.Fatalf("第一项 = %+v, 期望 big/目录/%d", rep.Items[0], wantBig)
	}
	if rep.Items[0].Files != 2 {
		t.Fatalf("big 下文件数 = %d, 期望 2", rep.Items[0].Files)
	}
	// 按大小降序：只断言单调不增，不断言具体名次——块对齐后
	// 100 字节和 512 字节的文件可能一样大，名次取决于同名兜底规则。
	for i := 1; i < len(rep.Items); i++ {
		if rep.Items[i-1].Size < rep.Items[i].Size {
			t.Fatalf("未按大小降序: %+v", rep.Items)
		}
	}
	// 直接躺在根下的文件也要作为一项出现，且 IsDir=false
	byName := map[string]DirUsage{}
	for _, it := range rep.Items {
		byName[it.Name] = it
	}
	if it, ok := byName["small"]; !ok || it.Size != allocSize(t, cTxt) {
		t.Fatalf("small 项 = %+v, 期望 %d", it, allocSize(t, cTxt))
	}
	if it, ok := byName["rootfile.log"]; !ok {
		t.Fatalf("根目录下的文件没有出现在结果里: %+v", rep.Items)
	} else if it.IsDir {
		t.Fatalf("rootfile.log 被标成了目录")
	}
}

func TestDiskScannerLifecycle(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "a.bin")
	if err := os.WriteFile(bin, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	want := allocSize(t, bin)

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
	if st.Report == nil || st.Report.Total != want {
		t.Fatalf("结果不对: %+v, 期望总计 %d", st.Report, want)
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
