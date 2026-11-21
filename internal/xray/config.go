// Package xray 负责把数据库里的入站/用户描述翻译成 Xray 配置并托管进程。
//
// 设计边界：
//   - 只做「生成配置 + 校验 + 启停 + 读统计」，不实现协议本身；
//   - 生成过程是纯函数（BuildConfig），可以被单测直接覆盖；
//   - 任何写配置的动作都必须先过 `xray -test`，失败即回滚（见 manager.go）。
package xray

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/store"
)

// Options 是入站的协议相关参数，存在 inbounds.options 里。
type Options struct {
	// REALITY
	Dest        string   `json:"dest,omitempty"`
	ServerNames []string `json:"server_names,omitempty"`
	ShortIDs    []string `json:"short_ids,omitempty"`
	PrivateKey  string   `json:"private_key,omitempty"`
	PublicKey   string   `json:"public_key,omitempty"`
	// TLS
	CertFile string   `json:"cert_file,omitempty"`
	KeyFile  string   `json:"key_file,omitempty"`
	Alpn     []string `json:"alpn,omitempty"`
	// 传输层
	WSPath      string `json:"ws_path,omitempty"`
	GRPCService string `json:"grpc_service,omitempty"`
	// Shadowsocks
	Method string `json:"method,omitempty"`
}

// ParseOptions 解析入库的 JSON，空串按空对象处理。
func ParseOptions(raw string) (Options, error) {
	var o Options
	if strings.TrimSpace(raw) == "" {
		return o, nil
	}
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return o, fmt.Errorf("解析入站参数失败: %w", err)
	}
	return o, nil
}

// Plan 是生成配置所需的全部输入。
type Plan struct {
	Inbounds []store.Inbound
	Clients  map[int64][]store.Client
	APIPort  int
	LogLevel string
	// Now 用于过期判断，测试可注入固定时间。
	Now time.Time
}

// ----- Xray 配置结构（只声明用得到的字段）-----

type Config struct {
	Log       *LogConfig     `json:"log,omitempty"`
	API       *APIConfig     `json:"api,omitempty"`
	Stats     *struct{}      `json:"stats,omitempty"`
	Policy    *PolicyConfig  `json:"policy,omitempty"`
	Inbounds  []InboundSpec  `json:"inbounds"`
	Outbounds []OutboundSpec `json:"outbounds"`
	Routing   *RoutingConfig `json:"routing,omitempty"`
}

type LogConfig struct {
	LogLevel string `json:"loglevel"`
}

type APIConfig struct {
	Tag      string   `json:"tag"`
	Services []string `json:"services"`
}

type PolicyConfig struct {
	Levels map[string]LevelPolicy `json:"levels"`
	System SystemPolicy           `json:"system"`
}

type LevelPolicy struct {
	StatsUserUplink   bool `json:"statsUserUplink"`
	StatsUserDownlink bool `json:"statsUserDownlink"`
}

type SystemPolicy struct {
	StatsInboundUplink    bool `json:"statsInboundUplink"`
	StatsInboundDownlink  bool `json:"statsInboundDownlink"`
	StatsOutboundUplink   bool `json:"statsOutboundUplink"`
	StatsOutboundDownlink bool `json:"statsOutboundDownlink"`
}

type InboundSpec struct {
	Tag      string          `json:"tag"`
	Listen   string          `json:"listen"`
	Port     int             `json:"port"`
	Protocol string          `json:"protocol"`
	Settings json.RawMessage `json:"settings"`
	Stream   *StreamSettings `json:"streamSettings,omitempty"`
	Sniffing *Sniffing       `json:"sniffing,omitempty"`
}

type OutboundSpec struct {
	Protocol string `json:"protocol"`
	Tag      string `json:"tag"`
}

type RoutingConfig struct {
	Rules []RoutingRule `json:"rules"`
}

type RoutingRule struct {
	Type        string   `json:"type"`
	InboundTag  []string `json:"inboundTag"`
	OutboundTag string   `json:"outboundTag"`
}

