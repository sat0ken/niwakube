// Package installer は installer/config.yaml から、ホストを自動インストールする ISO を作る。
//
// ベースは Ubuntu の mini ISO。起動すると Ubuntu Server の ISO をネットワークからメモリに
// ダウンロードして起動し、USB 上の CIDATA パーティションにある autoinstall の設定でインストールする。
// 詳細は docs/installer.md を参照。
package installer

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config は installer/config.yaml の内容。
type Config struct {
	Hostname string        `yaml:"hostname"`
	Timezone string        `yaml:"timezone"`
	Keyboard string        `yaml:"keyboard"`
	Locale   string        `yaml:"locale"`
	Ubuntu   UbuntuConfig  `yaml:"ubuntu"`
	User     UserConfig    `yaml:"user"`
	Network  *Network      `yaml:"network"` // nil なら DHCP
	Storage  StorageConfig `yaml:"storage"`
	Install  InstallConfig `yaml:"install"`
	Niwakube NiwakubeSetup `yaml:"niwakube"`
	// Autoinstall は生成した autoinstall の設定に深くマージして上書きする (上級者向け)。
	Autoinstall map[string]any `yaml:"autoinstall"`

	// serialConsole は CI のテスト用。GRUB と mini ISO のカーネルもシリアルに出力する。
	serialConsole bool
}

type UbuntuConfig struct {
	Release string `yaml:"release"` // 例: "26.04"
}

type UserConfig struct {
	Name              string   `yaml:"name"`
	GitHubKeys        string   `yaml:"github_keys"` // GitHub のユーザー名。公開鍵を取り込む
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys"`
}

type Network struct {
	Address string   `yaml:"address"` // 例: 192.168.1.50/24
	Gateway string   `yaml:"gateway"`
	DNS     []string `yaml:"dns"`
	// IPv6 を false にすると、ルーターが配る IPv6 (RA のアドレスと DNS サーバー) を受け取らない。
	// IPv6 の経路が使えない回線で、名前解決や接続が IPv6 に向かって止まるのを防ぐ (既定 true)
	IPv6 *bool `yaml:"ipv6"`
}

// DiskMatch はインストール先のディスクの選び方。"largest"、"internal" か、モデル名 / シリアル番号。
type DiskMatch struct {
	Largest  bool
	Internal bool // 内蔵 (SATA / NVMe) のうち一番大きいディスク。USB のディスクは選ばない
	Model    string
	Serial   string
}

