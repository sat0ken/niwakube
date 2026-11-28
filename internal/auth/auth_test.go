package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeGitHub はトークンの交換と GitHub の API の真似をする。
func fakeGitHub(t *testing.T, login string, emails []map[string]any, orgs []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "s3cret" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"at","token_type":"bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer at" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 42, "login": login, "name": "Alice"})
		case "/user/emails":
			json.NewEncoder(w).Encode(emails)
		case "/user/orgs":
			var out []map[string]string
			for _, o := range orgs {
				out = append(out, map[string]string{"login": o})
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestAuth(t *testing.T, gh *httptest.Server, allow string) (*Authenticator, http.Handler) {
	t.Helper()
	p := GitHub("cid", "s3cret")
	p.Endpoint = oauth2.Endpoint{AuthURL: gh.URL + "/authorize", TokenURL: gh.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
	p.API = gh.URL
	al, err := ParseAllowList(allow)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Providers: []*Provider{p}, Allow: al, Token: "0123456789abcdef-token", Key: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pong")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ui")) })
	return a, a.Protect("/api/", mux)
}

func cookieOf(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// loginFlow は /auth/login → (プロバイダー) → /auth/callback を通し、callback の応答を返す。
func loginFlow(t *testing.T, h http.Handler, code string, tamper func(*url.Values)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:18080/auth/login/github?next=/%23/clusters/demo", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("redirect_uri") != "http://localhost:18080/auth/callback/github" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Fatalf("authorize URL: %s", loc)
	}
	st := cookieOf(rec, stateCookie)
	if st == nil || !st.HttpOnly || st.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie: %+v", st)
	}
	cb := url.Values{"code": {code}, "state": {q.Get("state")}}
	if tamper != nil {
		tamper(&cb)
	}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:18080/auth/callback/github?"+cb.Encode(), nil)
	req.AddCookie(st)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginAndProtect(t *testing.T) {
	gh := fakeGitHub(t, "Alice", []map[string]any{{"email": "Alice@Example.com", "verified": true, "primary": true}}, nil)
	defer gh.Close()
	_, h := newTestAuth(t, gh, "github:alice")

	get := func(path string, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:18080"+path, nil)
		if c != nil {
			req.AddCookie(c)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// ログインしていなければ API は 401、Web UI とログインの URL は通る
	if rec := get("/api/ping", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("api without login: %d", rec.Code)
	}
	if rec := get("/", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("ui: %d", rec.Code)
	}
	if rec := get("/auth/providers", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"github"`) {
		t.Errorf("providers: %d %s", rec.Code, rec.Body)
	}

	rec := loginFlow(t, h, "good-code", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/#/clusters/demo" {
		t.Fatalf("callback: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	sess := cookieOf(rec, sessionCookie)
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.MaxAge <= 0 {
		t.Fatalf("session cookie: %+v", sess)
	}
	if rec := get("/api/ping", sess, nil); rec.Code != http.StatusOK || rec.Body.String() != "pong" {
		t.Errorf("api with session: %d %s", rec.Code, rec.Body)
	}
	if rec := get("/auth/me", sess, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"Alice"`) || !strings.Contains(rec.Body.String(), "alice@example.com") {
		t.Errorf("me: %d %s", rec.Code, rec.Body)
	}
	// Cookie を書き換えると通らない
	bad := *sess
	bad.Value = strings.Replace(bad.Value, ".", "x.", 1)
	if rec := get("/api/ping", &bad, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered cookie: %d", rec.Code)
	}
	// トークン
	if rec := get("/api/ping", nil, map[string]string{"Authorization": "Bearer 0123456789abcdef-token"}); rec.Code != http.StatusOK {
		t.Errorf("token: %d", rec.Code)
	}
	if rec := get("/api/ping", sess, map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token must not fall back to the cookie: %d", rec.Code)
	}
	// ログアウト
	req := httptest.NewRequest(http.MethodPost, "http://localhost:18080/auth/logout", nil)
	lrec := httptest.NewRecorder()
	h.ServeHTTP(lrec, req)
	if c := cookieOf(lrec, sessionCookie); lrec.Code != http.StatusNoContent || c == nil || c.MaxAge >= 0 {
		t.Errorf("logout: %d %+v", lrec.Code, c)
	}
}

func TestLoginDeniedAndBadState(t *testing.T) {
	gh := fakeGitHub(t, "mallory", []map[string]any{{"email": "m@evil.example", "verified": true, "primary": true}, {"email": "alice@example.com", "verified": false}}, []string{"other"})
	defer gh.Close()
	_, h := newTestAuth(t, gh, "github:alice alice@example.com github-org:myorg")
	// 確認していないメールアドレスや、許可リストに無い Organization では入れない
	if rec := loginFlow(t, h, "good-code", nil); rec.Code != http.StatusForbidden || cookieOf(rec, sessionCookie) != nil {
		t.Errorf("denied: %d %s", rec.Code, rec.Body)
	}
	if rec := loginFlow(t, h, "good-code", func(v *url.Values) { v.Set("state", "forged") }); rec.Code != http.StatusBadRequest {
		t.Errorf("forged state: %d", rec.Code)
	}
	if rec := loginFlow(t, h, "bad-code", nil); rec.Code != http.StatusBadGateway {
		t.Errorf("bad code: %d", rec.Code)
	}
}

func TestOrgMember(t *testing.T) {
	gh := fakeGitHub(t, "bob", nil, []string{"MyOrg"})
	defer gh.Close()
	_, h := newTestAuth(t, gh, "github-org:myorg")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:18080/auth/login/github", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if !strings.Contains(loc.Query().Get("scope"), "read:org") {
		t.Errorf("org rules need read:org: %s", loc.Query().Get("scope"))
	}
	if rec := loginFlow(t, h, "good-code", nil); rec.Code != http.StatusFound {
		t.Errorf("org member: %d %s", rec.Code, rec.Body)
	}
}

func TestSessionExpiresAndAllowListChanges(t *testing.T) {
	gh := fakeGitHub(t, "alice", nil, nil)
	defer gh.Close()
	a, _ := newTestAuth(t, gh, "github:alice")
	id := Identity{Provider: "github", Login: "alice"}
	mk := func(exp time.Time) *http.Request {
		v, _ := a.sign(session{Identity: id, Exp: exp.Unix()})
		r := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
		return r
	}
	if _, ok := a.User(mk(time.Now().Add(time.Hour))); !ok {
		t.Error("valid session")
	}
	if _, ok := a.User(mk(time.Now().Add(-time.Second))); ok {
		t.Error("expired session must be rejected")
	}
	a.cfg.Allow, _ = ParseAllowList("github:someone-else")
	if _, ok := a.User(mk(time.Now().Add(time.Hour))); ok {
		t.Error("a user removed from the allow list must be rejected")
	}
}

func TestAllowList(t *testing.T) {
	a, err := ParseAllowList("alice@example.com, @corp.example\ngithub:Bob github-org:MyOrg")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   Identity
		want bool
	}{
		{Identity{Provider: "google", Emails: []string{"alice@example.com"}}, true},
		{Identity{Provider: "google", Emails: []string{"carol@corp.example"}}, true},
		{Identity{Provider: "google", Emails: []string{"carol@evilcorp.example"}}, false},
		{Identity{Provider: "github", Login: "bob"}, true},
		{Identity{Provider: "google", Login: "bob"}, false},
		{Identity{Provider: "github", Login: "x", Orgs: []string{"myorg"}}, true},
		{Identity{Provider: "github", Login: "x", Orgs: []string{"other"}}, false},
	} {
		if got := a.Allows(c.id); got != c.want {
			t.Errorf("%+v: got %v", c.id, got)
		}
	}
	for _, bad := range []string{"bob", "@", "github:", "a@b@c"} {
		if _, err := ParseAllowList(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestNewValidates(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	empty, _ := ParseAllowList("")
	allow, _ := ParseAllowList("github:alice")
	for name, cfg := range map[string]Config{
		"nothing":       {Key: key},
		"no allow list": {Providers: []*Provider{GitHub("id", "secret")}, Allow: empty, Key: key},
		"short token":   {Token: "short", Key: key},
		"short key":     {Token: "0123456789abcdef", Key: []byte("k")},
		"no secret":     {Providers: []*Provider{GitHub("id", "")}, Allow: allow, Key: key},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	if _, err := New(Config{Token: "0123456789abcdef", Key: key}); err != nil {
		t.Errorf("token only: %v", err)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/#/x": "/#/x", "": "/", "//evil.example": "/", "https://evil.example": "/", "/\\evil": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}

func TestLoadKey(t *testing.T) {
	p := t.TempDir() + "/auth/session.key"
	k1, err := LoadKey(p)
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, err := LoadKey(p)
	if err != nil || string(k1) != string(k2) {
		t.Error("the key must persist")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadFile(dir + "/none.json"); err != ErrNoConfig {
		t.Errorf("missing file: %v", err)
	}
	p := dir + "/auth.json"
	body := `{"allow":["github:alice","alice@example.com"],"github":{"clientID":"id","clientSecret":"sec"},"apiToken":"0123456789abcdef","baseURL":"http://localhost:18080"}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("world-readable file must be rejected: %v", err)
	}
	os.Chmod(p, 0o600)
	cfg, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "github" || cfg.Token != "0123456789abcdef" || cfg.BaseURL != "http://localhost:18080" ||
		!cfg.Allow.Allows(Identity{Provider: "github", Login: "alice"}) {
		t.Errorf("config: %+v", cfg)
	}
	os.WriteFile(p, []byte(`{"allow":["x@y.z"],"githb":{}}`), 0o600)
	if _, err := LoadFile(p); err == nil {
		t.Error("unknown fields must be rejected (typo)")
	}
}
