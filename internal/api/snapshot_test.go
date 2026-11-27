package api

import (
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/cluster"
)

func TestSnapshotAPI(t *testing.T) {
	// 置き場所が無ければ 501
	ts0 := newTestServer(t)
	do(t, http.MethodPost, ts0.URL+"/api/clusters", `{"name":"demo","workers":0}`)
	if resp, _ := do(t, http.MethodGet, ts0.URL+"/api/clusters/demo/snapshots", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("disabled: %d", resp.StatusCode)
	}

	ts := newTestServerWith(t, cluster.Config{Subnet: netip.MustParsePrefix("172.30.0.0/16"), SnapshotDir: filepath.Join(t.TempDir(), "snap")})
	base := ts.URL + "/api/clusters/demo"
	if resp, body := do(t, http.MethodPost, ts.URL+"/api/clusters", `{"name":"demo","workers":0}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	resp, body := do(t, http.MethodPost, base+"/snapshots", `{"name":"s1"}`)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"operation":"snapshot s1`) {
		t.Errorf("create snapshot: %d %s", resp.StatusCode, body)
	}
	waitNoOperation(t, base)
	if resp, body := do(t, http.MethodGet, base+"/snapshots", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"s1"`) {
		t.Errorf("list: %d %s", resp.StatusCode, body)
	}
	// 名前を省略してもよい (本文なし)
	if resp, body := do(t, http.MethodPost, base+"/snapshots", ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("no name: %d %s", resp.StatusCode, body)
	}
	waitNoOperation(t, base)
	if resp, body := do(t, http.MethodPost, base+"/snapshots/s1/restore", ""); resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"operation":"restore s1`) {
		t.Errorf("restore: %d %s", resp.StatusCode, body)
	}
	waitNoOperation(t, base)
	// 順番が意味を持つ (消したあとにもう一度消す) ので、スライスで順に試す
	for _, c := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"duplicate", http.MethodPost, "/snapshots", `{"name":"s1"}`, http.StatusConflict},
		{"bad name", http.MethodPost, "/snapshots", `{"name":"S 1"}`, http.StatusBadRequest},
		{"restore unknown", http.MethodPost, "/snapshots/nope/restore", "", http.StatusNotFound},
		{"delete", http.MethodDelete, "/snapshots/s1", "", http.StatusNoContent},
		{"delete again", http.MethodDelete, "/snapshots/s1", "", http.StatusNotFound},
	} {
		if resp, body := do(t, c.method, base+c.path, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, body)
		}
	}
	if resp, _ := do(t, http.MethodGet, ts.URL+"/api/clusters/nope/snapshots", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown cluster: %d", resp.StatusCode)
	}
}

func waitNoOperation(t *testing.T, clusterURL string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second) // 状態の確認は API に届かないと 3 秒待つ
	for {
		_, body := do(t, http.MethodGet, clusterURL, "")
		if !strings.Contains(body, `"operation"`) {
			return
		}
		if strings.Contains(body, `"operation":"failed`) || time.Now().After(deadline) {
			t.Fatalf("operation did not finish: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
