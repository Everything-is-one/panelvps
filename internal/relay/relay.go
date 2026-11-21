// Package relay 实现 TCP / UDP 端口转发（中转）。
//
// 使用场景：目标机器从本地不可达，但手上另有一台可达的机器。在中转机上建一条
// 规则，把本机端口收到的字节原样转给目标，客户端只认中转机地址。
//
// 刻意不借用 Xray：转发是纯 TCP/UDP 语义，与代理协议无关。用 net.Listen +
// io.Copy 实现有三点好处：不需要为每条规则生成一份 Xray 配置；规则增删不会
// 触发 Xray 重启（重启会掐断所有在线的代理连接）；失败面小到可以一眼看完。
package relay

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dialTimeout   = 10 * time.Second
	idleUDP       = 90 * time.Second
	maxUDPSession = 1024
	copyBufSize   = 32 << 10
	udpBufSize    = 64 << 10
)

// Rule 是一条转发规则。
type Rule struct {
	ID         int64
	Name       string
	Listen     string // 监听 IP，空或 0.0.0.0 表示全部
	ListenPort int
	Target     string // host:port
	Network    string // tcp / udp / both
	Enabled    bool
}

// Addr 返回监听地址。
func (r Rule) Addr() string {
	host := strings.TrimSpace(r.Listen)
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(r.ListenPort))
}

// Stats 是运行期统计。重启面板后清零，不做持久化——中转只是通道，
// 累计流量对排查问题没有价值，持久化反而要处理写入放大。
type Stats struct {
	Running   bool   `json:"running"`
	Up        int64  `json:"up"`
	Down      int64  `json:"down"`
	Active    int32  `json:"active"`
	Total     int64  `json:"total"`
	LastError string `json:"last_error,omitempty"`
}

// Manager 托管全部转发规则。
type Manager struct {
	logger *slog.Logger
	mu     sync.Mutex
	runs   map[int64]*runner
}

// New 构造管理器。
func New(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{logger: logger, runs: map[int64]*runner{}}
}

// Apply 把运行状态收敛到规则列表：多余或参数变化的停掉重来，缺的补上。
//
// 之所以做差分而不是"全停再全起"，是因为停掉一条正在跑的中转会掐断用户
// 当前连接。改一条规则的端口不该影响另一条。
func (m *Manager) Apply(rules []Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[int64]Rule, len(rules))
	for _, r := range rules {
		if r.Enabled {
			want[r.ID] = r
		}
	}
	for id, run := range m.runs {
		r, ok := want[id]
		if !ok || !sameRule(run.rule, r) {
			run.Stop()
			delete(m.runs, id)
		}
	}
	for id, r := range want {
		if _, ok := m.runs[id]; ok {
			continue
		}
		run := newRunner(r, m.logger)
		if err := run.Start(); err != nil {
			// 启动失败也要留在表里：前端需要看到 last_error，
			// 否则用户只会看到"已启用但没反应"。
			m.logger.Warn("中转启动失败", "name", r.Name, "addr", r.Addr(), "err", err)
			run.setErr(err.Error())
		}
		m.runs[id] = run
	}
}

// Snapshot 返回各规则的当前统计。
func (m *Manager) Snapshot() map[int64]Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64]Stats, len(m.runs))
	for id, run := range m.runs {
		out[id] = run.Stats()
	}
	return out
}

// StopAll 停掉全部规则。
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, run := range m.runs {
		run.Stop()
		delete(m.runs, id)
	}
}

func sameRule(a, b Rule) bool {
	return a.Listen == b.Listen && a.ListenPort == b.ListenPort &&
		a.Target == b.Target && a.Network == b.Network
}

// ---------- 单条规则 ----------

type runner struct {
	rule   Rule
	logger *slog.Logger

	up     atomic.Int64
	down   atomic.Int64
	active atomic.Int32
	total  atomic.Int64
	errMsg atomic.Value

	mu   sync.Mutex
	ln   net.Listener
	pc   net.PacketConn
	stop chan struct{}
	wg   sync.WaitGroup

	udpMu sync.Mutex
	udp   map[string]*udpSession
}

func newRunner(r Rule, logger *slog.Logger) *runner {
	return &runner{rule: r, logger: logger}
}

func (r *runner) setErr(msg string) {
	r.errMsg.Store(msg)
}

func (r *runner) Stats() Stats {
	st := Stats{
		Up:     r.up.Load(),
		Down:   r.down.Load(),
		Active: r.active.Load(),
		Total:  r.total.Load(),
	}
	if v, ok := r.errMsg.Load().(string); ok {
		st.LastError = v
	}
	r.mu.Lock()
	st.Running = r.ln != nil || r.pc != nil
	r.mu.Unlock()
	return st
}

