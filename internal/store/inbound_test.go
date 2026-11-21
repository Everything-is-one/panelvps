package store

import (
	"context"
	"testing"
)

// PublicPort 要能存能读：NAT 机器上它是分享链接里唯一正确的端口。
func TestInboundPublicPortRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateInbound(ctx, &Inbound{
		Tag: "vless-1", Remark: "nat", Protocol: "vless",
		Listen: "0.0.0.0", Port: 47736, PublicPort: 30986,
		Network: "tcp", Security: "reality", Options: "{}", Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建入站失败: %v", err)
	}

	got, err := st.InboundByID(ctx, id)
	if err != nil {
		t.Fatalf("读取入站失败: %v", err)
	}
	if got.PublicPort != 30986 || got.Port != 47736 {
		t.Fatalf("端口读回不对：port=%d public_port=%d", got.Port, got.PublicPort)
	}

	// 改成 0（取消映射）也要落库
	got.PublicPort = 0
	if err := st.UpdateInbound(ctx, got); err != nil {
		t.Fatalf("更新入站失败: %v", err)
	}
	again, err := st.InboundByID(ctx, id)
	if err != nil {
		t.Fatalf("再次读取失败: %v", err)
	}
	if again.PublicPort != 0 {
		t.Fatalf("public_port 应被清成 0，得到 %d", again.PublicPort)
	}

	list, err := st.ListInbounds(ctx)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(list) != 1 || list[0].PublicPort != 0 {
		t.Fatalf("列表返回的 public_port 不对: %+v", list)
	}
}

// 老库升级：inbounds 没有 public_port 列时要自动补上，不能直接报错。
func TestMigrateAddsPublicPort(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 模拟老库：删掉列（SQLite 3.35+ 支持 DROP COLUMN）
	if _, err := st.DB().ExecContext(ctx, `ALTER TABLE inbounds DROP COLUMN public_port`); err != nil {
		t.Skipf("当前 SQLite 不支持 DROP COLUMN，跳过: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('inbounds') WHERE name='public_port'`).Scan(&n); err != nil {
		t.Fatalf("查询列失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("列没被删掉，前置条件不成立")
	}

	// 再跑一次迁移，应该把列补回来
	if err := st.migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('inbounds') WHERE name='public_port'`).Scan(&n); err != nil {
		t.Fatalf("查询列失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("迁移后 public_port 列仍不存在")
	}

	// 补完列后还能正常写入
	if _, err := st.CreateInbound(ctx, &Inbound{
		Tag: "after-migrate", Protocol: "vless", Listen: "0.0.0.0",
		Port: 12345, PublicPort: 23456, Network: "tcp", Security: "none",
		Options: "{}", Enabled: true,
	}); err != nil {
		t.Fatalf("迁移后写入失败: %v", err)
	}
}
