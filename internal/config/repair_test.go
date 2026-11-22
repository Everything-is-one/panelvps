package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 0.4.8 之前的 PatchScalar 会留下这种形态：键已改成单行，列表项还在下面。
// 这是「升级完面板起不来」的唯一现场，必须能自动修。
const brokenYAML = `listen: 127.0.0.1:20873
data_dir: ./data
ssh_guard_ban: 1h
ssh_ports: [2222]
    - 22
update_repo: Everything-is-one/panelvps
`

func TestSanitizeDropsOrphanItems(t *testing.T) {
	out, notes, err := Sanitize([]byte(brokenYAML))
	if err != nil {
		t.Fatalf("Sanitize 失败: %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("应当报告删掉了哪些行")
	}
	if strings.Contains(string(out), "- 22") {
		t.Fatalf("孤儿列表项还在：\n%s", out)
	}

	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("修复后仍无法解析: %v", err)
	}
	if len(back.SSHPorts) != 1 || back.SSHPorts[0] != 2222 {
		t.Fatalf("ssh_ports = %v，期望 [2222]", back.SSHPorts)
	}
}

// 能正常解析的配置不该被碰，也不该报告「已修复」。
func TestSanitizeLeavesGoodFileAlone(t *testing.T) {
	body, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	out, notes, err := Sanitize(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("好配置被改了: %v", notes)
	}
	if string(out) != string(body) {
		t.Fatal("好配置内容发生变化")
	}
}

// 不认识的坏法（缩进错乱）必须原样返回并报错，不能猜着改。
func TestSanitizeRefusesUnknownBreakage(t *testing.T) {
	raw := []byte("listen: 127.0.0.1:20873\n  data_dir: ./data\n bad: [\n")
	out, notes, err := Sanitize(raw)
	if err == nil {
		t.Fatalf("非法配置应当报错，notes=%v", notes)
	}
	if string(out) != string(raw) {
		t.Fatalf("原文被改动：\n%s", out)
	}
}

func TestRepairWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.yaml")
	if err := os.WriteFile(path, []byte(brokenYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, notes, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair 失败: %v", err)
	}
	if !changed || len(notes) == 0 {
		t.Fatal("应当修复")
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("没有留下备份: %v", err)
	}
	if err := Check(path); err != nil {
		t.Fatalf("修复后 Check 仍未通过: %v", err)
	}

	// 再跑一次应当是空操作
	changed, _, err = Repair(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("已经修好了还再改一次")
	}
}
