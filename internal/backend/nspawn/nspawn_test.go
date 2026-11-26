package nspawn

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/seed"
	"github.com/sat0ken/niwakube/internal/sys"
)

// host はホストの状態 (unit、マウント、ネットワーク名前空間、veth) をまねる。
type host struct {
	mu      sync.Mutex
	active  bool
	mounted bool
	netns   bool
	veth    bool
	runArgs []string
}

func (h *host) runner() *sys.FakeRunner {
	return &sys.FakeRunner{Handler: func(name string, args []string) ([]byte, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		cmd := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "systemctl is-active"):
			if h.active {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		case strings.HasPrefix(cmd, "systemctl stop"), strings.HasPrefix(cmd, "machinectl poweroff"), strings.HasPrefix(cmd, "machinectl kill"):
			h.active = false
		case strings.HasPrefix(cmd, "systemctl reset-failed"):
		case name == "systemd-run":
			h.active = true
			h.runArgs = args
		case name == "mountpoint":
			if !h.mounted {
				return nil, errors.New("not a mountpoint")
			}
		case name == "mount":
			h.mounted = true
		case name == "umount":
			h.mounted = false
		case strings.HasPrefix(cmd, "ip netns exec"):
			if !h.netns {
				return nil, errors.New("no such netns")
			}
		case strings.HasPrefix(cmd, "ip netns add"):
			h.netns = true
		case strings.HasPrefix(cmd, "ip netns del"):
			h.netns, h.veth = false, false
		case strings.HasPrefix(cmd, "ip link show"):
			if !h.veth {
				return nil, errors.New("does not exist")
			}
		case strings.HasPrefix(cmd, "ip link add"):
			h.veth = true
		case strings.HasPrefix(cmd, "ip link del"):
			h.veth = false
		case name == "ip":
		case name == "journalctl":
			return []byte("Welcome to Ubuntu 26.04 LTS!\nniwakube login:\n"), nil
		default:
			return nil, errors.New("unexpected command: " + cmd)
		}
		return nil, nil
	}}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "images", "ubuntu-26.04.rootfs", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &host{}
	r := h.runner()
	b := New(Config{DataDir: dataDir, StopTimeout: time.Second, Poll: 10 * time.Millisecond}, r)
	spec := &backend.MachineSpec{
		Name: "c1", CPUs: 2, MemoryMiB: 2048, DiskGiB: 20, Image: "ubuntu-26.04",
		NIC: backend.NIC{Bridge: "nkbr0", MAC: "52:54:00:12:34:56",
			Address: netip.MustParsePrefix("172.30.0.10/16"), Gateway: netip.MustParseAddr("172.30.0.1")},
		Seed: seed.Data{UserData: []byte("#cloud-config\n"), MetaData: []byte("instance-id: x\n")},
	}
	nc, err := seed.NetworkConfig(seed.Network{MAC: spec.NIC.MAC, Address: spec.NIC.Address, Gateway: spec.NIC.Gateway,
		DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}})
	if err != nil {
		t.Fatal(err)
	}
	spec.Seed.NetworkConfig = nc

	if err := b.Create(ctx, &backend.MachineSpec{Name: "x", Image: "nope"}); !errors.Is(err, backend.ErrImageNotFound) {
		t.Fatalf("missing image: %v", err)
	}
	if err := b.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(ctx, spec); !errors.Is(err, backend.ErrAlreadyExists) {
		t.Fatalf("second create: %v", err)
	}
	dir := filepath.Join(dataDir, "vms", "c1")
	for _, f := range []string{"seed/user-data", "seed/meta-data", "seed/network-config", "upper/etc/cloud/cloud.cfg.d/90-niwakube-nspawn.cfg", "volumes/rancher", "volumes/containerd", "volumes/kubelet", "work", "rootfs"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if s, _ := b.Status(ctx, "c1"); s.State != backend.StateStopped {
		t.Fatalf("status after create: %+v", s)
	}
	// ネットワークはホストで設定するので、cloud-init には設定させず、DNS は resolv.conf に書く
	if cfg, _ := os.ReadFile(filepath.Join(dir, "upper", "etc", "cloud", "cloud.cfg.d", "90-niwakube-nspawn.cfg")); !strings.Contains(string(cfg), "config: disabled") {
		t.Errorf("cloud-init must not configure the network:\n%s", cfg)
	}
	if resolv, _ := os.ReadFile(filepath.Join(dir, "upper", "etc", "resolv.conf")); !strings.Contains(string(resolv), "nameserver 1.1.1.1") {
		t.Errorf("resolv.conf:\n%s", resolv)
	}
	if target, err := os.Readlink(filepath.Join(dir, "upper", "etc", "systemd", "system", "systemd-networkd-wait-online.service")); err != nil || target != "/dev/null" {
		t.Errorf("systemd-networkd-wait-online must be masked: %q %v", target, err)
	}

	// Start: ネットワーク名前空間 → overlayfs → systemd-run
	if err := b.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Status(ctx, "c1"); s.State != backend.StateRunning {
		t.Fatalf("status after start: %+v", s)
	}
	calls := strings.Join(r.Calls(), "\n")
	for _, want := range []string{
		"ip netns add niwakube-c1",
		"ip link add nkv",
		"master nkbr0 up",
		"ip -n niwakube-c1 link set dev eth0 address 52:54:00:12:34:56",
		"ip -n niwakube-c1 addr add 172.30.0.10/16 dev eth0",
		"ip -n niwakube-c1 route add default via 172.30.0.1",
		"mount -t overlay overlay -o lowerdir=" + filepath.Join(dataDir, "images", "ubuntu-26.04.rootfs") + ",upperdir=" + filepath.Join(dir, "upper") + ",workdir=" + filepath.Join(dir, "work") + " " + filepath.Join(dir, "rootfs"),
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("start did not run %q:\n%s", want, calls)
		}
	}
	args := strings.Join(h.runArgs, " ")
	for _, want := range []string{
		"--unit=niwakube-vm-c1", "--property=Delegate=yes", "--property=CPUQuota=200%", "--property=MemoryMax=2048M",
		"--setenv=SYSTEMD_NSPAWN_API_VFS_WRITABLE=yes", "--setenv=SYSTEMD_SECCOMP=0",
		"systemd-nspawn --quiet --keep-unit --machine=c1 --directory=" + filepath.Join(dir, "rootfs"), " --boot ",
		"--network-namespace-path=/run/netns/niwakube-c1", "--capability=all", "--private-users=no",
		"--bind-ro=" + filepath.Join(dir, "seed") + ":/var/lib/cloud/seed/nocloud",
		"--bind=" + filepath.Join(dir, "volumes", "rancher") + ":/var/lib/rancher",
		"--bind=/dev/kmsg",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("systemd-run args do not contain %q:\n%s", want, args)
		}
	}
	// 2 回目の Start は何もしない
	n := len(r.Calls())
	if err := b.Start(ctx, "c1"); err != nil || len(r.Calls()) != n+1 {
		t.Errorf("second start: %v (%d new calls)", err, len(r.Calls())-n)
	}

	if out, err := b.SerialLog(ctx, "c1", 16); err != nil || string(out) != "niwakube login:\n" {
		t.Errorf("serial log = %q, %v", out, err)
	}

	// Resize: 動いている間はできない
	if err := b.Resize(ctx, "c1", backend.Resources{CPUs: 4}); !errors.Is(err, backend.ErrRunning) {
		t.Fatalf("resize while running: %v", err)
	}

	// Stop: 電源断を依頼 → ルートを外す → ネットワーク名前空間を消す
	if err := b.Stop(ctx, "c1", false); err != nil {
		t.Fatal(err)
	}
	if h.active || h.mounted || h.netns || h.veth {
		t.Errorf("after stop: %+v", h)
	}
	if !strings.Contains(strings.Join(r.Calls(), "\n"), "machinectl poweroff c1") {
		t.Error("stop must ask the container to power off")
	}

	if err := b.Resize(ctx, "c1", backend.Resources{CPUs: 4, MemoryMiB: 4096, DiskGiB: 99}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(h.runArgs, " "); !strings.Contains(args, "CPUQuota=400%") || !strings.Contains(args, "MemoryMax=4096M") {
		t.Errorf("resize did not apply: %s", args)
	}

	// Delete: 止めて消す
	if err := b.Delete(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("machine dir still exists: %v", err)
	}
	if s, _ := b.Status(ctx, "c1"); s.State != backend.StateNotFound {
		t.Errorf("status after delete: %+v", s)
	}
	if err := b.Start(ctx, "c1"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("start after delete: %v", err)
	}
}

