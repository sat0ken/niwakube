// Package practice は CKA / CKAD の練習問題を出し、クラスタの状態を見て採点する。
//
// 問題は problems/*.yaml に 1 問 1 ファイルで書き、バイナリに埋め込む。本物の試験問題は使わず、
// 公開されている出題範囲 (CNCF のカリキュラム) に沿ったオリジナルの問題にする。
//
// 1 問の流れ:
//  1. Setup: ノードの後片付け (全問の cleanup) → 問題の Namespace を作り直す → setup のマニフェストを
//     server-side apply → nodeSetup をノードで動かす
//  2. 受験者がターミナルの kubectl (と ssh) で解く
//  3. Check: checks の条件を確かめる。オブジェクトの値、ノードでのコマンドの終了コード、権限 (can-i) の 3 種類
//
// ノードを使う問題 (CKA) は、ノードに SSH で入れるクラスタ (kubeadm / k3s で、ノードの鍵があるもの) でだけ出せる。
// ノードで動かすコマンドはリポジトリにある問題のファイルに書いたものだけ。
package practice

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sat0ken/niwakube/internal/nodeexec"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/yaml"
)

//go:embed problems/*.yaml
var problemFS embed.FS

var (
	// ErrNotFound は問題が無いときのエラー。
	ErrNotFound = errors.New("problem not found")
	// ErrUnsupported はこのクラスタでは出せない問題のときのエラー (ディストリが違う、ノードに入れない)。
	ErrUnsupported = errors.New("this problem cannot be used on this cluster")
)

// Problem は 1 問。
type Problem struct {
	ID     string `json:"id"`
	Exam   string `json:"exam"`   // CKA | CKAD
	Domain string `json:"domain"` // 出題範囲の分野
	Title  string `json:"title"`
	Weight int    `json:"weight"` // 配点
	// Requires は問題を出せるディストリ (例: kubeadm)。空ならどれでもよい
	Requires string `json:"requires,omitempty"`
	// Namespaces はこの問題が使う Namespace。準備のたびに消して作り直す (setup で作ること)
	Namespaces []string `json:"namespaces,omitempty"`
	Setup      string   `json:"setup,omitempty"`     // 出題前に適用するマニフェスト
	NodeSetup  []Step   `json:"nodeSetup,omitempty"` // 出題前にノードで動かすコマンド (マニフェストの後)
	// Cleanup はこの問題がノードに残す変更 (taint、cordon、止めたサービスなど) を元に戻すコマンド。
	// どの問題を出すときも、全問の cleanup を先に動かす (ほかの問題の邪魔をしないように)。何度動かしてもよいこと
	Cleanup  []Step  `json:"cleanup,omitempty"`
	Task     string  `json:"task"` // 問題文
	Checks   []Check `json:"checks"`
	Hint     string  `json:"hint,omitempty"`
	Solution string  `json:"solution,omitempty"` // 解説と解き方の例
	// Answer と AnswerRun は問題を解いた状態にする (CI で満点になることを確かめる。Web UI の「模範解答を当てる」)
	Answer    string `json:"answer,omitempty"`
	AnswerRun []Step `json:"answerRun,omitempty"`
	// ResetSnapshot が true の問題は、元に戻せない変更 (アップグレードなど) をさせる。最初の出題でクラスタの
	// スナップショット (practice-<id>) を取り、2 回目からはそれに戻してから出題する
	ResetSnapshot bool `json:"resetSnapshot,omitempty"`
	// NeedsOlderVersion が true の問題は、マイナーバージョンを指定して作ったクラスタ (v1.36 など) が要る。
	// {{version}} (v1.36) と {{nextVersion}} (v1.37) が使える
	NeedsOlderVersion bool `json:"needsOlderVersion,omitempty"`
}

// Step はノードで動かすコマンド。Node は cp / worker (1 台目)、cp2 / worker2 ... (2 台目以降)。
// Run は bash で動かし、ubuntu ユーザーなので root が要るものは sudo を付ける。
type Step struct {
	Node string `json:"node"`
	Run  string `json:"run"`
}

