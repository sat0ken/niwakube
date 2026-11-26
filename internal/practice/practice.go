// Package practice は CKA / CKAD の練習問題を出し、クラスタの状態を見て採点する。
//
// 問題は problems/*.yaml に 1 問 1 ファイルで書き、バイナリに埋め込む。本物の試験問題は使わず、
// 公開されている出題範囲 (CNCF のカリキュラム) に沿ったオリジナルの問題にする。
//
// 1 問の流れ:
//  1. Setup: 問題の Namespace を消して作り直し、setup のマニフェストを server-side apply する
//  2. 受験者がターミナルの kubectl で解く
//  3. Check: checks に書いた条件を、クラスタのオブジェクトを読んで確かめる (シェルは動かさない)
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

	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/yaml"
)

//go:embed problems/*.yaml
var problemFS embed.FS

// ErrNotFound は問題が無いときのエラー。
var ErrNotFound = errors.New("problem not found")

// Problem は 1 問。
type Problem struct {
	ID     string `json:"id"`
	Exam   string `json:"exam"`   // CKA | CKAD
	Domain string `json:"domain"` // 出題範囲の分野
	Title  string `json:"title"`
	Weight int    `json:"weight"` // 配点
	// Namespaces はこの問題が使う Namespace。準備のたびに消して作り直す (setup で作ること)
	Namespaces []string `json:"namespaces"`
	Setup      string   `json:"setup,omitempty"` // 出題前に適用するマニフェスト
	Task       string   `json:"task"`            // 問題文
	Checks     []Check  `json:"checks"`
	Hint       string   `json:"hint,omitempty"`
	Solution   string   `json:"solution,omitempty"` // 解説と解き方の例
	// Answer は問題を解いた状態にするマニフェスト (CI で、採点が満点になることを確かめるのに使う)
	Answer string `json:"answer,omitempty"`
}

// Check は採点の 1 項目。Name か Selector のどちらかでオブジェクトを選び、JSONPath の結果を比べる。
// Selector のときは {"items": [...]} に対して JSONPath を評価する。
// Equals / Contains / Exists がどれも無ければ、オブジェクトがあれば合格。
type Check struct {
	Description string  `json:"description"`
	APIVersion  string  `json:"apiVersion"`
	Kind        string  `json:"kind"`
	Namespace   string  `json:"namespace,omitempty"`
	Name        string  `json:"name,omitempty"`
	Selector    string  `json:"selector,omitempty"`
	JSONPath    string  `json:"jsonpath,omitempty"`
	Equals      *string `json:"equals,omitempty"`
	Contains    string  `json:"contains,omitempty"`
	Exists      bool    `json:"exists,omitempty"`
}

// Summary は一覧に出す項目 (問題文や解答は含めない)。
type Summary struct {
	ID     string `json:"id"`
	Exam   string `json:"exam"`
	Domain string `json:"domain"`
	Title  string `json:"title"`
	Weight int    `json:"weight"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

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
	case len(p.Namespaces) == 0:
		return errors.New("namespaces is required (the problem's own namespaces, recreated on setup)")
	case len(p.Checks) == 0:
		return errors.New("checks is required")
	}
	for _, ns := range p.Namespaces {
		if slices.Contains(protectedNamespaces, ns) {
			return fmt.Errorf("namespace %q cannot be used by a problem", ns)
		}
	}
	for i, c := range p.Checks {
		if c.Description == "" || c.APIVersion == "" || c.Kind == "" {
			return fmt.Errorf("check %d: description, apiVersion and kind are required", i+1)
		}
		if (c.Name == "") == (c.Selector == "") {
			return fmt.Errorf("check %d: set exactly one of name and selector", i+1)
		}
		if c.JSONPath != "" {
			if _, err := parseJSONPath(c.JSONPath); err != nil {
				return fmt.Errorf("check %d: jsonpath: %w", i+1, err)
			}
		} else if c.Equals != nil || c.Contains != "" || c.Exists {
			return fmt.Errorf("check %d: equals/contains/exists need a jsonpath", i+1)
		}
	}
	return nil
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

// Cluster は問題を出すクラスタの操作 (kube.Client が満たす。テストで差し替える)。
type Cluster interface {
	Apply(ctx context.Context, manifest string) error
	Get(ctx context.Context, apiVersion, kind, ns, name string) (map[string]any, error)
	List(ctx context.Context, apiVersion, kind, ns, selector string) (map[string]any, error)
	DeleteNamespace(ctx context.Context, name string) error
	NamespaceExists(ctx context.Context, name string) (bool, error)
}

// Service は問題の一覧と、準備・採点。
type Service struct {
	problems []*Problem
	byID     map[string]*Problem
	connect  func(ctx context.Context, cluster string) (Cluster, error)
	poll     time.Duration
	nsWait   time.Duration
}

// NewService は問題を読み込む。connect はクラスタ名から操作用のクライアントを作る。
func NewService(connect func(ctx context.Context, cluster string) (Cluster, error)) (*Service, error) {
	ps, err := Load()
	if err != nil {
		return nil, err
	}
	return newService(ps, connect), nil
}

func newService(ps []*Problem, connect func(ctx context.Context, cluster string) (Cluster, error)) *Service {
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
		out = append(out, Summary{ID: p.ID, Exam: p.Exam, Domain: p.Domain, Title: p.Title, Weight: p.Weight})
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

// Setup は問題の Namespace を消して (消え終わるのを待って) から、setup を適用する。
// 何度呼んでも同じ「出題直後」の状態になる (やり直しにも使う)。
func (s *Service) Setup(ctx context.Context, cluster, id string) error {
	p, err := s.Get(id)
	if err != nil {
		return err
	}
	c, err := s.connect(ctx, cluster)
	if err != nil {
		return err
	}
	for _, ns := range p.Namespaces {
		if err := c.DeleteNamespace(ctx, ns); err != nil {
			return fmt.Errorf("delete namespace %s: %w", ns, err)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, s.nsWait)
	defer cancel()
	for _, ns := range p.Namespaces {
		for {
			exists, err := c.NamespaceExists(wctx, ns)
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
	if strings.TrimSpace(p.Setup) == "" {
		return nil
	}
	if err := c.Apply(ctx, p.Setup); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	return nil
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
	c, err := s.connect(ctx, cluster)
	if err != nil {
		return nil, err
	}
	r := &Result{ID: p.ID, Weight: p.Weight, Total: len(p.Checks)}
	for _, chk := range p.Checks {
		ok, msg, err := evaluate(ctx, c, chk)
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
func evaluate(ctx context.Context, c Cluster, chk Check) (bool, string, error) {
	var obj map[string]any
	var err error
	if chk.Name != "" {
		obj, err = c.Get(ctx, chk.APIVersion, chk.Kind, chk.Namespace, chk.Name)
	} else {
		obj, err = c.List(ctx, chk.APIVersion, chk.Kind, chk.Namespace, chk.Selector)
	}
	if err != nil {
		return false, "", err
	}
	if obj == nil {
		return false, fmt.Sprintf("%s %s が見つかりません", chk.Kind, chk.Name), nil
	}
	if chk.JSONPath == "" {
		return true, "", nil
	}
	got, err := find(obj, chk.JSONPath)
	if err != nil {
		return false, "", err
	}
	switch {
	case chk.Equals != nil:
		if got != strings.TrimSpace(*chk.Equals) {
			return false, "値が違います", nil
		}
	case chk.Contains != "":
		if !strings.Contains(got, chk.Contains) {
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
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}
