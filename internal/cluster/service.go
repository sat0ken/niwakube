// Package cluster は Kubernetes クラスタの払い出しをまとめる。
//
// クラスタ N には 172.30.N.0/24 を割り当てる (N は 1〜254)。
//
//	.10       API の VIP (HA 用に予約。いまは未使用)
//	.11〜     コントロールプレーン
//	.21〜     ワーカー
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sat0ken/niwakube/internal/addons"
	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/kube"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/nodeexec"
	"github.com/sat0ken/niwakube/internal/pki"
	"github.com/sat0ken/niwakube/internal/store"
)

const (
	maxIndex    = 254
	maxWorkers  = 50
	cpHostBase  = 10  // CP i は .10+i
	wHostBase   = 20  // ワーカー i は .20+i
	lbHostStart = 100 // LoadBalancer の IP は .100〜.199
	lbHostEnd   = 199
)

type Config struct {
	Subnet   netip.Prefix // VM のサブネット。/16 であること (例: 172.30.0.0/16)
	Defaults Defaults     // リクエストで省略したときの CNI とランタイム
	// Forwarder はホストのポートからクラスタへの転送 (DNAT) を反映する。nil なら反映しない (テスト用)
	Forwarder ForwardApplier
	// NewApplier はクラスタにマニフェストを適用するクライアントを作る。nil なら kube.New
	NewApplier func(kubeconfig []byte) (Applier, error)
	// NFS はホストの NFS サーバー。Root が空なら NFS の StorageClass は作らない
	NFS NFSConfig
	// NodeOps はノードの大きさを変えるときの cordon / drain / Ready の確認。nil なら Kubernetes の API を使う
	NodeOps NodeOps
	// SnapshotDir はクラスタのスナップショットの置き場所。空ならスナップショットは使えない
	SnapshotDir string
}

// NFSConfig はホストの NFS サーバーの設定。クラスタごとに Root/<クラスタ名> を共有する。
type NFSConfig struct {
	Server netip.Addr // VM から見たホストのアドレス (ブリッジのアドレス)
	Root   string     // ホスト上で export しているディレクトリ (例: /var/lib/niwakube/nfs)
}

// ForwardApplier はホストのポートの転送をまとめて反映する。
type ForwardApplier interface {
	Apply(ctx context.Context, forwards []netx.PortForward) error
}

// Applier はクラスタにマニフェストを適用する (kube.Client)。
type Applier interface {
	Apply(ctx context.Context, manifest string) error
}

// Defaults はクラスタを作るときの既定値 (niwakubed の起動オプションで変えられる)。
type Defaults struct {
	CNI     string        `json:"cni"`    // 空なら cilium
	Hubble  bool          `json:"hubble"` // 参考情報 (CNI が cilium なら true)
	Runtime store.Runtime `json:"runtime"`
}

func (d Defaults) withFallback() Defaults {
	if d.CNI == "" {
		d.CNI = distro.CNICilium
	}
	d.Hubble = d.CNI == distro.CNICilium
	if d.Runtime.Default == "" {
		d.Runtime.Default = distro.RuntimeRunc
	}
	if d.Runtime.Default != distro.RuntimeRunc && !slices.Contains(d.Runtime.Install, d.Runtime.Default) {
		d.Runtime.Install = append(d.Runtime.Install, d.Runtime.Default)
	}
	if d.Runtime.Install == nil {
		d.Runtime.Install = []string{}
	}
	slices.Sort(d.Runtime.Install)
	d.Runtime.Install = slices.Compact(d.Runtime.Install)
	return d
}

type Service struct {
	cfg      Config
	store    *store.Store
	machines *machine.Service
	distros  map[string]distro.Distro
	defDist  string
	prober   Prober
	log      *slog.Logger

	mu        sync.Mutex // クラスタの作成・削除・起動・停止を直列化する
	adminMu   sync.Mutex
	adminCert map[string]pki.KeyPair // 状態確認用の admin 証明書のキャッシュ

	addonMu    sync.Mutex
	addonState map[string]string // クラスタごとのアドオンの適用状況 (pending / applied / エラー)

	opMu    sync.Mutex
	opState map[string]string // クラスタごとの進行中の操作 (ノードの大きさの変更など)。"failed: ..." は失敗
	nodeOps NodeOps

	drainTimeout     time.Duration
	nodeReadyTimeout time.Duration

	// 裏で動く処理 (bootstrap、アドオンの適用) の設定と、止めるための context
	postCreateTimeout  time.Duration
	postCreateInterval time.Duration
	bg                 context.Context
	stop               context.CancelFunc
}

