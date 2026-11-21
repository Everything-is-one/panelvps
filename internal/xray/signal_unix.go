//go:build !windows

package xray

import (
	"os"
	"syscall"
)

// terminateSignal 让 Xray 有机会写完日志再退出。
func terminateSignal() os.Signal { return syscall.SIGTERM }
