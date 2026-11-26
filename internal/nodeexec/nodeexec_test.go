package nodeexec

import (
	"context"
	"encoding/binary"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshServer は公開鍵 authorized だけを受け付け、exec されたコマンドを手元の bash で動かす。
func sshServer(t *testing.T, authorized string) string {
	t.Helper()
	hostKey, _ := NewKey("host")
	hs, err := ssh.ParsePrivateKey([]byte(hostKey))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) == authorized {
			return nil, nil
		}
		return nil, net.ErrClosed
	}}
	cfg.AddHostKey(hs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, _ := nch.Accept()
					go func() {
						for r := range creqs {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							r.Reply(true, nil)
							cmd := exec.Command("bash", "-s")
							cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
							code := 0
							if err := cmd.Run(); err != nil {
								if ee, ok := err.(*exec.ExitError); ok {
									code = ee.ExitCode()
								}
							}
							st := make([]byte, 4)
							binary.BigEndian.PutUint32(st, uint32(code))
							ch.SendRequest("exit-status", false, st)
							ch.Close()
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestRun(t *testing.T) {
	key, err := NewKey("niwakube-test")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := AuthorizedKey(key)
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("authorized key: %q %v", pub, err)
	}
	addr := sshServer(t, pub)
	host, port, _ := net.SplitHostPort(addr)
	old := sshPort
	sshPort = port
	defer func() { sshPort = old }()

	ctx := context.Background()
	r, err := Run(ctx, host, "ubuntu", key, "echo hello\nexit 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 3 || strings.TrimSpace(r.Output) != "hello" {
		t.Errorf("result: %+v", r)
	}
	if r, err := Run(ctx, host, "ubuntu", key, "true"); err != nil || r.ExitCode != 0 {
		t.Errorf("true: %+v %v", r, err)
	}
	// 別の鍵では入れない
	other, _ := NewKey("other")
	if _, err := Run(ctx, host, "ubuntu", other, "true"); err == nil {
		t.Error("an unknown key must be rejected")
	}
	// 時間切れ
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := Run(tctx, host, "ubuntu", key, "sleep 5"); err == nil {
		t.Error("want a timeout")
	}
}
