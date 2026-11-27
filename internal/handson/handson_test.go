package handson

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRender(t *testing.T) {
	src := "# Cilium\n\n" +
		"- [KubeVirtのインストール確認](#kubevirtのインストール確認)\n\n" +
		"## KubeVirtのインストール確認\n\n" +
		"まずは [chapter_cluster-create](../chapter_cluster-create/) と [CMP](./README_cmp.md) と [values](helm/values.yaml)、[外](https://cilium.io)。\n\n" +
		"![](./image/ch1.png)\n\n" +
		"<img src=\"image/loki.png\" width=600 onerror=\"alert(1)\">\n" +
		"<img src=\"https://grafana.com/x.svg\" width=700>\n\n" +
		"<details>\n<summary>答え</summary>\n\nkubectl get pods\n\n</details>\n\n" +
		"<script>alert(1)</script>\n\n" +
		"[悪い](javascript:alert(1))\n\n" +
		"```bash\nkubectl get pods -n handson | grep '<none>'\n```\n\n" +
		"| a | b |\n|---|---|\n| 1 | 2 |\n"
	out, err := Render([]byte(src), "chapter_argocd/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<h2 id="md-kubevirtのインストール確認">`,
		`<a href="#md-kubevirtのインストール確認">`,
		`<a href="#doc:chapter_cluster-create/README.md">`,
		`<a href="#doc:chapter_argocd/README_cmp.md">`,
		`<a href="https://github.com/cloudnativedaysjp/cnd-handson/blob/main/chapter_argocd/helm/values.yaml" target="_blank" rel="noopener">`,
		`<a href="https://cilium.io" target="_blank" rel="noopener">`,
		`<img src="/api/handson/files/chapter_argocd/image/ch1.png"`,
		`<img src="/api/handson/files/chapter_argocd/image/loki.png" alt="" loading="lazy" width="600">`,
		`<a href="https://grafana.com/x.svg" target="_blank" rel="noopener">[画像: ]</a>`,
		`<details>`, `<summary>答え</summary>`,
		`<code class="language-bash">kubectl get pods -n handson | grep &#39;&lt;none&gt;&#39;`,
		`<table>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	for _, bad := range []string{"<script", "alert(1)</", "onerror", "javascript:"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q\n%s", bad, out)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"KubeVirtのインストール確認":     "kubevirtのインストール確認",
		"virtctl CLIの確認":        "virtctl-cliの確認",
		"Step 1. Gateway (API)": "step-1-gateway-api",
		"仮想マシン用サービスの作成":         "仮想マシン用サービスの作成",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServiceFetchAndCache(t *testing.T) {
	var hits atomic.Int32
	down := atomic.Bool{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/chapter_cilium/README.md":
			w.Write([]byte("# Cilium\n\n![](image/a.png)\n"))
		case "/chapter_cilium/image/a.png":
			w.Write([]byte("PNG"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s := New()
	s.Base = ts.URL + "/"
	ctx := context.Background()

	p, err := s.Page(ctx, "chapter_cilium", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.File != "README.md" || !strings.Contains(p.HTML, "/api/handson/files/chapter_cilium/image/a.png") || !strings.HasSuffix(p.Source, "/blob/main/chapter_cilium/README.md") {
		t.Errorf("page: %+v", p)
	}
	if _, err := s.Page(ctx, "chapter_cilium", ""); err != nil || hits.Load() != 1 {
		t.Errorf("second read must come from the cache: hits %d, %v", hits.Load(), err)
	}
	// 古くなっても、届かなければ覚えているものを返す
	s.mu.Lock()
	c := s.cache["chapter_cilium/README.md"]
	c.at = c.at.Add(-2 * cacheTTL)
	s.cache["chapter_cilium/README.md"] = c
	s.mu.Unlock()
	down.Store(true)
	if _, err := s.Page(ctx, "chapter_cilium", ""); err != nil {
		t.Errorf("stale page must be served when GitHub is down: %v", err)
	}
	down.Store(false)

	data, ct, err := s.File(ctx, "chapter_cilium/image/a.png")
	if err != nil || string(data) != "PNG" || ct != "image/png" {
		t.Errorf("file: %q %q %v", data, ct, err)
	}
	for _, bad := range []string{"chapter_cilium/README.md", "../etc/passwd.png", "chapter_cilium/../../x.png", "LICENSE.png", "chapter_cilium/a.sh"} {
		if _, _, err := s.File(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", bad, err)
		}
	}
	for _, bad := range [][2]string{{"chapter_nope", ""}, {"chapter_cilium", "README_x.md"}, {"chapter_cilium", "../LICENSE"}} {
		if _, err := s.Page(ctx, bad[0], bad[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("%v: want ErrNotFound, got %v", bad, err)
		}
	}
}

func TestChapters(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Chapters {
		if seen[c.ID] || !chapterDirRe.MatchString(c.ID) || len(c.Docs) == 0 || c.Title == "" {
			t.Errorf("bad chapter %+v", c)
		}
		seen[c.ID] = true
	}
	if c, ok := find("chapter_cluster-create"); !ok || len(c.Notes) == 0 || !strings.Contains(strings.Join(c.Notes, ""), "helmfile sync") {
		t.Error("chapter_cluster-create must warn about helmfile sync")
	}
}
