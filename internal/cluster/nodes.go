package cluster

import (
	"context"
	"fmt"
	"slices"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/distro"
)

// NodeUser はノードに SSH で入るユーザー (cloud-init で作る。sudo できる)。
const NodeUser = "ubuntu"

// Node はクラスタのノード 1 台 (SSH の接続先)。
type Node struct {
	Name  string
	Alias string // cp、worker (1 台目)、cp2、worker2 ... (練習問題とターミナルの ssh で使う別名)
	Role  distro.Role
	IP    string
}

// NodeAccess はクラスタのノードに SSH で入るための情報。
type NodeAccess struct {
	Distro string
	User   string
	Key    string // 秘密鍵 (OpenSSH 形式)
	Nodes  []Node // CP、ワーカーの順 (それぞれ名前順)
}

// NodeAccess はノードに入るための鍵と、ノードの一覧を返す。
// 鍵の無いクラスタ (この機能より前に作ったもの) と、SSH の無い Talos は backend.ErrNotSupported。
func (s *Service) NodeAccess(ctx context.Context, name string) (*NodeAccess, error) {
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	sec, _, err := s.secrets(rec)
	if err != nil {
		return nil, err
	}
	if rec.Distro == "talos" {
		return nil, fmt.Errorf("talos nodes have no ssh: %w", backend.ErrNotSupported)
	}
	if sec.NodeSSHKey == "" {
		return nil, fmt.Errorf("cluster %s was created before node ssh keys existed (create it again): %w", name, backend.ErrNotSupported)
	}
	ms, err := s.machines.ListCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	a := &NodeAccess{Distro: rec.Distro, User: NodeUser, Key: sec.NodeSSHKey}
	for _, m := range ms {
		a.Nodes = append(a.Nodes, Node{Name: m.Name, Role: distro.Role(m.Role), IP: m.IP})
	}
	rank := func(n Node) int {
		if n.Role == distro.RoleControlPlane {
			return 0
		}
		return 1
	}
	slices.SortFunc(a.Nodes, func(x, y Node) int {
		if d := rank(x) - rank(y); d != 0 {
			return d
		}
		if x.Role == distro.RoleWorker && y.Role == distro.RoleWorker {
			return workerIndex(name, x.Name) - workerIndex(name, y.Name)
		}
		if x.Name < y.Name {
			return -1
		}
		return 1
	})
	nCP, nW := 0, 0
	for i := range a.Nodes {
		if a.Nodes[i].Role == distro.RoleControlPlane {
			nCP++
			a.Nodes[i].Alias = alias("cp", nCP)
		} else {
			nW++
			a.Nodes[i].Alias = alias("worker", nW)
		}
	}
	return a, nil
}

func alias(base string, i int) string {
	if i == 1 {
		return base
	}
	return fmt.Sprintf("%s%d", base, i)
}

// DistroOf はクラスタのディストリの名前と、作ったときのバージョンの指定 (v1.36、stable など) を返す。
func (s *Service) DistroOf(ctx context.Context, name string) (distroName, version string, err error) {
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return "", "", err
	}
	return rec.Distro, rec.Spec.Version, nil
}
