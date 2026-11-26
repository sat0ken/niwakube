package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/practice"
	"github.com/sat0ken/niwakube/internal/store"
)

func TestPracticeAPI(t *testing.T) {
	// 練習問題の機能が無ければ 501
	ts0 := newTestServer(t)
	if resp, _ := do(t, http.MethodGet, ts0.URL+"/api/practice/problems", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("without practice: %d", resp.StatusCode)
	}

	drills, err := practice.NewService(func(_ context.Context, name string) (practice.Cluster, error) {
		return nil, fmt.Errorf("cluster %s: %w", name, store.ErrNotFound)
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Options{Practice: drills})
	base := ts.URL + "/api/practice/problems"

	resp, body := do(t, http.MethodGet, base, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"id":"ckad-cronjob"`) || strings.Contains(body, `"answer"`) {
		t.Errorf("list: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, base+"/ckad-cronjob", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"task"`) || !strings.Contains(body, `"checks"`) {
		t.Errorf("get: %d %s", resp.StatusCode, body)
	}
	for name, c := range map[string]struct {
		method, path, body string
		want               int
	}{
		"unknown problem":  {http.MethodGet, "/nope", "", http.StatusNotFound},
		"setup no cluster": {http.MethodPost, "/ckad-cronjob/setup", `{}`, http.StatusBadRequest},
		"setup bad json":   {http.MethodPost, "/ckad-cronjob/setup", `{"x":1}`, http.StatusBadRequest},
		"setup unknown":    {http.MethodPost, "/ckad-cronjob/setup", `{"cluster":"nope"}`, http.StatusNotFound},
		"check unknown":    {http.MethodPost, "/ckad-cronjob/check", `{"cluster":"nope"}`, http.StatusNotFound},
		"check bad id":     {http.MethodPost, "/nope/check", `{"cluster":"demo"}`, http.StatusNotFound},
	} {
		if resp, body := do(t, c.method, base+c.path, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
}
