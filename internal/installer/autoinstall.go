package installer

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// UserData は CIDATA に置く user-data (autoinstall の設定を含む #cloud-config) を返す。
func (c *Config) UserData() ([]byte, error) {
	ai, err := c.autoinstall()
	if err != nil {
		return nil, err
	}
	if c.Autoinstall != nil {
		ai = deepMerge(ai, c.Autoinstall)
	}
	b, err := yaml.Marshal(map[string]any{"autoinstall": ai})
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), b...), nil
}

// MetaData は CIDATA に置く meta-data を返す。
func (c *Config) MetaData() []byte {
	return []byte("instance-id: niwakube-installer-" + c.Hostname + "\n")
}

func (c *Config) autoinstall() (map[string]any, error) {
	firstboot, err := c.firstbootScript()
	if err != nil {
		return nil, err
	}
	user := map[string]any{
		"name":        c.User.Name,
		"groups":      []string{"adm", "sudo"},
		"shell":       "/bin/bash",
		"sudo":        "ALL=(ALL) NOPASSWD:ALL",
		"lock_passwd": true, // パスワードではログインさせない (SSH 鍵のみ)
	}
	if c.User.GitHubKeys != "" {
		user["ssh_import_id"] = []string{"gh:" + c.User.GitHubKeys}
	}
	if len(c.User.SSHAuthorizedKeys) > 0 {
		user["ssh_authorized_keys"] = c.User.SSHAuthorizedKeys
	}
	userData := map[string]any{
		"hostname":          c.Hostname,
		"preserve_hostname": false,
		"users":             []any{user},
	}
	if c.Niwakube.Enabled() {
		userData["write_files"] = []map[string]any{{
			"path": "/usr/local/sbin/niwakube-firstboot.sh", "permissions": "0755", "content": firstboot,
		}}
		userData["runcmd"] = []any{[]string{"bash", "-c", "/usr/local/sbin/niwakube-firstboot.sh 2>&1 | tee /var/log/niwakube-firstboot.log"}}
	}

	return map[string]any{
		"version":  1,
		"source":   map[string]any{"id": "ubuntu-server-minimal"},
		"locale":   c.Locale,
		"keyboard": map[string]any{"layout": c.Keyboard},
		"timezone": c.Timezone,
		"network":  c.netplan(),
		"storage":  c.storage(),
		"ssh":      map[string]any{"install-server": true, "allow-pw": false},
		"packages": []string{"xfsprogs", "lvm2", "git", "make", "curl", "jq", "ca-certificates"},
		"updates":  "security",
		"shutdown": c.Install.After,
		// 残りの領域を XFS の /var/lib/niwakube にする (VM のディスクを reflink で複製できるように)
		"late-commands": []string{
			"lvcreate -y -l 100%FREE -n niwakube ubuntu-vg",
			"mkfs.xfs -f /dev/ubuntu-vg/niwakube",
			"mkdir -p /target/var/lib/niwakube",
			`echo "UUID=$(blkid -s UUID -o value /dev/ubuntu-vg/niwakube) /var/lib/niwakube xfs defaults 0 2" >> /target/etc/fstab`,
		},
		"user-data": userData,
	}, nil
}

func (c *Config) netplan() map[string]any {
	eth := map[string]any{"match": map[string]any{"name": "en*"}}
	if n := c.Network; n == nil {
		eth["dhcp4"] = true
	} else {
		eth["dhcp4"] = false
		eth["addresses"] = []string{n.Address}
		eth["routes"] = []map[string]any{{"to": "default", "via": n.Gateway}}
		if len(n.DNS) > 0 {
			eth["nameservers"] = map[string]any{"addresses": n.DNS}
		}
	}
	return map[string]any{"version": 2, "ethernets": map[string]any{"lan": eth}}
}

// storage は curtin のストレージ設定。GPT + ESP 1G + /boot 2G + LVM (ubuntu-vg)。
// ルートの LV は root_size、残りは late-commands で /var/lib/niwakube (XFS) にする。
func (c *Config) storage() map[string]any {
	match := map[string]any{}
	switch d := c.Storage.Disk; {
	case d.Serial != "":
		match["serial"] = diskGlob(d.Serial)
	case d.Model != "":
		match["model"] = diskGlob(d.Model)
	case d.Internal:
		match["id_path"] = internalIDPath
		match["size"] = "largest"
	default:
		match["size"] = "largest"
	}
	cfg := []map[string]any{
		{"type": "disk", "id": "disk0", "match": match, "ptable": "gpt", "wipe": "superblock-recursive", "preserve": false, "grub_device": false},
		{"type": "partition", "id": "esp", "device": "disk0", "number": 1, "size": "1G", "flag": "boot", "grub_device": true, "wipe": "superblock", "preserve": false},
		{"type": "format", "id": "esp-fs", "volume": "esp", "fstype": "fat32", "preserve": false},
		{"type": "partition", "id": "boot", "device": "disk0", "number": 2, "size": "2G", "wipe": "superblock", "preserve": false},
		{"type": "format", "id": "boot-fs", "volume": "boot", "fstype": "ext4", "preserve": false},
		{"type": "partition", "id": "pv", "device": "disk0", "number": 3, "size": -1, "wipe": "superblock", "preserve": false},
		{"type": "lvm_volgroup", "id": "vg0", "name": "ubuntu-vg", "devices": []string{"pv"}, "preserve": false},
		{"type": "lvm_partition", "id": "root", "volgroup": "vg0", "name": "ubuntu-lv", "size": c.Storage.RootSize, "wipe": "superblock", "preserve": false},
		{"type": "format", "id": "root-fs", "volume": "root", "fstype": "ext4", "preserve": false},
		{"type": "mount", "id": "m-root", "device": "root-fs", "path": "/"},
		{"type": "mount", "id": "m-boot", "device": "boot-fs", "path": "/boot"},
		{"type": "mount", "id": "m-esp", "device": "esp-fs", "path": "/boot/efi"},
	}
	return map[string]any{"swap": map[string]any{"size": 0}, "config": cfg}
}

