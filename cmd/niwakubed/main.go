// niwakubed は niwakube のデーモン。VM の作成・削除を REST API で受け付ける。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sat0ken/niwakube/internal/api"
	"github.com/sat0ken/niwakube/internal/backend/ch"
	"github.com/sat0ken/niwakube/internal/cluster"
	"github.com/sat0ken/niwakube/internal/distro/k3s"
	"github.com/sat0ken/niwakube/internal/distro/kubeadm"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
	"github.com/sat0ken/niwakube/internal/sys"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "niwakubed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVer  = flag.Bool("version", false, "バージョンを表示して終了する")
		listen   = flag.String("listen", "127.0.0.1:8080", "HTTP の待ち受けアドレス")
		dataDir  = flag.String("data-dir", "/var/lib/niwakube", "データディレクトリ")
		runDir   = flag.String("run-dir", "/run/niwakube", "API socket を置くディレクトリ")
		bridge   = flag.String("bridge", "nkbr0", "VM をつなぐブリッジ")
		subnet   = flag.String("subnet", "172.30.0.0/16", "VM のサブネット")
		gateway  = flag.String("gateway", "172.30.0.1", "VM のデフォルトゲートウェイ (ブリッジのアドレス)")
		pool     = flag.String("pool", "172.30.0.10-172.30.0.254", "単体 VM に払い出すアドレス範囲")
		dns      = flag.String("dns", "1.1.1.1,8.8.8.8", "VM に設定する DNS サーバー (カンマ区切り)")
		chBinary = flag.String("ch-binary", "/usr/local/bin/cloud-hypervisor", "cloud-hypervisor のパス")
		sshKey   = flag.String("ssh-key", "", "全 VM に入れる公開鍵 (既定: <data-dir>/ssh/id_ed25519.pub)")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if os.Geteuid() != 0 {
		log.Warn("not running as root; creating taps and systemd units will fail")
	}

	cfg := machine.Config{Bridge: *bridge}
	var err error
	if cfg.Subnet, err = netip.ParsePrefix(*subnet); err != nil {
		return fmt.Errorf("-subnet: %w", err)
	}
	if cfg.Gateway, err = netip.ParseAddr(*gateway); err != nil {
		return fmt.Errorf("-gateway: %w", err)
	}
	if cfg.Pool, err = ipam.ParsePool(*pool); err != nil {
		return fmt.Errorf("-pool: %w", err)
	}
	for _, d := range strings.Split(*dns, ",") {
		a, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil {
			return fmt.Errorf("-dns: %w", err)
		}
		cfg.DNS = append(cfg.DNS, a)
	}
	keyPath := *sshKey
	if keyPath == "" {
		keyPath = filepath.Join(*dataDir, "ssh", "id_ed25519.pub")
	}
	if b, err := os.ReadFile(keyPath); err == nil {
		cfg.DefaultSSHKeys = []string{strings.TrimSpace(string(b))}
	} else {
		log.Warn("default ssh key not loaded", "path", keyPath, "err", err)
	}

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*dataDir, "niwakube.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	chb := ch.New(ch.Config{Binary: *chBinary, DataDir: *dataDir, RunDir: *runDir}, sys.ExecRunner{})
	svc, err := machine.NewService(cfg, st, log, chb)
	if err != nil {
		return err
	}
	clusters, err := cluster.NewService(cluster.Config{Subnet: cfg.Subnet}, st, svc, nil, log, k3s.New(), kubeadm.New())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ホスト再起動後などに、起動しているはずの VM を起動し直す
	go svc.Reconcile(ctx)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.New(svc, clusters, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *listen, "version", version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down (VMs keep running)")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}
