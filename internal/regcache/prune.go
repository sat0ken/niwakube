package regcache

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// キャッシュの自動の削除。使った時刻はファイルの更新時刻で覚える (キャッシュから返したときに付け直す)。
//
//  1. MaxAge のあいだ使われていないものを消す
//  2. 合計が MaxBytes を超えたら、使われていない順に、MaxBytes の 9 割まで消す
//  3. ファイルシステムの空きが MinFreeRatio を切ったら、空きが戻るまで使われていない順に消す
//
// マニフェストとレイヤーを別々に消すので、イメージの一部だけが残ることがある。そのときは足りない分を上流から取り直す。

// touchEvery は、使ったときに更新時刻を付け直す間隔 (毎回書かないように)。
const touchEvery = time.Hour

// tmpMaxAge より古い一時ファイルは、落ちたときの書きかけとして消す。
const tmpMaxAge = 24 * time.Hour

// touch は path の更新時刻を今にする (前に付け直してから touchEvery 経っていれば)。
func touch(path string) {
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) < touchEvery {
		return
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// PruneResult は 1 回の削除の結果。
type PruneResult struct {
	At         time.Time `json:"at"`
	Removed    int       `json:"removed"`    // 消したレイヤーとマニフェストの数
	FreedBytes int64     `json:"freedBytes"` // 空いた量
	Remaining  int64     `json:"remaining"`  // 残っている量
}

type entry struct {
	paths []string // マニフェストは本体と .json
	size  int64
	used  time.Time
}

func (c *Cache) entries() ([]entry, error) {
	var out []entry
	err := filepath.WalkDir(filepath.Join(c.cfg.Dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return nil // 消えた途中
		}
		out = append(out, entry{paths: []string{p}, size: fi.Size(), used: fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(filepath.Join(c.cfg.Dir, "manifests"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".json") {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		e := entry{paths: []string{p, p + ".json"}, size: fi.Size(), used: fi.ModTime()}
		if mi, err := os.Stat(p + ".json"); err == nil {
			e.size += mi.Size()
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// diskUsage はファイルシステムの空きと大きさ (テストで差し替える)。
func diskUsage(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// Prune は決まりにしたがってキャッシュを減らす。定期的に (Run) 呼ばれるほか、API からも呼べる。
func (c *Cache) Prune() (PruneResult, error) {
	c.pruneMu.Lock()
	defer c.pruneMu.Unlock()
	now := time.Now()
	res := PruneResult{At: now}
	all, err := c.entries()
	if err != nil {
		return res, err
	}
	remove := func(e entry) {
		for _, p := range e.paths {
			_ = os.Remove(p)
		}
		res.Removed++
		res.FreedBytes += e.size
	}

	// 1. 長く使われていないもの
	var keep []entry
	for _, e := range all {
		if c.cfg.MaxAge > 0 && now.Sub(e.used) > c.cfg.MaxAge {
			remove(e)
			continue
		}
		keep = append(keep, e)
	}
	var total int64
	for _, e := range keep {
		total += e.size
	}

	// 2・3. 合計の上限と、ファイルシステムの空き。どちらかを超えていれば、使われていない順に消す
	target := int64(-1)
	if c.cfg.MaxBytes > 0 && total > c.cfg.MaxBytes {
		target = c.cfg.MaxBytes * 9 / 10
	}
	if c.cfg.MinFreeRatio > 0 {
		if free, size, err := c.diskUsage(c.cfg.Dir); err == nil && size > 0 {
			want := uint64(c.cfg.MinFreeRatio * float64(size))
			if free < want {
				t := max(total-int64(want-free), 0)
				if target < 0 || t < target {
					target = t
				}
			}
		}
	}
	if target >= 0 {
		slices.SortFunc(keep, func(a, b entry) int { return a.used.Compare(b.used) })
		for _, e := range keep {
			if total <= target {
				break
			}
			remove(e)
			total -= e.size
		}
	}
	res.Remaining = total

	// タグの記録 (長く確かめていないもの) と、書きかけの一時ファイル
	c.removeOld(filepath.Join(c.cfg.Dir, "tags"), c.cfg.MaxAge)
	c.removeOld(filepath.Join(c.cfg.Dir, "tmp"), tmpMaxAge)

	c.statsMu.Lock()
	c.lastPrune = &res
	c.statsMu.Unlock()
	if res.Removed > 0 {
		c.log.Info("registry cache pruned", "removed", res.Removed, "freed", res.FreedBytes, "remaining", res.Remaining)
	}
	return res, nil
}

func (c *Cache) removeOld(dir string, age time.Duration) {
	if age <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && !e.IsDir() && time.Since(fi.ModTime()) > age {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Run は起動時と interval ごとに Prune する (ctx が終わるまで)。
func (c *Cache) Run(ctx context.Context, interval time.Duration) {
	for {
		if _, err := c.Prune(); err != nil {
			c.log.Warn("registry cache prune", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
