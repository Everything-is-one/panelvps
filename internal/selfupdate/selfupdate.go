// Package selfupdate 从 GitHub Release 检查并安装面板新版本。
//
// 只信任 HTTPS + GitHub 域名 + 可选的 checksums.txt；
// 替换前校验 ELF 头，替换时保留上一版，失败可人工回滚。
package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported 表示当前平台不支持自更新。
var ErrUnsupported = errors.New("自更新仅在 Linux 服务器上可用")

// Asset 是 Release 附件。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release 是 GitHub API 的响应子集。
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
	Prerelease  bool      `json:"prerelease"`
}

// Info 是给前端的检查结果。
type Info struct {
	Current     string    `json:"current"`
	Latest      string    `json:"latest"`
	Available   bool      `json:"available"`
	Notes       string    `json:"notes,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	AssetName   string    `json:"asset_name,omitempty"`
	AssetURL    string    `json:"asset_url,omitempty"`
	AssetSize   int64     `json:"asset_size,omitempty"`
	ChecksumURL string    `json:"checksum_url,omitempty"`
}

// AssetNameFor 返回当前平台期望的发布文件名。
func AssetNameFor() string {
	return "panelvps-" + runtime.GOOS + "-" + runtime.GOARCH
}

// Check 查询最新 Release 并与当前版本比较。
func Check(ctx context.Context, repo, current string) (*Info, error) {
	if repo == "" {
		return nil, errors.New("未配置更新仓库")
	}
	api := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "PanelVPS-selfupdate")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("仓库不存在或没有发布 Release")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 失败: %w", err)
	}

	info := &Info{
		Current:     current,
		Latest:      strings.TrimPrefix(rel.Tag, "v"),
		Notes:       clip(rel.Body, 4000),
		PublishedAt: rel.PublishedAt,
	}
	want := AssetNameFor()
	for _, a := range rel.Assets {
		switch {
		case a.Name == want:
			info.AssetName = a.Name
			info.AssetURL = a.URL
			info.AssetSize = a.Size
		case a.Name == "checksums.txt" || a.Name == "SHA256SUMS":
			info.ChecksumURL = a.URL
		}
	}
	info.Available = compareVersion(info.Latest, current) > 0 && info.AssetURL != ""
	if info.Available && info.AssetName == "" {
		info.Available = false
	}
	return info, nil
}

// compareVersion 比较形如 1.2.3 的版本号，返回 -1/0/1。
func compareVersion(a, b string) int {
	pa := splitVersion(a)
	pb := splitVersion(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// Download 下载二进制到 dest（临时文件）。
func Download(ctx context.Context, assetURL, dest string) error {
	u, err := url.Parse(assetURL)
	if err != nil || u.Scheme != "https" {
		return errors.New("下载地址必须是 https")
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com") && !strings.HasSuffix(host, ".github.com") {
		return fmt.Errorf("拒绝从非 GitHub 域名下载: %s", host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "PanelVPS-selfupdate")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 %s", resp.Status)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	const maxSize = 64 << 20
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxSize+1))
	cerr := f.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if n > maxSize {
		return errors.New("二进制超过 64MB，已中止")
	}
	return nil
}

// ChecksumSkip 表示校验和拿不到（下载失败、或文件里没有该条目）。
// 这不是安全错误：来源仍是 GitHub 的 HTTPS，调用方可以降级为「跳过校验」，
// 只在哈希算出来对不上时才必须中止。
type ChecksumSkip struct{ Reason string }

func (e *ChecksumSkip) Error() string { return e.Reason }

// parseChecksum 从一行校验和里取 (hash, 文件名)。
//
// 实际撞到过的写法都得认：
//
//	<hash>  panelvps-linux-amd64      GNU 文本模式
//	<hash> *panelvps-linux-amd64      GNU 二进制模式（Windows 上的 sha256sum）
//	<hash>  bin/panelvps-linux-amd64  带任意目录前缀
//	SHA256 (panelvps-linux-amd64) = <hash>   BSD / shasum
func parseChecksum(line string) (hash, name string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if i := strings.Index(line, "("); i > 0 {
		if j := strings.Index(line[i:], ") = "); j > 0 {
			h := strings.ToLower(strings.TrimSpace(line[i+j+4:]))
			n := strings.TrimSpace(line[i+1 : i+j])
			if isHexSum(h) && n != "" {
				return h, checksumBase(n), true
			}
		}
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	h := strings.ToLower(fields[0])
	if !isHexSum(h) {
		return "", "", false
	}
	// 文件名理论上可以带空格，所以从第二个字段一直拼到行尾。
	return h, checksumBase(strings.Join(fields[1:], " ")), true
}

// checksumBase 去掉 sha256sum 可能带上的 * 标记与目录前缀，只留基名。
func checksumBase(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "*")
	name = strings.TrimPrefix(name, "./")
	return path.Base(name)
}

// isHexSum 判断是不是 64 位十六进制。
func isHexSum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// Verify 用校验和文件核对下载到的二进制。
//
// 返回值 semantics：verified=true 表示核对通过；false + nil 表示没有校验和可用。
// 返回 *ChecksumSkip 时调用方可以降级跳过，只有哈希不匹配才是必须中止的错误。
func Verify(ctx context.Context, checksumURL, assetName, filePath string) (bool, error) {
	if checksumURL == "" {
		return false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "PanelVPS-selfupdate")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &ChecksumSkip{Reason: "校验和文件下载失败（" + resp.Status + "）"}
	}

	want := ""
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		if h, name, ok := parseChecksum(sc.Text()); ok && name == assetName {
			want = h
			break
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	if want == "" {
		return false, &ChecksumSkip{Reason: "校验和文件里没有 " + assetName}
	}

	f, err := os.Open(filePath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return false, fmt.Errorf("sha256 不匹配：期望 %s，实际 %s", want, got)
	}
	return true, nil
}

// Apply 原子替换目标二进制，保留上一版。仅 Linux 可用。
func Apply(newBin, target string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", ErrUnsupported
	}
	return applyTo(newBin, target)
}

// applyTo 是 Apply 的实现，单独拆出来方便在开发机上跑测试。
func applyTo(newBin, target string) (string, error) {
	raw, err := os.ReadFile(newBin)
	if err != nil {
		return "", err
	}
	if len(raw) < 4 || string(raw[:4]) != "\x7fELF" {
		return "", errors.New("下载的文件不是 Linux ELF 可执行文件")
	}

	// 目标可能是个软链（/usr/local/bin/panelvps -> /opt/...），
	// 换掉软链本身会把布局弄坏，先落到真实路径上。
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = real
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	// 先把新二进制落到目标目录里再 rename。下载的临时文件在 /tmp，而 /tmp 常常是
	// tmpfs，与 /opt 不是同一个文件系统，跨设备 rename 会直接返回
	// EXDEV（invalid cross-device link）。同目录内的 rename 才是原子的。
	staged := filepath.Join(dir, "."+filepath.Base(target)+".new-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := writeExec(staged, raw); err != nil {
		return "", fmt.Errorf("写入新版本失败（%s 是否可写？）: %w", dir, err)
	}
	defer os.Remove(staged) // 成功后它已被 rename 走，这里删的是不存在的路径，忽略

	backup := target + ".old-" + time.Now().Format("20060102-150405")
	if _, err := os.Stat(target); err == nil {
		// 用硬链接留底：同一 inode，瞬间完成，且不会出现「目标文件短暂消失」的窗口
		if err := os.Link(target, backup); err != nil {
			if err := copyFile(target, backup); err != nil {
				return "", fmt.Errorf("备份旧版本失败: %w", err)
			}
		}
	}
	if err := os.Rename(staged, target); err != nil {
		return "", fmt.Errorf("替换二进制失败: %w", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return backup, err
	}
	return backup, nil
}

// copyFile 在硬链接不可用时复制一份，权限照抄源文件。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// writeExec 落一个可执行文件并刷盘，避免掉电后拿到半截二进制。
func writeExec(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return os.Chmod(path, 0o755)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
