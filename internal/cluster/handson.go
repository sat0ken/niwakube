package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/distro/kubeadm"
	"github.com/sat0ken/niwakube/internal/store"
)

// ProfileHandson は CloudNative Days のハンズオン (https://github.com/cloudnativedaysjp/cnd-handson) 用のクラスタ。
//
// ハンズオンは kind (kubeadm) で CP 1 台 + ワーカー 2 台を作り、Cilium・Envoy Gateway を入れて、
// ホストのポートを CP の NodePort につなぐ。niwakube では同じものをクラスタの作成と一緒に用意する:
//   - kubeadm、Kubernetes v1.36 (Cilium 1.20 と Envoy Gateway 1.9 が対応する範囲)、ワーカー 2 台
//   - ハンズオンの設定の Cilium と、Envoy Gateway と Gateway handson-gateway (distro/handson.go)
//   - ホストの 80・8080・8443・18080・18443 から CP の NodePort への転送
const ProfileHandson = "cnd-handson"

// applyProfile は Profile の既定値を入れ、合わない指定を断る。
func applyProfile(req *CreateRequest) error {
	switch req.Profile {
	case "":
		return nil
	case ProfileHandson:
	default:
		return invalid("unknown profile %q (choose %s)", req.Profile, ProfileHandson)
	}
	if req.Distro == "" {
		req.Distro = kubeadm.Name
	}
	if req.Distro != kubeadm.Name {
		return invalid("profile %s uses kubeadm (like kind in the hands-on), not %s", ProfileHandson, req.Distro)
	}
	if req.Version == "" {
		req.Version = distro.HandsonKubernetesVersion
	}
	if req.CNI == "" {
		req.CNI = distro.CNICilium
	}
	if req.CNI != distro.CNICilium {
		return invalid("profile %s needs cni: cilium", ProfileHandson)
	}
	if req.Hubble != nil && !*req.Hubble {
		return invalid("profile %s needs hubble", ProfileHandson)
	}
	t := true
	req.Hubble = &t
	return nil
}

// checkProfileForwards は、ハンズオンが使うホストのポートが空いているか確かめる (クラスタを作る前に)。
func (s *Service) checkProfileForwards(ctx context.Context, profile string) error {
	if profile != ProfileHandson {
		return nil
	}
	fs, err := s.store.ListForwards(ctx, "")
	if err != nil {
		return err
	}
	for _, f := range fs {
		for _, p := range distro.HandsonNodePorts {
			if f.Proto == "tcp" && f.HostPort == p.HostPort {
				return fmt.Errorf("host port %d (%s) is already forwarded to cluster %s; delete that forward or cluster first: %w",
					p.HostPort, p.What, f.Cluster, store.ErrExists)
			}
		}
	}
	return nil
}

// addProfileForwards はハンズオンのポートの転送を作る (ホスト → CP の NodePort)。
func (s *Service) addProfileForwards(ctx context.Context, rec *store.Cluster) error {
	if rec.Spec.Profile != ProfileHandson {
		return nil
	}
	cp := s.cpNode(rec, 1).IP
	for _, p := range distro.HandsonNodePorts {
		f := &store.Forward{Proto: "tcp", HostPort: p.HostPort, Cluster: rec.Name, TargetIP: cp, TargetPort: p.NodePort, CreatedAt: time.Now()}
		if err := s.store.InsertForward(ctx, f); err != nil {
			return err
		}
	}
	if err := s.applyForwards(ctx); err != nil {
		return fmt.Errorf("apply forwards: %w", err)
	}
	s.log.Info("hands-on port forwards added", "cluster", rec.Name, "target", cp)
	return nil
}

// HandsonHostnames は、ハンズオン (chapter_setup) が PC の hosts に書く名前。
var HandsonHostnames = []string{
	"hello-world.example.com", "rollout.example.com", "blue-green.example.com", "app.example.com",
	"prometheus.example.com", "grafana.example.com", "jaeger.example.com", "argocd.example.com",
	"app-argocd.example.com", "app-cicd.example.com", "dev-kustomize-argocd.example.com", "prd-kustomize-argocd.example.com",
	"helm-argocd.example.com", "app-preview-argocd.example.com", "headlamp.example.com", "argocdupdate.example.com",
	"app-rollouts.example.com", "app-preview-rollouts.example.com", "kiali.example.com", "kiali-ambient.example.com",
	"app-cilium.example.com", "hubble-cilium.example.com", "pyroscope.example.com", "troubleshoot.example.com",
	"cnd-web.example.com",
}

// TerminalOptions は Web UI のターミナルに渡す、クラスタごとの設定。
type TerminalOptions struct {
	// HomeID が空でなければ、ターミナルのホームを閉じても残す (ハンズオンは一日かけて同じホームで進めるため)
	HomeID string
	// HostIP と HostNames は、ターミナルの /etc/hosts に足す名前 (ハンズオンの *.example.com をホストの転送に向ける)
	HostIP    netip.Addr
	HostNames []string
}

func homeID(rec *store.Cluster) string { return fmt.Sprintf("%s-%d", rec.Name, rec.CreatedAt.Unix()) }

// gateway はブリッジのホスト側のアドレス。ここ宛ての接続はホストのポートの転送 (DNAT) を通る。
func (s *Service) gateway() netip.Addr {
	if s.cfg.Gateway.IsValid() {
		return s.cfg.Gateway
	}
	return s.hostIP(0, 1)
}

// TerminalOptions はクラスタのターミナルの設定を返す。
func (s *Service) TerminalOptions(ctx context.Context, name string) (TerminalOptions, error) {
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return TerminalOptions{}, err
	}
	if rec.Spec.Profile != ProfileHandson {
		return TerminalOptions{}, nil
	}
	return TerminalOptions{HomeID: homeID(rec), HostIP: s.gateway(), HostNames: HandsonHostnames}, nil
}

// TerminalHomes は、ターミナルのホームを残すクラスタの HomeID の一覧 (消したクラスタのホームを消すのに使う)。
func (s *Service) TerminalHomes(ctx context.Context) ([]string, error) {
	recs, err := s.store.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range recs {
		if r.Spec.Profile == ProfileHandson {
			out = append(out, homeID(r))
		}
	}
	return out, nil
}
