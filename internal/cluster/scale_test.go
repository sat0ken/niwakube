package cluster

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

func workerNames(t *testing.T, ms *machine.Service, cluster string) []string {
	t.Helper()
	all, err := ms.ListCluster(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range all {
		if m.Role == "worker" {
			out = append(out, m.Name+"="+m.IP)
		}
	}
	slices.Sort(out)
	return out
}

func intp(i int) *int { return &i }

func TestScaleWorkers(t *testing.T) {
	ctx := context.Background()
	cs, ms, _, _ := newTest(t)
	ops := &fakeNodeOps{}
	cs.nodeOps = ops
	cs.postCreateInterval = 10 * time.Millisecond
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: intp(1)}); err != nil {
		t.Fatal(err)
	}

	// 増やす: 空いている番号で作る。IP はワーカーの番号から決まる
	c, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(3)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Operation, "adding") { // 裏の処理が進んでいれば "adding demo-worker-2 (1/2)"
		t.Errorf("operation = %q", c.Operation)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("scale up failed: %s", op)
	}
	want := []string{"demo-worker-1=172.30.1.21", "demo-worker-2=172.30.1.22", "demo-worker-3=172.30.1.23"}
	if got := workerNames(t, ms, "demo"); !slices.Equal(got, want) {
		t.Errorf("workers after scale up: %v, want %v", got, want)
	}
	if c, _ := cs.Get(ctx, "demo"); c.Workers != 3 {
		t.Errorf("spec workers = %d", c.Workers)
	}
	if m, _ := ms.Get(ctx, "demo-worker-3"); m.CPUs != c.Worker.CPUs || m.MemoryMiB != c.Worker.MemoryMiB {
		t.Errorf("new worker must have the worker size %+v: %+v", c.Worker, m)
	}
	if len(ops.Calls()) != 0 {
		t.Errorf("scale up must not touch Kubernetes: %v", ops.Calls())
	}

	// 減らす: 番号の大きいものから cordon → drain → Node を消す → マシンを消す
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(1)}); err != nil {
		t.Fatal(err)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("scale down failed: %s", op)
	}
	wantCalls := []string{
		"cordon demo-worker-3 true", "drain demo-worker-3", "delete demo-worker-3",
		"cordon demo-worker-2 true", "drain demo-worker-2", "delete demo-worker-2",
	}
	if got := ops.Calls(); !slices.Equal(got, wantCalls) {
		t.Errorf("node operations:\n got %v\nwant %v", got, wantCalls)
	}
	if got := workerNames(t, ms, "demo"); !slices.Equal(got, want[:1]) {
		t.Errorf("workers after scale down: %v", got)
	}
	if c, _ := cs.Get(ctx, "demo"); c.Workers != 1 || c.Status.Nodes != 2 {
		t.Errorf("after scale down: workers %d nodes %d", c.Workers, c.Status.Nodes)
	}

	// 0 台にしてから足すと、1 番から使う
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(0)}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(1)}); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, cs, "demo")
	if got := workerNames(t, ms, "demo"); !slices.Equal(got, want[:1]) {
		t.Errorf("workers after 0 -> 1: %v", got)
	}

	// 同じ台数なら何もしない
	if c, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(1)}); err != nil || c.Operation != "" {
		t.Errorf("no-op scale: %v %q", err, c.Operation)
	}

	for name, req := range map[string]ScaleRequest{
		"missing":  {},
		"negative": {Workers: intp(-1)},
		"too many": {Workers: intp(maxWorkers + 1)},
	} {
		if _, err := cs.Scale(ctx, "demo", req); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := cs.Scale(ctx, "nope", ScaleRequest{Workers: intp(1)}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown cluster: %v", err)
	}
	if _, err := cs.Stop(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(2)}); !errors.Is(err, machine.ErrInvalid) {
		t.Errorf("stopped cluster: want ErrInvalid, got %v", err)
	}
}

func TestScaleDownBusyAndNoAPI(t *testing.T) {
	ctx := context.Background()
	cs, ms, _, _ := newTest(t)
	ops := &fakeNodeOps{block: make(chan struct{})}
	cs.nodeOps = ops
	cs.postCreateInterval = 10 * time.Millisecond
	if _, err := cs.Create(ctx, CreateRequest{Name: "demo", Workers: intp(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(1)}); err != nil {
		t.Fatal(err)
	}
	// drain の間は、ほかの変更を受け付けない
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(3)}); !errors.Is(err, ErrBusy) {
		t.Errorf("scale while scaling: want ErrBusy, got %v", err)
	}
	if _, err := cs.Resize(ctx, "demo", ResizeRequest{Role: "worker", Size: store.NodeSize{CPUs: 4}}); !errors.Is(err, ErrBusy) {
		t.Errorf("resize while scaling: want ErrBusy, got %v", err)
	}
	close(ops.block)
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("scale down failed: %s", op)
	}

	// API に届かないときは、Node を残したままマシンを消さない
	cs.nodeOps = &fakeNodeOps{cordonErr: errors.New("connection refused")}
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(0)}); err != nil {
		t.Fatal(err)
	}
	if op := waitOperation(t, cs, "demo"); !strings.Contains(op, "API must be reachable") {
		t.Errorf("operation = %q", op)
	}
	if got := workerNames(t, ms, "demo"); len(got) != 1 {
		t.Errorf("the worker must stay: %v", got)
	}
	// 失敗のあとは、やり直せる
	cs.nodeOps = &fakeNodeOps{}
	if _, err := cs.Scale(ctx, "demo", ScaleRequest{Workers: intp(0)}); err != nil {
		t.Fatal(err)
	}
	if op := waitOperation(t, cs, "demo"); op != "" {
		t.Fatalf("retry failed: %s", op)
	}
	if c, _ := cs.Get(ctx, "demo"); c.Workers != 0 || len(workerNames(t, ms, "demo")) != 0 {
		t.Errorf("after retry: %d workers", c.Workers)
	}
}
