// Package auth は Web UI と API のログイン (Google / GitHub の OAuth) と、API トークンを扱う。
//
//   - ログインは OAuth 2.0 の authorization code (PKCE つき)。戻ってきたら、許可リストに合う人だけを通す
//   - ログインした人は、署名した Cookie (HttpOnly、SameSite=Lax) で覚える。サーバーには状態を持たない
//   - ブラウザ以外 (curl やスクリプト) は、Authorization: Bearer <トークン> でも通す
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	sessionCookie = "niwakube_session"
	stateCookie   = "niwakube_oauth"
	// DefaultSessionTTL はログインを覚えておく長さ。
	DefaultSessionTTL = 7 * 24 * time.Hour
	stateTTL          = 10 * time.Minute
)

// Config はログインの設定。
type Config struct {
	Providers []*Provider
	Allow     AllowList
	// Token が空でなければ、Authorization: Bearer <Token> の API の呼び出しも通す
	Token string
	// Key は Cookie の署名の鍵 (32 バイト以上)
	Key []byte
	// BaseURL は OAuth の戻り先の URL の前半 (例: http://localhost:18080)。空ならリクエストの Host から作る
	BaseURL    string
	SessionTTL time.Duration
	// HTTPClient はプロバイダーとの通信に使う (テストで差し替える)
	HTTPClient *http.Client
	Log        *slog.Logger
}

// Authenticator はログインを扱う。
type Authenticator struct {
	cfg Config
}

// New は設定を確かめて Authenticator を作る。
func New(cfg Config) (*Authenticator, error) {
	if len(cfg.Providers) == 0 && cfg.Token == "" {
		return nil, errors.New("auth: no provider and no API token")
	}
	if len(cfg.Providers) > 0 && cfg.Allow.Empty() {
		// 許可リストが無いと、Google / GitHub のアカウントを持つ誰でも入れてしまう
		return nil, errors.New(`auth: "allow" is required when a login provider is configured`)
	}
	if cfg.Token != "" && len(cfg.Token) < 16 {
		return nil, errors.New("auth: the API token must be at least 16 characters")
	}
	if len(cfg.Key) < 32 {
		return nil, errors.New("auth: the session key must be at least 32 bytes")
	}
	for _, p := range cfg.Providers {
		if p.ClientID == "" || p.Secret == "" {
			return nil, fmt.Errorf("auth: %s needs both a client ID and a client secret", p.Name)
		}
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	return &Authenticator{cfg: cfg}, nil
}

// LoadKey は Cookie の署名の鍵を path から読む。無ければ作って保存する (0600)。
func LoadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) < 32 {
			return nil, fmt.Errorf("%s: the session key is too short", path)
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// --- 署名した Cookie ---

func (a *Authenticator) sign(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, a.cfg.Key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (a *Authenticator) verify(s string, v any) bool {
	p, sig, ok := strings.Cut(s, ".")
	if !ok {
		return false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.cfg.Key)
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return false
	}
	return json.Unmarshal(payload, v) == nil
}

type session struct {
	Identity
	Exp int64 `json:"exp"`
}

type oauthState struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Verifier string `json:"v"`
	Redirect string `json:"r"`
	Next     string `json:"n"`
	Exp      int64  `json:"exp"`
}

func secure(r *http.Request) bool { return r.TLS != nil }

func (a *Authenticator) setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
		MaxAge: int(ttl.Seconds()),
	})
}

func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// User はリクエストのログインした人を返す (トークンなら Provider は "token")。
func (a *Authenticator) User(r *http.Request) (Identity, bool) {
	if a.cfg.Token != "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(a.cfg.Token)) == 1 {
				return Identity{Provider: "token", Name: "API トークン"}, true
			}
			return Identity{}, false
		}
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Identity{}, false
	}
	var s session
	if !a.verify(c.Value, &s) || time.Now().Unix() > s.Exp {
		return Identity{}, false
	}
	// 許可リストから外した人は、Cookie が残っていても通さない
	if !a.cfg.Allow.Allows(s.Identity) {
		return Identity{}, false
	}
	return s.Identity, true
}

