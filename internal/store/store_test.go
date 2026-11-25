package store

import (
	"context"
	"database/sql"
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
		Name: "vm1", Backend: "cloud-hypervisor", Image: "ubuntu-26.04",
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

func TestClusterCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	c := &Cluster{
		Name: "demo", Distro: "k3s", Index: 1,
		Spec:         ClusterSpec{Version: "stable", ControlPlanes: 1, Workers: 2, Worker: NodeSize{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}},
		Secrets:      []byte(`{"token":"x"}`),
		DesiredState: "running",
		CreatedAt:    time.Now(),
	}
	if err := s.InsertCluster(ctx, c); err != nil {
		t.Fatal(err)
	}
	dup := *c
	dup.Name = "other" // index が重複
	if err := s.InsertCluster(ctx, &dup); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate index: want ErrExists, got %v", err)
	}
	got, err := s.GetCluster(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Workers != 2 || got.Spec.Worker.MemoryMiB != 2048 || string(got.Secrets) != `{"token":"x"}` {
		t.Errorf("got %+v", got)
	}
	if err := s.SetClusterDesiredState(ctx, "demo", "stopped"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListClusters(ctx)
	if err != nil || len(list) != 1 || list[0].DesiredState != "stopped" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.DeleteCluster(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCluster(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMigrateAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// フェーズ1 の DB (cluster / role 列が無い) を作る
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE machines (
		name TEXT PRIMARY KEY, backend TEXT NOT NULL, image TEXT NOT NULL, cpus INTEGER NOT NULL,
		memory_mib INTEGER NOT NULL, disk_gib INTEGER NOT NULL, ip TEXT NOT NULL UNIQUE, mac TEXT NOT NULL UNIQUE,
		ssh_keys TEXT NOT NULL DEFAULT '[]', desired_state TEXT NOT NULL, created_at TEXT NOT NULL);
		INSERT INTO machines VALUES ('old','cloud-hypervisor','ubuntu-24.04',2,4096,20,'172.30.0.10','52:54:00:00:00:01','[]','running','2026-11-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.GetMachine(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if m.Cluster != "" || m.Role != "" {
		t.Errorf("unexpected cluster/role: %+v", m)
	}
}
