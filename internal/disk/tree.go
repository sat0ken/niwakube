package disk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// CopyTree はディレクトリ src を dst (まだ無いこと) に丸ごと複製する (スナップショットに使う)。
//
//   - 通常のファイルは CloneOrCopy で複製する (reflink できれば一瞬、できなければ穴を保つコピー)
//   - 所有者・パーミッション・更新時刻・拡張属性 (overlayfs の trusted.overlay.* や file capabilities) を保つ
//   - シンボリックリンク・デバイス・FIFO もそのまま作る (overlayfs の whiteout は 0:0 の文字デバイス)
//   - ハードリンクはハードリンクのまま複製する。ソケットは飛ばす
//
// 途中で失敗したら dst を消す。
func CopyTree(src, dst string) (err error) {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dst)
		}
	}()
	type inode struct{ dev, ino uint64 }
	links := map[inode]string{}
	var dirs []string // 属性は中身を作ったあとに付ける (読み取り専用のディレクトリと更新時刻のため)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return &fs.PathError{Op: "lstat", Path: path, Err: err}
		}
		mode := st.Mode & unix.S_IFMT
		if mode != unix.S_IFDIR && st.Nlink > 1 {
			key := inode{uint64(st.Dev), st.Ino}
			if first, ok := links[key]; ok {
				return os.Link(first, target)
			}
			links[key] = target
		}
		switch mode {
		case unix.S_IFDIR:
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, path)
			return nil
		case unix.S_IFREG:
			if _, err := CloneOrCopy(path, target); err != nil {
				return err
			}
		case unix.S_IFLNK:
			to, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(to, target); err != nil {
				return err
			}
		case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO:
			if err := unix.Mknod(target, st.Mode, int(st.Rdev)); err != nil {
				return &fs.PathError{Op: "mknod", Path: target, Err: err}
			}
		default:
			return nil // ソケットなど
		}
		return copyMeta(path, target, &st)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		rel, _ := filepath.Rel(src, dirs[i])
		var st unix.Stat_t
		if err := unix.Lstat(dirs[i], &st); err != nil {
			return err
		}
		if err := copyMeta(dirs[i], filepath.Join(dst, rel), &st); err != nil {
			return err
		}
	}
	return nil
}

// copyMeta は所有者・拡張属性・パーミッション・時刻を写す (シンボリックリンクはたどらない)。
func copyMeta(src, dst string, st *unix.Stat_t) error {
	isLink := st.Mode&unix.S_IFMT == unix.S_IFLNK
	if os.Geteuid() == 0 {
		if err := os.Lchown(dst, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if err := copyXattrs(src, dst); err != nil {
		return err
	}
	if !isLink {
		// chown で setuid / setgid が外れるので、パーミッションは chown のあと
		if err := unix.Chmod(dst, st.Mode&0o7777); err != nil {
			return &fs.PathError{Op: "chmod", Path: dst, Err: err}
		}
	}
	ts := []unix.Timespec{st.Atim, st.Mtim}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil && !isLink {
		return err
	}
	return nil
}

func copyXattrs(src, dst string) error {
	size, err := unix.Llistxattr(src, nil)
	if err != nil || size == 0 {
		if errors.Is(err, unix.ENOTSUP) {
			return nil
		}
		return err
	}
	buf := make([]byte, size)
	n, err := unix.Llistxattr(src, buf)
	if err != nil {
		return err
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vsize, err := unix.Lgetxattr(src, name, nil)
		if err != nil {
			return fmt.Errorf("getxattr %s %s: %w", src, name, err)
		}
		val := make([]byte, vsize)
		if vsize > 0 {
			if _, err := unix.Lgetxattr(src, name, val); err != nil {
				return fmt.Errorf("getxattr %s %s: %w", src, name, err)
			}
		}
		if err := unix.Lsetxattr(dst, name, val, 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("setxattr %s %s: %w", dst, name, err)
		}
	}
	return nil
}
