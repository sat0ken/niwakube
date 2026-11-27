package cluster

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

func snapshotTest(t *testing.T) (*Service, *machine.Service) {
	t.Helper()
	cs, ms, _, _ := newTest(t)
	cs.nodeOps = &fakeNodeOps{}
	cs.postCreateInterval = 10 * time.Millisecond
	cs.cfg.SnapshotDir = filepath.Join(t.TempDir(), "snapshots")
	cs.cfg.NFS = NFSConfig{Server: netip.MustParseAddr("172.30.0.1"), Root: filepath.Join(t.TempDir(), "nfs")}
	return cs, ms
}

func nodeNames(t *testing.T, ms *machine.Service, cluster string) []string {
	t.Helper()
	all, err := ms.ListCluster(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range all {
		out = append(out, m.Name+":"+string(m.Status.State))
	}
	slices.Sort(out)
	return out
}

func TestSnapshotAndRestore(t *testing.T) {
	ctx := context.Background()
	cs, ms := snapshotTest(t)
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: intp(1)}); err != nil {
		t.Fatal(err)
	}
	nfsFile := filepath.Join(cs.nfsDir("demo"), "pvc-1", "data.txt")
	if err := os.MkdirAll(filepath.Dir(nfsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(nfsFile, []byte("v1"), 0o644)

	c, err := cs.CreateSnapshot(ctx, "demo", SnapshotRequest{Name: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Operation == "" {
		t.Error("snapshot must run as an operation")
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("snapshot failed: %s", op)
	}
	snaps, err := cs.ListSnapshots(ctx, "demo")
	if err != nil || len(snaps) != 1 || snaps[0].Name != "before" || !snaps[0].NFS ||
		!slices.Equal(snaps[0].Nodes, []string{"demo-cp-1", "demo-worker-1"}) {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	// 取ったあとは、もとどおり動いている
	want := []string{"demo-cp-1:running", "demo-worker-1:running"}
	if got := nodeNames(t, ms, "demo"); !slices.Equal(got, want) {
		t.Errorf("after snapshot: %v", got)
	}

	// 変える: ワーカーを足し、もう 1 台のサイズを変え、NFS のデータを書き換える
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(2)}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	if _, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "worker", Size: store.NodeSize{MemoryMiB: 8192}}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	os.WriteFile(nfsFile, []byte("v2"), 0o644)

	if _, err := cs.RestoreSnapshot(ctx, "demo", "before"); err != nil {
		t.Fatal(err)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("restore failed: %s", op)
	}
	if got := nodeNames(t, ms, "demo"); !slices.Equal(got, want) {
		t.Errorf("after restore: %v (the added worker must be gone)", got)
	}
	if m, _ := ms.Get(ctx, "demo-worker-1"); m.MemoryMiB != 4096 {
		t.Errorf("worker size must be restored: %d", m.MemoryMiB)
	}
	if c, _ := cs.Get(ctx, "demo"); c.Workers != 1 || c.Worker.MemoryMiB != 4096 {
		t.Errorf("cluster spec must be restored: workers %d, worker %+v", c.Workers, c.Worker)
	}
	if b, _ := os.ReadFile(nfsFile); string(b) != "v1" {
		t.Errorf("nfs data = %q", b)
	}

	// スナップショットのあとに消したノードは戻る
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(0)}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	if _, err := cs.RestoreSnapshot(ctx, "demo", "before"); err != nil {
		t.Fatal(err)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("restore failed: %s", op)
	}
	if got := nodeNames(t, ms, "demo"); !slices.Equal(got, want) {
		t.Errorf("the removed worker must come back: %v", got)
	}

	// 止まっているクラスタは、止まったまま取る・戻す
	if _, err := cs.Stop(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateSnapshot(ctx, "demo", SnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	if got := nodeNames(t, ms, "demo"); !slices.Equal(got, []string{"demo-cp-1:stopped", "demo-worker-1:stopped"}) {
		t.Errorf("a stopped cluster must stay stopped: %v", got)
	}
	if snaps, _ := cs.ListSnapshots(ctx, "demo"); len(snaps) != 2 || snaps[1].Name != "before" {
		t.Errorf("newest first: %+v", snaps)
	}

	if err := cs.DeleteSnapshot(ctx, "demo", "before"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.RestoreSnapshot(ctx, "demo", "before"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted snapshot: %v", err)
	}
	// クラスタを消すとスナップショットも消える
	if err := cs.Delete(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cs.snapshotRoot("demo")); !os.IsNotExist(err) {
		t.Errorf("snapshots must be deleted with the cluster: %v", err)
	}
}

func TestSnapshotErrors(t *testing.T) {
	ctx := context.Background()
	cs, _ := snapshotTest(t)
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: intp(0)}); err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]SnapshotRequest{"upper case": {Name: "Before"}, "slash": {Name: "a/b"}, "dots": {Name: ".."}} {
		if _, err := cs.CreateSnapshot(ctx, "demo", req); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := cs.CreateSnapshot(ctx, "demo", SnapshotRequest{Name: "s1"}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	if _, err := cs.CreateSnapshot(ctx, "demo", SnapshotRequest{Name: "s1"}); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := cs.CreateSnapshot(ctx, "nope", SnapshotRequest{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown cluster: %v", err)
	}
	if _, err := cs.RestoreSnapshot(ctx, "demo", "../demo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bad snapshot name: %v", err)
	}
	// 別の操作の最中は受け付けない (起動・停止・削除も)
	cs.setOperation("demo", "resizing worker nodes (0/1)")
	if _, err := cs.CreateSnapshot(ctx, "demo", SnapshotRequest{}); !errors.Is(err, ErrBusy) {
		t.Errorf("busy snapshot: %v", err)
	}
	if _, err := cs.RestoreSnapshot(ctx, "demo", "s1"); !errors.Is(err, ErrBusy) {
		t.Errorf("busy restore: %v", err)
	}
	if _, err := cs.Stop(ctx, "demo"); !errors.Is(err, ErrBusy) {
		t.Errorf("busy stop: %v", err)
	}
	if err := cs.Delete(ctx, "demo"); !errors.Is(err, ErrBusy) {
		t.Errorf("busy delete: %v", err)
	}
	cs.setOperation("demo", "")
	// 置き場所が無ければ使えない
	cs.cfg.SnapshotDir = ""
	if _, err := cs.ListSnapshots(ctx, "demo"); !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("disabled: %v", err)
	}
}

func TestSnapshotAndWait(t *testing.T) {
	ctx := context.Background()
	cs, _, _, pr := newTest(t)
	cs.cfg.SnapshotDir = filepath.Join(t.TempDir(), "snapshots")
	cs.postCreateInterval = 10 * time.Millisecond
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: intp(1)}); err != nil {
		t.Fatal(err)
	}
	pr.mu.Lock()
	pr.err, pr.ready = nil, 2 // Kubernetes の API に届き、2 台とも Ready
	pr.mu.Unlock()

	if has, err := cs.HasSnapshot(ctx, "demo", "practice-x"); err != nil || has {
		t.Fatalf("before: %v %v", has, err)
	}
	if err := cs.SnapshotAndWait(ctx, "demo", "practice-x"); err != nil {
		t.Fatal(err)
	}
	if has, _ := cs.HasSnapshot(ctx, "demo", "practice-x"); !has {
		t.Error("snapshot must exist")
	}
	if cs.operation("demo") != "" {
		t.Error("SnapshotAndWait must return after the operation finished")
	}
	if err := cs.RestoreAndWait(ctx, "demo", "practice-x"); err != nil {
		t.Fatal(err)
	}
	// ready に戻らなければ時間切れ
	pr.mu.Lock()
	pr.err = errors.New("connection refused")
	pr.mu.Unlock()
	cs.readyWait = 200 * time.Millisecond
	if err := cs.RestoreAndWait(ctx, "demo", "practice-x"); err == nil {
		t.Error("want a timeout while the cluster is not ready")
	}
}
