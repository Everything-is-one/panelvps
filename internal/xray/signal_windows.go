//go:build windows

package xray

import "os"

// Windows 没有 SIGTERM，Kill 是唯一可靠的方式。
// 面板只在开发机跑 Windows，生产目标是 Linux。
func terminateSignal() os.Signal { return os.Kill }
