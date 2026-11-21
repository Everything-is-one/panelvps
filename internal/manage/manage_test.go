package manage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccessURLs(t *testing.T) {
	got := AccessURLs([]string{"1.2.3.4", "1.2.3.4", "example.com"}, 20873, true)
	want := []string{"http://1.2.3.4:20873", "http://example.com:20873"}
	if len(got) != len(want) {
		t.Fatalf("数量不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符: got %q want %q", i, got[i], want[i])
		}
	}

	loop := AccessURLs([]string{"1.2.3.4"}, 20873, false)
	if len(loop) != 1 || loop[0] != "http://127.0.0.1:20873" {
		t.Fatalf("仅本机时不应展示公网地址: %v", loop)
	}
}

func TestAccessURLsSkipsBlank(t *testing.T) {
	got := AccessURLs([]string{" ", "", "10.0.0.1"}, 8080, true)
	if len(got) != 1 || got[0] != "http://10.0.0.1:8080" {
		t.Fatalf("空地址应被跳过: %v", got)
	}
}

// patchListen 只动 listen 那一行。曾经这里走的是 config.Load → cfg.Save，
// 而 Load 会把 data_dir: ./data 按执行命令时的目录解析成绝对路径，落盘后
// systemd 启动就去别的目录找数据库了。
func TestPatchListenKeepsOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	body := "listen: 127.0.0.1:20873\n" +
		"data_dir: ./data\n" +
		"db_path: \"\"\n" +
		"nested:\n  listen: 127.0.0.1:1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := patchListen(path, "0.0.0.0:20900"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	for _, want := range []string{
		"listen: 0.0.0.0:20900",
		"data_dir: ./data",
		`db_path: ""`,
		"nested:\n  listen: 127.0.0.1:1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("期望保留 %q，实际内容：\n%s", want, got)
		}
	}
	// 顶层的旧值必须消失，且不能多于一处
	if strings.Contains(got, "listen: 127.0.0.1:20873") {
		t.Fatalf("顶层 listen 没被替换：\n%s", got)
	}
}
