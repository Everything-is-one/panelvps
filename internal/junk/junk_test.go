package junk

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeFile 造一个指定大小与 mtime 的文件（内容为零字节，大小就是 Size）。
func writeFile(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// newFixture 造一棵覆盖所有文件类别的临时目录树，并把类别根目录指过去。
// journal / docker 不覆盖：它们在测试机上通常不存在，正好验证「不可用」分支。
func newFixture(t *testing.T) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)

	apt := filepath.Join(dir, "apt")
	writeFile(t, filepath.Join(apt, "curl_8.deb"), 120, old)
	writeFile(t, filepath.Join(apt, "partial", "half.deb"), 30, old)
	writeFile(t, filepath.Join(apt, "README.txt"), 999, old)

	tmp1 := filepath.Join(dir, "tmp")
	writeFile(t, filepath.Join(tmp1, "stale.bin"), 64, old)
	writeFile(t, filepath.Join(tmp1, "fresh.bin"), 32, now)
	writeFile(t, filepath.Join(tmp1, "systemd-private-abc"), 256, old)
	writeFile(t, filepath.Join(tmp1, "nested", "deep.bin"), 128, old)

	tmp2 := filepath.Join(dir, "tmp2")
	writeFile(t, filepath.Join(tmp2, "also-old"), 16, old)

	logs := filepath.Join(dir, "log")
	writeFile(t, filepath.Join(logs, "syslog.1"), 200, old)
	writeFile(t, filepath.Join(logs, "dpkg.log.2.gz"), 40, old)
	writeFile(t, filepath.Join(logs, "access.log"), 500, now)
	writeFile(t, filepath.Join(logs, "nginx", "access.log-20260101.gz"), 20, old)
	writeFile(t, filepath.Join(logs, "nginx", "error.log"), 100, now)

	crash := filepath.Join(dir, "crash")
	writeFile(t, filepath.Join(crash, "app.crash"), 48, old)
	writeFile(t, filepath.Join(crash, "notes.txt"), 48, old)

	cache := filepath.Join(dir, "cache")
	writeFile(t, filepath.Join(cache, "go-build", "aa", "obj"), 300, old)

	trash := filepath.Join(dir, "trash")
	writeFile(t, filepath.Join(trash, "deleted.txt"), 11, old)
	writeFile(t, filepath.Join(trash, "dir", "nested.txt"), 22, old)

	opt := Options{
		DataDir: dir,
		Now:     now,
		roots: map[string][]string{
			"apt":      {apt},
			"tmp":      {tmp1, tmp2},
			"rotated":  {logs, filepath.Join(logs, "nginx")},
			"crash":    {crash},
			"devcache": {cache},
			"trash":    {trash},
			"journal":  {},
			"docker":   {},
		},
	}
	return opt, dir
}

func waitDone(t *testing.T, sc *Scanner) *Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := sc.Start(false)
		if st.State == "done" {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("扫描未在 5 秒内完成")
	return nil
}

func findItem(t *testing.T, rep *Report, key string) Item {
	t.Helper()
	for _, it := range rep.Items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("报告里没有类别 %q", key)
	return Item{}
}

func TestScanCollectsOnlyWhitelistedFiles(t *testing.T) {
	opt, _ := newFixture(t)
	st := waitDone(t, NewScanner(opt))
	rep := st.Report
	if rep == nil {
		t.Fatal("done 状态必须带报告")
	}

	cases := []struct {
		key   string
		bytes int64
		files int
	}{
		{"apt", 150, 2},      // 两个 .deb，README.txt 不算
		{"tmp", 80, 2},       // 旧的 64 + 16；新文件 / systemd-private / 子目录都不算
		{"rotated", 260, 3},  // syslog.1 + .2.gz + 带日期的 nginx 归档；正在写的两份不算
		{"crash", 48, 1},     // 只有 .crash
		{"devcache", 300, 1}, // 缓存目录里全部普通文件
		{"trash", 33, 2},     // 回收站递归统计
	}
	for _, c := range cases {
		it := findItem(t, rep, c.key)
		if !it.Present {
			t.Fatalf("%s 应判为存在: %+v", c.key, it)
		}
		if it.Bytes != c.bytes || it.Files != c.files {
			t.Fatalf("%s 期望 %d 字节 / %d 个文件，得到 %d / %d",
				c.key, c.bytes, c.files, it.Bytes, it.Files)
		}
		if it.Detail == "" || it.Hint == "" {
			t.Fatalf("%s 缺少说明文案", c.key)
		}
	}

	// journal / docker 在测试机上没有覆盖根目录，可能不可用，但条目必须存在，
	// 前端要能显示「为什么这一项不可用」。
	for _, k := range []string{"journal", "docker"} {
		_ = findItem(t, rep, k)
	}
	if rep.Total < 871 {
		t.Fatalf("合计至少应包含文件类的 871 字节，得到 %d", rep.Total)
	}
	// 排序：大的在前，0 字节的沉底
	for i := 1; i < len(rep.Items); i++ {
		if rep.Items[i-1].Bytes < rep.Items[i].Bytes {
			t.Fatalf("条目未按占用降序：%s < %s", rep.Items[i-1].Key, rep.Items[i].Key)
		}
	}
}

func TestScanMissingRootIsNotPresent(t *testing.T) {
	opt, dir := newFixture(t)
	opt.roots["apt"] = []string{filepath.Join(dir, "nope")}
	st := waitDone(t, NewScanner(opt))
	it := findItem(t, st.Report, "apt")
	if it.Present || it.Bytes != 0 {
		t.Fatalf("不存在的位置不应判为可用: %+v", it)
	}
}

