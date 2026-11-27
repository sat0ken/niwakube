// Package nodeexec はクラスタのノードに SSH で入ってコマンドを動かす (練習問題の出題と採点に使う)。
//
// 鍵はクラスタごとに niwakubed が作り、そのクラスタのノードにだけ入れる (ほかのクラスタには入れない)。
package nodeexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshPort はノードの SSH のポート (テストで差し替える)。
var sshPort = "22"

// NewKey は ed25519 の鍵を作り、OpenSSH 形式の秘密鍵 (PEM) を返す。
func NewKey(comment string) (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(block)), nil
}

// AuthorizedKey は秘密鍵から authorized_keys の 1 行を作る。
func AuthorizedKey(privateKey string) (string, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

// Result はコマンドの結果。
type Result struct {
	ExitCode int
	Output   string // 標準出力と標準エラー (最後の 4KB)
}

// Run は addr (IP) のノードに user で入り、script を bash に渡して動かす。
// 接続できないなどはエラー、コマンドが 0 以外で終わったのは Result.ExitCode で返す。
func Run(ctx context.Context, addr, user, privateKey, script string) (*Result, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// ノードは niwakubed が作ったブリッジの内側 (NAT) にあり、作り直すたびにホスト鍵が変わる。
		// 接続先は niwakubed が払い出した IP なので、ホスト鍵は確かめない
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         10 * time.Second,
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, sshPort))
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var out tailBuffer
	sess.Stdout = &out
	sess.Stderr = &out
	sess.Stdin = strings.NewReader(script)
	done := make(chan error, 1)
	go func() { done <- sess.Run("bash -s") }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		client.Close()
		return nil, ctx.Err()
	case err := <-done:
		res := &Result{Output: out.String()}
		var exit *ssh.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit):
			res.ExitCode = exit.ExitStatus()
		default:
			return nil, err
		}
		return res, nil
	}
}

// tailBuffer は最後の 4KB だけを残す。
type tailBuffer struct{ b bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b.Write(p)
	if t.b.Len() > 4096 {
		rest := t.b.Bytes()[t.b.Len()-4096:]
		t.b = *bytes.NewBuffer(append([]byte(nil), rest...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.b.String() }
