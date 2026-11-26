package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sat0ken/niwakube/internal/terminal"
)

func TestProtect(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cases := []struct {
		name     string
		loopback bool
		method   string
		host     string
		headers  map[string]string
		want     int
	}{
		{"loopback host", true, "GET", "127.0.0.1:8080", nil, 204},
		{"localhost", true, "GET", "localhost:8080", nil, 204},
		{"ipv6 loopback", true, "GET", "[::1]:8080", nil, 204},
		{"rebinding", true, "GET", "evil.example:8080", nil, 403},
		{"any host when listening on LAN", false, "GET", "192.168.1.20:8080", nil, 204},
		{"curl post", true, "POST", "127.0.0.1:8080", nil, 204},
		{"same-origin post", true, "POST", "127.0.0.1:8080",
			map[string]string{"Origin": "http://127.0.0.1:8080", "Sec-Fetch-Site": "same-origin"}, 204},
		{"cross-origin post", false, "POST", "192.168.1.20:8080",
			map[string]string{"Origin": "https://evil.example"}, 403},
		{"cross-site delete", false, "DELETE", "192.168.1.20:8080",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site but different origin", false, "POST", "192.168.1.20:8080",
			map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"cross-site get is allowed (no CORS, response is unreadable)", false, "GET", "192.168.1.20:8080",
			map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, 204},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "/api/vms", strings.NewReader("{}"))
			req.Host = c.host
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			protect(ok, c.loopback).ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
}

func TestIsLoopbackListen(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080":  true,
		"localhost:8080":  true,
		"[::1]:8080":      true,
		":8080":           false,
		"0.0.0.0:8080":    false,
		"192.168.1.20:80": false,
	} {
		if got := IsLoopbackListen(addr); got != want {
			t.Errorf("%s: got %v, want %v", addr, got, want)
		}
	}
}

func TestHostInfo(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, Options{Version: "v9.9.9", DataDir: dir})
	resp, body := do(t, "POST", ts.URL+"/api/vms", `{"name":"a","cpus":2,"memoryMiB":2048}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, "POST", ts.URL+"/api/vms", `{"name":"b","cpus":1,"memoryMiB":1024}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, "POST", ts.URL+"/api/vms/b/stop", ""); resp.StatusCode/100 != 2 {
		t.Fatalf("stop: %d %s", resp.StatusCode, body)
	}

	res, err := http.Get(ts.URL + "/api/host")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var h HostInfo
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if h.Version != "v9.9.9" || h.CPUs < 1 || h.Disk.Path != dir || h.Disk.TotalGiB <= 0 {
		t.Errorf("unexpected host info: %+v", h)
	}
	want := AllocatedInfo{Machines: 2, Running: 1, CPUs: 2, MemoryMiB: 2048}
	if h.Allocated != want {
		t.Errorf("allocated = %+v, want %+v", h.Allocated, want)
	}
}

func TestUIRouting(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") })
	ts := newTestServer(t, Options{UI: ui})
	for path, want := range map[string]string{"/": "ui", "/ui/app.js": "ui", "/api/vms": "[]", "/healthz": "ok"} {
		_, body := do(t, "GET", ts.URL+path, "")
		if !strings.Contains(body, want) {
			t.Errorf("%s: body %q does not contain %q", path, body, want)
		}
	}
	// POST は UI に渡さない
	if resp, _ := do(t, "POST", ts.URL+"/", ""); resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /: %d", resp.StatusCode)
	}
}

func TestTerminal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	term := terminal.New(terminal.Config{Dir: t.TempDir()}, nil) // サンドボックス無し (開発用)
	ts := newTestServer(t, Options{Terminal: term})
	if resp, body := do(t, "POST", ts.URL+"/api/clusters", `{"name":"demo","workers":0}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, "GET", ts.URL+"/api/clusters/nope/terminal", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown cluster: %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/clusters/demo/terminal", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("grep current-context \"$KUBECONFIG\"; exit\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), "current-context: demo") {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (output: %q)", err, out.String())
		}
		out.Write(data)
	}

	// ターミナルを有効にしていなければ 501
	ts2 := newTestServer(t)
	if resp, body := do(t, "POST", ts2.URL+"/api/clusters", `{"name":"demo","workers":0}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, "GET", ts2.URL+"/api/clusters/demo/terminal", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("terminal disabled: %d", resp.StatusCode)
	}
}
