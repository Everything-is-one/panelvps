// Command panelvps 是面板的唯一入口。
//
// 子命令：
//
//	panelvps                  进入管理菜单（交互式，等同 panelvps menu）
//	panelvps menu             进入管理菜单
//	panelvps serve            前台启动服务
//	panelvps version          打印版本
//	panelvps info             打印运行时路径与账号概况
//	panelvps init             创建管理员账号
//	panelvps reset-password   重置管理员密码为随机值
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/relay"
	"github.com/Everything-is-one/panelvps/internal/server"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/version"
)

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	if cmd == "" {
		// 有终端就进管理菜单；被 systemd 或管道调用时仍按老行为前台启动，
		// 避免 `panelvps < /dev/null` 这类用法突然停在那儿等输入。
		if isTerminal(os.Stdin) {
			cmd = "menu"
		} else {
			cmd = "serve"
		}
	}

	// 中转模式自己带一套参数，走独立分支：它不需要配置文件、不需要数据库，
	// 在一台只当跳板的机器上直接跑起来即可。
	if cmd == "relay" {
		runRelayCmd(args)
		return
	}

	fs := flag.NewFlagSet("panelvps "+cmd, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	username := fs.String("user", "admin", "管理员用户名")
	dbOverride := fs.String("db", "", "直接指定数据库文件，跳过配置文件里的 data_dir 推导（救急用）")
	newPassword := fs.String("password", "", "指定新密码，留空则随机生成（仅 reset-password 使用）")
	fixConfig := fs.Bool("fix", false, "尝试修复已知的配置损坏，改动前备份为 .bak（仅 check 使用）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `PanelVPS %s

用法:
  panelvps [命令] [参数]

命令:
  menu              管理菜单：启停、端口、重置密码、BBR、重启（默认）
  serve             前台启动服务
  version           打印版本信息
  info              打印配置、运行中的服务与数据库体检
  check             只检查配置能否解析，不改任何文件（安装脚本启动前用）
  init              创建管理员账号（已存在则跳过）
  reset-password    重置管理员密码为随机值
  relay             前台运行一条端口转发（不需要配置文件与数据库）
                    panelvps relay -listen :8443 -to 1.2.3.4:443 [-udp]

忘记密码时先看 info：它会把「服务实际打开的数据库」和配置解析出的
数据库都列出来。两者不一致时，用 -db 指向服务在用的那个。

不带命令直接运行 panelvps 会打开管理菜单；没有终端时按 serve 处理。

参数:
`, version.Version)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "menu":
		err = runMenu(*cfgPath, *username)
	case "serve":
		err = runServe(*cfgPath)
	case "version":
		fmt.Printf("panelvps %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildAt)
	case "info":
		err = runInfo(*cfgPath, *dbOverride)
	case "check":
		err = runCheck(*cfgPath, *fixConfig)
	case "init":
		err = runInit(*cfgPath, *username, *dbOverride)
	case "reset-password":
		err = runResetPassword(*cfgPath, *username, *dbOverride, *newPassword)
	default:
		fs.Usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// runRelayCmd 是独立的端口转发模式。
//
// 存在的意义：中转机上可能已经跑着宝塔、nginx 或别的东西，为了转一条 TCP
// 专门装一套面板并不划算。这个子命令不读配置、不建库，一条命令就能起转发。
func runRelayCmd(args []string) {
	fs := flag.NewFlagSet("panelvps relay", flag.ExitOnError)
	listen := fs.String("listen", ":8443", "本地监听地址，例如 :8443 或 0.0.0.0:8443")
	to := fs.String("to", "", "目标地址 host:port，例如 1.2.3.4:443")
	udp := fs.Bool("udp", false, "同时转发 UDP（默认只转 TCP）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `panelvps relay —— 独立端口转发

用法:
  panelvps relay -listen :8443 -to 1.2.3.4:443 [-udp]

参数:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	target := strings.TrimSpace(*to)
	if target == "" {
		fs.Usage()
		os.Exit(2)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		fmt.Fprintln(os.Stderr, "错误: -to 要写成 主机:端口，例如 1.2.3.4:443")
		os.Exit(2)
	}

	host, portStr, err := net.SplitHostPort(strings.TrimSpace(*listen))
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: -listen 不合法: %v\n", err)
		os.Exit(2)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "错误: -listen 的端口必须在 1–65535 之间")
		os.Exit(2)
	}

	network := "tcp"
	if *udp {
		network = "both"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m := relay.New(logger)
	m.Apply([]relay.Rule{{
		ID: 1, Name: "relay", Listen: host, ListenPort: port,
		Target: target, Network: network, Enabled: true,
	}})

	st := m.Snapshot()[1]
	if !st.Running {
		fmt.Fprintln(os.Stderr, "错误: 监听失败:", st.LastError)
		os.Exit(1)
	}
	fmt.Printf("中转已启动: %s -> %s (%s)\n按 Ctrl-C 停止。\n", net.JoinHostPort(host, portStr), target, network)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	m.StopAll()
	fmt.Println("已停止。")
}

