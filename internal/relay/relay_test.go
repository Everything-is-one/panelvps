package relay

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// 测试里只关心转发是否真的通了、计数是否正确，不覆盖异常分支：
// 那些分支的失败模式（端口占用、目标不可达）由 last_error 呈现，
// 用真实网络去制造它们只会让测试变慢变脆。
func TestTCPForwardAndStats(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起回声服务失败: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	port := freePort(t)
	m := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Apply([]Rule{{
		ID: 1, Name: "test", Listen: "127.0.0.1", ListenPort: port,
		Target: echo.Addr().String(), Network: "tcp", Enabled: true,
	}})
	defer m.StopAll()

	conn := dialRetry(t, fmt.Sprintf("127.0.0.1:%d", port))
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("回显内容不对: %q", buf)
	}

	// 计数器是异步累加的，给一个极短的窗口让它落定。
	time.Sleep(100 * time.Millisecond)
	st := m.Snapshot()[1]
	if !st.Running {
		t.Fatal("规则应为运行中")
	}
	if st.Up < 5 || st.Down < 5 {
		t.Fatalf("流量计数未累计: up=%d down=%d", st.Up, st.Down)
	}
	if st.Total < 1 {
		t.Fatalf("连接数未累计: %d", st.Total)
	}
}

func TestDisabledRuleDoesNotListen(t *testing.T) {
	port := freePort(t)
	m := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Apply([]Rule{{
		ID: 2, Name: "off", Listen: "127.0.0.1", ListenPort: port,
		Target: "127.0.0.1:9", Network: "tcp", Enabled: false,
	}})
	defer m.StopAll()

	if _, ok := m.Snapshot()[2]; ok {
		t.Fatal("停用的规则不应该出现在运行表里")
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		c.Close()
		t.Fatal("停用的规则不应监听端口")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func dialRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	var lastErr error
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return c
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("连接 %s 失败: %v", addr, lastErr)
	return nil
}
