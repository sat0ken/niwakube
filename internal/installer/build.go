package installer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sat0ken/niwakube/internal/sys"
)

// BuildOptions は ISO を作るための入力。
type BuildOptions struct {
	MiniISO string    // 検証済みの Ubuntu mini ISO
	Server  ServerISO // インストーラーがダウンロードする Server ISO
	Output  string    // 出力する ISO
	WorkDir string    // 作業ディレクトリ (空なら一時ディレクトリ)
}

// Build は mini ISO を元に、config の内容を組み込んだ ISO を作る。
// 外部コマンド xorriso / mkfs.vfat / mcopy を使う。
func (c *Config) Build(ctx context.Context, r sys.Runner, o BuildOptions) error {
	grub, err := c.GrubConfig(o.Server)
	if err != nil {
		return err
	}
	userData, err := c.UserData()
	if err != nil {
		return err
	}
	overlay, err := InitrdOverlay()
	if err != nil {
		return err
	}

	work := o.WorkDir
	if work == "" {
		if work, err = os.MkdirTemp("", "niwakube-iso-"); err != nil {
			return err
		}
		defer os.RemoveAll(work)
	}
	p := func(name string) string { return filepath.Join(work, name) }

	// 1. initrd を取り出し、niwakube のスクリプトを入れた cpio を末尾に足す
	if _, err := r.Run(ctx, "xorriso", "-osirrox", "on", "-indev", o.MiniISO, "-extract", "/casper/initrd", p("initrd")); err != nil {
		return fmt.Errorf("extract initrd: %w", err)
	}
	if err := os.Chmod(p("initrd"), 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(p("initrd"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	// カーネルは 4 バイト境界から始まる cpio しか cpio として読まない (ずれると圧縮データとみなして失敗する)。
	// 元の initrd のサイズは境界に揃っていないので、ゼロで 512 バイト境界まで埋めてから足す (ゼロは読み飛ばされる)。
	if pad := (512 - fi.Size()%512) % 512; pad > 0 {
		overlay = append(make([]byte, pad), overlay...)
	}
	if _, err := f.Write(overlay); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// 2. GRUB の設定
	if err := os.WriteFile(p("grub.cfg"), grub, 0o644); err != nil {
		return err
	}

	// 3. autoinstall の設定を入れた CIDATA (FAT) のイメージ
	if err := os.WriteFile(p("user-data"), userData, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(p("meta-data"), c.MetaData(), 0o644); err != nil {
		return err
	}
	if _, err := r.Run(ctx, "mkfs.vfat", "-C", "-n", "CIDATA", p("cidata.img"), "4096"); err != nil {
		return fmt.Errorf("create cidata image: %w", err)
	}
	if _, err := r.Run(ctx, "mcopy", "-i", p("cidata.img"), p("user-data"), p("meta-data"), "::"); err != nil {
		return fmt.Errorf("copy into cidata image: %w", err)
	}

	// 4. ISO を作り直す (起動の仕組みはそのまま、ファイルの差し替えとパーティションの追加だけ)
	_ = os.Remove(o.Output)
	if _, err := r.Run(ctx, "xorriso",
		"-indev", o.MiniISO,
		"-outdev", o.Output,
		"-boot_image", "any", "replay",
		"-map", p("initrd"), "/casper/initrd",
		"-map", p("grub.cfg"), "/boot/grub/grub.cfg",
		"-map", p("user-data"), "/niwakube/user-data",
		"-append_partition", "3", "0x0c", p("cidata.img"),
	); err != nil {
		return fmt.Errorf("build ISO: %w", err)
	}
	return nil
}
