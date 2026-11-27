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
	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/backend/ch"
	"github.com/sat0ken/niwakube/internal/backend/fake"
	"github.com/sat0ken/niwakube/internal/backend/nspawn"
	"github.com/sat0ken/niwakube/internal/cluster"
	"github.com/sat0ken/niwakube/internal/distro/k3s"
	"github.com/sat0ken/niwakube/internal/distro/kubeadm"
	"github.com/sat0ken/niwakube/internal/distro/talos"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/kube"
	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/nodeexec"
	"github.com/sat0ken/niwakube/internal/practice"
	"github.com/sat0ken/niwakube/internal/store"
	"github.com/sat0ken/niwakube/internal/sys"
	"github.com/sat0ken/niwakube/internal/terminal"
	"github.com/sat0ken/niwakube/internal/webui"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

// practiceSnapshots は練習問題が使う、クラスタ 1 つのスナップショットの操作。
type practiceSnapshots struct {
	clusters *cluster.Service
	name     string
}

func (p practiceSnapshots) Has(ctx context.Context, snap string) (bool, error) {
	return p.clusters.HasSnapshot(ctx, p.name, snap)
}

func (p practiceSnapshots) Take(ctx context.Context, snap string) error {
	return p.clusters.SnapshotAndWait(ctx, p.name, snap)
}