// Check は採点の 1 項目。次のどれか 1 つ:
//   - オブジェクト: Name か Selector (と FieldSelector) で選び、JSONPath の結果を比べる。
//     Selector のときは {"items": [...]} に対して評価する。Equals / Contains / Exists がどれも無ければ、あれば合格
//   - ノード: Node で Run を動かし、終了コードが 0 なら合格
//   - 権限: CanI の操作を、その ServiceAccount ができる (Allowed: true) / できない (false) なら合格
//
// Name・Selector・FieldSelector・JSONPath・Run の {{cp}} や {{worker}} は、ノードの名前に置き換える。
type Check struct {
	Description   string  `json:"description"`
	APIVersion    string  `json:"apiVersion,omitempty"`
	Kind          string  `json:"kind,omitempty"`
	Namespace     string  `json:"namespace,omitempty"`
	Name          string  `json:"name,omitempty"`
	Selector      string  `json:"selector,omitempty"`
	FieldSelector string  `json:"fieldSelector,omitempty"`
	JSONPath      string  `json:"jsonpath,omitempty"`
	Equals        *string `json:"equals,omitempty"`
	Contains      string  `json:"contains,omitempty"`
	Exists        bool    `json:"exists,omitempty"`
	Node          string  `json:"node,omitempty"`
	Run           string  `json:"run,omitempty"`
	CanI          *CanI   `json:"canI,omitempty"`
	Allowed       *bool   `json:"allowed,omitempty"`
}

// CanI は権限の確認 (kubectl auth can-i --as=system:serviceaccount:<ns>:<name> と同じ)。
type CanI struct {
	ServiceAccount string `json:"serviceAccount"` // <namespace>/<name>
	Verb           string `json:"verb"`
	Group          string `json:"group,omitempty"` // API グループ (Deployment なら apps。core は空)
	Resource       string `json:"resource"`        // 複数形 (pods、deployments)
	Namespace      string `json:"namespace,omitempty"`
}

