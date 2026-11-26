package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/backend/fake"
	"github.com/sat0ken/niwakube/internal/distro/k3s"
	"github.com/sat0ken/niwakube/internal/distro/kubeadm"
	"github.com/sat0ken/niwakube/internal/distro/talos"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/kube"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/pki"
	"github.com/sat0ken/niwakube/internal/store"
)

type fakeProber struct {
	mu    sync.Mutex
	ready int
	err   error
	calls []string
}

func (p *fakeProber) Probe(_ context.Context, endpoint, _ string, _ pki.KeyPair) (int, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, endpoint)
	return p.ready, p.ready, p.err
}

func newTest(t *testing.T) (*Service, *machine.Service, *fake.Backend, *fakeProber) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fb := fake.New()
	ms, err := machine.NewService(machine.Config{
		Bridge:  "nkbr0",
		Subnet:  netip.MustParsePrefix("172.30.0.0/16"),
		Gateway: netip.MustParseAddr("172.30.0.1"),
		Pool:    ipam.Pool{Start: netip.MustParseAddr("172.30.0.10"), End: netip.MustParseAddr("172.30.0.254")},
	}, st, nil, fb)
	if err != nil {
		t.Fatal(err)
	}
	pr := &fakeProber{err: errors.New("connection refused")}
	cs, err := NewService(Config{
		Subnet:     netip.MustParsePrefix("172.30.0.0/16"),
		Forwarder:  &fakeForwarder{},
		NewApplier: func([]byte) (Applier, error) { return &fakeApplier{}, nil },
	}, st, ms, pr, nil, k3s.New(), kubeadm.New(), talos.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Close)
	return cs, ms, fb, pr
}