// defaultConfigPath 优先使用安装目录下的配置，其次当前目录。
func defaultConfigPath() string {
	const installed = "/opt/panelvps/conf/panel.yaml"
	if _, err := os.Stat(installed); err == nil {
		return installed
	}
	return "./conf/panel.yaml"
}

func newLogger(level, dataDir string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	// 主通道仍是 stdout，交给 journald 收集（PRD 附录 ADR-08）。
	// 同时写一份 <data_dir>/panel.log：容器里服务沙箱常读不到自己的
	// journal 单元，日志页需要一份不依赖 journald 的副本。
	out := io.Writer(os.Stdout)
	if lw, err := newRotatingWriter(filepath.Join(dataDir, "panel.log"), 5<<20); err == nil {
		out = io.MultiWriter(os.Stdout, lw)
	}
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: lv}))
}

// tuneMemory 把 Go 的内存上限压到 64MiB。
//
// 这是「面板空载 ≤ 50MB」这条硬指标在运行时侧的保险：
// GOMEMLIMIT 是软限制，触发后 GC 更积极，而不是直接 OOM。
func tuneMemory() {
	debug.SetMemoryLimit(64 << 20)
	debug.SetGCPercent(50)
}

func runServe(cfgPath string) error {
	tuneMemory()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		// 带上路径：systemd 下日志只剩这一行，没有路径很难判断是哪个文件
		return fmt.Errorf("读取配置 %s 失败: %w", cfgPath, err)
	}
	logger := newLogger(cfg.LogLevel, cfg.DataDir)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("打开数据库 %s 失败: %w（检查磁盘空间与目录权限）", cfg.DBPath, err)
	}
	defer st.Close()

	ctx := context.Background()
	n, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("读取账号失败: %w", err)
	}
	if n == 0 {
		pwd, err := createAdmin(ctx, st, "admin")
		if err != nil {
			return err
		}
		printInitialPassword(logger, "admin", pwd)
	}

	srv, err := server.New(cfg, st, logger)
	if err != nil {
		return err
	}

	// 恢复代理：配置存在则直接接管 Xray，不存在则生成一份空配置
	srv.StartXray(context.Background())

	// 后台维护任务：清理过期会话与限流表
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			srv.Cleanup(context.Background())
		}
	}()

	// 流量采样：一分钟一次，写入增量并做配额检查
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			if err := srv.CollectTraffic(context.Background()); err != nil {
				logger.Debug("采集流量失败", "err", err)
			}
		}
	}()

	// SSH 爆破巡检：一分钟一次，达到阈值自动封禁
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			srv.SSHGuardTick(context.Background())
			// iptables 后端没有「到期自动解除」，靠这里清理；
			// nft 由内核按元素 timeout 自己过期，这一步是空转。
			if n, err := firewall.SweepExpiredBans(); err == nil && n > 0 {
				logger.Info("清理到期的封禁规则", "count", n)
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	if !cfg.IsLoopback() {
		logger.Warn("面板监听在非回环地址，已对公网开放",
			"listen", cfg.Listen,
			"建议", "确认已开启 2FA，或用防火墙限制来源 IP")
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("面板已启动",
			"listen", cfg.Listen,
			"db", cfg.DBPath,
			"version", version.Version,
			"loopback", cfg.IsLoopback())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("监听失败: %w", err)
		}
		return nil
	case sig := <-sigCh:
		logger.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("关闭失败: %w", err)
	}
	srv.StopXray()
	logger.Info("已安全退出")
	return nil
}

func createAdmin(ctx context.Context, st *store.Store, username string) (string, error) {
	pwd, err := auth.RandomPassword(20)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(pwd)
	if err != nil {
		return "", err
	}
	if _, err := st.CreateUser(ctx, username, hash, true); err != nil {
		return "", fmt.Errorf("创建管理员失败: %w", err)
	}
	_ = st.Audit(ctx, username, "admin_created", "user", "首次启动自动创建", "system")
	return pwd, nil
}

// printInitialPassword 把初始密码打到 stdout。journalctl 能看到，
// 但不落盘——落盘等于把钥匙和锁放在一起。
func printInitialPassword(logger *slog.Logger, username, password string) {
	logger.Warn("已生成初始管理员账号，请登录后立即修改密码",
		"username", username, "password", password)
	fmt.Printf("\n========================================\n")
	fmt.Printf("  首次启动，已创建管理员账号\n")
	fmt.Printf("  用户名: %s\n", username)
	fmt.Printf("  密  码: %s\n", password)
	fmt.Printf("  登录后必须修改密码\n")
	fmt.Printf("========================================\n\n")
}

