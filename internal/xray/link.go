package xray

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Everything-is-one/panelvps/internal/store"
)

// LinkTarget 描述「分享链接该指向哪里」——这批信息不在入站表里，
// 而是部署环境决定的（公网地址、域名、CDN）。
type LinkTarget struct {
	Host string // 服务器公网地址或域名
	Port int    // 覆盖入站端口（走反代 / CDN 时用），0 表示用入站端口
	SNI  string // 覆盖 TLS/REALITY 的 serverName
}

// BuildLink 为单个用户生成分享链接。
func BuildLink(in store.Inbound, c store.Client, t LinkTarget) (string, error) {
	opts, err := ParseOptions(in.Options)
	if err != nil {
		return "", err
	}

	host := strings.TrimSpace(t.Host)
	if host == "" {
		return "", fmt.Errorf("未配置服务器公网地址，无法生成分享链接")
	}
	port := in.Port
	if t.Port > 0 {
		port = t.Port
	}
	remark := linkRemark(in, c)

	switch in.Protocol {
	case "vless":
		return buildVLESS(in, c, opts, host, port, remark, t), nil
	case "vmess":
		return buildVMess(in, c, opts, host, port, remark, t)
	case "trojan":
		return buildTrojan(in, c, opts, host, port, remark, t), nil
	case "shadowsocks":
		return buildSS(in, c, opts, host, port, remark)
	default:
		return "", fmt.Errorf("不支持的协议 %q", in.Protocol)
	}
}

func buildVLESS(in store.Inbound, c store.Client, opts Options, host string, port int, remark string, t LinkTarget) string {
	q := url.Values{}
	q.Set("encryption", "none")
	q.Set("type", networkParam(in.Network))

	switch in.Security {
	case "reality":
		sni := t.SNI
		if sni == "" && len(opts.ServerNames) > 0 {
			sni = opts.ServerNames[0]
		}
		q.Set("security", "reality")
		q.Set("sni", sni)
		q.Set("fp", "chrome")
		q.Set("pbk", opts.PublicKey)
		if len(opts.ShortIDs) > 0 {
			q.Set("sid", opts.ShortIDs[0])
		}
		q.Set("spx", "/")
		// flow 只在 TCP 上有效；grpc / ws 带上去客户端会直接连不上
		if c.Flow != "" && in.Network == "tcp" {
			q.Set("flow", c.Flow)
		}
	case "tls":
		sni := t.SNI
		if sni == "" {
			sni = host
		}
		q.Set("security", "tls")
		q.Set("sni", sni)
		q.Set("fp", "chrome")
		if len(opts.Alpn) > 0 {
			q.Set("alpn", strings.Join(opts.Alpn, ","))
		}
		// flow 只在 TCP 上有效；grpc / ws 带上去客户端会直接连不上
		if c.Flow != "" && in.Network == "tcp" {
			q.Set("flow", c.Flow)
		}
	default:
		q.Set("security", "none")
		// flow 只在 TCP 上有效；grpc / ws 带上去客户端会直接连不上
		if c.Flow != "" && in.Network == "tcp" {
			q.Set("flow", c.Flow)
		}
	}

	if in.Network == "ws" {
		path := opts.WSPath
		if path == "" {
			path = "/"
		}
		q.Set("path", path)
		q.Set("host", host)
	}
	if in.Network == "grpc" {
		q.Set("serviceName", opts.GRPCService)
	}

	return "vless://" + c.Secret + "@" + joinHostPort(host, port) + "?" + q.Encode() + "#" + url.PathEscape(remark)
}

func buildVMess(in store.Inbound, c store.Client, opts Options, host string, port int, remark string, t LinkTarget) (string, error) {
	netName := networkParam(in.Network)
	obj := map[string]any{
		"v":    "2",
		"ps":   remark,
		"add":  host,
		"port": strconv.Itoa(port),
		"id":   c.Secret,
		"aid":  "0",
		"scy":  "auto",
		"net":  netName,
		"type": "none",
		"host": "",
		"path": "",
		"tls":  "",
		"sni":  "",
	}
	if in.Network == "ws" {
		path := opts.WSPath
		if path == "" {
			path = "/"
		}
		obj["path"] = path
		obj["host"] = host
	}
	if in.Network == "grpc" {
		obj["path"] = opts.GRPCService
	}
	if in.Security == "tls" {
		sni := t.SNI
		if sni == "" {
			sni = host
		}
		obj["tls"] = "tls"
		obj["sni"] = sni
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(raw), nil
}

func buildTrojan(in store.Inbound, c store.Client, opts Options, host string, port int, remark string, t LinkTarget) string {
	q := url.Values{}
	q.Set("type", networkParam(in.Network))
	if in.Security == "tls" {
		sni := t.SNI
		if sni == "" {
			sni = host
		}
		q.Set("security", "tls")
		q.Set("sni", sni)
	} else {
		q.Set("security", "none")
	}
	if in.Network == "ws" {
		path := opts.WSPath
		if path == "" {
			path = "/"
		}
		q.Set("path", path)
		q.Set("host", host)
	}
	if in.Network == "grpc" {
		q.Set("serviceName", opts.GRPCService)
	}
	return "trojan://" + url.QueryEscape(c.Secret) + "@" + joinHostPort(host, port) + "?" + q.Encode() + "#" + url.PathEscape(remark)
}

func buildSS(in store.Inbound, c store.Client, opts Options, host string, port int, remark string) (string, error) {
	if opts.Method == "" {
		return "", fmt.Errorf("入站缺少 Shadowsocks 加密方式")
	}
	userInfo := base64.RawURLEncoding.EncodeToString([]byte(opts.Method + ":" + c.Secret))
	// SIP002：ss://base64(method:password)@host:port#remark
	return "ss://" + userInfo + "@" + joinHostPort(host, port) + "#" + url.PathEscape(remark), nil
}

func linkRemark(in store.Inbound, c store.Client) string {
	base := in.Remark
	if base == "" {
		base = in.Tag
	}
	return base + "/" + c.Email
}

func networkParam(network string) string {
	if network == "" {
		return "tcp"
	}
	return network
}

// joinHostPort 只在必要时加方括号（IPv6）。
func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	return host + ":" + strconv.Itoa(port)
}
