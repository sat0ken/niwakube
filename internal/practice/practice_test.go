package practice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sat0ken/niwakube/internal/kube"
	"github.com/sat0ken/niwakube/internal/nodeexec"
)

// fakeCluster はマニフェストを覚えておくだけのクラスタ。apply は同じ名前のオブジェクトを丸ごと置き換える。
type fakeCluster struct {
	canICalls   []string
	allowed     map[string]bool // "verb resource" → できるか
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

func (f *fakeCluster) List(_ context.Context, apiVersion, kind, ns, _, _ string) (map[string]any, error) {
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

func (f *fakeCluster) CanI(_ context.Context, user string, groups []string, verb, group, resource, ns string) (bool, error) {
	f.canICalls = append(f.canICalls, user+" "+strings.Join(groups, ",")+" "+verb+" "+group+"/"+resource+" "+ns)
	return f.allowed[verb+" "+resource], nil
}

// fakeNodes はノードで動かしたコマンドを記録する。exit に書いたコマンドはその終了コードを返す (ほかは 0)。
type fakeNodes struct {
	log  []string
	exit map[string]int
}

func (n *fakeNodes) run(_ context.Context, node, script string) (*nodeexec.Result, error) {
	n.log = append(n.log, node+": "+script)
	return &nodeexec.Result{ExitCode: n.exit[script]}, nil
}

func target(fc *fakeCluster, nodes *fakeNodes) *Target {
	t := &Target{Kube: fc, Distro: "kubeadm", Version: "v1.36", Snapshots: &fakeSnapshots{have: map[string]bool{}}}
	if nodes != nil {
		t.Nodes = map[string]string{"cp": "demo-cp-1", "worker": "demo-worker-1"}
		t.Run = nodes.run
	}
	return t
}

func serviceFor(t *testing.T, ps []*Problem, tg *Target) *Service {
	t.Helper()
	s := newService(ps, func(_ context.Context, cluster string) (*Target, error) {
		if cluster != "demo" {
			return nil, errors.New("no such cluster")
		}
		return tg, nil
	})
	s.poll = time.Millisecond
	return s
}

func service(t *testing.T, fc *fakeCluster) *Service {
	t.Helper()
	ps, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return serviceFor(t, ps, target(fc, &fakeNodes{}))
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
		if (p.Answer == "" && len(p.AnswerRun) == 0) || p.Solution == "" {
			t.Errorf("%s: every problem needs a solution and an answer (CI applies the answer)", p.ID)
		}
		if _, err := kube.Decode(p.Setup + "\n---\n" + p.Answer); err != nil {
			t.Errorf("%s: setup/answer are not valid manifests: %v", p.ID, err)
		}
	}
}

// static は本物のクラスタが無くても確かめられる項目か。状態 (.status)、コントローラーや kubelet が作る・変える
// オブジェクト (selector で選ぶもの、Node、ノードの名前を使うもの)、ノードでのコマンド、権限は E2E で確かめる。
func static(c Check) bool {
	eq := ""
	if c.Equals != nil {
		eq = *c.Equals
	}
	return c.CanI == nil && c.Node == "" && c.Kind != "Node" && c.Selector == "" && c.FieldSelector == "" &&
		!strings.Contains(c.JSONPath, ".status") && !phRe.MatchString(c.Name+c.JSONPath+eq)
}

// 解答を当てると static な項目はすべて合格すること。全項目が static な問題は、出題直後は満点でないこと。
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
		staticPass, staticTotal := 0, 0
		for i, c := range before.Checks {
			if static(p.Checks[i]) {
				staticTotal++
				if c.Passed {
					staticPass++
				}
			}
		}
		// 全項目が static な問題だけ、出題直後に満点でないことをここで確かめる (残りは E2E)
		if staticTotal == len(p.Checks) && staticPass == staticTotal {
			t.Errorf("%s: all static checks pass right after setup", p.ID)
		}
		if err := s.Solve(ctx, "demo", p.ID); err != nil {
			t.Fatalf("%s: solve: %v", p.ID, err)
		}
		after, err := s.Check(ctx, "demo", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range after.Checks {
			if static(p.Checks[i]) && !c.Passed {
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
		"object and node":   {"x-1", base + "    node: cp\n    run: x\n", "exactly one"},
		"bad node":          {"x-1", strings.Replace(base, "    apiVersion: v1\n    kind: ConfigMap\n    namespace: a\n    name: cm\n", "    node: master\n    run: x\n", 1), "node check"},
		"canI w/o allowed":  {"x-1", strings.Replace(base, "    apiVersion: v1\n    kind: ConfigMap\n    namespace: a\n    name: cm\n", "    canI: {serviceAccount: a/b, verb: get, resource: pods}\n", 1), "allowed"},
		"bad requires":      {"x-1", base + "requires: talos\n", "requires"},
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

const nodeProblems = `id: n-1
exam: CKA
domain: d
title: node problem
weight: 4
requires: kubeadm
namespaces: [n1]
setup: |
  apiVersion: v1
  kind: Namespace
  metadata:
    name: n1
nodeSetup:
  - node: worker
    run: break {{worker}}
cleanup:
  - node: cp
    run: uncordon {{worker}}
task: fix it
checks:
  - description: node command
    node: worker
    run: verify {{cp}}
  - description: can deploy
    canI:
      serviceAccount: n1/deployer
      verb: create
      group: apps
      resource: deployments
      namespace: n1
    allowed: true
  - description: cannot delete pods
    canI:
      serviceAccount: n1/deployer
      verb: delete
      resource: pods
      namespace: n1
    allowed: false
  - description: scheduled on the worker
    apiVersion: v1
    kind: Pod
    namespace: n1
    fieldSelector: spec.nodeName={{worker}}
    jsonpath: '{.items[*].metadata.name}'
    equals: ""
answerRun:
  - node: cp
    run: fix {{worker}}
solution: s
`

const plainProblem = `id: p-1
exam: CKAD
domain: d
title: plain
weight: 1
namespaces: [p1]
cleanup:
  - node: worker2
    run: never runs (no such node)
task: t
checks:
  - description: c
    apiVersion: v1
    kind: ConfigMap
    namespace: p1
    name: cm
answer: |
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm
    namespace: p1
solution: s
`

func TestNodeSteps(t *testing.T) {
	ctx := context.Background()
	ps, err := load(fstest.MapFS{
		"problems/n-1.yaml": {Data: []byte(nodeProblems)},
		"problems/p-1.yaml": {Data: []byte(plainProblem)},
	})
	if err != nil {
		t.Fatal(err)
	}
	fc := newFake()
	nodes := &fakeNodes{exit: map[string]int{"verify demo-cp-1": 1}}
	s := serviceFor(t, ps, target(fc, nodes))

	if sum := s.List(); !sum[0].Nodes || sum[0].Requires != "kubeadm" || sum[1].Nodes {
		t.Errorf("summaries: %+v", sum)
	}

	// 出題: 全問の cleanup (無いノードの手順は飛ばす) → マニフェスト → nodeSetup。{{...}} はノードの名前になる
	if err := s.Setup(ctx, "demo", "n-1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(nodes.log, " | "); got != "cp: uncordon demo-worker-1 | worker: break demo-worker-1" {
		t.Errorf("node steps: %s", got)
	}
	if fc.objs[key("v1", "Namespace", "", "n1")] == nil {
		t.Error("the setup manifest must be applied")
	}

	// 採点: ノードのコマンドは終了コード、権限は ServiceAccount のユーザーとグループで問い合わせる
	fc.allowed = map[string]bool{"create deployments": true}
	r, err := s.Check(ctx, "demo", "n-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed != 3 || r.Checks[0].Passed || r.Checks[0].Message != "ノードでの確認が通りません" || !r.Checks[1].Passed || !r.Checks[2].Passed {
		t.Errorf("check: %+v", r)
	}
	if want := "system:serviceaccount:n1:deployer system:serviceaccounts,system:serviceaccounts:n1,system:authenticated create apps/deployments n1"; fc.canICalls[0] != want {
		t.Errorf("canI call: %q", fc.canICalls[0])
	}
	fc.allowed["delete pods"] = true // 余計な権限があると不合格
	if r, _ := s.Check(ctx, "demo", "n-1"); r.Checks[2].Passed || r.Checks[2].Message != "権限が違います" {
		t.Errorf("extra permission must fail: %+v", r.Checks[2])
	}
	nodes.exit["verify demo-cp-1"] = 0
	fc.allowed["delete pods"] = false
	if r, _ := s.Check(ctx, "demo", "n-1"); r.Passed != r.Total || r.Score != 4 {
		t.Errorf("all pass: %+v", r)
	}

	// 模範解答
	nodes.log = nil
	if err := s.Solve(ctx, "demo", "n-1"); err != nil || strings.Join(nodes.log, "|") != "cp: fix demo-worker-1" {
		t.Errorf("solve: %v %v", err, nodes.log)
	}
	// ノードのコマンドが失敗したら、出力つきのエラー
	nodes.exit["break demo-worker-1"] = 2
	if err := s.Setup(ctx, "demo", "n-1"); err == nil || !strings.Contains(err.Error(), "exited with 2") {
		t.Errorf("failing node step: %v", err)
	}

	// ノードに入れないクラスタ、ディストリが違うクラスタでは出せない
	for name, tg := range map[string]*Target{
		"no ssh":    target(fc, nil),
		"k3s":       {Kube: fc, Distro: "k3s", Nodes: map[string]string{"cp": "x", "worker": "y"}, Run: nodes.run},
		"no worker": {Kube: fc, Distro: "kubeadm", Nodes: map[string]string{"cp": "x"}, Run: nodes.run},
	} {
		s2 := serviceFor(t, ps, tg)
		if err := s2.Setup(ctx, "demo", "n-1"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: want ErrUnsupported, got %v", name, err)
		}
	}
	// ノードを使わない問題は、ノードに入れないクラスタでも出せる (cleanup は動かさない)
	nodes.log = nil
	s3 := serviceFor(t, ps, target(fc, nil))
	if err := s3.Setup(ctx, "demo", "p-1"); err != nil || len(nodes.log) != 0 {
		t.Errorf("plain problem without ssh: %v %v", err, nodes.log)
	}
}

func TestFindNullArray(t *testing.T) {
	// EndpointSlice がまだ endpoints を持たない (null) ときも、エラーではなく「値が無い」
	obj := map[string]any{"items": []any{map[string]any{"endpoints": nil}}}
	got, err := find(obj, `{.items[*].endpoints[?(@.conditions.ready==true)].targetRef.kind}`)
	if err != nil || got != "" {
		t.Errorf("got %q %v", got, err)
	}
}

const upgradeProblem = `id: u-1
exam: CKA
domain: d
title: upgrade
weight: 5
requires: kubeadm
resetSnapshot: true
needsOlderVersion: true
nodeSetup:
  - node: cp
    run: check {{nextVersion}}
task: t
checks:
  - description: kubelet is new
    apiVersion: v1
    kind: Node
    name: '{{cp}}'
    jsonpath: '{.status.nodeInfo.kubeletVersion}'
    contains: '{{nextVersion}}.'
answerRun:
  - node: cp
    run: upgrade {{version}} to {{nextVersion}}
solution: s
`

type fakeSnapshots struct {
	have map[string]bool
	log  []string
}

func (f *fakeSnapshots) Has(_ context.Context, name string) (bool, error) { return f.have[name], nil }
func (f *fakeSnapshots) Take(_ context.Context, name string) error {
	f.log = append(f.log, "take "+name)
	f.have[name] = true
	return nil
}
func (f *fakeSnapshots) Restore(_ context.Context, name string) error {
	f.log = append(f.log, "restore "+name)
	return nil
}

func TestResetSnapshotAndVersions(t *testing.T) {
	ctx := context.Background()
	ps, err := load(fstest.MapFS{
		"problems/u-1.yaml": {Data: []byte(upgradeProblem)},
		"problems/p-1.yaml": {Data: []byte(plainProblem)},
	})
	if err != nil {
		t.Fatal(err)
	}
	fc := newFake()
	nodes := &fakeNodes{}
	snaps := &fakeSnapshots{have: map[string]bool{}}
	tg := target(fc, nodes)
	tg.Version, tg.Snapshots = "v1.36", snaps
	s := serviceFor(t, ps, tg)

	// 1 回目はスナップショットを取り、2 回目からは戻す。{{version}} / {{nextVersion}} は作ったときのバージョンから
	for range 2 {
		if err := s.Setup(ctx, "demo", "u-1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(snaps.log, ","); got != "take practice-u-1,restore practice-u-1" {
		t.Errorf("snapshots: %s", got)
	}
	if nodes.log[0] != "cp: check v1.37" {
		t.Errorf("node setup: %v", nodes.log)
	}
	if err := s.Solve(ctx, "demo", "u-1"); err != nil || nodes.log[len(nodes.log)-1] != "cp: upgrade v1.36 to v1.37" {
		t.Errorf("solve: %v %v", err, nodes.log)
	}
	fc.objs[key("v1", "Node", "", "demo-cp-1")] = map[string]any{"status": map[string]any{"nodeInfo": map[string]any{"kubeletVersion": "v1.37.2"}}}
	if r, err := s.Check(ctx, "demo", "u-1"); err != nil || r.Passed != 1 {
		t.Errorf("contains must expand {{nextVersion}}: %+v %v", r, err)
	}

	// 出せるかどうかの一覧
	sum, err := s.ListFor(ctx, "demo")
	if err != nil || !*sum[0].Available || !*sum[1].Available {
		t.Errorf("available: %+v %v", sum, err)
	}
	for name, mod := range map[string]func(*Target){
		"stable":       func(t *Target) { t.Version = "stable" },
		"no snapshots": func(t *Target) { t.Snapshots = nil },
		"k3s":          func(t *Target) { t.Distro = "k3s" },
	} {
		tg2 := *tg
		mod(&tg2)
		s2 := serviceFor(t, ps, &tg2)
		sum, _ := s2.ListFor(ctx, "demo")
		var u Summary
		for _, x := range sum {
			if x.ID == "u-1" {
				u = x
			}
		}
		if *u.Available || u.Reason == "" {
			t.Errorf("%s: must be unavailable with a reason: %+v", name, u)
		}
		if err := s2.Setup(ctx, "demo", "u-1"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: setup: %v", name, err)
		}
	}
}
