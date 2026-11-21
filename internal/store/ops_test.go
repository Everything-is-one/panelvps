package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 这些结构体会被直接序列化给前端。前端按 snake_case 取字段，
// 一旦有人删掉 json 标签，页面就会整片变成 undefined —— 用测试钉住。
func TestAPIModelsUseSnakeCaseJSON(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want []string
	}{
		{"Site", Site{ID: 1, Name: "a", Domain: "b", PHPVersion: "8.2", CreatedAt: time.Now()},
			[]string{"id", "name", "domain", "root", "kind", "php_version", "port", "ssl", "enabled", "created_at"}},
		{"FirewallRule", FirewallRule{ID: 1, Action: "accept", Proto: "tcp", Port: "80", CreatedAt: time.Now()},
			[]string{"id", "action", "proto", "port", "source", "comment", "enabled", "created_at"}},
		{"InstalledApp", InstalledApp{Slug: "nginx", Version: "1.22", Status: "installed", InstalledAt: time.Now()},
			[]string{"slug", "version", "status", "installed_at"}},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: 序列化失败: %v", c.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: 反序列化失败: %v", c.name, err)
		}
		for _, k := range c.want {
			if _, ok := got[k]; !ok {
				t.Errorf("%s 缺少字段 %q，实际输出：%s", c.name, k, string(raw))
			}
		}
		// Go 字段名（首字母大写）绝不能出现在响应里
		for k := range got {
			if k != "" && k[0] >= 'A' && k[0] <= 'Z' {
				t.Errorf("%s 泄漏了 Go 字段名 %q：%s", c.name, k, string(raw))
			}
		}
		// 主键必须是数字，不是 "1"
		if _, ok := got["id"]; ok {
			if _, isNum := got["id"].(float64); !isNum {
				t.Errorf("%s 的 id 应为数字：%s", c.name, string(raw))
			}
		}
		if strings.Contains(string(raw), `"ID"`) {
			t.Errorf("%s 输出含 \"ID\"：%s", c.name, string(raw))
		}
	}
}