func (r *runner) Start() error {
	r.mu.Lock()
	r.stop = make(chan struct{})
	r.udp = map[string]*udpSession{}
	stop := r.stop
	r.mu.Unlock()

	var errs []string
	if r.rule.Network == "tcp" || r.rule.Network == "both" {
		ln, err := net.Listen("tcp", r.rule.Addr())
		if err != nil {
			errs = append(errs, "TCP: "+err.Error())
		} else {
			r.mu.Lock()
			r.ln = ln
			r.mu.Unlock()
			r.wg.Add(1)
			go r.acceptLoop(ln, stop)
		}
	}
	if r.rule.Network == "udp" || r.rule.Network == "both" {
		pc, err := net.ListenPacket("udp", r.rule.Addr())
		if err != nil {
			errs = append(errs, "UDP: "+err.Error())
		} else {
			r.mu.Lock()
			r.pc = pc
			r.mu.Unlock()
			r.wg.Add(1)
			go r.udpLoop(pc, stop)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	joined := errors.New(strings.Join(errs, "; "))
	// 双栈规则里只挂了一半仍算部分可用，把错误记下来但不阻止另一半工作。
	if r.ln == nil && r.pc == nil {
		return joined
	}
	r.setErr(joined.Error())
	return nil
}

func (r *runner) Stop() {
	r.mu.Lock()
	stop := r.stop
	ln, pc := r.ln, r.pc
	r.ln, r.pc = nil, nil
	r.mu.Unlock()

	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	if ln != nil {
		_ = ln.Close()
	}
	if pc != nil {
		_ = pc.Close()
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// 长连接不该拖住面板重启；进程退出时内核会回收。
	}
}

func (r *runner) acceptLoop(ln net.Listener, stop chan struct{}) {
	defer r.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// 监听器被关掉或出现不可恢复错误：退出，不空转。
			return
		}
		r.active.Add(1)
		r.total.Add(1)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handleTCP(c)
		}()
	}
}

func (r *runner) handleTCP(client net.Conn) {
	defer r.active.Add(-1)
	defer client.Close()

	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	upstream, err := net.DialTimeout("tcp", r.rule.Target, dialTimeout)
	if err != nil {
		r.setErr("连接目标失败: " + err.Error())
		return
	}
	defer upstream.Close()
	if tc, ok := upstream.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
	}
	r.setErr("")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.pipe(upstream, client, &r.up)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		r.pipe(client, upstream, &r.down)
		closeWrite(client)
	}()
	wg.Wait()
}

// pipe 单向搬运并计数。
//
// 用普通的 Read/Write 而不是 io.Copy：需要在中途累计字节数，且 io.Copy 的
// 缓冲策略在大流量下会额外分配内存，低配机器上不值得。
func (r *runner) pipe(dst io.Writer, src io.Reader, ctr *atomic.Int64) {
	buf := make([]byte, copyBufSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			ctr.Add(int64(n))
		}
		if err != nil {
			return
		}
	}
}

func closeWrite(v any) {
	if cw, ok := v.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	if c, ok := v.(io.Closer); ok {
		_ = c.Close()
	}
}

func (r *runner) udpLoop(pc net.PacketConn, stop chan struct{}) {
	defer r.wg.Done()
	buf := make([]byte, udpBufSize)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
				return
			}
		}
		if n == 0 {
			continue
		}
		r.dispatchUDP(pc, addr, buf[:n])
	}
}

// dispatchUDP 按来源地址维护会话：UDP 无连接，但目标侧需要一个固定的
// 套接字把回包认回来，否则多条会话的回包会串。
func (r *runner) dispatchUDP(pc net.PacketConn, addr net.Addr, payload []byte) {
	key := addr.String()

	r.udpMu.Lock()
	s := r.udp[key]
	if s == nil {
		if len(r.udp) >= maxUDPSession {
			r.udpMu.Unlock()
			r.setErr(fmt.Sprintf("UDP 会话超过 %d 条，已丢弃新会话", maxUDPSession))
			return
		}
		raddr, err := net.ResolveUDPAddr("udp", r.rule.Target)
		if err != nil {
			r.udpMu.Unlock()
			r.setErr("解析目标失败: " + err.Error())
			return
		}
		conn, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			r.udpMu.Unlock()
			r.setErr("连接目标失败: " + err.Error())
			return
		}
		s = &udpSession{conn: conn, client: addr}
		s.touch()
		r.udp[key] = s
		r.active.Add(1)
		r.total.Add(1)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.udpReader(pc, key, s)
		}()
	}
	r.udpMu.Unlock()

	s.touch()
	if _, err := s.conn.Write(payload); err != nil {
		r.setErr("发送失败: " + err.Error())
		return
	}
	r.up.Add(int64(len(payload)))
}

func (r *runner) udpReader(pc net.PacketConn, key string, s *udpSession) {
	buf := make([]byte, udpBufSize)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(idleUDP))
		n, err := s.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Since(s.last()) >= idleUDP {
					break
				}
				continue
			}
			break
		}
		if n > 0 {
			if _, werr := pc.WriteTo(buf[:n], s.client); werr != nil {
				break
			}
			r.down.Add(int64(n))
			s.touch()
		}
	}

	_ = s.conn.Close()
	r.udpMu.Lock()
	if cur, ok := r.udp[key]; ok && cur == s {
		delete(r.udp, key)
	}
	r.udpMu.Unlock()
	r.active.Add(-1)
}

type udpSession struct {
	conn   *net.UDPConn
	client net.Addr
	seen   atomic.Int64
}

func (s *udpSession) touch() { s.seen.Store(time.Now().UnixNano()) }

func (s *udpSession) last() time.Time { return time.Unix(0, s.seen.Load()) }
