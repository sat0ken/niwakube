package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/kube"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

// ErrBusy はクラスタで別の操作 (ノードの大きさの変更など) が進んでいるときのエラー。
var ErrBusy = errors.New("another operation is in progress on this cluster")

// NodeOps はノードを順番に再起動するときの Kubernetes の操作 (テストで差し替える)。
type NodeOps interface {
	Cordon(ctx context.Context, kubeconfig []byte, node string, unschedulable bool) error
	Drain(ctx context.Context, kubeconfig []byte, node string) error
	ReadySince(ctx context.Context, kubeconfig []byte, node string, since time.Time) (bool, error)
}

// kubeNodeOps は kube.Nodes を使う NodeOps。
type kubeNodeOps struct{}

func (kubeNodeOps) Cordon(ctx context.Context, kc []byte, node string, on bool) error {
	n, err := kube.NewNodes(kc)
	if err != nil {
		return err
	}
	return n.Cordon(ctx, node, on)
}

func (kubeNodeOps) Drain(ctx context.Context, kc []byte, node string) error {
	n, err := kube.NewNodes(kc)
	if err != nil {
		return err
	}
	return n.Drain(ctx, node)
}

func (kubeNodeOps) ReadySince(ctx context.Context, kc []byte, node string, since time.Time) (bool, error) {
	n, err := kube.NewNodes(kc)
	if err != nil {
		return false, err
	}
	return n.ReadySince(ctx, node, since)
}

// ResizeRequest はクラスタのノードの大きさの変更。Size の 0 の項目は変えない。ディスクは広げるだけ。
type ResizeRequest struct {
	Role string         `json:"role"` // control-plane | worker
	Size store.NodeSize `json:"size"`
}

// Resize はロール (control-plane / worker) のノードの大きさを変える。
// クラスタの構成 (これから足すノードの大きさ) を更新し、裏で 1 台ずつ次を行う:
// cordon → drain → 止めて大きさを変えて起動 → Ready に戻るのを待つ → uncordon。
// 進み具合は Cluster.Operation に出る。
func (s *Service) Resize(ctx context.Context, name string, req ResizeRequest) (*Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	var cur *store.NodeSize
	switch distro.Role(req.Role) {
	case distro.RoleControlPlane:
		cur = &rec.Spec.ControlPlane
	case distro.RoleWorker:
		cur = &rec.Spec.Worker
	default:
		return nil, invalid("role must be %s or %s", distro.RoleControlPlane, distro.RoleWorker)
	}
	want := *cur
	if req.Size.CPUs != 0 {
		want.CPUs = req.Size.CPUs
	}
	if req.Size.MemoryMiB != 0 {
		want.MemoryMiB = req.Size.MemoryMiB
	}
	if req.Size.DiskGiB != 0 {
		want.DiskGiB = req.Size.DiskGiB
	}
	if want.DiskGiB < cur.DiskGiB {
		return nil, invalid("diskGiB cannot shrink (%d -> %d)", cur.DiskGiB, want.DiskGiB)
	}
	if err := validSize(want); err != nil {
		return nil, err
	}
	if s.operation(name) != "" && !s.operationFailed(name) {
		return nil, fmt.Errorf("cluster %s: %s: %w", name, s.operation(name), ErrBusy)
	}

	ms, err := s.machines.ListCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	var targets []*machine.Machine
	for _, m := range ms {
		if m.Role == req.Role && (m.CPUs != want.CPUs || m.MemoryMiB != want.MemoryMiB || m.DiskGiB < want.DiskGiB) {
			targets = append(targets, m)
		}
	}
	slices.SortFunc(targets, func(a, b *machine.Machine) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})

	*cur = want
	if err := s.store.SetClusterSpec(ctx, name, rec.Spec); err != nil {
		return nil, err
	}
	s.log.Info("cluster resize", "cluster", name, "role", req.Role, "size", want, "nodes", len(targets))
	if len(targets) > 0 {
		s.setOperation(name, fmt.Sprintf("resizing %s nodes (0/%d)", req.Role, len(targets)))
		go s.rollResize(name, targets, want)
	}
	return s.view(ctx, rec)
}

