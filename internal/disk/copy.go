// Package disk はディスクイメージの操作をまとめる。
package disk

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const blockSize = 1 << 20

// CloneOrCopy は src を dst (新規作成) に複製する。
//
// XFS / btrfs など reflink に対応したファイルシステムでは FICLONE で一瞬で複製し、容量もほとんど使わない
// (reflinked = true)。対応していなければ、ゼロだけのブロックを書かずに飛ばす (穴を空ける) 通常のコピーをする。
// cp コマンドに頼らないのは、ディストリビューションによって cp の実装やフラグの扱いが違うため。
func CloneOrCopy(src, dst string) (reflinked bool, err error) {
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) == nil {
		return true, nil
	}
	if err := sparseCopy(out, in); err != nil {
		return false, fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	// 末尾がゼロで Seek だけしていた場合に備えてサイズを合わせる
	return false, out.Truncate(fi.Size())
}

func sparseCopy(out, in *os.File) error {
	buf := make([]byte, blockSize)
	for {
		n, rerr := io.ReadFull(in, buf)
		if n > 0 {
			if allZero(buf[:n]) {
				if _, err := out.Seek(int64(n), io.SeekCurrent); err != nil {
					return err
				}
			} else if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Grow はファイルを size バイトまで広げる (増えた分は穴になる)。元のサイズより小さい size はエラー。
func Grow(path string, size int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() > size {
		return fmt.Errorf("requested size %d bytes is smaller than %s (%d bytes)", size, path, fi.Size())
	}
	return os.Truncate(path, size)
}
