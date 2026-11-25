// Package ipam は IP アドレスと MAC アドレスを払い出す。
package ipam

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

var ErrExhausted = errors.New("no free address in pool")

// Pool は Start から End まで (両端を含む) のアドレス範囲。
type Pool struct {
	Start, End netip.Addr
}

// ParsePool は "172.30.0.10-172.30.0.254" 形式を読む。
func ParsePool(s string) (Pool, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Pool{}, fmt.Errorf("pool %q: want START-END", s)
	}
	start, err := netip.ParseAddr(strings.TrimSpace(a))
	if err != nil {
		return Pool{}, err
	}
	end, err := netip.ParseAddr(strings.TrimSpace(b))
	if err != nil {
		return Pool{}, err
	}
	if !start.Is4() || !end.Is4() || end.Less(start) {
		return Pool{}, fmt.Errorf("pool %q: invalid IPv4 range", s)
	}
	return Pool{Start: start, End: end}, nil
}

func (p Pool) Contains(a netip.Addr) bool {
	return !a.Less(p.Start) && !p.End.Less(a)
}

// Allocate は used に含まれない最小のアドレスを返す。
func (p Pool) Allocate(used map[netip.Addr]bool) (netip.Addr, error) {
	for a := p.Start; p.Contains(a); a = a.Next() {
		if !used[a] {
			return a, nil
		}
		if a == p.End {
			break
		}
	}
	return netip.Addr{}, ErrExhausted
}

// RandomMAC は 52:54:00 で始まるローカル管理用の MAC アドレスを返す。
func RandomMAC() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2]), nil
}
