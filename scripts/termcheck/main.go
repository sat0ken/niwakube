// termcheck は Web UI のターミナル (WebSocket) にコマンドを送り、出力を確かめる E2E 用のツール。
//
//	termcheck -url ws://127.0.0.1:8080/api/clusters/demo/terminal -send 'kubectl get nodes; exit' -expect node/ -reject 'U=0'
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	url := flag.String("url", "", "ターミナルの WebSocket の URL")
	send := flag.String("send", "", "送るコマンド (改行は自動で足す)")
	timeout := flag.Duration("timeout", time.Minute, "全体の制限時間")
	var expect, reject multi
	flag.Var(&expect, "expect", "出力に含まれるべき文字列 (複数指定可)")
	flag.Var(&reject, "reject", "出力に含まれてはいけない文字列 (複数指定可)")
	flag.Parse()
	if err := run(*url, *send, *timeout, expect, reject); err != nil {
		fmt.Fprintln(os.Stderr, "termcheck:", err)
		os.Exit(1)
	}
}

func run(url, send string, timeout time.Duration, expect, reject []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":200,"rows":50}`)); err != nil {
		return err
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte(send+"\n")); err != nil {
		return err
	}
	var out strings.Builder
	defer func() { fmt.Print(out.String()) }()
	for {
		_, data, err := c.Read(ctx)
		out.Write(data)
		if err != nil {
			// シェルが終わって閉じた (exit) か、時間切れ
			break
		}
	}
	s := out.String()
	for _, e := range expect {
		if !strings.Contains(s, e) {
			return fmt.Errorf("output does not contain %q", e)
		}
	}
	for _, r := range reject {
		if strings.Contains(s, r) {
			return fmt.Errorf("output contains %q", r)
		}
	}
	return nil
}
