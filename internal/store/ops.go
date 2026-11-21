package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"
)

// 运维相关表（schema v3）：网站、防火墙规则、已装软件。
// 与 proxy 表分开，避免单文件过长，也方便将来拆分存储实现。
const opsSchema = `
CREATE TABLE IF NOT EXISTS sites (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    domain      TEXT    NOT NULL,
    root        TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL DEFAULT 'static',
    php_version TEXT    NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 80,
    ssl         INTEGER NOT NULL DEFAULT 0,
    cert_path   TEXT    NOT NULL DEFAULT '',
    key_path    TEXT    NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sites_domain ON sites(domain);

CREATE TABLE IF NOT EXISTS firewall_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    action     TEXT    NOT NULL,
    proto      TEXT    NOT NULL DEFAULT 'tcp',
    port       TEXT    NOT NULL DEFAULT '',
    source     TEXT    NOT NULL DEFAULT '',
    comment    TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS installed_apps (
    slug         TEXT PRIMARY KEY,
    version      TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL DEFAULT 'installed',
    installed_at INTEGER NOT NULL,
    detail       TEXT    NOT NULL DEFAULT ''
);
`

// Site 是一个 Nginx 站点。
//
// JSON 标签必须显式写全：这些结构体会被直接序列化给前端，
// 少了标签就会输出 Go 字段名（ID/Name/Domain），前端拿到 undefined。
type Site struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Domain     string    `json:"domain"`
	Root       string    `json:"root"`
	Kind       string    `json:"kind"` // static | php | proxy
	PHPVersion string    `json:"php_version"`
	Port       int       `json:"port"`
	SSL        bool      `json:"ssl"`
	CertPath   string    `json:"cert_path,omitempty"`
	KeyPath    string    `json:"key_path,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// FirewallRule 是一条入站过滤规则。
type FirewallRule struct {
	ID        int64     `json:"id"`
	Action    string    `json:"action"` // accept | drop
	Proto     string    `json:"proto"`  // tcp | udp | any
	Port      string    `json:"port"`   // 单个端口或 1000-2000
	Source    string    `json:"source"` // 空表示任意来源
	Comment   string    `json:"comment"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// InstalledApp 记录软件商店的安装状态。
type InstalledApp struct {
	Slug        string    `json:"slug"`
	Version     string    `json:"version"`
	Status      string    `json:"status"`
	InstalledAt time.Time `json:"installed_at"`
	Detail      string    `json:"detail,omitempty"`
}

// ---------- sites ----------

// ListSites 按 ID 升序返回全部站点。
func (s *Store) ListSites(ctx context.Context) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, domain, root, kind, php_version, port, ssl,
		        cert_path, key_path, enabled, created_at
		   FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Site
	for rows.Next() {
		var x Site
		var ssl, enabled int
		var created int64
		if err := rows.Scan(&x.ID, &x.Name, &x.Domain, &x.Root, &x.Kind,
			&x.PHPVersion, &x.Port, &ssl, &x.CertPath, &x.KeyPath, &enabled, &created); err != nil {
			return nil, err
		}
		x.SSL = ssl == 1
		x.Enabled = enabled == 1
		x.CreatedAt = time.Unix(created, 0)
		out = append(out, x)
	}
	return out, rows.Err()
}

