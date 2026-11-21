// Package terminal 提供 Web 终端所需的 PTY 会话。
//
// 终端默认关闭（config.terminal_enabled），因为它等价于给登录用户一个
// root shell。开启后仍然只有已登录管理员可用，且每次打开都会写审计。
package terminal

import (
	"errors"
	"io"
)

// ErrUnsupported 表示当前平台没有 PTY 实现。
var ErrUnsupported = errors.New("当前系统不支持 Web 终端（仅 Linux/macOS）")

// Session 是一个伪终端会话。
type Session interface {
	io.ReadWriteCloser
	// Resize 调整终端尺寸；部分平台可能返回 ErrUnsupported。
	Resize(cols, rows uint16) error
	// Wait 等待子进程退出。
	Wait() error
}
