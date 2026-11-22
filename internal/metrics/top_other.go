//go:build !linux

package metrics

import "errors"

// 非 Linux 平台没有 /proc，概览的资源排行不可用。
var errTopUnsupported = errors.New("当前系统不支持进程排行（仅 Linux）")

func (c *Collector) TopProcesses(kind string, limit int) ([]TopProc, error) {
	return nil, errTopUnsupported
}

// ProcessTotals 在非 Linux 平台没有 /proc 可读。
func (c *Collector) ProcessTotals() (uint64, int) { return 0, 0 }

func TopMounts(limit int) ([]MountUsage, error) {
	return nil, errTopUnsupported
}
