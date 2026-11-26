package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarBytes(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func layer(t *testing.T, entries []entry) v1.Layer {
	t.Helper()
	b := tarBytes(t, entries)
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPull(t *testing.T) {
	// レジストリをテストの中で動かし、2 層のイメージを置く (2 層目で 1 層目のファイルを消す)
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	img, err := mutate.AppendLayers(empty.Image,
		layer(t, []entry{
			{name: "sbin/", typ: tar.TypeDir, mode: 0o755},
			{name: "sbin/init", typ: tar.TypeReg, body: "#!init", mode: 0o755},
			{name: "etc/", typ: tar.TypeDir, mode: 0o755},
			{name: "etc/os-release", typ: tar.TypeReg, body: "NAME=Talos"},
			{name: "etc/removed", typ: tar.TypeReg, body: "gone"},
			{name: "bin", typ: tar.TypeSymlink, link: "sbin"},
		}),
		layer(t, []entry{
			{name: "etc/.wh.removed", typ: tar.TypeReg},
			{name: "etc/added", typ: tar.TypeReg, body: "new"},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(u.Host + "/siderolabs/talos:v1.14.7")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "talos-v1.14.7.rootfs")
	if err := Pull(context.Background(), ref.String(), dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sbin", "init")); string(b) != "#!init" {
		t.Errorf("sbin/init = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dir, "sbin", "init")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("sbin/init mode: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc", "added")); string(b) != "new" {
		t.Errorf("etc/added = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "etc", "removed")); !os.IsNotExist(err) {
		t.Errorf("whiteout must remove etc/removed: %v", err)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "bin")); l != "sbin" {
		t.Errorf("bin -> %q", l)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary directory must be renamed")
	}
}

func TestUntarRejectsEscapes(t *testing.T) {
	for name, entries := range map[string][]entry{
		"dot dot":         {{name: "../evil", typ: tar.TypeReg, body: "x"}},
		"through symlink": {{name: "link", typ: tar.TypeSymlink, link: "/tmp"}, {name: "link/evil", typ: tar.TypeReg, body: "x"}},
		"hardlink out":    {{name: "h", typ: tar.TypeLink, link: "../../etc/passwd"}},
	} {
		dir := t.TempDir()
		err := Untar(bytes.NewReader(tarBytes(t, entries)), dir)
		if name == "dot dot" || name == "hardlink out" {
			// ".." はルートの中に丸められる (Clean("/../evil") = "/evil")。ルートの外には書かない
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); statErr == nil {
				t.Errorf("%s: wrote outside the root", name)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: want an error, got %v", name, err)
		}
	}
}