// Summary は一覧に出す項目 (問題文や解答は含めない)。
type Summary struct {
	ID       string `json:"id"`
	Exam     string `json:"exam"`
	Domain   string `json:"domain"`
	Title    string `json:"title"`
	Weight   int    `json:"weight"`
	Requires string `json:"requires,omitempty"`
	Nodes    bool   `json:"nodes,omitempty"` // ノードに入る問題 (ノードの鍵があるクラスタが要る)
	// Available と Reason は、クラスタを指定して一覧を取ったときだけ入る (そのクラスタで出せるか、出せない理由)
	Available *bool  `json:"available,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nodeRe = regexp.MustCompile(`^(cp|worker)[0-9]*$`)
	saRe   = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9.-]+$`)
	phRe   = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9]+)\s*\}\}`)
	// minorRe はクラスタを作ったときのバージョンの指定 (v1.36、v1.36.2)
	minorRe = regexp.MustCompile(`^v1\.([0-9]+)(\.[0-9]+)?$`)
)

// Load は埋め込んだ問題をすべて読み、形を確かめる。
func Load() ([]*Problem, error) {
	return load(problemFS)
}

func load(fsys fs.FS) ([]*Problem, error) {
	files, err := fs.Glob(fsys, "problems/*.yaml")
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	seen := map[string]bool{}
	var out []*Problem
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		var p Problem
		if err := yaml.UnmarshalStrict(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if want := strings.TrimSuffix(path.Base(f), ".yaml"); p.ID != want {
			return nil, fmt.Errorf("%s: id %q must match the file name", f, p.ID)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("%s: duplicate id %q", f, p.ID)
		}
		seen[p.ID] = true
		out = append(out, &p)
	}
	return out, nil
}

func (p *Problem) validate() error {
	switch {
	case !idRe.MatchString(p.ID):
		return fmt.Errorf("bad id %q", p.ID)
	case p.Exam != "CKA" && p.Exam != "CKAD":
		return fmt.Errorf("exam must be CKA or CKAD, got %q", p.Exam)
	case p.Title == "" || p.Task == "" || p.Domain == "":
		return errors.New("title, domain and task are required")
	case p.Weight <= 0:
		return errors.New("weight must be positive")
	case len(p.Checks) == 0:
		return errors.New("checks is required")
	case p.Requires != "" && p.Requires != "k3s" && p.Requires != "kubeadm":
		return fmt.Errorf("requires must be k3s or kubeadm, got %q", p.Requires)
	}
	for _, ns := range p.Namespaces {
		if slices.Contains(protectedNamespaces, ns) {
			return fmt.Errorf("namespace %q cannot be used by a problem", ns)
		}
	}
	for name, steps := range map[string][]Step{"nodeSetup": p.NodeSetup, "cleanup": p.Cleanup, "answerRun": p.AnswerRun} {
		for i, st := range steps {
			if !nodeRe.MatchString(st.Node) || strings.TrimSpace(st.Run) == "" {
				return fmt.Errorf("%s %d: node must be cp/worker(N) and run is required", name, i+1)
			}
		}
	}
	for i, c := range p.Checks {
		if err := c.validate(); err != nil {
			return fmt.Errorf("check %d: %w", i+1, err)
		}
	}
	return nil
}

func (c *Check) validate() error {
	if c.Description == "" {
		return errors.New("description is required")
	}
	kinds := 0
	if c.Kind != "" || c.APIVersion != "" {
		kinds++
	}
	if c.Node != "" || c.Run != "" {
		kinds++
	}
	if c.CanI != nil {
		kinds++
	}
	if kinds != 1 {
		return errors.New("set exactly one of an object (apiVersion/kind), a node command (node/run) or canI")
	}
	switch {
	case c.CanI != nil:
		if !saRe.MatchString(c.CanI.ServiceAccount) || c.CanI.Verb == "" || c.CanI.Resource == "" {
			return errors.New("canI needs serviceAccount (<ns>/<name>), verb and resource")
		}
		if c.Allowed == nil {
			return errors.New("canI needs allowed: true or false")
		}
	case c.Node != "" || c.Run != "":
		if !nodeRe.MatchString(c.Node) || strings.TrimSpace(c.Run) == "" {
			return errors.New("a node check needs node (cp/worker(N)) and run")
		}
	default:
		if c.APIVersion == "" || c.Kind == "" {
			return errors.New("apiVersion and kind are required")
		}
		if (c.Name == "") == (c.Selector == "" && c.FieldSelector == "") {
			return errors.New("set exactly one of name and selector/fieldSelector")
		}
		if c.JSONPath != "" {
			if _, err := parseJSONPath(c.JSONPath); err != nil {
				return fmt.Errorf("jsonpath: %w", err)
			}
		} else if c.Equals != nil || c.Contains != "" || c.Exists {
			return errors.New("equals/contains/exists need a jsonpath")
		}
	}
	if c.Allowed != nil && c.CanI == nil {
		return errors.New("allowed is only for canI")
	}
	return nil
}

// usesNodes はノードに入る (ノードの名前を使う) 問題か。cleanup は後片付けなので数えない。
func (p *Problem) usesNodes() bool {
	if len(p.NodeSetup)+len(p.AnswerRun) > 0 || nodePh(p.Answer) || nodePh(p.Setup) {
		return true
	}
	for _, c := range p.Checks {
		eq := ""
		if c.Equals != nil {
			eq = *c.Equals
		}
		if c.Node != "" || nodePh(c.Name+c.Selector+c.FieldSelector+c.JSONPath+eq+c.Contains) {
			return true
		}
	}
	return false
}

// nodePh は s にノードの名前の置き換え ({{cp}}、{{worker}} など。{{version}} は除く) があるか。
func nodePh(s string) bool {
	for _, m := range phRe.FindAllStringSubmatch(s, -1) {
		if m[1] != "version" && m[1] != "nextVersion" {
			return true
		}
	}
	return false
}

// protectedNamespaces は問題が消してはいけない Namespace。
var protectedNamespaces = []string{"default", "kube-system", "kube-public", "kube-node-lease", "local-path-storage"}

func parseJSONPath(expr string) (*jsonpath.JSONPath, error) {
	j := jsonpath.New("check").AllowMissingKeys(true)
	if err := j.Parse(expr); err != nil {
		return nil, err
	}
	return j, nil
}

// Cluster は問題を出すクラスタの Kubernetes の操作 (kube.Client が満たす。テストで差し替える)。
type Cluster interface {
	Apply(ctx context.Context, manifest string) error
	Get(ctx context.Context, apiVersion, kind, ns, name string) (map[string]any, error)
	List(ctx context.Context, apiVersion, kind, ns, selector, fieldSelector string) (map[string]any, error)
	DeleteNamespace(ctx context.Context, name string) error
	NamespaceExists(ctx context.Context, name string) (bool, error)
	CanI(ctx context.Context, user string, groups []string, verb, group, resource, ns string) (bool, error)
}

// Target は問題を出すクラスタ。
type Target struct {
	Kube   Cluster
	Distro string
	// Nodes は別名 (cp, worker, worker2, ...) からノードの名前。Run と一緒に、ノードに入れるクラスタだけ
	Nodes map[string]string
	// Run は別名のノードで script を動かす。ノードに入れないクラスタは nil
	Run func(ctx context.Context, node, script string) (*nodeexec.Result, error)
	// Version はクラスタを作ったときのバージョンの指定 (v1.36、stable など)
	Version string
	// Snapshots はクラスタのスナップショット。nil なら使えない
	Snapshots Snapshots
}

// Snapshots はクラスタのスナップショットの操作。Take と Restore は終わるまで待つ。
type Snapshots interface {
	Has(ctx context.Context, name string) (bool, error)
	Take(ctx context.Context, name string) error
	Restore(ctx context.Context, name string) error
}

// versions は {{version}} と {{nextVersion}} の値を返す (マイナーバージョンを指定して作ったクラスタだけ)。
func (t *Target) versions() (cur, next string, ok bool) {
	m := minorRe.FindStringSubmatch(t.Version)
	if m == nil {
		return "", "", false
	}
	var minor int
	fmt.Sscanf(m[1], "%d", &minor)
	return fmt.Sprintf("v1.%d", minor), fmt.Sprintf("v1.%d", minor+1), true
}

// expand は {{cp}} や {{worker}} をノードの名前に置き換える。知らない別名があればエラー。
func (t *Target) expand(s string) (string, error) {
	var missing string
	cur, next, hasVersion := t.versions()
	out := phRe.ReplaceAllStringFunc(s, func(m string) string {
		alias := phRe.FindStringSubmatch(m)[1]
		switch {
		case alias == "version" && hasVersion:
			return cur
		case alias == "nextVersion" && hasVersion:
			return next
		}
		if name, ok := t.Nodes[alias]; ok {
			return name
		}
		missing = alias
		return m
	})
	if missing != "" {
		return "", fmt.Errorf("the cluster has no node %q: %w", missing, ErrUnsupported)
	}
	return out, nil
}

func (t *Target) runSteps(ctx context.Context, what string, steps []Step, skipMissing bool) error {
	for _, st := range steps {
		if _, ok := t.Nodes[st.Node]; !ok {
			if skipMissing {
				continue
			}
			return fmt.Errorf("%s: the cluster has no node %q: %w", what, st.Node, ErrUnsupported)
		}
		script, err := t.expand(st.Run)
		if err != nil {
			if skipMissing {
				continue
			}
			return err
		}
		res, err := t.Run(ctx, st.Node, script)
		if err != nil {
			return fmt.Errorf("%s on %s: %w", what, st.Node, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s on %s exited with %d: %s", what, st.Node, res.ExitCode, strings.TrimSpace(res.Output))
		}
	}
	return nil
}

// Service は問題の一覧と、準備・採点。
type Service struct {
	problems []*Problem
	byID     map[string]*Problem
	connect  func(ctx context.Context, cluster string) (*Target, error)
	poll     time.Duration
	nsWait   time.Duration
}

// NewService は問題を読み込む。connect はクラスタ名から問題を出す相手を作る。
func NewService(connect func(ctx context.Context, cluster string) (*Target, error)) (*Service, error) {
	ps, err := Load()
	if err != nil {
		return nil, err
	}
	return newService(ps, connect), nil
}

func newService(ps []*Problem, connect func(ctx context.Context, cluster string) (*Target, error)) *Service {
	s := &Service{problems: ps, byID: map[string]*Problem{}, connect: connect, poll: 2 * time.Second, nsWait: 3 * time.Minute}
	for _, p := range ps {
		s.byID[p.ID] = p
	}
	return s
}

// List は問題の一覧 (試験、分野、ID の順)。
func (s *Service) List() []Summary {
	out := make([]Summary, 0, len(s.problems))
	for _, p := range s.problems {
		out = append(out, Summary{ID: p.ID, Exam: p.Exam, Domain: p.Domain, Title: p.Title, Weight: p.Weight, Requires: p.Requires, Nodes: p.usesNodes()})
	}
	slices.SortStableFunc(out, func(a, b Summary) int {
		return strings.Compare(a.Exam+"\x00"+a.Domain+"\x00"+a.ID, b.Exam+"\x00"+b.Domain+"\x00"+b.ID)
	})
	return out
}

// Get は問題を返す。
func (s *Service) Get(id string) (*Problem, error) {
	p, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	return p, nil
}

// target は問題を出す相手を作り、その問題を出せるか確かめる。
func (s *Service) target(ctx context.Context, cluster string, p *Problem) (*Target, error) {
	t, err := s.connect(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if reason := unsupported(t, p); reason != "" {
		return nil, fmt.Errorf("%s: %s: %w", p.ID, reason, ErrUnsupported)
	}
	return t, nil
}

// unsupported はクラスタ t で問題 p を出せない理由を返す (出せるなら空)。
func unsupported(t *Target, p *Problem) string {
	switch {
	case p.Requires != "" && t.Distro != p.Requires:
		return fmt.Sprintf("%s のクラスタが要ります (このクラスタは %s)", p.Requires, t.Distro)
	case p.usesNodes() && t.Run == nil:
		return "ノードに SSH できるクラスタが要ります (v0.4.0 より前に作ったクラスタにはノードの鍵が無いので、作り直してください)"
	case p.ResetSnapshot && t.Snapshots == nil:
		return "スナップショットが使えません"
	}
	if _, _, ok := t.versions(); p.NeedsOlderVersion && !ok {
		v := t.Version
		if v == "" {
			v = "stable"
		}
		return fmt.Sprintf("最新より 1 つ古いマイナーバージョンを指定して作ったクラスタが要ります (例: v1.36。このクラスタは %s)", v)
	}
	return ""
}

// ListFor は問題の一覧に、クラスタ cluster で出せるか (Available) と出せない理由 (Reason) を付けて返す。
func (s *Service) ListFor(ctx context.Context, cluster string) ([]Summary, error) {
	t, err := s.connect(ctx, cluster)
	if err != nil {
		return nil, err
	}
	out := s.List()
	for i := range out {
		reason := unsupported(t, s.byID[out[i].ID])
		ok := reason == ""
		out[i].Available, out[i].Reason = &ok, reason
	}
	return out, nil
}

// resetBySnapshot は、最初の出題ならスナップショットを取り、2 回目からはそれに戻す。
func resetBySnapshot(ctx context.Context, t *Target, p *Problem) error {
	name := "practice-" + p.ID
	has, err := t.Snapshots.Has(ctx, name)
	if err != nil {
		return err
	}
	if has {
		if err := t.Snapshots.Restore(ctx, name); err != nil {
			return fmt.Errorf("restore snapshot %s: %w", name, err)
		}
		return nil
	}
	if err := t.Snapshots.Take(ctx, name); err != nil {
		return fmt.Errorf("take snapshot %s: %w", name, err)
	}
	return nil
}

// Setup はノードの後片付けをし、問題の Namespace を消して (消え終わるのを待って) から、setup と nodeSetup を適用する。
// 何度呼んでも同じ「出題直後」の状態になる (やり直しにも使う)。
func (s *Service) Setup(ctx context.Context, cluster, id string) error {
	p, err := s.Get(id)
	if err != nil {
		return err
	}
	t, err := s.target(ctx, cluster, p)
	if err != nil {
		return err
	}
	// ほかの問題がノードに残した変更を戻す (このクラスタで出せる問題の分だけ。無いノードの手順は飛ばす)
	if t.Run != nil {
		for _, q := range s.problems {
			if len(q.Cleanup) == 0 || (q.Requires != "" && q.Requires != t.Distro) {
				continue
			}
			if err := t.runSteps(ctx, q.ID+" cleanup", q.Cleanup, true); err != nil {
				return err
			}
		}
	}
	// 元に戻せない変更をさせる問題は、スナップショットで出題前の状態に戻す (初回は取る)。
	// 後片付けのあとに取るので、ほかの問題の変更 (cordon など) は入らない
	if p.ResetSnapshot {
		if err := resetBySnapshot(ctx, t, p); err != nil {
			return err
		}
	}
	for _, ns := range p.Namespaces {
		if err := t.Kube.DeleteNamespace(ctx, ns); err != nil {
			return fmt.Errorf("delete namespace %s: %w", ns, err)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, s.nsWait)
	defer cancel()
	for _, ns := range p.Namespaces {
		for {
			exists, err := t.Kube.NamespaceExists(wctx, ns)
			if err != nil {
				return fmt.Errorf("namespace %s: %w", ns, err)
			}
			if !exists {
				break
			}
			select {
			case <-wctx.Done():
				return fmt.Errorf("namespace %s is still terminating: %w", ns, wctx.Err())
			case <-time.After(s.poll):
			}
		}
	}
	if strings.TrimSpace(p.Setup) != "" {
		manifest, err := t.expand(p.Setup)
		if err != nil {
			return err
		}
		if err := t.Kube.Apply(ctx, manifest); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}
	return t.runSteps(ctx, "nodeSetup", p.NodeSetup, false)
}

// Solve は模範解答をクラスタに当てる (answer のマニフェストと answerRun)。
func (s *Service) Solve(ctx context.Context, cluster, id string) error {
	p, err := s.Get(id)
	if err != nil {
		return err
	}
	t, err := s.target(ctx, cluster, p)
	if err != nil {
		return err
	}
	if strings.TrimSpace(p.Answer) != "" {
		manifest, err := t.expand(p.Answer)
		if err != nil {
			return err
		}
		if err := t.Kube.Apply(ctx, manifest); err != nil {
			return fmt.Errorf("answer: %w", err)
		}
	}
	return t.runSteps(ctx, "answerRun", p.AnswerRun, false)
}

// CheckResult は採点の 1 項目の結果。答えが分かってしまうので、実際の値は返さない。
type CheckResult struct {
	Description string `json:"description"`
	Passed      bool   `json:"passed"`
	Message     string `json:"message,omitempty"` // 不合格の理由 ("見つかりません" など)
}

// Result は 1 問の採点結果。Score は配点 × 合格した項目の割合 (切り捨て)。
type Result struct {
	ID     string        `json:"id"`
	Score  int           `json:"score"`
	Weight int           `json:"weight"`
	Passed int           `json:"passed"`
	Total  int           `json:"total"`
	Checks []CheckResult `json:"checks"`
}

// Check はクラスタの今の状態で採点する。
func (s *Service) Check(ctx context.Context, cluster, id string) (*Result, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	t, err := s.target(ctx, cluster, p)
	if err != nil {
		return nil, err
	}
	r := &Result{ID: p.ID, Weight: p.Weight, Total: len(p.Checks)}
	for _, chk := range p.Checks {
		ok, msg, err := evaluate(ctx, t, chk)
		if err != nil {
			return nil, fmt.Errorf("check %q: %w", chk.Description, err)
		}
		if ok {
			r.Passed++
		}
		r.Checks = append(r.Checks, CheckResult{Description: chk.Description, Passed: ok, Message: msg})
	}
	r.Score = p.Weight * r.Passed / r.Total
	return r, nil
}

// evaluate は 1 項目を確かめる。クラスタに届かないなどはエラー、条件を満たさないのは (false, 理由)。
func evaluate(ctx context.Context, t *Target, chk Check) (bool, string, error) {
	switch {
	case chk.CanI != nil:
		ns, name, _ := strings.Cut(chk.CanI.ServiceAccount, "/")
		user := "system:serviceaccount:" + ns + ":" + name
		groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"}
		allowed, err := t.Kube.CanI(ctx, user, groups, chk.CanI.Verb, chk.CanI.Group, chk.CanI.Resource, chk.CanI.Namespace)
		if err != nil {
			return false, "", err
		}
		if allowed != *chk.Allowed {
			return false, "権限が違います", nil
		}
		return true, "", nil
	case chk.Node != "":
		script, err := t.expand(chk.Run)
		if err != nil {
			return false, "", err
		}
		res, err := t.Run(ctx, chk.Node, script)
		if err != nil {
			return false, "", err
		}
		if res.ExitCode != 0 {
			return false, "ノードでの確認が通りません", nil
		}
		return true, "", nil
	}

	name, err := t.expand(chk.Name)
	if err != nil {
		return false, "", err
	}
	var obj map[string]any
	if name != "" {
		obj, err = t.Kube.Get(ctx, chk.APIVersion, chk.Kind, chk.Namespace, name)
	} else {
		var sel, fsel string
		if sel, err = t.expand(chk.Selector); err != nil {
			return false, "", err
		}
		if fsel, err = t.expand(chk.FieldSelector); err != nil {
			return false, "", err
		}
		obj, err = t.Kube.List(ctx, chk.APIVersion, chk.Kind, chk.Namespace, sel, fsel)
	}
	if err != nil {
		return false, "", err
	}
	if obj == nil {
		return false, fmt.Sprintf("%s %s が見つかりません", chk.Kind, name), nil
	}
	if chk.JSONPath == "" {
		return true, "", nil
	}
	expr, err := t.expand(chk.JSONPath)
	if err != nil {
		return false, "", err
	}
	got, err := find(obj, expr)
	if err != nil {
		return false, "", err
	}
	switch {
	case chk.Equals != nil:
		want, err := t.expand(strings.TrimSpace(*chk.Equals))
		if err != nil {
			return false, "", err
		}
		if got != want {
			return false, "値が違います", nil
		}
	case chk.Contains != "":
		want, err := t.expand(chk.Contains)
		if err != nil {
			return false, "", err
		}
		if !strings.Contains(got, want) {
			return false, "値が違います", nil
		}
	case chk.Exists:
		if got == "" {
			return false, "設定されていません", nil
		}
	}
	return true, "", nil
}

// find は JSONPath の結果を、空白区切りの 1 つの文字列にする (kubectl -o jsonpath と同じ)。
func find(obj map[string]any, expr string) (string, error) {
	j, err := parseJSONPath(expr)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := j.Execute(&b, obj); err != nil {
		// 式は読み込むときに確かめてある。ここでの失敗は、まだ値が無い (フィルターする配列が null など) ということなので、
		// 「値が無い」(不合格) として扱う
		return "", nil
	}
	return strings.TrimSpace(b.String()), nil
}
