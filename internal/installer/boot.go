package installer

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
)

//go:embed assets/30mini-iso-menu
var premountScript []byte

// ServerISO はインストーラーがダウンロードする Ubuntu Server の ISO。
type ServerISO struct {
	Version string // 例: 26.04.1
	URL     string // releases.ubuntu.com の URL
	Size    int64
	SHA256  string
	Mirrors []string // URL より先に試す URL (install.iso_mirror)
}

func (s ServerISO) validate() error {
	if !strings.HasPrefix(s.URL, "https://") || !strings.HasSuffix(s.URL, ".iso") || s.Size <= 0 || len(s.SHA256) != 64 {
		return fmt.Errorf("invalid server ISO info: %+v", s)
	}
	for _, m := range s.Mirrors {
		if !mirrorRe.MatchString(m) || !strings.HasSuffix(m, ".iso") {
			return fmt.Errorf("invalid mirror URL: %q", m)
		}
	}
	return nil
}

// URLs はダウンロードを試す順の URL。インストーラーには "," でつないで渡す。
func (s ServerISO) URLs() []string {
	return append(append([]string{}, s.Mirrors...), s.URL)
}

// officialBase は ServerISO.URL の取得元。ミラーはこの下と同じ構成 (<release>/<file>) になっている。
const officialBase = "https://releases.ubuntu.com"

// MirrorURL は ServerISO.URL を install.iso_mirror の下の URL に置き換える。
func MirrorURL(mirror, official string) (string, error) {
	rest, ok := strings.CutPrefix(official, officialBase+"/")
	if !ok {
		return "", fmt.Errorf("server ISO URL is not under %s: %s", officialBase, official)
	}
	return strings.TrimRight(mirror, "/") + "/" + rest, nil
}

// CheckMirror はミラーに同じ大きさの ISO があることを確かめる (ミラーの同期が遅れていることがあるため)。
// 中身は、インストール時に SHA256 で確かめる。
func CheckMirror(ctx context.Context, client *http.Client, url string, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	if resp.ContentLength != size {
		return fmt.Errorf("%s: size %d, want %d (the mirror may not be in sync yet)", url, resp.ContentLength, size)
	}
	return nil
}

type grubEntry struct {
	Title string
	Args  string
}

const grubTmpl = `# niwakube installer ({{.Hostname}}) - generated from installer/config.yaml
set timeout={{.Timeout}}
set default={{.Default}}
{{if .Serial}}
serial --unit=0 --speed=115200
terminal_input console serial
terminal_output console serial
{{end}}
loadfont unicode

set menu_color_normal=white/black
set menu_color_highlight=black/light-gray
{{range .Entries}}
menuentry "{{.Title}}" {
    set gfxpayload=keep
    linux  /casper/vmlinuz {{.Args}} ---
    initrd /casper/initrd
}
{{end}}`

// GrubConfig は ISO の /boot/grub/grub.cfg を返す。
func (c *Config) GrubConfig(iso ServerISO) ([]byte, error) {
	if err := iso.validate(); err != nil {
		return nil, err
	}
	base := fmt.Sprintf("iso-chooser-menu ip=dhcp niwakube-iso-url=%s niwakube-iso-size=%d niwakube-iso-256sum=%s nk.ds=nocloud",
		strings.Join(iso.URLs(), ","), iso.Size, iso.SHA256)
	for _, a := range c.Install.KernelArgs {
		base += " nk." + a
	}
	if c.serialConsole {
		// mini ISO のカーネル (1 段目・2 段目) の出力もシリアルに出す
		base += " console=tty0 console=ttyS0,115200"
	}
	entries := []grubEntry{
		{Title: fmt.Sprintf("niwakube: install Ubuntu Server %s on %s (asks before erasing the disk)", iso.Version, c.Hostname), Args: base},
	}
	def := 0
	// 無人インストールの項目は、ディスクをモデル名かシリアル番号で指定したときだけ出す
	// ("largest" のままだと、どの PC で起動しても一番大きいディスクを確認なしで消してしまうため)
	if !c.Storage.Disk.Largest {
		entries = append(entries, grubEntry{
			Title: fmt.Sprintf("niwakube: UNATTENDED install - ERASES THE DISK WITH %s WITHOUT ASKING", strings.ToUpper(c.Storage.Disk.String())),
			Args:  base + " nk.autoinstall",
		})
		if c.Install.Mode == "unattended" {
			def = 1
		}
	}
	entries = append(entries, grubEntry{Title: "Choose an Ubuntu version to install (original mini ISO menu)", Args: "iso-chooser-menu ip=dhcp"})
	var buf bytes.Buffer
	err := template.Must(template.New("").Parse(grubTmpl)).Execute(&buf, map[string]any{
		"Hostname": c.Hostname, "Timeout": c.Install.Timeout, "Default": def, "Entries": entries, "Serial": c.serialConsole,
	})
	return buf.Bytes(), err
}

// cpio の newc 形式の 1 エントリを書く。
func writeCpioEntry(w io.Writer, ino int, name string, mode uint32, data []byte) error {
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
	var buf bytes.Buffer
	buf.WriteString(hdr)
	buf.WriteString(name)
	buf.WriteByte(0)
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
	buf.Write(data)
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// InitrdOverlay は mini ISO の initrd の末尾に足す cpio を返す。
// Linux は連結された cpio を順に展開するので、後ろのファイルが前のファイルを上書きする。
func InitrdOverlay() ([]byte, error) {
	var buf bytes.Buffer
	entries := []struct {
		name string
		mode uint32
		data []byte
	}{
		{"scripts", 0o040755, nil},
		{"scripts/casper-premount", 0o040755, nil},
		{"scripts/casper-premount/30mini-iso-menu", 0o100755, premountScript},
	}
	for i, e := range entries {
		if err := writeCpioEntry(&buf, 900000+i, e.name, e.mode, e.data); err != nil {
			return nil, err
		}
	}
	if err := writeCpioEntry(&buf, 0, "TRAILER!!!", 0, nil); err != nil {
		return nil, err
	}
	// 512 バイト境界まで埋める (cpio の慣習)
	for buf.Len()%512 != 0 {
		buf.WriteByte(0)
	}
	return buf.Bytes(), nil
}
