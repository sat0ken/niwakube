package api

import "net/http"

func (s *Server) cacheEnabled(w http.ResponseWriter) bool {
	if s.opts.Cache == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "the registry cache is not enabled"})
		return false
	}
	return true
}

// cacheStats はコンテナイメージのキャッシュの大きさと、使われ方 (niwakubed を起動してから) を返す。
func (s *Server) cacheStats(w http.ResponseWriter, _ *http.Request) {
	if !s.cacheEnabled(w) {
		return
	}
	st, err := s.opts.Cache.Stats()
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// clearCache はキャッシュを空にする (次に取るときは上流から取り直す)。
func (s *Server) clearCache(w http.ResponseWriter, _ *http.Request) {
	if !s.cacheEnabled(w) {
		return
	}
	if err := s.opts.Cache.Clear(); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pruneCache は自動の削除をいま動かし、結果を返す。
func (s *Server) pruneCache(w http.ResponseWriter, _ *http.Request) {
	if !s.cacheEnabled(w) {
		return
	}
	res, err := s.opts.Cache.Prune()
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
