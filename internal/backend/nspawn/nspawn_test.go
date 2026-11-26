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
		case strings.HasPrefix(cmd, "systemctl stop"), strings.HasPrefix(cmd, "machinectl poweroff"):
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
	// cloud-init にはインターフェース名で合わせさせる (MAC と set-name は使わない)
	netcfg, _ := os.ReadFile(filepath.Join(dir, "seed", "network-config"))
	if s := string(netcfg); !strings.Contains(s, "name: eth0") || strings.Contains(s, "macaddress") || strings.Contains(s, "set-name") ||
		!strings.Contains(s, "172.30.0.10/16") || !strings.Contains(s, "1.1.1.1") {
		t.Errorf("seed network-config:\n%s", s)
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
		"systemd-nspawn --quiet --keep-unit --boot --machine=c1 --directory=" + filepath.Join(dir, "rootfs"),
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