type Sniffing struct {
	Enabled      bool     `json:"enabled"`
	DestOverride []string `json:"destOverride"`
	RouteOnly    bool     `json:"routeOnly,omitempty"`
}

type StreamSettings struct {
	Network         string           `json:"network,omitempty"`
	Security        string           `json:"security,omitempty"`
	TLSSettings     *TLSSettings     `json:"tlsSettings,omitempty"`
	RealitySettings *RealitySettings `json:"realitySettings,omitempty"`
	WSSettings      *WSSettings      `json:"wsSettings,omitempty"`
	GRPCSettings    *GRPCSettings    `json:"grpcSettings,omitempty"`
	Sockopt         *Sockopt         `json:"sockopt,omitempty"`
}

type TLSSettings struct {
	ServerName   string        `json:"serverName,omitempty"`
	Alpn         []string      `json:"alpn,omitempty"`
	Certificates []Certificate `json:"certificates,omitempty"`
}

type Certificate struct {
	CertificateFile string `json:"certificateFile"`
	KeyFile         string `json:"keyFile"`
}

type RealitySettings struct {
	Show        bool     `json:"show"`
	Dest        string   `json:"dest"`
	Xver        int      `json:"xver"`
	ServerNames []string `json:"serverNames"`
	PrivateKey  string   `json:"privateKey"`
	ShortIDs    []string `json:"shortIds"`
}

type WSSettings struct {
	Path string `json:"path,omitempty"`
}

type GRPCSettings struct {
	ServiceName string `json:"serviceName,omitempty"`
}

// Sockopt 打开 TCP Fast Open 与拥塞控制，对高延迟链路有实际收益。
type Sockopt struct {
	TCPFastOpen      bool   `json:"tcpFastOpen,omitempty"`
	TCPCongestion    string `json:"tcpCongestion,omitempty"`
	AcceptProxyProto bool   `json:"acceptProxyProtocol,omitempty"`
}

// ---------- 协议客户端结构 ----------

type vlessClient struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Flow  string `json:"flow,omitempty"`
	Level int    `json:"level"`
}

type vmessClient struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Level   int    `json:"level"`
	AlterID int    `json:"alterId"`
}

type trojanClient struct {
	Password string `json:"password"`
	Email    string `json:"email"`
	Level    int    `json:"level"`
}

type ssClient struct {
	Password string `json:"password"`
	Email    string `json:"email"`
	Level    int    `json:"level"`
}

type vlessSettings struct {
	Clients    []vlessClient `json:"clients"`
	Decryption string        `json:"decryption"`
}

type vmessSettings struct {
	Clients []vmessClient `json:"clients"`
}

type trojanSettings struct {
	Clients []trojanClient `json:"clients"`
}

type ssSettings struct {
	Method   string     `json:"method"`
	Password string     `json:"password"`
	Clients  []ssClient `json:"clients,omitempty"`
	Network  string     `json:"network"`
}

// APITag 是内部 API 入站的固定 tag。
const APITag = "api"

