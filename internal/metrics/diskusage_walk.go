package metrics

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// scanTree 遍历 root 下的一级子项，累计每个子项整棵子树的实际占用。
//
// 用 WalkDir 而不是 du：不需要外部命令（最小化镜像里可能没有 coreutils），
// 且能随时响应 context 取消。代价是 Go 的 lstat 比 fts 略慢，
// 所以扫描放在后台任务里，前端轮询进度。
//
// 文件系统相关的三件事（设备号、占用块）由 platform 文件提供，
// 这样非 Linux 也能跑通（本地开发时用来验证界面）。
func scanTree(ctx context.Context, j *UsageJob) (*UsageReport, error) {
	root := j.path
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return nil, errors.New("该路径不是目录")
	}
	rootDev := deviceOf(root)

	started := time.Now()
	agg := make(map[string]*DirUsage, 32)
	var total uint64
	var files int
	var scanned int64
	partial := false
	sep := string(filepath.Separator)

	walkErr := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			partial = true
			return filepath.SkipAll
		}
		if err != nil {
			// 权限不足、扫描途中被删除：跳过这一项，别让整次扫描失败
			if de != nil && de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		first := rel
		if i := strings.Index(rel, sep); i > 0 {
			first = rel[:i]
		}

		typ := de.Type()
		// 符号链接本身几乎不占空间，且 WalkDir 不会跟进去，直接跳过
		if typ&fs.ModeSymlink != 0 {
			return nil
		}
		if de.IsDir() {
			// 不跨文件系统：别的设备上的目录不属于这棵树的账
			if !sameDevice(rootDev, de) {
				return filepath.SkipDir
			}
			return nil
		}
		if !typ.IsRegular() {
			return nil
		}
		fi, infoErr := de.Info()
		if infoErr != nil {
			return nil
		}
		size := fileBlocks(fi)

		item := agg[first]
		if item == nil {
			// rel 里出现分隔符，说明 first 是目录；否则是直接躺在
			// root 下的文件（如 /swapfile）。不能用 de.IsDir()：
			// 这里回调到的是叶子文件，父目录早被跳过了。
			item = &DirUsage{
				Path:  filepath.Join(root, first),
				Name:  first,
				IsDir: first != rel,
			}
			agg[first] = item
		}
		item.Size += size
		item.Files++
		total += size
		files++
		scanned++

		if scanned%250 == 0 {
			j.progress(1, size, filepath.Join(root, rel))
			if scanned >= usageMaxFiles {
				partial = true
				return filepath.SkipAll
			}
			return nil
		}
		j.progress(1, size, "")
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return nil, walkErr
	}

	items := make([]DirUsage, 0, len(agg))
	for _, it := range agg {
		items = append(items, *it)
	}
	sort.Slice(items, func(a, b int) bool {
		if items[a].Size != items[b].Size {
			return items[a].Size > items[b].Size
		}
		return items[a].Name < items[b].Name
	})

	return &UsageReport{
		Path:      root,
		Total:     total,
		Files:     files,
		Items:     items,
		Partial:   partial,
		ElapsedMS: time.Since(started).Milliseconds(),
		StartedAt: started,
	}, nil
}
