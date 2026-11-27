package machine

import (
	"context"
	"errors"
	"fmt"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/store"
)

func snapshotter(b backend.Backend) (backend.Snapshotter, error) {
	sn, ok := b.(backend.Snapshotter)
	if !ok {
		return nil, fmt.Errorf("backend %s cannot take snapshots: %w", b.Name(), backend.ErrNotSupported)
	}
	return sn, nil
}

// Record は保存している登録 (スナップショットに一緒に残す) を返す。
func (s *Service) Record(ctx context.Context, name string) (*store.Machine, error) {
	return s.store.GetMachine(ctx, name)
}

// SnapshotMachine は止まっているマシンのデータを dest (まだ無いディレクトリ) に複製する (cluster パッケージ用)。
func (s *Service) SnapshotMachine(ctx context.Context, name, dest string) error {
	unlock := s.locks.Lock(name)
	defer unlock()
	_, b, err := s.backendFor(ctx, name)
	if err != nil {
		return err
	}
	sn, err := snapshotter(b)
	if err != nil {
		return err
	}
	return sn.Snapshot(ctx, name, dest)
}

// RestoreClusterMachine はマシンを、スナップショットを取ったときの登録 rec とデータ src に戻す (cluster パッケージ用)。
// 登録が無ければ (スナップショットのあとに消したノード) 作り直す。復元したマシンは止まった状態になる。
func (s *Service) RestoreClusterMachine(ctx context.Context, rec *store.Machine, src string) error {
	unlock := s.locks.Lock(rec.Name)
	defer unlock()
	b, ok := s.backends[rec.Backend]
	if !ok {
		return fmt.Errorf("backend %q is not available: %w", rec.Backend, backend.ErrNotSupported)
	}
	sn, err := snapshotter(b)
	if err != nil {
		return err
	}
	_, err = s.store.GetMachine(ctx, rec.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		r := *rec
		r.DesiredState = DesiredStopped
		if err := s.store.InsertMachine(ctx, &r); err != nil {
			return err
		}
		if err := sn.Restore(ctx, rec.Name, src); err != nil {
			_ = s.store.DeleteMachine(ctx, rec.Name)
			return err
		}
		s.log.Info("machine restored (recreated)", "machine", rec.Name)
		return nil
	case err != nil:
		return err
	}
	if err := sn.Restore(ctx, rec.Name, src); err != nil {
		return err
	}
	// ディスクの大きさなども、スナップショットを取ったときに戻る
	if err := s.store.SetMachineSize(ctx, rec.Name, rec.CPUs, rec.MemoryMiB, rec.DiskGiB); err != nil {
		return err
	}
	if err := s.store.SetDesiredState(ctx, rec.Name, DesiredStopped); err != nil {
		return err
	}
	s.log.Info("machine restored", "machine", rec.Name)
	return nil
}
