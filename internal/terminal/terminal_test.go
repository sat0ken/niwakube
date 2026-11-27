package terminal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newTestManager(t *testing.T, script string) (*Manager, *httptest.Server) {
	t.Helper()
	m := New(Config{Dir: t.TempDir(), MaxSessions: 1}, nil)
	m.command = func(s *session) *exec.Cmd {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(os.Environ(), "CREDENTIALS_DIRECTORY="+s.dir)
		return cmd
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.Serve(w, r, Files{Cluster: "demo", Kubeconfig: []byte("apiVersion: v1\nkind: Config\n")})
	}))
	t.Cleanup(ts.Close)
	return m, ts
}

func wsURL(ts *httptest.Server) string { return "ws" + strings.TrimPrefix(ts.URL, "http") }

// readUntil は want がすべて出るまで (または期限まで) 出力を読む。
func readUntil(ctx context.Context, t *testing.T, c *websocket.Conn, want ...string) string {
	t.Helper()
	var out strings.Builder
	for {
		ok := true
		for _, w := range want {
			if !strings.Contains(out.String(), w) {
				ok = false
			}
		}
		if ok {
			return out.String()
		}
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (output so far: %q)", err, out.String())
		}
		out.Write(data)
	}
}

func TestServe(t *testing.T) {
	// 入力を 1 行読み、サイズと credential の kubeconfig を表示して終わる
	_, ts := newTestManager(t, `read line; echo "got:$line"; stty size; head -n1 "$CREDENTIALS_DIRECTORY/kubeconfig"; test -f "$CREDENTIALS_DIRECTORY/bashrc" && echo bashrc-ok`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(ctx, t, c, "got:hello", "30 100", "apiVersion: v1", "bashrc-ok")

	// シェルが終わると正常終了で閉じる
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Errorf("close: %v", err)
			}
			break
		}
	}
}

func TestServeRejectsOtherOrigins(t *testing.T) {
	_, ts := newTestManager(t, `sleep 5`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin websocket must be rejected: %v %v", err, resp)
	}
}

func TestServeLimitsSessions(t *testing.T) {
	_, ts := newTestManager(t, `sleep 10`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(100 * time.Millisecond)
	if _, resp, err := websocket.Dial(ctx, wsURL(ts), nil); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second terminal must be refused (MaxSessions 1): %v %v", err, resp)
	}
}

func TestSandboxCommand(t *testing.T) {
	m := New(Config{Dir: "/run/niwakube/term", Sandbox: true, Hidden: []string{"/var/lib/niwakube", "/run/niwakube"}}, nil)
	s := &session{id: "demo-abc", dir: "/run/niwakube/term/demo-abc", cluster: "demo", talos: true}
	cmd := m.shellCommand(s)
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"systemd-run --pty", "--unit=niwakube-term-demo-abc",
		"--property=DynamicUser=yes", "--property=ProtectHome=yes", "--property=CapabilityBoundingSet=",
		"--property=LoadCredential=kubeconfig:/run/niwakube/term/demo-abc/kubeconfig",
		"--property=LoadCredential=talosconfig:/run/niwakube/term/demo-abc/talosconfig",
		"--property=InaccessiblePaths=-/var/lib/niwakube", "--property=InaccessiblePaths=-/run/niwakube",
		"--setenv=HOME=/run/niwakube-term-demo-abc",
		"-- /bin/bash --rcfile /run/credentials/niwakube-term-demo-abc.service/bashrc -i",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("sandbox command does not contain %q:\n%s", want, args)
		}
	}
}

func TestBashrc(t *testing.T) {
	rc := bashrc("tal", true, nil)
	for _, want := range []string{`cp "$CREDENTIALS_DIRECTORY/kubeconfig" "$HOME/.kube/config"`, `export TALOSCONFIG=`, "alias k=kubectl", "kubectl completion bash", "talosctl completion bash", "tal"} {
		if !strings.Contains(rc, want) {
			t.Errorf("bashrc does not contain %q:\n%s", want, rc)
		}
	}
	if strings.Contains(bashrc("k3s", false, nil), "talos") {
		t.Error("talos settings must not be in a non-Talos bashrc")
	}
	// bash で構文エラーが無いこと
	f := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(f, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Errorf("bash -n: %v %s", err, out)
	}
}

func TestPrepareRejectsBadNames(t *testing.T) {
	m := New(Config{Dir: t.TempDir()}, nil)
	for _, n := range []string{"", "../x", "a b", "A"} {
		if _, err := m.prepare(Files{Cluster: n}); err == nil {
			t.Errorf("%q must be rejected", n)
		}
	}
}

func TestSSH(t *testing.T) {
	x := &SSH{User: "ubuntu", Key: []byte("PRIVATE KEY"), Hosts: []SSHHost{
		{Names: []string{"demo-cp-1", "cp"}, IP: netip.MustParseAddr("172.30.1.11")},
		{Names: []string{"demo-worker-1", "worker"}, IP: netip.MustParseAddr("172.30.1.21")},
	}}
	dir := t.TempDir()
	m := New(Config{Dir: dir}, nil)
	s, err := m.prepare(Files{Cluster: "demo", Kubeconfig: []byte("kc"), SSH: x})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(s.dir, "sshkey")); err != nil || string(b) != "PRIVATE KEY" {
		t.Errorf("sshkey: %q %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(s.dir, "sshkey")); fi.Mode().Perm() != 0o600 {
		t.Errorf("sshkey mode: %v", fi.Mode())
	}
	sb := New(Config{Dir: dir, Sandbox: true}, nil)
	args := strings.Join(sb.shellCommand(&session{id: "demo-abc", cluster: "demo", ssh: true}).Args, " ")
	if !strings.Contains(args, "--property=LoadCredential=sshkey:") {
		t.Errorf("the key must be passed as a credential:\n%s", args)
	}

	// bashrc を実際に動かし、ssh の設定ができること
	home := filepath.Join(dir, "home")
	os.MkdirAll(home, 0o700)
	rc := bashrc("demo", false, x)
	script := rc + "\ncat \"$HOME/.ssh/config\"; type ssh; stat -c %a \"$HOME/.ssh/id_ed25519\"\n"
	f := filepath.Join(dir, "rc")
	os.WriteFile(f, []byte(script), 0o600)
	cmd := exec.Command("bash", f)
	cmd.Env = []string{"HOME=" + home, "CREDENTIALS_DIRECTORY=" + s.dir, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bashrc: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Host demo-cp-1 cp\n  HostName 172.30.1.11",
		"Host demo-worker-1 worker\n  HostName 172.30.1.21",
		"User ubuntu",
		"IdentityFile " + home + "/.ssh/id_ed25519",
		"UserKnownHostsFile " + home + "/.ssh/known_hosts",
		`command ssh -F "$HOME/.ssh/config"`,
		"600",
		"ssh <ノード> でノードに入れます (demo-cp-1, demo-worker-1",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	for name, bad := range map[string]*SSH{
		"bad name": {User: "ubuntu", Key: []byte("k"), Hosts: []SSHHost{{Names: []string{"a;rm -rf /"}, IP: netip.MustParseAddr("1.2.3.4")}}},
		"bad user": {User: "-oProxyCommand=x", Key: []byte("k")},
		"no key":   {User: "ubuntu"},
		"no ip":    {User: "ubuntu", Key: []byte("k"), Hosts: []SSHHost{{Names: []string{"cp"}}}},
	} {
		if _, err := m.prepare(Files{Cluster: "demo", Kubeconfig: []byte("kc"), SSH: bad}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
