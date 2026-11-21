package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAuditIsAppendOnly(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.Audit(ctx, "admin", "login_success", "panel", "test", "127.0.0.1"); err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `UPDATE audit_logs SET detail = 'tampered'`); err == nil {
		t.Fatal("审计表不应允许 UPDATE")
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM audit_logs`); err == nil {
		t.Fatal("审计表不应允许 DELETE")
	}

	n, err := st.AuditCount(ctx)
	if err != nil || n != 1 {
		t.Fatalf("审计条数应为 1，实际 %d (err=%v)", n, err)
	}
}

func TestUserLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	n, err := st.CountUsers(ctx)
	if err != nil || n != 0 {
		t.Fatalf("初始账号数应为 0，实际 %d", n)
	}

	id, err := st.CreateUser(ctx, "admin", "$argon2id$fake", true)
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	u, err := st.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if u.ID != id || !u.MustChangePassword {
		t.Fatalf("用户字段不符: %+v", u)
	}

	if err := st.UpdatePassword(ctx, id, "$argon2id$new"); err != nil {
		t.Fatalf("更新密码失败: %v", err)
	}
	u, _ = st.UserByID(ctx, id)
	if u.PasswordHash != "$argon2id$new" || u.MustChangePassword {
		t.Fatalf("改密后字段不符: %+v", u)
	}

	if _, err := st.UserByUsername(ctx, "ghost"); err != ErrNotFound {
		t.Fatalf("不存在的用户应返回 ErrNotFound，实际 %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _ := st.CreateUser(ctx, "admin", "$argon2id$fake", false)

	if err := st.CreateSession(ctx, "hash-1", id, "127.0.0.1", "ua", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// 已过期的会话应被视为不存在，并从表中清除
	if _, _, err := st.SessionByToken(ctx, "hash-1"); err != ErrNotFound {
		t.Fatalf("过期会话应返回 ErrNotFound，实际 %v", err)
	}

	if err := st.CreateSession(ctx, "hash-2", id, "127.0.0.1", "ua", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	sess, u, err := st.SessionByToken(ctx, "hash-2")
	if err != nil {
		t.Fatalf("有效会话应能读取: %v", err)
	}
	if sess.UserID != id || u.Username != "admin" {
		t.Fatalf("会话关联错误: %+v %+v", sess, u)
	}

	if err := st.DeleteUserSessions(ctx, id); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}
	if _, _, err := st.SessionByToken(ctx, "hash-2"); err != ErrNotFound {
		t.Fatal("删除后会话不应存在")
	}
}
