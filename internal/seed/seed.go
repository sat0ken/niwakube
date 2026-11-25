// Package seed は cloud-init (NoCloud) に渡す初期設定を生成する。
//
// 中身 (user-data / meta-data / network-config) の生成は全バックエンド共通。
// 渡し方 (ISO ディスク / ディレクトリの bind mount など) はバックエンドごとに異なる。
package seed

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/sys"
)

// Data は NoCloud の 3 ファイル分の中身。
type Data struct {
	UserData      []byte
	MetaData      []byte
	NetworkConfig []byte
}

// User は user-data の基本部分。
type User struct {
	SSHAuthorizedKeys []string
	// Extra は user-data のトップレベルに追加・上書きするキー。
	// フェーズ2で Distro が packages / write_files / runcmd などを足すのに使う。
	Extra map[string]any
}

// UserData は #cloud-config 形式の user-data を返す。
func UserData(u User) ([]byte, error) {
	keys := u.SSHAuthorizedKeys
	if keys == nil {
		keys = []string{}
	}
	m := map[string]any{
		"users": []map[string]any{{
			"name":                "ubuntu",
			"sudo":                "ALL=(ALL) NOPASSWD:ALL",
			"shell":               "/bin/bash",
			"lock_passwd":         true,
			"ssh_authorized_keys": keys,
		}},
		"ssh_pwauth": false,
	}
	for k, v := range u.Extra {
		m[k] = v
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal user-data: %w", err)
	}
	return append([]byte("#cloud-config\n"), b...), nil
}

// MetaData は meta-data を返す。instanceID が変わると cloud-init は初回起動として再実行する。
func MetaData(instanceID, hostname string) ([]byte, error) {
	return yaml.Marshal(map[string]string{
		"instance-id":    instanceID,
		"local-hostname": hostname,
	})
}

// Network は固定 IP の設定。
type Network struct {
	MAC     string
	Address netip.Prefix
	Gateway netip.Addr
	DNS     []netip.Addr
}

type netplanRoute struct {
	To  string `yaml:"to"`
	Via string `yaml:"via"`
}

type netplanEthernet struct {
	Match struct {
		MACAddress string `yaml:"macaddress"`
	} `yaml:"match"`
	SetName     string         `yaml:"set-name"`
	Addresses   []string       `yaml:"addresses"`
	Routes      []netplanRoute `yaml:"routes"`
	Nameservers struct {
		Addresses []string `yaml:"addresses"`
	} `yaml:"nameservers"`
}

type netplan struct {
	Version   int                        `yaml:"version"`
	Ethernets map[string]netplanEthernet `yaml:"ethernets"`
}

// NetworkConfig は network-config (v2) を返す。NIC は MAC で特定し eth0 に改名する。
func NetworkConfig(n Network) ([]byte, error) {
	if !n.Address.IsValid() || !n.Gateway.IsValid() || n.MAC == "" {
		return nil, fmt.Errorf("network-config: MAC, address and gateway are required")
	}
	var e netplanEthernet
	e.Match.MACAddress = n.MAC
	e.SetName = "eth0"
	e.Addresses = []string{n.Address.String()}
	e.Routes = []netplanRoute{{To: "default", Via: n.Gateway.String()}}
	e.Nameservers.Addresses = []string{}
	for _, d := range n.DNS {
		e.Nameservers.Addresses = append(e.Nameservers.Addresses, d.String())
	}
	return yaml.Marshal(netplan{Version: 2, Ethernets: map[string]netplanEthernet{"eth0": e}})
}

// WriteFiles は dir に user-data / meta-data / network-config を書き出す。
func WriteFiles(dir string, d Data) error {
	files := map[string][]byte{
		"user-data":      d.UserData,
		"meta-data":      d.MetaData,
		"network-config": d.NetworkConfig,
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// BuildISO は dir に 3 ファイルを書き出し、cloud-localds で dir/seed.iso (ラベル cidata) を作る。
// VM 型のバックエンド (Cloud Hypervisor、後で Firecracker) で使う。
func BuildISO(ctx context.Context, r sys.Runner, dir string, d Data) (string, error) {
	if err := WriteFiles(dir, d); err != nil {
		return "", fmt.Errorf("write seed files: %w", err)
	}
	iso := filepath.Join(dir, "seed.iso")
	if _, err := r.Run(ctx, "cloud-localds",
		"-N", filepath.Join(dir, "network-config"),
		iso,
		filepath.Join(dir, "user-data"),
		filepath.Join(dir, "meta-data"),
	); err != nil {
		return "", fmt.Errorf("build seed iso: %w", err)
	}
	return iso, nil
}
