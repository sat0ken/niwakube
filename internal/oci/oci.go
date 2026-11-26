// Package oci は OCI イメージをレジストリから取得して、ルートファイルシステムとして展開する
// (Docker も skopeo も使わない)。systemd-nspawn のバックエンドで、Talos のコンテナイメージを使うために使う。
package oci

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sys/unix"
)

// Pull は ref (例: ghcr.io/siderolabs/talos:v1.14.7) のイメージのうちホストのアーキテクチャのものを取得し、
// レイヤーを重ねたファイルシステムを dir に展開する。dir は空か存在しないこと。
// 途中で失敗したら dir は作らない (dir.tmp に展開してから名前を変える)。
func Pull(ctx context.Context, ref, dir string, opts ...remote.Option) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("image reference %q: %w", ref, err)
	}
	opts = append([]remote.Option{
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	}, opts...)
	img, err := remote.Image(r, opts...)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	rc := mutate.Extract(img) // 全レイヤーを重ねた (whiteout を反映した) ファイルシステムの tar
	defer rc.Close()
	if err := Untar(rc, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("extract %s: %w", ref, err)
	}
	return os.Rename(tmp, dir)
}

// Untar は tar を dir に展開する。所有者・パーミッション・拡張属性 (file capabilities など) も復元する。
// エントリが dir の外に出るもの (.. や絶対パス、シンボリックリンクをたどる書き込み) は拒否する。
func Untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	type dirMeta struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirMeta // ディレクトリの属性は中身を作ったあとに付ける (読み取り専用のディレクトリのため)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		if target == dir {
			continue
		}
		if err := checkParents(dir, target); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode & 0o7777)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{target, hdr})
			continue
		case tar.TypeReg:
			_ = os.Remove(target)
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			src, err := safeJoin(dir, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := checkParents(dir, src); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(src, target); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			_ = os.Remove(target)
			kind := map[byte]uint32{tar.TypeChar: unix.S_IFCHR, tar.TypeBlock: unix.S_IFBLK, tar.TypeFifo: unix.S_IFIFO}[hdr.Typeflag]
			dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
			if err := unix.Mknod(target, kind|uint32(mode), int(dev)); err != nil {
				return fmt.Errorf("mknod %s: %w", hdr.Name, err)
			}
		default:
			continue // そのほか (PAX のグローバルヘッダーなど) は無視する
		}
		if err := setMeta(target, hdr, mode); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := setMeta(dirs[i].path, dirs[i].hdr, os.FileMode(dirs[i].hdr.Mode&0o7777)); err != nil {
			return err
		}
	}
	return nil
}

// setMeta は所有者・拡張属性・パーミッションを付ける (シンボリックリンクはたどらない)。
func setMeta(path string, hdr *tar.Header, mode os.FileMode) error {
	if os.Geteuid() == 0 {
		if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil {
			return err
		}
	}
	for k, v := range hdr.PAXRecords {
		if attr, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok && os.Geteuid() == 0 {
			if err := unix.Lsetxattr(path, attr, []byte(v), 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
				return fmt.Errorf("setxattr %s %s: %w", path, attr, err)
			}
		}
	}
	if hdr.Typeflag == tar.TypeSymlink {
		return nil
	}
	// chown で setuid / setgid が外れるので、最後にパーミッションを付ける
	return os.Chmod(path, mode)
}

// safeJoin は dir と tar のエントリ名をつなぐ。dir の外に出る名前はエラー。
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean("/" + name)
	target := filepath.Join(dir, clean)
	if target != dir && !strings.HasPrefix(target, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("tar entry %q escapes the root", name)
	}
	return target, nil
}

// checkParents は target の親ディレクトリにシンボリックリンクが無いことを確かめる
// (イメージの中のシンボリックリンクをたどって、ルートの外に書き込まないため)。
func checkParents(dir, target string) error {
	rel, err := filepath.Rel(dir, filepath.Dir(target))
	if err != nil || rel == "." {
		return err
	}
	p := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("tar entry %s: parent %s is a symlink", target, p)
		}
	}
	return nil
}