func TestTalos(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	h := &host{}
	r := h.runner()
	var pulled []string
	b := New(Config{DataDir: dataDir, StopTimeout: time.Second, Poll: 10 * time.Millisecond,
		Pull: func(_ context.Context, ref, dir string) error {
			pulled = append(pulled, ref)
			return os.MkdirAll(filepath.Join(dir, "sbin"), 0o755)
		}}, r)
	nc, _ := seed.NetworkConfig(seed.Network{MAC: "52:54:00:aa:bb:cc", Address: netip.MustParsePrefix("172.30.2.11/16"),
		Gateway: netip.MustParseAddr("172.30.0.1"), DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}})
	spec := &backend.MachineSpec{
		Name: "tal-cp-1", CPUs: 2, MemoryMiB: 3072, Image: "talos-v1.14.7",
		NIC: backend.NIC{Bridge: "nkbr0", MAC: "52:54:00:aa:bb:cc",
			Address: netip.MustParsePrefix("172.30.2.11/16"), Gateway: netip.MustParseAddr("172.30.0.1")},
		Seed: seed.Data{UserData: []byte("version: v1alpha1\nmachine: {}\n"), NetworkConfig: nc},
	}
	if err := b.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// 2 台目は取得済みのイメージを使う
	spec2 := *spec
	spec2.Name = "tal-worker-1"
	if err := b.Create(ctx, &spec2); err != nil {
		t.Fatal(err)
	}
	if len(pulled) != 1 || pulled[0] != "ghcr.io/siderolabs/talos:v1.14.7" {
		t.Errorf("pulled %v", pulled)
	}
	dir := filepath.Join(dataDir, "vms", "tal-cp-1")
	for f, want := range map[string]string{"hostname": "tal-cp-1", "resolv.conf": "nameserver 1.1.1.1", "hosts": "172.30.2.11 tal-cp-1", "talos.yaml": "version: v1alpha1"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); !strings.Contains(string(b), want) {
			t.Errorf("%s = %q, want %q", f, b, want)
		}
	}
	for _, v := range []string{"var", "state", "opt"} {
		if _, err := os.Stat(filepath.Join(dir, "volumes", v)); err != nil {
			t.Errorf("volume %s: %v", v, err)
		}
	}

	if err := b.Start(ctx, "tal-cp-1"); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(h.runArgs, " ")
	for _, want := range []string{
		"--property=EnvironmentFile=" + filepath.Join(dir, "talos.env"),
		"--setenv=PLATFORM --setenv=USERDATA --setenv=TALOSSKU",
		"--tmpfs=/system --tmpfs=/tmp",
		"--bind=" + filepath.Join(dir, "volumes", "state") + ":/system/state",
		"--bind=" + filepath.Join(dir, "volumes", "var") + ":/var",
		"--bind-ro=" + filepath.Join(dir, "hostname") + ":/etc/hostname",
		"--network-namespace-path=/run/netns/niwakube-tal-cp-1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("talos args do not contain %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--boot") || !strings.HasSuffix(args, " /sbin/init") || strings.Contains(args, "USERDATA=") {
		t.Errorf("talos must run /sbin/init without --boot, and the config must not be on the command line:\n%s", args)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "talos.env"))
	if !strings.Contains(string(env), "PLATFORM=container\nUSERDATA=dmVyc2lvbjogdjFhbHBoYTEK") || !strings.Contains(string(env), "TALOSSKU=2CPU-3072RAM") {
		t.Errorf("talos.env:\n%s", env)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "talos.env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("talos.env must be 0600: %v", fi.Mode())
	}

	if err := b.Stop(ctx, "tal-cp-1", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.Calls(), "\n"), "machinectl kill --kill-whom=leader --signal=SIGTERM tal-cp-1") {
		t.Error("talos must be stopped with SIGTERM")
	}
	if err := b.Create(ctx, &backend.MachineSpec{Name: "x", Image: "ubuntu-99.04", Seed: seed.Data{NetworkConfig: nc}}); !errors.Is(err, backend.ErrImageNotFound) {
		t.Errorf("an image that is neither present nor in a registry: %v", err)
	}
}
