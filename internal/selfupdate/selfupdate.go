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

// Verify 用 checksums.txt 校验文件。checksumURL 为空时跳过并返回 false。
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
		return false, fmt.Errorf("下载校验和失败: %s", resp.Status)
	}

	want := ""
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return false, fmt.Errorf("校验和文件中没有 %s", assetName)
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

// Apply 原子替换目标二进制，保留上一版。
func Apply(newBin, target string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", ErrUnsupported
	}
	raw, err := os.ReadFile(newBin)
	if err != nil {
		return "", err
	}
	if len(raw) < 4 || string(raw[:4]) != "\x7fELF" {
		return "", errors.New("下载的文件不是 Linux ELF 可执行文件")
	}
	if err := os.Chmod(newBin, 0o755); err != nil {
		return "", err
	}

	backup := target + ".old-" + time.Now().Format("20060102-150405")
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("备份旧版本失败: %w", err)
		}
	}
	if err := os.Rename(newBin, target); err != nil {
		// 替换失败则把旧版本换回来
		_ = os.Rename(backup, target)
		return "", fmt.Errorf("替换二进制失败: %w", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return backup, err
	}
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	return backup, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
