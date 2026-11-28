package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/auth"
	"github.com/sat0ken/niwakube/internal/webui"
)

func TestAuthProtectsAPI(t *testing.T) {
	allow, _ := auth.ParseAllowList("github:alice")
	a, err := auth.New(auth.Config{Providers: []*auth.Provider{auth.GitHub("id", "secret")}, Allow: allow,
		Token: "0123456789abcdef-token", Key: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Options{Auth: a, UI: webui.Handler()})
	get := func(path, token string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := get("/api/vms", ""); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "login required") {
		t.Errorf("api without login: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get("/api/vms", "0123456789abcdef-token"); resp.StatusCode != http.StatusOK {
		t.Errorf("api with token: %d", resp.StatusCode)
	}
	for _, p := range []string{"/", "/ui/app.js", "/healthz", "/auth/providers"} {
		if resp, _ := get(p, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("%s must be open: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := get("/auth/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me without login: %d", resp.StatusCode)
	}
	// ログアウトもほかの POST と同じく、別のサイトからは断る
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site logout: %d", resp.StatusCode)
	}
}
