// Package store は niwakubed の状態を SQLite に保存する。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go の SQLite ドライバ (cgo 不要)
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Machine は保存されている VM / コンテナの定義 (望ましい状態)。
type Machine struct {
	Name         string
	Backend      string
	Image        string
	CPUs         int
	MemoryMiB    int
	DiskGiB      int
	IP           netip.Addr
	MAC          string
	SSHKeys      []string
	DesiredState string // "running" | "stopped"
	Cluster      string // 所属するクラスタ (単体の VM なら空)
	Role         string // クラスタ内の役割 (control-plane / worker)
	CreatedAt    time.Time
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS machines (
	name          TEXT PRIMARY KEY,
	backend       TEXT NOT NULL,
	image         TEXT NOT NULL,
	cpus          INTEGER NOT NULL,
	memory_mib    INTEGER NOT NULL,
	disk_gib      INTEGER NOT NULL,
	ip            TEXT NOT NULL UNIQUE,
	mac           TEXT NOT NULL UNIQUE,
	ssh_keys      TEXT NOT NULL DEFAULT '[]',
	desired_state TEXT NOT NULL,
	created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS clusters (
	name          TEXT PRIMARY KEY,
	distro        TEXT NOT NULL,
	idx           INTEGER NOT NULL UNIQUE,
	spec          TEXT NOT NULL,
	secrets       TEXT NOT NULL,
	desired_state TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
`

// migrations は既存の DB に後から足した列。列が無ければ追加する。
var migrations = []struct{ table, column, ddl string }{
	{"machines", "cluster", `ALTER TABLE machines ADD COLUMN cluster TEXT NOT NULL DEFAULT ''`},
	{"machines", "role", `ALTER TABLE machines ADD COLUMN role TEXT NOT NULL DEFAULT ''`},
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(m.ddl); err != nil {
				return fmt.Errorf("%s: %w", m.ddl, err)
			}
		}
	}
	return nil
}

// Open は path の SQLite を開き、スキーマを用意する。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) InsertMachine(ctx context.Context, m *Machine) error {
	keys, err := json.Marshal(nonNil(m.SSHKeys))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO machines (name, backend, image, cpus, memory_mib, disk_gib, ip, mac, ssh_keys, desired_state, cluster, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Name, m.Backend, m.Image, m.CPUs, m.MemoryMiB, m.DiskGiB, m.IP.String(), m.MAC, string(keys),
		m.DesiredState, m.Cluster, m.Role, m.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		// name / ip / mac のどれが重複しても ErrExists にする (どの制約が先に報告されるかは SQLite 次第)
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("machine %q: %w: %v", m.Name, ErrExists, err)
		}
		return err
	}
	return nil
}

const selectMachine = `SELECT name, backend, image, cpus, memory_mib, disk_gib, ip, mac, ssh_keys, desired_state, cluster, role, created_at FROM machines`

func scanMachine(row interface{ Scan(...any) error }) (*Machine, error) {
	var (
		m                 Machine
		ip, keys, created string
	)
	if err := row.Scan(&m.Name, &m.Backend, &m.Image, &m.CPUs, &m.MemoryMiB, &m.DiskGiB, &ip, &m.MAC, &keys, &m.DesiredState, &m.Cluster, &m.Role, &created); err != nil {
		return nil, err
	}
	var err error
	if m.IP, err = netip.ParseAddr(ip); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(keys), &m.SSHKeys); err != nil {
		return nil, err
	}
	if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) GetMachine(ctx context.Context, name string) (*Machine, error) {
	m, err := scanMachine(s.db.QueryRowContext(ctx, selectMachine+` WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("machine %q: %w", name, ErrNotFound)
	}
	return m, err
}

func (s *Store) ListMachines(ctx context.Context) ([]*Machine, error) {
	rows, err := s.db.QueryContext(ctx, selectMachine+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Machine{}
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetDesiredState(ctx context.Context, name, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE machines SET desired_state = ? WHERE name = ?`, state, name)
	if err != nil {
		return err
	}
	return mustAffect(res, name)
}

func (s *Store) DeleteMachine(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return mustAffect(res, name)
}

func mustAffect(res sql.Result, name string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("machine %q: %w", name, ErrNotFound)
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
