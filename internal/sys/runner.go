// Package sys は外部コマンドの実行を抽象化する。
// ip / systemd-run / cp などを呼ぶ箇所をテストで差し替えられるようにするため。
package sys

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Runner は外部コマンドを実行する。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner は os/exec で実際にコマンドを実行する。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// FakeRunner はテスト用の Runner。呼ばれたコマンドを記録し、Handler の結果を返す。
type FakeRunner struct {
	Handler func(name string, args []string) ([]byte, error)

	mu    sync.Mutex
	calls [][]string
}

func (f *FakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if f.Handler == nil {
		return nil, nil
	}
	return f.Handler(name, args)
}

// Calls はこれまでに呼ばれたコマンドを "name arg1 arg2" 形式で返す。
func (f *FakeRunner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}