// openStoreFor 打开 CLI 要操作的数据库，同时返回实际使用的路径。
//
// dbOverride 非空时直连那个文件，连配置文件都不要求存在——「密码登不上、
// 配置也读不出个所以然」正是它存在的场景。此时 DataDir 之类只用来展示，
// 不能反过来影响库的选择。
//
// 返回的 path 是**真正被打开**的文件，打印时一律用它：用 cfg.DBPath 会出现
// 「说改的是 A、实际改的是 B」这种更让人头大的输出。
func openStoreFor(cfgPath, dbOverride string) (*store.Store, *config.Config, string, error) {
	if dbOverride != "" {
		abs, err := filepath.Abs(dbOverride)
		if err != nil {
			return nil, nil, "", fmt.Errorf("解析 -db 路径失败: %w", err)
		}
		if fi, err := os.Stat(abs); err != nil {
			return nil, nil, "", fmt.Errorf("数据库 %s 不存在: %w", abs, err)
		} else if fi.IsDir() {
			return nil, nil, "", fmt.Errorf("数据库 %s 是目录，应该是 .db 文件", abs)
		}
		cfg, _ := config.Load(cfgPath) // 读不到也无妨，只用于展示
		st, err := store.Open(abs)
		if err != nil {
			return nil, nil, "", fmt.Errorf("打开数据库 %s 失败: %w", abs, err)
		}
		return st, cfg, abs, nil
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, "", err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("打开数据库 %s 失败（面板正在运行时也可能出现，稍后重试）: %w", cfg.DBPath, err)
	}
	return st, cfg, cfg.DBPath, nil
}

// confirmPassword 写完立刻回读校验。
//
// 存在的意义：密码改动有几个「静默成功」的失败模式——写到影子库、库是只读
// 副本、事务没提交。它们都不会报错，只会让人反复试密码。回读一次再拿明文
// 走一遍完整的 argon2 校验，通过才算真的改成功。
func confirmPassword(st *store.Store, userID int64, pwd string) error {
	ctx := context.Background()
	u, err := st.UserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("回读账号失败: %w", err)
	}
	ok, err := auth.VerifyPassword(pwd, u.PasswordHash)
	if err != nil {
		return fmt.Errorf("回读的密码哈希无法校验: %w", err)
	}
	if !ok {
		return errors.New("写入后校验没通过：数据库里的哈希与新密码不匹配")
	}
	return nil
}

func runInit(cfgPath, username, dbOverride string) error {
	st, _, dbPath, err := openStoreFor(cfgPath, dbOverride)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("数据库    : %s\n", dbPath)

	ctx := context.Background()
	if _, err := st.UserByUsername(ctx, username); err == nil {
		fmt.Printf("账号 %s 已存在，未做修改\n", username)
		return nil
	}
	pwd, err := createAdmin(ctx, st, username)
	if err != nil {
		return err
	}
	fmt.Printf("已创建账号 %s，初始密码: %s\n", username, pwd)
	fmt.Println("登录后必须修改密码")
	return nil
}

func runResetPassword(cfgPath, username, dbOverride, explicit string) error {
	st, _, dbPath, err := openStoreFor(cfgPath, dbOverride)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("数据库    : %s\n", dbPath)

	ctx := context.Background()
	u, err := st.UserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("这个库里没有账号 %s。先用 panelvps info 确认服务在用的是哪个库，"+
				"再执行 panelvps reset-password -db <那个库> -user %s", username, username)
		}
		return err
	}

	pwd := explicit
	mustChange := false
	if pwd == "" {
		pwd, err = auth.RandomPassword(20)
		if err != nil {
			return err
		}
		mustChange = true
	} else if err := auth.ValidatePassword(pwd, username); err != nil {
		return err
	}

	hash, err := auth.HashPassword(pwd)
	if err != nil {
		return err
	}
	if err := st.UpdatePassword(ctx, u.ID, hash); err != nil {
		return err
	}
	// 重置后强制改密（随机密码时），并踢掉所有在线会话
	flag := 0
	if mustChange {
		flag = 1
	}
	_, _ = st.DB().ExecContext(ctx, `UPDATE users SET must_change_password = ? WHERE id = ?`, flag, u.ID)
	_ = st.DeleteUserSessions(ctx, u.ID)

	if err := confirmPassword(st, u.ID, pwd); err != nil {
		return err
	}
	_ = st.Audit(ctx, "cli", "password_reset", "user", username, "local")

	fmt.Printf("已重置 %s 的密码: %s\n", username, pwd)
	if mustChange {
		fmt.Println("登录后必须修改密码")
	}
	fmt.Println("所有在线会话已失效，可以立即用上面的密码登录")
	fmt.Printf("（改动已回读校验，落在这个库里：%s）\n", dbPath)
	return nil
}
