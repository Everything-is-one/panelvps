package xray

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Everything-is-one/panelvps/internal/store"
)

// findXray 优先用环境变量，其次找开发机上的便携版，最后查 PATH。
// 找不到就跳过——CI 上不装 Xray 也不应该让测试失败。
func findXray(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("PANELVPS_XRAY_BIN"); p != "" {
		return p
	}
	candidates := []string{
		`C:/Users/Administrator/.workbuddy/binaries/xray/win/xray.exe`,
		"/usr/local/bin/xray",
		"/usr/bin/xray",
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("xray"); err == nil {
		return p
	}
	t.Skip("未找到 xray 可执行文件，跳过真实配置校验")
	return ""
}

func mustUUID(t *testing.T) string {
	t.Helper()
	u, err := NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func baseInbound(protocol, network, security string, port int, opts Options) store.Inbound {
	raw, _ := marshalTestOptions(opts)
	return store.Inbound{
		ID: 1, Tag: protocol + "-" + itoa(port), Remark: "测试",
		Protocol: protocol, Listen: "0.0.0.0", Port: port,
		Network: network, Security: security, Options: raw, Enabled: true,
	}
}

func marshalTestOptions(o Options) (string, error) {
	raw, err := marshalJSON(o)
	return raw, err
}

func TestBuildConfigAllProtocols(t *testing.T) {
	bin := findXray(t)
	dir := t.TempDir()

	// 自签证书，让 TLS 入站也能过 -test
	certFile, keyFile := writeSelfSigned(t, dir)

	realityPriv, realityPub, err := NewRealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	ssKey, err := NewSSPassword("2022-blake3-aes-128-gcm")
	if err != nil {
		t.Fatal(err)
	}
	trojanPwd, _ := NewPassword(24)

	inbounds := []store.Inbound{
		baseInbound("vless", "tcp", "reality", 24443, Options{
			Dest: "www.cloudflare.com:443", ServerNames: []string{"www.cloudflare.com"},
			ShortIDs: []string{"ab12cd34"}, PrivateKey: realityPriv, PublicKey: realityPub,
		}),
		baseInbound("vless", "ws", "tls", 24444, Options{
			CertFile: certFile, KeyFile: keyFile, WSPath: "/ray",
		}),
		baseInbound("vmess", "ws", "none", 24445, Options{WSPath: "/vm"}),
		baseInbound("trojan", "tcp", "tls", 24446, Options{
			CertFile: certFile, KeyFile: keyFile,
		}),
		baseInbound("shadowsocks", "tcp", "none", 24447, Options{
			Method: "2022-blake3-aes-128-gcm",
		}),
	}

	clients := map[int64][]store.Client{
		1: {{ID: 1, InboundID: 1, Email: "alice", Secret: mustUUID(t), Flow: "xtls-rprx-vision", Enabled: true}},
		2: {{ID: 2, InboundID: 2, Email: "bob", Secret: mustUUID(t), Enabled: true}},
		3: {{ID: 3, InboundID: 3, Email: "carol", Secret: mustUUID(t), Enabled: true}},
		4: {{ID: 4, InboundID: 4, Email: "dave", Secret: trojanPwd, Enabled: true}},
		5: {{ID: 5, InboundID: 5, Email: "erin", Secret: ssKey, Enabled: true}},
	}
	// baseInbound 默认把 ID 置为 1，这里按顺序补齐，否则所有入站会共用同一批用户
	for i := range inbounds {
		inbounds[i].ID = int64(i + 1)
	}

	cfg, err := BuildConfig(Plan{
		Inbounds: inbounds, Clients: clients, APIPort: 10085,
		LogLevel: "warning", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("构建配置失败: %v", err)
	}
	raw, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 结构断言：API 入站必须在最前，且只监听回环
	if len(cfg.Inbounds) != 6 {
		t.Fatalf("入站数量应为 6（含 API），实际 %d", len(cfg.Inbounds))
	}
	if cfg.Inbounds[0].Tag != APITag || cfg.Inbounds[0].Listen != "127.0.0.1" {
		t.Fatalf("API 入站配置异常: %+v", cfg.Inbounds[0])
	}

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// 关键一步：交给真实的 xray -test 判定
	out, err := exec.Command(bin, "run", "-test", "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("xray -test 失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Configuration OK") {
		t.Fatalf("xray -test 未确认配置: %s", out)
	}
	t.Logf("五种入站配置均通过 xray -test")
}

func TestBuildConfigSkipsInactiveClients(t *testing.T) {
	now := time.Now()
	in := baseInbound("vless", "tcp", "none", 24001, Options{})
	clients := map[int64][]store.Client{
		1: {
			{ID: 1, InboundID: 1, Email: "ok", Secret: mustUUID(t), Enabled: true},
			{ID: 2, InboundID: 1, Email: "disabled", Secret: mustUUID(t), Enabled: false},
			{ID: 3, InboundID: 1, Email: "expired", Secret: mustUUID(t), Enabled: true,
				ExpiresAt: sqlTime(now.Add(-time.Hour))},
			{ID: 4, InboundID: 1, Email: "quota", Secret: mustUUID(t), Enabled: true,
				TotalBytes: 100, Up: 60, Down: 40},
		},
	}
	cfg, err := BuildConfig(Plan{Inbounds: []store.Inbound{in}, Clients: clients, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	settings := string(cfg.Inbounds[1].Settings)
	if !strings.Contains(settings, `"ok"`) {
		t.Fatalf("正常用户应写入配置: %s", settings)
	}
	for _, name := range []string{"disabled", "expired", "quota"} {
		if strings.Contains(settings, `"`+name+`"`) {
			t.Fatalf("不应写入的用户 %s 出现在配置里: %s", name, settings)
		}
	}
}

func TestBuildConfigPortConflict(t *testing.T) {
	a := baseInbound("vless", "tcp", "none", 24002, Options{})
	b := baseInbound("vmess", "tcp", "none", 24002, Options{})
	b.ID, b.Tag = 2, "vmess-24002-b"
	_, err := BuildConfig(Plan{Inbounds: []store.Inbound{a, b}})
	if err == nil || !strings.Contains(err.Error(), "重复占用") {
		t.Fatalf("应报告端口冲突，实际: %v", err)
	}
}

func TestBuildConfigRejectsBadReality(t *testing.T) {
	in := baseInbound("vless", "tcp", "reality", 24003, Options{Dest: "x:443"})
	_, err := BuildConfig(Plan{Inbounds: []store.Inbound{in}})
	if err == nil || !strings.Contains(err.Error(), "private_key") {
		t.Fatalf("缺少私钥时应报错，实际: %v", err)
	}
}

func TestBuildConfigDeterministic(t *testing.T) {
	now := time.Unix(1700000000, 0)
	in := baseInbound("vless", "tcp", "none", 24004, Options{})
	cli := map[int64][]store.Client{1: {{ID: 1, InboundID: 1, Email: "a", Secret: mustUUID(t), Enabled: true}}}
	a, err := BuildConfig(Plan{Inbounds: []store.Inbound{in}, Clients: cli, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildConfig(Plan{Inbounds: []store.Inbound{in}, Clients: cli, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := MarshalConfig(a)
	rb, _ := MarshalConfig(b)
	if string(ra) != string(rb) {
		t.Fatal("同样的输入应生成完全一致的配置（否则每次都会误重启）")
	}
}

func TestRealityKeypairRoundTrip(t *testing.T) {
	priv, pub, err := NewRealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != 43 || len(pub) != 43 {
		t.Fatalf("密钥长度应为 43，实际 %d / %d", len(priv), len(pub))
	}
	derived, err := PublicFromPrivate(priv)
	if err != nil {
		t.Fatal(err)
	}
	if derived != pub {
		t.Fatalf("公钥推导不一致: %s != %s", derived, pub)
	}
}

func TestSSKeyValidation(t *testing.T) {
	key, err := NewSSPassword("2022-blake3-aes-256-gcm")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSSKeyValid("2022-blake3-aes-256-gcm", key) {
		t.Fatal("生成的密钥应当合法")
	}
	if IsSSKeyValid("2022-blake3-aes-256-gcm", "短密钥") {
		t.Fatal("错误长度的密钥不应通过校验")
	}
}

func TestLinkGeneration(t *testing.T) {
	priv, pub, _ := NewRealityKeypair()
	in := baseInbound("vless", "tcp", "reality", 443, Options{
		Dest: "www.cloudflare.com:443", ServerNames: []string{"www.cloudflare.com"},
		ShortIDs: []string{"ab12cd34"}, PrivateKey: priv, PublicKey: pub,
	})
	c := store.Client{Email: "alice", Secret: "11111111-2222-4333-8444-555555555555", Flow: "xtls-rprx-vision"}
	link, err := BuildLink(in, c, LinkTarget{Host: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"vless://11111111-2222-4333-8444-555555555555@1.2.3.4:443",
		"security=reality", "pbk=" + pub, "sid=ab12cd34", "flow=xtls-rprx-vision",
		"sni=www.cloudflare.com", "encryption=none",
	} {
		if !strings.Contains(link, want) {
			t.Fatalf("链接缺少 %q\n%s", want, link)
		}
	}

	ss := baseInbound("shadowsocks", "tcp", "none", 8388, Options{Method: "2022-blake3-aes-128-gcm"})
	key, _ := NewSSPassword("2022-blake3-aes-128-gcm")
	ssLink, err := BuildLink(ss, store.Client{Email: "bob", Secret: key}, LinkTarget{Host: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ssLink, "ss://") || !strings.Contains(ssLink, "@example.com:8388") {
		t.Fatalf("SS 链接格式异常: %s", ssLink)
	}

	vm := baseInbound("vmess", "ws", "none", 8080, Options{WSPath: "/vm"})
	vmLink, err := BuildLink(vm, store.Client{Email: "carol", Secret: mustUUID(t)}, LinkTarget{Host: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(vmLink, "vmess://") {
		t.Fatalf("VMess 链接格式异常: %s", vmLink)
	}

	if _, err := BuildLink(in, c, LinkTarget{Host: ""}); err == nil {
		t.Fatal("缺少公网地址时应报错")
	}
}
