//go:build linux

package metrics

import "golang.org/x/sys/unix"

// diskUsage 返回指定路径所在文件系统的总量与已用量（字节）。
// 已用量按「总量 - 非特权用户可用量」计算，与实际 df 的口径一致。
func diskUsage(path string) (total, used uint64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	free := uint64(st.Bavail) * bs
	if total > free {
		used = total - free
	}
	return total, used
}
