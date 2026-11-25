package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI は discovery (ConfigMap だけ) と server-side apply を受け付ける最小限の API サーバー。
type fakeAPI struct {
	mu      sync.Mutex
	patches []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api":
		fmt.Fprint(w, `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"x"}]}`)
	case r.URL.Path == "/apis":
		fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
	case r.URL.Path == "/api/v1":
		fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get","patch","create"]}]}`)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.patches = append(f.patches, fmt.Sprintf("%s %s %s %s", r.URL.Path, r.Header.Get("Content-Type"), r.URL.Query().Get("fieldManager"), b))
		f.mu.Unlock()
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func kubeconfigFor(url string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: %q}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
`, url))
}

func TestApply(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c, err := New(kubeconfigFor(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	err = c.Apply(ctx, `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
  namespace: kube-system
data:
  a: "1"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: other
data: {}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.patches) != 2 {
		t.Fatalf("patches: %v", api.patches)
	}
	if !strings.HasPrefix(api.patches[0], "/api/v1/namespaces/kube-system/configmaps/demo application/apply-patch+yaml niwakube ") {
		t.Errorf("server-side apply: %s", api.patches[0])
	}
	if !strings.HasPrefix(api.patches[1], "/api/v1/namespaces/default/configmaps/other ") {
		t.Errorf("namespace defaults to default: %s", api.patches[1])
	}

	// まだ CRD が無い種類は ErrNotReady
	err = c.Apply(ctx, "apiVersion: cilium.io/v2alpha1\nkind: CiliumL2AnnouncementPolicy\nmetadata: {name: x}\n")
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("unknown kind: want ErrNotReady, got %v", err)
	}
}

func TestDecode(t *testing.T) {
	objs, err := Decode("---\n# comment only\n---\napiVersion: v1\nkind: Namespace\nmetadata: {name: a}\n---\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].GetName() != "a" {
		t.Errorf("objs: %v", objs)
	}
}
