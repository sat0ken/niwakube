package store

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMachineCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	m := &Machine{
		Name: "vm1", Backend: "cloud-hypervisor", Image: "ubuntu-24.04",
		CPUs: 2, MemoryMiB: 4096, DiskGiB: 20,
		IP: netip.MustParseAddr("172.30.0.10"), MAC: "52:54:00:00:00:01",
		SSHKeys: []string{"ssh-ed25519 AAAA"}, DesiredState: "running",
		CreatedAt: time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC),
	}
	if err := s.InsertMachine(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertMachine(ctx, m); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate insert: want ErrExists, got %v", err)
	}

	got, err := s.GetMachine(ctx, "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if got.IP != m.IP || got.MAC != m.MAC || got.SSHKeys[0] != "ssh-ed25519 AAAA" || !got.CreatedAt.Equal(m.CreatedAt) {
		t.Errorf("got %+v", got)
	}

	if err := s.SetDesiredState(ctx, "vm1", "stopped"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListMachines(ctx)
	if err != nil || len(list) != 1 || list[0].DesiredState != "stopped" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	if err := s.DeleteMachine(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMachine(ctx, "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.DeleteMachine(ctx, "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
