// Package regcache はコンテナイメージのプルスルーキャッシュ (レジストリのミラー)。
//
// クラスタのノードの containerd は、レジストリ (docker.io、registry.k8s.io など) のミラーとしてこのキャッシュを使う。
// containerd はミラーに問い合わせるとき、元のレジストリを ?ns=registry.k8s.io で付けてくるので、1 つのポートで
// 複数のレジストリをまとめて受けられる。
//
//   - レイヤー (blob) とマニフェストは中身のダイジェストで保存する (変わらないので、一度取れば上流に聞かない)
//   - タグ → ダイジェストは TagTTL のあいだ使い回し、過ぎたら上流に HEAD で確かめる
//     (Docker Hub は HEAD を取得回数の制限に数えない)。上流に届かなければ、保存してあるものを返す
//   - 同じレイヤーを複数のノードが同時に取りにきても、上流からは 1 回だけ取る
//
// キャッシュが動いていなくても、containerd は元のレジストリから取る (ミラーは「まず試す先」)。
package regcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// DefaultUpstreams はミラーするレジストリ。
var DefaultUpstreams = []string{"docker.io", "registry.k8s.io", "ghcr.io", "quay.io", "gcr.io"}

// Config はキャッシュの設定。
type Config struct {
	Dir       string        // 保存先
	Upstreams []string      // 取りにいってよいレジストリ (ns の値)。空なら DefaultUpstreams
	TagTTL    time.Duration // タグを上流に確かめずに使う時間。0 なら 10 分
	// Insecure は HTTP で取りにいくレジストリ (テスト用)
	Insecure []string
	Log      *slog.Logger
	// 自動の削除 (prune.go)。0 ならその決まりは使わない
	MaxBytes     int64         // 合計の上限
	MaxAge       time.Duration // これより長く使われていないものは消す
	MinFreeRatio float64       // ファイルシステムの空きがこの割合を切ったら消す (0.1 = 10%)
}

// Cache はプルスルーキャッシュ。http.Handler として /v2/ を受ける。
type Cache struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	inflight map[string]*flight // 上流から取っている途中のレイヤー (ダイジェストごと)
	trans    map[string]cachedTransport

	pruneMu   sync.Mutex // Prune と Clear を同時に動かさない
	statsMu   sync.Mutex
	lastPrune *PruneResult
	diskUsage func(path string) (free, total uint64, err error)

	stats struct {
		manifestHits, manifestMisses atomic.Int64
		blobHits, blobMisses         atomic.Int64
		bytesFromCache, bytesFromUp  atomic.Int64
		staleServed                  atomic.Int64
	}
}

type flight struct {
	done chan struct{}
	err  error
}

type cachedTransport struct {
	rt      http.RoundTripper
	created time.Time
}

var (
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// New はキャッシュを作る。
func New(cfg Config) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, errors.New("regcache: Dir is required")
	}
	if len(cfg.Upstreams) == 0 {
		cfg.Upstreams = DefaultUpstreams
	}
	if cfg.TagTTL == 0 {
		cfg.TagTTL = 10 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, d := range []string{"blobs", "manifests", "tags", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, d), 0o750); err != nil {
			return nil, err
		}
	}
	return &Cache{cfg: cfg, log: cfg.Log, inflight: map[string]*flight{}, trans: map[string]cachedTransport{}, diskUsage: diskUsage}, nil
}

// registryError はレジストリの API の形のエラーを返す。404 なら containerd は元のレジストリに取りにいく。
func registryError(w http.ResponseWriter, code int, errCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": errCode, "message": msg}}})
}

func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		registryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "this mirror is read-only")
		return
	}
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	path := r.URL.Path
	if path == "/v2" || path == "/v2/" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		ns = "docker.io"
	}
	if !slices.Contains(c.cfg.Upstreams, ns) {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN", "registry "+ns+" is not mirrored")
		return
	}
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN", "not found")
		return
	}
	var kind, repo, ref string
	if i := strings.LastIndex(rest, "/manifests/"); i > 0 {
		kind, repo, ref = "manifests", rest[:i], rest[i+len("/manifests/"):]
	} else if i := strings.LastIndex(rest, "/blobs/"); i > 0 {
		kind, repo, ref = "blobs", rest[:i], rest[i+len("/blobs/"):]
	} else {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN", "not found")
		return
	}
	repository, err := name.NewRepository(ns+"/"+repo, c.nameOpts(ns)...)
	if err != nil {
		registryError(w, http.StatusBadRequest, "NAME_INVALID", err.Error())
		return
	}
	if kind == "blobs" {
		if !digestRe.MatchString(ref) {
			registryError(w, http.StatusBadRequest, "DIGEST_INVALID", "bad digest")
			return
		}
		c.serveBlob(w, r, repository, ref)
		return
	}
	c.serveManifest(w, r, repository, ref)
}

