// Package terminal は Web UI のターミナル (ブラウザの xterm.js と WebSocket でつなぐシェル) を提供する。
//
// シェルは root の niwakubed からは起動せず、systemd-run の一時ユーザー (DynamicUser) とサンドボックスの中で
// 動かす。クラスタの kubeconfig / talosconfig は systemd の credential として渡すので、シェルからは
// 自分のクラスタの設定しか見えない (/var/lib/niwakube と /run/niwakube は見えない)。
package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// Config はターミナルの設定。
type Config struct {
	// Dir はセッションごとの credential のファイルを置くディレクトリ (root だけが読める。例: /run/niwakube/term)。
	Dir string
	// Sandbox が true なら systemd-run の DynamicUser とサンドボックスでシェルを動かす。
	// false はサンドボックスの無い開発用 (niwakubed と同じユーザーでシェルが動く)。
	Sandbox bool
	// Shell はシェルのパス (既定 /bin/bash)。
	Shell string
	// MaxSessions は同時に開けるターミナルの数 (既定 8)。
	MaxSessions int
	// Hidden はシェルから見えなくするパス (Sandbox のとき)。
	Hidden []string
}

// Files はシェルに渡すクラスタの設定。
type Files struct {
	Cluster     string
	Kubeconfig  []byte
	Talosconfig []byte // Talos のクラスタのみ
	// SSH があれば、シェルから ssh <ノード名> でそのクラスタのノードに入れる (鍵はクラスタごと)
	SSH *SSH
}

// SSH はノードに入るための鍵と接続先。
type SSH struct {
	User  string
	Key   []byte // 秘密鍵 (OpenSSH 形式)
	Hosts []SSHHost
}

// SSHHost は ssh の接続先 1 台。Names は ssh に渡せる名前 (ノード名と cp / worker などの別名)。
type SSHHost struct {
	Names []string
	IP    netip.Addr
}

var sshNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

func (x *SSH) validate() error {
	if !sshNameRe.MatchString(x.User) || len(x.Key) == 0 {
		return errors.New("ssh: bad user or empty key")
	}
	for _, h := range x.Hosts {
		if !h.IP.IsValid() || len(h.Names) == 0 {
			return errors.New("ssh: bad host")
		}
		for _, n := range h.Names {
			if !sshNameRe.MatchString(n) {
				return fmt.Errorf("ssh: bad host name %q", n)
			}
		}
	}
	return nil
}

// Manager はターミナルのセッションを管理する。
type Manager struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	active int

	// command はシェルを起動するコマンドを作る (テストで差し替える)。
	command func(s *session) *exec.Cmd
}

