// Package jobs 提供内存态的异步任务执行器。
//
// 用途：apt 安装、证书签发、备份打包这类可能跑几分钟的操作不能占用
// 一个 HTTP 请求（服务器 WriteTimeout 只有 30 秒）。任务只保留最近若干条，
// 进程重启即丢失——这些操作本身有系统侧的最终状态（dpkg/文件），
// 不需要持久化队列。
package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Status 是任务状态。
type Status string

const (
	StatusRunning Status = "running"
	StatusOK      Status = "ok"
	StatusFailed  Status = "failed"
)

// Job 是一次异步操作。
type Job struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`
	Status     Status    `json:"status"`
	Log        string    `json:"log"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Manager 管理任务生命周期。
type Manager struct {
	mu    sync.Mutex
	seq   uint64
	jobs  map[string]*Job
	order []string
	max   int
	run   int
	limit int
}

// New 创建 Manager。keep 为保留的历史任务数，concurrent 为并发上限。
func New(keep, concurrent int) *Manager {
	if keep <= 0 {
		keep = 50
	}
	if concurrent <= 0 {
		concurrent = 2
	}
	return &Manager{jobs: map[string]*Job{}, max: keep, limit: concurrent}
}

// ErrBusy 表示并发任务已达上限。
var ErrBusy = errors.New("已有任务在运行，请稍后再试")

// Start 启动任务。fn 通过 log 回调追加输出。
func (m *Manager) Start(kind, target string, fn func(ctx context.Context, log func(string)) error) (*Job, error) {
	m.mu.Lock()
	if m.run >= m.limit {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.seq++
	id := fmt.Sprintf("%s-%d-%d", kind, time.Now().Unix(), m.seq)
	j := &Job{ID: id, Kind: kind, Target: target, Status: StatusRunning, StartedAt: time.Now()}
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.run++
	m.evictLocked()
	snap := *j
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			m.run--
			j.FinishedAt = time.Now()
			if j.Status == StatusRunning {
				j.Status = StatusOK
			}
			m.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		err := fn(ctx, func(s string) {
			m.mu.Lock()
			// 单条任务日志上限 256KB，避免长输出吃内存
			if len(j.Log) < 256<<10 {
				j.Log += s + "\n"
			}
			m.mu.Unlock()
		})

		m.mu.Lock()
		if err != nil {
			j.Status = StatusFailed
			j.Error = err.Error()
		} else {
			j.Status = StatusOK
		}
		m.mu.Unlock()
	}()

	return &snap, nil
}

// Snapshot 返回任务副本，避免调用方读到并发写入的字段。
func (j *Job) Snapshot() *Job {
	c := *j
	return &c
}

// Get 查询任务。
func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, false
	}
	return j.Snapshot(), true
}

// List 返回最近任务，新的在前。
func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if j, ok := m.jobs[m.order[i]]; ok {
			out = append(out, j.Snapshot())
		}
	}
	return out
}

func (m *Manager) evictLocked() {
	for len(m.order) > m.max {
		oldest := m.order[0]
		m.order = m.order[1:]
		// 只清理已结束的任务，运行中的即使超龄也保留
		if j, ok := m.jobs[oldest]; ok && j.Status == StatusRunning {
			m.order = append(m.order, oldest)
			break
		}
		delete(m.jobs, oldest)
	}
}
