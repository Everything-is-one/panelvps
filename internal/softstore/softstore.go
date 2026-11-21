// Package softstore 实现声明式软件商店。
//
// 清单只允许 7 种动作：apt / download / extract / write_file / path /
// service / check。刻意不支持自由脚本——一旦允许，商店就变成任意代码
// 执行入口，审计、回滚、卸载都会失效。
//
// 清单随二进制内嵌（manifests/*.yaml），不从网络或用户输入加载。
package softstore

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Everything-is-one/panelvps/internal/sysinfo"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

// ErrNotFound 表示清单不存在。
var ErrNotFound = errors.New("软件不存在")

// ErrUnsupported 表示当前平台无法执行安装。
var ErrUnsupported = errors.New("软件商店仅在 Linux 服务器上可执行")

// Manifest 是一份安装清单。
type Manifest struct {
	Slug      string   `yaml:"slug" json:"slug"`
	Name      string   `yaml:"name" json:"name"`
	Category  string   `yaml:"category" json:"category"`
	Summary   string   `yaml:"summary" json:"summary"`
	Version   string   `yaml:"version" json:"version"`
	MemoryMB  int      `yaml:"memory_mb" json:"memory_mb"`
	DiskMB    int      `yaml:"disk_mb" json:"disk_mb"`
	Ports     []int    `yaml:"ports" json:"ports"`
	Deps      []string `yaml:"deps" json:"deps"`
	Conflicts []string `yaml:"conflicts" json:"conflicts"`
	Risk      string   `yaml:"risk" json:"risk"` // low | medium | high
	Warning   string   `yaml:"warning" json:"warning"`
	// Detect 是一条探测命令（如 xray version）。apt 装的软件靠 dpkg 判断，
	// 直接下载二进制的软件没有包记录，只能靠命令能否跑起来判断已装。
	Detect []string `yaml:"detect" json:"detect,omitempty"`
	Plan   Plan     `yaml:"plan" json:"-"`
}

// Plan 是安装与卸载的声明式步骤。
type Plan struct {
	Install   []Action `yaml:"install" json:"install"`
	Uninstall []Action `yaml:"uninstall" json:"uninstall"`
}

// Action 是一步操作，七个字段至多一个非空。
type Action struct {
	Apt       *AptAction       `yaml:"apt" json:"apt,omitempty"`
	Download  *DownloadAction  `yaml:"download" json:"download,omitempty"`
	Extract   *ExtractAction   `yaml:"extract" json:"extract,omitempty"`
	WriteFile *WriteFileAction `yaml:"write_file" json:"write_file,omitempty"`
	Path      *PathAction      `yaml:"path" json:"path,omitempty"`
	Service   *ServiceAction   `yaml:"service" json:"service,omitempty"`
	Check     *CheckAction     `yaml:"check" json:"check,omitempty"`
}

// AptAction 调用 apt-get。
type AptAction struct {
	Names  []string `yaml:"names" json:"names"`
	Update bool     `yaml:"update" json:"update"`
	Purge  bool     `yaml:"purge" json:"purge"`
}

// DownloadAction 下载文件到本机。
type DownloadAction struct {
	URL    string `yaml:"url" json:"url"`
	Dest   string `yaml:"dest" json:"dest"`
	SHA256 string `yaml:"sha256" json:"sha256"`
	Mode   string `yaml:"mode" json:"mode"`
}

// ExtractAction 解压到目标目录。Include 非空时只解压这些文件名（按 basename 匹配）。
type ExtractAction struct {
	Src     string   `yaml:"src" json:"src"`
	Dest    string   `yaml:"dest" json:"dest"`
	Include []string `yaml:"include" json:"include,omitempty"`
}

// WriteFileAction 从内嵌模板写文件。
type WriteFileAction struct {
	Path string `yaml:"path" json:"path"`
	From string `yaml:"from" json:"from"`
	Mode string `yaml:"mode" json:"mode"`
}

// PathAction 创建或删除目录。
type PathAction struct {
	Path  string `yaml:"path" json:"path"`
	State string `yaml:"state" json:"state"` // directory | absent
}

// ServiceAction 控制 systemd 服务。
type ServiceAction struct {
	Name    string `yaml:"name" json:"name"`
	Enable  bool   `yaml:"enable" json:"enable"`
	Disable bool   `yaml:"disable" json:"disable"`
	Start   bool   `yaml:"start" json:"start"`
	Stop    bool   `yaml:"stop" json:"stop"`
	// IgnoreError 用于「单元可能不存在」的场景：停用随包安装的服务时，
	// 没装过官方包的机器上 systemctl 会报未找到，不该让整个安装回滚。
	IgnoreError bool `yaml:"ignore_error" json:"ignore_error"`
}

