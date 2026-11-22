package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repair 修复已知的、由本程序早期版本写坏的配置。
//
// 目前只认一种形态：某个键被改成了单行写法（`ssh_ports: [2222]`），
// 而它原来的分行列表项还留在下面。这是 0.4.8 之前 PatchScalar 的产物，
// 症状是面板升级后起不来、日志里只有「yaml: line N: did not find expected key」。
//
// 刻意做得很窄：只删「内联值后面那些列表项」，别的语法错误一概不猜。
// 配置里每一行都是用户的环境（端口、证书邮箱、域名），猜错比不修更糟。
//
// 返回是否改动、改了哪些地方。改动前会把原文件留在 <path>.bak。
func Repair(path string) (bool, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := Check(path); err == nil {
		// 本来就没问题：什么都不做，也别说「已修复」
		return false, nil, nil
	}

	fixed, notes, err := Sanitize(raw)
	if err != nil {
		return false, nil, fmt.Errorf("按已知形态修复后仍无法解析（%v），未改动文件", err)
	}
	if len(notes) == 0 {
		return false, nil, nil
	}

	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		return false, nil, fmt.Errorf("备份原配置失败: %w", err)
	}
	if err := os.WriteFile(path, fixed, 0o600); err != nil {
		return false, nil, fmt.Errorf("写入修复后的配置失败: %w", err)
	}
	return true, notes, nil
}

// Sanitize 对一份配置原文做已知形态的修复，并在返回前验证它确实能解析。
//
// 数据来源于备份包时尤其需要这一步：备份里存的配置可能就是坏的
// （0.4.8 之前改过 SSH 端口就会出现），原样写回去等于在重启指令后面
// 埋一颗雷。
func Sanitize(raw []byte) ([]byte, []string, error) {
	out, notes := dropOrphanItems(raw)
	var probe any
	if err := yaml.Unmarshal(out, &probe); err != nil {
		return raw, nil, err
	}
	return out, notes, nil
}

// dropOrphanItems 删掉「键已经有内联值、紧跟其后的列表项」这些行。
func dropOrphanItems(raw []byte) ([]byte, []string) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	var (
		out   []string
		notes []string
	)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		out = append(out, line)

		if !hasInlineValue(line) {
			continue
		}
		// 往后看：属于这个键的孤儿列表项
		dropped := 0
		for i+1 < len(lines) && isListItem(lines[i+1]) {
			i++
			dropped++
		}
		if dropped > 0 {
			notes = append(notes, fmt.Sprintf(
				"第 %d 行 %q 已是单行写法，删掉其后的 %d 个列表项",
				i-dropped+1, strings.TrimSpace(line), dropped))
		}
	}
	if len(notes) == 0 {
		return raw, nil
	}
	return []byte(strings.Join(out, "\n")), notes
}

// hasInlineValue 判断这一行是不是「顶层键: 值」。
func hasInlineValue(line string) bool {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
		return false
	}
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return false
	}
	return strings.TrimSpace(line[idx+1:]) != ""
}

// isListItem 判断是不是 YAML 列表项（含缩进）。
func isListItem(line string) bool {
	t := strings.TrimSpace(line)
	return t == "-" || strings.HasPrefix(t, "- ")
}