func NewService(cfg Config, st *store.Store, ms *machine.Service, prober Prober, log *slog.Logger, distros ...distro.Distro) (*Service, error) {
	if cfg.Subnet.Bits() != 16 || !cfg.Subnet.Addr().Is4() {
		return nil, fmt.Errorf("cluster subnet must be an IPv4 /16: %s", cfg.Subnet)
	}
	cfg.Defaults = cfg.Defaults.withFallback()
	if err := addonsOf(cfg.Defaults.CNI, cfg.Defaults.Hubble, cfg.Defaults.Runtime).Validate(); err != nil {
		return nil, fmt.Errorf("invalid cluster defaults: %w", err)
	}
	if len(distros) == 0 {
		return nil, errors.New("at least one distro is required")
	}
	if prober == nil {
		prober = HTTPProber{}
	}
	if cfg.NodeOps == nil {
		cfg.NodeOps = kubeNodeOps{}
	}
	if log == nil {
		log = slog.Default()
	}
	m := map[string]distro.Distro{}
	for _, d := range distros {
		m[d.Name()] = d
	}
	if cfg.NewApplier == nil {
		cfg.NewApplier = func(kc []byte) (Applier, error) { return kube.New(kc) }
	}
	bg, stop := context.WithCancel(context.Background())
	return &Service{
		cfg: cfg, store: st, machines: ms, distros: m, defDist: distros[0].Name(),
		prober: prober, log: log, adminCert: map[string]pki.KeyPair{}, addonState: map[string]string{},
		opState: map[string]string{}, nodeOps: cfg.NodeOps, drainTimeout: 5 * time.Minute, nodeReadyTimeout: 10 * time.Minute,
		postCreateTimeout: 30 * time.Minute, postCreateInterval: 10 * time.Second, bg: bg, stop: stop,
	}, nil
}

// DistroInfo は利用できるディストリビューションの一覧に使う。
type DistroInfo struct {
	Name           string `json:"name"`
	Default        bool   `json:"default"`
	DefaultVersion string `json:"defaultVersion"`
	Image          string `json:"image"`
}

