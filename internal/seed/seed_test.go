package seed

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/sys"
)

func TestUserData(t *testing.T) {
	b, err := UserData(User{
		SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA test"},
		Extra:             map[string]any{"packages": []string{"curl"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "#cloud-config\n") {
		t.Fatalf("missing #cloud-config header:\n%s", b)
	}
	var got struct {
		Users []struct {
			Name string   `yaml:"name"`
			Keys []string `yaml:"ssh_authorized_keys"`
		} `yaml:"users"`
		SSHPwauth bool     `yaml:"ssh_pwauth"`
		Packages  []string `yaml:"packages"`
	}
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Users) != 1 || got.Users[0].Name != "ubuntu" || got.Users[0].Keys[0] != "ssh-ed25519 AAAA test" {
		t.Errorf("unexpected users: %+v", got.Users)
	}
	if got.SSHPwauth || len(got.Packages) != 1 {
		t.Errorf("unexpected user-data: %+v", got)
	}
}

func TestNetworkConfig(t *testing.T) {
	b, err := NetworkConfig(Network{
		MAC:     "52:54:00:aa:bb:cc",
		Address: netip.MustParsePrefix("172.30.0.10/16"),
		Gateway: netip.MustParseAddr("172.30.0.1"),
		DNS:     []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got netplan
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	e := got.Ethernets["eth0"]
	if got.Version != 2 || e.Match.MACAddress != "52:54:00:aa:bb:cc" || e.SetName != "eth0" ||
		e.Addresses[0] != "172.30.0.10/16" || e.Routes[0].Via != "172.30.0.1" || e.Nameservers.Addresses[0] != "1.1.1.1" {
		t.Errorf("unexpected network-config:\n%s", b)
	}

	if _, err := NetworkConfig(Network{}); err == nil {
		t.Error("expected error for empty network")
	}
}

func TestBuildISO(t *testing.T) {
	dir := t.TempDir()
	r := &sys.FakeRunner{}
	iso, err := BuildISO(context.Background(), r, dir, Data{UserData: []byte("u"), MetaData: []byte("m"), NetworkConfig: []byte("n")})
	if err != nil {
		t.Fatal(err)
	}
	if iso != filepath.Join(dir, "seed.iso") {
		t.Errorf("iso = %s", iso)
	}
	for name, want := range map[string]string{"user-data": "u", "meta-data": "m", "network-config": "n"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	calls := r.Calls()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "cloud-localds -N ") {
		t.Errorf("calls = %v", calls)
	}
}

func TestNameservers(t *testing.T) {
	nc, err := NetworkConfig(Network{MAC: "52:54:00:12:34:56", Address: netip.MustParsePrefix("172.30.0.10/16"),
		Gateway: netip.MustParseAddr("172.30.0.1"), DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}})
	if err != nil {
		t.Fatal(err)
	}
	if ns, err := Nameservers(nc); err != nil || strings.Join(ns, ",") != "1.1.1.1,8.8.8.8" {
		t.Errorf("Nameservers = %v, %v", ns, err)
	}
	if _, err := Nameservers([]byte("{")); err == nil {
		t.Error("invalid YAML must fail")
	}
}
