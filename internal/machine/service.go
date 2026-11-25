// Package machine は VM / コンテナの作成・削除をまとめる。
// store (望ましい状態)、ipam、seed、backend (実体) をつなぐ層。
package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/seed"
	"github.com/sat0ken/niwakube/internal/store"
)

var (
	// ErrInvalid はリクエストの値が不正なときのエラー。
	ErrInvalid = errors.New("invalid request")
	// ErrPartOfCluster はクラスタに属するマシンを単体で消そうとしたときのエラー。
	ErrPartOfCluster = errors.New("machine belongs to a cluster; delete the cluster instead")
)

const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
)

type Config struct {
	Bridge         string
	Subnet         netip.Prefix // VM が属するサブネット (例: 172.30.0.0/16)
	Gateway        netip.Addr
	Pool           ipam.Pool // 単体 VM 用のアドレス範囲
	DNS            []netip.Addr
	DefaultSSHKeys []string // すべての VM に入れる鍵 (niwakube 自身の鍵など)
	DefaultBackend string
}

type Service struct {
	cfg      Config
	store    *store.Store
	backends map[string]backend.Backend
	log      *slog.Logger

	allocMu sync.Mutex // IP / MAC の払い出しを直列化する
	locks   keyedMutex // マシン名ごとの操作を直列化する
}

func NewService(cfg Config, st *store.Store, log *slog.Logger, backends ...backend.Backend) (*Service, error) {
	if len(backends) == 0 {
		return nil, errors.New("at least one backend is required")
	}
	m := map[string]backend.Backend{}
	for _, b := range backends {
		m[b.Name()] = b
	}
	if cfg.DefaultBackend == "" {
		cfg.DefaultBackend = backends[0].Name()
	}
	if _, ok := m[cfg.DefaultBackend]; !ok {
		return nil, fmt.Errorf("default backend %q is not registered", cfg.DefaultBackend)
	}
	if !cfg.Subnet.Contains(cfg.Pool.Start) || !cfg.Subnet.Contains(cfg.Pool.End) {
		return nil, fmt.Errorf("pool %s-%s is outside subnet %s", cfg.Pool.Start, cfg.Pool.End, cfg.Subnet)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, store: st, backends: m, log: log}, nil
}

// BackendInfo は利用できるバックエンドの一覧に使う。
type BackendInfo struct {
	Name         string               `json:"name"`
	Default      bool                 `json:"default"`
	Capabilities backend.Capabilities `json:"capabilities"`
}

