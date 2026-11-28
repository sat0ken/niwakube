package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// File はログインの設定ファイル (既定 /etc/niwakube/auth.json)。クライアントシークレットが入るので 0600 にする。
//
//	{
//	  "allow": ["alice@example.com", "@example.com", "github:alice", "github-org:myorg"],
//	  "google": {"clientID": "xxx.apps.googleusercontent.com", "clientSecret": "..."},
//	  "github": {"clientID": "Iv1.xxx", "clientSecret": "..."},
//	  "apiToken": "curl やスクリプトから使うトークン (16 文字以上)",
//	  "baseURL": "http://localhost:18080"
//	}
//
// google と github はどちらか片方でもよい。baseURL は OAuth の戻り先で、省略するとブラウザが開いている URL から作る。
type File struct {
	Allow  []string `json:"allow"`
	Google *Client  `json:"google,omitempty"`
	GitHub *Client  `json:"github,omitempty"`
	// APIToken が空でなければ、Authorization: Bearer <APIToken> の呼び出しも通す
	APIToken string `json:"apiToken,omitempty"`
	BaseURL  string `json:"baseURL,omitempty"`
}

// Client は OAuth のクライアント (Google の OAuth クライアント ID、GitHub の OAuth App)。
type Client struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

// ErrNoConfig は設定ファイルが無い (ログインを使わない)。
var ErrNoConfig = errors.New("no auth config")

// LoadFile は設定ファイルを読み、Config にする (Key と Log は呼ぶ側で入れる)。
// ファイルが無ければ ErrNoConfig。ほかのユーザーが読めるファイルは断る (シークレットが入るため)。
func LoadFile(path string) (Config, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNoConfig
	}
	if err != nil {
		return Config{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("%s is readable by other users (mode %o); run: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var f File
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	allow, err := ParseAllowList(strings.Join(f.Allow, ","))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg := Config{Allow: allow, Token: f.APIToken, BaseURL: f.BaseURL}
	if f.Google != nil {
		cfg.Providers = append(cfg.Providers, Google(f.Google.ClientID, f.Google.ClientSecret))
	}
	if f.GitHub != nil {
		cfg.Providers = append(cfg.Providers, GitHub(f.GitHub.ClientID, f.GitHub.ClientSecret))
	}
	return cfg, nil
}
