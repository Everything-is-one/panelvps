package geoip

import (
	"net"
	"testing"
)

// 内网与保留地址不该发到外部接口查询，也不该显示成某个国家；
// 解析不了的字符串直接跳过（宁可空白，不要标成「内网」误导）。
func TestLookupPrivateIPs(t *testing.T) {
	r := New()
	got := r.Lookup(t.Context(), []string{
		"127.0.0.1", "10.0.0.4", "192.168.1.1", "172.16.0.1", "::1", "fd00::1", "not-an-ip",
	})
	if len(got) != 6 {
		t.Fatalf("应识别 6 个内网地址并跳过 1 个非法值，实际 %d 条: %v", len(got), got)
	}
	for ip, info := range got {
		if info.Country != "内网" {
			t.Errorf("%s 应标记为内网，实际 %q", ip, info.Country)
		}
	}
	if _, ok := got["not-an-ip"]; ok {
		t.Error("非法 IP 不应出现在结果里")
	}
}

func TestIsPrivate(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.4", "192.168.1.1", "172.16.0.1", "::1", "fd00::1"} {
		if ip := net.ParseIP(s); ip == nil || !isPrivate(ip) {
			t.Errorf("%s 应判为内网", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2001:4860:4860::8888"} {
		if ip := net.ParseIP(s); ip != nil && isPrivate(ip) {
			t.Errorf("%s 是公网地址", s)
		}
	}
}

// 缓存命中时不应重复请求：用一个指向本地的 count 记录。
func TestCacheSkipsSecondLookup(t *testing.T) {
	r := New()
	// 预置一条缓存，命中后直接返回，不会再走网络
	r.cache["1.1.1.1"] = cacheEntry{info: Info{Country: "澳大利亚", Code: "AU"}, expires: r.now().Add(hitTTL)}

	got := r.Lookup(t.Context(), []string{"1.1.1.1"})
	if got["1.1.1.1"].Country != "澳大利亚" {
		t.Fatalf("缓存未命中: %v", got)
	}
}

// Addr 拼地址时要吃掉重复段：ip-api 对北京 / 新加坡这类城市国家
// 会把同名值同时塞进 region 和 city，直接拼就是「北京 北京」。
func TestInfoAddr(t *testing.T) {
	cases := []struct {
		name string
		in   Info
		want string
	}{
		{"国家省市齐全", Info{Country: "美国", Region: "加利福尼亚州", City: "洛杉矶"}, "美国 加利福尼亚州 洛杉矶"},
		{"只有国家", Info{Country: "新加坡", Code: "SG"}, "新加坡"},
		{"省市重名", Info{Country: "中国", Region: "北京", City: "北京"}, "中国 北京"},
		{"三段都重名", Info{Country: "新加坡", Region: "新加坡", City: "新加坡"}, "新加坡"},
		{"省市为空但有空格", Info{Country: "荷兰", Region: "  ", City: "阿姆斯特丹"}, "荷兰 阿姆斯特丹"},
		{"全空", Info{}, ""},
	}
	for _, c := range cases {
		if got := c.in.Addr(); got != c.want {
			t.Errorf("%s: Addr() = %q, want %q", c.name, got, c.want)
		}
	}
}
