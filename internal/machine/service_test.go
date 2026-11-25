package machine

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/backend/fake"
	"github.com/sat0ken/niwakube/internal/ipam"
	"github.com/sat0ken/niwakube/internal/store"
)

func newTestService(t *testing.T) (*Service, *fake.Backend) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fb := fake.New()
	svc, err := NewService(Config{
		Bridge:         "nkbr0",
		Subnet:         netip.MustParsePrefix("172.30.0.0/16"),
		Gateway:        netip.MustParseAddr("172.30.0.1"),
		Pool:           ipam.Pool{Start: netip.MustParseAddr("172.30.0.10"), End: netip.MustParseAddr("172.30.0.11")},
		DNS:            []netip.Addr{netip.MustParseAddr("1.1.1.1")},
		DefaultSSHKeys: []string{"ssh-ed25519 HOSTKEY niwakube"},
	}, st, nil, fb)
	if err != nil {
		t.Fatal(err)
	}
	return svc, fb
}

func TestCreateAllocatesAndStarts(t *testing.T) {
	ctx := context.Background()
	svc, fb := newTestService(t)

	vm, err := svc.Create(ctx, CreateRequest{Name: "vm1", SSHAuthorizedKeys: []string{"ssh-ed25519 USERKEY"}})
	if err != nil {
		t.Fatal(err)
	}
	if vm.IP != "172.30.0.10" || vm.CPUs != 2 || vm.MemoryMiB != 4096 || vm.DiskGiB != 20 ||
		vm.Image != "ubuntu-26.04" || vm.Backend != fake.Name || vm.Status.State != backend.StateRunning {
		t.Errorf("unexpected vm: %+v", vm)
	}

	spec, _ := fb.Spec("vm1")
	if spec.NIC.Address.String() != "172.30.0.10/16" || spec.NIC.Bridge != "nkbr0" || spec.NIC.MAC != vm.MAC {
		t.Errorf("unexpected nic: %+v", spec.NIC)
	}
	ud := string(spec.Seed.UserData)
	if !strings.Contains(ud, "HOSTKEY") || !strings.Contains(ud, "USERKEY") {
		t.Errorf("ssh keys missing from user-data:\n%s", ud)
	}
	if !strings.Contains(string(spec.Seed.NetworkConfig), "172.30.0.10/16") {
		t.Errorf("ip missing from network-config:\n%s", spec.Seed.NetworkConfig)
	}

	vm2, err := svc.Create(ctx, CreateRequest{Name: "vm2"})
	if err != nil {
		t.Fatal(err)
	}
	if vm2.IP != "172.30.0.11" || vm2.MAC == vm.MAC {
		t.Errorf("vm2 = %+v", vm2)
	}

	// プールが尽きた
	if _, err := svc.Create(ctx, CreateRequest{Name: "vm3"}); !errors.Is(err, ipam.ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	// 削除すると IP が再利用される
	if err := svc.Delete(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	vm3, err := svc.Create(ctx, CreateRequest{Name: "vm3"})
	if err != nil {
		t.Fatal(err)
	}
	if vm3.IP != "172.30.0.10" {
		t.Errorf("ip not reused: %s", vm3.IP)
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	bad := []CreateRequest{
		{Name: ""},
		{Name: "Bad_Name"},
		{Name: "-x"},
		{Name: "ok", CPUs: 100},
		{Name: "ok", MemoryMiB: 100},
		{Name: "ok", DiskGiB: 1},
		{Name: "ok", Backend: "nope"},
		{Name: "ok", Image: "../etc/passwd"},
	}
	for _, req := range bad {
		if _, err := svc.Create(ctx, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", req, err)
		}
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "dup"}); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate: want ErrExists, got %v", err)
	}
}

func TestCreateRollbackOnBackendError(t *testing.T) {
	ctx := context.Background()
	svc, fb := newTestService(t)
	fb.CreateErr = backend.ErrImageNotFound
	if _, err := svc.Create(ctx, CreateRequest{Name: "vm1"}); !errors.Is(err, backend.ErrImageNotFound) {
		t.Fatalf("want ErrImageNotFound, got %v", err)
	}
	if _, err := svc.Get(ctx, "vm1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record should be rolled back, got %v", err)
	}
}

func TestStopStartAndReconcile(t *testing.T) {
	ctx := context.Background()
	svc, fb := newTestService(t)
	if _, err := svc.Create(ctx, CreateRequest{Name: "vm1"}); err != nil {
		t.Fatal(err)
	}
	vm, err := svc.Stop(ctx, "vm1", false)
	if err != nil {
		t.Fatal(err)
	}
	if vm.DesiredState != DesiredStopped || vm.Status.State != backend.StateStopped {
		t.Errorf("after stop: %+v", vm)
	}
	// 望ましい状態が stopped なら Reconcile は起動しない
	svc.Reconcile(ctx)
	if s, _ := fb.Status(ctx, "vm1"); s.State != backend.StateStopped {
		t.Errorf("reconcile started a stopped machine")
	}

	if _, err := svc.Start(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	// ホスト再起動で落ちたことにする → Reconcile で起動し直す
	_ = fb.Stop(ctx, "vm1", true)
	svc.Reconcile(ctx)
	if s, _ := fb.Status(ctx, "vm1"); s.State != backend.StateRunning {
		t.Errorf("reconcile did not start the machine: %+v", s)
	}
}
