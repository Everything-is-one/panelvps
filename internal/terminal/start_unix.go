//go:build linux || darwin

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

// Supported 报告当前平台是否提供 PTY。
func Supported() bool { return true }

// Start 启动一个交互式 shell 并绑定到新的 PTY。
//
// shell 必须是绝对路径，且不允许携带参数——避免把配置项变成命令注入入口。
func Start(shell string, cols, rows uint16) (Session, error) {
	if !filepath.IsAbs(shell) {
		return nil, fmt.Errorf("shell 必须是绝对路径: %q", shell)
	}
	if strings.ContainsAny(shell, " \t\n;&|$`") {
		return nil, fmt.Errorf("shell 路径含非法字符: %q", shell)
	}
	if st, err := os.Stat(shell); err != nil || st.IsDir() {
		return nil, fmt.Errorf("shell 不可用: %s", shell)
	}

	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=C.UTF-8",
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 32
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})

	return &session{cmd: cmd, ptmx: ptmx}, nil
}

type session struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func (s *session) Read(p []byte) (int, error)  { return s.ptmx.Read(p) }
func (s *session) Write(p []byte) (int, error) { return s.ptmx.Write(p) }

func (s *session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

func (s *session) Close() error {
	// 先关 PTY 再杀进程组，避免 shell 变成孤儿继续占资源
	_ = s.ptmx.Close()
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_, _ = s.cmd.Process.Wait()
	}
	return nil
}

func (s *session) Wait() error { return s.cmd.Wait() }
