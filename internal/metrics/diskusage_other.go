//go:build !linux

package metrics

import (
	"io/fs"
	"os"
)

// 非 Linux 只用于本地开发调试：不做设备号判断，按逻辑大小统计。
func deviceOf(string) uint64 { return 0 }

func sameDevice(uint64, fs.DirEntry) bool { return true }

func fileBlocks(fi os.FileInfo) uint64 {
	if fi.Size() > 0 {
		return uint64(fi.Size())
	}
	return 0
}
