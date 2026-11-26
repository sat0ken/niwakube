package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

// fakeNodeOps は Kubernetes の操作を記録する。ReadySince は大きさを変えたあと (since 以降) なら Ready を返す。
type fakeNodeOps struct {
	mu        sync.Mutex
	calls     []string
	cordonErr error
	block     chan struct{} // nil でなければ Drain がこれを待つ
}

func (f *fakeNodeOps) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeNodeOps) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeNodeOps) Cordon(_ context.Context, _ []byte, node string, on bool) error {
	if f.cordonErr != nil {
		return f.cordonErr
	}
	f.record(fmt.Sprintf("cordon %s %t", node, on))
	return nil
}

func (f *fakeNodeOps) Drain(ctx context.Context, _ []byte, node string) error {
	f.record("drain " + node)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return nil
}

func (f *fakeNodeOps) ReadySince(_ context.Context, _ []byte, node string, since time.Time) (bool, error) {
	f.record("ready? " + node)
	return !since.IsZero(), nil
}

func waitOperation(t *testing.T, cs *Service, name string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		op := cs.operation(name)
		if op == "" || strings.HasPrefix(op, "failed:") {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation did not finish: %s", op)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResizeWorkers(t *testing.T) {
	ctx := context.Background()
	cs, ms, fb, _ := newTest(t)
	ops := &fakeNodeOps{}
	cs.nodeOps = ops
	cs.postCreateInterval = 10 * time.Millisecond

	two := 2
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: &two, Worker: store.NodeSize{CPUs: 2, MemoryMiB: 2048, DiskGiB: 30}}); err != nil {
		t.Fatal(err)
	}

	c, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "worker", Size: store.NodeSize{MemoryMiB: 4096, DiskGiB: 40}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Worker != (store.NodeSize{CPUs: 2, MemoryMiB: 4096, DiskGiB: 40}) || !strings.HasPrefix(c.Operation, "resizing") {
		t.Errorf("after resize request: worker %+v operation %q", c.Worker, c.Operation)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("resize failed: %s", op)
	}

	// 1 台ずつ: cordon → drain → (大きさの変更) → Ready の確認 → uncordon
	want := []string{
		"cordon demo-worker-1 true", "drain demo-worker-1", "ready? demo-worker-1", "cordon demo-worker-1 false",
		"cordon demo-worker-2 true", "drain demo-worker-2", "ready? demo-worker-2", "cordon demo-worker-2 false",
	}
	if got := ops.Calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("node operations:\n got %v\nwant %v", got, want)
	}
	for _, n := range []string{"demo-worker-1", "demo-worker-2"} {
		m, _ := ms.Get(ctx, n)
		spec, _ := fb.Spec(n)
		if m.MemoryMiB != 4096 || m.DiskGiB != 40 || spec.MemoryMiB != 4096 || m.Status.State != "running" {
			t.Errorf("%s: %+v (backend %+v)", n, m, spec)
		}
	}
	// コントロールプレーンは変えない
	if m, _ := ms.Get(ctx, "demo-cp-1"); m.MemoryMiB != 4096 || m.DiskGiB != 30 {
		t.Errorf("control plane must not change: %+v", m)
	}

	// 同じ大きさならノードは触らない
	before := len(ops.Calls())
	if c, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "worker", Size: store.NodeSize{MemoryMiB: 4096}}); err != nil || c.Operation != "" {
		t.Errorf("no-op resize: %v %q", err, c.Operation)
	}
	if len(ops.Calls()) != before {
		t.Error("no-op resize must not touch nodes")
	}

	for name, req := range map[string]ResizeRequest{
		"bad role":    {Role: "etcd", Size: store.NodeSize{CPUs: 4}},
		"shrink disk": {Role: "worker", Size: store.NodeSize{DiskGiB: 30}},
		"too big":     {Role: "worker", Size: store.NodeSize{MemoryMiB: 1 << 20}},
	} {
		if _, err := cs.Resize(ctx, "demo", req); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := cs.Resize(ctx, "nope", ResizeRequest{Role: "worker"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown cluster: %v", err)
	}
}

func TestResizeBusyAndNoAPI(t *testing.T) {
	ctx := context.Background()
	cs, ms, _, _ := newTest(t)
	ops := &fakeNodeOps{block: make(chan struct{})}
	cs.nodeOps = ops
	cs.postCreateInterval = 10 * time.Millisecond
	one := 1
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: &one}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "worker", Size: store.NodeSize{CPUs: 4}}); err != nil {
		t.Fatal(err)
	}
	// drain が終わるまでは、別の変更を受け付けない
	if _, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "control-plane", Size: store.NodeSize{CPUs: 4}}); !errors.Is(err, ErrBusy) {
		t.Errorf("want ErrBusy while resizing, got %v", err)
	}
	close(ops.block)
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("resize failed: %s", op)
	}

	// API に届かない (cordon が失敗する) ときは drain せずに再起動する
	ops2 := &fakeNodeOps{cordonErr: errors.New("connection refused")}
	cs.nodeOps = ops2
	if _, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "control-plane", Size: store.NodeSize{CPUs: 4}}); err != nil {
		t.Fatal(err)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("resize failed: %s", op)
	}
	if m, _ := ms.Get(ctx, "demo-cp-1"); m.CPUs != 4 {
		t.Errorf("control plane: %+v", m)
	}
	for _, c := range ops2.Calls() {
		if strings.HasPrefix(c, "drain") {
			t.Errorf("must not drain when the API is unreachable: %v", ops2.Calls())
		}
	}
}
