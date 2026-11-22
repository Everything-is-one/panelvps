package manage

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/firewall"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
)

// SSH 端口修改。
//
// 为什么要在面板里做：默认 22 端口一天能被扫几千次（日志里全是
// 「尝试用户 root/admin/oracle」），换成高位端口后噪声立刻掉一个数量级。
// 但改 sshd_config 是「改错就把自己锁在门外」的操作，所以这里把顺序
// 固定成：备份 → 改 → sshd -t 校验 → 放行防火墙 → reload，
// 任何一步失败都回滚，且永远只 reload 不 restart（restart 会掐断当前会话）。

const (
	sshMainConfig = "/etc/ssh/sshd_config"
	sshDropInDir  = "/etc/ssh/sshd_config.d"
)

// SSHPortResult 描述一次端口变更的结果。
type SSHPortResult struct {
	Changed     bool     `json:"changed"`
	OldPorts    []int    `json:"old_ports"`
	NewPort     int      `json:"new_port"`
	ConfigPath  string   `json:"config_path"`
	Backup      string   `json:"backup,omitempty"`
	Reloaded    bool     `json:"reloaded"`
	Service     string   `json:"service,omitempty"`
	Firewall    bool     `json:"firewall_synced"`
	ConfigSaved bool     `json:"config_saved"`
	Warnings    []string `json:"warnings"`
}

// CurrentSSHPorts 返回 sshd 实际生效的端口。
//
// 先问 sshd 自己（-T 会把 Include 进来的 drop-in 一起展开），
// 拿不到再退回解析主配置——容器里没装 sshd 时前者会失败。
func CurrentSSHPorts() []int {
	if out, err := exec.Command("sshd", "-T").Output(); err == nil {
		var ports []int
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "port" {
				if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 && n < 65536 {
					ports = append(ports, n)
				}
			}
		}
		if len(ports) > 0 {
			sort.Ints(ports)
			return ports
		}
	}
	return sysinfo.FindSSHPorts()
}

// sshConfigFiles 列出所有可能写着 Port 的文件：主配置 + drop-in。
//
// Debian 11+ 把 Include 放在主配置第一行，用户在 drop-in 里写的 Port
// 优先级高于主配置；只改主配置等于没改。
func sshConfigFiles() []string {
	files := []string{sshMainConfig}
	if items, err := filepath.Glob(filepath.Join(sshDropInDir, "*.conf")); err == nil {
		sort.Strings(items)
		files = append(files, items...)
	}
	return files
}

// SetSSHPort 把 SSH 端口改成 port。cfgPath 用于同步登记到面板配置。
func SetSSHPort(cfgPath string, port int) (SSHPortResult, error) {
	res := SSHPortResult{NewPort: port, ConfigPath: sshMainConfig}
	if port < 1 || port > 65535 {
		return res, fmt.Errorf("端口应在 1–65535 之间，收到 %d", port)
	}
	if _, err := os.Stat(sshMainConfig); err != nil {
		return res, fmt.Errorf("找不到 %s，这台机器可能没有装 OpenSSH", sshMainConfig)
	}

	res.OldPorts = CurrentSSHPorts()
	if len(res.OldPorts) == 1 && res.OldPorts[0] == port {
		return res, nil // 没有变化
	}
	if port < 1024 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d 是特权端口，占用它的服务需要 root；建议用 1024 以上", port))
	}
	if inUse(port) {
		return res, fmt.Errorf("端口 %d 已被其他进程占用，换一个", port)
	}

	// 1) 备份所有要改的文件
	backups := map[string]string{}
	for _, f := range sshConfigFiles() {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		bak := f + ".pv-bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(bak, raw, 0o600); err != nil {
			return res, fmt.Errorf("备份 %s 失败: %w", f, err)
		}
		backups[f] = bak
		if res.Backup == "" {
			res.Backup = bak
		}
	}

	rollback := func() {
		for f, bak := range backups {
			if raw, err := os.ReadFile(bak); err == nil {
				_ = writePreservingMode(f, raw)
			}
		}
	}

	// 2) 注释掉所有已有的 Port 行（sshd 对 Port 是累加的，留着旧值就会同时监听两个端口）
	for f := range backups {
		if err := commentPortLines(f); err != nil {
			rollback()
			return res, fmt.Errorf("改写 %s 失败: %w", f, err)
		}
	}

	// 3) 在主配置最前面写入新端口：即使某个 drop-in 又被别人加回 Port，
	//    主配置这一行也排在最前，sshd 取第一个值。
	if err := prependPort(sshMainConfig, port); err != nil {
		rollback()
		return res, fmt.Errorf("写入 %s 失败: %w", sshMainConfig, err)
	}

	// 4) 语法校验：sshd -t 会连 host key 一起检查，过不了就回滚
	if out, err := exec.Command("sshd", "-t").CombinedOutput(); err != nil {
		rollback()
		return res, fmt.Errorf("sshd 配置校验失败，已回滚: %s", strings.TrimSpace(string(out)))
	}

	// 5) SELinux：CentOS/RHEL 上不改标签，sshd 起不来
	if semanage, err := exec.LookPath("semanage"); err == nil {
		if out, err := exec.Command(semanage, "port", "-a", "-t", "ssh_port_t", "-p", "tcp",
			strconv.Itoa(port)).CombinedOutput(); err != nil {
			// 已经加过会报 "already defined"，不当失败
			if !strings.Contains(string(out), "already defined") {
				res.Warnings = append(res.Warnings,
					"SELinux 端口标签没加上，若重启后 sshd 起不来请手动执行："+
						"semanage port -a -t ssh_port_t -p tcp "+strconv.Itoa(port))
			}
		}
	}

	// 6) 先放行防火墙，再 reload —— 顺序反了就有几秒钟连不上
	if ok, warns := openSSHFirewall(cfgPath, port); ok {
		res.Firewall = true
		res.Warnings = append(res.Warnings, warns...)
	} else {
		res.Warnings = append(res.Warnings, warns...)
	}

	// 7) reload（不是 restart）：已建立的连接不受影响
	if svc, err := reloadSSH(); err != nil {
		res.Warnings = append(res.Warnings,
			"配置已改好但重载失败："+err.Error()+
				"；请在当前会话里手动执行 systemctl reload ssh（Debian）或 sshd（RHEL）")
	} else {
		res.Reloaded = true
		res.Service = svc
	}

	// 8) 登记到面板配置，防火墙模块据此永久放行新端口
	if cfgPath != "" {
		if err := config.PatchScalar(cfgPath, "ssh_ports", fmt.Sprintf("[%d]", port)); err == nil {
			res.ConfigSaved = true
		}
	}

	res.Changed = true
	res.Warnings = append(res.Warnings,
		fmt.Sprintf("云厂商安全组/防火墙里也要放行 %d/tcp，否则外网仍然连不上", port),
		"先别关当前这个 SSH 窗口：另开一个终端用新端口登录成功后再关",
	)
	return res, nil
}