// DefaultAPIPort 是 stats / 用户管理接口的默认端口（仅监听回环）。
const DefaultAPIPort = 10085

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// BuildConfig 把 Plan 翻译成 Xray 配置。
//
// 这里的顺序很重要：先按端口排序，保证同样的数据生成同样的配置，
// 这样「有没有真改动」可以直接靠字节比较判断，避免无意义重启。
func BuildConfig(p Plan) (*Config, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	apiPort := p.APIPort
	if apiPort <= 0 {
		apiPort = DefaultAPIPort
	}

	cfg := &Config{
		Log: &LogConfig{LogLevel: normalizeLogLevel(p.LogLevel)},
		API: &APIConfig{Tag: APITag, Services: []string{
			"HandlerService", "LoggerService", "StatsService",
		}},
		Stats: &struct{}{},
		Policy: &PolicyConfig{
			Levels: map[string]LevelPolicy{
				"0": {StatsUserUplink: true, StatsUserDownlink: true},
			},
			System: SystemPolicy{
				StatsInboundUplink:    true,
				StatsInboundDownlink:  true,
				StatsOutboundUplink:   true,
				StatsOutboundDownlink: true,
			},
		},
		Outbounds: []OutboundSpec{
			{Protocol: "freedom", Tag: "direct"},
			{Protocol: "blackhole", Tag: "blocked"},
			// API 入站必须有对应出站，否则路由规则指向不存在的 tag 会启动失败
			{Protocol: "blackhole", Tag: APITag},
		},
		Routing: &RoutingConfig{Rules: []RoutingRule{{
			Type:        "field",
			InboundTag:  []string{APITag},
			OutboundTag: APITag,
		}}},
	}

	// 内部 API 入站，只监听回环，公网无法访问
	cfg.Inbounds = append(cfg.Inbounds, InboundSpec{
		Tag:      APITag,
		Listen:   "127.0.0.1",
		Port:     apiPort,
		Protocol: "dokodemo-door",
		Settings: json.RawMessage(`{"address":"127.0.0.1"}`),
	})

	inbounds := make([]store.Inbound, 0, len(p.Inbounds))
	inbounds = append(inbounds, p.Inbounds...)
	sort.SliceStable(inbounds, func(i, j int) bool {
		if inbounds[i].Port == inbounds[j].Port {
			return inbounds[i].Tag < inbounds[j].Tag
		}
		return inbounds[i].Port < inbounds[j].Port
	})

	seenPorts := map[int]string{apiPort: APITag}
	for _, in := range inbounds {
		if !in.Enabled {
			continue
		}
		if other, ok := seenPorts[in.Port]; ok {
			return nil, fmt.Errorf("端口 %d 被 %s 与 %s 重复占用", in.Port, other, in.Tag)
		}
		seenPorts[in.Port] = in.Tag

		spec, err := buildInbound(in, p.Clients[in.ID], now)
		if err != nil {
			return nil, fmt.Errorf("入站 %s: %w", in.Tag, err)
		}
		cfg.Inbounds = append(cfg.Inbounds, spec)
	}
	return cfg, nil
}