func New(cfg Config, log *slog.Logger) *Manager {
	if cfg.Shell == "" {
		cfg.Shell = "/bin/bash"
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 8
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Manager{cfg: cfg, log: log}
	m.command = m.shellCommand
	return m
}

// Cleanup は前回の niwakubed が残したターミナルを止める (起動時に呼ぶ)。
func (m *Manager) Cleanup(ctx context.Context) {
	if m.cfg.Sandbox {
		_ = exec.CommandContext(ctx, "systemctl", "stop", unitPrefix+"*.service").Run()
	}
	_ = os.RemoveAll(m.cfg.Dir)
}

const unitPrefix = "niwakube-term-"

type session struct {
	id      string
	dir     string // credential のファイル
	cluster string
	talos   bool
	ssh     bool
}

func (s *session) unit() string { return unitPrefix + s.id }

var clusterRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// sshConfig は ~/.ssh/config の中身。DynamicUser のホームは / なので、ssh には -F でこのファイルを渡す
// (ssh は HOME ではなくユーザーの登録情報からホームを決める)。
func sshConfig(x *SSH) string {
	var b strings.Builder
	for _, h := range x.Hosts {
		fmt.Fprintf(&b, "Host %s\n  HostName %s\n", strings.Join(h.Names, " "), h.IP)
	}
	fmt.Fprintf(&b, `Host *
  User %s
  IdentityFile $HOME/.ssh/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile $HOME/.ssh/known_hosts
  StrictHostKeyChecking accept-new
  LogLevel ERROR
`, x.User)
	return b.String()
}

// bashrc はシェルの起動時に読む設定。credential を書き込める HOME にコピーし、補完とプロンプトを設定する。
func bashrc(cluster string, talos bool, ssh *SSH) string {
	var b strings.Builder
	b.WriteString(`# niwakube のターミナル (このセッションだけのホーム。閉じると消える)
[ -f /etc/bash.bashrc ] && . /etc/bash.bashrc
umask 077
mkdir -p "$HOME/.kube"
cp "$CREDENTIALS_DIRECTORY/kubeconfig" "$HOME/.kube/config"
export KUBECONFIG="$HOME/.kube/config"
`)
	if talos {
		b.WriteString(`mkdir -p "$HOME/.talos"
cp "$CREDENTIALS_DIRECTORY/talosconfig" "$HOME/.talos/config"
export TALOSCONFIG="$HOME/.talos/config"
`)
	}
	if ssh != nil {
		b.WriteString(`mkdir -p "$HOME/.ssh"
cp "$CREDENTIALS_DIRECTORY/sshkey" "$HOME/.ssh/id_ed25519"
cat > "$HOME/.ssh/config" <<NIWAKUBE_SSH
` + sshConfig(ssh) + `NIWAKUBE_SSH
ssh() { command ssh -F "$HOME/.ssh/config" "$@"; }
scp() { command scp -F "$HOME/.ssh/config" "$@"; }
`)
	}
	b.WriteString(`alias k=kubectl
if command -v kubectl >/dev/null 2>&1; then
  source <(kubectl completion bash)
  complete -o default -F __start_kubectl k
else
  echo "kubectl が見つかりません (ミニPC で scripts/host-setup.sh を実行すると入ります)"
fi
`)
	if talos {
		b.WriteString(`command -v talosctl >/dev/null 2>&1 && source <(talosctl completion bash)
`)
	}
	fmt.Fprintf(&b, `PS1='\[\e[1;32m\]%s\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ '
cd "$HOME"
echo "niwakube: クラスタ %s のターミナル (kubectl / k`, cluster, cluster)
	if talos {
		b.WriteString(` / talosctl`)
	}
	b.WriteString(` が使えます。ホームはこのセッションだけのもので、閉じると消えます)"
`)
	if ssh != nil {
		var names []string
		for _, h := range ssh.Hosts {
			names = append(names, h.Names[0])
		}
		fmt.Fprintf(&b, "echo \"niwakube: ssh <ノード> でノードに入れます (%s。sudo も使えます)\"\n", strings.Join(names, ", "))
	}
	return b.String()
}

// shellCommand はシェルを起動するコマンドを返す。
func (m *Manager) shellCommand(s *session) *exec.Cmd {
	if !m.cfg.Sandbox {
		// 開発用: サンドボックス無しで、セッションのディレクトリをホームと credential にする
		home := filepath.Join(s.dir, "home")
		_ = os.MkdirAll(home, 0o700)
		cmd := exec.Command(m.cfg.Shell, "--rcfile", filepath.Join(s.dir, "bashrc"), "-i")
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "HOME="+home, "CREDENTIALS_DIRECTORY="+s.dir, "TERM=xterm-256color")
		return cmd
	}
	unit := s.unit()
	home := "/run/" + unit
	args := []string{
		"--pty", "--quiet", "--collect", "--unit=" + unit,
		"--property=Description=niwakube terminal (" + s.cluster + ")",
		// root ではなく一時ユーザーで動かす (ProtectSystem=strict、PrivateTmp、NoNewPrivileges なども付く)
		"--property=DynamicUser=yes",
		"--property=ProtectHome=yes",
		"--property=RuntimeDirectory=" + unit,
		"--property=WorkingDirectory=" + home,
		"--property=LoadCredential=kubeconfig:" + filepath.Join(s.dir, "kubeconfig"),
		"--property=LoadCredential=bashrc:" + filepath.Join(s.dir, "bashrc"),
		"--property=ProtectKernelTunables=yes",
		"--property=ProtectKernelModules=yes",
		"--property=ProtectKernelLogs=yes",
		"--property=ProtectControlGroups=yes",
		"--property=ProtectClock=yes",
		"--property=ProtectHostname=yes",
		"--property=CapabilityBoundingSet=",
		"--property=LockPersonality=yes",
		"--property=MemoryMax=1G",
		"--property=TasksMax=256",
		"--property=RuntimeMaxSec=12h",
		"--setenv=HOME=" + home,
		"--setenv=TERM=xterm-256color",
		"--setenv=LANG=C.UTF-8",
		"--setenv=NIWAKUBE_CLUSTER=" + s.cluster,
	}
	if s.talos {
		args = append(args, "--property=LoadCredential=talosconfig:"+filepath.Join(s.dir, "talosconfig"))
	}
	if s.ssh {
		args = append(args, "--property=LoadCredential=sshkey:"+filepath.Join(s.dir, "sshkey"))
	}
	for _, p := range m.cfg.Hidden {
		args = append(args, "--property=InaccessiblePaths=-"+p) // "-" は無くてもよい
	}
	args = append(args, "--", m.cfg.Shell, "--rcfile", "/run/credentials/"+unit+".service/bashrc", "-i")
	return exec.Command("systemd-run", args...)
}

