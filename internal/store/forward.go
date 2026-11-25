package store

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Forward はホストのポートからクラスタの IP (LoadBalancer など) への転送 (DNAT)。
// LAN の PC から NAT の内側にあるクラスタに届かせるために使う。
type Forward struct {
	Proto      string // tcp | udp
	HostPort   int
	Cluster    string
	TargetIP   netip.Addr
	TargetPort int
	CreatedAt  time.Time
}

func (s *Store) InsertForward(ctx context.Context, f *Forward) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO forwards (proto, host_port, cluster, target_ip, target_port, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		f.Proto, f.HostPort, f.Cluster, f.TargetIP.String(), f.TargetPort, f.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%s port %d is already forwarded: %w", f.Proto, f.HostPort, ErrExists)
	}
	return err
}

// ListForwards は転送の一覧を返す。cluster が空なら全部。
func (s *Store) ListForwards(ctx context.Context, cluster string) ([]*Forward, error) {
	q := `SELECT proto, host_port, cluster, target_ip, target_port, created_at FROM forwards`
	var args []any
	if cluster != "" {
		q += ` WHERE cluster = ?`
		args = append(args, cluster)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY proto, host_port`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Forward{}
	for rows.Next() {
		var (
			f           Forward
			ip, created string
		)
		if err := rows.Scan(&f.Proto, &f.HostPort, &f.Cluster, &ip, &f.TargetPort, &created); err != nil {
			return nil, err
		}
		if f.TargetIP, err = netip.ParseAddr(ip); err != nil {
			return nil, err
		}
		if f.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

func (s *Store) DeleteForward(ctx context.Context, cluster, proto string, hostPort int) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM forwards WHERE cluster = ? AND proto = ? AND host_port = ?`, cluster, proto, hostPort)
	if err != nil {
		return err
	}
	return mustAffectKind(res, "forward", fmt.Sprintf("%s/%d", proto, hostPort))
}

func (s *Store) DeleteClusterForwards(ctx context.Context, cluster string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM forwards WHERE cluster = ?`, cluster)
	return err
}