func buildInbound(in store.Inbound, clients []store.Client, now time.Time) (InboundSpec, error) {
	opts, err := ParseOptions(in.Options)
	if err != nil {
		return InboundSpec{}, err
	}

	spec := InboundSpec{
		Tag:      in.Tag,
		Listen:   in.Listen,
		Port:     in.Port,
		Protocol: in.Protocol,
	}

	active := make([]store.Client, 0, len(clients))
	if in.Protocol == "shadowsocks" {
		// SS 没有「客户端列表」概念时也要能用：单用户场景允许空列表
		active = append(active, clients...)
	} else {
		for _, c := range clients {
			if c.Active(now) {
				active = append(active, c)
			}
		}
	}

	switch in.Protocol {
	case "vless":
		s := vlessSettings{Decryption: "none", Clients: []vlessClient{}}
		for _, c := range clients {
			if !c.Active(now) {
				continue
			}
			if !uuidRe.MatchString(c.Secret) {
				return InboundSpec{}, fmt.Errorf("用户 %s 的 UUID 格式非法", c.Email)
			}
			flow := c.Flow
			if in.Security != "reality" {
				flow = "" // xtls-rprx-vision 只用于 REALITY/TLS
			}
			s.Clients = append(s.Clients, vlessClient{
				ID: c.Secret, Email: c.Email, Flow: flow, Level: c.Level,
			})
		}
		spec.Settings, err = json.Marshal(s)

	case "vmess":
		s := vmessSettings{Clients: []vmessClient{}}
		for _, c := range clients {
			if !c.Active(now) {
				continue
			}
			if !uuidRe.MatchString(c.Secret) {
				return InboundSpec{}, fmt.Errorf("用户 %s 的 UUID 格式非法", c.Email)
			}
			s.Clients = append(s.Clients, vmessClient{
				ID: c.Secret, Email: c.Email, Level: c.Level, AlterID: 0,
			})
		}
		spec.Settings, err = json.Marshal(s)

	case "trojan":
		s := trojanSettings{Clients: []trojanClient{}}
		for _, c := range clients {
			if !c.Active(now) {
				continue
			}
			if c.Secret == "" {
				return InboundSpec{}, fmt.Errorf("用户 %s 缺少密码", c.Email)
			}
			s.Clients = append(s.Clients, trojanClient{
				Password: c.Secret, Email: c.Email, Level: c.Level,
			})
		}
		spec.Settings, err = json.Marshal(s)

	case "shadowsocks":
		method := opts.Method
		if method == "" {
			return InboundSpec{}, errors.New("Shadowsocks 必须指定加密方式")
		}
		s := ssSettings{Method: method, Network: "tcp,udp"}
		if len(active) == 0 {
			return InboundSpec{}, errors.New("Shadowsocks 至少需要一个用户或服务端密码")
		}
		// 2022 系列支持多用户：clients 各自持有独立密钥，
		// 服务端 password 取第一个用户，保持与单用户配置兼容。
		s.Password = active[0].Secret
		for _, c := range active {
			if c.Secret == "" {
				return InboundSpec{}, fmt.Errorf("用户 %s 缺少密码", c.Email)
			}
			s.Clients = append(s.Clients, ssClient{
				Password: c.Secret, Email: c.Email, Level: c.Level,
			})
		}
		spec.Settings, err = json.Marshal(s)

	default:
		return InboundSpec{}, fmt.Errorf("不支持的协议 %q", in.Protocol)
	}
	if err != nil {
		return InboundSpec{}, err
	}

	// 传输层
	stream := &StreamSettings{Network: in.Network}
	if stream.Network == "" {
		stream.Network = "tcp"
	}
	switch stream.Network {
	case "ws":
		path := opts.WSPath
		if path == "" {
			path = "/"
		}
		stream.WSSettings = &WSSettings{Path: path}
	case "grpc":
		stream.GRPCSettings = &GRPCSettings{ServiceName: opts.GRPCService}
	case "tcp":
		stream.Sockopt = &Sockopt{TCPFastOpen: true, TCPCongestion: "bbr"}
	}

	switch in.Security {
	case "reality":
		if stream.Network != "tcp" && stream.Network != "grpc" {
			return InboundSpec{}, errors.New("REALITY 只支持 tcp / grpc 传输")
		}
		if opts.PrivateKey == "" || opts.Dest == "" || len(opts.ServerNames) == 0 {
			return InboundSpec{}, errors.New("REALITY 需要 private_key、dest 与 server_names")
		}
		shortIDs := opts.ShortIDs
		if len(shortIDs) == 0 {
			shortIDs = []string{""}
		}
		stream.Security = "reality"
		stream.RealitySettings = &RealitySettings{
			Show:        false,
			Dest:        opts.Dest,
			Xver:        0,
			ServerNames: opts.ServerNames,
			PrivateKey:  opts.PrivateKey,
			ShortIDs:    shortIDs,
		}
	case "tls":
		if opts.CertFile == "" || opts.KeyFile == "" {
			return InboundSpec{}, errors.New("TLS 需要证书与私钥路径")
		}
		stream.Security = "tls"
		stream.TLSSettings = &TLSSettings{
			Alpn:         opts.Alpn,
			Certificates: []Certificate{{CertificateFile: opts.CertFile, KeyFile: opts.KeyFile}},
		}
	case "none", "":
		// 不加密
	default:
		return InboundSpec{}, fmt.Errorf("不支持的安全类型 %q", in.Security)
	}
	spec.Stream = stream
	spec.Sniffing = &Sniffing{Enabled: true, DestOverride: []string{"http", "tls", "quic"}}

	return spec, nil
}

func normalizeLogLevel(l string) string {
	switch strings.ToLower(l) {
	case "debug", "info", "warning", "error", "none":
		if l == "info" {
			return "info"
		}
		return strings.ToLower(l)
	default:
		return "warning"
	}
}

// MarshalConfig 输出便于人眼排查的 JSON。
func MarshalConfig(c *Config) ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}
