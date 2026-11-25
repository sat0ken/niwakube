// Package api は niwakubed の REST API。
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/cluster"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

type Server struct {
	svc      *machine.Service
	clusters *cluster.Service
	log      *slog.Logger
	opts     Options
}

// Options は API サーバーの設定。
type Options struct {
	Version string // niwakubed のバージョン (表示用)
	DataDir string // ディスクの空きを表示するディレクトリ
	// LoopbackOnly は localhost だけで待ち受けているとき true。Host ヘッダーが localhost 以外なら拒否する
	LoopbackOnly bool
	// UI は Web UI のハンドラー。nil なら Web UI は出さない
	UI http.Handler
}

func New(svc *machine.Service, clusters *cluster.Service, log *slog.Logger, opts ...Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{svc: svc, clusters: clusters, log: log}
	if len(opts) > 0 {
		s.opts = opts[0]
	}
	return s
}

// Handler は API のルーティングを返す。
//
//	GET    /                         Web UI
//	GET    /healthz
//	GET    /api/host                 ホストの CPU / メモリ / ディスクと、VM に割り当てたリソース
//	GET    /api/backends
//	GET    /api/vms
//	POST   /api/vms
//	GET    /api/vms/{name}
//	DELETE /api/vms/{name}
//	POST   /api/vms/{name}/start
//	POST   /api/vms/{name}/stop      ?force=true で強制停止
//	GET    /api/vms/{name}/serial    ?bytes=N でシリアルログの末尾 N バイト (既定 64KiB)
//	GET    /api/distros
//	GET    /api/cluster-defaults        省略したときの CNI とランタイム
//	GET    /api/clusters
//	POST   /api/clusters
//	GET    /api/clusters/{name}
//	DELETE /api/clusters/{name}
//	POST   /api/clusters/{name}/start
//	POST   /api/clusters/{name}/stop
//	GET    /api/clusters/{name}/kubeconfig
//	GET    /api/clusters/{name}/talosconfig  Talos のみ
//	GET    /api/clusters/{name}/forwards     ホストのポートからクラスタへの転送の一覧
//	POST   /api/clusters/{name}/forwards     {"hostPort":8080,"ip":"172.30.1.100","port":80}
//	DELETE /api/clusters/{name}/forwards/{proto}/{port}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/host", s.hostInfo)
	mux.HandleFunc("GET /api/backends", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.svc.Backends())
	})
	mux.HandleFunc("GET /api/vms", s.listVMs)
	mux.HandleFunc("POST /api/vms", s.createVM)
	mux.HandleFunc("GET /api/vms/{name}", s.getVM)
	mux.HandleFunc("DELETE /api/vms/{name}", s.deleteVM)
	mux.HandleFunc("POST /api/vms/{name}/start", s.startVM)
	mux.HandleFunc("POST /api/vms/{name}/stop", s.stopVM)
	mux.HandleFunc("GET /api/vms/{name}/serial", s.serialLog)

	mux.HandleFunc("GET /api/distros", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.clusters.Distros())
	})
	mux.HandleFunc("GET /api/cluster-defaults", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.clusters.Defaults())
	})
	mux.HandleFunc("GET /api/clusters", s.listClusters)
	mux.HandleFunc("POST /api/clusters", s.createCluster)
	mux.HandleFunc("GET /api/clusters/{name}", s.getCluster)
	mux.HandleFunc("DELETE /api/clusters/{name}", s.deleteCluster)
	mux.HandleFunc("POST /api/clusters/{name}/start", s.startCluster)
	mux.HandleFunc("POST /api/clusters/{name}/stop", s.stopCluster)
	mux.HandleFunc("GET /api/clusters/{name}/kubeconfig", s.kubeconfig)
	mux.HandleFunc("GET /api/clusters/{name}/talosconfig", s.clientConfig)
	mux.HandleFunc("GET /api/clusters/{name}/forwards", s.listForwards)
	mux.HandleFunc("POST /api/clusters/{name}/forwards", s.addForward)
	mux.HandleFunc("DELETE /api/clusters/{name}/forwards/{proto}/{port}", s.deleteForward)
	if s.opts.UI != nil {
		mux.Handle("GET /", s.opts.UI)
	}
	return s.logging(protect(mux, s.opts.LoopbackOnly))
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.svc.List(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req machine.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	vm, err := s.svc.Create(r.Context(), req)
	if err != nil {
		if vm != nil {
			// 作成はできたが起動に失敗した
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error(), VM: vm})
			return
		}
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, vm)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	vm, err := s.svc.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startVM(w http.ResponseWriter, r *http.Request) {
	vm, err := s.svc.Start(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) {
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	vm, err := s.svc.Stop(r.Context(), r.PathValue("name"), force)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (s *Server) serialLog(w http.ResponseWriter, r *http.Request) {
	n := int64(64 << 10)
	if q := r.URL.Query().Get("bytes"); q != "" {
		v, err := strconv.ParseInt(q, 10, 64)
		if err != nil || v <= 0 || v > 16<<20 {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "bytes must be between 1 and 16777216"})
			return
		}
		n = v
	}
	b, err := s.svc.SerialLog(r.Context(), r.PathValue("name"), n)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	cs, err := s.clusters.List(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	var req cluster.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.clusters.Create(r.Context(), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	c, err := s.clusters.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.clusters.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startCluster(w http.ResponseWriter, r *http.Request) {
	c, err := s.clusters.Start(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) stopCluster(w http.ResponseWriter, r *http.Request) {
	c, err := s.clusters.Stop(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) kubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, err := s.clusters.Kubeconfig(r.Context(), name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.kubeconfig"`)
	_, _ = w.Write(b)
}

func (s *Server) clientConfig(w http.ResponseWriter, r *http.Request) {
	filename, b, err := s.clusters.ClientConfig(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(b)
}

func (s *Server) listForwards(w http.ResponseWriter, r *http.Request) {
	fs, err := s.clusters.ListForwards(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fs)
}

func (s *Server) addForward(w http.ResponseWriter, r *http.Request) {
	var req cluster.ForwardRequest
	if !decode(w, r, &req) {
		return
	}
	f, err := s.clusters.AddForward(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) deleteForward(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid port"})
		return
	}
	if err := s.clusters.DeleteForward(r.Context(), r.PathValue("name"), r.PathValue("proto"), port); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type errorBody struct {
	Error string           `json:"error"`
	VM    *machine.Machine `json:"vm,omitempty"`
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, machine.ErrInvalid), errors.Is(err, backend.ErrImageNotFound):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound), errors.Is(err, backend.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrExists), errors.Is(err, backend.ErrAlreadyExists), errors.Is(err, ipam.ErrExhausted),
		errors.Is(err, machine.ErrPartOfCluster):
		return http.StatusConflict
	case errors.Is(err, backend.ErrNotSupported):
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	code := statusFor(err)
	if code >= 500 {
		s.log.Error("request failed", "err", err)
	}
	writeJSON(w, code, errorBody{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "duration", time.Since(start).Round(time.Millisecond))
	})
}