// inUse 判断端口是否已被监听。sshd 自己占用的端口不算冲突。
func inUse(port int) bool {
	if ports, err := sysinfo.ListeningPorts(); err == nil {
		for _, p := range ports {
			if p.Port == port {
				return true
			}
		}
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// writePreservingMode 保留原文件权限写入。
// sshd_config 可能是 600，改端口这种「顺手写一下」不能把权限放宽成 644。
func writePreservingMode(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(path, data, mode)
}

// commentPortLines 把文件里所有生效的 Port 行注释掉，保留值便于人工回看。
func commentPortLines(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Port") {
			lines[i] = "# panelvps: " + trimmed
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writePreservingMode(path, []byte(strings.Join(lines, "\n")))
}

// prependPort 在文件开头写入 Port 行。
func prependPort(path string, port int) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("# panelvps: SSH 端口（由面板管理菜单写入）\nPort %d\n", port)
	body := append([]byte(head), bytes.TrimLeft(raw, "\n")...)
	return writePreservingMode(path, body)
}

// reloadSSH 重载 sshd。Debian 叫 ssh.service，RHEL 叫 sshd.service。
func reloadSSH() (string, error) {
	var lastErr error
	for _, svc := range []string{"ssh", "sshd"} {
		if out, err := exec.Command("systemctl", "reload", svc).CombinedOutput(); err == nil {
			return svc, nil
		} else {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			lastErr = fmt.Errorf("%s: %s", svc, firstLine(msg, msg))
		}
	}
	// 没有 systemd 的老环境：直接给 sshd 发 HUP
	if out, err := exec.Command("pkill", "-HUP", "-x", "sshd").CombinedOutput(); err == nil {
		return "sshd (SIGHUP)", nil
	} else if lastErr == nil {
		lastErr = fmt.Errorf("%s", firstLine(string(out), err.Error()))
	}
	return "", lastErr
}

// openSSHFirewall 放行新端口：ufw 与面板自己的规则链各处理一遍。
func openSSHFirewall(cfgPath string, port int) (bool, []string) {
	var warns []string
	synced := false

	// ufw：很多 VPS 用它，面板不该假设只有自己的 nft 表
	if path, err := exec.LookPath("ufw"); err == nil {
		if out, err := exec.Command(path, "status").Output(); err == nil &&
			strings.Contains(string(out), "Status: active") {
			if out, err := exec.Command(path, "allow", fmt.Sprintf("%d/tcp", port)).CombinedOutput(); err != nil {
				warns = append(warns, "ufw 放行失败："+firstLine(string(out), err.Error()))
			} else {
				synced = true
				warns = append(warns, "已在 ufw 放行 "+strconv.Itoa(port)+"/tcp（旧端口规则保留，可自行 ufw delete）")
			}
		}
	}

	// 面板规则链：只有用户真的下发过规则时才重建，否则会把 22 一起放开
	if cfgPath != "" {
		cfg, err := config.Load(cfgPath)
		if err == nil {
			ok, w, err := syncFirewallSSH(cfg, port)
			if err != nil {
				warns = append(warns, "面板防火墙规则更新失败："+err.Error())
			} else if ok {
				synced = true
			}
			warns = append(warns, w...)
		}
	}
	return synced, warns
}

// syncFirewallSSH 按新的 SSH 端口重下面板规则。
func syncFirewallSSH(cfg *config.Config, newPort int) (bool, []string, error) {
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
		return false, []string{"防火墙后端当前不可用，新端口没有加进面板规则；请确认云安全组已放行"}, nil
	}
	ports := append([]int{newPort}, cfg.SSHPorts...)
	sort.Ints(ports)
	panelPort := 0
	if _, p, err := net.SplitHostPort(cfg.Listen); err == nil {
		panelPort, _ = strconv.Atoi(p)
	}
	opt := firewall.ApplyOptions{
		Rules:         toRules(rows),
		SSHPorts:      ports,
		PanelPort:     panelPort,
		PanelLoopback: cfg.IsLoopback(),
	}
	if _, err := firewall.Apply(opt); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}
