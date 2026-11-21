package filemgr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TrashItem 是回收站里的一条记录。
type TrashItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Original  string    `json:"original"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	DeletedAt time.Time `json:"deleted_at"`
}

// Trash 是数据目录下的回收站。删除的文件先落到这里，可恢复也可彻底清除。
//
// 目录结构：<trash>/<id>/payload（原文件或目录）+ <trash>/<id>/meta.json。
// 每个条目独占一个目录，重名不会互相覆盖，恢复时按 meta 里的原路径放回。
type Trash struct {
	dir string
}

// NewTrash 创建回收站目录。
func NewTrash(dir string) (*Trash, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("回收站目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建回收站目录失败: %w", err)
	}
	return &Trash{dir: dir}, nil
}

// Dir 返回回收站根目录。
func (t *Trash) Dir() string { return t.dir }

// Put 把 abs 移入回收站，original 是相对文件根目录的路径。
// 跨分区（EXDEV）时退化为「复制 + 删除」，仍保证原路径消失。
func (t *Trash) Put(abs, original string) (TrashItem, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return TrashItem{}, err
	}

	id := time.Now().UTC().Format("20060102-150405") + "-" + randHex(3)
	itemDir := filepath.Join(t.dir, id)
	if err := os.MkdirAll(itemDir, 0o700); err != nil {
		return TrashItem{}, fmt.Errorf("创建回收站条目失败: %w", err)
	}
	payload := filepath.Join(itemDir, "payload")
	if err := movePath(abs, payload); err != nil {
		_ = os.RemoveAll(itemDir)
		return TrashItem{}, err
	}

	item := TrashItem{
		ID:        id,
		Name:      filepath.Base(abs),
		Original:  strings.TrimPrefix(strings.ReplaceAll(original, "\\", "/"), "/"),
		IsDir:     fi.IsDir(),
		DeletedAt: time.Now().UTC(),
	}
	switch {
	case fi.Mode().IsRegular():
		item.Size = fi.Size()
	case fi.IsDir():
		item.Size = dirSize(payload)
	}
	if err := writeMeta(itemDir, item); err != nil {
		return TrashItem{}, err
	}
	return item, nil
}

// List 按删除时间倒序返回回收站内容。
func (t *Trash) List() ([]TrashItem, error) {
	ents, err := os.ReadDir(t.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]TrashItem, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		it, err := readMeta(filepath.Join(t.dir, e.Name()))
		if err != nil {
			continue // 半途中断的条目直接略过，不拖垮整个列表
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	return out, nil
}

// Get 读取单条记录。
func (t *Trash) Get(id string) (TrashItem, error) {
	if err := validID(id); err != nil {
		return TrashItem{}, err
	}
	return readMeta(filepath.Join(t.dir, id))
}

// Restore 把条目放回原路径（相对文件根目录）。目标已存在时报 ErrExists，不覆盖。
func (t *Trash) Restore(id, root string) (TrashItem, error) {
	item, err := t.Get(id)
	if err != nil {
		return TrashItem{}, err
	}
	rel := strings.TrimPrefix(strings.ReplaceAll(item.Original, "\\", "/"), "/")
	clean := path.Clean(rel)
	if clean == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return TrashItem{}, ErrOutsideRoot
	}
	dst := filepath.Join(root, filepath.FromSlash(clean))
	// 双保险：meta.json 可能被手工改过，落点必须仍在文件根目录内
	if r, err := filepath.Rel(root, dst); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return TrashItem{}, ErrOutsideRoot
	}
	if _, err := os.Lstat(dst); err == nil {
		return TrashItem{}, ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return TrashItem{}, fmt.Errorf("重建上级目录失败: %w", err)
	}
	if err := movePath(filepath.Join(t.dir, id, "payload"), dst); err != nil {
		return TrashItem{}, err
	}
	_ = os.RemoveAll(filepath.Join(t.dir, id))
	return item, nil
}

// Purge 彻底删除一个条目。
func (t *Trash) Purge(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(t.dir, id)); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(t.dir, id))
}

// Empty 清空回收站。
func (t *Trash) Empty() (int, error) {
	ents, err := os.ReadDir(t.dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(t.dir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}

// Sweep 删除超过 maxAge 的条目，返回清理数量。启动时跑一次即可。
func (t *Trash) Sweep(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	items, err := t.List()
	if err != nil {
		return 0
	}
	cut := time.Now().Add(-maxAge)
	n := 0
	for _, it := range items {
		if it.DeletedAt.Before(cut) {
			if err := t.Purge(it.ID); err == nil {
				n++
			}
		}
	}
	return n
}

// ---------- 内部工具 ----------

func validID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return errors.New("非法的回收站条目编号")
	}
	return nil
}

func writeMeta(itemDir string, it TrashItem) error {
	raw, err := json.Marshal(it)
	if err != nil {
		return err
	}
	tmp := filepath.Join(itemDir, "meta.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(itemDir, "meta.json"))
}

func readMeta(itemDir string) (TrashItem, error) {
	raw, err := os.ReadFile(filepath.Join(itemDir, "meta.json"))
	if err != nil {
		return TrashItem{}, err
	}
	var it TrashItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return TrashItem{}, err
	}
	if it.ID == "" {
		it.ID = filepath.Base(itemDir)
	}
	return it, nil
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// movePath 优先用 rename（同一分区内是原子的），跨分区再退回复制。
func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyAny(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyAny 保留符号链接本身（不跟随），用于跨分区搬运。
func copyAny(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case fi.IsDir():
		if err := os.MkdirAll(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		items, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := copyAny(filepath.Join(src, it.Name()), filepath.Join(dst, it.Name())); err != nil {
				return err
			}
		}
		return nil
	case fi.Mode().IsRegular():
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
		if err := out.Close(); err != nil {
			return err
		}
		_ = os.Chtimes(dst, fi.ModTime(), fi.ModTime())
		return nil
	default:
		return ErrNotRegular
	}
}
