package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/backend/fake"
	"github.com/sat0ken/niwakube/internal/cluster"
	"github.com/sat0ken/niwakube/internal/distro/k3s"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

func newTestServer(t *testing.T, opts ...Options) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := machine.NewService(machine.Config{
		Bridge:  "nkbr0",
		Subnet:  netip.MustParsePrefix("172.30.0.0/16"),
		Gateway: netip.MustParseAddr("172.30.0.1"),
		Pool:    ipam.Pool{Start: netip.MustParseAddr("172.30.0.10"), End: netip.MustParseAddr("172.30.0.254")},
	}, st, nil, fake.New())
	if err != nil {
		t.Fatal(err)
	}
	cs, err := cluster.NewService(cluster.Config{Subnet: netip.MustParsePrefix("172.30.0.0/16")}, st, svc, nil, nil, k3s.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Close) // 裏の処理 (ノードの大きさの変更など) を止める
	ts := httptest.NewServer(New(svc, cs, nil, opts...).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestVMLifecycleAPI(t *testing.T) {
	ts := newTestServer(t)
	base := ts.URL + "/api/vms"

	resp, body := do(t, http.MethodPost, base, `{"name":"vm1","cpus":4}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var vm machine.Machine
	if err := json.Unmarshal([]byte(body), &vm); err != nil {
		t.Fatal(err)
	}
	if vm.Name != "vm1" || vm.CPUs != 4 || vm.IP != "172.30.0.10" || vm.Status.State != "running" {
		t.Errorf("unexpected vm: %+v", vm)
	}

	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "", `{"name":"vm1"}`, http.StatusConflict},
		{http.MethodPost, "", `{"name":"BAD"}`, http.StatusBadRequest},
		{http.MethodPost, "", `{"name":"x","unknown":1}`, http.StatusBadRequest},
		{http.MethodPost, "", `not json`, http.StatusBadRequest},
		{http.MethodGet, "", "", http.StatusOK},
		{http.MethodGet, "/vm1", "", http.StatusOK},
		{http.MethodGet, "/nope", "", http.StatusNotFound},
		{http.MethodPost, "/vm1/stop", "", http.StatusOK},
		{http.MethodPost, "/vm1/start", "", http.StatusOK},
		{http.MethodPost, "/vm1/stop?force=true", "", http.StatusOK},
		{http.MethodGet, "/vm1/serial", "", http.StatusNotImplemented}, // fake は SerialLog 未対応
		{http.MethodGet, "/vm1/serial?bytes=0", "", http.StatusBadRequest},
		{http.MethodDelete, "/vm1", "", http.StatusNoContent},
		{http.MethodDelete, "/vm1", "", http.StatusNotFound},
	}
	for _, c := range cases {
		resp, body := do(t, c.method, base+c.path, c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: got %d, want %d (%s)", c.method, c.path, resp.StatusCode, c.want, body)
		}
	}
}

func TestBackendsAndHealthz(t *testing.T) {
	ts := newTestServer(t)
	if resp, _ := do(t, http.MethodGet, ts.URL+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
	resp, body := do(t, http.MethodGet, ts.URL+"/api/backends", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"fake"`) || !strings.Contains(body, `"default":true`) {
		t.Errorf("backends: %d %s", resp.StatusCode, body)
	}
}

func TestClusterAPI(t *testing.T) {
	ts := newTestServer(t)
	base := ts.URL + "/api/clusters"

	resp, body := do(t, http.MethodPost, base, `{"name":"demo","workers":1}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var c cluster.Cluster
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "demo" || len(c.Machines) != 2 || c.Status.Phase != "provisioning" {
		t.Errorf("unexpected cluster: %+v", c)
	}

	resp, body = do(t, http.MethodGet, base+"/demo/kubeconfig", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "current-context: demo") ||
		resp.Header.Get("Content-Type") != "application/yaml" {
		t.Errorf("kubeconfig: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, base+"/demo/kubeconfig?proxy=socks5%3A%2F%2Flocalhost%3A1080", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "proxy-url: socks5://localhost:1080") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename="demo-ssh.kubeconfig"`) {
		t.Errorf("kubeconfig via SSH: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, body = do(t, http.MethodGet, ts.URL+"/api/kubeconfig?cluster=demo&proxy=socks5%3A%2F%2Flocalhost%3A1080", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "current-context: demo") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename="niwakube-ssh.kubeconfig"`) {
		t.Errorf("merged kubeconfig: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	for path, want := range map[string]int{"/api/kubeconfig": http.StatusBadRequest, "/api/kubeconfig?cluster=demo&cluster=nope": http.StatusNotFound} {
		if resp, body := do(t, http.MethodGet, ts.URL+path, ""); resp.StatusCode != want {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}

	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "", "", http.StatusOK},
		{http.MethodGet, "/demo", "", http.StatusOK},
		{http.MethodGet, "/nope", "", http.StatusNotFound},
		{http.MethodPost, "", `{"name":"demo"}`, http.StatusConflict},
		{http.MethodPost, "", `{"name":"x","distro":"nope"}`, http.StatusBadRequest},
		{http.MethodPost, "/demo/stop", "", http.StatusOK},
		{http.MethodPost, "/demo/start", "", http.StatusOK},
		{http.MethodGet, "/nope/kubeconfig", "", http.StatusNotFound},
		{http.MethodGet, "/demo/kubeconfig?proxy=ftp%3A%2F%2Fx%3A1", "", http.StatusBadRequest},
		{http.MethodGet, "/demo/talosconfig", "", http.StatusNotImplemented},
		{http.MethodPost, "/demo/forwards", `{"hostPort":18080,"ip":"172.30.1.100","port":8080}`, http.StatusCreated},
		{http.MethodPost, "/demo/forwards", `{"hostPort":18080,"ip":"172.30.1.100","port":8080}`, http.StatusConflict},
		{http.MethodPost, "/demo/forwards", `{"hostPort":18081,"ip":"10.0.0.1","port":8080}`, http.StatusBadRequest},
		{http.MethodGet, "/demo/forwards", "", http.StatusOK},
		{http.MethodDelete, "/demo/forwards/tcp/18080", "", http.StatusNoContent},
		{http.MethodDelete, "/demo/forwards/tcp/18080", "", http.StatusNotFound},
	}
	for _, c := range cases {
		resp, body := do(t, c.method, base+c.path, c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: got %d, want %d (%s)", c.method, c.path, resp.StatusCode, c.want, body)
		}
	}

	// クラスタのノードは /api/vms からは消せない
	if resp, _ := do(t, http.MethodDelete, ts.URL+"/api/vms/demo-worker-1", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete cluster vm: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, ts.URL+"/api/distros", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("distros: %d", resp.StatusCode)
	}
	if resp, body := do(t, http.MethodGet, ts.URL+"/api/cluster-defaults", ""); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"cni":"cilium"`) || !strings.Contains(body, `"default":"runc"`) {
		t.Errorf("cluster-defaults: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodDelete, base+"/demo", ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, http.MethodGet, ts.URL+"/api/vms/demo-cp-1", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("vm should be gone: %d", resp.StatusCode)
	}
}

func TestResizeAPI(t *testing.T) {
	ts := newTestServer(t)
	if resp, body := do(t, http.MethodPost, ts.URL+"/api/vms", `{"name":"vm1","cpus":1,"memoryMiB":1024,"diskGiB":10}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	resp, body := do(t, http.MethodPost, ts.URL+"/api/vms/vm1/resize", `{"cpus":2,"diskGiB":20}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"cpus":2`) || !strings.Contains(body, `"diskGiB":20`) {
		t.Errorf("resize vm: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, ts.URL+"/api/vms/vm1/resize", `{"diskGiB":10}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("shrink: %d %s", resp.StatusCode, body)
	}

	if resp, body := do(t, http.MethodPost, ts.URL+"/api/clusters", `{"name":"demo","workers":1}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cluster: %d %s", resp.StatusCode, body)
	}
	// クラスタのノードは単体の API では変えられない
	if resp, body := do(t, http.MethodPost, ts.URL+"/api/vms/demo-worker-1/resize", `{"cpus":4}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("cluster node via vm API: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPost, ts.URL+"/api/clusters/demo/resize", `{"role":"worker","size":{"memoryMiB":2048}}`)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"operation":"resizing`) {
		t.Errorf("resize cluster: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, ts.URL+"/api/clusters/demo/resize", `{"role":"worker","size":{"cpus":4}}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("resize while busy: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPost, ts.URL+"/api/clusters/demo/resize", `{"role":"etcd"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad role: %d %s", resp.StatusCode, body)
	}
}
