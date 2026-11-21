package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp 造一个内容已知的临时文件，返回路径与它的 sha256。
func writeTemp(t *testing.T, body string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "panelvps-linux-amd64")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return p, hex.EncodeToString(sum[:])
}

// serveSum 起一个只返回固定文本的校验和服务器。
func serveSum(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestParseChecksum 覆盖实际发布里出现过的四种写法。
func TestParseChecksum(t *testing.T) {
	const sum = "7f83b224d6555fc572e14dd73ba6fc375455904daf4e2920af94513a9ed3655e"
	cases := []struct {
		line string
		name string
		ok   bool
	}{
		{sum + "  panelvps-linux-amd64", "panelvps-linux-amd64", true},           // GNU 文本
		{sum + " *panelvps-linux-amd64", "panelvps-linux-amd64", true},           // GNU 二进制（Windows 产出）
		{sum + "  bin/panelvps-linux-amd64", "panelvps-linux-amd64", true},       // 带目录前缀
		{sum + "  ./panelvps-linux-amd64", "panelvps-linux-amd64", true},         // ./ 前缀
		{"SHA256 (panelvps-linux-amd64) = " + sum, "panelvps-linux-amd64", true}, // BSD
		{sum + "  other-file", "other-file", true},
		{"", "", false},
		{"# comment", "", false},
		{"not-a-hash  panelvps-linux-amd64", "", false},
		{sum, "", false}, // 只有哈希没有文件名
	}
	for _, c := range cases {
		h, name, ok := parseChecksum(c.line)
		if ok != c.ok || name != c.name {
			t.Errorf("parseChecksum(%q) = (%q, %q, %v)，期望 name=%q ok=%v", c.line, h, name, ok, c.name, c.ok)
		}
		if ok && h != sum && !strings.HasPrefix(c.line, "SHA256") {
			t.Errorf("parseChecksum(%q) 哈希解析错了: %q", c.line, h)
		}
	}
}

// TestVerifyFormats 确认四种写法都能真的校验通过。
func TestVerifyFormats(t *testing.T) {
	path, sum := writeTemp(t, "panelvps binary bytes")
	formats := []string{
		sum + "  panelvps-linux-amd64\n",
		sum + " *panelvps-linux-amd64\n",
		sum + "  bin/panelvps-linux-amd64\n",
		"SHA256 (panelvps-linux-amd64) = " + sum + "\n",
	}
	for _, body := range formats {
		srv := serveSum(t, body, http.StatusOK)
		ok, err := Verify(context.Background(), srv.URL, "panelvps-linux-amd64", path)
		if err != nil || !ok {
			t.Errorf("校验和内容 %q 应通过，得到 ok=%v err=%v", body, ok, err)
		}
	}
}

// TestVerifyMissingEntrySkips 文件在但没这条记录时，应返回 ChecksumSkip 而不是硬失败。
func TestVerifyMissingEntrySkips(t *testing.T) {
	path, sum := writeTemp(t, "x")
	srv := serveSum(t, sum+"  panelvps-linux-arm64\n", http.StatusOK)
	ok, err := Verify(context.Background(), srv.URL, "panelvps-linux-amd64", path)
	var skip *ChecksumSkip
	if ok || !errors.As(err, &skip) {
		t.Fatalf("期望 ChecksumSkip，得到 ok=%v err=%v", ok, err)
	}
	if !strings.Contains(skip.Reason, "panelvps-linux-amd64") {
		t.Errorf("提示里应带上文件名: %q", skip.Reason)
	}
}

// TestVerifyDownloadFailsSkips 校验和 404 时同样降级为跳过。
func TestVerifyDownloadFailsSkips(t *testing.T) {
	path, _ := writeTemp(t, "x")
	srv := serveSum(t, "Not Found", http.StatusNotFound)
	ok, err := Verify(context.Background(), srv.URL, "panelvps-linux-amd64", path)
	var skip *ChecksumSkip
	if ok || !errors.As(err, &skip) {
		t.Fatalf("期望 ChecksumSkip，得到 ok=%v err=%v", ok, err)
	}
}

// TestVerifyMismatchFails 哈希对不上必须报错，不能降级。
func TestVerifyMismatchFails(t *testing.T) {
	path, _ := writeTemp(t, "tampered")
	srv := serveSum(t, strings.Repeat("a", 64)+"  panelvps-linux-amd64\n", http.StatusOK)
	ok, err := Verify(context.Background(), srv.URL, "panelvps-linux-amd64", path)
	var skip *ChecksumSkip
	if ok || err == nil || errors.As(err, &skip) {
		t.Fatalf("哈希不匹配必须硬失败，得到 ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "sha256 不匹配") {
		t.Errorf("错误信息应说明不匹配: %v", err)
	}
}
