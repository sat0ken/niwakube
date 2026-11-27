package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/disk"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

// スナップショットは、クラスタを止めて全ノードのデータ (ディスクなど) と NFS のディレクトリを複製したもの。
// 止めてから取るので etcd も含めて整合した状態になる。XFS なら reflink で一瞬で取れ、容量もほとんど使わない。
//
// 置き場所: <SnapshotDir>/<クラスタ>/<スナップショット>/{meta.json, machines/<ノード>/, nfs/}
// meta.json は最後に書くので、meta.json の無いディレクトリ (取っている途中か失敗したもの) は一覧に出さない。

var snapNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// SnapshotRequest はスナップショットを取るリクエスト。Name を省略すると日時 (20261127-153000) にする。
type SnapshotRequest struct {
	Name string `json:"name"`
}

// Snapshot は API で返すスナップショットの情報。
type Snapshot struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Nodes     []string  `json:"nodes"`
	NFS       bool      `json:"nfs"` // NFS の PV のデータも含むか
}

// snapshotMeta は meta.json の中身。復元に必要なクラスタの構成とノードの登録を持つ。
type snapshotMeta struct {
	Snapshot
	Cluster  string            `json:"cluster"`
	Spec     store.ClusterSpec `json:"spec"`
	Machines []store.Machine   `json:"machines"`
}

func (s *Service) snapshotsEnabled() error {
	if s.cfg.SnapshotDir == "" {
		return fmt.Errorf("snapshots are not configured: %w", backend.ErrNotSupported)
	}
	return nil
}

func (s *Service) snapshotRoot(cluster string) string {
	return filepath.Join(s.cfg.SnapshotDir, cluster)
}

func (s *Service) snapshotPath(cluster, snap string) string {
	return filepath.Join(s.snapshotRoot(cluster), snap)
}

func (s *Service) readSnapshot(cluster, snap string) (*snapshotMeta, error) {
	if !snapNameRe.MatchString(snap) {
		return nil, fmt.Errorf("snapshot %q: %w", snap, store.ErrNotFound)
	}
	raw, err := os.ReadFile(filepath.Join(s.snapshotPath(cluster, snap), "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("snapshot %s/%s: %w", cluster, snap, store.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var m snapshotMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("snapshot %s/%s: %w", cluster, snap, err)
	}
	if m.Cluster != cluster {
		return nil, fmt.Errorf("snapshot %s/%s belongs to cluster %q: %w", cluster, snap, m.Cluster, store.ErrNotFound)
	}
	return &m, nil
}

// ListSnapshots はクラスタのスナップショットを新しい順に返す。
func (s *Service) ListSnapshots(ctx context.Context, cluster string) ([]Snapshot, error) {
	if err := s.snapshotsEnabled(); err != nil {
		return nil, err
	}
	if _, err := s.store.GetCluster(ctx, cluster); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.snapshotRoot(cluster))
	if errors.Is(err, os.ErrNotExist) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := s.readSnapshot(cluster, e.Name())
		if err != nil {
			continue // 取っている途中、または失敗して残ったもの
		}
		out = append(out, m.Snapshot)
	}
	slices.SortFunc(out, func(a, b Snapshot) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// busy は別の操作が進んでいれば ErrBusy を返す。
func (s *Service) busy(name string) error {
	if op := s.operation(name); op != "" && !s.operationFailed(name) {
		return fmt.Errorf("cluster %s: %s: %w", name, op, ErrBusy)
	}
	return nil
}

// CreateSnapshot はスナップショットを取り始める。クラスタが動いていれば、いったん止めて取り、また起動する。
// 裏で進み、進み具合は Cluster.Operation に出る。
func (s *Service) CreateSnapshot(ctx context.Context, cluster string, req SnapshotRequest) (*Cluster, error) {
	if err := s.snapshotsEnabled(); err != nil {
		return nil, err
	}
	if req.Name == "" {
		req.Name = time.Now().Format("20060102-150405")
	}
	if !snapNameRe.MatchString(req.Name) {
		return nil, invalid("snapshot name must be lowercase letters, digits and '-' (up to 40)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if err := s.busy(cluster); err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.snapshotPath(cluster, req.Name)); err == nil {
		return nil, fmt.Errorf("snapshot %s/%s: %w", cluster, req.Name, store.ErrExists)
	}
	s.log.Info("cluster snapshot", "cluster", cluster, "snapshot", req.Name)
	s.setOperation(cluster, fmt.Sprintf("snapshot %s: starting", req.Name))
	go s.takeSnapshot(cluster, req.Name)
	return s.view(ctx, rec)
}

func (s *Service) takeSnapshot(cluster, snap string) {
	ctx := s.bg
	dir := s.snapshotPath(cluster, snap)
	err := s.withClusterStopped(ctx, cluster, "snapshot "+snap, func(rec *store.Cluster) error {
		ms, err := s.machines.ListCluster(ctx, cluster)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "machines"), 0o700); err != nil {
			return err
		}
		meta := snapshotMeta{
			Snapshot: Snapshot{Name: snap, CreatedAt: time.Now().UTC(), Nodes: []string{}},
			Cluster:  cluster, Spec: rec.Spec,
		}
		for i, m := range ms {
			s.setOperation(cluster, fmt.Sprintf("snapshot %s: copying %s (%d/%d)", snap, m.Name, i+1, len(ms)))
			r, err := s.machines.Record(ctx, m.Name)
			if err != nil {
				return err
			}
			if err := s.machines.SnapshotMachine(ctx, m.Name, filepath.Join(dir, "machines", m.Name)); err != nil {
				return fmt.Errorf("%s: %w", m.Name, err)
			}
			meta.Machines = append(meta.Machines, *r)
			meta.Nodes = append(meta.Nodes, m.Name)
		}
		if s.nfsEnabled() {
			if _, err := os.Stat(s.nfsDir(cluster)); err == nil {
				s.setOperation(cluster, fmt.Sprintf("snapshot %s: copying NFS data", snap))
				if err := disk.CopyTree(s.nfsDir(cluster), filepath.Join(dir, "nfs")); err != nil {
					return fmt.Errorf("nfs: %w", err)
				}
				meta.NFS = true
			}
		}
		raw, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			return err
		}
		tmp := filepath.Join(dir, "meta.json.tmp")
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(dir, "meta.json"))
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		s.log.Error("cluster snapshot failed", "cluster", cluster, "snapshot", snap, "err", err)
		s.setOperation(cluster, fmt.Sprintf("failed: snapshot %s: %v", snap, err))
		return
	}
	s.log.Info("cluster snapshot taken", "cluster", cluster, "snapshot", snap)
	s.setOperation(cluster, "")
}

