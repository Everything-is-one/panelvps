// Package manage 提供面板之外的管理动作：服务启停、监听地址调整、地址枚举。
//
// 网页控制台与命令行菜单都走这里，避免两套实现各写一遍。
package manage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
)

// UnitName 是面板的 systemd 单元名。
const UnitName = "panelvps"

// ---------- 服务 ----------

// HasSystemd 判断当前环境能否用 systemctl 管理面板。
func HasSystemd() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// 调用 systemctl。返回合并后的输出，便于直接把报错打给用户。
func systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// UnitActive 报告单元是否处于 active。
func UnitActive() bool {
	out, err := systemctl("is-active", UnitName)
	return err == nil && out == "active"
}

// UnitEnabled 报告是否开机自启。
func UnitEnabled() bool {
	out, err := systemctl("is-enabled", UnitName)
	return err == nil && out == "enabled"
}

// UnitPID 取主进程号，取不到返回 0。
func UnitPID() int {
	out, err := systemctl("show", "-p", "MainPID", "--value", UnitName)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// Start 启动面板。
func Start() error {
	if !HasSystemd() {
		return errors.New("未检测到 systemd，请直接前台运行：panelvps serve")
	}
	if out, err := systemctl("start", UnitName); err != nil {
		return fmt.Errorf("启动失败: %s", firstLine(out, err.Error()))
	}
	return nil
}

// Stop 停止面板。
func Stop() error {
	if !HasSystemd() {
		return errors.New("未检测到 systemd，无法停止；前台运行时按 Ctrl-C")
	}
	if out, err := systemctl("stop", UnitName); err != nil {
		return fmt.Errorf("停止失败: %s", firstLine(out, err.Error()))
	}
	return nil
}

// Restart 重启面板。用 --no-block 是为了让命令本身尽快返回，
// 否则在面板进程里调用会等到自己被 SIGTERM 杀掉。
func Restart() error {
	if !HasSystemd() {
		return errors.New("未检测到 systemd，请手动重启：panelvps serve")
	}
	if out, err := systemctl("--no-block", "restart", UnitName); err != nil {
		return fmt.Errorf("重启失败: %s", firstLine(out, err.Error()))
	}
	return nil
}

// Reboot 重启整台服务器。
func Reboot() error {
	if !HasSystemd() {
		return errors.New("未检测到 systemd，无法重启服务器")
	}
	if out, err := systemctl("--no-block", "reboot"); err != nil {
		return fmt.Errorf("重启服务器失败: %s", firstLine(out, err.Error()))
	}
	return nil
}

// Status 返回 systemctl 的简要状态，供菜单显示。
func Status() (string, error) {
	if !HasSystemd() {
		return "", errors.New("未检测到 systemd")
	}
	out, err := systemctl("status", UnitName, "--no-pager", "-n", "8")
	return out, err
}

// Logs 返回面板最近的日志（journald）。
func Logs(lines int) (string, error) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return "", errors.New("未检测到 journalctl，无法读取日志")
	}
	out, err := exec.Command("journalctl", "-u", UnitName, "-n", strconv.Itoa(lines), "--no-pager").CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("读取日志失败: %s", firstLine(string(out), err.Error()))
	}
	return string(out), nil
}

// ---------- 地址 ----------

// LocalIPv4s 枚举本机可用于访问的 IPv4（跳过回环与链路本地）。
func LocalIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	set := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			set[ip.String()] = true
		}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// AccessURLs 生成可点击的访问地址。仅本机时只给 127.0.0.1。
func AccessURLs(hosts []string, port int, public bool) []string {
	if !public {
		return []string{fmt.Sprintf("http://127.0.0.1:%d", port)}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, fmt.Sprintf("http://%s:%d", h, port))
	}
	return out
}

// AccessHosts 是没有 HTTP 请求可参考时的地址列表：本机网卡地址。
func AccessHosts() []string {
	return LocalIPv4s()
}

// ---------- 监听地址 ----------

