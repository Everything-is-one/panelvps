package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 面板自己的日志文件存在时，来源列表要能报出大小与修改时间——
// 前端靠它显示「这个日志现在多大」，也是清理按钮的决策依据。
func TestSourcesReportsSizeForFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.log")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := Sources(dir)
	if err != nil {
		t.Fatal(err)
	}
	var panel *Source
	for i := range items {
		if items[i].Key == "panel" {
			panel = &items[i]
		}
	}
	if panel == nil {
		t.Fatal("没找到 panel 来源")
	}
	if panel.Kind != "file" {
		t.Fatalf("Kind = %s, want file", panel.Kind)
	}
	if panel.Size != int64(len("line one\nline two\n")) {
		t.Fatalf("Size = %d, want %d", panel.Size, len("line one\nline two\n"))
	}
	if panel.ModTime == "" {
		t.Fatal("ModTime 为空，前端就显示不出「最后写入」")
	}
}

// 读取结果里也带当前大小：每次刷新都能看到文件变大，不必再拉一次来源列表
// （来源列表会为 journal 来源跑 systemctl show，代价不低）。
func TestReadReportsSize(t *testing.T) {
	dir := t.TempDir()
	body := "a\nb\nc\n"
	if err := os.WriteFile(filepath.Join(dir, "panel.log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Read("panel", Options{DataDir: dir, Lines: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(body)) {
		t.Fatalf("Size = %d, want %d", res.Size, len(body))
	}
	if len(res.Lines) != 3 {
		t.Fatalf("Lines = %d, want 3", len(res.Lines))
	}
}

// 清空必须「截断」而不是「删除」：删掉文件后 nginx / 面板还拿着旧 inode 写，
// 日志会进入一个谁也打不开的幽灵文件。这里钉住「文件还在、大小为 0」。
func TestCleanTruncatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Clean("panel", Options{DataDir: dir}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before != 10 || res.After != 0 || res.Freed != 10 {
		t.Fatalf("before/after/freed = %d/%d/%d, want 10/0/10", res.Before, res.After, res.Freed)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("文件被删掉了：%v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("Size = %d, want 0", st.Size())
	}
}

// 未知来源不能当成路径处理——日志接口的白名单是整个模块的安全边界。
func TestCleanRejectsUnknownSource(t *testing.T) {
	if _, err := Clean("../../etc/passwd", Options{DataDir: t.TempDir()}, 0); err != ErrUnknownSource {
		t.Fatalf("err = %v, want ErrUnknownSource", err)
	}
}

// 文件来源的路径是写死的（nginx 访问日志等）。logrotate 把文件改名的那一瞬间
// 路径不存在，此时清理必须给出明确错误，而不是 panic 或静默成功。
func TestCleanMissingFile(t *testing.T) {
	if _, err := os.Stat("/var/log/nginx/access.log"); err == nil {
		t.Skip("本机真的有 nginx 访问日志，跳过")
	}
	_, err := Clean("nginx-access", Options{DataDir: t.TempDir()}, 0)
	if err == nil {
		t.Fatal("文件不存在时不该静默成功")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("err = %v, want 提到「不存在」", err)
	}
}

func TestJournalTargetFloor(t *testing.T) {
	// 目标小于下限时按 8MB 处理，避免把当次会话的日志也删掉
	if minJournalKeep != 8<<20 {
		t.Fatalf("minJournalKeep = %d, want 8MiB", minJournalKeep)
	}
}
