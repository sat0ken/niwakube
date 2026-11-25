package disk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneOrCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.raw")

	// データ / ゼロ / データ / 末尾ゼロ の並び
	var want []byte
	want = append(want, bytes.Repeat([]byte{0xab}, 1000)...)
	want = append(want, make([]byte, 3*blockSize)...)
	want = append(want, []byte("middle")...)
	want = append(want, make([]byte, 2*blockSize+17)...)
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.raw")
	reflinked, err := CloneOrCopy(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reflinked = %v", reflinked)
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}

	// 既存のファイルは上書きしない
	if _, err := CloneOrCopy(src, dst); err == nil {
		t.Fatal("expected error when dst exists")
	}
	// 失敗しても dst は残る (上書きも削除もしない)
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("dst removed: %v", err)
	}
	// src が無ければエラーで、dst は作られない
	if _, err := CloneOrCopy(filepath.Join(dir, "nope"), filepath.Join(dir, "x")); err == nil {
		t.Fatal("expected error for missing src")
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Fatalf("dst should not exist: %v", err)
	}
}

func TestGrow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.raw")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Grow(p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Size() != 1<<20 {
		t.Fatalf("size = %d", fi.Size())
	}
	if err := Grow(p, 10); err == nil {
		t.Fatal("expected error when shrinking")
	}
}
