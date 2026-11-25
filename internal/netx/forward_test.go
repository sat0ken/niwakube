package netx

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/sys"
)

func TestForwarderRuleset(t *testing.T) {
	dir := t.TempDir()
	r := &sys.FakeRunner{}
	f := Forwarder{Run: r, Bridge: "nkbr0", Dir: dir}
	err := f.Apply(context.Background(), []PortForward{
		{Proto: "tcp", HostPort: 8080, TargetIP: netip.MustParseAddr("172.30.1.100"), TargetPort: 80},
		{Proto: "udp", HostPort: 5353, TargetIP: netip.MustParseAddr("172.30.1.101"), TargetPort: 53},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "forwards.nft"))
	rs := string(b)
	for _, want := range []string{
		"delete table ip niwakube_fwd",
		`iifname != "nkbr0" tcp dport 8080 dnat to 172.30.1.100:80`,
		"fib daddr type local tcp dport 8080 dnat to 172.30.1.100:80",
		`iifname != "nkbr0" udp dport 5353 dnat to 172.30.1.101:53`,
		"ct status dnat accept",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, rs)
		}
	}
	if calls := r.Calls(); len(calls) != 1 || calls[0] != "nft -f "+filepath.Join(dir, "forwards.nft") {
		t.Errorf("calls: %v", calls)
	}
	// 転送が無くてもテーブルは作り直す (前の転送が消える)
	if !strings.Contains(f.Ruleset(nil), "chain prerouting") {
		t.Error("empty ruleset must still define the table")
	}
}