func (p practiceSnapshots) Restore(ctx context.Context, snap string) error {
	return p.clusters.RestoreAndWait(ctx, p.name, snap)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "niwakubed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVer     = flag.Bool("version", false, "バージョンを表示して終了する")
		listen      = flag.String("listen", "127.0.0.1:8080", "HTTP の待ち受けアドレス")
		dataDir     = flag.String("data-dir", "/var/lib/niwakube", "データディレクトリ")
		runDir      = flag.String("run-dir", "/run/niwakube", "API socket を置くディレクトリ")
		bridge      = flag.String("bridge", "nkbr0", "VM をつなぐブリッジ")
		subnet      = flag.String("subnet", "172.30.0.0/16", "VM のサブネット")
		gateway     = flag.String("gateway", "172.30.0.1", "VM のデフォルトゲートウェイ (ブリッジのアドレス)")
		pool        = flag.String("pool", "172.30.0.10-172.30.0.254", "単体 VM に払い出すアドレス範囲")
		dns         = flag.String("dns", "1.1.1.1,8.8.8.8", "VM に設定する DNS サーバー (カンマ区切り)")
		chBinary    = flag.String("ch-binary", "/usr/local/bin/cloud-hypervisor", "cloud-hypervisor のパス")
		sshKey      = flag.String("ssh-key", "", "全 VM に入れる公開鍵 (既定: <data-dir>/ssh/id_ed25519.pub)")
		defCNI      = flag.String("default-cni", "cilium", "クラスタの既定の CNI (cilium | flannel)")
		defRT       = flag.String("default-runtime", "runc", "クラスタの既定のコンテナランタイム (runc | crun | youki)")
		defRTs      = flag.String("default-runtimes", "", "クラスタに既定で追加するランタイム (カンマ区切り。例: crun,youki)")
		nfsRoot     = flag.String("nfs-root", "", "NFS で export しているディレクトリ (既定: <data-dir>/nfs。無ければ NFS の StorageClass は作らない)")
		backendName = flag.String("backend", "cloud-hypervisor", "ノードを作るバックエンド (cloud-hypervisor: VM / nspawn: systemd-nspawn のコンテナ)")
		devFake     = flag.Bool("dev-fake-backend", false, "VM を実際には作らない (KVM の無い PC で Web UI を開発するため)")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if os.Geteuid() != 0 && !*devFake {
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

	var be backend.Backend
	switch *backendName {
	case ch.Name:
		be = ch.New(ch.Config{Binary: *chBinary, DataDir: *dataDir, RunDir: *runDir}, sys.ExecRunner{})
	case nspawn.Name:
		// コンテナ型: ホストのカーネルを共有するので、クラスタの既定の CNI は flannel になる
		be = nspawn.New(nspawn.Config{DataDir: *dataDir}, sys.ExecRunner{})
	default:
		return fmt.Errorf("-backend: unknown backend %q (cloud-hypervisor or nspawn)", *backendName)
	}
	if *devFake {
		log.Warn("using the fake backend: VMs are not actually created (for UI development)")
		be = fake.New()
	}
	svc, err := machine.NewService(cfg, st, log, be)
	if err != nil {
		return err
	}
	defaults := cluster.Defaults{CNI: *defCNI, Runtime: store.Runtime{Default: *defRT, Install: splitList(*defRTs)}}
	var fwd cluster.ForwardApplier = netx.Forwarder{Run: sys.ExecRunner{}, Bridge: *bridge, Dir: *runDir}
	if *devFake {
		fwd = nil
	}
	nfs := cluster.NFSConfig{Server: cfg.Gateway, Root: *nfsRoot}
	if nfs.Root == "" {
		nfs.Root = filepath.Join(*dataDir, "nfs")
	}
	if fi, err := os.Stat(nfs.Root); err != nil || !fi.IsDir() {
		log.Warn("NFS is disabled (run host-setup.sh to set up the NFS server)", "root", nfs.Root)
		nfs.Root = ""
	}
	clusters, err := cluster.NewService(cluster.Config{Subnet: cfg.Subnet, Defaults: defaults, Forwarder: fwd, NFS: nfs,
		SnapshotDir: filepath.Join(*dataDir, "snapshots")},
		st, svc, nil, log, k3s.New(), kubeadm.New(), talos.New())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Web UI のターミナル。シェルは systemd-run の一時ユーザーとサンドボックスで動かし、
	// VM のディスクや秘密鍵のあるデータディレクトリと API socket のディレクトリは見せない
	term := terminal.New(terminal.Config{
		Dir:     filepath.Join(*runDir, "term"),
		Sandbox: !*devFake,
		Hidden:  []string{*dataDir, *runDir},
	}, log)
	if *devFake {
		log.Warn("terminal runs without the sandbox (dev mode)")
	}
	term.Cleanup(ctx)

	// CKA / CKAD の練習問題。問題を出すクラスタには admin の kubeconfig でつなぎ、ノードにはクラスタの鍵で SSH する
	drills, err := practice.NewService(func(ctx context.Context, name string) (*practice.Target, error) {
		kc, err := clusters.Kubeconfig(ctx, name)
		if err != nil {
			return nil, err
		}
		kc2, err := kube.New(kc)
		if err != nil {
			return nil, err
		}
		d, ver, err := clusters.DistroOf(ctx, name)
		if err != nil {
			return nil, err
		}
		t := &practice.Target{Kube: kc2, Distro: d, Version: ver, Snapshots: practiceSnapshots{clusters, name}}
		a, err := clusters.NodeAccess(ctx, name)
		switch {
		case err == nil:
			t.Nodes = map[string]string{}
			ips := map[string]string{}
			for _, n := range a.Nodes {
				t.Nodes[n.Alias] = n.Name
				ips[n.Alias] = n.IP
			}
			t.Run = func(ctx context.Context, node, script string) (*nodeexec.Result, error) {
				ctx, cancel := context.WithTimeout(ctx, 20*time.Minute) // アップグレードの模範解答は数分かかる
				defer cancel()
				return nodeexec.Run(ctx, ips[node], a.User, a.Key, script)
			}
		case !errors.Is(err, backend.ErrNotSupported):
			return nil, err
		}
		return t, nil
	})
	if err != nil {
		log.Error("load practice problems", "err", err)
		os.Exit(1)
	}

	// ホスト再起動後などに、起動しているはずの VM を起動し直す
	go func() {
		svc.Reconcile(ctx)
		clusters.Reconcile(ctx)
	}()

	srv := &http.Server{
		Addr: *listen,
		Handler: api.New(svc, clusters, log, api.Options{
			Version:      version,
			DataDir:      *dataDir,
			LoopbackOnly: api.IsLoopbackListen(*listen),
			UI:           webui.Handler(),
			Terminal:     term,
			Practice:     drills,
		}).Handler(),
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

func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
