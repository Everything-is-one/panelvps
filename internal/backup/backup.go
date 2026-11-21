// Package backup 负责面板数据的打包、列举与受控恢复。
//
// 关键约束：
//   - 备份文件名由本包生成，恢复时只接受符合该格式的名字，避免路径穿越；
//   - 数据库通过 VACUUM INTO 取一致性快照，而不是直接拷 .db 文件；
//   - 恢复是显式的高危操作：先做安全备份，再校验归档里的 SQLite 头，
//     最后替换文件并提示必须重启（运行中的进程仍持有旧句柄）。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrInvalidName 表示备份名不合法。
var ErrInvalidName = errors.New("备份名不合法")

var nameRe = regexp.MustCompile(`^panelvps-\d{8}-\d{6}(-[a-z0-9]{1,16})?\.tar\.gz$`)

// File 是备份文件信息。
type File struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Note    string    `json:"note,omitempty"`
}

// Manager 绑定备份目录与源数据目录。
type Manager struct {
	Dir     string
	DataDir string
}

// New 创建 Manager 并确保目录存在。
func New(dir, dataDir string) (*Manager, error) {
	if dir == "" || dataDir == "" {
		return nil, errors.New("备份目录与数据目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{Dir: dir, DataDir: dataDir}, nil
}

// Create 生成备份。
//
// snapshot 由调用方提供，负责把数据库快照与配置写进临时目录；
// 这样 backup 包不需要依赖 store，也不会误拷 WAL 中间态。
func (m *Manager) Create(ctx context.Context, note string, snapshot func(ctx context.Context, dir string) error) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(m.DataDir, ".backup-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	if err := snapshot(ctx, staging); err != nil {
		return nil, fmt.Errorf("生成数据快照失败: %w", err)
	}
	if note != "" {
		_ = os.WriteFile(filepath.Join(staging, "NOTE.txt"), []byte(note+"\n"), 0o600)
	}

	name := "panelvps-" + time.Now().Format("20060102-150405") + ".tar.gz"
	dest := filepath.Join(m.Dir, name)
	if _, err := os.Stat(dest); err == nil {
		// 同一秒内重复触发：加后缀，不覆盖旧备份
		name = "panelvps-" + time.Now().Format("20060102-150405") + "-dup.tar.gz"
		dest = filepath.Join(m.Dir, name)
	}

	tmp, err := os.CreateTemp(m.Dir, ".panelvps-backup-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	if err := addTree(tw, staging, ""); err != nil {
		tw.Close()
		gz.Close()
		tmp.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		tmp.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return nil, err
	}

	fi, err := os.Stat(dest)
	if err != nil {
		return nil, err
	}
	return &File{Name: name, Size: fi.Size(), ModTime: fi.ModTime(), Note: note}, nil
}

func addTree(tw *tar.Writer, root, prefix string) error {
	return filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		if fi.IsDir() {
			return tw.WriteHeader(&tar.Header{
				Name: name + "/", Mode: 0o700, Typeflag: tar.TypeDir,
				ModTime: fi.ModTime(),
			})
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Mode = 0o600
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
}

// List 返回备份列表，新的在前。
func (m *Manager) List() ([]File, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !nameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Resolve 校验名字并返回绝对路径。
func (m *Manager) Resolve(name string) (string, error) {
	if !nameRe.MatchString(name) || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return "", ErrInvalidName
	}
	return filepath.Join(m.Dir, name), nil
}

// Delete 删除备份。
func (m *Manager) Delete(name string) error {
	p, err := m.Resolve(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Extract 把备份展开到目标目录，逐条校验路径。
func (m *Manager) Extract(name, destDir string) error {
	src, err := m.Resolve(name)
	if err != nil {
		return err
	}
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

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	count, total := 0, int64(0)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		count++
		if count > 20000 {
			return errors.New("备份条目过多，疑似异常文件")
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, 512<<20))
			out.Close()
			if err != nil {
				return err
			}
			total += n
			if total > 1<<30 {
				return errors.New("备份展开后超过 1GB，已中止")
			}
		case tar.TypeSymlink, tar.TypeLink:
			continue
		}
	}
	return nil
}

func safeJoin(root, name string) (string, error) {
	// 统一分隔符后再判断，避免 Windows 下 "\abs" 被 filepath.IsAbs 漏判
	normalized := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(normalized, "/") {
		return "", ErrInvalidName
	}
	clean := filepath.Clean(filepath.FromSlash(normalized))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrInvalidName
	}
	abs := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrInvalidName
	}
	return abs, nil
}

// Prune 只保留最近 keep 份，返回删除数量。
func (m *Manager) Prune(keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	files, err := m.List()
	if err != nil {
		return 0, err
	}
	removed := 0
	for i := keep; i < len(files); i++ {
		if err := m.Delete(files[i].Name); err == nil {
			removed++
		}
	}
	return removed, nil
}

// LooksLikeSQLite 校验文件头，防止把非数据库文件恢复成 panel.db。
func LooksLikeSQLite(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, _ := io.ReadFull(f, buf)
	return n == 16 && string(buf) == "SQLite format 3\x00"
}
