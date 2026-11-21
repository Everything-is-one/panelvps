package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateListExtractPrune(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "backups")
	m, err := New(dir, dataDir)
	if err != nil {
		t.Fatal(err)
	}

	f, err := m.Create(context.Background(), "unit-test", func(ctx context.Context, staging string) error {
		if err := os.WriteFile(filepath.Join(staging, "panel.db"), []byte("SQLite format 3\x00payload"), 0o600); err != nil {
			return err
		}
		return os.MkdirAll(filepath.Join(staging, "conf"), 0o700)
	})
	if err != nil {
		t.Fatalf("创建备份失败: %v", err)
	}
	if !nameRe.MatchString(f.Name) {
		t.Errorf("备份名不符合约定: %s", f.Name)
	}

	files, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("备份数量错误: %d", len(files))
	}

	dst := filepath.Join(dataDir, "restore")
	if err := m.Extract(f.Name, dst); err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if !LooksLikeSQLite(filepath.Join(dst, "panel.db")) {
		t.Error("展开后的数据库缺少 SQLite 头")
	}
	if _, err := os.Stat(filepath.Join(dst, "NOTE.txt")); err != nil {
		t.Errorf("备注文件缺失: %v", err)
	}

	// 再生成两份，Prune 只保留最新 1 份
	for i := 0; i < 2; i++ {
		if _, err := m.Create(context.Background(), "", func(ctx context.Context, staging string) error {
			return os.WriteFile(filepath.Join(staging, "panel.db"), []byte("SQLite format 3\x00x"), 0o600)
		}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := m.Prune(1)
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Error("Prune 未删除任何文件")
	}
	files, _ = m.List()
	if len(files) != 1 {
		t.Errorf("Prune 后应只剩 1 份，实际 %d", len(files))
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	m, err := New(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../panelvps-20260101-000000.tar.gz", "panelvps-20260101-000000.tar.gz/../../x", "evil.tar.gz"} {
		if _, err := m.Resolve(name); err == nil {
			t.Errorf("非法备份名被放行: %s", name)
		}
	}
}

func TestExtractRejectsTraversalEntry(t *testing.T) {
	dataDir := t.TempDir()
	// 手工构造一个包含 ../ 的归档不在本包职责内（写入端由 addTree 控制），
	// 这里验证 safeJoin 本身。
	if _, err := safeJoin(dataDir, "../escape.txt"); err == nil {
		t.Fatal("safeJoin 应拒绝 ../")
	}
	if _, err := safeJoin(dataDir, "/abs.txt"); err == nil {
		t.Fatal("safeJoin 应拒绝绝对路径")
	}
}

func TestLooksLikeSQLite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	os.WriteFile(p, []byte("not a database"), 0o600)
	if LooksLikeSQLite(p) {
		t.Error("非 SQLite 文件被误判")
	}
}
