// Package filemgr 提供受限于单一根目录的文件操作。
//
// 安全模型（PRD §14 S-B01）：
//  1. 所有路径先 Clean 再与根目录做 Rel，任何以 .. 开头的结果都被拒绝；
//  2. 目标本身或最近的已存在祖先会被 EvalSymlinks，符号链接指到根外同样被拒；
//  3. 写入走「同目录临时文件 + rename」，避免写一半留下损坏文件；
//  4. 解压时逐条校验条目路径与总展开体积，防 zip slip 与压缩炸弹。
//
// 这一层不关心 HTTP，也不做权限判断——调用方必须先通过鉴权。
package filemgr

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 常见错误，便于 HTTP 层映射状态码。
var (
	ErrOutsideRoot = errors.New("路径超出允许的根目录")
	ErrNotRegular  = errors.New("目标不是普通文件")
	ErrTooLarge    = errors.New("文件超过大小限制")
	ErrExists      = errors.New("目标已存在")
)

// Manager 是绑定到某个根目录的文件操作器。
type Manager struct {
	root     string
	MaxRead  int64
	MaxWrite int64
}

// Entry 是目录列表中的一项。
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"` // 相对根目录，始终用 / 分隔
	IsDir   bool      `json:"is_dir"`
	IsLink  bool      `json:"is_link"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	Target  string    `json:"target,omitempty"`
}

// New 创建 Manager。root 不存在时返回错误——不替用户猜测目录。
func New(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("文件根目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析根目录失败: %w", err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("根目录不可用: %w", err)
	}
	if !fi.IsDir() {
		return nil, errors.New("根目录不是目录")
	}
	return &Manager{root: abs, MaxRead: 4 << 20, MaxWrite: 512 << 20}, nil
}

// Root 返回解析后的绝对根目录。
func (m *Manager) Root() string { return m.root }

// Resolve 把用户输入的路径转换为受控的绝对路径。
func (m *Manager) Resolve(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" || rel == "/" || rel == "." {
		return m.root, nil
	}
	// 去掉前导 /，一律按「相对根目录」解释，避免暴露真实文件系统布局
	rel = strings.TrimPrefix(rel, "/")
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrOutsideRoot
	}

	abs := filepath.Join(m.root, filepath.FromSlash(clean))
	if !m.inside(abs) {
		return "", ErrOutsideRoot
	}

	// 目标存在时直接解析；不存在时解析最近的已存在祖先，
	// 这样 "link-to-outside/new.txt" 也会被拦住。
	probe := abs
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if probe == abs {
				if !m.inside(real) {
					return "", ErrOutsideRoot
				}
			} else {
				// 祖先被解析后，重新拼回剩余部分再校验
				suffix, relErr := filepath.Rel(probe, abs)
				if relErr != nil {
					return "", ErrOutsideRoot
				}
				if !m.inside(filepath.Join(real, suffix)) {
					return "", ErrOutsideRoot
				}
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return abs, nil
}

func (m *Manager) inside(p string) bool {
	rel, err := filepath.Rel(m.root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (m *Manager) rel(abs string) string {
	r, err := filepath.Rel(m.root, abs)
	if err != nil || r == "." {
		return ""
	}
	return filepath.ToSlash(r)
}

// Stat 返回单个路径的信息。
func (m *Manager) Stat(rel string) (*Entry, error) {
	abs, err := m.Resolve(rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	e := &Entry{
		Name:    filepath.Base(abs),
		Path:    m.rel(abs),
		IsDir:   fi.IsDir(),
		IsLink:  fi.Mode()&os.ModeSymlink != 0,
		Size:    fi.Size(),
		Mode:    fi.Mode().String(),
		ModTime: fi.ModTime(),
	}
	if e.IsLink {
		if target, err := os.Readlink(abs); err == nil {
			e.Target = target
		}
	}
	return e, nil
}

// List 返回目录内容，目录在前、同类按名称排序。
func (m *Manager) List(rel string, showHidden bool) ([]Entry, error) {
	abs, err := m.Resolve(rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("目标不是目录")
	}

	items, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		name := it.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		e := Entry{
			Name:    name,
			Path:    m.rel(filepath.Join(abs, name)),
			IsDir:   it.IsDir(),
			IsLink:  it.Type()&os.ModeSymlink != 0,
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			ModTime: info.ModTime(),
		}
		if e.IsLink {
			if target, err := os.Readlink(filepath.Join(abs, name)); err == nil {
				e.Target = target
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// SearchOptions 控制一次文件名搜索的边界。
//
// 搜索是在实时文件系统上遍历，没有索引，必须设上限：目录再深也就
// 翻 maxDepth 层，命中数到 limit 就停，遍历过的条目数也有硬上限。
// 否则一次在 / 上搜索就能把面板的 CPU 和磁盘 IO 全部占住。
type SearchOptions struct {
	Query    string
	Limit    int
	MaxDepth int
	// MaxVisited 是遍历条目总数上限，0 用默认值。
	MaxVisited int
}

// Search 在 rel 目录下按文件名包含关系搜索（大小写不敏感）。
// 返回 truncated 表示因为条数/深度/遍历量上限提前结束，结果不完整。
func (m *Manager) Search(ctx context.Context, rel string, opt SearchOptions) ([]Entry, bool, error) {
	base, err := m.Resolve(rel)
	if err != nil {
		return nil, false, err
	}
	fi, err := os.Stat(base)
	if err != nil {
		return nil, false, err
	}
	if !fi.IsDir() {
		return nil, false, errors.New("目标不是目录")
	}
	q := strings.ToLower(strings.TrimSpace(opt.Query))
	if q == "" {
		return nil, false, errors.New("请输入搜索关键词")
	}
	limit := opt.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	maxDepth := opt.MaxDepth
	if maxDepth <= 0 || maxDepth > 32 {
		maxDepth = 6
	}
	maxVisited := opt.MaxVisited
	if maxVisited <= 0 {
		maxVisited = 200000
	}

	var out []Entry
	visited := 0
	truncated := false

	walkErr := filepath.WalkDir(base, func(p string, de fs.DirEntry, walkErr error) error {
		if ctx != nil && ctx.Err() != nil {
			truncated = true
			return filepath.SkipAll
		}
		if walkErr != nil {
			if de != nil && de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == base {
			return nil
		}
		visited++
		if visited > maxVisited {
			truncated = true
			return filepath.SkipAll
		}

		relPath := m.rel(p)
		depth := strings.Count(filepath.ToSlash(relPath), "/")
		if de.IsDir() {
			// 目录不匹配也算命中（用户往往想跳到某个目录），
			// 但太深的子树不再往下翻。
			if depth >= maxDepth {
				if strings.Contains(strings.ToLower(de.Name()), q) && len(out) < limit {
					out = append(out, m.entryOf(p, de))
				}
				return filepath.SkipDir
			}
			if strings.Contains(strings.ToLower(de.Name()), q) && len(out) < limit {
				out = append(out, m.entryOf(p, de))
			}
			return nil
		}
		if !strings.Contains(strings.ToLower(de.Name()), q) {
			return nil
		}
		out = append(out, m.entryOf(p, de))
		if len(out) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return nil, truncated, walkErr
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path)
	})
	return out, truncated, nil
}

// entryOf 把 WalkDir 的条目转成 Entry（与 List 的字段口径一致）。
func (m *Manager) entryOf(abs string, de fs.DirEntry) Entry {
	info, err := de.Info()
	e := Entry{
		Name:   de.Name(),
		Path:   m.rel(abs),
		IsDir:  de.IsDir(),
		IsLink: de.Type()&fs.ModeSymlink != 0,
	}
	if err == nil {
		e.Size = info.Size()
		e.Mode = info.Mode().String()
		e.ModTime = info.ModTime()
	}
	if e.IsLink {
		if target, err := os.Readlink(abs); err == nil {
			e.Target = target
		}
	}
	return e
}

// Open 打开普通文件用于下载。调用方负责关闭。
func (m *Manager) Open(rel string) (*os.File, os.FileInfo, error) {
	abs, err := m.Resolve(rel)
	if err != nil {
		return nil, nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, nil, err
	}
	if fi.IsDir() {
		return nil, nil, errors.New("不能直接下载目录")
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// 解析后的目标已在 Resolve 阶段校验过，但读链接本身没有意义
		if fi, err = os.Stat(abs); err != nil {
			return nil, nil, err
		}
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, err
	}
	return f, fi, nil
}

// ReadAll 读取文本文件，超过 MaxRead 时拒绝，避免把内存打满。
func (m *Manager) ReadAll(rel string) ([]byte, error) {
	f, fi, err := m.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > m.MaxRead {
		return nil, ErrTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(f, m.MaxRead+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > m.MaxRead {
		return nil, ErrTooLarge
	}
	return raw, nil
}

// WriteStream 原子写入文件：临时文件 + rename。
func (m *Manager) WriteStream(rel string, r io.Reader, mode os.FileMode) (int64, error) {
	abs, err := m.Resolve(rel)
	if err != nil {
		return 0, err
	}
	if abs == m.root {
		return 0, errors.New("不能写入根目录")
	}
	if fi, err := os.Lstat(abs); err == nil {
		if fi.IsDir() {
			return 0, errors.New("目标是一个目录")
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return 0, errors.New("拒绝写入符号链接")
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	if mode == 0 {
		mode = 0o644
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), ".pv-upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	n, err := io.Copy(tmp, io.LimitReader(r, m.MaxWrite+1))
	if err != nil {
		return 0, err
	}
	if n > m.MaxWrite {
		return 0, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Chmod(mode); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return 0, err
	}
	return n, nil
}

// WriteFile 写入字节切片，便于小文件与测试。
func (m *Manager) WriteFile(rel string, data []byte, mode os.FileMode) error {
	_, err := m.WriteStream(rel, bytes.NewReader(data), mode)
	return err
}

// Mkdir 创建目录（含父级）。
func (m *Manager) Mkdir(rel string) error {
	abs, err := m.Resolve(rel)
	if err != nil {
		return err
	}
	if abs == m.root {
		return nil
	}
	return os.MkdirAll(abs, 0o755)
}

// Rename 重命名或移动。overwrite=false 时目标存在即报错。
func (m *Manager) Rename(from, to string, overwrite bool) error {
	src, err := m.Resolve(from)
	if err != nil {
		return err
	}
	dst, err := m.Resolve(to)
	if err != nil {
		return err
	}
	if src == m.root || dst == m.root {
		return errors.New("不能移动根目录")
	}
	if !overwrite {
		if _, err := os.Lstat(dst); err == nil {
			return ErrExists
		}
	}
	return os.Rename(src, dst)
}

// Remove 删除文件或目录（递归）。调用方必须先让用户二次确认。
func (m *Manager) Remove(rel string) error {
	abs, err := m.Resolve(rel)
	if err != nil {
		return err
	}
	if abs == m.root {
		return errors.New("拒绝删除根目录")
	}
	return os.RemoveAll(abs)
}

// Copy 复制文件或目录。
func (m *Manager) Copy(from, to string, overwrite bool) error {
	src, err := m.Resolve(from)
	if err != nil {
		return err
	}
	dst, err := m.Resolve(to)
	if err != nil {
		return err
	}
	if !overwrite {
		if _, err := os.Lstat(dst); err == nil {
			return ErrExists
		}
	}
	return copyPath(src, dst)
}

func copyPath(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// 只复制链接的目标内容，不复制链接本身，避免把根外路径带进来
		fi, err = os.Stat(src)
		if err != nil {
			return err
		}
	}
	if fi.IsDir() {
		if err := os.MkdirAll(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		items, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := copyPath(filepath.Join(src, it.Name()), filepath.Join(dst, it.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !fi.Mode().IsRegular() {
		return ErrNotRegular
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// packTargets 统一校验「要打包哪些路径」，返回绝对路径。
// 打包到服务器（Archive）与流式下载（WriteArchive）共用同一套规则：
// 拒绝根目录、拒绝根目录外、拒绝一次几百项把内存和 CPU 拖死。
func (m *Manager) packTargets(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("没有选择要压缩的文件")
	}
	if len(paths) > 200 {
		return nil, errors.New("一次最多打包 200 项，请分批操作")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := m.Resolve(p)
		if err != nil {
			return nil, err
		}
		if abs == m.root {
			return nil, errors.New("不能压缩根目录，请选择具体文件")
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

// CheckPack 只做校验，让 HTTP 层在写响应头之前就能报错。
func (m *Manager) CheckPack(paths []string) error {
	_, err := m.packTargets(paths)
	return err
}

// WriteArchive 把若干路径打成 tar.gz 直接写进 w，不落盘。
// 用于浏览器「打包下载」：服务器上不留临时文件，也不会因为
// 忘记清理而在 /tmp 里积累一堆压缩包。
func (m *Manager) WriteArchive(w io.Writer, paths []string) error {
	targets, err := m.packTargets(paths)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, abs := range targets {
		if err := addToTar(tw, abs, filepath.Base(abs)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Archive 把若干路径打成 tar.gz。dest 是压缩包路径（相对根目录）。
func (m *Manager) Archive(paths []string, dest string) error {
	targets, err := m.packTargets(paths)
	if err != nil {
		return err
	}
	dstAbs, err := m.Resolve(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstAbs), ".pv-archive-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	for _, abs := range targets {
		base := filepath.Base(abs)
		if err := addToTar(tw, abs, base); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dstAbs)
}

func addToTar(tw *tar.Writer, abs, name string) error {
	return filepath.Walk(abs, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		entryName := name
		if rel != "." {
			entryName = path.Join(name, filepath.ToSlash(rel))
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// 不打包链接，避免解压后指向根外
			return nil
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = entryName
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.IsDir() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Extract 解压 tar.gz / tgz / zip 到目标目录。
//
// 防御：拒绝绝对路径与 ..；限制条目数与总展开体积。
func (m *Manager) Extract(archivePath, destDir string) error {
	srcAbs, err := m.Resolve(archivePath)
	if err != nil {
		return err
	}
	dstAbs, err := m.Resolve(destDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstAbs, 0o755); err != nil {
		return err
	}

	lower := strings.ToLower(srcAbs)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(srcAbs, dstAbs)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"), strings.HasSuffix(lower, ".tar"):
		return extractTar(srcAbs, dstAbs)
	default:
		return errors.New("仅支持 .zip / .tar.gz / .tgz / .tar")
	}
}

const (
	maxArchiveEntries = 20000
	maxArchiveBytes   = 2 << 30 // 2GB
)

func safeJoin(root, name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
		return "", ErrOutsideRoot
	}
	abs := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	return abs, nil
}

func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > maxArchiveEntries {
		return errors.New("压缩包条目过多")
	}

	var total int64
	for _, f := range zr.File {
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return fmt.Errorf("压缩包包含非法路径 %q", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			continue // 不解出符号链接
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm()|0o600)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxArchiveBytes-total+1))
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
		total += n
		if total > maxArchiveBytes {
			return errors.New("压缩包展开后体积过大")
		}
	}
	return nil
}

func extractTar(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(strings.ToLower(src), ".tar") && !strings.HasSuffix(strings.ToLower(src), ".tar.gz") {
		// 纯 tar
	} else {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}

	tr := tar.NewReader(r)
	var total int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveEntries {
			return errors.New("压缩包条目过多")
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return fmt.Errorf("压缩包包含非法路径 %q", hdr.Name)
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
			n, err := io.Copy(out, io.LimitReader(tr, maxArchiveBytes-total+1))
			out.Close()
			if err != nil {
				return err
			}
			total += n
			if total > maxArchiveBytes {
				return errors.New("压缩包展开后体积过大")
			}
		case tar.TypeSymlink, tar.TypeLink:
			// 跳过链接，避免逃逸
			continue
		}
	}
	return nil
}
