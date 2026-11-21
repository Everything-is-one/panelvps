package metrics

import "errors"

// ErrProcUnavailable 表示内核没暴露 /proc（或读取失败），
// 与「平台不支持」区分开：容器里 /proc 可能被裁掉一部分。
var ErrProcUnavailable = errors.New("读取 /proc 失败，无法获取进程排行")

// TopProc 是资源排行里的一项。CPU 是两次采样之间的瞬时占用，
// 与 top 的 %CPU 口径一致；RSS 是当前常驻内存。
type TopProc struct {
	PID    int     `json:"pid"`
	Name   string  `json:"name"`
	CPUPct float64 `json:"cpu_pct"`
	RSS    uint64  `json:"rss"`
	MemPct float64 `json:"mem_pct"`
}

// MountUsage 是一个挂载点的占用情况。
type MountUsage struct {
	Path   string  `json:"path"`
	Device string  `json:"device"`
	Fstype string  `json:"fstype"`
	Total  uint64  `json:"total"`
	Used   uint64  `json:"used"`
	Pct    float64 `json:"pct"`
}
