package cluster

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/backend/fake"
	"github.com/sat0ken/niwakube/internal/distro/k3s"
	"github.com/sat0ken/niwakube/internal/distro/kubeadm"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/machine"
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
	cs, err := NewService(Config{Subnet: netip.MustParsePrefix("172.30.0.0/16")}, st, ms, pr, nil, k3s.New(), kubeadm.New())
	if err != nil {
		t.Fatal(err)
	}
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
	if len(pr.calls) == 0 || pr.calls[0] != "https://172.30.1.11:6443" {
		t.Errorf("probe calls = %v", pr.calls)
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