// CheckAction 执行一条校验命令。
type CheckAction struct {
	Cmd []string `yaml:"cmd" json:"cmd"`
}

// Store 是清单集合与执行器。
type Store struct {
	manifests map[string]*Manifest
	order     []string
}

// Load 读取内嵌清单并做静态校验。
func Load() (*Store, error) {
	entries, err := fs.ReadDir(manifestFS, "manifests")
	if err != nil {
		return nil, err
	}
	s := &Store{manifests: map[string]*Manifest{}}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := manifestFS.ReadFile(path.Join("manifests", e.Name()))
		if err != nil {
			return nil, err
		}
		var m Manifest
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("解析清单 %s 失败: %w", e.Name(), err)
		}
		if err := m.validate(); err != nil {
			return nil, fmt.Errorf("清单 %s 不合法: %w", e.Name(), err)
		}
		if _, dup := s.manifests[m.Slug]; dup {
			return nil, fmt.Errorf("清单 slug 重复: %s", m.Slug)
		}
		s.manifests[m.Slug] = &m
		s.order = append(s.order, m.Slug)
	}
	sort.Strings(s.order)
	return s, nil
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)
var pkgRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,60}$`)

func (m *Manifest) validate() error {
	if !slugRe.MatchString(m.Slug) {
		return errors.New("slug 只能用小写字母、数字与 -")
	}
	if m.Name == "" || m.Summary == "" {
		return errors.New("name 与 summary 不能为空")
	}
	switch m.Risk {
	case "low", "medium", "high":
	default:
		return errors.New("risk 只能是 low/medium/high")
	}
	if len(m.Plan.Install) == 0 {
		return errors.New("安装计划不能为空")
	}
	for _, list := range [][]Action{m.Plan.Install, m.Plan.Uninstall} {
		for i, a := range list {
			if err := a.validate(); err != nil {
				return fmt.Errorf("第 %d 步: %w", i+1, err)
			}
		}
	}
	return nil
}

func (a Action) validate() error {
	n := 0
	if a.Apt != nil {
		n++
		if len(a.Apt.Names) == 0 {
			return errors.New("apt 必须给出 names")
		}
		for _, p := range a.Apt.Names {
			if !pkgRe.MatchString(p) {
				return fmt.Errorf("包名非法: %q", p)
			}
		}
	}
	if a.Download != nil {
		n++
		u, err := url.Parse(a.Download.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("download.url 必须是 https 地址")
		}
		if err := checkPublicHost(u.Hostname()); err != nil {
			return err
		}
		if !path.IsAbs(a.Download.Dest) {
			return errors.New("download.dest 必须是绝对路径")
		}
		if a.Download.SHA256 != "" && len(a.Download.SHA256) != 64 {
			return errors.New("sha256 长度应为 64")
		}
	}
	if a.Extract != nil {
		n++
		if !path.IsAbs(a.Extract.Src) || !path.IsAbs(a.Extract.Dest) {
			return errors.New("extract 的 src/dest 必须是绝对路径")
		}
	}
	if a.WriteFile != nil {
		n++
		if !path.IsAbs(a.WriteFile.Path) {
			return errors.New("write_file.path 必须是绝对路径")
		}
		if a.WriteFile.From == "" || strings.Contains(a.WriteFile.From, "..") {
			return errors.New("write_file.from 非法")
		}
	}
	if a.Path != nil {
		n++
		if !path.IsAbs(a.Path.Path) {
			return errors.New("path.path 必须是绝对路径")
		}
		if a.Path.State != "directory" && a.Path.State != "absent" {
			return errors.New("path.state 只能是 directory 或 absent")
		}
	}
	if a.Service != nil {
		n++
		if !pkgRe.MatchString(a.Service.Name) {
			return fmt.Errorf("服务名非法: %q", a.Service.Name)
		}
	}
	if a.Check != nil {
		n++
		if len(a.Check.Cmd) == 0 {
			return errors.New("check.cmd 不能为空")
		}
	}
	if n != 1 {
		return errors.New("每一步必须且只能声明一种动作")
	}
	return nil
}

// checkPublicHost 拒绝把下载指向回环/内网，避免清单被篡改后变成 SSRF 跳板。
func checkPublicHost(host string) error {
	if host == "" {
		return errors.New("下载地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("下载地址不允许指向内网: %s", host)
		}
	}
	if strings.EqualFold(host, "localhost") {
		return errors.New("下载地址不允许指向 localhost")
	}
	return nil
}

// List 返回全部清单（按 slug 排序）。
func (s *Store) List() []*Manifest {
	out := make([]*Manifest, 0, len(s.order))
	for _, slug := range s.order {
		out = append(out, s.manifests[slug])
	}
	return out
}

// Get 按 slug 取清单。
func (s *Store) Get(slug string) (*Manifest, error) {
	m, ok := s.manifests[slug]
	if !ok {
		return nil, ErrNotFound
	}
	return m, nil
}

// PlanLines 把计划翻译成人类可读的步骤，供安装前预览。
func PlanLines(m *Manifest, mode string) []string {
	list := m.Plan.Install
	if mode == "uninstall" {
		list = m.Plan.Uninstall
	}
	var out []string
	for _, a := range list {
		switch {
		case a.Apt != nil:
			if a.Apt.Purge {
				out = append(out, "apt purge "+strings.Join(a.Apt.Names, " "))
			} else {
				s := "apt install " + strings.Join(a.Apt.Names, " ")
				if a.Apt.Update {
					s = "apt update && " + s
				}
				out = append(out, s+"（--no-install-recommends）")
			}
		case a.Download != nil:
			out = append(out, "下载 "+a.Download.URL+" → "+a.Download.Dest)
		case a.Extract != nil:
			out = append(out, "解压 "+a.Extract.Src+" → "+a.Extract.Dest)
		case a.WriteFile != nil:
			out = append(out, "写入 "+a.WriteFile.Path+"（模板 "+a.WriteFile.From+"）")
		case a.Path != nil:
			out = append(out, "路径 "+a.Path.Path+" → "+a.Path.State)
		case a.Service != nil:
			var ops []string
			if a.Service.Stop {
				ops = append(ops, "停止")
			}
			if a.Service.Disable {
				ops = append(ops, "取消自启")
			}
			if a.Service.Enable {
				ops = append(ops, "开机自启")
			}
			if a.Service.Start {
				ops = append(ops, "启动")
			}
			out = append(out, "服务 "+a.Service.Name+"："+strings.Join(ops, " + "))
		case a.Check != nil:
			out = append(out, "校验 "+strings.Join(a.Check.Cmd, " "))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// Install 执行安装计划；任一步失败则尽力回滚。
func (s *Store) Install(ctx context.Context, slug string, log func(string), force bool) error {
	m, err := s.Get(slug)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return ErrUnsupported
	}

	if msg, installed := Installed(m); installed {
		return fmt.Errorf("已检测到安装（版本 %s），无需重复安装", msg)
	}
	for _, c := range m.Conflicts {
		if _, ok := dpkgVersion(c); ok {
			return fmt.Errorf("与已安装的 %s 冲突，请先卸载它", c)
		}
	}
	for _, dep := range m.Deps {
		dm, err := s.Get(dep)
		if err != nil {
			continue
		}
		if _, ok := Installed(dm); !ok {
			return fmt.Errorf("缺少依赖 %s，请先安装它", dm.Name)
		}
	}
	if !force && m.MemoryMB > 0 {
		if avail := sysinfo.MemAvailable(); avail > 0 && int64(m.MemoryMB)<<20 > avail/2 {
			return fmt.Errorf("可用内存不足：该软件预计占用 %dMB，当前仅剩 %dMB；确认要继续请勾选强制安装",
				m.MemoryMB, avail>>20)
		}
	}

	log(fmt.Sprintf("开始安装 %s（%s）", m.Name, m.Version))
	if err := preflight(m.Plan.Install, log); err != nil {
		return err
	}
	if err := runPlan(ctx, m.Plan.Install, log); err != nil {
		log("安装失败，开始回滚")
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if rerr := runPlan(rollbackCtx, m.Plan.Uninstall, log); rerr != nil {
			log("回滚未完全成功：" + rerr.Error())
		}
		return err
	}
	log("安装完成")
	return nil
}

// Uninstall 执行卸载计划。
func (s *Store) Uninstall(ctx context.Context, slug string, log func(string)) error {
	m, err := s.Get(slug)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return ErrUnsupported
	}
	log(fmt.Sprintf("开始卸载 %s", m.Name))
	if err := runPlan(ctx, m.Plan.Uninstall, log); err != nil {
		return err
	}
	log("卸载完成")
	return nil
}

func runPlan(ctx context.Context, plan []Action, log func(string)) error {
	for i, a := range plan {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("任务被取消或超时: %w", err)
		}
		label := fmt.Sprintf("[%d/%d] %s", i+1, len(plan), describe(a))
		log(label)
		if err := runAction(ctx, a, log); err != nil {
			return fmt.Errorf("%s 失败: %w", label, err)
		}
	}
	return nil
}

func describe(a Action) string {
	switch {
	case a.Apt != nil:
		if a.Apt.Purge {
			return "apt purge " + strings.Join(a.Apt.Names, " ")
		}
		return "apt install " + strings.Join(a.Apt.Names, " ")
	case a.Download != nil:
		return "下载 " + a.Download.URL
	case a.Extract != nil:
		return "解压 " + a.Extract.Src
	case a.WriteFile != nil:
		return "写入 " + a.WriteFile.Path
	case a.Path != nil:
		return "路径 " + a.Path.Path
	case a.Service != nil:
		return "服务 " + a.Service.Name
	case a.Check != nil:
		return "校验 " + strings.Join(a.Check.Cmd, " ")
	}
	return "未知动作"
}

func runAction(ctx context.Context, a Action, log func(string)) error {
	switch {
	case a.Apt != nil:
		return runApt(ctx, *a.Apt, log)
	case a.Download != nil:
		return runDownload(ctx, *a.Download, log)
	case a.Extract != nil:
		return runExtract(*a.Extract, log)
	case a.WriteFile != nil:
		return runWriteFile(*a.WriteFile, log)
	case a.Path != nil:
		return runPath(*a.Path, log)
	case a.Service != nil:
		return runService(*a.Service, log)
	case a.Check != nil:
		return runCheck(ctx, *a.Check, log)
	}
	return errors.New("空动作")
}

func runCmd(ctx context.Context, log func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C.UTF-8")
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if len(line) > 400 {
				line = line[:400] + "…"
			}
			log("  " + line)
		}
	}
	return err
}

func runApt(ctx context.Context, a AptAction, log func(string)) error {
	if a.Update {
		if err := runCmd(ctx, log, "apt-get", "update", "-q"); err != nil {
			return fmt.Errorf("apt-get update 失败: %w", err)
		}
	}
	args := []string{"-y", "--no-install-recommends"}
	if a.Purge {
		args = []string{"-y", "purge"}
	}
	args = append(args, a.Names...)
	return runCmd(ctx, log, "apt-get", args...)
}

func runDownload(ctx context.Context, a DownloadAction, log func(string)) error {
	if err := os.MkdirAll(filepath.Dir(a.Dest), 0o755); err != nil {
		return err
	}
	// 清单里用 ${ARCH} 表示架构，下载前再展开，这样一份清单同时覆盖 amd64 / arm64
	url := strings.ReplaceAll(a.URL, "${ARCH}", archTag())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(a.Dest), ".pv-dl-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	h := sha256.New()
	const maxSize = 300 << 20
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		tmp.Close()
		return err
	}
	if n > maxSize {
		tmp.Close()
		return errors.New("下载文件超过 300MB 限制")
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if a.SHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, a.SHA256) {
			return fmt.Errorf("sha256 不匹配：期望 %s，实际 %s", a.SHA256, got)
		}
	}
	mode := os.FileMode(0o644)
	if a.Mode != "" {
		if v, err := strconv.ParseUint(strings.TrimPrefix(a.Mode, "0o"), 8, 32); err == nil {
			mode = os.FileMode(v)
		}
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	log(fmt.Sprintf("  已下载 %d 字节，sha256 %s", n, hex.EncodeToString(h.Sum(nil))[:16]))
	return os.Rename(name, a.Dest)
}

func runExtract(a ExtractAction, log func(string)) error {
	if err := os.MkdirAll(a.Dest, 0o755); err != nil {
		return err
	}
	lower := strings.ToLower(a.Src)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(a.Src, a.Dest, a.Include, log)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(a.Src, a.Dest, a.Include, log)
	default:
		return errors.New("仅支持 .zip / .tar.gz")
	}
}

// archTag 返回上游发布件常用的架构命名。
func archTag() string {
	switch runtime.GOARCH {
	case "amd64":
		return "64"
	case "arm64":
		return "arm64-v8a"
	case "386":
		return "32"
	case "arm":
		return "arm32-v7a"
	}
	return runtime.GOARCH
}

// wantFile 判断某个归档条目是否需要解压。include 为空表示全要。
func wantFile(name string, include []string) bool {
	if len(include) == 0 {
		return true
	}
	base := filepath.Base(name)
	for _, want := range include {
		if base == want {
			return true
		}
	}
	return false
}

func safeJoin(root, name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", errors.New("压缩包包含非法路径: " + name)
	}
	abs := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("压缩包包含非法路径: " + name)
	}
	return abs, nil
}

func extractZip(src, dst string, include []string, log func(string)) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	kept := 0
	for _, f := range zr.File {
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !f.FileInfo().IsDir() && !wantFile(f.Name, include) {
			continue
		}
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		kept++
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode().Perm()|0o600)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 512<<20))
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	log(fmt.Sprintf("  解压 %d 个条目", kept))
	return nil
}

func extractTarGz(src, dst string, include []string, log func(string)) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		count++
		if count > 50000 {
			return errors.New("压缩包条目过多")
		}
		if hdr.Typeflag == tar.TypeReg && !wantFile(hdr.Name, include) {
			continue
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, 512<<20))
			out.Close()
			if err != nil {
				return err
			}
		}
	}
	log(fmt.Sprintf("  解压 %d 个条目", count))
	return nil
}

func runWriteFile(a WriteFileAction, log func(string)) error {
	raw, err := manifestFS.ReadFile(path.Join("templates", a.From))
	if err != nil {
		return fmt.Errorf("模板不存在: %s", a.From)
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if a.Mode != "" {
		if v, err := strconv.ParseUint(strings.TrimPrefix(a.Mode, "0o"), 8, 32); err == nil {
			mode = os.FileMode(v)
		}
	}
	if err := os.WriteFile(a.Path, raw, mode); err != nil {
		return err
	}
	log("  已写入 " + a.Path)
	return nil
}

func runPath(a PathAction, log func(string)) error {
	if a.State == "absent" {
		if err := os.RemoveAll(a.Path); err != nil {
			return err
		}
		log("  已删除 " + a.Path)
		return nil
	}
	if err := os.MkdirAll(a.Path, 0o755); err != nil {
		return err
	}
	log("  已创建 " + a.Path)
	return nil
}

func runService(a ServiceAction, log func(string)) error {
	type step struct {
		on   bool
		verb string
	}
	for _, s := range []step{
		{a.Stop, "stop"},
		{a.Disable, "disable"},
		{a.Enable, "enable"},
		{a.Start, "start"},
	} {
		if !s.on {
			continue
		}
		if err := runCmd(context.Background(), log, "systemctl", s.verb, a.Name); err != nil {
			if a.IgnoreError {
				log("  跳过 systemctl " + s.verb + " " + a.Name)
				continue
			}
			return fmt.Errorf("systemctl %s %s 失败: %w", s.verb, a.Name, err)
		}
		if s.verb == "start" || s.verb == "stop" {
			// 给服务一点启动/停止时间，避免下一步 check 撞上中间态
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil
}

func runCheck(ctx context.Context, a CheckAction, log func(string)) error {
	name := a.Cmd[0]
	args := a.Cmd[1:]
	if err := runCmd(ctx, log, name, args...); err != nil {
		return fmt.Errorf("校验命令返回非零: %w", err)
	}
	return nil
}

// Installed 判断清单里的软件是否已安装，返回版本号。
func Installed(m *Manifest) (string, bool) {
	if len(m.Detect) > 0 && runtime.GOOS == "linux" {
		out, err := exec.Command(m.Detect[0], m.Detect[1:]...).Output()
		if err == nil {
			v := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
			if len(v) > 60 {
				v = v[:60] + "…"
			}
			return v, true
		}
	}
	for _, a := range m.Plan.Install {
		if a.Apt == nil {
			continue
		}
		for _, name := range a.Apt.Names {
			if v, ok := dpkgVersion(name); ok {
				return v, true
			}
		}
	}
	return "", false
}

func dpkgVersion(pkg string) (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}\t${Version}", pkg).Output()
	if err != nil {
		return "", false
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) != 2 || !strings.Contains(parts[0], "install ok installed") {
		return "", false
	}
	return parts[1], true
}
