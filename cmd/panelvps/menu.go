package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/auth"
	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/manage"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/tune"
	"github.com/Everything-is-one/panelvps/internal/version"
	"github.com/mattn/go-isatty"
)

// 管理菜单：把装完之后最常做的几件事做成数字选项，和 x-ui 的用法一致。
// 直接敲 `panelvps` 就进菜单（无 systemd 的容器里也能用，只是服务相关的项会提示不可用）。

type menu struct {
	r        *bufio.Reader
	cfgPath  string
	username string
}

func runMenu(cfgPath, username string) error {
	if _, err := config.Load(cfgPath); err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	m := &menu{r: bufio.NewReader(os.Stdin), cfgPath: cfgPath, username: username}

	for {
		if err := m.draw(); err != nil {
			return err
		}
		choice, err := m.ask("请选择")
		if err != nil {
			fmt.Println()
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "1":
			m.action(m.startPanel)
		case "2":
			m.action(m.stopPanel)
		case "3":
			m.action(m.restartPanel)
		case "4":
			m.action(m.showStatus)
		case "5":
			m.action(m.showLogs)
		case "6":
			m.action(m.setAccess)
		case "7":
			m.action(m.resetPassword)
		case "8":
			m.action(m.bbrMenu)
		case "9":
			m.action(m.showInfo)
		case "10":
			m.action(m.reboot)
		case "0", "q", "exit", "quit":
			fmt.Println("已退出")
			return nil
		case "":
			// 回车不做事，直接回到菜单
		default:
			m.note("无效选项，请输入 0–10")
		}
	}
}

// ---------- 界面 ----------

func (m *menu) draw() error {
	cfg, err := config.Load(m.cfgPath)
	if err != nil {
		return err
	}
	_, portStr, _ := splitListen(cfg.Listen)
	where := "仅本机"
	if !cfg.IsLoopback() {
		where = "公网可访问"
	}

	svc := "未检测到 systemd"
	switch {
	case !manage.HasSystemd():
	case manage.UnitActive():
		pid := manage.UnitPID()
		svc = "运行中"
		if pid > 0 {
			svc = fmt.Sprintf("运行中 (pid %d)", pid)
		}
		if manage.UnitEnabled() {
			svc += " · 开机自启"
		}
	default:
		svc = "未运行"
	}

	bbr := tune.BBR()
	bbrText := "不可用"
	switch {
	case bbr.Enabled:
		bbrText = fmt.Sprintf("已开启（%s / %s）", bbr.Current, bbr.Qdisc)
	case bbr.Supported:
		bbrText = "未开启（" + bbr.Current + "）"
	case bbr.Reason != "":
		bbrText = bbr.Reason
	}

	fmt.Printf("\nPanelVPS %s  管理菜单\n", version.Version)
	fmt.Println(strings.Repeat("─", 52))
	fmt.Printf(" 服务 : %s\n", svc)
	fmt.Printf(" 监听 : %s（%s）\n", cfg.Listen, where)
	fmt.Printf(" BBR  : %s\n", bbrText)
	if portStr == "" {
		fmt.Println(" 提示 : 监听地址里没有端口，请到第 6 项重新设置")
	}
	fmt.Println(strings.Repeat("─", 52))
	fmt.Println("  1) 启动面板")
	fmt.Println("  2) 停止面板")
	fmt.Println("  3) 重启面板")
	fmt.Println("  4) 查看运行状态")
	fmt.Println("  5) 查看最近日志")
	fmt.Println("  6) 监听地址与端口（IP:端口 外网访问）")
	fmt.Println("  7) 重置账号密码")
	fmt.Println("  8) BBR 加速开关")
	fmt.Println("  9) 显示面板信息")
	fmt.Println(" 10) 重启服务器")
	fmt.Println("  0) 退出")
	return nil
}

func (m *menu) action(fn func() error) {
	if err := fn(); err != nil {
		fmt.Println("错误:", err)
	}
	m.pause()
}

