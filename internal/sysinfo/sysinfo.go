// Package sysinfo 采集 Linux 系统信息并托管 systemd 服务。
//
// 全部实现基于 /proc 与 systemctl，不引入 cgo，也不依赖 ps/netstat 等外部命令
// 的解析格式（systemctl 除外，因为它是 Debian 上的事实标准）。
// 非 Linux 平台统一返回 ErrUnsupported，面板其余功能不受影响。
package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported 表示当前平台不提供该能力。
var ErrUnsupported = errors.New("当前系统不支持该功能（仅 Linux）")

// Proc 是一个进程的快照。
type Proc struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	Name    string  `json:"name"`
	State   string  `json:"state"`
	RSS     int64   `json:"rss"`     // 字节
	CPU     float64 `json:"cpu_pct"` // 进程生命周期内的平均值，避免维护全局采样状态
	Threads int     `json:"threads"`
	Command string  `json:"command"`
}

// Service 是受面板关注的 systemd 服务状态。
type Service struct {
	Name    string `json:"name"`
	Load    string `json:"load"`
	Active  string `json:"active"`
	Sub     string `json:"sub"`
	PID     int    `json:"pid"`
	Memory  int64  `json:"memory"`
	Enabled bool   `json:"enabled"`
	Desc    string `json:"description,omitempty"`
}

// Port 是监听中的端口。
type Port struct {
	Proto  string `json:"proto"`
	Addr   string `json:"addr"`
	Port   int    `json:"port"`
	Expose bool   `json:"public"` // 是否绑定在非回环地址
}

// NetIface 是网卡累计流量。
type NetIface struct {
	Name    string `json:"name"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

// OSInfo 是主机概况。
type OSInfo struct {
	Hostname string `json:"hostname"`
	Kernel   string `json:"kernel"`
	Distro   string `json:"distro"`
	Arch     string `json:"arch"`
	Uptime   int64  `json:"uptime_sec"`
	CPUCores int    `json:"cpu_cores"`
}

// Info 返回主机概况。
func Info() OSInfo {
	host, _ := os.Hostname()
	out := OSInfo{
		Hostname: host,
		Arch:     runtime.GOARCH,
		Uptime:   readUptime(),
		CPUCores: runtime.NumCPU(),
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		out.Kernel = strings.TrimSpace(string(raw))
	}
	if f, err := os.Open("/etc/os-release"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				out.Distro = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
		}
	}
	if out.Distro == "" {
		out.Distro = runtime.GOOS
	}
	return out
}

func readUptime() int64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return int64(v)
}

const clockTicks = 100 // Linux 用户态的 USER_HZ 基本都是 100

// Processes 按内存占用返回前 limit 个进程。limit<=0 表示全部。
func Processes(limit int) ([]Proc, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	boot := readUptime()
	pageSize := int64(os.Getpagesize())
	var out []Proc

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, ok := readProc(pid, pageSize, boot)
		if !ok {
			continue
		}
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RSS > out[j].RSS })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func readProc(pid int, pageSize, bootUptime int64) (Proc, bool) {
	base := "/proc/" + strconv.Itoa(pid)
	raw, err := os.ReadFile(base + "/stat")
	if err != nil {
		return Proc{}, false
	}
	// comm 可能含空格与括号，按最后一个 ')' 切分
	s := string(raw)
	open := strings.Index(s, "(")
	close := strings.LastIndex(s, ")")
	if open < 0 || close <= open {
		return Proc{}, false
	}
	name := s[open+1 : close]
	rest := strings.Fields(s[close+1:])
	if len(rest) < 22 {
		return Proc{}, false
	}
	state := rest[0]
	ppid, _ := strconv.Atoi(rest[1])
	utime, _ := strconv.ParseInt(rest[11], 10, 64)
	stime, _ := strconv.ParseInt(rest[12], 10, 64)
	threads, _ := strconv.Atoi(rest[17])
	rssPages, _ := strconv.ParseInt(rest[21], 10, 64)
	startTicks, _ := strconv.ParseInt(rest[19], 10, 64)

	// 进程存活秒数 = 系统 uptime - 启动时刻
	startSec := float64(startTicks) / clockTicks
	alive := float64(bootUptime) - startSec
	if alive < 1 {
		alive = 1
	}
	cpuPct := float64(utime+stime) / clockTicks / alive * 100
	if cpuPct < 0 || cpuPct > 100*float64(runtime.NumCPU()) {
		cpuPct = 0
	}

	cmd := name
	if raw, err := os.ReadFile(base + "/cmdline"); err == nil && len(raw) > 1 {
		cmd = strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(raw, "\x00")), "\x00", " "))
	}

	return Proc{
		PID:     pid,
		PPID:    ppid,
		User:    procUser(base),
		Name:    name,
		State:   state,
		RSS:     rssPages * pageSize,
		CPU:     cpuPct,
		Threads: threads,
		Command: truncate(cmd, 240),
	}, true
}

func procUser(base string) string {
	raw, err := os.ReadFile(base + "/status")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if uid, err := strconv.Atoi(fields[1]); err == nil {
					return lookupUID(uid)
				}
			}
		}
	}
	return ""
}

// uidCache 避免对每个进程都读一遍 /etc/passwd。
var uidCache = map[int]string{}

func lookupUID(uid int) string {
	if name, ok := uidCache[uid]; ok {
		return name
	}
	name := strconv.Itoa(uid)
	if f, err := os.Open("/etc/passwd"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			parts := strings.Split(sc.Text(), ":")
			if len(parts) >= 3 {
				if v, _ := strconv.Atoi(parts[2]); v == uid {
					name = parts[0]
					break
				}
			}
		}
	}
	uidCache[uid] = name
	return name
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// 受关注的服务：面板自身、代理、Web、数据库、容器。
var watchedUnits = []string{
	"panelvps", "xray", "nginx", "apache2",
	"php8.2-fpm", "php8.3-fpm", "php8.4-fpm", "php8.1-fpm",
	"mariadb", "mysql", "redis-server", "docker", "ssh", "sshd", "cron",
}

var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9@._:-]{1,64}$`)

