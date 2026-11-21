// Package store 封装 SQLite 访问。
//
// 只用 database/sql + 纯 Go 驱动（modernc.org/sqlite），无需 cgo，
// 交叉编译到 arm64 不需要额外的工具链。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound 表示查询没有命中。
var ErrNotFound = errors.New("record not found")

// Store 是数据库句柄。
type Store struct {
	db *sql.DB
}

// User 是面板管理员账号。M0 只支持单管理员，但表结构保留多行能力。
type User struct {
	ID                 int64
	Username           string
	PasswordHash       string
	TOTPSecret         string
	TOTPEnabled        bool
	MustChangePassword bool
	CreatedAt          time.Time
	LastLoginAt        sql.NullTime
	LastLoginIP        string
}

// Session 是一次登录会话。Token 本身不落库，只存 SHA-256。
type Session struct {
	ID        int64
	UserID    int64
	IP        string
	UserAgent string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Open 打开数据库并完成迁移。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=cache_size(-8000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 单写者：把连接数收紧，避免 "database is locked"。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库不可用: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层句柄给需要直接查询的模块（如未来的审计导出）。
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    username              TEXT    NOT NULL UNIQUE,
    password_hash         TEXT    NOT NULL,
    totp_secret           TEXT    NOT NULL DEFAULT '',
    totp_enabled          INTEGER NOT NULL DEFAULT 0,
    must_change_password  INTEGER NOT NULL DEFAULT 0,
    created_at            INTEGER NOT NULL,
    last_login_at         INTEGER,
    last_login_ip         TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash  TEXT    NOT NULL UNIQUE,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ip          TEXT    NOT NULL DEFAULT '',
    user_agent  TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    username   TEXT    NOT NULL DEFAULT '',
    action     TEXT    NOT NULL,
    target     TEXT    NOT NULL DEFAULT '',
    detail     TEXT    NOT NULL DEFAULT '',
    ip         TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at);

-- 审计表只允许追加，禁止篡改（PRD S-G01）
CREATE TRIGGER IF NOT EXISTS audit_logs_no_update
BEFORE UPDATE ON audit_logs
BEGIN
    SELECT RAISE(ABORT, 'audit_logs is append-only');
END;

CREATE TRIGGER IF NOT EXISTS audit_logs_no_delete
BEFORE DELETE ON audit_logs
BEGIN
    SELECT RAISE(ABORT, 'audit_logs is append-only');
END;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	// v2：代理模块（入站 / 客户端 / 流量游标与采样）
	if _, err := s.db.ExecContext(ctx, proxySchema); err != nil {
		return fmt.Errorf("初始化代理表结构失败: %w", err)
	}
	// v3：运维模块（网站 / 防火墙规则 / 已装软件）
	if _, err := s.db.ExecContext(ctx, opsSchema); err != nil {
		return fmt.Errorf("初始化运维表结构失败: %w", err)
	}
	for _, v := range []int{1, 2, 3} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			v, time.Now().Unix()); err != nil {
			return fmt.Errorf("写入迁移版本失败: %w", err)
		}
	}
	return nil
}

// ---------- users ----------

// CountUsers 返回账号数量，用于判断是否首次启动。
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser 新建账号。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, mustChange bool) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, must_change_password, created_at) VALUES(?,?,?,?)`,
		username, passwordHash, boolToInt(mustChange), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UserByUsername 按用户名查询。
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, totp_secret, totp_enabled,
		        must_change_password, created_at, last_login_at, last_login_ip
		   FROM users WHERE username = ?`, username)
	return scanUser(row)
}

// UserByID 按 ID 查询。
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, totp_secret, totp_enabled,
		        must_change_password, created_at, last_login_at, last_login_ip
		   FROM users WHERE id = ?`, id)
	return scanUser(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(r rowScanner) (*User, error) {
	var u User
	var created int64
	var totp, mustChange int
	var lastLogin sql.NullInt64
	err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &totp,
		&mustChange, &created, &lastLogin, &u.LastLoginIP)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.TOTPEnabled = totp == 1
	u.MustChangePassword = mustChange == 1
	u.CreatedAt = time.Unix(created, 0)
	if lastLogin.Valid {
		u.LastLoginAt = sql.NullTime{Time: time.Unix(lastLogin.Int64, 0), Valid: true}
	}
	return &u, nil
}

// UpdatePassword 修改密码并清除「必须改密」标记。
func (s *Store) UpdatePassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, must_change_password = 0 WHERE id = ?`, hash, userID)
	return err
}

// TouchLogin 记录登录成功的时间与来源 IP。
func (s *Store) TouchLogin(ctx context.Context, userID int64, ip string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = ?, last_login_ip = ? WHERE id = ?`,
		time.Now().Unix(), ip, userID)
	return err
}

// ---------- sessions ----------

// CreateSession 写入会话，返回会话 ID。
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, ip, ua string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(token_hash, user_id, ip, user_agent, created_at, expires_at)
		 VALUES(?,?,?,?,?,?)`,
		tokenHash, userID, ip, ua, time.Now().Unix(), expires.Unix())
	return err
}

// SessionByToken 校验会话是否有效并返回所属用户。
func (s *Store) SessionByToken(ctx context.Context, tokenHash string) (*Session, *User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, ip, user_agent, created_at, expires_at
		   FROM sessions WHERE token_hash = ?`, tokenHash)

	var sess Session
	var created, expires int64
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.IP, &sess.UserAgent, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	sess.CreatedAt = time.Unix(created, 0)
	sess.ExpiresAt = time.Unix(expires, 0)

	if time.Now().After(sess.ExpiresAt) {
		_ = s.DeleteSession(ctx, sess.ID)
		return nil, nil, ErrNotFound
	}

	u, err := s.UserByID(ctx, sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	return &sess, u, nil
}

// DeleteSession 注销单个会话。
func (s *Store) DeleteSession(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteUserSessions 踢掉某个用户的全部会话（改密后调用）。
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// CleanupSessions 清理过期会话，由后台任务周期调用。
func (s *Store) CleanupSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- settings ----------

// SetSetting 写入键值配置。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

// GetSetting 读取键值配置，不存在时返回空串。
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// ---------- audit ----------

// Audit 追加一条审计记录。该表有触发器保护，无法 UPDATE/DELETE。
func (s *Store) Audit(ctx context.Context, username, action, target, detail, ip string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs(username, action, target, detail, ip, created_at) VALUES(?,?,?,?,?,?)`,
		username, action, target, detail, ip, time.Now().Unix())
	return err
}

// AuditCount 返回审计条数，用于 /healthz 自检。
func (s *Store) AuditCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TrimDetail 截断过长的审计详情，避免单条记录撑爆页面。
func TrimDetail(s string) string {
	const max = 512
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
