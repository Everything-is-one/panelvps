// Command panelvps 是面板的唯一入口。
//
// 子命令：
//
//	panelvps serve            启动服务（默认）
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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/server"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/version"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet("panelvps "+cmd, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	username := fs.String("user", "admin", "管理员用户名")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `PanelVPS %s

用法:
  panelvps [命令] [参数]

命令:
  serve             启动服务（默认）
  version           打印版本信息
  info              打印配置与账号概况
  init              创建管理员账号（已存在则跳过）
  reset-password    重置管理员密码为随机值

参数:
`, version.Version)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "serve":
		err = runServe(*cfgPath)
	case "version":
		fmt.Printf("panelvps %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildAt)
	case "info":
		err = runInfo(*cfgPath)
	case "init":
		err = runInit(*cfgPath, *username)
	case "reset-password":
		err = runResetPassword(*cfgPath, *username)
	default:
		fs.Usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// defaultConfigPath 优先使用安装目录下的配置，其次当前目录。
func defaultConfigPath() string {
	const installed = "/opt/panelvps/conf/panel.yaml"
	if _, err := os.Stat(installed); err == nil {
		return installed
	}
	return "./conf/panel.yaml"
}

func newLogger(level string) *slog.Logger {
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
	// 日志走 stdout，交给 journald 收集（PRD 附录 ADR-08）
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
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
		return err
	}
	logger := newLogger(cfg.LogLevel)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
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

func runInfo(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("版本      : %s (%s)\n", version.Version, version.Commit)
	fmt.Printf("配置文件  : %s\n", cfgPath)
	fmt.Printf("监听地址  : %s%s\n", cfg.Listen, map[bool]string{true: "  [仅本机]", false: "  [已对外]"}[cfg.IsLoopback()])
	fmt.Printf("数据目录  : %s\n", cfg.DataDir)
	fmt.Printf("数据库    : %s\n", cfg.DBPath)
	fmt.Printf("会话超时  : %s\n", cfg.SessionTTL)
	fmt.Printf("文件根目录: %s\n", cfg.FileRoot)
	fmt.Printf("终端功能  : %v\n", cfg.TerminalEnabled)

	if _, err := os.Stat(cfg.DBPath); err == nil {
		st, err := store.Open(cfg.DBPath)
		if err != nil {
			return err
		}
		defer st.Close()
		ctx := context.Background()
		if n, err := st.CountUsers(ctx); err == nil {
			fmt.Printf("账号数量  : %d\n", n)
		}
		if n, err := st.AuditCount(ctx); err == nil {
			fmt.Printf("审计条数  : %d\n", n)
		}
		if fi, err := os.Stat(cfg.DBPath); err == nil {
			fmt.Printf("数据库大小: %.1f KB\n", float64(fi.Size())/1024)
		}
	} else {
		fmt.Printf("数据库    : 尚未初始化（首次 serve 时创建）\n")
	}
	return nil
}

func runInit(cfgPath, username string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

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

func runResetPassword(cfgPath, username string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	u, err := st.UserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("账号 %s 不存在，请先执行 panelvps init", username)
		}
		return err
	}

	pwd, err := auth.RandomPassword(20)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pwd)
	if err != nil {
		return err
	}
	if err := st.UpdatePassword(ctx, u.ID, hash); err != nil {
		return err
	}
	// 重置后强制改密，并踢掉所有在线会话
	_, _ = st.DB().ExecContext(ctx, `UPDATE users SET must_change_password = 1 WHERE id = ?`, u.ID)
	_ = st.DeleteUserSessions(ctx, u.ID)
	_ = st.Audit(ctx, "cli", "password_reset", "user", username, "local")

	fmt.Printf("已重置 %s 的密码: %s\n", username, pwd)
	fmt.Println("所有在线会话已失效，登录后必须修改密码")
	return nil
}
