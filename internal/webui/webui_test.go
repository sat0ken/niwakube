package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	cases := []struct {
		path, wantType, wantBody string
		want                     int
	}{
		{"/", "text/html", `<script type="module" src="/ui/app.js">`, 200},
		{"/index.html", "text/html", "niwakube", 200},
		{"/ui/app.js", "javascript", "function route()", 200},
		{"/ui/style.css", "text/css", "--accent", 200},
		{"/ui/missing.js", "", "", 404},
		{"/api/unknown", "", "", 404},
		{"/etc/passwd", "", "", 404},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.path, rec.Code, c.want)
			continue
		}
		if c.want != 200 {
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.wantType) {
			t.Errorf("%s: Content-Type %q, want %q", c.path, ct, c.wantType)
		}
		if !strings.Contains(rec.Body.String(), c.wantBody) {
			t.Errorf("%s: body does not contain %q", c.path, c.wantBody)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
}
