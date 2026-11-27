package api

import (
	"net/http"

	"github.com/sat0ken/niwakube/internal/cluster"
)

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.clusters.ListSnapshots(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

// createSnapshot はスナップショットを取り始める ({"name":"before-upgrade"}、名前は省略できる)。裏で進むので 202。
func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req cluster.SnapshotRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	c, err := s.clusters.CreateSnapshot(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

// restoreSnapshot はスナップショットの状態に戻し始める。裏で進むので 202。
func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	c, err := s.clusters.RestoreSnapshot(r.Context(), r.PathValue("name"), r.PathValue("snap"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.clusters.DeleteSnapshot(r.Context(), r.PathValue("name"), r.PathValue("snap")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
