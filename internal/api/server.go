// Package api は niwakubed の REST API。
package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/cluster"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
	"github.com/sat0ken/niwakube/internal/terminal"
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
	// Terminal は Web UI のターミナル。nil ならターミナルは使えない (501)
	Terminal *terminal.Manager
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
//	POST   /api/vms/{name}/resize    {"cpus":4,"memoryMiB":8192,"diskGiB":50} 止めて変えて起動し直す (単体の VM)
//	GET    /api/distros
//	GET    /api/cluster-defaults        省略したときの CNI とランタイム
//	GET    /api/clusters
//	POST   /api/clusters
//	GET    /api/clusters/{name}
//	DELETE /api/clusters/{name}
//	POST   /api/clusters/{name}/start
//	POST   /api/clusters/{name}/stop
//	POST   /api/clusters/{name}/resize   {"role":"worker","size":{"cpus":4,"memoryMiB":8192,"diskGiB":50}} 1 台ずつ drain して再起動 (202)
//	POST   /api/clusters/{name}/scale    {"workers":3} ワーカーの台数を変える (減らすときは drain してから消す) (202)
//	GET    /api/clusters/{name}/kubeconfig     ?proxy=socks5://localhost:1080 で proxy-url を入れる
//	GET    /api/kubeconfig?cluster=a&cluster=b  複数のクラスタを 1 つにまとめた kubeconfig (?proxy= も使える)
//	GET    /api/clusters/{name}/terminal       WebSocket。クラスタの kubeconfig を設定したシェル (Web UI のターミナル)
//	GET    /api/clusters/{name}/talosconfig  Talos のみ (?proxy= で proxy-url を入れる)
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
	mux.HandleFunc("POST /api/vms/{name}/resize", s.resizeVM)

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
	mux.HandleFunc("POST /api/clusters/{name}/resize", s.resizeCluster)
	mux.HandleFunc("POST /api/clusters/{name}/scale", s.scaleCluster)
	mux.HandleFunc("GET /api/clusters/{name}/kubeconfig", s.kubeconfig)
	mux.HandleFunc("GET /api/kubeconfig", s.mergedKubeconfig)
	mux.HandleFunc("GET /api/clusters/{name}/terminal", s.terminal)
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

// resizeVM は単体の VM の vCPU・メモリ・ディスクを変える ({"cpus":4,"memoryMiB":8192,"diskGiB":50})。
// 動いていれば止めてから変えて起動し直す (終わるまで待つ)。ディスクは広げるだけ。
func (s *Server) resizeVM(w http.ResponseWriter, r *http.Request) {
	var req machine.ResizeRequest
	if !decode(w, r, &req) {
		return
	}
	vm, err := s.svc.Resize(r.Context(), r.PathValue("name"), req)
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

// resizeCluster はロールのノードの大きさを変える ({"role":"worker","size":{"memoryMiB":8192}})。
// ノードは裏で 1 台ずつ drain して再起動するので、すぐに 202 を返す (進み具合は operation)。
func (s *Server) resizeCluster(w http.ResponseWriter, r *http.Request) {
	var req cluster.ResizeRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.clusters.Resize(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

// scaleCluster はワーカーの台数を変える ({"workers":3})。裏で進むので 202 を返す。
func (s *Server) scaleCluster(w http.ResponseWriter, r *http.Request) {
	var req cluster.ScaleRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.clusters.Scale(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

func (s *Server) stopCluster(w http.ResponseWriter, r *http.Request) {
	c, err := s.clusters.Stop(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// kubeconfig は admin の kubeconfig を返す。?proxy=socks5://localhost:1080 を付けると proxy-url を入れる
// (別の PC から ssh -D 1080 の SOCKS プロキシ経由で kubectl を使うため)。
func (s *Server) kubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	proxy := r.URL.Query().Get("proxy")
	b, err := s.clusters.Kubeconfig(r.Context(), name, cluster.KubeconfigOptions{ProxyURL: proxy})
	if err != nil {
		s.writeError(w, err)
		return
	}
	filename := name + ".kubeconfig"
	if proxy != "" {
		filename = name + "-ssh.kubeconfig"
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(b)
}

// terminal は WebSocket に切り替えて、クラスタの kubeconfig (Talos なら talosconfig も) を設定したシェルとつなぐ。
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	if s.opts.Terminal == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "terminal is not enabled"})
		return
	}
	name := r.PathValue("name")
	kc, err := s.clusters.Kubeconfig(r.Context(), name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	f := terminal.Files{Cluster: name, Kubeconfig: kc}
	if _, tc, err := s.clusters.ClientConfig(r.Context(), name); err == nil {
		f.Talosconfig = tc
	} else if !errors.Is(err, backend.ErrNotSupported) {
		s.writeError(w, err)
		return
	}
	s.opts.Terminal.Serve(w, r, f)
}

// mergedKubeconfig は ?cluster= (複数指定) のクラスタを 1 つにまとめた kubeconfig を返す。
func (s *Server) mergedKubeconfig(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	proxy := q.Get("proxy")
	b, err := s.clusters.KubeconfigFor(r.Context(), q["cluster"], cluster.KubeconfigOptions{ProxyURL: proxy})
	if err != nil {
		s.writeError(w, err)
		return
	}
	filename := "niwakube.kubeconfig"
	if proxy != "" {
		filename = "niwakube-ssh.kubeconfig"
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(b)
}

func (s *Server) clientConfig(w http.ResponseWriter, r *http.Request) {
	filename, b, err := s.clusters.ClientConfig(r.Context(), r.PathValue("name"),
		cluster.KubeconfigOptions{ProxyURL: r.URL.Query().Get("proxy")})
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
		errors.Is(err, machine.ErrPartOfCluster), errors.Is(err, cluster.ErrBusy):
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

// Hijack は WebSocket (ターミナル) のために接続をそのまま渡す。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack is not supported")
	}
	r.code = http.StatusSwitchingProtocols
	return hj.Hijack()
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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
