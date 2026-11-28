package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/store"
)

// 设置里的公网地址允许连端口一起填（NAT 机器上公网端口 ≠ 监听端口）。
// 早先直接把这个字符串塞给 joinHostPort，会拼成
// "204.197.163.238:30986:47736" 这种坏链接。
func TestSplitPublicHost(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"203.0.113.9", "203.0.113.9", 0},
		{"203.0.113.9:30986", "203.0.113.9", 30986},
		{"http://203.0.113.9:30986/", "203.0.113.9", 30986},
		{" https://node.example.com:443 ", "node.example.com", 443},
		{"node.example.com", "node.example.com", 0},
		{"[2001:db8::1]:30986", "2001:db8::1", 30986},
		{"2001:db8::1", "2001:db8::1", 0},
		// 端口越界时当没写，链接回落到入站端口，而不是生成 :0 或 :99999
		{"203.0.113.9:99999", "203.0.113.9", 0},
		{"203.0.113.9:0", "203.0.113.9", 0},
	}
	for _, c := range cases {
		host, port := splitPublicHost(c.in)
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("splitPublicHost(%q) = (%q, %d)，期望 (%q, %d)",
				c.in, host, port, c.wantHost, c.wantPort)
		}
	}
}

// 端口优先级：入站对外端口 > 全局端口 > 监听端口。
func TestLinkPortPrecedence(t *testing.T) {
	in := store.Inbound{Port: 47736}
	if got := linkPort(in, 0); got != 0 {
		t.Fatalf("都没填时应返回 0（由 BuildLink 回落到监听端口），得到 %d", got)
	}
	if got := linkPort(in, 30986); got != 30986 {
		t.Fatalf("只有全局端口时应返回 30986，得到 %d", got)
	}
	in.PublicPort = 40000
	if got := linkPort(in, 30986); got != 40000 {
		t.Fatalf("入站对外端口应覆盖全局端口，得到 %d", got)
	}
}

// 没配「公网地址」时，链接端口只能来自入站自己；请求头里的端口是面板的
// 访问端口，跟节点无关。
//
// 这个 bug 很隐蔽：面板跑在 IP:20873 上时，Host 头带的是 20873，
// 被当成「全局端口」压过了入站的监听端口，于是不管新建入站随机到哪个
// 端口，复制出来的链接永远是 :20873——客户端连到面板端口，必然握手失败。
func TestPublicTargetIgnoresPanelPortFromRequest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	s := &Server{st: st, cfg: &config.Config{Listen: "127.0.0.1:20873"}}
	ctx := context.Background()

	req := httptest.NewRequest("GET", "http://203.0.113.9:20873/api/v1/proxies/overview", nil)
	host, port := s.publicTarget(req)
	if host != "203.0.113.9" {
		t.Errorf("主机应取自 Host 头，得到 %q", host)
	}
	if port != 0 {
		t.Errorf("从请求推断时不应带端口，得到 %d（会把面板端口写进节点链接）", port)
	}

	// X-Forwarded-Host 是同一类输入，同样只取主机。
	req = httptest.NewRequest("GET", "http://127.0.0.1:20873/api/v1/proxies/overview", nil)
	req.Header.Set("X-Forwarded-Host", "node.example.com:8443")
	if _, port := s.publicTarget(req); port != 0 {
		t.Errorf("X-Forwarded-Host 的端口同样不应采信，得到 %d", port)
	}

	// 用户明确写进设置里的端口仍然生效（NAT 机器的场景）。
	if err := st.SetSetting(ctx, "public_host", "203.0.113.9:30986"); err != nil {
		t.Fatal(err)
	}
	if _, port := s.publicTarget(req); port != 30986 {
		t.Errorf("显式配置的公网端口应生效，得到 %d", port)
	}

	// 只写 IP 时不带端口，链接回落入站端口。
	if err := st.SetSetting(ctx, "public_host", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if _, port := s.publicTarget(req); port != 0 {
		t.Errorf("只写 IP 时不应凭空有端口，得到 %d", port)
	}

	// 旧版本可能已经把面板端口存进去了，老配置也要能用。
	s.cfg.Listen = "0.0.0.0:20873"
	if err := st.SetSetting(ctx, "public_host", "203.0.113.9:20873"); err != nil {
		t.Fatal(err)
	}
	if _, port := s.publicTarget(req); port != 0 {
		t.Errorf("存量配置里的面板端口应被忽略，得到 %d", port)
	}

	// 面板只监听回环时，公网同号端口是空的，节点可以合法借用。
	s.cfg.Listen = "127.0.0.1:20873"
	if _, port := s.publicTarget(req); port != 20873 {
		t.Errorf("回环监听时不应判定为面板端口，得到 %d", port)
	}
}

// 设置页回填必须是原值：填了 IP:30986，刷新后还得是 IP:30986，
// 否则用户改个别的设置再保存，端口就被写没了。
func TestPublicHostSettingKeepsPort(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	s := &Server{st: st, cfg: &config.Config{Listen: "127.0.0.1:20873"}}
	req := httptest.NewRequest("GET", "http://203.0.113.9:20873/api/v1/settings", nil)

	// 没配置时回填推断到的主机名，方便用户确认链接会用哪个地址。
	if got := s.publicHostSetting(req); got != "203.0.113.9" {
		t.Errorf("未配置时应回填推断主机，得到 %q", got)
	}

	if err := st.SetSetting(context.Background(), "public_host", "203.0.113.9:30986"); err != nil {
		t.Fatal(err)
	}
	if got := s.publicHostSetting(req); got != "203.0.113.9:30986" {
		t.Errorf("已配置时应原样回填，得到 %q", got)
	}
	// 真正的链接仍然只取主机和端口两段，不会拼出双端口。
	if host, port := s.publicTarget(req); host != "203.0.113.9" || port != 30986 {
		t.Errorf("publicTarget = (%q, %d)，期望 (203.0.113.9, 30986)", host, port)
	}
}

// 设置里把面板端口当公网端口填进来是最常见的误填，保存时就要拦住。
func TestCheckPublicHostRejectsPanelPort(t *testing.T) {
	s := &Server{cfg: &config.Config{Listen: "0.0.0.0:20873"}}
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"203.0.113.9", false},
		{"node.example.com", false},
		{"203.0.113.9:30986", false}, // NAT：公网端口 ≠ 监听端口
		{"203.0.113.9:20873", true},  // 面板自己的端口
		{"https://1.2.3.4:20873/", true},
		{"203.0.113.9 30986", true}, // 空格分隔，前端容易这么粘
	} {
		err := s.checkPublicHost(tc.in)
		if tc.wantErr != (err != nil) {
			t.Errorf("checkPublicHost(%q) err=%v，期望出错=%v", tc.in, err, tc.wantErr)
		}
	}
}