// RestoreSnapshot はクラスタをスナップショットの状態に戻し始める。ノードの構成 (台数・大きさ)、
// ディスクの中身 (etcd を含む)、NFS の PV のデータが戻る。スナップショットのあとに足したノードは消え、消したノードは戻る。
// クラスタが動いていれば、戻したあとに起動する。
func (s *Service) RestoreSnapshot(ctx context.Context, cluster, snap string) (*Cluster, error) {
	if err := s.snapshotsEnabled(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, cluster)
	if err != nil {
		return nil, err
	}
	meta, err := s.readSnapshot(cluster, snap)
	if err != nil {
		return nil, err
	}
	if err := s.busy(cluster); err != nil {
		return nil, err
	}
	s.log.Info("cluster restore", "cluster", cluster, "snapshot", snap)
	s.setOperation(cluster, fmt.Sprintf("restore %s: starting", snap))
	go s.restoreSnapshot(cluster, meta)
	return s.view(ctx, rec)
}

func (s *Service) restoreSnapshot(cluster string, meta *snapshotMeta) {
	ctx := s.bg
	snap := meta.Name
	dir := s.snapshotPath(cluster, snap)
	err := s.withClusterStopped(ctx, cluster, "restore "+snap, func(*store.Cluster) error {
		ms, err := s.machines.ListCluster(ctx, cluster)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, m := range meta.Machines {
			keep[m.Name] = true
		}
		// スナップショットのあとに足したノードは消す
		for _, m := range ms {
			if !keep[m.Name] {
				s.setOperation(cluster, fmt.Sprintf("restore %s: removing %s", snap, m.Name))
				if err := s.machines.DeleteClusterMachine(ctx, m.Name); err != nil {
					return fmt.Errorf("delete %s: %w", m.Name, err)
				}
			}
		}
		for i := range meta.Machines {
			m := &meta.Machines[i]
			s.setOperation(cluster, fmt.Sprintf("restore %s: restoring %s (%d/%d)", snap, m.Name, i+1, len(meta.Machines)))
			if err := s.machines.RestoreClusterMachine(ctx, m, filepath.Join(dir, "machines", m.Name)); err != nil {
				return fmt.Errorf("%s: %w", m.Name, err)
			}
		}
		if meta.NFS && s.nfsEnabled() {
			s.setOperation(cluster, fmt.Sprintf("restore %s: restoring NFS data", snap))
			if err := backend.RestoreDir(s.nfsDir(cluster), filepath.Join(dir, "nfs")); err != nil {
				return fmt.Errorf("nfs: %w", err)
			}
		}
		return s.store.SetClusterSpec(ctx, cluster, meta.Spec)
	})
	if err != nil {
		s.log.Error("cluster restore failed", "cluster", cluster, "snapshot", snap, "err", err)
		s.setOperation(cluster, fmt.Sprintf("failed: restore %s: %v", snap, err))
		return
	}
	s.log.Info("cluster restored", "cluster", cluster, "snapshot", snap)
	s.setOperation(cluster, "")
}

