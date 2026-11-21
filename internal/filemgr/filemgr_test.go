package filemgr

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	m, err := New(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	return m
}

func TestResolveRejectsTraversal(t *testing.T) {
	m := newTestManager(t)
	for _, p := range []string{"../etc/passwd", "../../root/.ssh/id_rsa", "a/../../b", `..\windows\system32`} {
		if _, err := m.Resolve(p); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Resolve(%q) 应被拒绝，实际 err=%v", p, err)
		}
	}
}

func TestResolveTreatsLeadingSlashAsRootRelative(t *testing.T) {
	m := newTestManager(t)
	got, err := m.Resolve("/etc/passwd")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	want := filepath.Join(m.Root(), "etc", "passwd")
	if got != want {
		t.Errorf("前导 / 应按根目录解释：got=%s want=%s", got, want)
	}
}

func TestResolveBlocksSymlinkEscape(t *testing.T) {
	m := newTestManager(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.Root(), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前系统无法创建符号链接: %v", err)
	}
	if _, err := m.Resolve("escape/secret.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("指向根目录外的符号链接应被拒绝，实际 err=%v", err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	m := newTestManager(t)
	if err := m.Mkdir("a/b"); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile("a/b/c.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := m.ReadAll("a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hello" {
		t.Errorf("读回内容不一致: %q", raw)
	}
	items, err := m.List("a/b", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "c.txt" || items[0].Path != "a/b/c.txt" {
		t.Errorf("目录列表异常: %+v", items)
	}
}

func TestWriteStreamRefusesSymlink(t *testing.T) {
	m := newTestManager(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.Root(), "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前系统无法创建符号链接: %v", err)
	}
	err := m.WriteFile("link.txt", []byte("hacked"), 0o644)
	if err == nil {
		t.Fatal("写入符号链接应被拒绝")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "original" {
		t.Errorf("根目录外的文件被改写了: %q", raw)
	}
}

func TestRemoveRefusesRoot(t *testing.T) {
	m := newTestManager(t)
	if err := m.Remove(""); err == nil {
		t.Fatal("删除根目录应被拒绝")
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	m := newTestManager(t)
	zipPath := filepath.Join(m.Root(), "evil.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../escaped.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("pwned"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.Extract("evil.zip", "out"); err == nil {
		t.Fatal("包含 ../ 的压缩包应被拒绝")
	}
	escaped := filepath.Join(filepath.Dir(m.Root()), "escaped.txt")
	if _, err := os.Stat(escaped); err == nil {
		os.Remove(escaped)
		t.Fatalf("zip slip 成功写出了根目录外的文件: %s", escaped)
	}
}

func TestArchiveExtractRoundTrip(t *testing.T) {
	m := newTestManager(t)
	if err := m.Mkdir("src"); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile("src/a.txt", []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile("src/b.txt", []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Archive([]string{"src"}, "src.tar.gz"); err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	if err := m.Extract("src.tar.gz", "dst"); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	raw, err := m.ReadAll("dst/src/a.txt")
	if err != nil {
		t.Fatalf("解压后的文件缺失: %v", err)
	}
	if string(raw) != "AAA" {
		t.Errorf("内容不一致: %q", raw)
	}
}

func TestReadAllSizeLimit(t *testing.T) {
	m := newTestManager(t)
	m.MaxRead = 4
	if err := m.WriteFile("big.bin", []byte("1234567890"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadAll("big.bin"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("应返回 ErrTooLarge，实际 %v", err)
	}
}
