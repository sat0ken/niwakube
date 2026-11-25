// Package webui は niwakubed に埋め込む Web UI (素の HTML / CSS / JavaScript。ビルドの手順は無い)。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// Handler は Web UI を返す。/ は index.html、/ui/ 以下は静的ファイル。/api/ は扱わない (404)。
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/ui/"):
			w.Header().Set("Cache-Control", "no-cache")
			http.StripPrefix("/ui/", files).ServeHTTP(w, r)
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			b, err := static.ReadFile("static/index.html")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			// 外部のスクリプトやフレームへの埋め込みを禁止する
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	})
}