// SiteByID 按 ID 查询站点。
func (s *Store) SiteByID(ctx context.Context, id int64) (*Site, error) {
	var x Site
	var ssl, enabled int
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, domain, root, kind, php_version, port, ssl,
		        cert_path, key_path, enabled, created_at
		   FROM sites WHERE id = ?`, id).
		Scan(&x.ID, &x.Name, &x.Domain, &x.Root, &x.Kind, &x.PHPVersion,
			&x.Port, &ssl, &x.CertPath, &x.KeyPath, &enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	x.SSL = ssl == 1
	x.Enabled = enabled == 1
	x.CreatedAt = time.Unix(created, 0)
	return &x, nil
}

// CreateSite 插入站点并回填 ID。
func (s *Store) CreateSite(ctx context.Context, x *Site) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sites(name, domain, root, kind, php_version, port, ssl,
		                   cert_path, key_path, enabled, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		x.Name, x.Domain, x.Root, x.Kind, x.PHPVersion, x.Port, boolToInt(x.SSL),
		x.CertPath, x.KeyPath, boolToInt(x.Enabled), time.Now().Unix())
	if err != nil {
		return err
	}
	x.ID, err = res.LastInsertId()
	return err
}

// UpdateSite 更新可编辑字段。
func (s *Store) UpdateSite(ctx context.Context, x *Site) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sites SET name=?, domain=?, root=?, kind=?, php_version=?, port=?,
		        ssl=?, cert_path=?, key_path=?, enabled=? WHERE id=?`,
		x.Name, x.Domain, x.Root, x.Kind, x.PHPVersion, x.Port, boolToInt(x.SSL),
		x.CertPath, x.KeyPath, boolToInt(x.Enabled), x.ID)
	return err
}

// DeleteSite 删除站点记录。
func (s *Store) DeleteSite(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sites WHERE id = ?`, id)
	return err
}

// SiteByNameOrDomain 用于创建前查重。
func (s *Store) SiteByNameOrDomain(ctx context.Context, name, domain string) (*Site, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM sites WHERE name = ? OR domain = ? LIMIT 1`, name, domain).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.SiteByID(ctx, id)
}

// ---------- firewall rules ----------

// ListFirewallRules 返回全部规则。
func (s *Store) ListFirewallRules(ctx context.Context) ([]FirewallRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, action, proto, port, source, comment, enabled, created_at
		   FROM firewall_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FirewallRule
	for rows.Next() {
		var r FirewallRule
		var enabled int
		var created int64
		if err := rows.Scan(&r.ID, &r.Action, &r.Proto, &r.Port, &r.Source,
			&r.Comment, &enabled, &created); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateFirewallRule 插入规则并回填 ID。
func (s *Store) CreateFirewallRule(ctx context.Context, r *FirewallRule) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO firewall_rules(action, proto, port, source, comment, enabled, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		r.Action, r.Proto, r.Port, r.Source, r.Comment, boolToInt(r.Enabled), time.Now().Unix())
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

// DeleteFirewallRule 删除规则。
func (s *Store) DeleteFirewallRule(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM firewall_rules WHERE id = ?`, id)
	return err
}

// ---------- installed apps ----------

// ListInstalledApps 返回全部安装记录。
func (s *Store) ListInstalledApps(ctx context.Context) (map[string]InstalledApp, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slug, version, status, installed_at, detail FROM installed_apps`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]InstalledApp{}
	for rows.Next() {
		var a InstalledApp
		var at int64
		if err := rows.Scan(&a.Slug, &a.Version, &a.Status, &at, &a.Detail); err != nil {
			return nil, err
		}
		a.InstalledAt = time.Unix(at, 0)
		out[a.Slug] = a
	}
	return out, rows.Err()
}

// MarkAppInstalled 写入/更新安装记录。
func (s *Store) MarkAppInstalled(ctx context.Context, slug, version, status, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO installed_apps(slug, version, status, installed_at, detail)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(slug) DO UPDATE SET
		     version=excluded.version, status=excluded.status,
		     installed_at=excluded.installed_at, detail=excluded.detail`,
		slug, version, status, time.Now().Unix(), TrimDetail(detail))
	return err
}

// MarkAppRemoved 删除安装记录。
func (s *Store) MarkAppRemoved(ctx context.Context, slug string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM installed_apps WHERE slug = ?`, slug)
	return err
}

// Snapshot 用 VACUUM INTO 生成一份一致的数据库副本。
//
// 直接拷贝 .db + -wal 在运行中可能拿到半写状态；VACUUM INTO 由 SQLite
// 自己保证一致性，且产出的是一份可直接打开的单文件。
func (s *Store) Snapshot(ctx context.Context, dest string) error {
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// VACUUM INTO 的目标是字面量，转义单引号防止注入
	quoted := "'" + strings.ReplaceAll(dest, "'", "''") + "'"
	_, err := s.db.ExecContext(ctx, "VACUUM INTO "+quoted)
	return err
}