// Services 返回受关注服务的状态。systemd 不可用时返回 ErrUnsupported。
func Services() ([]Service, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, ErrUnsupported
	}

	var out []Service
	for _, name := range watchedUnits {
		unit := name + ".service"
		// 只展示系统上真实存在的单元，避免把「未安装」和「已停止」混在一起
		if !unitExists(unit) {
			continue
		}
		fields := showUnit(unit)
		out = append(out, Service{
			Name:    name,
			Load:    fields["LoadState"],
			Active:  fields["ActiveState"],
			Sub:     fields["SubState"],
			PID:     atoiSafe(fields["MainPID"]),
			Memory:  int64(atoiSafe(fields["MemoryCurrent"])),
			Enabled: fields["UnitFileState"] == "enabled",
			Desc:    fields["Description"],
		})
	}
	return out, nil
}

func unitExists(unit string) bool {
	cmd := exec.Command("systemctl", "show", "-p", "LoadState", "--value", unit)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "not-found"
}

func showUnit(unit string) map[string]string {
	cmd := exec.Command("systemctl", "show", unit,
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState",
		"-p", "MainPID", "-p", "MemoryCurrent", "-p", "UnitFileState",
		"-p", "Description")
	out, err := cmd.Output()
	if err != nil {
		return map[string]string{}
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = v
		}
	}
	return fields
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	if n < 0 {
		return 0
	}
	return n
}

var allowedActions = map[string]bool{
	"start": true, "stop": true, "restart": true, "reload": true,
	"enable": true, "disable": true,
}

// ServiceAction 对指定服务执行动作。服务名与动作都走白名单校验，
// 不拼接 shell，避免命令注入。
func ServiceAction(name, action string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", ErrUnsupported
	}
	name = strings.TrimSuffix(name, ".service")
	if !unitNameRe.MatchString(name) {
		return "", fmt.Errorf("服务名非法: %q", name)
	}
	if !allowedActions[action] {
		return "", fmt.Errorf("不支持的动作: %q", action)
	}
	unit := name + ".service"
	if !unitExists(unit) {
		return "", fmt.Errorf("服务不存在: %s", unit)
	}
	cmd := exec.Command("systemctl", action, unit)
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		return msg, fmt.Errorf("systemctl %s %s 失败: %w", action, name, err)
	}
	return msg, nil
}

