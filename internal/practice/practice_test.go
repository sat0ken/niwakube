package practice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sat0ken/niwakube/internal/kube"
)

// fakeCluster はマニフェストを覚えておくだけのクラスタ。apply は同じ名前のオブジェクトを丸ごと置き換える。
type fakeCluster struct {
	objs        map[string]map[string]any
	deleted     []string
	terminating map[string]int // Namespace ごとに、消えるまでに NamespaceExists が true を返す回数
	applyErr    error
}

func newFake() *fakeCluster {
	return &fakeCluster{objs: map[string]map[string]any{}, terminating: map[string]int{}}
}

func key(apiVersion, kind, ns, name string) string {
	return apiVersion + "/" + kind + "/" + ns + "/" + name
}

func (f *fakeCluster) Apply(_ context.Context, manifest string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	objs, err := kube.Decode(manifest)
	if err != nil {
		return err
	}
	for _, o := range objs {
		f.objs[key(o.GetAPIVersion(), o.GetKind(), o.GetNamespace(), o.GetName())] = o.Object
	}
	return nil
}

func (f *fakeCluster) Get(_ context.Context, apiVersion, kind, ns, name string) (map[string]any, error) {
	return f.objs[key(apiVersion, kind, ns, name)], nil
}

func (f *fakeCluster) List(_ context.Context, apiVersion, kind, ns, _ string) (map[string]any, error) {
	var items []any
	for k, o := range f.objs {
		if strings.HasPrefix(k, apiVersion+"/"+kind+"/"+ns+"/") {
			items = append(items, o)
		}
	}
	return map[string]any{"items": items}, nil
}

func (f *fakeCluster) DeleteNamespace(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	for k := range f.objs {
		if strings.Contains(k, "/"+name+"/") || strings.HasSuffix(k, "/Namespace//"+name) {
			delete(f.objs, k)
		}
	}
	return nil
}

func (f *fakeCluster) NamespaceExists(_ context.Context, name string) (bool, error) {
	if f.terminating[name] > 0 {
		f.terminating[name]--
		return true, nil
	}
	return false, nil
}

func service(t *testing.T, fc *fakeCluster) *Service {
	t.Helper()
	ps, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := newService(ps, func(_ context.Context, cluster string) (Cluster, error) {
		if cluster != "demo" {
			return nil, errors.New("no such cluster")
		}
		return fc, nil
	})
	s.poll = time.Millisecond
	return s
}

func TestLoadEmbedded(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) < 10 {
		t.Fatalf("want at least 10 problems, got %d", len(ps))
	}
	for _, p := range ps {
		if p.Answer == "" || p.Solution == "" {
			t.Errorf("%s: every problem needs a solution and an answer (CI applies the answer)", p.ID)
		}
		if _, err := kube.Decode(p.Setup + "\n---\n" + p.Answer); err != nil {
			t.Errorf("%s: setup/answer are not valid manifests: %v", p.ID, err)
		}
	}
}

