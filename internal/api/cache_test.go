package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/regcache"
)

func TestCacheAPI(t *testing.T) {
	ts0 := newTestServer(t)
	if resp, _ := do(t, http.MethodGet, ts0.URL+"/api/cache", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("disabled: %d", resp.StatusCode)
	}
	c, err := regcache.New(regcache.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Options{Cache: c})
	if resp, body := do(t, http.MethodGet, ts.URL+"/api/cache", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"blobs":0`) {
		t.Errorf("stats: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, http.MethodDelete, ts.URL+"/api/cache", ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("clear: %d", resp.StatusCode)
	}
}