// AccessResult 描述一次监听地址变更的结果。
type AccessResult struct {
	Changed        bool     `json:"changed"`
	OldListen      string   `json:"old_listen"`
	NewListen      string   `json:"new_listen"`
	Port           int      `json:"port"`
	Public         bool     `json:"public"`
	URLs           []string `json:"urls"`
	FirewallSynced bool     `json:"firewall_synced"`
	Warnings       []string `json:"warnings"`
}

// SetAccess 切换监听地址/端口。
//
// 顺序是「试绑 → 写配置 → 按新端口重下防火墙」，防火墙失败会回滚配置：
// 否则重启后就会出现「新端口没放行、旧端口已关」，把自己锁在门外。
func SetAccess(cfgPath string, port int, public bool) (AccessResult, error) {
	res := AccessResult{Port: port, Public: public}
	if port < 1 || port > 65535 {
		return res, errors.New("端口应在 1–65535 之间")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return res, err
	}
	res.OldListen = cfg.Listen

	host := "127.0.0.1"
	if public {
		host = "0.0.0.0"
	}
	newListen := net.JoinHostPort(host, strconv.Itoa(port))
	res.NewListen = newListen
	res.URLs = AccessURLs(AccessHosts(), port, public)

	if newListen == cfg.Listen {
		res.Changed = false
		return res, nil
	}

	_, oldPortStr, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return res, fmt.Errorf("当前监听地址不合法: %s", cfg.Listen)
	}
	oldPort, _ := strconv.Atoi(oldPortStr)

	// 换端口时先试绑一次，避免写进配置才发现端口被占用。
	if port != oldPort {
		ln, err := net.Listen("tcp", newListen)
		if err != nil {
			return res, fmt.Errorf("端口 %d 不可用：%v", port, err)
		}
		_ = ln.Close()
	}

	oldListen := cfg.Listen
	cfg.Listen = newListen
	if err := cfg.Save(cfgPath); err != nil {
		return res, fmt.Errorf("保存配置失败: %w", err)
	}
	res.Changed = true

	if port < 1024 {
		res.Warnings = append(res.Warnings, "端口低于 1024，需要 root 才能绑定")
	}

	// 已下发过防火墙规则时，必须按新端口重下一遍。
	if synced, warns, err := syncFirewall(cfg, port, public); err != nil {
		cfg.Listen = oldListen
		_ = cfg.Save(cfgPath)
		res.Changed = false
		return res, fmt.Errorf("防火墙规则下发失败，已取消修改: %w", err)
	} else {
		res.FirewallSynced = synced
		res.Warnings = append(res.Warnings, warns...)
	}

	return res, nil
}

// syncFirewall 按新的面板端口重下规则。没配置过防火墙时什么都不做。
func syncFirewall(cfg *config.Config, port int, public bool) (bool, []string, error) {
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return false, nil, nil
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return false, nil, nil
	}
	defer st.Close()

	rows, err := st.ListFirewallRules(context.Background())
	if err != nil {
		return false, nil, nil
	}
	if len(rows) == 0 && !fileExists(firewall.PersistPath()) {
		return false, nil, nil
	}
	fw := firewall.Detect()
	if !fw.Supported {
		return false, []string{"防火墙后端当前不可用，规则未同步；请确认新端口在云安全组中已放行"}, nil
	}
	opt := firewall.ApplyOptions{
		Rules:         toRules(rows),
		SSHPorts:      effectiveSSHPorts(cfg),
		PanelPort:     port,
		PanelLoopback: !public,
	}
	if _, err := firewall.Apply(opt); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

func toRules(items []store.FirewallRule) []firewall.Rule {
	out := make([]firewall.Rule, 0, len(items))
	for _, r := range items {
		out = append(out, firewall.Rule{
			ID: r.ID, Action: r.Action, Proto: r.Proto, Port: r.Port,
			Source: r.Source, Comment: r.Comment, Enabled: r.Enabled,
		})
	}
	return out
}

// effectiveSSHPorts 合并配置与 sshd_config，宁可多放行也不要锁死自己。
func effectiveSSHPorts(cfg *config.Config) []int {
	set := map[int]bool{}
	for _, p := range cfg.SSHPorts {
		set[p] = true
	}
	for _, p := range sysinfo.FindSSHPorts() {
		set[p] = true
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstLine(out, fallback string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return fallback
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return strings.TrimSpace(out)
}
