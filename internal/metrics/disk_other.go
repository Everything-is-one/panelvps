//go:build !linux

package metrics

// diskUsage 在非 Linux 平台返回零值。本地开发（Windows/macOS）时首页
// 会显示「系统指标不可用」，这是预期行为，不影响业务逻辑验证。
func diskUsage(path string) (total, used uint64) {
	return 0, 0
}
