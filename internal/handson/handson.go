package handson

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
)

const (
	// DefaultBase は資料を取る場所 (GitHub の raw)。
	DefaultBase = "https://raw.githubusercontent.com/cloudnativedaysjp/cnd-handson/main/"
	// RepoURL は資料のリポジトリ (Markdown 以外へのリンクの行き先)。
	RepoURL = "https://github.com/cloudnativedaysjp/cnd-handson"

	cacheTTL = time.Hour
	maxSize  = 8 << 20
)

// ErrNotFound は無い章・ファイル。
var ErrNotFound = errors.New("not found")

// Service は資料を取ってきて HTML にする。取ったものは cacheTTL のあいだ覚え、GitHub に届かなければ古いものを返す。
type Service struct {
	Base   string // 末尾は /
	Client *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	data []byte
	ct   string
	at   time.Time
}

func New() *Service {
	return &Service{Base: DefaultBase, Client: &http.Client{Timeout: 30 * time.Second}, cache: map[string]cached{}}
}

// Page は 1 つの資料。
type Page struct {
	Chapter Chapter `json:"chapter"`
	File    string  `json:"file"`
	HTML    string  `json:"html"`
	Source  string  `json:"source"` // GitHub で見る URL
}

// Page は章 id の file を HTML にして返す。
func (s *Service) Page(ctx context.Context, id, file string) (*Page, error) {
	c, ok := find(id)
	if !ok {
		return nil, fmt.Errorf("chapter %q: %w", id, ErrNotFound)
	}
	if file == "" {
		file = c.Docs[0].File
	}
	if !c.hasDoc(file) {
		return nil, fmt.Errorf("%s/%s: %w", id, file, ErrNotFound)
	}
	p := id + "/" + file
	md, _, err := s.fetch(ctx, p)
	if err != nil {
		return nil, err
	}
	out, err := Render(md, p)
	if err != nil {
		return nil, err
	}
	return &Page{Chapter: *c, File: file, HTML: out, Source: RepoURL + "/blob/main/" + p}, nil
}

var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".svg": "image/svg+xml", ".webp": "image/webp",
}

var repoPathRe = regexp.MustCompile(`^chapter_[a-z0-9-]+/[A-Za-z0-9._/-]+$`)

// File は資料の中の画像を返す (章のディレクトリの中の画像だけ)。
func (s *Service) File(ctx context.Context, p string) ([]byte, string, error) {
	ct, ok := imageTypes[strings.ToLower(path.Ext(p))]
	if !ok || !repoPathRe.MatchString(p) || strings.Contains(p, "..") {
		return nil, "", fmt.Errorf("%s: %w", p, ErrNotFound)
	}
	data, _, err := s.fetch(ctx, p)
	return data, ct, err
}

func (s *Service) fetch(ctx context.Context, p string) ([]byte, string, error) {
	s.mu.Lock()
	c, ok := s.cache[p]
	s.mu.Unlock()
	if ok && time.Since(c.at) < cacheTTL {
		return c.data, c.ct, nil
	}
	data, ct, err := s.get(ctx, p)
	if err != nil {
		if ok && !errors.Is(err, ErrNotFound) {
			return c.data, c.ct, nil // 届かなければ古いもの
		}
		return nil, "", err
	}
	s.mu.Lock()
	s.cache[p] = cached{data: data, ct: ct, at: time.Now()}
	s.mu.Unlock()
	return data, ct, nil
}