// 解答を当てると、状態 (.status) やコントローラーが作るオブジェクトを見ない項目はすべて合格し、
// 出題直後は合格しない項目があること。(残りの項目は、本物のクラスタで E2E が確かめる)
func TestAnswersSatisfyChecks(t *testing.T) {
	ctx := context.Background()
	fc := newFake()
	s := service(t, fc)
	for _, sum := range s.List() {
		p, _ := s.Get(sum.ID)
		if err := s.Setup(ctx, "demo", p.ID); err != nil {
			t.Fatalf("%s: setup: %v", p.ID, err)
		}
		before, err := s.Check(ctx, "demo", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if before.Passed == before.Total {
			t.Errorf("%s: all checks pass right after setup", p.ID)
		}
		if err := fc.Apply(ctx, p.Answer); err != nil {
			t.Fatalf("%s: answer: %v", p.ID, err)
		}
		after, err := s.Check(ctx, "demo", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range after.Checks {
			// 状態 (.status) や、コントローラーが作るオブジェクト (selector で選ぶ EndpointSlice など) は本物のクラスタでしか確かめられない
			if strings.Contains(p.Checks[i].JSONPath, ".status") || p.Checks[i].Selector != "" {
				continue
			}
			if !c.Passed {
				t.Errorf("%s: answer fails %q: %s", p.ID, c.Description, c.Message)
			}
		}
	}
}

func TestSetupRecreatesNamespaces(t *testing.T) {
	ctx := context.Background()
	fc := newFake()
	s := service(t, fc)
	fc.objs[key("v1", "ConfigMap", "shop", "leftover")] = map[string]any{"data": map[string]any{"x": "y"}}
	fc.terminating["shop"] = 3
	if err := s.Setup(ctx, "demo", "ckad-configmap-env"); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "shop" {
		t.Errorf("deleted namespaces: %v", fc.deleted)
	}
	if fc.terminating["shop"] != 0 {
		t.Error("setup must wait until the namespace is gone")
	}
	if fc.objs[key("v1", "ConfigMap", "shop", "leftover")] != nil {
		t.Error("objects from the last attempt must be gone")
	}
	if fc.objs[key("v1", "Namespace", "", "shop")] == nil {
		t.Error("setup must create the namespace again")
	}

	// 消え終わらなければタイムアウト
	fc.terminating["shop"] = 1 << 30
	s.nsWait = 20 * time.Millisecond
	if err := s.Setup(ctx, "demo", "ckad-configmap-env"); err == nil || !strings.Contains(err.Error(), "terminating") {
		t.Errorf("want a terminating error, got %v", err)
	}

	if err := s.Setup(ctx, "demo", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown problem: %v", err)
	}
	if err := s.Setup(ctx, "other", "ckad-cronjob"); err == nil {
		t.Error("unknown cluster must fail")
	}
}

func TestCheckScoreAndMessages(t *testing.T) {
	ctx := context.Background()
	fc := newFake()
	s := service(t, fc)
	if err := s.Setup(ctx, "demo", "ckad-cronjob"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Check(ctx, "demo", "ckad-cronjob")
	if err != nil {
		t.Fatal(err)
	}
	if r.Score != 0 || r.Passed != 0 || !strings.Contains(r.Checks[0].Message, "見つかりません") {
		t.Errorf("before answering: %+v", r)
	}
	// 6 項目のうち、スケジュールだけ間違えた解答 → 5/6、配点 5 × 5/6 = 4 (切り捨て)
	p, _ := s.Get("ckad-cronjob")
	if err := fc.Apply(ctx, strings.Replace(p.Answer, "'*/5 * * * *'", "'*/10 * * * *'", 1)); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Check(ctx, "demo", "ckad-cronjob")
	if r.Passed != 5 || r.Total != 6 || r.Score != 4 || r.Checks[0].Passed || r.Checks[0].Message != "値が違います" {
		t.Errorf("one wrong field: %+v", r)
	}
}

func TestValidate(t *testing.T) {
	base := `id: x-1
exam: CKAD
domain: d
title: t
weight: 1
namespaces: [a]
task: do it
checks:
  - description: c
    apiVersion: v1
    kind: ConfigMap
    namespace: a
    name: cm
`
	for name, c := range map[string]struct {
		file, body, want string
	}{
		"ok":                {"x-1", base, ""},
		"id vs file name":   {"x-2", base, "file name"},
		"bad exam":          {"x-1", strings.Replace(base, "CKAD", "CKS", 1), "exam"},
		"protected ns":      {"x-1", strings.Replace(base, "[a]", "[kube-system]", 1), "cannot be used"},
		"name and selector": {"x-1", base + "    selector: app=x\n", "exactly one"},
		"bad jsonpath":      {"x-1", base + "    jsonpath: '{.spec[}'\n", "jsonpath"},
		"equals w/o path":   {"x-1", base + "    equals: v\n", "need a jsonpath"},
		"unknown field":     {"x-1", base + "extra: 1\n", "unknown field"},
	} {
		_, err := load(fstest.MapFS{"problems/" + c.file + ".yaml": {Data: []byte(c.body)}})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}
