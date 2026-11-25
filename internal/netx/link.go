// Package netx はブリッジにつなぐ tap / veth の操作をまとめる。
package netx

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/sat0ken/niwakube/internal/sys"
)

// TapName はマシン名から tap デバイス名を作る。
// インターフェース名は 15 文字までなので、名前のハッシュを使う (nkt + 8 桁)。
func TapName(machine string) string {
	h := sha1.Sum([]byte(machine))
	return "nkt" + hex.EncodeToString(h[:4])
}

// Links は ip コマンドでリンクを操作する。
type Links struct {
	Run sys.Runner
}

func (l Links) exists(ctx context.Context, name string) bool {
	_, err := l.Run.Run(ctx, "ip", "link", "show", "dev", name)
	return err == nil
}

// EnsureTap は tap を (なければ) 作り、bridge につないで up する。
func (l Links) EnsureTap(ctx context.Context, name, bridge string) error {
	if !l.exists(ctx, name) {
		if _, err := l.Run.Run(ctx, "ip", "tuntap", "add", "dev", name, "mode", "tap"); err != nil {
			return fmt.Errorf("create tap %s: %w", name, err)
		}
	}
	if _, err := l.Run.Run(ctx, "ip", "link", "set", "dev", name, "master", bridge, "up"); err != nil {
		return fmt.Errorf("attach %s to %s: %w", name, bridge, err)
	}
	return nil
}

// Delete はリンクを消す。存在しなければ何もしない。
func (l Links) Delete(ctx context.Context, name string) error {
	if !l.exists(ctx, name) {
		return nil
	}
	if _, err := l.Run.Run(ctx, "ip", "link", "del", "dev", name); err != nil {
		return fmt.Errorf("delete link %s: %w", name, err)
	}
	return nil
}
