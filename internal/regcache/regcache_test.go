package regcache

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// upstream は手元で動かすレジストリ。blob と manifest の GET の回数を数える。
type upstream struct {
	srv       *httptest.Server
	host      string
	blobGets  atomic.Int64
	manifests atomic.Int64
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	reg := registry.New()
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			u.blobGets.Add(1)
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
			u.manifests.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(u.srv.Close)
	pu, _ := url.Parse(u.srv.URL)
	u.host = pu.Host
	return u
}

func (u *upstream) push(t *testing.T, repoTag string) v1.Image {
	t.Helper()
	img, err := random.Image(256*1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(u.host+"/"+repoTag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	return img
}

func newCache(t *testing.T, u *upstream) (*Cache, *httptest.Server) {
	t.Helper()
	c, err := New(Config{Dir: t.TempDir(), Upstreams: []string{u.host}, Insecure: []string{u.host}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(c)
	t.Cleanup(ts.Close)
	return c, ts
}

func get(t *testing.T, method, url string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func TestPullThroughAndOffline(t *testing.T) {
	u := newUpstream(t)
	img := u.push(t, "team/app:v1")
	c, ts := newCache(t, u)
	ns := "?ns=" + u.host

	if resp, _ := get(t, http.MethodGet, ts.URL+"/v2/"); resp.StatusCode != http.StatusOK {
		t.Errorf("/v2/: %d", resp.StatusCode)
	}
	resp, body := get(t, http.MethodGet, ts.URL+"/v2/team/app/manifests/v1"+ns)
	want, _ := img.RawManifest()
	digest, _ := img.Digest()
	if resp.StatusCode != http.StatusOK || string(body) != string(want) || resp.Header.Get("Docker-Content-Digest") != digest.String() {
		t.Fatalf("manifest: %d %q %s", resp.StatusCode, resp.Header.Get("Docker-Content-Digest"), body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "manifest") {
		t.Errorf("content type: %s", ct)
	}
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	resp, blob := get(t, http.MethodGet, ts.URL+"/v2/team/app/blobs/"+ld.String()+ns)
	if resp.StatusCode != http.StatusOK || sha(blob) != ld.String() {
		t.Fatalf("blob: %d %s", resp.StatusCode, sha(blob))
	}
	// HEAD はヘッダーだけ
	resp, body = get(t, http.MethodHead, ts.URL+"/v2/team/app/manifests/v1"+ns)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.Header.Get("Docker-Content-Digest") != digest.String() {
		t.Errorf("head: %d %d", resp.StatusCode, len(body))
	}
	// Range (containerd は途中から取り直すことがある)
	resp, part := get(t, http.MethodGet, ts.URL+"/v2/team/app/blobs/"+ld.String()+ns, "Range", "bytes=0-9")
	if resp.StatusCode != http.StatusPartialContent || len(part) != 10 {
		t.Errorf("range: %d %d", resp.StatusCode, len(part))
	}

	// 上流が止まっても、取ったものは返せる (タグは期限内、ダイジェストはいつでも)
	u.srv.Close()
	for _, p := range []string{"/manifests/v1", "/manifests/" + digest.String(), "/blobs/" + ld.String()} {
		if resp, _ := get(t, http.MethodGet, ts.URL+"/v2/team/app"+p+ns); resp.StatusCode != http.StatusOK {
			t.Errorf("offline %s: %d", p, resp.StatusCode)
		}
	}
	// タグの期限が切れていて上流に届かなくても、保存してあるものを返す
	c.cfg.TagTTL = time.Nanosecond
	if resp, body := get(t, http.MethodGet, ts.URL+"/v2/team/app/manifests/v1"+ns); resp.StatusCode != http.StatusOK || string(body) != string(want) {
		t.Errorf("stale: %d", resp.StatusCode)
	}
	st, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.StaleServed != 1 || st.Blobs != 1 || st.Manifests != 1 || st.BlobMisses != 1 || st.BlobHits < 2 || st.BytesFromUp == 0 {
		t.Errorf("stats: %+v", st)
	}
	// 取っていないものは、上流に届かなければ 502
	if resp, _ := get(t, http.MethodGet, ts.URL+"/v2/team/other/manifests/v1"+ns); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("unknown while offline: %d", resp.StatusCode)
	}
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Stats(); st.Blobs != 0 || st.Manifests != 0 {
		t.Errorf("after clear: %+v", st)
	}
}

func TestTagRevalidation(t *testing.T) {
	u := newUpstream(t)
	u.push(t, "app:latest")
	c, ts := newCache(t, u)
	ns := "?ns=" + u.host
	get(t, http.MethodGet, ts.URL+"/v2/app/manifests/latest"+ns)
	if u.manifests.Load() != 1 {
		t.Fatalf("manifest GETs: %d", u.manifests.Load())
	}
	// 期限内は上流に聞かない
	get(t, http.MethodGet, ts.URL+"/v2/app/manifests/latest"+ns)
	if u.manifests.Load() != 1 {
		t.Errorf("must not ask upstream within the TTL: %d", u.manifests.Load())
	}
	// 期限切れでも、変わっていなければ HEAD だけで済ませる (GET は増えない)
	c.cfg.TagTTL = time.Nanosecond
	get(t, http.MethodGet, ts.URL+"/v2/app/manifests/latest"+ns)
	if u.manifests.Load() != 1 {
		t.Errorf("unchanged tag must be revalidated by HEAD: %d GETs", u.manifests.Load())
	}
	// タグが変わったら、新しいものを取る
	img2 := u.push(t, "app:latest")
	d2, _ := img2.Digest()
	resp, _ := get(t, http.MethodGet, ts.URL+"/v2/app/manifests/latest"+ns)
	if resp.Header.Get("Docker-Content-Digest") != d2.String() {
		t.Errorf("moved tag: got %s, want %s", resp.Header.Get("Docker-Content-Digest"), d2)
	}
}

func TestConcurrentBlobFetchedOnce(t *testing.T) {
	u := newUpstream(t)
	img := u.push(t, "big:v1")
	_, ts := newCache(t, u)
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	var wg sync.WaitGroup
	var bad atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, b := get(t, http.MethodGet, ts.URL+"/v2/big/blobs/"+ld.String()+"?ns="+u.host)
			if resp.StatusCode != http.StatusOK || sha(b) != ld.String() {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d clients got a bad blob", bad.Load())
	}
	if n := u.blobGets.Load(); n != 1 {
		t.Errorf("upstream blob GETs: %d (want 1)", n)
	}
}

func TestRejects(t *testing.T) {
	u := newUpstream(t)
	_, ts := newCache(t, u)
	ns := "?ns=" + u.host
	for name, c := range map[string]struct {
		method, path string
		want         int
	}{
		"not mirrored": {http.MethodGet, "/v2/app/manifests/v1?ns=evil.example.com", http.StatusNotFound},
		"bad digest":   {http.MethodGet, "/v2/app/blobs/sha256:xyz" + ns, http.StatusBadRequest},
		"traversal":    {http.MethodGet, "/v2/app/blobs/sha256:../../etc/passwd" + ns, http.StatusBadRequest},
		"bad tag":      {http.MethodGet, "/v2/app/manifests/-bad" + ns, http.StatusBadRequest},
		"write":        {http.MethodPut, "/v2/app/manifests/v1" + ns, http.StatusMethodNotAllowed},
		"bad path":     {http.MethodGet, "/v2/app/tags/list" + ns, http.StatusNotFound},
		"missing":      {http.MethodGet, "/v2/nope/manifests/v1" + ns, http.StatusNotFound},
	} {
		if resp, body := get(t, c.method, ts.URL+c.path); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
}
