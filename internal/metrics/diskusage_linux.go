//go:build linux

package metrics

import (
	"io/fs"
	"os"
	"syscall"
)

// deviceOf 返回路径所在文件系统的设备号，用于「不跨文件系统」判断。
func deviceOf(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

// sameDevice 判断目录项是否与扫描根在同一文件系统上。
// /proc、/sys、tmpfs 以及挂在子目录下的数据盘都会返回 false，
// 与 du -x 的口径一致：那些空间不属于这棵树的账。
func sameDevice(rootDev uint64, de fs.DirEntry) bool {
	if rootDev == 0 {
		return true
	}
	fi, err := de.Info()
	if err != nil {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return uint64(st.Dev) == rootDev
}

// fileBlocks 返回文件实际占用的磁盘空间（块数 × 512），与 du 同口径。
// 稀疏文件不会被算成逻辑大小那么满。
func fileBlocks(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Blocks) * 512
	}
	if fi.Size() > 0 {
		return uint64(fi.Size())
	}
	return 0
}
