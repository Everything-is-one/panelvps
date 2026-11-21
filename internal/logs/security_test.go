package logs

import "testing"

const sampleLine = `45.148.10.7 - - [21/Nov/2026:21:04:11 +0800] "GET /.env HTTP/1.1" 404 162 "-" "Mozilla/5.0 zgrab/0.x"`

func TestParseAccessLine(t *testing.T) {
	h, ok := parseAccessLine(sampleLine)
	if !ok {
		t.Fatal("标准 combined 格式应当解析成功")
	}
	if h.IP != "45.148.10.7" || h.Method != "GET" || h.Path != "/.env" || h.Code != 404 {
		t.Fatalf("解析结果不对: %+v", h)
	}
	if h.Time.Year() != 2026 || h.Time.Month() != 11 || h.Time.Day() != 21 {
		t.Fatalf("时间解析不对: %v", h.Time)
	}

	if _, ok := parseAccessLine("这不是一行访问日志"); ok {
		t.Fatal("非日志行应当解析失败")
	}
}

func TestDetectScan(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "探测 .env（404）",
			line: `1.2.3.4 - - [21/Nov/2026:10:00:00 +0800] "GET /.env HTTP/1.1" 404 0 "-" "curl/8.4.0"`,
			want: true,
		},
		{
			name: "站长登录 WordPress 后台（200）",
			line: `1.2.3.4 - - [21/Nov/2026:10:00:00 +0800] "GET /wp-admin/ HTTP/1.1" 200 5120 "-" "Mozilla/5.0 (Windows NT 10.0) Chrome/131.0"`,
			want: false,
		},
		{
			name: "扫描器 UA + 404",
			line: `1.2.3.4 - - [21/Nov/2026:10:00:00 +0800] "GET /whatever HTTP/1.1" 404 0 "-" "Mozilla/5.0 (compatible; Nuclei - Open-source project)"`,
			want: true,
		},
		{
			name: "普通访客访问首页",
			line: `1.2.3.4 - - [21/Nov/2026:10:00:00 +0800] "GET / HTTP/1.1" 200 900 "-" "Mozilla/5.0 (Macintosh) Safari/605"`,
			want: false,
		},
		{
			name: "phpMyAdmin 探测（200 也算）",
			line: `1.2.3.4 - - [21/Nov/2026:10:00:00 +0800] "GET /phpmyadmin/ HTTP/1.1" 200 12 "-" "Mozilla/5.0"`,
			want: true,
		},
	}
	for _, c := range cases {
		h, ok := parseAccessLine(c.line)
		if !ok {
			t.Fatalf("%s: 解析失败", c.name)
		}
		_, _, got := detectScan(h)
		if got != c.want {
			t.Errorf("%s: detectScan = %v, 期望 %v", c.name, got, c.want)
		}
	}
}