// internalIDPath は内蔵の SATA / NVMe のディスクに合う udev の ID_PATH のパターン。
// 例: SATA "pci-0000:00:17.0-ata-1" (または ata-1.0)、NVMe "pci-0000:01:00.0-nvme-1"。
// USB のディスクは "pci-0000:00:14.0-usb-0:1:1.0-scsi-0:0:0:0" なので合わない。
// subiquity の照合 (fnmatch) には「または」が無いので、"-ata" と "-nvm" の両方に合う文字クラスにしている。
// 起動に使った USB は、size を指定すると使用中のディスクとして外される。
const internalIDPath = "*-[an][tv][am]*"

// diskGlob は lsblk で見えるモデル名 / シリアル番号を、curtin の照合に使うパターンに変換する。
// udev の値は lsblk の表示と違い、スペースが "_" になったり (ID_MODEL)、NVMe ではシリアルの前に
// モデル名が付いたりする (ID_SERIAL = "モデル名_シリアル")。そのため前後を * で囲み、スペースは
// 任意の 1 文字 (?) にする。例: "Samsung SSD 980 1TB" → "*Samsung?SSD?980?1TB*"
func diskGlob(s string) string {
	return "*" + strings.ReplaceAll(s, " ", "?") + "*"
}

// deepMerge は src を dst に再帰的にマージする (map 同士はマージ、それ以外は src で上書き)。
func deepMerge(dst, src map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func (c *Config) firstbootScript() (string, error) {
	t := template.Must(template.New("").Parse(firstbootTmpl))
	var buf bytes.Buffer
	err := t.Execute(&buf, map[string]string{"Repo": c.Niwakube.Repo, "Ref": c.Niwakube.Ref})
	return buf.String(), err
}

// firstbootTmpl はインストール後の初回起動で niwakube のホストを準備する。
const firstbootTmpl = `#!/bin/bash
# niwakube: 初回起動時のセットアップ (installer/config.yaml の niwakube.setup で無効にできる)
set -euxo pipefail
REPO="{{.Repo}}"
REF="{{.Ref}}"

rm -rf /opt/niwakube
git clone --depth 1 --branch "$REF" "https://github.com/$REPO.git" /opt/niwakube
cd /opt/niwakube
./scripts/host-setup.sh

case "$REF" in
  v*)
    curl -fL -o /usr/local/bin/niwakubed "https://github.com/$REPO/releases/download/$REF/niwakubed-linux-amd64"
    chmod 0755 /usr/local/bin/niwakubed
    install -m 0644 deploy/niwakubed.service /etc/systemd/system/niwakubed.service
    systemctl daemon-reload
    systemctl enable --now niwakubed
    ;;
  *)
    echo "niwakube.ref ($REF) が v* のタグではないので niwakubed のインストールは省略します"
    ;;
esac
touch /var/lib/niwakube/.firstboot-done
`

// Summary は人が確認するための設定の要約を返す。
func (c *Config) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hostname: %s\n", c.Hostname)
	fmt.Fprintf(&b, "user:     %s (SSH 鍵のみ)\n", c.User.Name)
	if c.Network == nil {
		b.WriteString("network:  DHCP\n")
	} else {
		fmt.Fprintf(&b, "network:  %s via %s\n", c.Network.Address, c.Network.Gateway)
	}
	fmt.Fprintf(&b, "disk:     %s (root %s, 残りは /var/lib/niwakube)\n", c.Storage.Disk, c.Storage.RootSize)
	fmt.Fprintf(&b, "install:  %s, %s after install\n", c.Install.Mode, c.Install.After)
	if c.Install.ISOMirror != "" {
		fmt.Fprintf(&b, "iso:      %s (止まったら releases.ubuntu.com と交互に試す)\n", c.Install.ISOMirror)
	} else {
		b.WriteString("iso:      https://releases.ubuntu.com\n")
	}
	return b.String()
}
