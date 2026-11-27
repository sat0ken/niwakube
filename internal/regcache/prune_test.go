package regcache

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fill は上流に n 個のイメージを置いて、キャッシュを通して取る。返すのは各イメージのレイヤーのダイジェスト。
func fill(t *testing.T, u *upstream, base string, n int) [][]string {
	t.Helper()
	var out [][]string
	for i := range n {
		tag := string(rune('a' + i))
		img := u.push(t, "img"+tag+":v1")
		get(t, http.MethodGet, base+"/v2/img"+tag+"/manifests/v1?ns="+u.host)
		layers, _ := img.Layers()
		var ds []string
		for _, l := range layers {
			d, _ := l.Digest()
			get(t, http.MethodGet, base+"/v2/img"+tag+"/blobs/"+d.String()+"?ns="+u.host)
			ds = append(ds, d.String())
		}
		out = append(out, ds)
	}
	return out
}

func setUsed(t *testing.T, c *Cache, digests []string, at time.Time) {
	t.Helper()
	for _, d := range digests {
		if err := os.Chtimes(c.blobPath(d), at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneByAge(t *testing.T) {
	u := newUpstream(t)
	c, ts := newCache(t, u)
	imgs := fill(t, u, ts.URL, 2)
	setUsed(t, c, imgs[0], time.Now().Add(-40*24*time.Hour))
	// 古い一時ファイル (落ちたときの書きかけ) と、新しい一時ファイル (書いている途中)
	old := filepath.Join(c.cfg.Dir, "tmp", "blob-old")
	young := filepath.Join(c.cfg.Dir, "tmp", "blob-young")
	os.WriteFile(old, []byte("x"), 0o600)
	os.WriteFile(young, []byte("x"), 0o600)
	os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))

	c.cfg.MaxAge = 30 * 24 * time.Hour
	res, err := c.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != len(imgs[0]) || res.FreedBytes == 0 {
		t.Errorf("result: %+v", res)
	}
	for _, d := range imgs[0] {
		if exists(c.blobPath(d)) {
			t.Errorf("unused blob %s must be removed", d)
		}
	}
	for _, d := range imgs[1] {
		if !exists(c.blobPath(d)) {
			t.Errorf("recent blob %s must stay", d)
		}
	}
	if exists(old) || !exists(young) {
		t.Errorf("tmp: old %v young %v", exists(old), exists(young))
	}
	if st, _ := c.Stats(); st.LastPrune == nil || st.LastPrune.Removed != res.Removed || st.MaxAge != "720h0m0s" {
		t.Errorf("stats: %+v", st)
	}
	// 消したレイヤーは、次に取りにきたら上流から取り直す
	resp, b := get(t, http.MethodGet, ts.URL+"/v2/imga/blobs/"+imgs[0][0]+"?ns="+u.host)
	if resp.StatusCode != http.StatusOK || sha(b) != imgs[0][0] {
		t.Errorf("refetch after prune: %d", resp.StatusCode)
	}
}

func TestPruneBySizeOldestFirst(t *testing.T) {
	u := newUpstream(t)
	c, ts := newCache(t, u)
	imgs := fill(t, u, ts.URL, 3)
	base := time.Now().Add(-3 * time.Hour)
	for i, ds := range imgs { // imga がいちばん古く使われた
		setUsed(t, c, ds, base.Add(time.Duration(i)*time.Hour))
	}
	st, _ := c.Stats()
	c.cfg.MaxBytes = st.Bytes / 2
	res, err := c.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining > c.cfg.MaxBytes*9/10 {
		t.Errorf("must go down to 90%% of the limit: remaining %d, limit %d", res.Remaining, c.cfg.MaxBytes)
	}
	for _, d := range imgs[0] {
		if exists(c.blobPath(d)) {
			t.Error("the least recently used image must go first")
		}
	}
	for _, d := range imgs[2] {
		if !exists(c.blobPath(d)) {
			t.Error("the most recently used image must stay")
		}
	}
}

func TestPruneByFreeSpace(t *testing.T) {
	u := newUpstream(t)
	c, ts := newCache(t, u)
	imgs := fill(t, u, ts.URL, 2)
	setUsed(t, c, imgs[0], time.Now().Add(-2*time.Hour))
	st, _ := c.Stats()
	// 空きが 10% を切っていて、足りない分は imga のレイヤー 1 つ分
	layer := st.Bytes / 5
	c.cfg.MinFreeRatio = 0.1
	c.diskUsage = func(string) (uint64, uint64, error) {
		total := uint64(100 * layer)
		return uint64(10*layer) - uint64(layer), total, nil
	}
	res, err := c.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if res.FreedBytes < layer || res.Removed == 0 {
		t.Errorf("must free at least the missing space: %+v (need %d)", res, layer)
	}
	for _, d := range imgs[1] {
		if !exists(c.blobPath(d)) {
			t.Error("recently used blobs must stay when freeing a little space")
		}
	}
	// 空きが足りていれば消さない
	c.diskUsage = func(string) (uint64, uint64, error) { return 50, 100, nil }
	if res, _ := c.Prune(); res.Removed != 0 {
		t.Errorf("enough free space: %+v", res)
	}
}

func TestHitTouchesUsedTime(t *testing.T) {
	u := newUpstream(t)
	c, ts := newCache(t, u)
	imgs := fill(t, u, ts.URL, 1)
	old := time.Now().Add(-10 * 24 * time.Hour)
	setUsed(t, c, imgs[0][:1], old)
	get(t, http.MethodGet, ts.URL+"/v2/imga/blobs/"+imgs[0][0]+"?ns="+u.host)
	fi, _ := os.Stat(c.blobPath(imgs[0][0]))
	if time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("a hit must refresh the used time: %v", fi.ModTime())
	}
}
