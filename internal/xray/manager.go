package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager 托管 Xray 子进程。
//
// 为什么不交给 systemd：面板要能在「改一个用户」后立刻生效，
// 自己持有进程才能做到改完即重启、失败即回滚。
// M2 之后会补一个 external 模式，允许接管已有的 xray.service。
type Manager struct {
	binPath  string
	confDir  string
	apiPort  int
	logLevel string
	logger   *slog.Logger

	mu        sync.Mutex
	cmd       *exec.Cmd
	startedAt time.Time
	stopCh    chan struct{}
	wg        sync.WaitGroup
	stopping  bool

	restarts int
	lastErr  string
	lastLog  *ringBuffer
	version  string
}

// Status 是面板展示的进程状态。
type Status struct {
	Available  bool   `json:"available"`
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	UptimeSec  int64  `json:"uptime_sec"`
	Version    string `json:"version,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	Restarts   int    `json:"restarts"`
	ConfigPath string `json:"config_path"`
	APIPort    int    `json:"api_port"`
	BinaryPath string `json:"binary_path,omitempty"`
	RecentLog  string `json:"recent_log,omitempty"`
}

// ApplyResult 描述一次配置应用的结果。
type ApplyResult struct {
	Changed    bool   `json:"changed"`
	Restarted  bool   `json:"restarted"`
	TestOutput string `json:"test_output,omitempty"`
	Backup     string `json:"backup,omitempty"`
}

// ErrNotAvailable 表示没有找到 Xray 可执行文件。
var ErrNotAvailable = errors.New("未找到 Xray 可执行文件")

// Detect 在常见位置寻找 xray，返回绝对路径。
//
// 顺序：显式配置 → /usr/local/bin → /usr/bin → PATH → 面板同目录。
func Detect(explicit string) (string, error) {
	candidates := []string{}
	// 环境变量优先级最高，便于在容器或非标准路径下部署
	if env := os.Getenv("PANELVPS_XRAY_BIN"); env != "" {
		candidates = append(candidates, env)
	}
	if strings.TrimSpace(explicit) != "" {
		candidates = append(candidates, explicit)
	}
	candidates = append(candidates,
		"/usr/local/bin/xray",
		"/usr/bin/xray",
		"/opt/xray/xray",
	)
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "xray"))
	}
	if p, err := exec.LookPath("xray"); err == nil {
		candidates = append(candidates, p)
	}

	for _, c := range candidates {
		st, err := os.Stat(c)
		if err != nil || st.IsDir() {
			continue
		}
		if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
			continue
		}
		abs, err := filepath.Abs(c)
		if err != nil {
			abs = c
		}
		return abs, nil
	}
	return "", ErrNotAvailable
}

// NewManager 构造管理器。binPath 为空（或不存在）时返回的管理器处于不可用状态。
func NewManager(binPath, confDir string, apiPort int, logLevel string, logger *slog.Logger) *Manager {
	if apiPort <= 0 {
		apiPort = DefaultAPIPort
	}
	return &Manager{
		binPath:  binPath,
		confDir:  confDir,
		apiPort:  apiPort,
		logLevel: logLevel,
		logger:   logger,
		lastLog:  newRingBuffer(8 << 10),
	}
}

// Available 表示是否探测到了可执行文件。
func (m *Manager) Available() bool { return m.binPath != "" }

// ConfigPath 返回主配置路径。
func (m *Manager) ConfigPath() string { return filepath.Join(m.confDir, "config.json") }

// EnsureVersion 读取一次版本号，失败不致命。
func (m *Manager) EnsureVersion(ctx context.Context) {
	if !m.Available() || m.version != "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, m.binPath, "version").CombinedOutput()
	if err != nil {
		return
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	m.mu.Lock()
	m.version = line
	m.mu.Unlock()
}

// Status 返回当前状态快照。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := Status{
		Available:  m.Available(),
		ConfigPath: m.ConfigPath(),
		APIPort:    m.apiPort,
		BinaryPath: m.binPath,
		Version:    m.version,
		LastError:  m.lastErr,
		Restarts:   m.restarts,
	}
	if m.cmd != nil && m.cmd.Process != nil {
		st.Running = true
		st.PID = m.cmd.Process.Pid
		if !m.startedAt.IsZero() {
			st.UptimeSec = int64(time.Since(m.startedAt).Seconds())
		}
	}
	if m.lastLog != nil {
		st.RecentLog = m.lastLog.Tail(2048)
	}
	return st
}

// Test 对指定配置文件执行 `xray -test`，返回原始输出。
func (m *Manager) Test(ctx context.Context, path string) (string, error) {
	if !m.Available() {
		return "", ErrNotAvailable
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var buf bytes.Buffer
	cmd := exec.CommandContext(c, m.binPath, "run", "-test", "-c", path)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, fmt.Errorf("xray -test 失败: %w", err)
	}
	if !strings.Contains(out, "Configuration OK") {
		return out, errors.New("xray -test 未返回 Configuration OK")
	}
	return out, nil
}

// Apply 校验并应用配置。
//
// 顺序：写临时文件 → -test → 与当前配置比对 → 备份 → 原子替换 → 重启 → 等端口就绪。
// 任一步失败都不会留下半截配置：要么是旧配置在跑，要么明确报错。
func (m *Manager) Apply(ctx context.Context, cfg *Config) (*ApplyResult, error) {
	if !m.Available() {
		return nil, ErrNotAvailable
	}
	raw, err := MarshalConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.confDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建配置目录失败: %w", err)
	}

	final := m.ConfigPath()
	// 注意：xray 按扩展名判断配置格式，临时文件必须以 .json 结尾
	tmp := filepath.Join(m.confDir, "config.tmp.json")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return nil, fmt.Errorf("写入临时配置失败: %w", err)
	}

	testOut, err := m.Test(ctx, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return &ApplyResult{TestOutput: testOut}, err
	}

	current, readErr := os.ReadFile(final)
	if readErr == nil && bytes.Equal(current, raw) && m.Running() {
		_ = os.Remove(tmp)
		return &ApplyResult{Changed: false, TestOutput: testOut}, nil
	}

	// 备份上一份可用配置，回滚时直接换回来
	backup := final + ".bak"
	if readErr == nil {
		if err := os.WriteFile(backup, current, 0o600); err != nil {
			m.logger.Warn("备份旧配置失败", "err", err)
			backup = ""
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, fmt.Errorf("替换配置失败: %w", err)
	}

	if err := m.Restart(ctx); err != nil {
		// 回滚
		if backup != "" {
			if rbErr := os.Rename(backup, final); rbErr == nil {
				_ = m.Restart(ctx)
			}
		}
		return &ApplyResult{Changed: true, TestOutput: testOut, Backup: backup},
			fmt.Errorf("新配置启动失败，已回滚: %w", err)
	}

	return &ApplyResult{Changed: true, Restarted: true, TestOutput: testOut, Backup: backup}, nil
}

// Running 判断子进程是否在跑。
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil && m.cmd.Process != nil
}

// Start 启动子进程（若已在运行则直接返回）。
func (m *Manager) Start(ctx context.Context) error {
	if !m.Available() {
		return ErrNotAvailable
	}
	m.mu.Lock()
	if m.cmd != nil {
		m.mu.Unlock()
		return nil
	}
	if _, err := os.Stat(m.ConfigPath()); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("配置文件不存在，请先应用配置: %w", err)
	}
	stop := make(chan struct{})
	m.stopCh = stop
	m.stopping = false
	m.wg.Add(1)
	m.mu.Unlock()

	go m.supervise(stop)

	// 等 API 端口就绪，最多 5 秒：进程起来了但配置坏掉的情况很常见，
	// 只看 PID 会给出「启动成功」的假象。
	if err := m.waitReady(5 * time.Second); err != nil {
		return err
	}
	return nil
}

// Stop 停止子进程。
func (m *Manager) Stop() {
	m.mu.Lock()
	stop := m.stopCh
	m.stopping = true
	cmd := m.cmd
	m.mu.Unlock()

	if stop != nil {
		close(stop)
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(terminateSignal())
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	} else {
		m.wg.Wait()
	}

	m.mu.Lock()
	m.cmd = nil
	m.stopCh = nil
	m.mu.Unlock()
}

// Restart 重启子进程。
func (m *Manager) Restart(ctx context.Context) error {
	m.Stop()
	return m.Start(ctx)
}

// supervise 是唯一的进程管理者：只管「让它活着」，重启策略集中在这里。
func (m *Manager) supervise(stop chan struct{}) {
	defer m.wg.Done()

	backoff := time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}

		cmd := exec.Command(m.binPath, "run", "-c", m.ConfigPath())
		cmd.Stdout = m.lastLog
		cmd.Stderr = m.lastLog
		if err := cmd.Start(); err != nil {
			m.recordError(fmt.Sprintf("启动失败: %v", err))
			if !m.sleep(stop, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		m.mu.Lock()
		m.cmd = cmd
		m.startedAt = time.Now()
		m.mu.Unlock()
		m.logger.Info("xray 已启动", "pid", cmd.Process.Pid)

		err := cmd.Wait()

		m.mu.Lock()
		m.cmd = nil
		m.mu.Unlock()

		select {
		case <-stop:
			m.logger.Info("xray 已停止")
			return
		default:
		}

		if err != nil {
			m.recordError(fmt.Sprintf("进程退出: %v", err))
		} else {
			m.recordError("进程退出: 退出码 0")
		}
		m.mu.Lock()
		m.restarts++
		m.mu.Unlock()
		m.logger.Warn("xray 意外退出，准备重启", "err", err, "backoff", backoff.String())

		if !m.sleep(stop, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (m *Manager) sleep(stop chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

func (m *Manager) recordError(msg string) {
	m.mu.Lock()
	m.lastErr = msg
	m.mu.Unlock()
}

// waitReady 等 API 端口可连接。
func (m *Manager) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(m.apiPort))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	m.mu.Lock()
	log := ""
	if m.lastLog != nil {
		log = m.lastLog.Tail(1024)
	}
	m.mu.Unlock()
	return fmt.Errorf("xray 未在 %s 内就绪（端口 %d），最近日志: %s", timeout, m.apiPort, log)
}

// StatEntry 是一条统计计数。
type StatEntry struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

type statsResponse struct {
	Stat []StatEntry `json:"stat"`
}

// flexInt 兼容 Xray 各版本把 value 输出为数字或字符串。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt(v)
	return nil
}

// QueryStats 通过 xray 自带 CLI 读取统计，避免引入 gRPC 与 protobuf 依赖。
//
// 代价是每次查询起一个短命子进程（约 30–60ms），由后台每分钟采样一次，
// 对低内存机器完全可接受。
func (m *Manager) QueryStats(ctx context.Context) ([]StatEntry, error) {
	if !m.Available() {
		return nil, ErrNotAvailable
	}
	if !m.Running() {
		return nil, errors.New("xray 未运行")
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(m.apiPort))
	cmd := exec.CommandContext(c, m.binPath, "api", "statsquery", "--server="+addr, "-pattern=")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("读取统计失败: %v: %s", err, strings.TrimSpace(buf.String()))
	}

	raw := bytes.TrimSpace(buf.Bytes())
	start := bytes.IndexByte(raw, '{')
	if start < 0 {
		return nil, fmt.Errorf("统计输出不是 JSON: %s", truncate(string(raw), 200))
	}
	var resp struct {
		Stat []struct {
			Name  string  `json:"name"`
			Value flexInt `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(raw[start:], &resp); err != nil {
		return nil, fmt.Errorf("解析统计失败: %w", err)
	}

	out := make([]StatEntry, 0, len(resp.Stat))
	for _, s := range resp.Stat {
		out = append(out, StatEntry{Name: s.Name, Value: int64(s.Value)})
	}
	return out, nil
}

// ParseStatName 拆解 `type>>>tag>>>traffic>>>direction`。
func ParseStatName(name string) (kind, tag, direction string, ok bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[2] != "traffic" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[3], true
}

// ---------- ring buffer ----------

type ringBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{size: size}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.size {
		r.buf = r.buf[len(r.buf)-r.size:]
	}
	return len(p), nil
}

func (r *ringBuffer) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.buf
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