func (d *DiskMatch) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch n.Value {
		case "largest":
			d.Largest = true
		case "internal":
			d.Internal = true
		default:
			return fmt.Errorf("storage.disk: want \"largest\", \"internal\" or {model: ...} / {serial: ...}, got %q", n.Value)
		}
		return nil
	}
	var m struct {
		Model  string `yaml:"model"`
		Serial string `yaml:"serial"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	d.Model, d.Serial = m.Model, m.Serial
	return nil
}

func (d DiskMatch) String() string {
	switch {
	case d.Serial != "":
		return "serial " + d.Serial
	case d.Model != "":
		return "model " + d.Model
	case d.Internal:
		return "largest internal disk (SATA / NVMe)"
	default:
		return "largest disk"
	}
}

type StorageConfig struct {
	Disk     DiskMatch `yaml:"disk"`
	RootSize string    `yaml:"root_size"` // 例: 100G。残りは /var/lib/niwakube (XFS)
}

type InstallConfig struct {
	Mode       string   `yaml:"mode"`        // confirm | unattended (GRUB の既定の項目)
	After      string   `yaml:"after"`       // poweroff | reboot
	Timeout    int      `yaml:"timeout"`     // GRUB のメニューの待ち時間 (秒)
	KernelArgs []string `yaml:"kernel_args"` // インストーラーのカーネルに追加する引数
	// ISOMirror は Server ISO を取るミラー (https://releases.ubuntu.com の代わり。
	// 例: https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases)。止まったときは releases.ubuntu.com と交互に試す
	ISOMirror string `yaml:"iso_mirror"`
}

type NiwakubeSetup struct {
	Setup *bool  `yaml:"setup"` // 初回起動時に host-setup.sh を実行する (既定 true)
	Repo  string `yaml:"repo"`  // 例: sat0ken/niwakube
	Ref   string `yaml:"ref"`   // ブランチかタグ。v* のタグなら niwakubed もリリースから入れる
}

func (n NiwakubeSetup) Enabled() bool { return n.Setup == nil || *n.Setup }

var (
	hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	usernameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	githubRe   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	sizeRe     = regexp.MustCompile(`^[1-9][0-9]*[MGT]$`)
	releaseRe  = regexp.MustCompile(`^\d{2}\.\d{2}$`)
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	safeArgRe  = regexp.MustCompile(`^[A-Za-z0-9_.,:=/+-]+$`)
	mirrorRe   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9_.~/-]*)?$`)
	simpleRe   = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)
	diskIDRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+( [A-Za-z0-9_.-]+)*$`)
)

// Load は config.yaml を読み、既定値を入れて検証する。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.setDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Timezone == "" {
		c.Timezone = "Etc/UTC"
	}
	if c.Keyboard == "" {
		c.Keyboard = "us"
	}
	if c.Locale == "" {
		c.Locale = "en_US.UTF-8"
	}
	if c.Ubuntu.Release == "" {
		c.Ubuntu.Release = "26.04"
	}
	if !c.Storage.Disk.Largest && !c.Storage.Disk.Internal && c.Storage.Disk.Model == "" && c.Storage.Disk.Serial == "" {
		c.Storage.Disk.Largest = true
	}
	if c.Storage.RootSize == "" {
		c.Storage.RootSize = "100G"
	}
	if c.Install.Mode == "" {
		c.Install.Mode = "confirm"
	}
	if c.Install.After == "" {
		c.Install.After = "poweroff"
	}
	if c.Install.Timeout == 0 {
		c.Install.Timeout = 10
	}
	if c.Niwakube.Repo == "" {
		c.Niwakube.Repo = "sat0ken/niwakube"
	}
	if c.Niwakube.Ref == "" {
		c.Niwakube.Ref = "main"
	}
}

// Validate は設定を検証する。
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !hostnameRe.MatchString(c.Hostname) {
		bad("hostname must be a lowercase DNS label: %q", c.Hostname)
	}
	for field, v := range map[string]string{"timezone": c.Timezone, "keyboard": c.Keyboard, "locale": c.Locale} {
		if !simpleRe.MatchString(v) {
			bad("%s has invalid characters: %q", field, v)
		}
	}
	if !releaseRe.MatchString(c.Ubuntu.Release) {
		bad("ubuntu.release must be like 26.04: %q", c.Ubuntu.Release)
	}
	if !usernameRe.MatchString(c.User.Name) {
		bad("user.name is invalid: %q", c.User.Name)
	}
	if c.User.GitHubKeys != "" && !githubRe.MatchString(c.User.GitHubKeys) {
		bad("user.github_keys must be a GitHub user name: %q", c.User.GitHubKeys)
	}
	if c.User.GitHubKeys == "" && len(c.User.SSHAuthorizedKeys) == 0 {
		bad("user: set github_keys or ssh_authorized_keys (password login is disabled)")
	}
	for _, k := range c.User.SSHAuthorizedKeys {
		if !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") && !strings.HasPrefix(k, "sk-") {
			bad("user.ssh_authorized_keys: not an SSH public key: %.20q", k)
		}
	}
	if n := c.Network; n != nil {
		if _, err := netip.ParsePrefix(n.Address); err != nil {
			bad("network.address must be like 192.168.1.50/24: %v", err)
		}
		if _, err := netip.ParseAddr(n.Gateway); err != nil {
			bad("network.gateway: %v", err)
		}
		for _, d := range n.DNS {
			if _, err := netip.ParseAddr(d); err != nil {
				bad("network.dns: %v", err)
			}
		}
	}
	d := c.Storage.Disk
	if (d.Model != "" && d.Serial != "") || (d.Largest && (d.Model != "" || d.Serial != "")) {
		bad("storage.disk: choose one of largest, model or serial")
	}
	for field, v := range map[string]string{"model": d.Model, "serial": d.Serial} {
		if v != "" && !diskIDRe.MatchString(v) {
			bad("storage.disk.%s may contain only letters, digits, spaces, '_', '.' and '-': %q", field, v)
		}
	}
	if !sizeRe.MatchString(c.Storage.RootSize) {
		bad("storage.root_size must be like 100G: %q", c.Storage.RootSize)
	}
	switch c.Install.Mode {
	case "confirm":
	case "unattended":
		if d.Largest {
			bad("install.mode unattended erases the disk without asking, so storage.disk must be internal, {model: ...} or {serial: ...}, not largest")
		}
	default:
		bad("install.mode must be confirm or unattended: %q", c.Install.Mode)
	}
	if c.Install.After != "poweroff" && c.Install.After != "reboot" {
		bad("install.after must be poweroff or reboot: %q", c.Install.After)
	}
	if c.Install.Timeout < 1 || c.Install.Timeout > 300 {
		bad("install.timeout must be between 1 and 300")
	}
	for _, a := range c.Install.KernelArgs {
		if !safeArgRe.MatchString(a) {
			bad("install.kernel_args: invalid argument %q", a)
		}
	}
	c.Install.ISOMirror = strings.TrimRight(c.Install.ISOMirror, "/")
	if c.Install.ISOMirror != "" && !mirrorRe.MatchString(c.Install.ISOMirror) {
		bad("install.iso_mirror must be an http(s) URL like https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases: %q", c.Install.ISOMirror)
	}
	if !repoRe.MatchString(c.Niwakube.Repo) {
		bad("niwakube.repo must be owner/name: %q", c.Niwakube.Repo)
	}
	if !refRe.MatchString(c.Niwakube.Ref) {
		bad("niwakube.ref is invalid: %q", c.Niwakube.Ref)
	}
	return errors.Join(errs...)
}

// TestDiskSerial は CI の VM でインストール先にするディスクのシリアル番号 (QEMU の NVMe に付ける)。
const TestDiskSerial = "niwakube-ci-disk"

// ForTest は CI の VM でテストできるように設定を変える。
// 実機向けの ISO との違いは、ネットワーク (DHCP)、ディスクの選び方、ルートのサイズ、無人インストール、
// シリアルコンソールだけ。autoinstall の生成や ISO の作り方は同じコードを通る。
func (c *Config) ForTest() {
	c.Network = nil
	// 実機のモデル名やシリアル番号は VM に無いので、VM の NVMe のシリアル番号にする。
	// internal はそのまま試す (VM には USB の ISO と、小さい SATA のディスクもつないである)
	if !c.Storage.Disk.Internal {
		c.Storage.Disk = DiskMatch{Serial: TestDiskSerial}
	}
	c.Storage.RootSize = "20G"
	c.Install.Mode = "unattended"
	c.Install.After = "poweroff"
	c.Install.Timeout = 3
	c.Install.KernelArgs = append(c.Install.KernelArgs, "console=tty0", "console=ttyS0,115200")
	c.serialConsole = true
}