// withClusterStopped はクラスタを止めて fn を動かし、もともと動いていれば (fn が失敗しても) また起動する。
func (s *Service) withClusterStopped(ctx context.Context, cluster, what string, fn func(rec *store.Cluster) error) error {
	rec, err := s.store.GetCluster(ctx, cluster)
	if err != nil {
		return err
	}
	wasRunning := rec.DesiredState == machine.DesiredRunning
	s.setOperation(cluster, what+": stopping nodes")
	if _, err := s.applyState(ctx, cluster, machine.DesiredStopped); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	ferr := fn(rec)
	if wasRunning {
		s.setOperation(cluster, what+": starting nodes")
		if _, err := s.applyState(ctx, cluster, machine.DesiredRunning); err != nil {
			return errors.Join(ferr, fmt.Errorf("start: %w", err))
		}
	}
	return ferr
}

// DeleteSnapshot はスナップショットを消す。
func (s *Service) DeleteSnapshot(ctx context.Context, cluster, snap string) error {
	if err := s.snapshotsEnabled(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.readSnapshot(cluster, snap); err != nil {
		return err
	}
	if err := s.busy(cluster); err != nil {
		return err
	}
	if err := os.RemoveAll(s.snapshotPath(cluster, snap)); err != nil {
		return err
	}
	s.log.Info("cluster snapshot deleted", "cluster", cluster, "snapshot", snap)
	return nil
}

// HasSnapshot はスナップショットがあるかを返す (練習問題用)。
func (s *Service) HasSnapshot(ctx context.Context, cluster, snap string) (bool, error) {
	if err := s.snapshotsEnabled(); err != nil {
		return false, err
	}
	_, err := s.readSnapshot(cluster, snap)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// SnapshotAndWait はスナップショットを取り、クラスタが ready に戻るまで待つ (練習問題用)。
func (s *Service) SnapshotAndWait(ctx context.Context, cluster, snap string) error {
	if _, err := s.CreateSnapshot(ctx, cluster, SnapshotRequest{Name: snap}); err != nil {
		return err
	}
	return s.waitDone(ctx, cluster)
}

// RestoreAndWait はスナップショットに戻し、クラスタが ready に戻るまで待つ (練習問題用)。
func (s *Service) RestoreAndWait(ctx context.Context, cluster, snap string) error {
	if _, err := s.RestoreSnapshot(ctx, cluster, snap); err != nil {
		return err
	}
	return s.waitDone(ctx, cluster)
}

// waitDone は操作が終わり、(動かしているクラスタなら) ready になるまで待つ。
func (s *Service) waitDone(ctx context.Context, cluster string) error {
	ctx, cancel := context.WithTimeout(ctx, s.readyWait)
	defer cancel()
	for {
		op := s.operation(cluster)
		if s.operationFailed(cluster) {
			return errors.New(op)
		}
		if op == "" {
			rec, err := s.store.GetCluster(ctx, cluster)
			if err != nil {
				return err
			}
			if rec.DesiredState != machine.DesiredRunning {
				return nil
			}
			if c, err := s.view(ctx, rec); err == nil && c.Status.Phase == "ready" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster %s did not become ready (%s): %w", cluster, op, ctx.Err())
		case <-time.After(s.postCreateInterval):
		}
	}
}
