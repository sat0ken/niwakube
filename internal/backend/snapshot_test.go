package backend

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotAndRestoreDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "vms", "n1")
	os.MkdirAll(dir, 0o750)
	os.WriteFile(filepath.Join(dir, "disk.raw"), []byte("v1"), 0o600)
	snap := filepath.Join(root, "snap", "n1")
	os.MkdirAll(filepath.Dir(snap), 0o700)
	if err := SnapshotDir(dir, snap); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "disk.raw"), []byte("v2"), 0o600)
	os.WriteFile(filepath.Join(dir, "added"), []byte("x"), 0o600)
	if err := RestoreDir(dir, snap); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "disk.raw")); string(b) != "v1" {
		t.Errorf("disk = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "added")); !os.IsNotExist(err) {
		t.Error("files added after the snapshot must be gone")
	}
	for _, leftover := range []string{dir + ".old", dir + ".restoring"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s must not remain", leftover)
		}
	}
	// 消えたノードも戻せる
	os.RemoveAll(dir)
	if err := RestoreDir(dir, snap); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "disk.raw")); string(b) != "v1" {
		t.Errorf("recreated disk = %q", b)
	}
	// 元が無ければ失敗し、ノードのディレクトリはそのまま
	if err := RestoreDir(dir, filepath.Join(root, "nope")); err == nil {
		t.Error("want an error")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "disk.raw")); string(b) != "v1" {
		t.Errorf("a failed restore must not touch the node: %q", b)
	}
	if err := SnapshotDir(filepath.Join(root, "vms", "nope"), filepath.Join(root, "x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown node: %v", err)
	}
}
