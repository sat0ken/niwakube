package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
)

// Provider はログインに使う OAuth のプロバイダー (Google か GitHub)。
type Provider struct {
	Name     string // google | github
	Label    string // ボタンの文字
	ClientID string
	Secret   string
	Endpoint oauth2.Endpoint
	Scopes   []string
	// API はユーザーの情報を取る API の URL (テストで差し替える)
	API string
}

// Google は Google のログイン (OpenID Connect の userinfo でメールアドレスを取る)。
func Google(clientID, secret string) *Provider {
	return &Provider{
		Name: "google", Label: "Google でログイン", ClientID: clientID, Secret: secret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:  "https://oauth2.googleapis.com/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes: []string{"openid", "email", "profile"},
		API:    "https://openidconnect.googleapis.com/v1/userinfo",
	}
}

// GitHub は GitHub のログイン (OAuth App)。
func GitHub(clientID, secret string) *Provider {
	return &Provider{
		Name: "github", Label: "GitHub でログイン", ClientID: clientID, Secret: secret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://github.com/login/oauth/authorize",
			TokenURL:  "https://github.com/login/oauth/access_token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		Scopes: []string{"read:user", "user:email"},
		API:    "https://api.github.com",
	}
}

func (p *Provider) config(redirect string, orgs bool) *oauth2.Config {
	scopes := p.Scopes
	if p.Name == "github" && orgs {
		scopes = append(append([]string{}, scopes...), "read:org")
	}
	return &oauth2.Config{ClientID: p.ClientID, ClientSecret: p.Secret, Endpoint: p.Endpoint, RedirectURL: redirect, Scopes: scopes}
}

// identity はアクセストークンでユーザーの情報を取る。
func (p *Provider) identity(ctx context.Context, client *http.Client, orgs bool) (Identity, error) {
	switch p.Name {
	case "google":
		var u struct {
			Sub           string `json:"sub"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
			Name          string `json:"name"`
		}
		if err := getJSON(ctx, client, p.API, &u); err != nil {
			return Identity{}, err
		}
		id := Identity{Provider: "google", Subject: u.Sub, Name: u.Name}
		if u.Email != "" && u.EmailVerified {
			id.Emails = []string{strings.ToLower(u.Email)}
		}
		return id, nil
	case "github":
		var u struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Name  string `json:"name"`
		}
		if err := getJSON(ctx, client, p.API+"/user", &u); err != nil {
			return Identity{}, err
		}
		id := Identity{Provider: "github", Subject: strconv.FormatInt(u.ID, 10), Login: u.Login, Name: u.Name}
		var emails []struct {
			Email    string `json:"email"`
			Verified bool   `json:"verified"`
			Primary  bool   `json:"primary"`
		}
		if err := getJSON(ctx, client, p.API+"/user/emails", &emails); err != nil {
			return Identity{}, err
		}
		for _, e := range emails {
			if !e.Verified {
				continue
			}
			if e.Primary {
				id.Emails = append([]string{strings.ToLower(e.Email)}, id.Emails...)
			} else {
				id.Emails = append(id.Emails, strings.ToLower(e.Email))
			}
		}
		if orgs {
			var os []struct {
				Login string `json:"login"`
			}
			if err := getJSON(ctx, client, p.API+"/user/orgs?per_page=100", &os); err != nil {
				return Identity{}, err
			}
			for _, o := range os {
				id.Orgs = append(id.Orgs, o.Login)
			}
		}
		return id, nil
	}
	return Identity{}, fmt.Errorf("unknown provider %q", p.Name)
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return json.Unmarshal(body, v)
}
