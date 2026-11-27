package disk

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCopyTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	must(os.WriteFile(filepath.Join(src, "a", "file"), []byte("hello"), 0o640))
	must(os.Link(filepath.Join(src, "a", "file"), filepath.Join(src, "hardlink")))
	must(os.Symlink("a/file", filepath.Join(src, "link")))
	must(unix.Mkfifo(filepath.Join(src, "fifo"), 0o600))
	// 穴のある大きなファイル (スナップショットのディスク)
	f, _ := os.Create(filepath.Join(src, "disk.raw"))
	must(f.Truncate(64 << 20))
	_, _ = f.WriteAt([]byte("tail"), 64<<20-4)
	f.Close()
	must(os.Chmod(filepath.Join(src, "a", "b"), 0o500)) // 読み取り専用のディレクトリ
	must(unix.Chmod(filepath.Join(src, "a", "file"), 0o2640))
	_ = unix.Lsetxattr(filepath.Join(src, "a", "file"), "user.niwakube", []byte("v"), 0) // tmpfs などで使えなくてもよい
	if os.Geteuid() == 0 {
		must(unix.Mknod(filepath.Join(src, "whiteout"), unix.S_IFCHR|0o000, 0)) // overlayfs の whiteout
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "a", "b"), 0o755) })

	dst := filepath.Join(t.TempDir(), "dst")
	must(CopyTree(src, dst))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, "a", "b"), 0o755) })

	if b, _ := os.ReadFile(filepath.Join(dst, "a", "file")); string(b) != "hello" {
		t.Errorf("file = %q", b)
	}
	var a, h unix.Stat_t
	_ = unix.Stat(filepath.Join(dst, "a", "file"), &a)
	_ = unix.Stat(filepath.Join(dst, "hardlink"), &h)
	if a.Ino != h.Ino {
		t.Error("hard links must stay hard links")
	}
	if a.Mode&0o7777 != 0o2640 {
		t.Errorf("mode = %o", a.Mode&0o7777)
	}
	if l, _ := os.Readlink(filepath.Join(dst, "link")); l != "a/file" {
		t.Errorf("symlink -> %q", l)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "fifo")); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("fifo: %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "a", "b")); fi.Mode().Perm() != 0o500 {
		t.Errorf("dir mode = %v", fi.Mode())
	}
	var ds unix.Stat_t
	_ = unix.Stat(filepath.Join(dst, "disk.raw"), &ds)
	if ds.Size != 64<<20 || ds.Blocks*512 > 8<<20 {
		t.Errorf("disk: size %d, allocated %d (must stay sparse)", ds.Size, ds.Blocks*512)
	}
	sv := make([]byte, 8)
	if n, err := unix.Lgetxattr(filepath.Join(src, "a", "file"), "user.niwakube", sv); err == nil {
		dv := make([]byte, 8)
		if m, err := unix.Lgetxattr(filepath.Join(dst, "a", "file"), "user.niwakube", dv); err != nil || string(dv[:m]) != string(sv[:n]) {
			t.Errorf("xattr: %q %v", dv[:m], err)
		}
	}
	if os.Geteuid() == 0 {
		var w unix.Stat_t
		if err := unix.Lstat(filepath.Join(dst, "whiteout"), &w); err != nil || w.Mode&unix.S_IFMT != unix.S_IFCHR || w.Rdev != 0 {
			t.Errorf("whiteout: %+v %v", w, err)
		}
	}
	// 既にあるところには複製しない
	if err := CopyTree(src, dst); err == nil {
		t.Error("copying onto an existing directory must fail")
	}
}
