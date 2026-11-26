package cluster

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

// ScaleRequest はワーカーの台数の変更。
type ScaleRequest struct {
	Workers *int `json:"workers"` // 0 台もあり得るのでポインタ
}

// Scale はワーカーの台数を変える。クラスタは動いている (desired running) こと。
//   - 増やす: 空いている番号のワーカーを、クラスタを作ったときと同じ参加の設定で作って起動する
//   - 減らす: 番号の大きいワーカーから 1 台ずつ cordon → drain → Node を消す → マシンを消す
//
// 裏で進め、進み具合は Cluster.Operation に出る。台数 (Spec.Workers) は 1 台ごとに更新する。
func (s *Service) Scale(ctx context.Context, name string, req ScaleRequest) (*Cluster, error) {
	if req.Workers == nil {
		return nil, invalid("workers is required")
	}
	want := *req.Workers
	if want < 0 || want > maxWorkers {
		return nil, invalid("workers must be between 0 and %d", maxWorkers)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec.DesiredState != machine.DesiredRunning {
		return nil, invalid("start the cluster before changing the number of workers")
	}
	if s.operation(name) != "" && !s.operationFailed(name) {
		return nil, fmt.Errorf("cluster %s: %s: %w", name, s.operation(name), ErrBusy)
	}
	workers, err := s.workerMachines(ctx, name)
	if err != nil {
		return nil, err
	}
	have := len(workers)
	switch {
	case want > have:
		dc, d, err := s.distroCluster(rec)
		if err != nil {
			return nil, err
		}
		used := map[string]bool{}
		for _, m := range workers {
			used[m.Name] = true
		}
		var nodes []distro.Node
		for i := 1; len(nodes) < want-have; i++ {
			if n := s.workerNode(rec, i); !used[n.Name] {
				nodes = append(nodes, n)
			}
		}
		s.log.Info("cluster scale up", "cluster", name, "workers", fmt.Sprintf("%d -> %d", have, want))
		s.setOperation(name, fmt.Sprintf("adding workers (0/%d)", len(nodes)))
		go s.addWorkers(rec, d, dc, nodes)
	case want < have:
		// 番号の大きいものから消す
		slices.SortFunc(workers, func(a, b *machine.Machine) int {
			return workerIndex(name, b.Name) - workerIndex(name, a.Name)
		})
		targets := workers[:have-want]
		s.log.Info("cluster scale down", "cluster", name, "workers", fmt.Sprintf("%d -> %d", have, want))
		s.setOperation(name, fmt.Sprintf("removing workers (0/%d)", len(targets)))
		go s.removeWorkers(name, targets)
	default:
		if rec.Spec.Workers != have {
			rec.Spec.Workers = have
			if err := s.store.SetClusterSpec(ctx, name, rec.Spec); err != nil {
				return nil, err
			}
		}
	}
	return s.view(ctx, rec)
}

func (s *Service) workerMachines(ctx context.Context, cluster string) ([]*machine.Machine, error) {
	ms, err := s.machines.ListCluster(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var out []*machine.Machine
	for _, m := range ms {
		if m.Role == string(distro.RoleWorker) {
			out = append(out, m)
		}
	}
	return out, nil
}

// workerIndex は <cluster>-worker-<i> の i を返す (形が違えば 0)。
func workerIndex(cluster, name string) int {
	i, err := strconv.Atoi(strings.TrimPrefix(name, cluster+"-worker-"))
	if err != nil {
		return 0
	}
	return i
}

// syncWorkers はクラスタの構成のワーカー数を、実際にあるワーカーのマシンの数に合わせる。
func (s *Service) syncWorkers(ctx context.Context, cluster string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, cluster)
	if err != nil {
		return // 途中でクラスタが消された
	}
	workers, err := s.workerMachines(ctx, cluster)
	if err != nil {
		s.log.Error("count workers", "cluster", cluster, "err", err)
		return
	}
	if rec.Spec.Workers == len(workers) {
		return
	}
	rec.Spec.Workers = len(workers)
	if err := s.store.SetClusterSpec(ctx, cluster, rec.Spec); err != nil {
		s.log.Error("update worker count", "cluster", cluster, "err", err)
	}
}

func (s *Service) addWorkers(rec *store.Cluster, d distro.Distro, dc *distro.Cluster, nodes []distro.Node) {
	ctx := s.bg
	for i, n := range nodes {
		s.setOperation(rec.Name, fmt.Sprintf("adding %s (%d/%d)", n.Name, i+1, len(nodes)))
		err := s.createNode(ctx, d, dc, rec, n, rec.Spec.Worker)
		s.syncWorkers(ctx, rec.Name)
		if err != nil {
			s.log.Error("cluster scale up failed", "cluster", rec.Name, "err", err)
			s.setOperation(rec.Name, "failed: "+err.Error())
			return
		}
		s.log.Info("worker added", "cluster", rec.Name, "node", n.Name)
	}
	s.setOperation(rec.Name, "")
}

func (s *Service) removeWorkers(cluster string, targets []*machine.Machine) {
	ctx := s.bg
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		s.log.Error("cluster scale down failed", "cluster", cluster, "err", msg)
		s.setOperation(cluster, "failed: "+msg)
	}
	kc, err := s.Kubeconfig(ctx, cluster)
	if err != nil {
		fail("kubeconfig: %v", err)
		return
	}
	for i, m := range targets {
		s.setOperation(cluster, fmt.Sprintf("removing %s (%d/%d)", m.Name, i+1, len(targets)))
		// Node を消せないままマシンだけ消すと、NotReady の Node が残る (同じ名前で足し直すときにも困る)。
		// API に届かないときはここで止める
		if err := s.retry(ctx, func(c context.Context) error { return s.nodeOps.Cordon(c, kc, m.Name, true) }); err != nil {
			fail("cordon %s (the Kubernetes API must be reachable): %v", m.Name, err)
			return
		}
		dctx, cancel := context.WithTimeout(ctx, s.drainTimeout)
		err := s.nodeOps.Drain(dctx, kc, m.Name)
		cancel()
		if err != nil {
			// 追い出せない Pod (PodDisruptionBudget など) があっても、台数を減らすことを優先する
			s.log.Warn("drain did not finish; removing anyway", "cluster", cluster, "node", m.Name, "err", err)
		}
		if err := s.retry(ctx, func(c context.Context) error { return s.nodeOps.DeleteNode(c, kc, m.Name) }); err != nil {
			fail("delete node %s: %v", m.Name, err)
			return
		}
		dctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		err = s.machines.DeleteClusterMachine(dctx, m.Name)
		cancel()
		s.syncWorkers(ctx, cluster)
		if err != nil {
			fail("delete machine %s: %v", m.Name, err)
			return
		}
		s.log.Info("worker removed", "cluster", cluster, "node", m.Name)
	}
	s.setOperation(cluster, "")
}
