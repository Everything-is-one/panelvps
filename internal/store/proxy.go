package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// 代理相关表。迁移是幂等的（CREATE TABLE IF NOT EXISTS），
// 老库直接启动即可升级，不需要单独的升级命令。
const proxySchema = `
CREATE TABLE IF NOT EXISTS inbounds (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    tag        TEXT    NOT NULL UNIQUE,
    remark     TEXT    NOT NULL DEFAULT '',
    protocol   TEXT    NOT NULL,
    listen     TEXT    NOT NULL DEFAULT '0.0.0.0',
    port       INTEGER NOT NULL,
    network    TEXT    NOT NULL DEFAULT 'tcp',
    security   TEXT    NOT NULL DEFAULT 'none',
    options    TEXT    NOT NULL DEFAULT '{}',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbounds_port ON inbounds(port);

CREATE TABLE IF NOT EXISTS clients (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    inbound_id  INTEGER NOT NULL REFERENCES inbounds(id) ON DELETE CASCADE,
    email       TEXT    NOT NULL,
    secret      TEXT    NOT NULL,
    flow        TEXT    NOT NULL DEFAULT '',
    level       INTEGER NOT NULL DEFAULT 0,
    enabled     INTEGER NOT NULL DEFAULT 1,
    up          INTEGER NOT NULL DEFAULT 0,
    down        INTEGER NOT NULL DEFAULT 0,
    total_bytes INTEGER NOT NULL DEFAULT 0,
    expires_at  INTEGER,
    created_at  INTEGER NOT NULL,
    UNIQUE(inbound_id, email)
);
CREATE INDEX IF NOT EXISTS idx_clients_inbound ON clients(inbound_id);

-- stats 计数器是「自 Xray 启动以来的累计值」，面板重启后归零。
-- 存一份游标，每次只累加增量，避免流量被清零或重复计入。
CREATE TABLE IF NOT EXISTS traffic_cursors (
    name       TEXT PRIMARY KEY,
    value      INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS traffic_samples (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id  INTEGER NOT NULL,
    up         INTEGER NOT NULL DEFAULT 0,
    down       INTEGER NOT NULL DEFAULT 0,
    sampled_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_client ON traffic_samples(client_id, sampled_at);
`

// Inbound 是一条入站配置。
type Inbound struct {
	ID        int64
	Tag       string
	Remark    string
	Protocol  string // vless | vmess | trojan | shadowsocks
	Listen    string
	Port      int
	Network   string // tcp | ws | grpc
	Security  string // none | tls | reality
	Options   string // JSON，协议相关参数
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Client 是一个入站下的用户。
type Client struct {
	ID         int64
	InboundID  int64
	Email      string
	Secret     string // VLESS/VMess 为 UUID，Trojan/SS 为密码
	Flow       string
	Level      int
	Enabled    bool
	Up         int64
	Down       int64
	TotalBytes int64 // 0 表示不限
	ExpiresAt  sql.NullTime
	CreatedAt  time.Time
}

// QuotaExceeded 判断是否已超出配额。
func (c *Client) QuotaExceeded() bool {
	return c.TotalBytes > 0 && c.Up+c.Down >= c.TotalBytes
}

// Expired 判断是否已过期。
func (c *Client) Expired(now time.Time) bool {
	return c.ExpiresAt.Valid && now.After(c.ExpiresAt.Time)
}

// Active 判断该用户当前是否应写入 Xray 配置。
func (c *Client) Active(now time.Time) bool {
	return c.Enabled && !c.Expired(now) && !c.QuotaExceeded()
}

// ---------- inbounds ----------

// ListInbounds 返回全部入站，按端口升序。
func (s *Store) ListInbounds(ctx context.Context) ([]Inbound, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tag, remark, protocol, listen, port, network, security, options,
		        enabled, created_at, updated_at
		   FROM inbounds ORDER BY port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Inbound
	for rows.Next() {
		var in Inbound
		var enabled int
		var created, updated int64
		if err := rows.Scan(&in.ID, &in.Tag, &in.Remark, &in.Protocol, &in.Listen, &in.Port,
			&in.Network, &in.Security, &in.Options, &enabled, &created, &updated); err != nil {
			return nil, err
		}
		in.Enabled = enabled == 1
		in.CreatedAt = time.Unix(created, 0)
		in.UpdatedAt = time.Unix(updated, 0)
		out = append(out, in)
	}
	return out, rows.Err()
}

// InboundByID 按主键查询。
func (s *Store) InboundByID(ctx context.Context, id int64) (*Inbound, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tag, remark, protocol, listen, port, network, security, options,
		        enabled, created_at, updated_at
		   FROM inbounds WHERE id = ?`, id)
	return scanInbound(row)
}

// InboundByPort 用于端口冲突检查。
func (s *Store) InboundByPort(ctx context.Context, listen string, port int, excludeID int64) (*Inbound, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, tag, remark, protocol, listen, port, network, security, options,
		        enabled, created_at, updated_at
		   FROM inbounds WHERE port = ? AND listen = ? AND id <> ?`, port, listen, excludeID)
	return scanInbound(row)
}

