package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/store"
)

// ForwardRequest はホストのポートをクラスタの IP (LoadBalancer の IP など) に転送するリクエスト。
type ForwardRequest struct {
	Proto    string `json:"proto,omitempty"` // tcp (既定) | udp
	HostPort int    `json:"hostPort"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
}

// Forward は API で返す転送。
type Forward struct {
	Proto     string    `json:"proto"`
	HostPort  int       `json:"hostPort"`
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	CreatedAt time.Time `json:"createdAt"`
}

func forwardView(f *store.Forward) Forward {
	return Forward{Proto: f.Proto, HostPort: f.HostPort, IP: f.TargetIP.String(), Port: f.TargetPort, CreatedAt: f.CreatedAt}
}

// AddForward はホストのポートからクラスタの IP への転送を追加する (LAN の PC から NAT の内側のクラスタに届かせる)。
func (s *Service) AddForward(ctx context.Context, name string, req ForwardRequest) (*Forward, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	if req.Proto == "" {
		req.Proto = "tcp"
	}
	if req.Proto != "tcp" && req.Proto != "udp" {
		return nil, invalid("proto must be tcp or udp")
	}
	if req.HostPort < 1 || req.HostPort > 65535 || req.Port < 1 || req.Port > 65535 {
		return nil, invalid("ports must be between 1 and 65535")
	}
	if req.HostPort == 22 {
		return nil, invalid("host port 22 is reserved for SSH to the host")
	}
	ip, err := netip.ParseAddr(req.IP)
	if err != nil {
		return nil, invalid("ip: %v", err)
	}
	if !s.subnetOf(rec.Index).Contains(ip) {
		return nil, invalid("ip %s is not in the cluster subnet %s", ip, s.subnetOf(rec.Index))
	}
	f := &store.Forward{Proto: req.Proto, HostPort: req.HostPort, Cluster: name, TargetIP: ip, TargetPort: req.Port, CreatedAt: time.Now()}
	if err := s.store.InsertForward(ctx, f); err != nil {
		return nil, err
	}
	if err := s.applyForwards(ctx); err != nil {
		_ = s.store.DeleteForward(context.WithoutCancel(ctx), name, f.Proto, f.HostPort)
		return nil, err
	}
	s.log.Info("port forward added", "cluster", name, "proto", f.Proto, "hostPort", f.HostPort, "target", fmt.Sprintf("%s:%d", ip, req.Port))
	v := forwardView(f)
	return &v, nil
}

func (s *Service) ListForwards(ctx context.Context, name string) ([]Forward, error) {
	if _, err := s.store.GetCluster(ctx, name); err != nil {
		return nil, err
	}
	fs, err := s.store.ListForwards(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]Forward, 0, len(fs))
	for _, f := range fs {
		out = append(out, forwardView(f))
	}
	return out, nil
}

func (s *Service) DeleteForward(ctx context.Context, name, proto string, hostPort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.DeleteForward(ctx, name, proto, hostPort); err != nil {
		return err
	}
	return s.applyForwards(ctx)
}

// applyForwards は保存されているすべての転送を nftables に反映する。
func (s *Service) applyForwards(ctx context.Context) error {
	if s.cfg.Forwarder == nil {
		return nil
	}
	fs, err := s.store.ListForwards(ctx, "")
	if err != nil {
		return err
	}
	pfs := make([]netx.PortForward, 0, len(fs))
	for _, f := range fs {
		pfs = append(pfs, netx.PortForward{Proto: f.Proto, HostPort: f.HostPort, TargetIP: f.TargetIP, TargetPort: f.TargetPort})
	}
	return s.cfg.Forwarder.Apply(ctx, pfs)
}
