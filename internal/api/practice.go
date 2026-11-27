package api

import (
	"fmt"
	"net/http"

	"github.com/sat0ken/niwakube/internal/machine"
)

// practiceRequest は練習問題を出す / 採点するクラスタ。
type practiceRequest struct {
	Cluster string `json:"cluster"`
}

func (s *Server) practiceEnabled(w http.ResponseWriter) bool {
	if s.opts.Practice == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "practice problems are not available"})
		return false
	}
	return true
}

func (s *Server) listProblems(w http.ResponseWriter, _ *http.Request) {
	if !s.practiceEnabled(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Practice.List())
}

// getProblem は問題文・ヒント・解説・解答を返す (Web UI は解説を押したときだけ見せる)。
func (s *Server) getProblem(w http.ResponseWriter, r *http.Request) {
	if !s.practiceEnabled(w) {
		return
	}
	p, err := s.opts.Practice.Get(r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) practiceCluster(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req practiceRequest
	if !decode(w, r, &req) {
		return "", false
	}
	if req.Cluster == "" {
		s.writeError(w, fmt.Errorf("cluster is required: %w", machine.ErrInvalid))
		return "", false
	}
	return req.Cluster, true
}

// setupProblem は問題の Namespace を作り直して、出題直後の状態にする ({"cluster":"demo"})。
func (s *Server) setupProblem(w http.ResponseWriter, r *http.Request) {
	if !s.practiceEnabled(w) {
		return
	}
	cluster, ok := s.practiceCluster(w, r)
	if !ok {
		return
	}
	if err := s.opts.Practice.Setup(r.Context(), cluster, r.PathValue("id")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkProblem はクラスタの今の状態で採点する ({"cluster":"demo"})。
func (s *Server) checkProblem(w http.ResponseWriter, r *http.Request) {
	if !s.practiceEnabled(w) {
		return
	}
	cluster, ok := s.practiceCluster(w, r)
	if !ok {
		return
	}
	res, err := s.opts.Practice.Check(r.Context(), cluster, r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// solveProblem は模範解答をクラスタに当てる ({"cluster":"demo"})。CI と、解説を読んだあとの確認用。
func (s *Server) solveProblem(w http.ResponseWriter, r *http.Request) {
	if !s.practiceEnabled(w) {
		return
	}
	cluster, ok := s.practiceCluster(w, r)
	if !ok {
		return
	}
	if err := s.opts.Practice.Solve(r.Context(), cluster, r.PathValue("id")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
