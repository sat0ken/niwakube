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
