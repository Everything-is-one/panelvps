// Package notify 把关键事件推送到外部 Webhook。
//
// 只支持一个通用 Webhook（POST JSON），不绑定具体 IM 厂商：
// 需要企业微信/钉钉时，中间加一层转换即可。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrInvalidURL 表示 Webhook 地址不合法。
var ErrInvalidURL = errors.New("Webhook 地址必须是 http(s) URL")

// Event 是推送内容。
type Event struct {
	Level   string    `json:"level"` // info | warn | error
	Title   string    `json:"title"`
	Detail  string    `json:"detail"`
	Host    string    `json:"host"`
	Time    time.Time `json:"time"`
	Version string    `json:"version,omitempty"`
}

// Notifier 负责去重与发送。
type Notifier struct {
	url    string
	logger *slog.Logger
	client *http.Client

	mu   sync.Mutex
	last map[string]time.Time
}

// New 创建通知器。url 为空时所有发送都是空操作。
func New(rawURL string, logger *slog.Logger) (*Notifier, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, ErrInvalidURL
		}
	}
	return &Notifier{
		url:    rawURL,
		logger: logger,
		client: &http.Client{Timeout: 8 * time.Second},
		last:   map[string]time.Time{},
	}, nil
}

// Enabled 报告是否配置了 Webhook。
func (n *Notifier) Enabled() bool { return n != nil && n.url != "" }

// Send 异步推送，5 分钟内相同标题只发一次，避免告警风暴。
func (n *Notifier) Send(ev Event) {
	if !n.Enabled() {
		return
	}
	n.mu.Lock()
	if t, ok := n.last[ev.Title]; ok && time.Since(t) < 5*time.Minute {
		n.mu.Unlock()
		return
	}
	n.last[ev.Title] = time.Now()
	n.mu.Unlock()

	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		body, err := json.Marshal(ev)
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "PanelVPS")
		resp, err := n.client.Do(req)
		if err != nil {
			n.logger.Warn("告警推送失败", "err", err, "title", ev.Title)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			n.logger.Warn("告警推送被拒绝", "status", resp.StatusCode, "title", ev.Title)
		}
	}()
}