// ListeningPorts 解析 /proc/net/tcp{,6} 得到监听端口。
func ListeningPorts() ([]Port, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	var out []Port
	for _, item := range []struct{ file, proto string }{
		{"/proc/net/tcp", "tcp"}, {"/proc/net/tcp6", "tcp6"},
		{"/proc/net/udp", "udp"}, {"/proc/net/udp6", "udp6"},
	} {
		ports, err := parseProcNet(item.file, item.proto)
		if err != nil {
			continue
		}
		out = append(out, ports...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out, nil
}

func parseProcNet(file, proto string) ([]Port, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Port
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "0A" && !strings.HasPrefix(proto, "udp") {
			continue // 非 LISTEN；UDP 没有连接状态，一律视为监听
		}
		ipHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		pv, err := strconv.ParseInt(portHex, 16, 32)
		if err != nil {
			continue
		}
		addr, public := decodeProcIP(ipHex)
		out = append(out, Port{Proto: proto, Addr: addr, Port: int(pv), Expose: public})
	}
	return out, sc.Err()
}

// decodeProcIP 把 /proc/net 里的小端十六进制地址还原成可读 IP。
func decodeProcIP(hex string) (string, bool) {
	switch len(hex) {
	case 8:
		b := make([]byte, 4)
		for i := 0; i < 4; i++ {
			v, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
			b[3-i] = byte(v)
		}
		ip := fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
		return ip, ip != "127.0.0.1"
	case 32:
		b := make([]byte, 16)
		for i := 0; i < 16; i++ {
			v, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
			b[15-i] = byte(v)
		}
		// 简化展示：仅识别 ::1 与 ::
		allZero := true
		loopback := true
		for i, v := range b {
			if v != 0 {
				allZero = false
			}
			if i != 15 && v != 0 {
				loopback = false
			}
		}
		if allZero {
			return "::", true
		}
		if loopback && b[15] == 1 {
			return "::1", false
		}
		parts := make([]string, 8)
		for i := 0; i < 8; i++ {
			parts[i] = fmt.Sprintf("%x", int(b[i*2])<<8|int(b[i*2+1]))
		}
		return strings.Join(parts, ":"), true
	}
	return hex, false
}

// Interfaces 返回网卡累计流量。
func Interfaces() ([]NetIface, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []NetIface
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || name == "" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		out = append(out, NetIface{Name: name, RxBytes: rx, TxBytes: tx})
	}
	return out, sc.Err()
}

// MemAvailable 返回可用内存字节数，供软件商店做装前拦截。
func MemAvailable() int64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

// FindSSHPorts 从 sshd 配置推断监听端口，失败时回退到 22。
func FindSSHPorts() []int {
	ports := map[int]bool{}
	for _, path := range []string{"/etc/ssh/sshd_config"} {
		if raw, err := os.ReadFile(path); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(raw))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "#") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) >= 2 && strings.EqualFold(fields[0], "Port") {
					if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 && n < 65536 {
						ports[n] = true
					}
				}
			}
		}
	}
	if len(ports) == 0 {
		ports[22] = true
	}
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// CgroupVersion 返回 "1" / "2" / ""，用于在概览里提示内核兼容性。
func CgroupVersion() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "2"
	}
	if st, err := os.Stat(filepath.Join("/sys/fs/cgroup", "unified")); err == nil && st.IsDir() {
		return "1"
	}
	return "1"
}

// KernelModules 返回已加载模块名集合，用于探测 nf_tables 能力。
func KernelModules() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile("/proc/modules")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			out[fields[0]] = true
		}
	}
	return out
}

// TimeNow 便于测试替换。
var TimeNow = time.Now
