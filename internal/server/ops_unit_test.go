package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Everything-is-one/panelvps/deploy"
)

func TestNormalizeUnit(t *testing.T) {
	a := "ProtectSystem=full\nReadWritePaths=-/usr  \n"
	b := "ProtectSystem=full\r\nReadWritePaths=-/usr"
	if normalizeUnit(a) != normalizeUnit(b) {
		t.Fatalf("行尾空白与 CRLF 应被视为等价")
	}
	if normalizeUnit("ProtectSystem=strict") == normalizeUnit("ProtectSystem=full") {
		t.Fatal("内容不同不应判为等价")
	}
}

func TestInspectUnitMatchesBuiltin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "panelvps.service")
	if err := os.WriteFile(p, []byte(deploy.ServiceUnit), 0o644); err != nil {
		t.Fatal(err)
	}
	st := inspectUnitAt(p)
	if !st.Present || !st.Match {
		t.Fatalf("内置模板应判为一致: %+v", st)
	}
	if !st.Writable {
		t.Fatalf("临时文件应可写: %+v", st)
	}
}

func TestInspectUnitDetectsOldVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "panelvps.service")
	old := strings.Replace(deploy.ServiceUnit, "ReadWritePaths=-/etc -/usr -/var",
		"ReadWritePaths=-/usr/local/share/xray", 1)
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	st := inspectUnitAt(p)
	if !st.Present || st.Match {
		t.Fatalf("旧单元应判为不一致: %+v", st)
	}
	if st.Reason == "" {
		t.Fatal("不一致时应给出原因")
	}
}

func TestInspectUnitMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.service")
	st := inspectUnitAt(p)
	if st.Present {
		t.Fatalf("不存在的文件不应判为存在: %+v", st)
	}
	if runtime.GOOS != "windows" && !st.Writable {
		// 父目录可写时，允许创建新单元
		t.Fatalf("父目录可写应视为可写: %+v", st)
	}
}