// ErrBusy はターミナルを開きすぎているときのエラー。
var ErrBusy = errors.New("too many terminals are open")

func (m *Manager) acquire() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active >= m.cfg.MaxSessions {
		return false
	}
	m.active++
	return true
}

func (m *Manager) release() {
	m.mu.Lock()
	m.active--
	m.mu.Unlock()
}

// prepare はセッションの credential のファイルを書く。
func (m *Manager) prepare(f Files) (*session, error) {
	if !clusterRe.MatchString(f.Cluster) {
		return nil, fmt.Errorf("invalid cluster name %q", f.Cluster)
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	if f.SSH != nil {
		if err := f.SSH.validate(); err != nil {
			return nil, err
		}
	}
	s := &session{id: f.Cluster + "-" + hex.EncodeToString(b[:]), cluster: f.Cluster, talos: len(f.Talosconfig) > 0, ssh: f.SSH != nil}
	s.dir = filepath.Join(m.cfg.Dir, s.id)
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	files := map[string][]byte{"kubeconfig": f.Kubeconfig, "bashrc": []byte(bashrc(f.Cluster, s.talos, f.SSH))}
	if s.talos {
		files["talosconfig"] = f.Talosconfig
	}
	if s.ssh {
		files["sshkey"] = f.SSH.Key
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(s.dir, name), data, 0o600); err != nil {
			os.RemoveAll(s.dir)
			return nil, err
		}
	}
	return s, nil
}

// control はブラウザから届く制御のメッセージ (テキストのフレーム)。端末の入力はバイナリのフレームで届く。
type control struct {
	Type string `json:"type"` // resize
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// Serve は WebSocket に切り替えて、クラスタのシェルとつなぐ。
// WebSocket の Origin はリクエストの Host と同じでなければ拒否する (別のサイトからの接続を防ぐ)。
func (m *Manager) Serve(w http.ResponseWriter, r *http.Request, f Files) {
	if !m.acquire() {
		http.Error(w, ErrBusy.Error(), http.StatusTooManyRequests)
		return
	}
	defer m.release()

	s, err := m.prepare(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(s.dir)

	conn, err := websocket.Accept(w, r, nil) // 既定で Origin と Host が違えば 403
	if err != nil {
		m.log.Warn("terminal: websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := m.command(s)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		conn.Close(websocket.StatusInternalError, "cannot start the shell: "+err.Error())
		return
	}
	m.log.Info("terminal: opened", "cluster", s.cluster, "session", s.id)
	defer func() {
		tty.Close()
		if m.cfg.Sandbox {
			_ = exec.Command("systemctl", "stop", s.unit()+".service").Run()
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		m.log.Info("terminal: closed", "cluster", s.cluster, "session", s.id)
	}()

	// シェルの出力 → ブラウザ
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return // シェルが終わった
			}
		}
	}()

	// ブラウザ → シェル
	go func() {
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := tty.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var c control
				if json.Unmarshal(data, &c) == nil && c.Type == "resize" && c.Cols > 0 && c.Rows > 0 && c.Cols <= 1000 && c.Rows <= 1000 {
					_ = pty.Setsize(tty, &pty.Winsize{Cols: uint16(c.Cols), Rows: uint16(c.Rows)})
				}
			}
		}
	}()

	select {
	case <-done:
		conn.Close(websocket.StatusNormalClosure, "the shell exited")
	case <-ctx.Done():
	}
}