func (s *Service) get(ctx context.Context, p string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+p, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("%s: %w", p, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch %s: HTTP %d", p, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxSize {
		return nil, "", fmt.Errorf("%s is too large", p)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// --- Markdown → HTML ---

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	// README には <img> や <details> がある。そのまま出し、あとの sanitize で決まったタグと属性だけ残す
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
)

// Render は repo の中の docPath (例: chapter_cilium/README.md) の Markdown を、Web UI に出せる HTML にする。
//   - タグと属性は決まったものだけ残す (script などは中身ごと消す)
//   - 見出しの id は GitHub と同じ規則で作り、md- を付ける (Web UI の要素とぶつからないように)。#… のリンクも合わせる
//   - 画像は /api/handson/files/ を通す (外部の画像はリンクにする)。ほかの章の Markdown へのリンクは #doc:<path>、
//     それ以外のファイルへのリンクは GitHub に向ける
func Render(src []byte, docPath string) (string, error) {
	ctx := parser.NewContext(parser.WithIDs(newIDs()))
	var buf bytes.Buffer
	if err := md.Convert(src, &buf, parser.WithContext(ctx)); err != nil {
		return "", err
	}
	return sanitize(buf.String(), path.Dir(docPath)), nil
}

// ids は GitHub と同じ見出しの id (小文字、記号を消し、空白を - に、重複は -1, -2…)。
type ids struct{ seen map[string]int }

func newIDs() *ids { return &ids{seen: map[string]int{}} }

func (x *ids) Generate(value []byte, _ ast.NodeKind) []byte {
	id := slug(string(value))
	n := x.seen[id]
	x.seen[id]++
	if n > 0 {
		id = fmt.Sprintf("%s-%d", id, n)
	}
	return []byte("md-" + id)
}

func (x *ids) Put(value []byte) { x.seen[strings.TrimPrefix(string(value), "md-")]++ }

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r < 128 && !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9'):
			// ASCII の記号は消す
		case strings.ContainsRune("、。「」『』（）［］【】・：；！？　", r):
			// 全角の記号も消す (GitHub と同じ)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

var allowed = map[string][]string{
	"h1": {"id"}, "h2": {"id"}, "h3": {"id"}, "h4": {"id"}, "h5": {"id"}, "h6": {"id"},
	"p": nil, "br": nil, "hr": nil, "blockquote": nil, "pre": nil, "code": {"class"},
	"ul": nil, "ol": {"start"}, "li": nil, "strong": nil, "em": nil, "del": nil, "b": nil, "i": nil,
	"a": {"href"}, "img": {"src", "alt", "width", "height"},
	"table": nil, "thead": nil, "tbody": nil, "tr": nil, "th": nil, "td": nil,
	"details": nil, "summary": nil, "kbd": nil, "sup": nil, "sub": nil, "input": {"type", "checked", "disabled"},
}

// sanitize は決まったタグと属性だけを残し、リンクと画像の URL を書き換える。
func sanitize(in, dir string) string {
	z := html.NewTokenizer(strings.NewReader(in))
	var b strings.Builder
	skip := 0 // script / style の中
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return b.String()
		case html.TextToken:
			if skip == 0 {
				b.WriteString(html.EscapeString(string(z.Text())))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data == "script" || t.Data == "style" || t.Data == "iframe" {
				if tt == html.StartTagToken {
					skip++
				}
				continue
			}
			if skip > 0 {
				continue
			}
			writeTag(&b, t, dir)
		case html.EndTagToken:
			t := z.Token()
			if t.Data == "script" || t.Data == "style" || t.Data == "iframe" {
				if skip > 0 {
					skip--
				}
				continue
			}
			if _, ok := allowed[t.Data]; ok && skip == 0 && t.Data != "img" && t.Data != "br" && t.Data != "hr" && t.Data != "input" {
				b.WriteString("</" + t.Data + ">")
			}
		}
	}
}

