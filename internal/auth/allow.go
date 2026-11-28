package auth

import (
	"fmt"
	"slices"
	"strings"
)

// Identity はログインした人。
type Identity struct {
	Provider string   `json:"provider"`         // google | github
	Subject  string   `json:"sub"`              // プロバイダーの中の ID
	Login    string   `json:"login,omitempty"`  // GitHub のユーザー名
	Name     string   `json:"name,omitempty"`   // 表示名
	Emails   []string `json:"emails,omitempty"` // 確認済みのメールアドレス (小文字)
	Orgs     []string `json:"orgs,omitempty"`   // GitHub の Organization (許可リストにあるものだけ覚える)
}

// Display は画面に出す名前。
func (id Identity) Display() string {
	switch {
	case id.Login != "":
		return id.Login
	case len(id.Emails) > 0:
		return id.Emails[0]
	default:
		return id.Name
	}
}

// AllowList はログインを許す人の一覧。1 項目は次のどれか。
//
//	alice@example.com   確認済みのメールアドレス (Google でも GitHub でも)
//	@example.com        そのドメインのメールアドレス
//	github:alice        GitHub のユーザー名
//	github-org:myorg    GitHub の Organization のメンバー
type AllowList struct {
	emails, domains, githubUsers, githubOrgs []string
}

// ParseAllowList はカンマか空白で区切った許可リストを読む。
func ParseAllowList(s string) (AllowList, error) {
	var a AllowList
	for _, item := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		item = strings.ToLower(strings.TrimSpace(item))
		switch {
		case strings.HasPrefix(item, "github-org:") && len(item) > len("github-org:"):
			a.githubOrgs = append(a.githubOrgs, strings.TrimPrefix(item, "github-org:"))
		case strings.HasPrefix(item, "github:") && len(item) > len("github:"):
			a.githubUsers = append(a.githubUsers, strings.TrimPrefix(item, "github:"))
		case strings.HasPrefix(item, "@") && len(item) > 1 && !strings.Contains(item[1:], "@"):
			a.domains = append(a.domains, item)
		case strings.Count(item, "@") == 1 && !strings.HasPrefix(item, "@") && !strings.HasSuffix(item, "@"):
			a.emails = append(a.emails, item)
		default:
			return AllowList{}, fmt.Errorf("allow list: %q is not an email, @domain, github:<user> or github-org:<org>", item)
		}
	}
	return a, nil
}

func (a AllowList) Empty() bool {
	return len(a.emails)+len(a.domains)+len(a.githubUsers)+len(a.githubOrgs) == 0
}

// NeedsOrgs は GitHub の Organization を調べる必要があるか (read:org の権限を求める)。
func (a AllowList) NeedsOrgs() bool { return len(a.githubOrgs) > 0 }

// Orgs は許可リストにある Organization のうち、orgs に含まれるもの。
func (a AllowList) Orgs(orgs []string) []string {
	var out []string
	for _, o := range orgs {
		if slices.Contains(a.githubOrgs, strings.ToLower(o)) {
			out = append(out, strings.ToLower(o))
		}
	}
	return out
}

// Allows は id がログインしてよいか。
func (a AllowList) Allows(id Identity) bool {
	for _, e := range id.Emails {
		e = strings.ToLower(e)
		if slices.Contains(a.emails, e) {
			return true
		}
		if at := strings.LastIndex(e, "@"); at >= 0 && slices.Contains(a.domains, e[at:]) {
			return true
		}
	}
	if id.Provider == "github" {
		if id.Login != "" && slices.Contains(a.githubUsers, strings.ToLower(id.Login)) {
			return true
		}
		if len(a.Orgs(id.Orgs)) > 0 {
			return true
		}
	}
	return false
}