func TestCreateCluster(t *testing.T) {
	ctx := context.Background()
	cs, ms, fb, pr := newTest(t)

	two := 2
	c, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: &two, Worker: store.NodeSize{MemoryMiB: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subnet != "172.30.1.0/24" || c.Endpoint != "https://172.30.1.11:6443" || c.Distro != "k3s" || c.Version != "stable" {
		t.Errorf("unexpected cluster: %+v", c)
	}
	if c.Worker.MemoryMiB != 2048 || c.Worker.CPUs != 2 || c.ControlPlane.MemoryMiB != 4096 {
		t.Errorf("sizes not defaulted: %+v %+v", c.ControlPlane, c.Worker)
	}
	want := map[string]string{"demo-cp-1": "172.30.1.11", "demo-worker-1": "172.30.1.21", "demo-worker-2": "172.30.1.22"}
	if len(c.Machines) != 3 {
		t.Fatalf("machines = %d", len(c.Machines))
	}
	for _, m := range c.Machines {
		if want[m.Name] != m.IP || m.Cluster != "demo" {
			t.Errorf("machine %s: ip %s cluster %q", m.Name, m.IP, m.Cluster)
		}
	}
	if c.Status.Phase != "provisioning" {
		t.Errorf("phase = %s", c.Status.Phase)
	}
	// 裏でアドオンの適用のために Probe が呼ばれるので、ロックを取って読む
	pr.mu.Lock()
	calls := append([]string(nil), pr.calls...)
	pr.mu.Unlock()
	if len(calls) == 0 || calls[0] != "https://172.30.1.11:6443" {
		t.Errorf("probe calls = %v", calls)
	}

	// CP の user-data には CA が入り、ワーカーには入らない
	cp, _ := fb.Spec("demo-cp-1")
	w, _ := fb.Spec("demo-worker-1")
	if !strings.Contains(string(cp.Seed.UserData), "server-ca.key") || strings.Contains(string(w.Seed.UserData), "server-ca.key") {
		t.Error("CA files must be written only to the control plane")
	}
	if !strings.Contains(string(w.Seed.UserData), "https://172.30.1.11:6443") {
		t.Error("worker must point at the control plane")
	}

	// 準備完了
	pr.mu.Lock()
	pr.ready, pr.err = 3, nil
	pr.mu.Unlock()
	c, err = cs.Get(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status.Phase != "ready" || c.Status.ReadyNodes != 3 {
		t.Errorf("status = %+v", c.Status)
	}

	// クラスタのマシンは単体では消せない
	if err := ms.Delete(ctx, "demo-worker-1"); !errors.Is(err, machine.ErrPartOfCluster) {
		t.Errorf("want ErrPartOfCluster, got %v", err)
	}

	// 2 つ目のクラスタは次のサブネット
	zero := 0
	c2, err := cs.Create(ctx, CreateRequest{Name: "second", Workers: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if c2.Subnet != "172.30.2.0/24" || len(c2.Machines) != 1 {
		t.Errorf("second cluster: %+v", c2)
	}
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo"}); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate: want ErrExists, got %v", err)
	}

	// 停止 → 起動
	c, err = cs.Stop(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status.Phase != "stopped" {
		t.Errorf("after stop: %+v", c.Status)
	}
	c, err = cs.Start(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status.Phase != "ready" {
		t.Errorf("after start: %+v", c.Status)
	}

	// 削除するとサブネットが空き、マシンも消える
	if err := cs.Delete(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if left, _ := ms.ListCluster(ctx, "demo"); len(left) != 0 {
		t.Errorf("machines left: %d", len(left))
	}
	if s, _ := fb.Status(ctx, "demo-cp-1"); s.State != backend.StateNotFound {
		t.Errorf("backend machine left: %+v", s)
	}
	c3, err := cs.Create(ctx, CreateRequest{Name: "third", Workers: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if c3.Subnet != "172.30.1.0/24" {
		t.Errorf("subnet not reused: %s", c3.Subnet)
	}
}

func TestCreateValidationAndRollback(t *testing.T) {
	ctx := context.Background()
	cs, ms, fb, _ := newTest(t)
	neg := -1
	for _, req := range []CreateRequest{
		{Name: "Bad"},
		{Name: "this-name-is-way-too-long-for-a-cluster"},
		{Name: "ok", Distro: "nope"},
		{Name: "ok", Version: "1.2.3"},
		{Name: "ok", ControlPlanes: 3},
		{Name: "ok", Workers: &neg},
		{Name: "ok", Backend: "nope"},
	} {
		if _, err := cs.Create(ctx, req); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", req, err)
		}
	}

	// 途中でノードの作成に失敗したら、作ったものをすべて消す
	fb.StartErr = errors.New("boom")
	if _, err := cs.Create(ctx, CreateRequest{Name: "broken"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := cs.Get(ctx, "broken"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cluster record should be rolled back: %v", err)
	}
	if left, _ := ms.ListCluster(ctx, "broken"); len(left) != 0 {
		t.Errorf("machines left after rollback: %d", len(left))
	}
}

func TestKubeconfig(t *testing.T) {
	ctx := context.Background()
	cs, _, _, _ := newTest(t)
	zero := 0
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: &zero}); err != nil {
		t.Fatal(err)
	}
	raw, err := cs.Kubeconfig(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var kc map[string]any
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		t.Fatal(err)
	}
	if kc["current-context"] != "demo" || kc["kind"] != "Config" {
		t.Errorf("kubeconfig: %s", raw)
	}
	if !strings.Contains(string(raw), "server: https://172.30.1.11:6443") || !strings.Contains(string(raw), "client-key-data:") {
		t.Errorf("kubeconfig: %s", raw)
	}
	if _, err := cs.Kubeconfig(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if strings.Contains(string(raw), "proxy-url") {
		t.Errorf("proxy-url must not be set by default: %s", raw)
	}

	// SSH の SOCKS プロキシ経由で使う kubeconfig: server はそのまま、proxy-url を足す
	raw, err = cs.Kubeconfig(ctx, "demo", KubeconfigOptions{ProxyURL: "socks5://localhost:1080"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "server: https://172.30.1.11:6443") || !strings.Contains(string(raw), "proxy-url: socks5://localhost:1080") {
		t.Errorf("kubeconfig with proxy: %s", raw)
	}
	for _, bad := range []string{"localhost:1080", "ftp://localhost:1080", "socks5://localhost", "socks5://u:p@localhost:1080", "socks5://localhost:1080/x", "socks5://:1080"} {
		if _, err := cs.Kubeconfig(ctx, "demo", KubeconfigOptions{ProxyURL: bad}); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", bad, err)
		}
	}
}

func TestKubeadmCluster(t *testing.T) {
	ctx := context.Background()
	cs, _, fb, _ := newTest(t)
	one := 1
	c, err := cs.Create(ctx, CreateRequest{Name: "kadm", Distro: "kubeadm", Version: "v1.37", Workers: &one})
	if err != nil {
		t.Fatal(err)
	}
	if c.Distro != "kubeadm" || c.Version != "v1.37" || len(c.Machines) != 2 {
		t.Fatalf("unexpected cluster: %+v", c)
	}
	cp, _ := fb.Spec("kadm-cp-1")
	if !strings.Contains(string(cp.Seed.UserData), "/etc/kubernetes/pki/ca.key") {
		t.Error("CP must receive the kubeadm CA")
	}
	raw, err := cs.Kubeconfig(ctx, "kadm")
	if err != nil {
		t.Fatal(err)
	}
	// kubeconfig の CA は kubeadm の "ca" (クライアント証明書も同じ CA で署名される)
	rec, _ := cs.store.GetCluster(ctx, "kadm")
	sec, _, _ := cs.secrets(rec)
	var kc struct {
		Clusters []struct {
			Cluster struct {
				CA string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		t.Fatal(err)
	}
	if kc.Clusters[0].Cluster.CA != b64(sec.CAs["ca"].CertPEM) {
		t.Error("kubeconfig must trust the kubeadm cluster CA")
	}
	if _, err := cs.Create(ctx, CreateRequest{Name: "bad", Distro: "kubeadm", Version: "v1.37.2+k3s1"}); !errors.Is(err, machine.ErrInvalid) {
		t.Errorf("k3s style version must be rejected for kubeadm: %v", err)
	}
}

func TestCNIAndRuntimeDefaults(t *testing.T) {
	ctx := context.Background()
	cs, _, fb, _ := newTest(t)
	zero := 0

	// 既定: Cilium + Hubble、ランタイムは runc
	c, err := cs.Create(ctx, CreateRequest{Name: "def", Workers: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if c.CNI != "cilium" || !c.Hubble || c.Runtime.Default != "runc" || len(c.Runtime.Install) != 0 {
		t.Errorf("defaults: cni=%s hubble=%v runtime=%+v", c.CNI, c.Hubble, c.Runtime)
	}
	cp, _ := fb.Spec("def-cp-1")
	if !strings.Contains(string(cp.Seed.UserData), "flannel-backend: none") || !strings.Contains(string(cp.Seed.UserData), "hubble.ui.enabled=true") {
		t.Error("cilium + hubble must reach the node's user-data")
	}

	// 既定のランタイムは自動で入れる対象に加わる
	f := false
	c, err = cs.Create(ctx, CreateRequest{Name: "rt", Workers: &zero, CNI: "flannel", Hubble: &f, Runtime: store.Runtime{Default: "youki", Install: []string{"crun"}}})
	if err != nil {
		t.Fatal(err)
	}
	if c.CNI != "flannel" || c.Hubble || c.Runtime.Default != "youki" || strings.Join(c.Runtime.Install, ",") != "crun,youki" {
		t.Errorf("runtime: cni=%s hubble=%v runtime=%+v", c.CNI, c.Hubble, c.Runtime)
	}

	tr := true
	for _, req := range []CreateRequest{
		{Name: "x", CNI: "calico"},
		{Name: "x", CNI: "flannel", Hubble: &tr},
		{Name: "x", Runtime: store.Runtime{Install: []string{"kata"}}},
	} {
		if _, err := cs.Create(ctx, req); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", req, err)
		}
	}
}

// CNI を選べるようになる前に作ったクラスタ (spec に cni が無い) は flannel として扱う。
func TestLegacyClusterIsFlannel(t *testing.T) {
	rec := &store.Cluster{Spec: store.ClusterSpec{}}
	if cniOf(rec) != "flannel" || runtimeOf(rec).Default != "runc" {
		t.Errorf("legacy: %s %+v", cniOf(rec), runtimeOf(rec))
	}
}

func TestNiwakubeDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ms, err := machine.NewService(machine.Config{
		Bridge: "nkbr0", Subnet: netip.MustParsePrefix("172.30.0.0/16"), Gateway: netip.MustParseAddr("172.30.0.1"),
		Pool: ipam.Pool{Start: netip.MustParseAddr("172.30.0.10"), End: netip.MustParseAddr("172.30.0.254")},
	}, st, nil, fake.New())
	if err != nil {
		t.Fatal(err)
	}
	newCS := func(d Defaults) (*Service, error) {
		return NewService(Config{Subnet: netip.MustParsePrefix("172.30.0.0/16"), Defaults: d}, st, ms, &fakeProber{err: errors.New("x")}, nil, k3s.New())
	}

	// niwakubed の既定を「flannel + crun (youki も入れる)」にする
	cs, err := newCS(Defaults{CNI: "flannel", Runtime: store.Runtime{Default: "crun", Install: []string{"youki"}}})
	if err != nil {
		t.Fatal(err)
	}
	if d := cs.Defaults(); d.CNI != "flannel" || d.Hubble || d.Runtime.Default != "crun" || strings.Join(d.Runtime.Install, ",") != "crun,youki" {
		t.Errorf("defaults: %+v", d)
	}
	zero := 0
	c, err := cs.Create(ctx, CreateRequest{Name: "a", Workers: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if c.CNI != "flannel" || c.Runtime.Default != "crun" || strings.Join(c.Runtime.Install, ",") != "crun,youki" {
		t.Errorf("cluster with niwakube defaults: cni=%s runtime=%+v", c.CNI, c.Runtime)
	}
	// リクエストで指定すればそちらが優先 (default だけ指定したら追加のランタイムは入れない)
	c, err = cs.Create(ctx, CreateRequest{Name: "b", Workers: &zero, CNI: "cilium", Runtime: store.Runtime{Default: "runc"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.CNI != "cilium" || !c.Hubble || c.Runtime.Default != "runc" || len(c.Runtime.Install) != 0 {
		t.Errorf("request overrides: cni=%s hubble=%v runtime=%+v", c.CNI, c.Hubble, c.Runtime)
	}

	// 不正な既定値では起動しない
	if _, err := newCS(Defaults{CNI: "calico"}); err == nil {
		t.Error("invalid default CNI must be rejected")
	}
	if _, err := newCS(Defaults{Runtime: store.Runtime{Default: "kata"}}); err == nil {
		t.Error("invalid default runtime must be rejected")
	}
}

func TestTalosCluster(t *testing.T) {
	// テストでは Talos API への bootstrap を試さない
	ctx := context.Background()
	cs, _, fb, _ := newTest(t)
	cs.postCreateTimeout = 0
	one := 1
	c, err := cs.Create(ctx, CreateRequest{Name: "tal", Distro: "talos", Workers: &one})
	if err != nil {
		t.Fatal(err)
	}
	if c.Distro != "talos" || c.CNI != "cilium" || len(c.Machines) != 2 {
		t.Fatalf("unexpected cluster: %+v", c)
	}
	cp, _ := fb.Spec("tal-cp-1")
	if cp.Image != "talos-"+talos.New().DefaultVersion() {
		t.Errorf("image: %s", cp.Image)
	}
	ud := string(cp.Seed.UserData)
	if strings.HasPrefix(ud, "#cloud-config") || !strings.Contains(ud, "type: controlplane") || !strings.Contains(ud, "niwakube-cilium-install") {
		t.Errorf("CP must receive the Talos machine config as is:\n%.300s", ud)
	}
	// network-config (固定 IP) は Talos でも NoCloud で渡る
	if !strings.Contains(string(cp.Seed.NetworkConfig), "172.30.1.11/16") {
		t.Error("network-config must still carry the static IP")
	}

	name, tc, err := cs.ClientConfig(ctx, "tal")
	if err != nil || name != "tal.talosconfig" || !strings.Contains(string(tc), "172.30.1.11") {
		t.Errorf("talosconfig: %s %v", name, err)
	}
	name, tc, err = cs.ClientConfig(ctx, "tal", KubeconfigOptions{ProxyURL: "socks5://localhost:1080"})
	if err != nil || name != "tal-ssh.talosconfig" || !strings.Contains(string(tc), "proxy-url: socks5://localhost:1080") {
		t.Errorf("talosconfig via SSH: %s %v\n%s", name, err, tc)
	}
	if _, _, err := cs.ClientConfig(ctx, "tal", KubeconfigOptions{ProxyURL: "ftp://x:1"}); !errors.Is(err, machine.ErrInvalid) {
		t.Errorf("bad proxy: want ErrInvalid, got %v", err)
	}
	if _, err := cs.Kubeconfig(ctx, "tal"); err != nil {
		t.Errorf("kubeconfig: %v", err)
	}

	// Talos は runc のみ
	if _, err := cs.Create(ctx, CreateRequest{Name: "tal2", Distro: "talos", Runtime: store.Runtime{Default: "crun"}}); !errors.Is(err, machine.ErrInvalid) {
		t.Errorf("crun must be rejected for talos: %v", err)
	}
	// k3s には talosconfig は無い
	zero := 0
	if _, err := cs.Create(ctx, CreateRequest{Name: "k", Workers: &zero}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.ClientConfig(ctx, "k"); !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("k3s talosconfig: want ErrNotSupported, got %v", err)
	}
}

type fakeForwarder struct {
	mu   sync.Mutex
	last []netx.PortForward
	n    int
}

func (f *fakeForwarder) Apply(_ context.Context, fs []netx.PortForward) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last, f.n = fs, f.n+1
	return nil
}

type fakeApplier struct {
	mu       sync.Mutex
	applied  []string
	notReady map[string]bool // この文字列を含むマニフェストは ErrNotReady にする
}

func (a *fakeApplier) Apply(_ context.Context, m string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.notReady {
		if strings.Contains(m, k) {
			return fmt.Errorf("%s: %w", k, kube.ErrNotReady)
		}
	}
	a.applied = append(a.applied, m)
	return nil
}

func TestForwards(t *testing.T) {
	ctx := context.Background()
	cs, _, _, _ := newTest(t)
	fw := cs.cfg.Forwarder.(*fakeForwarder)
	zero := 0
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: &zero}); err != nil {
		t.Fatal(err)
	}
	f, err := cs.AddForward(ctx, "demo", ForwardRequest{HostPort: 8080, IP: "172.30.1.100", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	if f.Proto != "tcp" || len(fw.last) != 1 || fw.last[0].TargetIP.String() != "172.30.1.100" || fw.last[0].HostPort != 8080 {
		t.Errorf("forward: %+v applied=%+v", f, fw.last)
	}
	for _, req := range []ForwardRequest{
		{HostPort: 8080, IP: "172.30.1.100", Port: 80}, // 重複
		{HostPort: 8081, IP: "192.168.1.10", Port: 80}, // クラスタのサブネットの外
		{HostPort: 22, IP: "172.30.1.100", Port: 22},   // SSH 用に予約
		{HostPort: 8082, IP: "172.30.1.100", Port: 0},  // ポートが不正
		{HostPort: 8083, IP: "172.30.1.100", Port: 80, Proto: "sctp"},
	} {
		if _, err := cs.AddForward(ctx, "demo", req); err == nil {
			t.Errorf("%+v: expected error", req)
		}
	}
	if list, _ := cs.ListForwards(ctx, "demo"); len(list) != 1 {
		t.Errorf("list: %+v", list)
	}
	// クラスタを消すと転送も消える
	if err := cs.Delete(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if len(fw.last) != 0 {
		t.Errorf("forwards must be removed with the cluster: %+v", fw.last)
	}
}

func TestLoadBalancerAddons(t *testing.T) {
	ctx := context.Background()
	cs, _, _, pr := newTest(t)
	cs.postCreateInterval = 10 * time.Millisecond
	fa := &fakeApplier{notReady: map[string]bool{"cilium.io/v2\n": true}} // v2 の CRD はまだ無い → v2alpha1 を使う
	cs.cfg.NewApplier = func([]byte) (Applier, error) { return fa, nil }
	pr.mu.Lock()
	pr.ready, pr.err = 1, nil
	pr.mu.Unlock()

	zero := 0
	c, err := cs.Create(ctx, CreateRequest{Name: "lb", Workers: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if c.LoadBalancerPool != "172.30.1.100-172.30.1.199" {
		t.Errorf("pool: %q", c.LoadBalancerPool)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cs.addonStatus("lb") != "applied" {
		if time.Now().After(deadline) {
			t.Fatalf("addons not applied: %q", cs.addonStatus("lb"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	fa.mu.Lock()
	all := strings.Join(fa.applied, "---")
	fa.mu.Unlock()
	if !strings.Contains(all, "kind: CiliumLoadBalancerIPPool") || !strings.Contains(all, `start: "172.30.1.100"`) ||
		!strings.Contains(all, "kind: CiliumL2AnnouncementPolicy") || !strings.Contains(all, "cilium.io/v2alpha1") {
		t.Errorf("applied manifests:\n%s", all)
	}

	// flannel のクラスタには LB のアドオンは入れない
	f := false
	c, err = cs.Create(ctx, CreateRequest{Name: "fl", Workers: &zero, CNI: "flannel", Hubble: &f})
	if err != nil {
		t.Fatal(err)
	}
	if c.LoadBalancerPool != "" || c.Addons != "" {
		t.Errorf("flannel cluster: pool=%q addons=%q", c.LoadBalancerPool, c.Addons)
	}
}

func TestStorageAddons(t *testing.T) {
	ctx := context.Background()
	cs, _, _, pr := newTest(t)
	cs.postCreateInterval = 10 * time.Millisecond
	root := t.TempDir()
	cs.cfg.NFS = NFSConfig{Server: netip.MustParseAddr("172.30.0.1"), Root: root}
	fa := &fakeApplier{}
	cs.cfg.NewApplier = func([]byte) (Applier, error) { return fa, nil }
	pr.mu.Lock()
	pr.ready, pr.err = 1, nil
	pr.mu.Unlock()

	zero := 0
	f := false
	c, err := cs.Create(ctx, CreateRequest{Name: "st", Distro: "kubeadm", Workers: &zero, CNI: "flannel", Hubble: &f})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.StorageClasses, ",") != "local-path,niwakube-nfs" {
		t.Errorf("storage classes: %v", c.StorageClasses)
	}
	if fi, err := os.Stat(filepath.Join(root, "st")); err != nil || !fi.IsDir() {
		t.Fatalf("nfs dir not created: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cs.addonStatus("st") != "applied" {
		if time.Now().After(deadline) {
			t.Fatalf("addons: %q", cs.addonStatus("st"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	fa.mu.Lock()
	all := strings.Join(fa.applied, "\n---\n")
	fa.mu.Unlock()
	if !strings.Contains(all, "local-path-provisioner") || !strings.Contains(all, "/opt/local-path-provisioner") ||
		!strings.Contains(all, "name: niwakube-nfs") || !strings.Contains(all, filepath.Join(root, "st")) {
		t.Errorf("kubeadm must get local-path and NFS:\n%.500s", all)
	}
	if strings.Contains(all, "CiliumLoadBalancerIPPool") {
		t.Error("no LoadBalancer addon for flannel")
	}

	// k3s は local-path が組み込みなので入れない
	if ms := cs.addonManifests(&store.Cluster{Name: "x", Distro: "k3s", Spec: store.ClusterSpec{CNI: "flannel"}}); len(ms) != 1 || ms[0].Name != "csi-driver-nfs" {
		t.Errorf("k3s addons: %+v", ms)
	}

	// クラスタを消すと NFS のディレクトリも消える
	if err := cs.Delete(ctx, "st"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "st")); !os.IsNotExist(err) {
		t.Errorf("nfs dir must be removed: %v", err)
	}
}

func TestKubeconfigFor(t *testing.T) {
	ctx := context.Background()
	cs, _, _, _ := newTest(t)
	zero := 0
	for _, n := range []string{"a", "b"} {
		if _, err := cs.Create(ctx, CreateRequest{Name: n, Workers: &zero}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := cs.KubeconfigFor(ctx, []string{"b", "a", "b"}, KubeconfigOptions{ProxyURL: "socks5://localhost:1080"})
	if err != nil {
		t.Fatal(err)
	}
	var kc struct {
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server   string `yaml:"server"`
				ProxyURL string `yaml:"proxy-url"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users    []struct{ Name string } `yaml:"users"`
		Contexts []struct {
			Name    string `yaml:"name"`
			Context struct{ Cluster, User string }
		} `yaml:"contexts"`
		CurrentContext string `yaml:"current-context"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		t.Fatal(err)
	}
	if len(kc.Clusters) != 2 || len(kc.Users) != 2 || len(kc.Contexts) != 2 || kc.CurrentContext != "b" {
		t.Fatalf("merged kubeconfig: %s", raw)
	}
	want := map[string]string{"a": "https://172.30.1.11:6443", "b": "https://172.30.2.11:6443"}
	for _, c := range kc.Clusters {
		if want[c.Name] != c.Cluster.Server || c.Cluster.ProxyURL != "socks5://localhost:1080" {
			t.Errorf("cluster %+v", c)
		}
	}
	for _, c := range kc.Contexts {
		if c.Context.Cluster != c.Name || c.Context.User != c.Name+"-admin" {
			t.Errorf("context %+v", c)
		}
	}
	if _, err := cs.KubeconfigFor(ctx, nil); !errors.Is(err, machine.ErrInvalid) {
		t.Errorf("no cluster: want ErrInvalid, got %v", err)
	}
	if _, err := cs.KubeconfigFor(ctx, []string{"a", "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown cluster: want ErrNotFound, got %v", err)
	}
}