func writeTag(b *strings.Builder, t html.Token, dir string) {
	attrs, ok := allowed[t.Data]
	if !ok {
		return
	}
	get := func(k string) (string, bool) {
		for _, a := range t.Attr {
			if a.Key == k {
				return a.Val, true
			}
		}
		return "", false
	}
	switch t.Data {
	case "img":
		src, _ := get("src")
		alt, _ := get("alt")
		u := imageURL(src, dir)
		if u == "" {
			// 外部の画像は読み込まない (CSP)。リンクにする
			if ext := externalURL(src); ext != "" {
				fmt.Fprintf(b, `<a href="%s" target="_blank" rel="noopener">[画像: %s]</a>`, html.EscapeString(ext), html.EscapeString(alt))
			}
			return
		}
		fmt.Fprintf(b, `<img src="%s" alt="%s" loading="lazy"`, html.EscapeString(u), html.EscapeString(alt))
		for _, k := range []string{"width", "height"} {
			if v, ok := get(k); ok && isNumber(v) {
				fmt.Fprintf(b, ` %s="%s"`, k, v)
			}
		}
		b.WriteString(">")
		return
	case "a":
		href, _ := get("href")
		u, external := linkURL(href, dir)
		b.WriteString("<a")
		if u != "" {
			fmt.Fprintf(b, ` href="%s"`, html.EscapeString(u))
		}
		if external {
			b.WriteString(` target="_blank" rel="noopener"`)
		}
		b.WriteString(">")
		return
	case "input":
		// タスクリストのチェックボックスだけ
		if v, _ := get("type"); v != "checkbox" {
			return
		}
		b.WriteString(`<input type="checkbox" disabled`)
		if _, ok := get("checked"); ok {
			b.WriteString(" checked")
		}
		b.WriteString(">")
		return
	}
	b.WriteString("<" + t.Data)
	for _, k := range attrs {
		v, ok := get(k)
		if !ok {
			continue
		}
		switch k {
		case "class":
			if !strings.HasPrefix(v, "language-") {
				continue
			}
		case "id":
			if !strings.HasPrefix(v, "md-") {
				continue
			}
		case "start":
			if !isNumber(v) {
				continue
			}
		}
		fmt.Fprintf(b, ` %s="%s"`, k, html.EscapeString(v))
	}
	b.WriteString(">")
}

func isNumber(s string) bool {
	if s == "" || len(s) > 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// externalURL は http(s) の URL ならそのまま返す。
func externalURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// resolve は dir からの相対パスを、リポジトリの中のパスにする (外に出れば "")。
func resolve(rel, dir string) string {
	if rel == "" || strings.Contains(rel, ":") || strings.HasPrefix(rel, "/") {
		return ""
	}
	p := path.Clean(path.Join(dir, rel))
	if p == "." || strings.HasPrefix(p, "..") {
		return ""
	}
	return p
}

func imageURL(src, dir string) string {
	u, err := url.Parse(strings.TrimSpace(src))
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	p := resolve(u.Path, dir)
	if _, ok := imageTypes[strings.ToLower(path.Ext(p))]; !ok || !repoPathRe.MatchString(p) {
		return ""
	}
	return "/api/handson/files/" + p
}

var chapterDirRe = regexp.MustCompile(`^chapter_[a-z0-9-]+$`)

// linkURL はリンクの行き先を返す。2 つ目は別のタブで開くか。
func linkURL(href, dir string) (string, bool) {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "#") {
		frag, err := url.PathUnescape(href[1:])
		if err != nil {
			frag = href[1:]
		}
		return "#md-" + strings.ToLower(frag), false
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	switch u.Scheme {
	case "http", "https":
		return u.String(), true
	case "mailto":
		return u.String(), false
	case "":
	default:
		return "", false // javascript: など
	}
	if u.Host != "" {
		return "", false
	}
	p := resolve(u.Path, dir)
	if p == "" {
		return "", false
	}
	if chapterDirRe.MatchString(p) {
		p += "/README.md"
	}
	if strings.HasSuffix(p, ".md") && strings.HasPrefix(p, "chapter_") {
		if c, ok := find(path.Dir(p)); ok && c.hasDoc(path.Base(p)) {
			return "#doc:" + p, false
		}
	}
	return RepoURL + "/blob/main/" + p, true
}