func (c *Cache) nameOpts(ns string) []name.Option {
	if slices.Contains(c.cfg.Insecure, ns) {
		return []name.Option{name.Insecure}
	}
	return nil
}

func hexOf(digest string) string { return strings.TrimPrefix(digest, "sha256:") }

func (c *Cache) blobPath(digest string) string {
	return filepath.Join(c.cfg.Dir, "blobs", hexOf(digest))
}

func (c *Cache) manifestPath(digest string) string {
	return filepath.Join(c.cfg.Dir, "manifests", hexOf(digest))
}

// tagPath はタグの記録の場所。リポジトリ名をそのままパスにしないよう、ハッシュにする。
func (c *Cache) tagPath(repo name.Repository, tag string) string {
	h := sha256.Sum256([]byte(repo.Name() + ":" + tag))
	return filepath.Join(c.cfg.Dir, "tags", hex.EncodeToString(h[:])+".json")
}

type tagEntry struct {
	Repo      string    `json:"repo"`
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	MediaType string    `json:"mediaType"`
	Checked   time.Time `json:"checked"`
}

type manifestMeta struct {
	MediaType string `json:"mediaType"`
}

// writeAtomic は一時ファイルに書いてから名前を変える (読みかけのファイルを見せない)。
func (c *Cache) writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Join(c.cfg.Dir, "tmp"), "w-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

func (c *Cache) loadManifest(digest string) ([]byte, string, bool) {
	body, err := os.ReadFile(c.manifestPath(digest))
	if err != nil {
		return nil, "", false
	}
	var meta manifestMeta
	raw, err := os.ReadFile(c.manifestPath(digest) + ".json")
	if err != nil || json.Unmarshal(raw, &meta) != nil {
		return nil, "", false
	}
	touch(c.manifestPath(digest)) // 使った時刻 (自動の削除で、使われていない順に消すため)
	return body, meta.MediaType, true
}

func (c *Cache) storeManifest(digest, mediaType string, body []byte) error {
	sum := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("manifest digest mismatch: want %s", digest)
	}
	meta, _ := json.Marshal(manifestMeta{MediaType: mediaType})
	if err := c.writeAtomic(c.manifestPath(digest)+".json", meta); err != nil {
		return err
	}
	return c.writeAtomic(c.manifestPath(digest), body)
}

func writeManifest(w http.ResponseWriter, r *http.Request, digest, mediaType string, body []byte) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (c *Cache) remoteOpts(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous)}
}

