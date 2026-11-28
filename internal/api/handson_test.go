package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/handson"
)

func TestHandsonAPI(t *testing.T) {
	ts0 := newTestServer(t)
	if resp, _ := do(t, http.MethodGet, ts0.URL+"/api/handson/chapters", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("disabled: %d", resp.StatusCode)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chapter_cicd/README_CI.md":
			w.Write([]byte("# CI\n\n![](image/ci.svg)\n"))
		case "/chapter_cicd/image/ci.svg":
			w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	h := handson.New()
	h.Base = gh.URL + "/"
	ts := newTestServer(t, Options{Handson: h})

	if resp, body := do(t, http.MethodGet, ts.URL+"/api/handson/chapters", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"chapter_cluster-create"`) {
		t.Errorf("chapters: %d %s", resp.StatusCode, body)
	}
	resp, body := do(t, http.MethodGet, ts.URL+"/api/handson/chapters/chapter_cicd?doc=README_CI.md", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `/api/handson/files/chapter_cicd/image/ci.svg`) || !strings.Contains(body, `"file":"README_CI.md"`) {
		t.Errorf("page: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, ts.URL+"/api/handson/files/chapter_cicd/image/ci.svg", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(body, "<svg") {
		t.Errorf("file: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	for _, u := range []string{"/api/handson/chapters/chapter_nope", "/api/handson/chapters/chapter_cicd?doc=../../etc", "/api/handson/files/chapter_cicd/README.md", "/api/handson/files/chapter_cicd/image/none.png"} {
		if resp, _ := do(t, http.MethodGet, ts.URL+u, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", u, resp.StatusCode)
		}
	}
}