func validSize(sz store.NodeSize) error {
	switch {
	case sz.CPUs < 1 || sz.CPUs > 32:
		return invalid("cpus must be between 1 and 32")
	case sz.MemoryMiB < 512 || sz.MemoryMiB > 64*1024:
		return invalid("memoryMiB must be between 512 and 65536")
	case sz.DiskGiB < 5 || sz.DiskGiB > 1024:
		return invalid("diskGiB must be between 5 and 1024")
	}
	return nil
}

// rollResize はノードを 1 台ずつ drain して大きさを変え、Ready に戻してから次へ進む。
func (s *Service) rollResize(cluster string, targets []*machine.Machine, size store.NodeSize) {
	ctx := s.bg
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		s.log.Error("cluster resize failed", "cluster", cluster, "err", msg)
		s.setOperation(cluster, "failed: "+msg)
	}
	kc, err := s.Kubeconfig(ctx, cluster)
	if err != nil {
		fail("kubeconfig: %v", err)
		return
	}
	for i, m := range targets {
		s.setOperation(cluster, fmt.Sprintf("resizing %s (%d/%d)", m.Name, i+1, len(targets)))
		running := m.DesiredState == machine.DesiredRunning
		cordoned := false
		if running {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.nodeOps.Cordon(cctx, kc, m.Name, true)
			cancel()
			if err != nil {
				// API に届かない (CP が 1 台で止まっているなど)。drain せずに再起動する
				s.log.Warn("cordon failed; restarting without drain", "cluster", cluster, "node", m.Name, "err", err)
			} else {
				cordoned = true
				dctx, cancel := context.WithTimeout(ctx, s.drainTimeout)
				err := s.nodeOps.Drain(dctx, kc, m.Name)
				cancel()
				if err != nil {
					// Pod が残っていても再起動で止まる。止まったままにはしない
					s.log.Warn("drain did not finish; restarting anyway", "cluster", cluster, "node", m.Name, "err", err)
				}
			}
		}
		disk := max(size.DiskGiB, m.DiskGiB)
		since := time.Now()
		if _, err := s.machines.ResizeClusterMachine(ctx, m.Name, machine.ResizeRequest{CPUs: size.CPUs, MemoryMiB: size.MemoryMiB, DiskGiB: disk}); err != nil {
			fail("%s: %v", m.Name, err)
			return
		}
		if !running {
			continue
		}
		if err := s.waitNodeReady(ctx, kc, m.Name, since); err != nil {
			fail("%s did not become Ready: %v", m.Name, err)
			return
		}
		if cordoned {
			if err := s.retry(ctx, func(c context.Context) error { return s.nodeOps.Cordon(c, kc, m.Name, false) }); err != nil {
				fail("uncordon %s: %v", m.Name, err)
				return
			}
		}
		s.log.Info("node resized", "cluster", cluster, "node", m.Name)
	}
	s.setOperation(cluster, "")
}

func (s *Service) waitNodeReady(ctx context.Context, kc []byte, node string, since time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, s.nodeReadyTimeout)
	defer cancel()
	var last error
	for {
		rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
		ok, err := s.nodeOps.ReadySince(rctx, kc, node, since)
		rcancel()
		if ok {
			return nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
			}
			return ctx.Err()
		case <-time.After(s.postCreateInterval):
		}
	}
}

// retry は fn を数回試す (API がまだ不安定なときのため)。
func (s *Service) retry(ctx context.Context, fn func(context.Context) error) error {
	var err error
	for range 5 {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = fn(c)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(s.postCreateInterval):
		}
	}
	return err
}

func (s *Service) setOperation(name, op string) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if op == "" {
		delete(s.opState, name)
	} else {
		s.opState[name] = op
	}
}

func (s *Service) operation(name string) string {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.opState[name]
}

func (s *Service) operationFailed(name string) bool {
	op := s.operation(name)
	return len(op) >= 7 && op[:7] == "failed:"
}