func upstreamStatus(err error) int {
	var te *transport.Error
	if errors.As(err, &te) && te.StatusCode == http.StatusNotFound {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

func (c *Cache) serveManifest(w http.ResponseWriter, r *http.Request, repo name.Repository, ref string) {
	ctx := r.Context()
	if strings.HasPrefix(ref, "sha256:") {
		if !digestRe.MatchString(ref) {
			registryError(w, http.StatusBadRequest, "DIGEST_INVALID", "bad digest")
			return
		}
		if body, mt, ok := c.loadManifest(ref); ok {
			c.stats.manifestHits.Add(1)
			writeManifest(w, r, ref, mt, body)
			return
		}
		c.stats.manifestMisses.Add(1)
		desc, err := remote.Get(repo.Digest(ref), c.remoteOpts(ctx)...)
		if err != nil {
			c.log.Warn("registry cache: fetch manifest", "ref", repo.Name()+"@"+ref, "err", err)
			registryError(w, upstreamStatus(err), "MANIFEST_UNKNOWN", err.Error())
			return
		}
		if err := c.storeManifest(ref, string(desc.MediaType), desc.Manifest); err != nil {
			registryError(w, http.StatusBadGateway, "MANIFEST_INVALID", err.Error())
			return
		}
		writeManifest(w, r, ref, string(desc.MediaType), desc.Manifest)
		return
	}

	if !tagRe.MatchString(ref) {
		registryError(w, http.StatusBadRequest, "TAG_INVALID", "bad tag")
		return
	}
	tag := repo.Tag(ref)
	var entry tagEntry
	cached := false
	if raw, err := os.ReadFile(c.tagPath(repo, ref)); err == nil && json.Unmarshal(raw, &entry) == nil {
		cached = true
	}
	if cached && time.Since(entry.Checked) < c.cfg.TagTTL {
		if body, mt, ok := c.loadManifest(entry.Digest); ok {
			c.stats.manifestHits.Add(1)
			writeManifest(w, r, entry.Digest, mt, body)
			return
		}
	}
	// 期限切れ: HEAD でダイジェストだけ確かめる (変わっていなければ取り直さない)
	if cached {
		if head, err := remote.Head(tag, c.remoteOpts(ctx)...); err == nil && head.Digest.String() == entry.Digest {
			if body, mt, ok := c.loadManifest(entry.Digest); ok {
				entry.Checked = time.Now()
				c.saveTag(repo, ref, entry)
				c.stats.manifestHits.Add(1)
				writeManifest(w, r, entry.Digest, mt, body)
				return
			}
		}
	}
	c.stats.manifestMisses.Add(1)
	desc, err := remote.Get(tag, c.remoteOpts(ctx)...)
	if err != nil {
		// 上流に届かない (オフライン、Docker Hub の制限など): 保存してあるものを返す
		if cached {
			if body, mt, ok := c.loadManifest(entry.Digest); ok {
				c.stats.staleServed.Add(1)
				c.log.Warn("registry cache: upstream failed, serving cached tag", "tag", tag.Name(), "err", err)
				writeManifest(w, r, entry.Digest, mt, body)
				return
			}
		}
		c.log.Warn("registry cache: fetch manifest", "tag", tag.Name(), "err", err)
		registryError(w, upstreamStatus(err), "MANIFEST_UNKNOWN", err.Error())
		return
	}
	digest := desc.Digest.String()
	if err := c.storeManifest(digest, string(desc.MediaType), desc.Manifest); err != nil {
		registryError(w, http.StatusBadGateway, "MANIFEST_INVALID", err.Error())
		return
	}
	c.saveTag(repo, ref, tagEntry{Repo: repo.Name(), Tag: ref, Digest: digest, MediaType: string(desc.MediaType), Checked: time.Now()})
	writeManifest(w, r, digest, string(desc.MediaType), desc.Manifest)
}

func (c *Cache) saveTag(repo name.Repository, tag string, e tagEntry) {
	raw, _ := json.Marshal(e)
	if err := c.writeAtomic(c.tagPath(repo, tag), raw); err != nil {
		c.log.Warn("registry cache: save tag", "err", err)
	}
}

// transport は上流のレジストリへの、読み取りの認証つきの http.RoundTripper (トークンは数分使い回す)。
func (c *Cache) transport(ctx context.Context, repo name.Repository) (http.RoundTripper, error) {
	key := repo.Name()
	c.mu.Lock()
	t, ok := c.trans[key]
	c.mu.Unlock()
	if ok && time.Since(t.created) < 4*time.Minute {
		return t.rt, nil
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, authn.Anonymous, http.DefaultTransport, []string{repo.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.trans[key] = cachedTransport{rt: rt, created: time.Now()}
	c.mu.Unlock()
	return rt, nil
}

func (c *Cache) serveBlob(w http.ResponseWriter, r *http.Request, repo name.Repository, digest string) {
	if c.serveBlobFile(w, r, digest) {
		c.stats.blobHits.Add(1)
		return
	}
	// 取っている途中なら、それを待って保存したものを返す
	c.mu.Lock()
	if f, ok := c.inflight[digest]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
		case <-r.Context().Done():
			return
		}
		if f.err == nil && c.serveBlobFile(w, r, digest) {
			c.stats.blobHits.Add(1)
			return
		}
		registryError(w, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream fetch failed")
		return
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[digest] = f
	c.mu.Unlock()
	c.stats.blobMisses.Add(1)
	f.err = c.fetchBlob(w, r, repo, digest)
	c.mu.Lock()
	delete(c.inflight, digest)
	c.mu.Unlock()
	close(f.done)
	if f.err != nil {
		c.log.Warn("registry cache: fetch blob", "repo", repo.Name(), "digest", digest, "err", f.err)
	}
}

func (c *Cache) serveBlobFile(w http.ResponseWriter, r *http.Request, digest string) bool {
	f, err := os.Open(c.blobPath(digest))
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method != http.MethodHead {
		c.stats.bytesFromCache.Add(fi.Size())
	}
	modTime := fi.ModTime()
	touch(c.blobPath(digest))               // 使った時刻
	http.ServeContent(w, r, "", modTime, f) // Range と HEAD も扱う
	return true
}

// fetchBlob は上流から取りながらクライアントに流し、同時にファイルに保存する。ダイジェストが合えばキャッシュに入れる。
// クライアントが途中で切れても、取り終えてキャッシュに入れる (ほかのノードが続けて取りにくるので)。
func (c *Cache) fetchBlob(w http.ResponseWriter, r *http.Request, repo name.Repository, digest string) error {
	// クライアントが切れても続けるので、リクエストの context は使わない
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rt, err := c.transport(ctx, repo)
	if err != nil {
		registryError(w, http.StatusBadGateway, "BLOB_UNKNOWN", err.Error())
		return err
	}
	scheme := repo.Registry.Scheme()
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, repo.RegistryStr(), repo.RepositoryStr(), digest)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		registryError(w, http.StatusBadGateway, "BLOB_UNKNOWN", err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		code := http.StatusBadGateway
		if resp.StatusCode == http.StatusNotFound {
			code = http.StatusNotFound
		}
		registryError(w, code, "BLOB_UNKNOWN", "upstream: "+resp.Status)
		return fmt.Errorf("upstream: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Join(c.cfg.Dir, "tmp"), "blob-*")
	if err != nil {
		registryError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return err
	}
	defer os.Remove(tmp.Name())
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprint(resp.ContentLength))
	}
	w.WriteHeader(http.StatusOK)
	h := sha256.New()
	client := &lenientWriter{w: w, skip: r.Method == http.MethodHead}
	n, err := io.Copy(io.MultiWriter(tmp, h, client), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	c.stats.bytesFromUp.Add(n)
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("blob digest mismatch: got %s", got)
	}
	return os.Rename(tmp.Name(), c.blobPath(digest))
}

// lenientWriter はクライアントへの書き込みに失敗しても止まらない (キャッシュへの保存は続ける)。
type lenientWriter struct {
	w    io.Writer
	skip bool
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if !l.skip {
		if _, err := l.w.Write(p); err != nil {
			l.skip = true
		}
	}
	return len(p), nil
}

// Stats はキャッシュの使われ方と大きさ。
type Stats struct {
	Blobs          int   `json:"blobs"`
	Manifests      int   `json:"manifests"`
	Bytes          int64 `json:"bytes"` // 保存しているレイヤーとマニフェストの合計
	BlobHits       int64 `json:"blobHits"`
	BlobMisses     int64 `json:"blobMisses"`
	ManifestHits   int64 `json:"manifestHits"`
	ManifestMisses int64 `json:"manifestMisses"`
	BytesFromCache int64 `json:"bytesFromCache"` // キャッシュから返したレイヤーの量 (niwakubed を起動してから)
	BytesFromUp    int64 `json:"bytesFromUpstream"`
	StaleServed    int64 `json:"staleServed"` // 上流に届かず、保存してあったタグを返した回数
	// 自動の削除の設定と、前回の結果
	MaxBytes     int64        `json:"maxBytes,omitempty"`
	MaxAge       string       `json:"maxAge,omitempty"`
	MinFreeRatio float64      `json:"minFreeRatio,omitempty"`
	LastPrune    *PruneResult `json:"lastPrune,omitempty"`
}

// Stats は今の状態を返す。
func (c *Cache) Stats() (Stats, error) {
	st := Stats{
		BlobHits: c.stats.blobHits.Load(), BlobMisses: c.stats.blobMisses.Load(),
		ManifestHits: c.stats.manifestHits.Load(), ManifestMisses: c.stats.manifestMisses.Load(),
		BytesFromCache: c.stats.bytesFromCache.Load(), BytesFromUp: c.stats.bytesFromUp.Load(),
		StaleServed: c.stats.staleServed.Load(),
		MaxBytes:    c.cfg.MaxBytes, MinFreeRatio: c.cfg.MinFreeRatio,
	}
	if c.cfg.MaxAge > 0 {
		st.MaxAge = c.cfg.MaxAge.String()
	}
	c.statsMu.Lock()
	st.LastPrune = c.lastPrune
	c.statsMu.Unlock()
	for dir, count := range map[string]*int{"blobs": &st.Blobs, "manifests": &st.Manifests} {
		err := filepath.WalkDir(filepath.Join(c.cfg.Dir, dir), func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if info, err := d.Info(); err == nil {
				st.Bytes += info.Size()
			}
			if !strings.HasSuffix(d.Name(), ".json") {
				*count++
			}
			return nil
		})
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

// Clear は保存しているものをすべて消す。
func (c *Cache) Clear() error {
	c.pruneMu.Lock()
	defer c.pruneMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range []string{"blobs", "manifests", "tags"} {
		p := filepath.Join(c.cfg.Dir, d)
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			return err
		}
	}
	return nil
}
