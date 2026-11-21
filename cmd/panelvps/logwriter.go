package main

import (
	"os"
	"sync"
)

// rotatingWriter 是给 slog 用的最小文件写入器：按大小轮转，只保留一份旧的。
//
// 为什么面板要自己留一份日志：服务单元带 ProtectSystem 沙箱，在部分容器里
// journalctl -u panelvps 从沙箱内部读不到本单元的记录，日志页会显示
// journalctl 自己那行 "-- No entries --"。写到数据目录下就不依赖 journald 了。
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func newRotatingWriter(path string, max int64) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, max: max}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = fi.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.max {
		w.f.Close()
		_ = os.Rename(w.path, w.path+".1")
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