func scanInbound(r rowScanner) (*Inbound, error) {
	var in Inbound
	var enabled int
	var created, updated int64
	err := r.Scan(&in.ID, &in.Tag, &in.Remark, &in.Protocol, &in.Listen, &in.Port,
		&in.Network, &in.Security, &in.Options, &enabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	in.Enabled = enabled == 1
	in.CreatedAt = time.Unix(created, 0)
	in.UpdatedAt = time.Unix(updated, 0)
	return &in, nil
}

// CreateInbound 新建入站并返回 ID。
func (s *Store) CreateInbound(ctx context.Context, in *Inbound) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO inbounds(tag, remark, protocol, listen, port, network, security, options,
		                      enabled, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		in.Tag, in.Remark, in.Protocol, in.Listen, in.Port, in.Network, in.Security, in.Options,
		boolToInt(in.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateInbound 覆盖式更新。
func (s *Store) UpdateInbound(ctx context.Context, in *Inbound) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inbounds SET tag=?, remark=?, protocol=?, listen=?, port=?, network=?,
		        security=?, options=?, enabled=?, updated_at=?
		  WHERE id=?`,
		in.Tag, in.Remark, in.Protocol, in.Listen, in.Port, in.Network,
		in.Security, in.Options, boolToInt(in.Enabled), time.Now().Unix(), in.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteInbound 删除入站，其下客户端由外键级联删除。
func (s *Store) DeleteInbound(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inbounds WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- clients ----------

// ListClientsByInbound 返回某入站下的全部客户端。
func (s *Store) ListClientsByInbound(ctx context.Context, inboundID int64) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, inbound_id, email, secret, flow, level, enabled, up, down,
		        total_bytes, expires_at, created_at
		   FROM clients WHERE inbound_id = ? ORDER BY id`, inboundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ClientsByInboundIDs 批量取，避免 N+1 查询。
func (s *Store) ClientsByInboundIDs(ctx context.Context, ids []int64) (map[int64][]Client, error) {
	out := make(map[int64][]Client, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query := `SELECT id, inbound_id, email, secret, flow, level, enabled, up, down,
	                 total_bytes, expires_at, created_at
	            FROM clients WHERE inbound_id IN (?` + placeholders(len(ids)-1) + `) ORDER BY id`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out[c.InboundID] = append(out[c.InboundID], *c)
	}
	return out, rows.Err()
}

// ClientByID 按主键查询。
func (s *Store) ClientByID(ctx context.Context, id int64) (*Client, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, inbound_id, email, secret, flow, level, enabled, up, down,
		        total_bytes, expires_at, created_at
		   FROM clients WHERE id = ?`, id)
	return scanClient(row)
}

// ClientByEmail 用于统计回填时按 email 定位用户。
func (s *Store) ClientByEmail(ctx context.Context, email string) (*Client, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, inbound_id, email, secret, flow, level, enabled, up, down,
		        total_bytes, expires_at, created_at
		   FROM clients WHERE email = ?`, email)
	return scanClient(row)
}

func scanClient(r rowScanner) (*Client, error) {
	var c Client
	var enabled int
	var expires sql.NullInt64
	var created int64
	err := r.Scan(&c.ID, &c.InboundID, &c.Email, &c.Secret, &c.Flow, &c.Level, &enabled,
		&c.Up, &c.Down, &c.TotalBytes, &expires, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Enabled = enabled == 1
	c.CreatedAt = time.Unix(created, 0)
	if expires.Valid {
		c.ExpiresAt = sql.NullTime{Time: time.Unix(expires.Int64, 0), Valid: true}
	}
	return &c, nil
}

// CreateClient 新增用户。email 必须全局唯一（Xray 用它做统计维度）。
func (s *Store) CreateClient(ctx context.Context, c *Client) (int64, error) {
	var expires any
	if c.ExpiresAt.Valid {
		expires = c.ExpiresAt.Time.Unix()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO clients(inbound_id, email, secret, flow, level, enabled, up, down,
		                     total_bytes, expires_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		c.InboundID, c.Email, c.Secret, c.Flow, c.Level, boolToInt(c.Enabled),
		c.Up, c.Down, c.TotalBytes, expires, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateClient 更新用户的可变字段（不动流量计数）。
func (s *Store) UpdateClient(ctx context.Context, c *Client) error {
	var expires any
	if c.ExpiresAt.Valid {
		expires = c.ExpiresAt.Time.Unix()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET email=?, secret=?, flow=?, level=?, enabled=?,
		        total_bytes=?, expires_at=?
		  WHERE id=?`,
		c.Email, c.Secret, c.Flow, c.Level, boolToInt(c.Enabled),
		c.TotalBytes, expires, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteClient 删除用户。
func (s *Store) DeleteClient(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clients WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddClientTraffic 累加流量（增量），并返回累加后的值。
func (s *Store) AddClientTraffic(ctx context.Context, id, upDelta, downDelta int64) error {
	if upDelta == 0 && downDelta == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE clients SET up = up + ?, down = down + ? WHERE id = ?`,
		upDelta, downDelta, id)
	return err
}

// ResetClientTraffic 清零某个用户的流量。
func (s *Store) ResetClientTraffic(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET up = 0, down = 0 WHERE id = ?`, id)
	return err
}

// AutoDisableExhausted 把超额/过期的用户置为禁用，返回受影响条数。
//
// 这是配额真正生效的地方：仅靠生成配置时过滤，用户会一直停在「看起来能用
// 但连不上」的状态；显式置灰才能让界面和实际一致。
func (s *Store) AutoDisableExhausted(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET enabled = 0
		  WHERE enabled = 1
		    AND ((total_bytes > 0 AND up + down >= total_bytes)
		      OR (expires_at IS NOT NULL AND expires_at > 0 AND expires_at < ?))`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- traffic cursors ----------

// TrafficCursor 读取某个计数器上次的累计值。
func (s *Store) TrafficCursor(ctx context.Context, name string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM traffic_cursors WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// SetTrafficCursor 写入累计值。
func (s *Store) SetTrafficCursor(ctx context.Context, name string, value int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO traffic_cursors(name, value, updated_at) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, value, time.Now().Unix())
	return err
}

// AddTrafficSample 追加一条采样点，用于画趋势图。
func (s *Store) AddTrafficSample(ctx context.Context, clientID, up, down int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO traffic_samples(client_id, up, down, sampled_at) VALUES(?,?,?,?)`,
		clientID, up, down, time.Now().Unix())
	return err
}

// PruneTrafficSamples 只保留最近 7 天的采样。
func (s *Store) PruneTrafficSamples(ctx context.Context, keep time.Duration) (int64, error) {
	cutoff := time.Now().Add(-keep).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM traffic_samples WHERE sampled_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		out = append(out, ',', '?')
	}
	return string(out)
}