func (s *Service) Distros() []DistroInfo {
	out := []DistroInfo{}
	for name, d := range s.distros {
		out = append(out, DistroInfo{Name: name, Default: name == s.defDist, DefaultVersion: d.DefaultVersion(), Image: d.Image(d.DefaultVersion())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CreateRequest はクラスタ作成のリクエスト。ゼロ値の項目には既定値が入る。
type CreateRequest struct {
	Name              string         `json:"name"`
	Distro            string         `json:"distro,omitempty"`
	Version           string         `json:"version,omitempty"`
	Backend           string         `json:"backend,omitempty"`
	ControlPlanes     int            `json:"controlPlanes,omitempty"`
	Workers           *int           `json:"workers,omitempty"` // 0 台もあり得るのでポインタ
	ControlPlane      store.NodeSize `json:"controlPlane,omitempty"`
	Worker            store.NodeSize `json:"worker,omitempty"`
	SSHAuthorizedKeys []string       `json:"sshAuthorizedKeys,omitempty"`
	CNI               string         `json:"cni,omitempty"`    // cilium (既定) | flannel
	Hubble            *bool          `json:"hubble,omitempty"` // 既定は Cilium なら true
	Runtime           store.Runtime  `json:"runtime,omitempty"`
}

// Status はクラスタの現在の状態。
type Status struct {
	// Phase は provisioning | ready | degraded | stopped | stopping
	Phase      string `json:"phase"`
	ReadyNodes int    `json:"readyNodes"`
	Nodes      int    `json:"nodes"`
	Message    string `json:"message,omitempty"`
}

// Cluster は API で返すクラスタの情報。
type Cluster struct {
	Name          string         `json:"name"`
	Distro        string         `json:"distro"`
	Version       string         `json:"version"`
	Backend       string         `json:"backend"`
	ControlPlanes int            `json:"controlPlanes"`
	Workers       int            `json:"workers"`
	ControlPlane  store.NodeSize `json:"controlPlane"`
	Worker        store.NodeSize `json:"worker"`
	CNI           string         `json:"cni"`
	Hubble        bool           `json:"hubble"`
	Runtime       store.Runtime  `json:"runtime"`
	Subnet        string         `json:"subnet"`
	Endpoint      string         `json:"endpoint"`
	// LoadBalancerPool は Service type: LoadBalancer に割り当てる IP の範囲 (Cilium のときだけ)
	LoadBalancerPool string `json:"loadBalancerPool,omitempty"`
	// Addons は niwakubed がクラスタに入れるもの (LB の IP プールなど) の適用状況
	Addons string `json:"addons,omitempty"`
	// StorageClasses は使える StorageClass (既定のものが先頭)
	StorageClasses []string `json:"storageClasses"`
	// Operation は進行中の操作 (例: "resizing demo-worker-1 (1/2)")。"failed: ..." は失敗した操作
	Operation    string             `json:"operation,omitempty"`
	DesiredState string             `json:"desiredState"`
	Status       Status             `json:"status"`
	Machines     []*machine.Machine `json:"machines"`
	CreatedAt    time.Time          `json:"createdAt"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", machine.ErrInvalid, fmt.Sprintf(format, a...))
}

func defaultSize(sz *store.NodeSize) {
	if sz.CPUs == 0 {
		sz.CPUs = 2
	}
	if sz.MemoryMiB == 0 {
		sz.MemoryMiB = 4096
	}
	if sz.DiskGiB == 0 {
		sz.DiskGiB = 30
	}
}

func (s *Service) normalize(req *CreateRequest) (distro.Distro, error) {
	if !nameRe.MatchString(req.Name) {
		return nil, invalid("cluster name must match %s", nameRe)
	}
	if req.Distro == "" {
		req.Distro = s.defDist
	}
	d, ok := s.distros[req.Distro]
	if !ok {
		return nil, invalid("unknown distro %q", req.Distro)
	}
	if err := d.ValidateVersion(req.Version); err != nil {
		return nil, invalid("%v", err)
	}
	if req.Version == "" {
		req.Version = d.DefaultVersion()
	}
	name, caps, err := s.machines.ResolveBackend(req.Backend)
	if err != nil {
		return nil, err
	}
	req.Backend = name
	if d.Requirements().NeedsOwnKernel && !caps.OwnKernel {
		return nil, invalid("distro %s needs its own kernel, but backend %s shares the host kernel", d.Name(), name)
	}
	if req.ControlPlanes == 0 {
		req.ControlPlanes = 1
	}
	if req.ControlPlanes != 1 {
		return nil, invalid("only 1 control plane is supported for now")
	}
	if req.Workers == nil {
		two := 2
		req.Workers = &two
	}
	if *req.Workers < 0 || *req.Workers > maxWorkers {
		return nil, invalid("workers must be between 0 and %d", maxWorkers)
	}
	defaultSize(&req.ControlPlane)
	defaultSize(&req.Worker)

	def := s.cfg.Defaults
	if req.CNI == "" {
		req.CNI = def.CNI
		// コンテナ型のバックエンドはホストのカーネルを共有するので、既定の CNI を flannel にする
		if req.CNI == distro.CNICilium && !caps.OwnKernel {
			req.CNI = distro.CNIFlannel
		}
	}
	if req.Hubble == nil {
		h := req.CNI == distro.CNICilium
		req.Hubble = &h
	}
	// ランタイムを何も指定しなければ niwakubed の既定を使う。default だけ指定した場合は追加のランタイムは入れない
	if req.Runtime.Default == "" && req.Runtime.Install == nil {
		req.Runtime = store.Runtime{Default: def.Runtime.Default, Install: slices.Clone(def.Runtime.Install)}
	}
	if req.Runtime.Default == "" {
		req.Runtime.Default = def.Runtime.Default
	}
	// 既定にしたランタイムは自動で入れる
	if req.Runtime.Default != distro.RuntimeRunc && !slices.Contains(req.Runtime.Install, req.Runtime.Default) {
		req.Runtime.Install = append(req.Runtime.Install, req.Runtime.Default)
	}
	slices.Sort(req.Runtime.Install)
	req.Runtime.Install = slices.Compact(req.Runtime.Install)
	addons := addonsOf(req.CNI, *req.Hubble, req.Runtime)
	if err := addons.Validate(); err != nil {
		return nil, invalid("%v", err)
	}
	if only := d.Requirements().OnlyRuntimes; only != nil {
		for _, rt := range append([]string{req.Runtime.Default}, req.Runtime.Install...) {
			if !slices.Contains(only, rt) {
				return nil, invalid("distro %s supports only these runtimes: %s", d.Name(), strings.Join(only, ", "))
			}
		}
	}
	// Cilium の eBPF はホストのカーネルを共有するコンテナ型では他のクラスタとぶつかるので、VM 型に限る
	if req.CNI == distro.CNICilium && !caps.OwnKernel {
		return nil, invalid("cni cilium needs a backend with its own kernel; use cni: flannel with backend %s", name)
	}
	return d, nil
}

// hostIP はクラスタ index のサブネット内の host 番目のアドレスを返す。
func (s *Service) hostIP(index, host int) netip.Addr {
	b := s.cfg.Subnet.Addr().As4()
	return netip.AddrFrom4([4]byte{b[0], b[1], byte(index), byte(host)})
}

func (s *Service) subnetOf(index int) netip.Prefix {
	return netip.PrefixFrom(s.hostIP(index, 0), 24)
}

func cpName(cluster string, i int) string     { return fmt.Sprintf("%s-cp-%d", cluster, i) }
func workerName(cluster string, i int) string { return fmt.Sprintf("%s-worker-%d", cluster, i) }

// Create はクラスタを登録し、ノードを作って起動する。
// Kubernetes の構築はノードの中で非同期に進むので、完了は Get の Status.Phase で確認する。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Cluster, error) {
	d, err := s.normalize(&req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.store.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, c := range existing {
		if c.Name == req.Name {
			return nil, fmt.Errorf("cluster %q: %w", req.Name, store.ErrExists)
		}
		used[c.Index] = true
	}
	index := 0
	for i := 1; i <= maxIndex; i++ {
		if !used[i] {
			index = i
			break
		}
	}
	if index == 0 {
		return nil, fmt.Errorf("no free cluster subnet: %w", store.ErrExists)
	}

	secrets, err := d.GenerateSecrets(req.Name)
	if err != nil {
		return nil, err
	}
	if secrets.NodeSSHKey, err = nodeexec.NewKey("niwakube-" + req.Name); err != nil {
		return nil, err
	}
	rawSecrets, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	rec := &store.Cluster{
		Name: req.Name, Distro: d.Name(), Index: index,
		Spec: store.ClusterSpec{
			Version: req.Version, Backend: req.Backend, Image: d.Image(req.Version),
			ControlPlanes: req.ControlPlanes, Workers: *req.Workers,
			ControlPlane: req.ControlPlane, Worker: req.Worker, SSHKeys: req.SSHAuthorizedKeys,
			CNI: req.CNI, Hubble: *req.Hubble, Runtime: req.Runtime,
		},
		Secrets: rawSecrets, DesiredState: machine.DesiredRunning, CreatedAt: time.Now(),
	}
	if err := s.store.InsertCluster(ctx, rec); err != nil {
		return nil, err
	}
	s.log.Info("cluster registered", "cluster", rec.Name, "distro", rec.Distro, "subnet", s.subnetOf(index))

	if s.nfsEnabled() {
		if err := os.MkdirAll(s.nfsDir(rec.Name), 0o755); err != nil {
			s.rollback(context.WithoutCancel(ctx), rec.Name)
			return nil, fmt.Errorf("create nfs dir: %w", err)
		}
	}
	if err := s.createNodes(ctx, d, rec, secrets); err != nil {
		s.rollback(context.WithoutCancel(ctx), rec.Name)
		return nil, err
	}
	s.startPostCreate(rec)
	return s.view(ctx, rec)
}

func (s *Service) createNodes(ctx context.Context, d distro.Distro, rec *store.Cluster, secrets *distro.Secrets) error {
	_, caps, _ := s.machines.ResolveBackend(rec.Spec.Backend)
	dc := &distro.Cluster{
		Name: rec.Name, Version: rec.Spec.Version, Secrets: secrets,
		APIAddr:   s.hostIP(rec.Index, cpHostBase+1),
		Addons:    addonsOf(cniOf(rec), rec.Spec.Hubble, rec.Spec.Runtime),
		Container: caps.Isolation == backend.IsolationContainer,
	}
	for i := 1; i <= rec.Spec.ControlPlanes; i++ {
		if err := s.createNode(ctx, d, dc, rec, s.cpNode(rec, i), rec.Spec.ControlPlane); err != nil {
			return err
		}
	}
	for i := 1; i <= rec.Spec.Workers; i++ {
		if err := s.createNode(ctx, d, dc, rec, s.workerNode(rec, i), rec.Spec.Worker); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cpNode(rec *store.Cluster, i int) distro.Node {
	return distro.Node{Name: cpName(rec.Name, i), Role: distro.RoleControlPlane, Index: i, IP: s.hostIP(rec.Index, cpHostBase+i)}
}

func (s *Service) workerNode(rec *store.Cluster, i int) distro.Node {
	return distro.Node{Name: workerName(rec.Name, i), Role: distro.RoleWorker, Index: i, IP: s.hostIP(rec.Index, wHostBase+i)}
}

// createNode はノードのマシンを作って起動する (user-data はディストリが作る)。
func (s *Service) createNode(ctx context.Context, d distro.Distro, dc *distro.Cluster, rec *store.Cluster, n distro.Node, size store.NodeSize) error {
	keys := append([]string{}, rec.Spec.SSHKeys...)
	if dc.Secrets != nil && dc.Secrets.NodeSSHKey != "" {
		pub, err := nodeexec.AuthorizedKey(dc.Secrets.NodeSSHKey)
		if err != nil {
			return fmt.Errorf("node ssh key: %w", err)
		}
		keys = append(keys, pub)
	}
	req := machine.CreateRequest{
		Name: n.Name, Backend: rec.Spec.Backend, Image: rec.Spec.Image,
		CPUs: size.CPUs, MemoryMiB: size.MemoryMiB, DiskGiB: size.DiskGiB,
		SSHAuthorizedKeys: keys,
		IP:                n.IP, Cluster: rec.Name, Role: string(n.Role),
	}
	var err error
	if rp, ok := d.(distro.RawUserDataProvider); ok {
		req.RawUserData, err = rp.RawUserData(dc, &n)
	} else {
		req.UserData, err = d.UserData(dc, &n)
	}
	if err != nil {
		return err
	}
	if _, err := s.machines.Create(ctx, req); err != nil {
		return fmt.Errorf("create node %s: %w", n.Name, err)
	}
	return nil
}

func (s *Service) rollback(ctx context.Context, name string) {
	if err := s.deleteLocked(ctx, name); err != nil {
		s.log.Error("cluster rollback failed", "cluster", name, "err", err)
	}
}

func (s *Service) List(ctx context.Context) ([]*Cluster, error) {
	recs, err := s.store.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Cluster, 0, len(recs))
	for _, r := range recs {
		v, err := s.view(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, name string) (*Cluster, error) {
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, rec)
}

func (s *Service) endpoint(rec *store.Cluster) string {
	d := s.distros[rec.Distro]
	port := 6443
	if d != nil {
		port = d.APIPort()
	}
	return fmt.Sprintf("https://%s:%d", s.hostIP(rec.Index, cpHostBase+1), port)
}

func (s *Service) view(ctx context.Context, rec *store.Cluster) (*Cluster, error) {
	ms, err := s.machines.ListCluster(ctx, rec.Name)
	if err != nil {
		return nil, err
	}
	v := &Cluster{
		Name: rec.Name, Distro: rec.Distro, Version: rec.Spec.Version, Backend: rec.Spec.Backend,
		ControlPlanes: rec.Spec.ControlPlanes, Workers: rec.Spec.Workers,
		ControlPlane: rec.Spec.ControlPlane, Worker: rec.Spec.Worker,
		CNI: cniOf(rec), Hubble: rec.Spec.Hubble, Runtime: runtimeOf(rec),
		Subnet: s.subnetOf(rec.Index).String(), Endpoint: s.endpoint(rec), LoadBalancerPool: s.lbPoolString(rec),
		Addons: s.addonStatus(rec.Name), StorageClasses: s.storageClasses(rec), Operation: s.operation(rec.Name),
		DesiredState: rec.DesiredState, Machines: ms, CreatedAt: rec.CreatedAt,
	}
	v.Status = s.status(ctx, rec, ms)
	return v, nil
}

func (s *Service) status(ctx context.Context, rec *store.Cluster, ms []*machine.Machine) Status {
	expected := rec.Spec.ControlPlanes + rec.Spec.Workers
	st := Status{Nodes: expected}
	running := 0
	for _, m := range ms {
		if m.Status.State == backend.StateRunning {
			running++
		}
	}
	if rec.DesiredState == machine.DesiredStopped {
		if running == 0 {
			st.Phase = "stopped"
		} else {
			st.Phase, st.Message = "stopping", fmt.Sprintf("%d/%d machines still running", running, len(ms))
		}
		return st
	}
	if running < expected {
		st.Phase, st.Message = "degraded", fmt.Sprintf("%d/%d machines running", running, expected)
		return st
	}
	serverCA, admin, err := s.adminCredentials(rec)
	if err != nil {
		st.Phase, st.Message = "degraded", err.Error()
		return st
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ready, _, err := s.prober.Probe(pctx, s.endpoint(rec), serverCA, admin)
	st.ReadyNodes = ready
	switch {
	case err != nil:
		st.Phase, st.Message = "provisioning", "Kubernetes API is not reachable yet"
	case ready < expected:
		st.Phase, st.Message = "provisioning", fmt.Sprintf("%d/%d nodes ready", ready, expected)
	default:
		st.Phase = "ready"
	}
	return st
}

func (s *Service) secrets(rec *store.Cluster) (*distro.Secrets, distro.Distro, error) {
	d, ok := s.distros[rec.Distro]
	if !ok {
		return nil, nil, fmt.Errorf("distro %q is not available: %w", rec.Distro, backend.ErrNotSupported)
	}
	var sec distro.Secrets
	if err := json.Unmarshal(rec.Secrets, &sec); err != nil {
		return nil, nil, fmt.Errorf("parse secrets of %s: %w", rec.Name, err)
	}
	return &sec, d, nil
}

// adminCredentials は API サーバーの CA と、admin のクライアント証明書を返す。
func (s *Service) adminCredentials(rec *store.Cluster) (string, pki.KeyPair, error) {
	sec, d, err := s.secrets(rec)
	if err != nil {
		return "", pki.KeyPair{}, err
	}
	serverName, clientName := d.AdminCAs()
	serverCA, ok1 := sec.CAs[serverName]
	clientCA, ok2 := sec.CAs[clientName]
	if !ok1 || !ok2 {
		return "", pki.KeyPair{}, fmt.Errorf("cluster %s: admin CAs are missing", rec.Name)
	}
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	key := rec.Name + "/" + rec.CreatedAt.String()
	if kp, ok := s.adminCert[key]; ok {
		return serverCA.CertPEM, kp, nil
	}
	kp, err := clientCA.SignClient("niwakube-admin", []string{"system:masters"}, 365*24*time.Hour)
	if err != nil {
		return "", pki.KeyPair{}, err
	}
	s.adminCert[key] = kp
	return serverCA.CertPEM, kp, nil
}

// KubeconfigOptions は kubeconfig の発行のオプション。
type KubeconfigOptions struct {
	// ProxyURL を入れると、クラスタの proxy-url にする (例: socks5://localhost:1080)。
	// クラスタはホストの中の NAT にあるので、SSH の SOCKS プロキシ (ssh -D 1080) 経由で
	// 別の PC から kubectl を使うときに指定する。server と証明書はそのまま使える。
	ProxyURL string
}

// ValidateProxyURL は kubeconfig の proxy-url に使える URL か確かめる (socks5 / http / https とホスト:ポート)。
func ValidateProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("proxy: %v", err)
	}
	switch u.Scheme {
	case "socks5", "http", "https":
	default:
		return invalid("proxy: scheme must be socks5, http or https: %q", raw)
	}
	if u.Hostname() == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return invalid("proxy: want scheme://host:port, got %q", raw)
	}
	return nil
}

// Kubeconfig は admin の kubeconfig を発行する。
func (s *Service) Kubeconfig(ctx context.Context, name string, opts ...KubeconfigOptions) ([]byte, error) {
	return s.KubeconfigFor(ctx, []string{name}, opts...)
}

// KubeconfigFor は複数のクラスタの admin の kubeconfig を 1 つにまとめて発行する
// (クラスタごとに cluster / user / context が入り、current-context は最初のクラスタ)。
func (s *Service) KubeconfigFor(ctx context.Context, names []string, opts ...KubeconfigOptions) ([]byte, error) {
	var o KubeconfigOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.ProxyURL != "" {
		if err := ValidateProxyURL(o.ProxyURL); err != nil {
			return nil, err
		}
	}
	if len(names) == 0 {
		return nil, invalid("no cluster selected")
	}
	seen := map[string]bool{}
	var entries []kubeconfigEntry
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		e, err := s.kubeconfigEntry(ctx, name, o.ProxyURL)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return marshalKubeconfig(entries)
}

func (s *Service) kubeconfigEntry(ctx context.Context, name, proxyURL string) (kubeconfigEntry, error) {
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return kubeconfigEntry{}, err
	}
	sec, d, err := s.secrets(rec)
	if err != nil {
		return kubeconfigEntry{}, err
	}
	serverName, clientName := d.AdminCAs()
	clientCA := sec.CAs[clientName]
	admin, err := clientCA.SignClient("niwakube-admin", []string{"system:masters"}, 365*24*time.Hour)
	if err != nil {
		return kubeconfigEntry{}, err
	}
	return newKubeconfigEntry(rec.Name, s.endpoint(rec), sec.CAs[serverName].CertPEM, proxyURL, admin), nil
}

// Delete はクラスタのノードをすべて削除し、登録を消す。
func (s *Service) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.store.GetCluster(ctx, name); err != nil {
		return err
	}
	if err := s.busy(name); err != nil {
		return err
	}
	return s.deleteLocked(ctx, name)
}

func (s *Service) deleteLocked(ctx context.Context, name string) error {
	ms, err := s.machines.ListCluster(ctx, name)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range ms {
		if err := s.machines.DeleteClusterMachine(ctx, m.Name); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", m.Name, err))
		}
	}
	if len(errs) > 0 {
		// ノードが残っているうちは登録を消さない (やり直せるように)
		return errors.Join(errs...)
	}
	if s.cfg.SnapshotDir != "" {
		// スナップショットもクラスタと一緒に消す
		if err := os.RemoveAll(s.snapshotRoot(name)); err != nil {
			s.log.Error("remove snapshots", "cluster", name, "err", err)
		}
	}
	if s.nfsEnabled() {
		// NFS のデータもクラスタと一緒に消す (同じ名前で作り直したときに古いデータが見えないように)
		if err := os.RemoveAll(s.nfsDir(name)); err != nil {
			s.log.Error("remove nfs dir", "cluster", name, "err", err)
		}
	}
	if err := s.store.DeleteClusterForwards(ctx, name); err != nil {
		return err
	}
	if err := s.applyForwards(ctx); err != nil {
		s.log.Error("apply forwards", "err", err)
	}
	if err := s.store.DeleteCluster(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.setAddonState(name, "")
	s.setOperation(name, "")
	s.log.Info("cluster deleted", "cluster", name)
	return nil
}

// Start はクラスタのノードをすべて起動する。
func (s *Service) Start(ctx context.Context, name string) (*Cluster, error) {
	if err := s.busy(name); err != nil {
		return nil, err
	}
	return s.setState(ctx, name, machine.DesiredRunning)
}

// Stop はクラスタのノードをすべて正常停止する (ワーカーから先に止める)。
func (s *Service) Stop(ctx context.Context, name string) (*Cluster, error) {
	if err := s.busy(name); err != nil {
		return nil, err
	}
	return s.setState(ctx, name, machine.DesiredStopped)
}

func (s *Service) setState(ctx context.Context, name, state string) (*Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetClusterDesiredState(ctx, name, state); err != nil {
		return nil, err
	}
	rec.DesiredState = state
	ms, err := s.machines.ListCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	// 起動は CP から、停止はワーカーから
	rank := func(m *machine.Machine) int {
		isCP := m.Role == string(distro.RoleControlPlane)
		if isCP == (state == machine.DesiredRunning) {
			return 0
		}
		return 1
	}
	sort.SliceStable(ms, func(i, j int) bool {
		if rank(ms[i]) != rank(ms[j]) {
			return rank(ms[i]) < rank(ms[j])
		}
		return ms[i].Name < ms[j].Name
	})
	var errs []error
	for _, m := range ms {
		var err error
		if state == machine.DesiredRunning {
			_, err = s.machines.Start(ctx, m.Name)
		} else {
			_, err = s.machines.Stop(ctx, m.Name, false)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return s.view(ctx, rec)
}

func addonsOf(cni string, hubble bool, rt store.Runtime) distro.Addons {
	def := rt.Default
	if def == "" {
		def = distro.RuntimeRunc
	}
	return distro.Addons{CNI: cni, Hubble: hubble, DefaultRuntime: def, Runtimes: rt.Install}
}

// cniOf は CNI を返す。CNI を選べるようになる前に作ったクラスタは flannel。
func cniOf(rec *store.Cluster) string {
	if rec.Spec.CNI == "" {
		return distro.CNIFlannel
	}
	return rec.Spec.CNI
}

func runtimeOf(rec *store.Cluster) store.Runtime {
	rt := rec.Spec.Runtime
	if rt.Default == "" {
		rt.Default = distro.RuntimeRunc
	}
	return rt
}

// Defaults は、リクエストで省略したときに使う CNI とランタイムを返す。
func (s *Service) Defaults() Defaults { return s.cfg.Defaults }

// distroCluster は保存されているクラスタから Distro に渡す情報を作る。
func (s *Service) distroCluster(rec *store.Cluster) (*distro.Cluster, distro.Distro, error) {
	sec, d, err := s.secrets(rec)
	if err != nil {
		return nil, nil, err
	}
	_, caps, _ := s.machines.ResolveBackend(rec.Spec.Backend)
	return &distro.Cluster{
		Name: rec.Name, Version: rec.Spec.Version, Secrets: sec,
		APIAddr:   s.hostIP(rec.Index, cpHostBase+1),
		Addons:    addonsOf(cniOf(rec), rec.Spec.Hubble, rec.Spec.Runtime),
		Container: caps.Isolation == backend.IsolationContainer,
	}, d, nil
}

// Close は裏で動いている処理を止める。
func (s *Service) Close() { s.stop() }

// startPostCreate は、クラスタを作ったあと (と niwakubed の起動時) に裏で次の処理を行う。
//  1. Distro が Bootstrapper なら、成功するまで初期化を試す (Talos の etcd の bootstrap)
//  2. クラスタの準備ができたら、アドオン (LoadBalancer の IP プールなど) を server-side apply する
//
// どちらも何度実行しても安全。
func (s *Service) startPostCreate(rec *store.Cluster) {
	dc, d, err := s.distroCluster(rec)
	if err != nil {
		return
	}
	manifests := s.addonManifests(rec)
	if len(manifests) > 0 {
		s.setAddonState(rec.Name, "pending")
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.bg, s.postCreateTimeout)
		defer cancel()
		retry := func(what string, fn func(context.Context) error) bool {
			for {
				actx, acancel := context.WithTimeout(ctx, 60*time.Second)
				err := fn(actx)
				acancel()
				if err == nil {
					return true
				}
				s.log.Debug(what+" not ready yet", "cluster", rec.Name, "err", err)
				select {
				case <-ctx.Done():
					s.log.Error(what+" gave up", "cluster", rec.Name, "err", err)
					if what == "addons" {
						s.setAddonState(rec.Name, "failed: "+err.Error())
					}
					return false
				case <-time.After(s.postCreateInterval):
				}
			}
		}
		if b, ok := d.(distro.Bootstrapper); ok {
			if !retry("bootstrap", func(c context.Context) error { return b.Bootstrap(c, dc) }) {
				return
			}
			s.log.Info("cluster bootstrapped", "cluster", rec.Name)
		}
		if len(manifests) == 0 {
			return
		}
		if retry("addons", func(c context.Context) error { return s.applyAddons(c, rec, manifests) }) {
			s.setAddonState(rec.Name, "applied")
			s.log.Info("cluster addons applied", "cluster", rec.Name)
		}
	}()
}

// addonManifests はクラスタに入れるアドオン。
func (s *Service) addonManifests(rec *store.Cluster) []addons.Manifest {
	var out []addons.Manifest
	if cniOf(rec) == distro.CNICilium {
		start, stop := s.lbPool(rec)
		out = append(out, addons.LoadBalancer(start, stop)...)
	}
	ls := s.localStorage(rec)
	opts := addons.StorageOptions{LocalPath: !ls.Builtin, LocalPathDir: ls.Dir, PrivilegedNamespace: ls.PrivilegedNamespace}
	if s.nfsEnabled() {
		opts.NFSServer, opts.NFSShare = s.cfg.NFS.Server.String(), s.nfsDir(rec.Name)
	}
	st, err := addons.Storage(opts)
	if err != nil {
		s.log.Error("storage addons", "cluster", rec.Name, "err", err)
		return out
	}
	return append(out, st...)
}

func (s *Service) localStorage(rec *store.Cluster) distro.LocalStorage {
	ls := distro.LocalStorage{Dir: "/opt/local-path-provisioner"}
	if p, ok := s.distros[rec.Distro].(distro.LocalStorageProvider); ok {
		ls = p.LocalStorage()
	}
	return ls
}

func (s *Service) nfsEnabled() bool { return s.cfg.NFS.Root != "" && s.cfg.NFS.Server.IsValid() }

// nfsDir はクラスタの NFS のディレクトリ (ホスト上のパス。NFS の share も同じパス)。
func (s *Service) nfsDir(cluster string) string { return filepath.Join(s.cfg.NFS.Root, cluster) }

func (s *Service) storageClasses(*store.Cluster) []string {
	out := []string{"local-path"}
	if s.nfsEnabled() {
		out = append(out, "niwakube-nfs")
	}
	return out
}

// applyAddons はクラスタの準備ができていればアドオンを適用する。
func (s *Service) applyAddons(ctx context.Context, rec *store.Cluster, manifests []addons.Manifest) error {
	ms, err := s.machines.ListCluster(ctx, rec.Name)
	if err != nil {
		return err
	}
	if st := s.status(ctx, rec, ms); st.Phase != "ready" {
		return fmt.Errorf("cluster is %s", st.Phase)
	}
	kc, err := s.Kubeconfig(ctx, rec.Name)
	if err != nil {
		return err
	}
	a, err := s.cfg.NewApplier(kc)
	if err != nil {
		return err
	}
	for _, m := range manifests {
		var lastErr error
		for _, v := range m.Variants {
			if lastErr = a.Apply(ctx, v); lastErr == nil || !errors.Is(lastErr, kube.ErrNotReady) {
				break
			}
		}
		if lastErr != nil {
			return fmt.Errorf("%s: %w", m.Name, lastErr)
		}
	}
	return nil
}

func (s *Service) setAddonState(name, state string) {
	s.addonMu.Lock()
	defer s.addonMu.Unlock()
	if state == "" {
		delete(s.addonState, name)
	} else {
		s.addonState[name] = state
	}
}

func (s *Service) addonStatus(name string) string {
	s.addonMu.Lock()
	defer s.addonMu.Unlock()
	return s.addonState[name]
}

// lbPool は Service type: LoadBalancer に割り当てる範囲 (172.30.N.100〜199)。
func (s *Service) lbPool(rec *store.Cluster) (netip.Addr, netip.Addr) {
	return s.hostIP(rec.Index, lbHostStart), s.hostIP(rec.Index, lbHostEnd)
}

func (s *Service) lbPoolString(rec *store.Cluster) string {
	if cniOf(rec) != distro.CNICilium {
		return ""
	}
	start, stop := s.lbPool(rec)
	return start.String() + "-" + stop.String()
}

// Reconcile は niwakubed の起動時に呼ぶ。初期化が必要なディストリのクラスタは、念のため初期化を試し直す
// (初期化の途中で niwakubed が止まった場合に備える。初期化済みなら何もしない)。
func (s *Service) Reconcile(ctx context.Context) {
	recs, err := s.store.ListClusters(ctx)
	if err != nil {
		s.log.Error("cluster reconcile", "err", err)
		return
	}
	for _, rec := range recs {
		if rec.DesiredState == machine.DesiredRunning {
			s.startPostCreate(rec)
		}
	}
	if err := s.applyForwards(ctx); err != nil {
		s.log.Error("apply forwards", "err", err)
	}
}

// ClientConfig は Kubernetes 以外のクライアントの設定 (Talos の talosconfig) を返す。
func (s *Service) ClientConfig(ctx context.Context, name string, opts ...KubeconfigOptions) (string, []byte, error) {
	var o KubeconfigOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.ProxyURL != "" {
		if err := ValidateProxyURL(o.ProxyURL); err != nil {
			return "", nil, err
		}
	}
	rec, err := s.store.GetCluster(ctx, name)
	if err != nil {
		return "", nil, err
	}
	dc, d, err := s.distroCluster(rec)
	if err != nil {
		return "", nil, err
	}
	p, ok := d.(distro.ClientConfigProvider)
	if !ok {
		return "", nil, fmt.Errorf("distro %s has no client config: %w", d.Name(), backend.ErrNotSupported)
	}
	return p.ClientConfig(dc, o.ProxyURL)
}
