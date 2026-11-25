package installer

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/sys"
)

const sample = `
hostname: niwa
timezone: Asia/Tokyo
keyboard: jp
user:
  name: sat0ken
  github_keys: sat0ken
network:
  address: 192.168.1.50/24
  gateway: 192.168.1.1
  dns: [192.168.1.1]
storage:
  disk: {model: "Samsung SSD 980 1TB"}
  root_size: 80G
install:
  mode: unattended
niwakube:
  ref: v0.3.0
autoinstall:
  packages: [htop]
  keyboard: {variant: ""}
`

var testISO = ServerISO{
	Version: "26.04.1",
	URL:     "https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso",
	Size:    2927861760,
	SHA256:  strings.Repeat("a", 64),
}

func parse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUserData(t *testing.T) {
	c := parse(t, sample)
	ud, err := c.UserData()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ud, []byte("#cloud-config\n")) {
		t.Fatal("missing #cloud-config")
	}
	var doc struct {
		Autoinstall map[string]any `yaml:"autoinstall"`
	}
	if err := yaml.Unmarshal(ud, &doc); err != nil {
		t.Fatal(err)
	}
	ai := doc.Autoinstall
	if ai["version"] != 1 || ai["timezone"] != "Asia/Tokyo" || ai["shutdown"] != "poweroff" {
		t.Errorf("autoinstall: %v", ai)
	}
	if ai["source"].(map[string]any)["id"] != "ubuntu-server-minimal" {
		t.Error("must install the minimized server")
	}
	if ssh := ai["ssh"].(map[string]any); ssh["allow-pw"] != false || ssh["install-server"] != true {
		t.Errorf("ssh: %v", ssh)
	}
	// autoinstall: で上書きした値 (packages は置き換え、keyboard はマージ)
	if pk := ai["packages"].([]any); len(pk) != 1 || pk[0] != "htop" {
		t.Errorf("packages override: %v", pk)
	}
	if kb := ai["keyboard"].(map[string]any); kb["layout"] != "jp" || kb["variant"] != "" {
		t.Errorf("keyboard merge: %v", kb)
	}

	// 固定 IP
	lan := ai["network"].(map[string]any)["ethernets"].(map[string]any)["lan"].(map[string]any)
	if lan["dhcp4"] != false || lan["addresses"].([]any)[0] != "192.168.1.50/24" {
		t.Errorf("network: %v", lan)
	}

	// ディスクはモデル名で選び、ルートは 80G、残りは late-commands で XFS
	cfg := ai["storage"].(map[string]any)["config"].([]any)
	disk := cfg[0].(map[string]any)
	if disk["match"].(map[string]any)["model"] != "*Samsung?SSD?980?1TB*" {
		t.Errorf("disk match: %v", disk)
	}
	foundRoot := false
	for _, e := range cfg {
		m := e.(map[string]any)
		if m["type"] == "lvm_partition" && m["name"] == "ubuntu-lv" && m["size"] == "80G" {
			foundRoot = true
		}
	}
	if !foundRoot {
		t.Error("root LV with root_size not found")
	}
	late := strings.Join(toStrings(ai["late-commands"]), "\n")
	if !strings.Contains(late, "mkfs.xfs -f /dev/ubuntu-vg/niwakube") || !strings.Contains(late, "/var/lib/niwakube xfs") {
		t.Errorf("late-commands: %s", late)
	}

	// ユーザーはパスワードをロックし、GitHub の鍵を取り込む
	ud2 := ai["user-data"].(map[string]any)
	user := ud2["users"].([]any)[0].(map[string]any)
	if user["name"] != "sat0ken" || user["lock_passwd"] != true || user["ssh_import_id"].([]any)[0] != "gh:sat0ken" {
		t.Errorf("user: %v", user)
	}
	if _, ok := user["passwd"]; ok {
		t.Error("password hash must never be written")
	}
	fb := ud2["write_files"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(fb, `REF="v0.3.0"`) || !strings.Contains(fb, "./scripts/host-setup.sh") {
		t.Errorf("firstboot: %s", fb)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestGrubConfig(t *testing.T) {
	// 無人インストール (モデル名を指定) → 無人の項目が既定
	c := parse(t, sample)
	g, err := c.GrubConfig(testISO)
	if err != nil {
		t.Fatal(err)
	}
	s := string(g)
	if !strings.Contains(s, "set default=1") || strings.Count(s, "menuentry") != 3 {
		t.Errorf("grub (unattended):\n%s", s)
	}
	if !strings.Contains(s, "niwakube-iso-size=2927861760") || !strings.Contains(s, "nk.ds=nocloud") || !strings.Contains(s, " nk.autoinstall ---") {
		t.Errorf("grub args:\n%s", s)
	}

	// 確認ありで、ディスクが largest → 無人の項目は出さない
	c = parse(t, "hostname: h\nuser: {name: u, github_keys: u}\n")
	g, err = c.GrubConfig(testISO)
	if err != nil {
		t.Fatal(err)
	}
	s = string(g)
	if strings.Contains(s, "nk.autoinstall") || strings.Count(s, "menuentry") != 2 || !strings.Contains(s, "set default=0") {
		t.Errorf("grub (confirm, largest):\n%s", s)
	}

	if _, err := c.GrubConfig(ServerISO{URL: "http://x"}); err == nil {
		t.Error("invalid server ISO must be rejected")
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]string{
		"no ssh key":           "hostname: h\nuser: {name: u}\n",
		"bad hostname":         "hostname: Bad_Host\nuser: {name: u, github_keys: u}\n",
		"bad disk word":        "hostname: h\nuser: {name: u, github_keys: u}\nstorage: {disk: external}\n",
		"unattended + largest": "hostname: h\nuser: {name: u, github_keys: u}\ninstall: {mode: unattended}\n",
		"model and serial":     "hostname: h\nuser: {name: u, github_keys: u}\nstorage: {disk: {model: a, serial: b}}\n",
		"bad disk scalar":      "hostname: h\nuser: {name: u, github_keys: u}\nstorage: {disk: smallest}\n",
		"bad root size":        "hostname: h\nuser: {name: u, github_keys: u}\nstorage: {root_size: 100GB}\n",
		"bad address":          "hostname: h\nuser: {name: u, github_keys: u}\nnetwork: {address: 192.168.1.50, gateway: 192.168.1.1}\n",
		"unknown field":        "hostname: h\nuser: {name: u, github_keys: u}\npassword: secret\n",
		"bad kernel arg":       "hostname: h\nuser: {name: u, github_keys: u}\ninstall: {kernel_args: [\"a;reboot\"]}\n",
		"not a key":            "hostname: h\nuser: {name: u, ssh_authorized_keys: [hello]}\n",
		"bad ref":              "hostname: h\nuser: {name: u, github_keys: u}\nniwakube: {ref: \"main; rm -rf /\"}\n",
		"bad timezone":         "hostname: h\nuser: {name: u, github_keys: u}\ntimezone: \"Asia/Tokyo $(id)\"\n",
		"glob in disk serial":  "hostname: h\nuser: {name: u, github_keys: u}\nstorage: {disk: {serial: \"*\"}}\n",
		"mirror not a URL":     "hostname: h\nuser: {name: u, github_keys: u}\ninstall: {iso_mirror: ftp.jaist.ac.jp}\n",
		"mirror with comma":    "hostname: h\nuser: {name: u, github_keys: u}\ninstall: {iso_mirror: \"https://a.example/x,https://b.example\"}\n",
		"mirror with space":    "hostname: h\nuser: {name: u, github_keys: u}\ninstall: {iso_mirror: \"https://a.example/x y\"}\n",
	}
	for name, s := range bad {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c := parse(t, "hostname: h\nuser: {name: u, github_keys: u}\n")
	if c.Timezone != "Etc/UTC" || c.Install.Mode != "confirm" || !c.Storage.Disk.Largest || c.Ubuntu.Release != "26.04" || !c.Niwakube.Enabled() {
		t.Errorf("defaults: %+v", c)
	}
}

func TestForTest(t *testing.T) {
	c := parse(t, sample)
	c.ForTest()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Network != nil || c.Storage.Disk.Serial != TestDiskSerial || c.Install.Mode != "unattended" {
		t.Errorf("test overrides: %+v", c)
	}
	g, _ := c.GrubConfig(testISO)
	if !strings.Contains(string(g), "nk.console=ttyS0,115200") {
		t.Error("test ISO must log to the serial console")
	}
}

// TestInitrdOverlay は追加する cpio が newc 形式として正しく読めることを確かめる。
func TestInitrdOverlay(t *testing.T) {
	b, err := InitrdOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if len(b)%512 != 0 {
		t.Errorf("overlay must be padded to 512 bytes: %d", len(b))
	}
	files := map[string][]byte{}
	off := 0
	for {
		if string(b[off:off+6]) != "070701" {
			t.Fatalf("bad magic at %d", off)
		}
		field := func(i int) int {
			v, err := strconv.ParseUint(string(b[off+6+8*i:off+14+8*i]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			return int(v)
		}
		size, nameSize := field(6), field(11)
		name := string(b[off+110 : off+110+nameSize-1])
		p := (off + 110 + nameSize + 3) &^ 3
		if name == "TRAILER!!!" {
			break
		}
		files[name] = b[p : p+size]
		off = (p + size + 3) &^ 3
	}
	script := files["scripts/casper-premount/30mini-iso-menu"]
	if !bytes.HasPrefix(script, []byte("#!/bin/sh")) || !bytes.Contains(script, []byte("niwakube-iso-url=")) {
		t.Errorf("premount script not found in overlay: %v", len(files))
	}
}

// TestBuildPadsInitrd は、元の initrd のサイズが 4 バイト境界に揃っていなくても、
// 追加する cpio が 512 バイト境界から始まることを確かめる (揃っていないとカーネルが読めない)。
func TestBuildPadsInitrd(t *testing.T) {
	c := parse(t, "hostname: h\nuser: {name: u, github_keys: u}\n")
	work := t.TempDir()
	var initrd []byte
	r := &sys.FakeRunner{Handler: func(name string, args []string) ([]byte, error) {
		switch {
		case name == "xorriso" && args[0] == "-osirrox":
			return nil, os.WriteFile(args[len(args)-1], bytes.Repeat([]byte{0x28}, 1001), 0o444) // 4 の倍数でないサイズ
		case name == "xorriso":
			b, err := os.ReadFile(filepath.Join(work, "initrd"))
			initrd = b
			return nil, err
		}
		return nil, nil
	}}
	if err := c.Build(context.Background(), r, BuildOptions{MiniISO: "mini.iso", Server: testISO, Output: filepath.Join(work, "out.iso"), WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(initrd, []byte("070701"))
	if i != 1024 || !bytes.Equal(initrd[1001:1024], make([]byte, 23)) {
		t.Errorf("overlay must start at a 512-byte boundary after zero padding: got offset %d", i)
	}
}

func TestISOMirror(t *testing.T) {
	c := parse(t, "hostname: h\nuser: {name: u, github_keys: u}\ninstall: {iso_mirror: \"https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases/\"}\n")
	if c.Install.ISOMirror != "https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases" {
		t.Fatalf("mirror = %q", c.Install.ISOMirror)
	}
	u, err := MirrorURL(c.Install.ISOMirror, testISO.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases/26.04/ubuntu-26.04.1-live-server-amd64.iso"; u != want {
		t.Errorf("mirror URL = %q, want %q", u, want)
	}
	if _, err := MirrorURL(c.Install.ISOMirror, "https://example.com/26.04/x.iso"); err == nil {
		t.Error("URL outside releases.ubuntu.com must be rejected")
	}

	iso := testISO
	iso.Mirrors = []string{u}
	g, err := c.GrubConfig(iso)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(g), "niwakube-iso-url="+u+","+testISO.URL+" ") {
		t.Errorf("grub must list the mirror first, then releases.ubuntu.com:\n%s", g)
	}
	if !strings.Contains(c.Summary(), "iso:      https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases") {
		t.Errorf("summary:\n%s", c.Summary())
	}
}

func TestCheckMirror(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/26.04/ok.iso":
			w.Header().Set("Content-Length", "1234")
		case "/26.04/old.iso":
			w.Header().Set("Content-Length", "999")
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
	}))
	defer ts.Close()
	ctx := context.Background()
	if err := CheckMirror(ctx, ts.Client(), ts.URL+"/26.04/ok.iso", 1234); err != nil {
		t.Errorf("ok: %v", err)
	}
	if err := CheckMirror(ctx, ts.Client(), ts.URL+"/26.04/old.iso", 1234); err == nil {
		t.Error("size mismatch must fail")
	}
	if err := CheckMirror(ctx, ts.Client(), ts.URL+"/26.04/missing.iso", 1234); err == nil {
		t.Error("404 must fail")
	}
}

func TestDiskInternal(t *testing.T) {
	c := parse(t, "hostname: h\nuser: {name: u, github_keys: u}\nstorage: {disk: internal}\ninstall: {mode: unattended}\n")
	if !c.Storage.Disk.Internal || c.Storage.Disk.Largest {
		t.Fatalf("disk = %+v", c.Storage.Disk)
	}
	ud, err := c.UserData()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ud), "id_path: '*-[an][tv][am]*'") || !strings.Contains(string(ud), "size: largest") {
		t.Errorf("match must select the largest SATA / NVMe disk:\n%s", ud)
	}
	g, err := c.GrubConfig(testISO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(g), "ERASES THE LARGEST INTERNAL DISK (SATA / NVMe) WITHOUT ASKING") || !strings.Contains(string(g), "set default=1") {
		t.Errorf("grub:\n%s", g)
	}
	// CI の VM でも internal のまま試す
	c.ForTest()
	if !c.Storage.Disk.Internal {
		t.Errorf("ForTest must keep internal: %+v", c.Storage.Disk)
	}

	// subiquity は fnmatch で照合する。ID_PATH に "/" は入らないので path.Match と同じ結果になる
	for idPath, want := range map[string]bool{
		"pci-0000:00:17.0-ata-1":                    true,
		"pci-0000:00:17.0-ata-1.0":                  true,
		"pci-0000:01:00.0-nvme-1":                   true,
		"pci-0000:00:14.0-usb-0:1:1.0-scsi-0:0:0:0": false,
		"pci-0000:00:04.0":                          false,
		"pci-0000:03:00.0-sas-phy0-lun-0":           false,
		"pci-0000:00:12.0-mmc-0x1":                  false,
	} {
		got, err := path.Match(internalIDPath, idPath)
		if err != nil || got != want {
			t.Errorf("%s: match = %v (%v), want %v", idPath, got, err, want)
		}
	}
}

func TestNetworkIPv6Off(t *testing.T) {
	c := parse(t, "hostname: h\nuser: {name: u, github_keys: u}\nnetwork: {address: 192.168.0.50/24, gateway: 192.168.0.1, dns: [1.1.1.1, 8.8.8.8], ipv6: false}\n")
	ud, err := c.UserData()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"accept-ra: false", "- 1.1.1.1", "- 8.8.8.8", "dhcp4: false"} {
		if !strings.Contains(string(ud), want) {
			t.Errorf("user-data does not contain %q:\n%s", want, ud)
		}
	}
	if !strings.Contains(c.Summary(), "DNS 1.1.1.1 8.8.8.8, IPv6 off") {
		t.Errorf("summary:\n%s", c.Summary())
	}
	// 書かなければ RA を受け取る (既定)
	c = parse(t, "hostname: h\nuser: {name: u, github_keys: u}\nnetwork: {address: 192.168.0.50/24, gateway: 192.168.0.1}\n")
	ud, _ = c.UserData()
	if strings.Contains(string(ud), "accept-ra") {
		t.Errorf("accept-ra must not be set by default:\n%s", ud)
	}
}
