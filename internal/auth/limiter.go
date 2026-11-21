package auth

import (
	"sync"
	"time"
)

// LoginLimiter 是进程内的登录失败计数器。
//
// 为什么不用 nftables 动态封禁：M0 阶段只需要挡住撞库，
// 进程内计数已经足够；内核级封禁（PRD S-D02/S-C05）留到 M2 与防火墙模块一起做。
// 放在进程内的代价是重启后计数清零——对单管理员面板可以接受。
type LoginLimiter struct {
	mu       sync.Mutex
	entries  map[string]*attempt
	maxFails int
	window   time.Duration
	block    time.Duration
}

type attempt struct {
	fails      int
	firstAt    time.Time
	blockedAt  time.Time
	blockUntil time.Time
}

// NewLoginLimiter 创建限流器：window 内失败 maxFails 次，封禁 block。
func NewLoginLimiter(maxFails int, window, block time.Duration) *LoginLimiter {
	return &LoginLimiter{
		entries:  make(map[string]*attempt),
		maxFails: maxFails,
		window:   window,
		block:    block,
	}
}

// Blocked 返回该来源还需被封禁多久（0 表示未封禁）。
func (l *LoginLimiter) Blocked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return 0
	}
	if e.blockUntil.After(time.Now()) {
		return time.Until(e.blockUntil)
	}
	return 0
}

// Fail 记录一次失败。返回 true 表示本次失败触发了封禁。
func (l *LoginLimiter) Fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	e, ok := l.entries[key]
	if !ok || now.Sub(e.firstAt) > l.window {
		e = &attempt{firstAt: now}
		l.entries[key] = e
	}
	e.fails++

	if e.fails >= l.maxFails {
		e.blockUntil = now.Add(l.block)
		e.fails = 0
		e.firstAt = now
		return true
	}
	return false
}

// Reset 在登录成功后清除计数。
func (l *LoginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// Sweep 清理过期条目，避免长期运行时 map 无限增长。
func (l *LoginLimiter) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	n := 0
	for k, e := range l.entries {
		stale := e.blockUntil.Before(now) && now.Sub(e.firstAt) > l.window
		if stale {
			delete(l.entries, k)
			n++
		}
	}
	return n
}
