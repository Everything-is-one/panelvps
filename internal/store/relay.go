package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Relay 是一条端口转发（中转）规则。
//
// 它和 Inbound 是两套东西：Inbound 由 Xray 承载代理协议，Relay 只做字节搬运。
// 放在同一张表里会让两条路径的语义互相污染，所以单独建表。
type Relay struct {
	ID         int64
	Name       string
	Listen     string
	ListenPort int
	Target     string
	Network    string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const relaySchema = `
CREATE TABLE IF NOT EXISTS relays (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL DEFAULT '',
    listen      TEXT    NOT NULL DEFAULT '0.0.0.0',
    listen_port INTEGER NOT NULL,
    target      TEXT    NOT NULL,
    network     TEXT    NOT NULL DEFAULT 'tcp',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_relays_addr ON relays(listen, listen_port, network);
`

const relayColumns = `id, name, listen, listen_port, target, network, enabled, created_at, updated_at`

// ListRelays 按创建顺序返回全部规则。
func (s *Store) ListRelays(ctx context.Context) ([]Relay, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+relayColumns+` FROM relays ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Relay
	for rows.Next() {
		r, err := scanRelay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RelayByID 按主键读取。
func (s *Store) RelayByID(ctx context.Context, id int64) (*Relay, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+relayColumns+` FROM relays WHERE id=?`, id)
	r, err := scanRelay(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// RelayByAddr 判断监听地址是否已被别的规则占用（exclude 用于更新自身）。
func (s *Store) RelayByAddr(ctx context.Context, listen string, port int, exclude int64) (*Relay, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+relayColumns+` FROM relays
		 WHERE listen=? AND listen_port=? AND id<>? LIMIT 1`, listen, port, exclude)
	r, err := scanRelay(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// CreateRelay 新建规则。
func (s *Store) CreateRelay(ctx context.Context, r *Relay) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO relays(name, listen, listen_port, target, network, enabled, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		r.Name, r.Listen, r.ListenPort, r.Target, r.Network, boolToInt(r.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateRelay 全量更新。
func (s *Store) UpdateRelay(ctx context.Context, r *Relay) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE relays SET name=?, listen=?, listen_port=?, target=?, network=?, enabled=?, updated_at=?
		 WHERE id=?`,
		r.Name, r.Listen, r.ListenPort, r.Target, r.Network, boolToInt(r.Enabled),
		time.Now().Unix(), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRelay 删除规则。
func (s *Store) DeleteRelay(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM relays WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRelay(sc rowScanner) (Relay, error) {
	var (
		r                    Relay
		enabled              int
		createdAt, updatedAt int64
	)
	if err := sc.Scan(&r.ID, &r.Name, &r.Listen, &r.ListenPort, &r.Target, &r.Network,
		&enabled, &createdAt, &updatedAt); err != nil {
		return r, err
	}
	r.Enabled = enabled != 0
	r.CreatedAt = time.Unix(createdAt, 0)
	r.UpdatedAt = time.Unix(updatedAt, 0)
	return r, nil
}
