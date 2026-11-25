package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NodeSize はノード 1 台のサイズ。
type NodeSize struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
	DiskGiB   int `json:"diskGiB"`
}

// ClusterSpec はクラスタの望ましい構成。
type ClusterSpec struct {
	Version       string   `json:"version"`
	Backend       string   `json:"backend"`
	Image         string   `json:"image"`
	ControlPlanes int      `json:"controlPlanes"`
	Workers       int      `json:"workers"`
	ControlPlane  NodeSize `json:"controlPlane"`
	Worker        NodeSize `json:"worker"`
	SSHKeys       []string `json:"sshAuthorizedKeys,omitempty"`
}

// Cluster は保存されているクラスタ。
type Cluster struct {
	Name         string
	Distro       string
	Index        int // 172.30.<Index>.0/24 を使う
	Spec         ClusterSpec
	Secrets      []byte // Distro の Secrets を JSON にしたもの
	DesiredState string
	CreatedAt    time.Time
}

func (s *Store) InsertCluster(ctx context.Context, c *Cluster) error {
	spec, err := json.Marshal(c.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO clusters (name, distro, idx, spec, secrets, desired_state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Distro, c.Index, string(spec), string(c.Secrets), c.DesiredState, c.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("cluster %q: %w: %v", c.Name, ErrExists, err)
		}
		return err
	}
	return nil
}

const selectCluster = `SELECT name, distro, idx, spec, secrets, desired_state, created_at FROM clusters`

func scanCluster(row interface{ Scan(...any) error }) (*Cluster, error) {
	var (
		c                      Cluster
		spec, secrets, created string
	)
	if err := row.Scan(&c.Name, &c.Distro, &c.Index, &spec, &secrets, &c.DesiredState, &created); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &c.Spec); err != nil {
		return nil, err
	}
	c.Secrets = []byte(secrets)
	var err error
	if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetCluster(ctx context.Context, name string) (*Cluster, error) {
	c, err := scanCluster(s.db.QueryRowContext(ctx, selectCluster+` WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("cluster %q: %w", name, ErrNotFound)
	}
	return c, err
}

func (s *Store) ListClusters(ctx context.Context) ([]*Cluster, error) {
	rows, err := s.db.QueryContext(ctx, selectCluster+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Cluster{}
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetClusterDesiredState(ctx context.Context, name, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE clusters SET desired_state = ? WHERE name = ?`, state, name)
	if err != nil {
		return err
	}
	return mustAffectKind(res, "cluster", name)
}

func (s *Store) DeleteCluster(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clusters WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return mustAffectKind(res, "cluster", name)
}

func mustAffectKind(res sql.Result, kind, name string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %q: %w", kind, name, ErrNotFound)
	}
	return nil
}
