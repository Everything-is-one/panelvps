// Package tune 负责内核网络参数（目前只有 BBR）的读取与切换。
//
// 主服务跑在 ProtectKernelTunables=yes 的沙箱里，自己写不了 /proc/sys，
// 所以这里只做两件事：写 /etc/sysctl.d 下的配置文件，再触发一个专门的
// oneshot 单元（panelvps-sysctl.service）去 modprobe + sysctl -p。
// 命令行的管理菜单用同一份实现，避免两套逻辑跑偏。
package tune

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	// BBRConfPath 是本面板管理的 sysctl 配置文件。
	BBRConfPath = "/etc/sysctl.d/99-panelvps.conf"
	// BBRUnitName 是应用配置的 oneshot 单元。
	BBRUnitName = "panelvps-sysctl.service"
	// BBRUnitPath 是单元文件位置。
	BBRUnitPath = "/etc/systemd/system/" + BBRUnitName
	// marker 写在配置文件首行，用来判断这个文件是不是本面板生成的。
	marker = "# Managed by PanelVPS"
)

// BBRView 是给前端与命令行共用的状态快照。
type BBRView struct {
	Supported     bool     `json:"supported"`
	Enabled       bool     `json:"enabled"`
	Current       string   `json:"current"`
	Qdisc         string   `json:"qdisc"`
	Available     []string `json:"available"`
	Reason        string   `json:"reason,omitempty"`
	ConfigPath    string   `json:"config_path"`
	ConfigExists  bool     `json:"config_exists"`
	UnitInstalled bool     `json:"unit_installed"`
}

// BBR 返回当前拥塞控制算法、队列算法与可用算法列表。
func BBR() BBRView {
	v := BBRView{
		ConfigPath: BBRConfPath,
		Available:  []string{},
	}
	if runtime.GOOS != "linux" {
		v.Reason = "当前系统不是 Linux，BBR 配置仅在服务器上可用"
		return v
	}
	v.Current = readTrim("/proc/sys/net/ipv4/tcp_congestion_control")
	v.Qdisc = readTrim("/proc/sys/net/core/default_qdisc")
	v.Available = strings.Fields(readTrim("/proc/sys/net/ipv4/tcp_available_congestion_control"))
	v.Enabled = v.Current == "bbr"
	v.ConfigExists = fileExists(BBRConfPath)
	v.UnitInstalled = fileExists(BBRUnitPath)

	switch {
	case slices.Contains(v.Available, "bbr") || v.Current == "bbr":
		v.Supported = true
	case exec.Command("modprobe", "-n", "tcp_bbr").Run() == nil:
		// 模块存在但尚未加载；开启时会由 helper 负责 modprobe。
		v.Supported = true
		v.Reason = "内核提供 tcp_bbr 模块，开启时自动加载"
	default:
		v.Reason = "内核未提供 tcp_bbr 模块（或 modprobe 不可用）"
	}
	return v
}

// SetBBR 打开或关闭 BBR，返回切换后的状态。
func SetBBR(enable bool) (BBRView, error) {
	before := BBR()
	if enable && !before.Supported {
		return before, errors.New("当前内核不支持 BBR：" + before.Reason)
	}

	prev, prevErr := os.ReadFile(BBRConfPath)
	if prevErr == nil && !strings.Contains(string(prev), marker) {
		return before, fmt.Errorf("%s 已存在且不是面板生成的，请手动处理后再试", BBRConfPath)
	}
	if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
		return before, fmt.Errorf("读取 %s 失败: %w", BBRConfPath, prevErr)
	}

	body := marker + "\nnet.ipv4.tcp_congestion_control=cubic\n"
	if enable {
		body = marker + "\nnet.core.default_qdisc=fq\nnet.ipv4.tcp_congestion_control=bbr\n"
	}
	if err := os.MkdirAll(filepath.Dir(BBRConfPath), 0o755); err != nil {
		return before, fmt.Errorf("创建 sysctl 目录失败: %w", err)
	}
	if err := writeFileAtomic(BBRConfPath, []byte(body), 0o644); err != nil {
		return before, fmt.Errorf("写入 sysctl 配置失败（安装脚本可能未放行 /etc/sysctl.d）: %w", err)
	}

	if err := applySysctl(); err != nil {
		// 回滚文件；已经生效的内核参数留到下次重启自然恢复。
		if prevErr == nil {
			_ = writeFileAtomic(BBRConfPath, prev, 0o644)
		} else {
			_ = os.Remove(BBRConfPath)
		}
		return BBR(), err
	}

	after := BBR()
	if enable && after.Current != "bbr" {
		return after, fmt.Errorf("配置已写入，但内核当前仍是 %s", after.Current)
	}
	if !enable && after.Current == "bbr" {
		return after, errors.New("配置已写入，但内核仍在使用 bbr")
	}
	return after, nil
}

// applySysctl 通过特权 helper 应用配置。
//
// 主服务开启了 ProtectSystem / ProtectKernelTunables，自己写 /proc/sys 会被拒绝；
// helper 是独立的 oneshot 单元，只执行 modprobe 与 sysctl -p 本文件。
func applySysctl() error {
	if fileExists(BBRUnitPath) {
		out, err := exec.Command("systemctl", "restart", BBRUnitName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("应用 sysctl 失败: %s", firstLine(string(out), err.Error()))
		}
		return nil
	}
	// 没有 helper（例如手工在终端里跑二进制）时直接应用。
	p, err := exec.LookPath("sysctl")
	if err != nil {
		return errors.New("系统没有 sysctl，且未安装 panelvps-sysctl.service")
	}
	_ = exec.Command("modprobe", "tcp_bbr").Run()
	out, err := exec.Command(p, "-p", BBRConfPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("应用 sysctl 失败: %s", firstLine(string(out), err.Error()))
	}
	return nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".panelvps-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
