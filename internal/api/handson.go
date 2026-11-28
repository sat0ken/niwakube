package api

import (
	"net/http"
	"strings"

	"github.com/sat0ken/niwakube/internal/handson"
)

func (s *Server) handsonEnabled(w http.ResponseWriter) bool {
	if s.opts.Handson == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "the hands-on docs are not enabled"})
		return false
	}
	return true
}

// listHandson はハンズオンの章の一覧 (読み替えの注意つき) を返す。
func (s *Server) listHandson(w http.ResponseWriter, _ *http.Request) {
	if !s.handsonEnabled(w) {
		return
	}
	writeJSON(w, http.StatusOK, handson.Chapters)
}

// handsonPage は章の資料を HTML にして返す (?doc=README_CI.md で章の中のほかのファイル)。
func (s *Server) handsonPage(w http.ResponseWriter, r *http.Request) {
	if !s.handsonEnabled(w) {
		return
	}
	p, err := s.opts.Handson.Page(r.Context(), r.PathValue("id"), r.URL.Query().Get("doc"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handsonFile は資料の画像を返す。SVG もあるので、ページとしては何も動かないようにする。
func (s *Server) handsonFile(w http.ResponseWriter, r *http.Request) {
	if !s.handsonEnabled(w) {
		return
	}
	data, ct, err := s.opts.Handson.File(r.Context(), strings.TrimPrefix(r.PathValue("path"), "/"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}
