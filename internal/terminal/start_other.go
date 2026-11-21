//go:build !linux && !darwin

package terminal

// Supported 报告当前平台是否提供 PTY。
func Supported() bool { return false }

// Start 在非 Unix 平台上一律拒绝，避免给出半可用的终端。
func Start(shell string, cols, rows uint16) (Session, error) {
	return nil, ErrUnsupported
}
