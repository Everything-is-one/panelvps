package xray

import (
	"os"
	"path/filepath"
	"sync"
)

// logSink 把 Xray 子进程的 stdout / stderr 追加到文件，超过 limit 轮转一份 .1。
//
// 为什么需要它：面板是 fork 出 `xray run` 子进程，不是 systemd 单元，
// 所以 journald 里没有它的任何记录 —— 日志页的「Xray 内核」源读的是
// `journalctl -u xray`，在面板托管的场景下永远是空的。用户排查
// 「客户端连不上」时，服务端一个字都看不到，只能猜。
//
// 失败策略：写盘失败不能拖垮 Xray，丢弃日志即可（返回成功）。
type logSink struct {
	path  string
	limit int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newLogSink(path string, limit int64) *logSink {
	if limit <= 0 {
		limit = 5 << 20
	}
	return &logSink{path: path, limit: limit}
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil {
		if err := s.open(); err != nil {
			return len(p), nil
		}
	}
	if s.size > 0 && s.size+int64(len(p)) > s.limit {
		s.rotate()
	}
	n, err := s.f.Write(p)
	s.size += int64(n)
	if err != nil {
		return len(p), nil
	}
	return n, nil
}

// Close 关闭文件句柄，Manager.Stop 时调用。
func (s *logSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

func (s *logSink) open() error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	s.f = f
	s.size = st.Size()
	return nil
}

// rotate 保留一份 .1，够排查用，也不会把磁盘写满。
func (s *logSink) rotate() {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	_ = os.Rename(s.path, s.path+".1")
	s.size = 0
	_ = s.open()
}
