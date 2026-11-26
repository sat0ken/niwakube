// Package netx はブリッジにつなぐ tap / veth の操作をまとめる。
package netx

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"

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

// VethName はマシン名からホスト側の veth の名前を作る (nkv + 8 桁)。
func VethName(machine string) string {
	h := sha1.Sum([]byte(machine))
	return "nkv" + hex.EncodeToString(h[:4])
}

// NetnsName はマシンのネットワーク名前空間の名前 (/run/netns/<名前>)。
func NetnsName(machine string) string { return "niwakube-" + machine }

// NetnsPath はネットワーク名前空間のパス。
func NetnsPath(machine string) string { return "/run/netns/" + NetnsName(machine) }

// NetnsSpec はコンテナ型のノードのネットワーク名前空間の設定。
type NetnsSpec struct {
	Name     string // ネットワーク名前空間の名前
	HostVeth string // ホスト側の veth (ブリッジにつなぐ)
	Bridge   string
	MAC      string       // 名前空間の中の eth0 の MAC
	Address  netip.Prefix // 名前空間の中の eth0 のアドレス
	Gateway  netip.Addr
}

// EnsureNetns はネットワーク名前空間と veth を作り、中の eth0 に MAC・アドレス・デフォルトルートを付ける。
// すでに両方あればブリッジにつなぎ直すだけ。片方だけ残っていれば作り直す。
// (コンテナ型のノードは自分でアドレスを付けないものもあるので、ホスト側で付けておく)
func (l Links) EnsureNetns(ctx context.Context, s NetnsSpec) error {
	_, nsErr := l.Run.Run(ctx, "ip", "netns", "exec", s.Name, "true")
	if nsErr == nil && l.exists(ctx, s.HostVeth) {
		_, err := l.Run.Run(ctx, "ip", "link", "set", "dev", s.HostVeth, "master", s.Bridge, "up")
		return err
	}
	if err := l.DeleteNetns(ctx, s.Name, s.HostVeth); err != nil {
		return err
	}
	steps := [][]string{
		{"ip", "netns", "add", s.Name},
		{"ip", "link", "add", s.HostVeth, "type", "veth", "peer", "name", "eth0", "netns", s.Name},
		{"ip", "link", "set", "dev", s.HostVeth, "master", s.Bridge, "up"},
		{"ip", "-n", s.Name, "link", "set", "dev", "eth0", "address", s.MAC},
		{"ip", "-n", s.Name, "link", "set", "dev", "lo", "up"},
		{"ip", "-n", s.Name, "link", "set", "dev", "eth0", "up"},
		{"ip", "-n", s.Name, "addr", "add", s.Address.String(), "dev", "eth0"},
		{"ip", "-n", s.Name, "route", "add", "default", "via", s.Gateway.String()},
	}
	for _, st := range steps {
		if _, err := l.Run.Run(ctx, st[0], st[1:]...); err != nil {
			_ = l.DeleteNetns(ctx, s.Name, s.HostVeth)
			return fmt.Errorf("%s: %w", strings.Join(st, " "), err)
		}
	}
	return nil
}

// DeleteNetns はネットワーク名前空間 (中の veth も消える) とホスト側の veth を消す。無ければ何もしない。
func (l Links) DeleteNetns(ctx context.Context, name, hostVeth string) error {
	if _, err := l.Run.Run(ctx, "ip", "netns", "exec", name, "true"); err == nil {
		if _, err := l.Run.Run(ctx, "ip", "netns", "del", name); err != nil {
			return fmt.Errorf("delete netns %s: %w", name, err)
		}
	}
	return l.Delete(ctx, hostVeth)
}