// Protect は next の前でログインを確かめる。prefix で始まるパスだけ (Web UI の静的ファイルやログインの画面は通す)。
func (a *Authenticator) Protect(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			if _, ok := a.User(r); !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"login required"}` + "\n"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Register はログインの URL を mux に足す。
//
//	GET  /auth/providers           ログインに使えるプロバイダー (Web UI のボタン)
//	GET  /auth/me                  ログインしている人 (していなければ 401)
//	GET  /auth/login/{provider}    プロバイダーのログインの画面へ (?next=/#/... で戻り先)
//	GET  /auth/callback/{provider} プロバイダーからの戻り
//	POST /auth/logout              ログアウト
func (a *Authenticator) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/providers", a.providers)
	mux.HandleFunc("GET /auth/me", a.me)
	mux.HandleFunc("GET /auth/login/{provider}", a.login)
	mux.HandleFunc("GET /auth/callback/{provider}", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *Authenticator) provider(name string) *Provider {
	for _, p := range a.cfg.Providers {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (a *Authenticator) providers(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	out := []item{}
	for _, p := range a.cfg.Providers {
		out = append(out, item{p.Name, p.Label})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Authenticator) me(w http.ResponseWriter, r *http.Request) {
	id, ok := a.User(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": id.Provider, "name": id.Display(), "emails": id.Emails})
}

// safeNext は戻り先を、このサーバーの中のパスだけにする (外のサイトへのリダイレクトに使わせない)。
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (a *Authenticator) redirectURL(r *http.Request, provider string) string {
	base := a.cfg.BaseURL
	if base == "" {
		scheme := "http"
		if secure(r) {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/auth/callback/" + provider
}

func randomString() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	p := a.provider(r.PathValue("provider"))
	if p == nil {
		http.NotFound(w, r)
		return
	}
	state, err := randomString()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	st := oauthState{Provider: p.Name, State: state, Verifier: oauth2.GenerateVerifier(), Redirect: a.redirectURL(r, p.Name),
		Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(stateTTL).Unix()}
	v, err := a.sign(st)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.setCookie(w, r, stateCookie, v, stateTTL)
	conf := p.config(st.Redirect, a.cfg.Allow.NeedsOrgs())
	http.Redirect(w, r, conf.AuthCodeURL(state, oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	p := a.provider(r.PathValue("provider"))
	if p == nil {
		http.NotFound(w, r)
		return
	}
	var st oauthState
	c, err := r.Cookie(stateCookie)
	q := r.URL.Query()
	if err != nil || !a.verify(c.Value, &st) || time.Now().Unix() > st.Exp || st.Provider != p.Name ||
		subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		a.page(w, http.StatusBadRequest, "ログインをやり直してください", "ログインの途中の情報が合いません (時間が経ちすぎたか、別のタブで始めたログインです)。")
		return
	}
	clearCookie(w, r, stateCookie)
	if e := q.Get("error"); e != "" {
		a.page(w, http.StatusForbidden, "ログインできませんでした", fmt.Sprintf("%s がログインを断りました (%s)。", p.Label, e))
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, a.cfg.HTTPClient)
	conf := p.config(st.Redirect, a.cfg.Allow.NeedsOrgs())
	tok, err := conf.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		a.cfg.Log.Warn("auth: token exchange failed", "provider", p.Name, "err", err)
		a.page(w, http.StatusBadGateway, "ログインできませんでした", "プロバイダーとのやり取りに失敗しました。もう一度試してください。")
		return
	}
	id, err := p.identity(ctx, conf.Client(ctx, tok), a.cfg.Allow.NeedsOrgs())
	if err != nil {
		a.cfg.Log.Warn("auth: cannot get the user", "provider", p.Name, "err", err)
		a.page(w, http.StatusBadGateway, "ログインできませんでした", "ユーザーの情報を取れませんでした。もう一度試してください。")
		return
	}
	id.Orgs = a.cfg.Allow.Orgs(id.Orgs) // Cookie には許可リストにある Organization だけ入れる
	if !a.cfg.Allow.Allows(id) {
		a.cfg.Log.Warn("auth: login denied", "provider", p.Name, "user", id.Display(), "emails", id.Emails)
		a.page(w, http.StatusForbidden, "このアカウントは使えません",
			fmt.Sprintf("%s (%s) は、niwakube の許可リスト (--auth-allow) にありません。管理者に追加してもらってください。", id.Display(), p.Name))
		return
	}
	v, err := a.sign(session{Identity: id, Exp: time.Now().Add(a.cfg.SessionTTL).Unix()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.setCookie(w, r, sessionCookie, v, a.cfg.SessionTTL)
	a.cfg.Log.Info("auth: logged in", "provider", p.Name, "user", id.Display())
	http.Redirect(w, r, st.Next, http.StatusFound)
}

func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

// page はログインの結果の簡単な画面を出す (Web UI の外なので素の HTML)。
func (a *Authenticator) page(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>niwakube</title><body style="font-family:system-ui,sans-serif;max-width:560px;margin:80px auto;padding:0 16px;line-height:1.7">
<h1 style="font-size:20px">%s</h1><p>%s</p><p><a href="/">niwakube に戻る</a></p></body></html>`, htmlEscape(title), htmlEscape(msg))
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}
