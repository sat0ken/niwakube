package ipam

import (
	"errors"
	"net/netip"
	"regexp"
	"testing"
)

func TestPoolAllocate(t *testing.T) {
	p, err := ParsePool("172.30.0.10-172.30.0.12")
	if err != nil {
		t.Fatal(err)
	}
	used := map[netip.Addr]bool{}
	for _, want := range []string{"172.30.0.10", "172.30.0.11", "172.30.0.12"} {
		a, err := p.Allocate(used)
		if err != nil {
			t.Fatal(err)
		}
		if a.String() != want {
			t.Fatalf("got %s, want %s", a, want)
		}
		used[a] = true
	}
	if _, err := p.Allocate(used); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	delete(used, netip.MustParseAddr("172.30.0.11"))
	if a, _ := p.Allocate(used); a.String() != "172.30.0.11" {
		t.Fatalf("freed address not reused: %s", a)
	}
}

func TestParsePoolInvalid(t *testing.T) {
	for _, s := range []string{"", "172.30.0.10", "172.30.0.20-172.30.0.10", "a-b", "::1-::2"} {
		if _, err := ParsePool(s); err == nil {
			t.Errorf("ParsePool(%q): expected error", s)
		}
	}
}

func TestRandomMAC(t *testing.T) {
	m, err := RandomMAC()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^52:54:00(:[0-9a-f]{2}){3}$`).MatchString(m) {
		t.Errorf("bad mac %q", m)
	}
}
