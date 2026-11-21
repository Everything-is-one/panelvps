package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// elfBytes 造一段带 ELF 魔数的假二进制，Apply 只校验头部。
func elfBytes(tag string) []byte {
	return append([]byte("\x7fELF"), []byte("fake-panelvps-"+tag)...)
}

// TestApplyAcrossDirs 模拟线上现场：新二进制在 /tmp（tmpfs），目标在 /opt，
// 两者不是同一个文件系统。旧实现直接 rename 会 EXDEV，新实现先落到目标目录再换。
func TestApplyAcrossDirs(t *testing.T) {
	root := t.TempDir()
	// 两个不同的目录，等价于「/tmp 与 /opt 不同挂载」
	srcDir := filepath.Join(root, "tmp")
	dstDir := filepath.Join(root, "opt", "bin")
	for _, d := range []string{srcDir, dstDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	newBin := filepath.Join(srcDir, "panelvps-linux-amd64")
	if err := os.WriteFile(newBin, elfBytes("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dstDir, "panelvps")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	backup, err := applyTo(newBin, target)
	if err != nil {
		t.Fatalf("applyTo 失败: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(elfBytes("new")) {
		t.Errorf("目标文件没被换成新版本，内容 = %q", got)
	}
	if fi, err := os.Stat(target); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS == "linux" && fi.Mode().Perm() != 0o755 {
		// Windows 上 chmod 只是模拟，权限位不可信，只在 Linux 断言
		t.Errorf("目标权限 = %o，期望 755", fi.Mode().Perm())
	}

	old, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("备份读不到: %v", err)
	}
	if string(old) != "old-binary" {
		t.Errorf("备份内容 = %q，期望旧版本", old)
	}
	// applyTo 会把目标路径 EvalSymlinks，Windows 上会变成 8.3 短名，
	// 所以比较前把期望值也解析一遍。
	wantDir, err := filepath.EvalSymlinks(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(backup) != wantDir {
		t.Errorf("备份应放在目标目录 %s，实际 %s", wantDir, filepath.Dir(backup))
	}

	// 目标目录里不应残留暂存文件
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".new-") {
			t.Errorf("残留暂存文件: %s", e.Name())
		}
	}
}

// TestApplyRejectsNonELF 非 ELF 必须在动旧版本之前就拒绝。
func TestApplyRejectsNonELF(t *testing.T) {
	dir := t.TempDir()
	newBin := filepath.Join(dir, "not-elf")
	if err := os.WriteFile(newBin, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "panelvps")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := applyTo(newBin, target); err == nil {
		t.Fatal("期望拒绝非 ELF 文件")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Errorf("被拒绝时旧版本不该被动过，实际 = %q", got)
	}
}

// TestApplyCreatesTargetDir 目标目录不存在时自动创建。
func TestApplyCreatesTargetDir(t *testing.T) {
	root := t.TempDir()
	newBin := filepath.Join(root, "new")
	if err := os.WriteFile(newBin, elfBytes("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "missing", "sub", "panelvps")
	if _, err := applyTo(newBin, target); err != nil {
		t.Fatalf("applyTo 失败: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("目标文件没生成: %v", err)
	}
}