func (m *menu) ask(prompt string) (string, error) {
	fmt.Printf("%s: ", prompt)
	line, err := m.r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (m *menu) askDefault(prompt, def string) (string, error) {
	if def != "" {
		prompt = fmt.Sprintf("%s [%s]", prompt, def)
	}
	v, err := m.ask(prompt)
	if err != nil {
		return def, err
	}
	if v == "" {
		return def, nil
	}
	return v, nil
}

func (m *menu) confirm(q string) bool {
	v, err := m.ask(q + " [y/N]")
	if err != nil {
		return false
	}
	v = strings.ToLower(v)
	return v == "y" || v == "yes"
}

func (m *menu) pause() {
	fmt.Print("\n按回车返回菜单…")
	_, _ = m.r.ReadString('\n')
}

func (m *menu) note(format string, a ...any) {
	fmt.Printf("[%s]\n", fmt.Sprintf(format, a...))
}

func (m *menu) ok(format string, a ...any) {
	fmt.Printf("[ok] %s\n", fmt.Sprintf(format, a...))
}

func (m *menu) warn(format string, a ...any) {
	fmt.Printf("[注意] %s\n", fmt.Sprintf(format, a...))
}

// ---------- 服务 ----------

func (m *menu) startPanel() error {
	if err := manage.Start(); err != nil {
		return err
	}
	if m.waitActive(10 * time.Second) {
		m.ok("面板已启动")
	} else {
		m.warn("启动命令已执行，但服务 10 秒内没有变成运行中，请用第 5 项看日志")
	}
	return nil
}

func (m *menu) stopPanel() error {
	if !manage.HasSystemd() {
		return errors.New("未检测到 systemd；前台运行的面板请按 Ctrl-C 退出")
	}
	if !m.confirm("停止面板后网页会立即断开，继续？") {
		m.note("已取消")
		return nil
	}
	if err := manage.Stop(); err != nil {
		return err
	}
	m.ok("面板已停止（网站与代理不受影响）")
	return nil
}

func (m *menu) restartPanel() error {
	if !manage.HasSystemd() {
		return errors.New("未检测到 systemd；请手动重启前台进程")
	}
	if err := manage.Restart(); err != nil {
		return err
	}
	m.note("已提交重启，等待服务恢复…")
	if m.waitActive(15 * time.Second) {
		m.ok("面板已恢复")
	} else {
		m.warn("15 秒内没有恢复，请用第 5 项看日志")
	}
	return nil
}

// waitActive 轮询等待单元变成 active。
func (m *menu) waitActive(timeout time.Duration) bool {
	if !manage.HasSystemd() {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if manage.UnitActive() {
			return true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

func (m *menu) showStatus() error {
	if !manage.HasSystemd() {
		m.warn("未检测到 systemd，只能前台运行 `panelvps serve`")
		cfg, err := config.Load(m.cfgPath)
		if err != nil {
			return err
		}
		m.note("配置里的监听地址: %s", cfg.Listen)
		return nil
	}
	out, err := manage.Status()
	fmt.Println(out)
	if err != nil && out == "" {
		return err
	}
	return nil
}

func (m *menu) showLogs() error {
	out, err := manage.Logs(40)
	if out != "" {
		fmt.Println(out)
	}
	if err != nil {
		return err
	}
	return nil
}

// ---------- 访问方式 ----------

func (m *menu) setAccess() error {
	cfg, err := config.Load(m.cfgPath)
	if err != nil {
		return err
	}
	_, portStr, _ := splitListen(cfg.Listen)
	if portStr == "" {
		portStr = "20873"
	}
	current := "仅本机（需要 SSH 隧道或反向代理）"
	if !cfg.IsLoopback() {
		current = "公网（IP:端口 直连）"
	}
	fmt.Printf("当前监听: %s，%s\n", cfg.Listen, current)

	choice, err := m.askDefault(" 1) 仅本机   2) 公网直连\n选择", "1")
	if err != nil {
		return err
	}
	public := false
	switch choice {
	case "1":
	case "2":
		public = true
	default:
		return fmt.Errorf("选择只能是 1 或 2，收到 %q", choice)
	}

	portStr, err = m.askDefault("端口", portStr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("端口应在 1–65535 之间，收到 %q", portStr)
	}

	res, err := manage.SetAccess(m.cfgPath, port, public)
	if err != nil {
		return err
	}
	if !res.Changed {
		m.note("监听地址没有变化，仍是 %s", res.NewListen)
		return nil
	}
	m.ok("配置已更新: %s → %s", res.OldListen, res.NewListen)
	for _, u := range res.URLs {
		fmt.Println("     访问地址:", u)
	}
	if res.FirewallSynced {
		m.ok("防火墙规则已按新端口重新下发")
	}
	for _, w := range res.Warnings {
		m.warn("%s", w)
	}
	if public {
		m.warn("面板只有 HTTP。公网直连请用 16 位以上随机密码，并在云厂商安全组放行 %d/tcp", port)
	}

	if !manage.HasSystemd() {
		m.note("未检测到 systemd，请手动重启面板让新配置生效")
		return nil
	}
	if err := manage.Restart(); err != nil {
		return err
	}
	if m.waitActive(15 * time.Second) {
		m.ok("面板已按新地址重启")
	} else {
		m.warn("面板没有在 15 秒内恢复，请用第 5 项看日志")
	}
	return nil
}

// splitListen 从 "host:port" 里取端口，解析失败返回空。
func splitListen(listen string) (string, string, bool) {
	i := strings.LastIndex(listen, ":")
	if i < 0 || i == len(listen)-1 {
		return listen, "", false
	}
	return listen[:i], listen[i+1:], true
}

// ---------- 账号 ----------

func (m *menu) resetPassword() error {
	user, err := m.askDefault("账号", m.username)
	if err != nil {
		return err
	}
	fmt.Println(" 1) 随机 20 位（推荐）")
	fmt.Println(" 2) 自定义")
	mode, err := m.askDefault("选择", "1")
	if err != nil {
		return err
	}

	switch mode {
	case "1":
		pwd, err := resetPasswordRandom(m.cfgPath, user)
		if err != nil {
			return err
		}
		m.ok("已重置 %s 的密码: %s", user, pwd)
		fmt.Println("     只显示这一次，请立即保存")
		fmt.Println("     所有在线会话已失效，下次登录需修改密码")
	case "2":
		pwd, err := m.ask("新密码（输入会显示在屏幕上）")
		if err != nil {
			return err
		}
		again, err := m.ask("再输一次")
		if err != nil {
			return err
		}
		if pwd != again {
			return errors.New("两次输入不一致")
		}
		if err := resetPasswordCustom(m.cfgPath, user, pwd); err != nil {
			return err
		}
		m.ok("已修改 %s 的密码", user)
		fmt.Println("     所有在线会话已失效")
	default:
		return fmt.Errorf("选择只能是 1 或 2，收到 %q", mode)
	}
	return nil
}

func resetPasswordRandom(cfgPath, username string) (string, error) {
	st, u, err := openUser(cfgPath, username)
	if err != nil {
		return "", err
	}
	defer st.Close()

	pwd, err := auth.RandomPassword(20)
	if err != nil {
		return "", err
	}
	if err := savePassword(st, u, pwd, true); err != nil {
		return "", err
	}
	return pwd, nil
}

func resetPasswordCustom(cfgPath, username, pwd string) error {
	st, u, err := openUser(cfgPath, username)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := auth.ValidatePassword(pwd, username); err != nil {
		return err
	}
	return savePassword(st, u, pwd, false)
}

func openUser(cfgPath, username string) (*store.Store, *store.User, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("打开数据库失败（面板正在运行时也可能出现，稍后重试）: %w", err)
	}
	ctx := context.Background()
	u, err := st.UserByUsername(ctx, username)
	if err != nil {
		st.Close()
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("账号 %s 不存在，请用 `panelvps init` 创建", username)
		}
		return nil, nil, err
	}
	return st, u, nil
}

func savePassword(st *store.Store, u *store.User, pwd string, mustChange bool) error {
	hash, err := auth.HashPassword(pwd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := st.UpdatePassword(ctx, u.ID, hash); err != nil {
		return err
	}
	flag := 0
	if mustChange {
		flag = 1
	}
	_, _ = st.DB().ExecContext(ctx, `UPDATE users SET must_change_password = ? WHERE id = ?`, flag, u.ID)
	_ = st.DeleteUserSessions(ctx, u.ID)
	_ = st.Audit(ctx, "cli", "password_reset", "user", u.Username, "menu")
	return nil
}

// ---------- BBR ----------

func (m *menu) bbrMenu() error {
	v := tune.BBR()
	fmt.Printf("当前: 拥塞控制 %s · 队列 %s\n", orDash(v.Current), orDash(v.Qdisc))
	if len(v.Available) > 0 {
		fmt.Printf("可用: %s\n", strings.Join(v.Available, " "))
	}
	if v.Reason != "" {
		m.note("%s", v.Reason)
	}
	fmt.Println(" 1) 开启 BBR")
	fmt.Println(" 2) 关闭 BBR（切回 cubic）")
	fmt.Println(" 0) 返回")

	choice, err := m.askDefault("选择", "0")
	if err != nil {
		return err
	}
	var enable bool
	switch choice {
	case "0":
		return nil
	case "1":
		enable = true
	case "2":
		enable = false
	default:
		return fmt.Errorf("选择只能是 0、1 或 2，收到 %q", choice)
	}
	if enable && !v.Supported {
		return errors.New("当前内核不支持 BBR：" + v.Reason)
	}

	after, err := tune.SetBBR(enable)
	if err != nil {
		return err
	}
	if enable {
		m.ok("BBR 已开启（%s / %s），已写入 %s", after.Current, after.Qdisc, after.ConfigPath)
		fmt.Println("     重启后依然生效")
	} else {
		m.ok("BBR 已关闭，当前拥塞控制 %s", after.Current)
	}
	return nil
}

// ---------- 信息与重启 ----------

func (m *menu) showInfo() error {
	cfg, err := config.Load(m.cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("版本      : %s (%s)\n", version.Version, version.Commit)
	fmt.Printf("配置文件  : %s\n", m.cfgPath)
	fmt.Printf("监听地址  : %s (%s)\n", cfg.Listen, map[bool]string{true: "仅本机", false: "公网"}[cfg.IsLoopback()])
	fmt.Printf("数据目录  : %s\n", cfg.DataDir)
	fmt.Printf("数据库    : %s\n", cfg.DBPath)
	fmt.Printf("会话超时  : %s\n", cfg.SessionTTL)
	fmt.Printf("文件根目录: %s\n", cfg.FileRoot)

	_, portStr, _ := splitListen(cfg.Listen)
	if port, err := strconv.Atoi(portStr); err == nil {
		for _, u := range manage.AccessURLs(manage.AccessHosts(), port, !cfg.IsLoopback()) {
			fmt.Println("访问地址  :", u)
		}
	}

	if st, err := store.Open(cfg.DBPath); err == nil {
		defer st.Close()
		ctx := context.Background()
		if n, err := st.CountUsers(ctx); err == nil {
			fmt.Printf("账号数量  : %d\n", n)
		}
		if n, err := st.AuditCount(ctx); err == nil {
			fmt.Printf("审计条数  : %d\n", n)
		}
	}
	return nil
}

func (m *menu) reboot() error {
	if !manage.HasSystemd() {
		return errors.New("未检测到 systemd，无法重启服务器")
	}
	m.warn("会中断所有站点、代理与当前 SSH 连接之外的服务")
	word, err := m.ask("确认重启服务器请输入 REBOOT")
	if err != nil {
		return err
	}
	if word != "REBOOT" {
		m.note("已取消")
		return nil
	}
	if err := manage.Reboot(); err != nil {
		return err
	}
	m.ok("重启指令已下发，连接即将中断")
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// isTerminal 判断标准输入是不是终端。
//
// 不能用 ModeCharDevice：/dev/null 也是字符设备，`panelvps </dev/null`
// 会被误判成交互式，systemd 里就卡在菜单上了。
func isTerminal(f *os.File) bool {
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