func (s *Service) Backends() []BackendInfo {
	out := make([]BackendInfo, 0, len(s.backends))
	for name, b := range s.backends {
		out = append(out, BackendInfo{Name: name, Default: name == s.cfg.DefaultBackend, Capabilities: b.Capabilities()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CreateRequest は VM 作成のリクエスト。ゼロ値の項目には既定値が入る。
type CreateRequest struct {
	Name              string   `json:"name"`
	Backend           string   `json:"backend,omitempty"`
	Image             string   `json:"image,omitempty"`
	CPUs              int      `json:"cpus,omitempty"`
	MemoryMiB         int      `json:"memoryMiB,omitempty"`
	DiskGiB           int      `json:"diskGiB,omitempty"`
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`

	// 以下はクラスタのノードを作るときに cluster パッケージが使う (API からは指定できない)
	IP       netip.Addr     `json:"-"` // 固定の IP。ゼロ値ならプールから払い出す
	Cluster  string         `json:"-"`
	Role     string         `json:"-"`
	UserData map[string]any `json:"-"` // cloud-init の user-data に追加するキー
	// RawUserData を指定すると cloud-init の user-data を作らず、これをそのまま渡す (Talos の machine config)
	RawUserData []byte `json:"-"`
}

// Machine は API で返す VM の情報 (望ましい状態 + 現在の状態)。
type Machine struct {
	Name         string         `json:"name"`
	Backend      string         `json:"backend"`
	Image        string         `json:"image"`
	CPUs         int            `json:"cpus"`
	MemoryMiB    int            `json:"memoryMiB"`
	DiskGiB      int            `json:"diskGiB"`
	IP           string         `json:"ip"`
	MAC          string         `json:"mac"`
	DesiredState string         `json:"desiredState"`
	Cluster      string         `json:"cluster,omitempty"`
	Role         string         `json:"role,omitempty"`
	Status       backend.Status `json:"status"`
	CreatedAt    time.Time      `json:"createdAt"`
}

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	imageRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

func (s *Service) normalize(req *CreateRequest) error {
	if !nameRe.MatchString(req.Name) {
		return fmt.Errorf("%w: name must match %s", ErrInvalid, nameRe)
	}
	if req.Backend == "" {
		req.Backend = s.cfg.DefaultBackend
	}
	if _, ok := s.backends[req.Backend]; !ok {
		return fmt.Errorf("%w: unknown backend %q", ErrInvalid, req.Backend)
	}
	if req.Image == "" {
		req.Image = "ubuntu-26.04"
	}
	if !imageRe.MatchString(req.Image) {
		return fmt.Errorf("%w: invalid image name %q", ErrInvalid, req.Image)
	}
	setDefault(&req.CPUs, 2)
	setDefault(&req.MemoryMiB, 4096)
	setDefault(&req.DiskGiB, 20)
	if err := inRange("cpus", req.CPUs, 1, 32); err != nil {
		return err
	}
	if err := inRange("memoryMiB", req.MemoryMiB, 512, 64*1024); err != nil {
		return err
	}
	return inRange("diskGiB", req.DiskGiB, 5, 1024)
}

func setDefault(v *int, def int) {
	if *v == 0 {
		*v = def
	}
}

func inRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%w: %s must be between %d and %d", ErrInvalid, field, lo, hi)
	}
	return nil
}

// allocate は名前の重複を確認し、IP と MAC を払い出して store に登録する。
func (s *Service) allocate(ctx context.Context, req *CreateRequest) (*store.Machine, error) {
	s.allocMu.Lock()
	defer s.allocMu.Unlock()

	existing, err := s.store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	usedIP := map[netip.Addr]bool{}
	usedMAC := map[string]bool{}
	for _, m := range existing {
		if m.Name == req.Name {
			return nil, fmt.Errorf("machine %q: %w", req.Name, store.ErrExists)
		}
		usedIP[m.IP] = true
		usedMAC[m.MAC] = true
	}
	ip := req.IP
	if ip.IsValid() {
		if !s.cfg.Subnet.Contains(ip) || ip == s.cfg.Gateway {
			return nil, fmt.Errorf("%w: ip %s is not usable in %s", ErrInvalid, ip, s.cfg.Subnet)
		}
		if usedIP[ip] {
			return nil, fmt.Errorf("ip %s: %w", ip, store.ErrExists)
		}
	} else if ip, err = s.cfg.Pool.Allocate(usedIP); err != nil {
		return nil, err
	}
	var mac string
	for {
		if mac, err = ipam.RandomMAC(); err != nil {
			return nil, err
		}
		if !usedMAC[mac] {
			break
		}
	}
	m := &store.Machine{
		Name: req.Name, Backend: req.Backend, Image: req.Image,
		CPUs: req.CPUs, MemoryMiB: req.MemoryMiB, DiskGiB: req.DiskGiB,
		IP: ip, MAC: mac, SSHKeys: req.SSHAuthorizedKeys,
		DesiredState: DesiredRunning, Cluster: req.Cluster, Role: req.Role, CreatedAt: time.Now(),
	}
	if err := s.store.InsertMachine(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) spec(m *store.Machine, extra map[string]any, raw []byte) (*backend.MachineSpec, error) {
	user := raw
	if user == nil {
		keys := append(append([]string{}, s.cfg.DefaultSSHKeys...), m.SSHKeys...)
		var err error
		if user, err = seed.UserData(seed.User{SSHAuthorizedKeys: keys, Extra: extra}); err != nil {
			return nil, err
		}
	}
	meta, err := seed.MetaData(m.Name+"-"+strconv.FormatInt(m.CreatedAt.Unix(), 10), m.Name)
	if err != nil {
		return nil, err
	}
	addr := netip.PrefixFrom(m.IP, s.cfg.Subnet.Bits())
	netcfg, err := seed.NetworkConfig(seed.Network{MAC: m.MAC, Address: addr, Gateway: s.cfg.Gateway, DNS: s.cfg.DNS})
	if err != nil {
		return nil, err
	}
	return &backend.MachineSpec{
		Name: m.Name, CPUs: m.CPUs, MemoryMiB: m.MemoryMiB, DiskGiB: m.DiskGiB, Image: m.Image,
		NIC:  backend.NIC{Bridge: s.cfg.Bridge, MAC: m.MAC, Address: addr, Gateway: s.cfg.Gateway},
		Seed: seed.Data{UserData: user, MetaData: meta, NetworkConfig: netcfg},
	}, nil
}

// Create は VM を登録し、作成して起動する。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Machine, error) {
	if err := s.normalize(&req); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(req.Name)
	defer unlock()

	m, err := s.allocate(ctx, &req)
	if err != nil {
		return nil, err
	}
	b := s.backends[m.Backend]
	spec, err := s.spec(m, req.UserData, req.RawUserData)
	if err == nil {
		err = b.Create(ctx, spec)
	}
	if err != nil {
		// 実体が作れなかったら登録も取り消す (IP を解放する)
		if derr := s.store.DeleteMachine(context.WithoutCancel(ctx), m.Name); derr != nil {
			s.log.Error("rollback failed", "machine", m.Name, "err", derr)
		}
		return nil, err
	}
	s.log.Info("machine created", "machine", m.Name, "backend", m.Backend, "ip", m.IP)

	if err := b.Start(ctx, m.Name); err != nil {
		// 実体は残す。原因を直してから start するか delete できる
		return s.view(ctx, m), fmt.Errorf("created but failed to start: %w", err)
	}
	s.log.Info("machine started", "machine", m.Name)
	return s.view(ctx, m), nil
}

func (s *Service) view(ctx context.Context, m *store.Machine) *Machine {
	v := &Machine{
		Name: m.Name, Backend: m.Backend, Image: m.Image, CPUs: m.CPUs, MemoryMiB: m.MemoryMiB, DiskGiB: m.DiskGiB,
		IP: m.IP.String(), MAC: m.MAC, DesiredState: m.DesiredState, Cluster: m.Cluster, Role: m.Role,
		CreatedAt: m.CreatedAt,
	}
	b, ok := s.backends[m.Backend]
	if !ok {
		v.Status = backend.Status{State: backend.StateUnknown, Detail: "backend " + m.Backend + " is not available"}
		return v
	}
	st, err := b.Status(ctx, m.Name)
	if err != nil {
		st = backend.Status{State: backend.StateUnknown, Detail: err.Error()}
	}
	v.Status = st
	return v
}

func (s *Service) List(ctx context.Context) ([]*Machine, error) {
	ms, err := s.store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Machine, 0, len(ms))
	for _, m := range ms {
		out = append(out, s.view(ctx, m))
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, name string) (*Machine, error) {
	m, err := s.store.GetMachine(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, m), nil
}

func (s *Service) backendFor(ctx context.Context, name string) (*store.Machine, backend.Backend, error) {
	m, err := s.store.GetMachine(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	b, ok := s.backends[m.Backend]
	if !ok {
		return nil, nil, fmt.Errorf("backend %q is not available: %w", m.Backend, backend.ErrNotSupported)
	}
	return m, b, nil
}

func (s *Service) Start(ctx context.Context, name string) (*Machine, error) {
	unlock := s.locks.Lock(name)
	defer unlock()
	m, b, err := s.backendFor(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetDesiredState(ctx, name, DesiredRunning); err != nil {
		return nil, err
	}
	m.DesiredState = DesiredRunning
	if err := b.Start(ctx, name); err != nil {
		return nil, err
	}
	return s.view(ctx, m), nil
}

func (s *Service) Stop(ctx context.Context, name string, force bool) (*Machine, error) {
	unlock := s.locks.Lock(name)
	defer unlock()
	m, b, err := s.backendFor(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetDesiredState(ctx, name, DesiredStopped); err != nil {
		return nil, err
	}
	m.DesiredState = DesiredStopped
	if err := b.Stop(ctx, name, force); err != nil {
		return nil, err
	}
	return s.view(ctx, m), nil
}

// Delete は単体の VM を削除する。クラスタに属するマシンは消せない (クラスタごと消す)。
func (s *Service) Delete(ctx context.Context, name string) error {
	return s.delete(ctx, name, false)
}

// DeleteClusterMachine はクラスタに属するマシンを削除する (cluster パッケージ用)。
func (s *Service) DeleteClusterMachine(ctx context.Context, name string) error {
	return s.delete(ctx, name, true)
}

func (s *Service) delete(ctx context.Context, name string, allowCluster bool) error {
	unlock := s.locks.Lock(name)
	defer unlock()
	m, b, err := s.backendFor(ctx, name)
	if err != nil {
		return err
	}
	if m.Cluster != "" && !allowCluster {
		return fmt.Errorf("%s (cluster %s): %w", name, m.Cluster, ErrPartOfCluster)
	}
	if err := b.Delete(ctx, name); err != nil {
		return err
	}
	if err := s.store.DeleteMachine(ctx, name); err != nil {
		return err
	}
	s.log.Info("machine deleted", "machine", name)
	return nil
}

func (s *Service) SerialLog(ctx context.Context, name string, maxBytes int64) ([]byte, error) {
	_, b, err := s.backendFor(ctx, name)
	if err != nil {
		return nil, err
	}
	r, ok := b.(backend.SerialLogReader)
	if !ok {
		return nil, backend.ErrNotSupported
	}
	return r.SerialLog(ctx, name, maxBytes)
}

// Reconcile は「望ましい状態が running なのに止まっている」マシンを起動する。
// ホストの再起動後など、niwakubed の起動時に呼ぶ。
func (s *Service) Reconcile(ctx context.Context) {
	ms, err := s.store.ListMachines(ctx)
	if err != nil {
		s.log.Error("reconcile: list machines", "err", err)
		return
	}
	for _, m := range ms {
		if m.DesiredState != DesiredRunning {
			continue
		}
		b, ok := s.backends[m.Backend]
		if !ok {
			continue
		}
		unlock := s.locks.Lock(m.Name)
		st, err := b.Status(ctx, m.Name)
		if err == nil && st.State == backend.StateStopped {
			s.log.Info("reconcile: starting machine", "machine", m.Name)
			if err := b.Start(ctx, m.Name); err != nil {
				s.log.Error("reconcile: start failed", "machine", m.Name, "err", err)
			}
		} else if err == nil && st.State == backend.StateNotFound {
			s.log.Warn("reconcile: machine is registered but its backend resources are missing", "machine", m.Name)
		}
		unlock()
	}
}

type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l, ok := k.m[key]
	if !ok {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ListCluster はクラスタに属するマシンを返す。
func (s *Service) ListCluster(ctx context.Context, cluster string) ([]*Machine, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []*Machine{}
	for _, m := range all {
		if m.Cluster == cluster {
			out = append(out, m)
		}
	}
	return out, nil
}

// ResolveBackend はバックエンド名 (空なら既定) を解決し、その能力を返す。
func (s *Service) ResolveBackend(name string) (string, backend.Capabilities, error) {
	if name == "" {
		name = s.cfg.DefaultBackend
	}
	b, ok := s.backends[name]
	if !ok {
		return "", backend.Capabilities{}, fmt.Errorf("%w: unknown backend %q", ErrInvalid, name)
	}
	return name, b.Capabilities(), nil
}