func TestCleanRemovesMatchedFilesOnly(t *testing.T) {
	opt, dir := newFixture(t)
	sc := NewScanner(opt)
	waitDone(t, sc)

	rep, err := sc.Clean([]string{"apt", "tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Freed != 230 || rep.Files != 4 {
		t.Fatalf("清理结果应为 230 字节 / 4 个文件，得到 %d / %d", rep.Freed, rep.Files)
	}

	gone := []string{
		filepath.Join(dir, "apt", "curl_8.deb"),
		filepath.Join(dir, "apt", "partial", "half.deb"),
		filepath.Join(dir, "tmp", "stale.bin"),
		filepath.Join(dir, "tmp2", "also-old"),
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应已删除", p)
		}
	}
	kept := []string{
		filepath.Join(dir, "apt", "README.txt"),          // 非 .deb
		filepath.Join(dir, "tmp", "fresh.bin"),           // 太新
		filepath.Join(dir, "tmp", "systemd-private-abc"), // 私有临时目录
		filepath.Join(dir, "tmp", "nested", "deep.bin"),  // 只清顶层
		filepath.Join(dir, "log", "access.log"),          // 没选中该类别
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s 不应被删除：%v", p, err)
		}
	}

	// 清理必须让缓存失效：再扫一次，apt / tmp 都应归零
	st := waitDone(t, sc)
	if it := findItem(t, st.Report, "apt"); it.Bytes != 0 || it.Files != 0 {
		t.Fatalf("清理后缓存未失效: %+v", it)
	}
	if it := findItem(t, st.Report, "tmp"); it.Files != 0 {
		t.Fatalf("清理后缓存未失效: %+v", it)
	}
}

func TestCleanTrashRemovesEntries(t *testing.T) {
	opt, dir := newFixture(t)
	sc := NewScanner(opt)
	waitDone(t, sc)

	rep, err := sc.Clean([]string{"trash"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Freed != 33 || rep.Files != 2 {
		t.Fatalf("回收站应释放 33 字节 / 2 个文件，得到 %d / %d", rep.Freed, rep.Files)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "trash"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("回收站应已清空，还剩 %d 项", len(entries))
	}
}

func TestNonLinuxWithoutOverrideIsUnavailable(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux 上走真实系统路径")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "trash", "keep.txt")
	writeFile(t, victim, 8, time.Now())

	sc := NewScanner(Options{DataDir: dir})
	st := waitDone(t, sc)
	if st.Report.Total != 0 || st.Report.Files != 0 {
		t.Fatalf("非 Linux 不应统计本机路径: %+v", st.Report)
	}
	for _, it := range st.Report.Items {
		if it.Present {
			t.Fatalf("非 Linux 上 %s 不应判为可用", it.Key)
		}
	}
	if _, err := sc.Clean([]string{"trash"}); err == nil {
		t.Fatal("非 Linux 必须拒绝清理")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("拒绝清理时不应删除任何文件：%v", err)
	}
}

func TestEmptyDataDirDoesNotUseRelativeTrash(t *testing.T) {
	// DataDir 为空时绝不能拼出相对路径 "trash"：面板的工作目录下恰好有同名
	// 目录时，扫描会误报、清理会删错地方。
	for _, c := range catalog(Options{}) {
		if c.key != "trash" {
			continue
		}
		if len(c.roots) != 1 || c.roots[0] != "" {
			t.Fatalf("空 DataDir 的回收站根目录应为空字符串，得到 %#v", c.roots)
		}
	}
}

func TestCleanRejectsUnknownKeyBeforeDeleting(t *testing.T) {
	opt, dir := newFixture(t)
	sc := NewScanner(opt)
	waitDone(t, sc)

	if _, err := sc.Clean([]string{"apt", "../../etc/nginx"}); err == nil {
		t.Fatal("未知类别必须报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "apt", "curl_8.deb")); err != nil {
		t.Fatal("带非法键的一批清理不应删除任何文件")
	}
	if _, err := sc.Clean(nil); err == nil {
		t.Fatal("空选择必须报错")
	}
}

func TestCleanSortsNothingAndDedupsKeys(t *testing.T) {
	opt, _ := newFixture(t)
	sc := NewScanner(opt)
	waitDone(t, sc)

	rep, err := sc.Clean([]string{"crash", "crash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 1 || rep.Items[0].Key != "crash" {
		t.Fatalf("重复键应合并，得到 %+v", rep.Items)
	}
	if rep.Freed != 48 {
		t.Fatalf("应释放 48 字节，得到 %d", rep.Freed)
	}
}

func TestParseDockerSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0B", 0, true},
		{"12kB", 12000, true},
		{"800MB", 800000000, true},
		{"1.5GB", 1500000000, true},
		{"3 TB", 3e12, true},
		{"1.2GiB", 1288490188, true},
		{"123", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDockerSize(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("parseDockerSize(%q) = %d,%v，期望 %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestIsRotatedLog(t *testing.T) {
	yes := []string{"syslog.1", "dpkg.log.2.gz", "access.log-20260101.gz", "old.log.old", "x.gz"}
	no := []string{"access.log", "error.log", "syslog", "journal", "README.md", "nginx.conf", "auth.log.bak"}
	for _, n := range yes {
		if !isRotatedLog(n) {
			t.Fatalf("%q 应判为轮转归档", n)
		}
	}
	for _, n := range no {
		if isRotatedLog(n) {
			t.Fatalf("%q 不应判为轮转归档", n)
		}
	}
}
