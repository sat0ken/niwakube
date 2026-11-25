package netx

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/sat0ken/niwakube/internal/sys"
)

// PortForward はホストのポートから VM の IP への転送。
type PortForward struct {
	Proto      string // tcp | udp
	HostPort   int
	TargetIP   netip.Addr
	TargetPort int
}

// Forwarder は nftables の専用テーブル (ip niwakube_fwd) で転送を設定する。
// 毎回テーブルごと作り直すので、何度呼んでも同じ結果になる。
type Forwarder struct {
	Run    sys.Runner
	Bridge string // VM のブリッジ (ここから入ってきたものは転送しない)
	Dir    string // ルールのファイルを書くディレクトリ (例: /run/niwakube)
}

// Ruleset は nft -f に渡すルールを返す。
func (f Forwarder) Ruleset(forwards []PortForward) string {
	var pre, out strings.Builder
	for _, fw := range forwards {
		target := fmt.Sprintf("%s:%d", fw.TargetIP, fw.TargetPort)
		// LAN など外から入ってきたもの
		fmt.Fprintf(&pre, "    iifname != %q %s dport %d dnat to %s\n", f.Bridge, fw.Proto, fw.HostPort, target)
		// ホスト自身からホストのアドレス宛てに接続したもの
		fmt.Fprintf(&out, "    fib daddr type local %s dport %d dnat to %s\n", fw.Proto, fw.HostPort, target)
	}
	return fmt.Sprintf(`table ip niwakube_fwd
delete table ip niwakube_fwd
table ip niwakube_fwd {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
%s  }
  chain output {
    type nat hook output priority dstnat; policy accept;
%s  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    ct status dnat accept
  }
}
`, pre.String(), out.String())
}

// Apply は転送の設定を反映する。
func (f Forwarder) Apply(ctx context.Context, forwards []PortForward) error {
	if err := os.MkdirAll(f.Dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(f.Dir, "forwards.nft")
	if err := os.WriteFile(path, []byte(f.Ruleset(forwards)), 0o600); err != nil {
		return err
	}
	if _, err := f.Run.Run(ctx, "nft", "-f", path); err != nil {
		return fmt.Errorf("apply port forwards: %w", err)
	}
	return nil
}
