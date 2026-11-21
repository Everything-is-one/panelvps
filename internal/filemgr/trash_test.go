package filemgr

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTrashPutRestore(t *testing.T) {
	root := t.TempDir()
	tr, err := NewTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatalf("创建回收站: %v", err)
	}

	// 目录里套文件，验证递归搬运与回填
	dir := filepath.Join(root, "conf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(target, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}

	item, err := tr.Put(target, "conf/app.conf")
	if err != nil {
		t.Fatalf("移入回收站: %v", err)
	}
	if item.IsDir || item.Size != 5 {
		t.Fatalf("条目信息不对: %+v", item)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("原文件应已不存在，err=%v", err)
	}

	items, err := tr.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("列表异常: %v %d", err, len(items))
	}
	if items[0].Original != "conf/app.conf" {
		t.Fatalf("原路径记录不对: %q", items[0].Original)
	}

	back, err := tr.Restore(item.ID, root)
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if back.Original != "conf/app.conf" {
		t.Fatalf("恢复返回的原路径不对: %q", back.Original)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("恢复后内容不对: %q %v", raw, err)
	}
	if items, _ := tr.List(); len(items) != 0 {
		t.Fatalf("恢复后回收站应为空，实际 %d 条", len(items))
	}
}

func TestTrashRestoreRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	tr, err := NewTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "a.txt")
	_ = os.WriteFile(target, []byte("v1"), 0o644)
	item, err := tr.Put(target, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// 删除后又建了同名文件：恢复必须拒绝，不能覆盖用户新写的文件
	_ = os.WriteFile(target, []byte("v2"), 0o644)
	if _, err := tr.Restore(item.ID, root); !errors.Is(err, ErrExists) {
		t.Fatalf("期望 ErrExists，实际 %v", err)
	}
}

func TestTrashPutDirectoryAndPurge(t *testing.T) {
	root := t.TempDir()
	tr, _ := NewTrash(filepath.Join(t.TempDir(), "trash"))
	dir := filepath.Join(root, "site")
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "index.html"), []byte("<h1>x</h1>"), 0o644)

	item, err := tr.Put(dir, "site")
	if err != nil {
		t.Fatalf("移入目录: %v", err)
	}
	if !item.IsDir || item.Size == 0 {
		t.Fatalf("目录条目信息不对: %+v", item)
	}
	if err := tr.Purge(item.ID); err != nil {
		t.Fatalf("彻底删除: %v", err)
	}
	if items, _ := tr.List(); len(items) != 0 {
		t.Fatalf("清除后应为空")
	}
}

func TestTrashSweep(t *testing.T) {
	root := t.TempDir()
	tr, _ := NewTrash(filepath.Join(t.TempDir(), "trash"))
	target := filepath.Join(root, "old.txt")
	_ = os.WriteFile(target, []byte("x"), 0o644)
	item, _ := tr.Put(target, "old.txt")

	// 把删除时间改到 8 天前，Sweep 7 天应清掉它
	itemDir := filepath.Join(tr.Dir(), item.ID)
	item.DeletedAt = time.Now().Add(-8 * 24 * time.Hour)
	if err := writeMeta(itemDir, item); err != nil {
		t.Fatal(err)
	}
	if n := tr.Sweep(7 * 24 * time.Hour); n != 1 {
		t.Fatalf("应清理 1 条，实际 %d", n)
	}
	if items, _ := tr.List(); len(items) != 0 {
		t.Fatalf("清理后应为空")
	}
}

func TestTrashRejectsBadID(t *testing.T) {
	tr, _ := NewTrash(filepath.Join(t.TempDir(), "trash"))
	for _, id := range []string{"", "..", "../etc", `..\x`, "a/b"} {
		if err := tr.Purge(id); err == nil {
			t.Fatalf("编号 %q 应被拒绝", id)
		}
	}
}

func TestGuardBlocksSystemDirs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("系统目录按 Linux 语义判断")
	}
	g := NewGuard("/opt/panelvps/data", "/opt/panelvps/conf")
	cases := map[string]bool{
		"/":                        true,
		"/etc":                     true,
		"/usr":                     true,
		"/var/lib":                 true,
		"/opt/panelvps/data":       true,
		"/etc/nginx/conf.d/x.conf": false,
		"/home/user/app/index.php": false,
		"/opt/panelvps/www/a.txt":  false,
	}
	for p, blocked := range cases {
		err := g.Check(p)
		if blocked && err == nil {
			t.Errorf("%s 应被拦截", p)
		}
		if !blocked && err != nil {
			t.Errorf("%s 不应被拦截：%v", p, err)
		}
	}
	if err := g.Check("/etc"); err == nil || !strings.Contains(err.Error(), "系统关键目录") {
		t.Errorf("拦截原因应说明是系统目录，实际 %v", err)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/mnt/my\040disk`); got != "/mnt/my disk" {
		t.Fatalf("got %q", got)
	}
	if got := unescapeMount("/plain"); got != "/plain" {
		t.Fatalf("got %q", got)
	}
}
