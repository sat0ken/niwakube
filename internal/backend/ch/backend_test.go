package ch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/seed"
	"github.com/sat0ken/niwakube/internal/sys"
)

// fakeVMM は cloud-hypervisor の API socket をまねるサーバー。
type fakeVMM struct {
	mu     sync.Mutex
	srv    *http.Server
	cfg    *VmConfig
	state  string // "" (未作成) | Created | Running | Shutdown
	calls  []string
	closed bool
	open   int // 開いている接続の数
}

func startFakeVMM(t *testing.T, sock string) *fakeVMM {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeVMM{}
	mux := http.NewServeMux()
	handle := func(pattern string, fn func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1/"))
			fn(w, r)
		})
	}
	handle("GET /api/v1/vmm.ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"version":"fake"}`)
	})
	handle("PUT /api/v1/vm.create", func(w http.ResponseWriter, r *http.Request) {
		var cfg VmConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// v55 と同じく image_type の無いディスクは拒否する
		for _, d := range cfg.Disks {
			if d.ImageType == "" {
				http.Error(w, "Image type required for disk", http.StatusBadRequest)
				return
			}
		}
		f.cfg, f.state = &cfg, "Created"
		w.WriteHeader(http.StatusNoContent)
	})
	handle("PUT /api/v1/vm.boot", func(w http.ResponseWriter, _ *http.Request) {
		if f.state == "" {
			http.Error(w, "VM is not created", http.StatusInternalServerError)
			return
		}
		f.state = "Running"
		w.WriteHeader(http.StatusNoContent)
	})
	handle("GET /api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		if f.state == "" {
			http.Error(w, "VM is not created", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(VmInfo{Config: *f.cfg, State: f.state})
	})
	handle("PUT /api/v1/vm.power-button", func(w http.ResponseWriter, _ *http.Request) {
		f.state = "Shutdown" // ゲストがすぐ電源を切ったことにする
		w.WriteHeader(http.StatusNoContent)
	})
	handle("PUT /api/v1/vmm.shutdown", func(w http.ResponseWriter, _ *http.Request) {
		f.closed = true
		w.WriteHeader(http.StatusNoContent)
		go f.srv.Close()
	})
	f.srv = &http.Server{Handler: mux, ConnState: func(_ net.Conn, st http.ConnState) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch st {
		case http.StateNew:
			f.open++
		case http.StateClosed, http.StateHijacked:
			f.open--
		}
	}}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeVMM) alive() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.closed
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	runDir, err := os.MkdirTemp("", "nk") // unix socket のパス長制限 (108 文字) を避けるため短くする
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })

	for _, d := range []string{"images", "firmware"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(dataDir, "images", "ubuntu-26.04.raw"), "base-image")
	mustWrite(t, filepath.Join(dataDir, "firmware", "CLOUDHV.fd"), "fw")

	var (
		mu   sync.Mutex
		vmm  *fakeVMM
		taps = map[string]bool{}
	)
	r := &sys.FakeRunner{Handler: func(name string, args []string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case name == "cloud-localds":
			return nil, os.WriteFile(args[2], []byte("iso"), 0o644)
		case name == "systemctl" && args[0] == "is-active":
			if vmm.alive() {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		case name == "systemctl" && args[0] == "stop":
			return nil, nil
		case name == "systemd-run":
			sock := strings.TrimPrefix(args[len(args)-1], "path=")
			vmm = startFakeVMM(t, sock)
			return nil, nil
		case name == "ip" && args[0] == "link" && args[1] == "show":
			if taps[args[3]] {
				return nil, nil
			}
			return nil, errors.New("does not exist")
		case name == "ip" && args[0] == "tuntap":
			taps[args[3]] = true
			return nil, nil
		case name == "ip" && args[0] == "link" && args[1] == "del":
			delete(taps, args[3])
			return nil, nil
		case name == "ip":
			return nil, nil
		}
		return nil, errors.New("unexpected command: " + name)
	}}

	b := New(Config{DataDir: dataDir, RunDir: runDir, StopTimeout: 2 * time.Second}, r)
	spec := &backend.MachineSpec{
		Name: "vm1", CPUs: 2, MemoryMiB: 1024, DiskGiB: 5, Image: "ubuntu-26.04",
		NIC: backend.NIC{
			Bridge: "nkbr0", MAC: "52:54:00:12:34:56",
			Address: netip.MustParsePrefix("172.30.0.10/16"), Gateway: netip.MustParseAddr("172.30.0.1"),
		},
		Seed: seed.Data{UserData: []byte("#cloud-config\n"), MetaData: []byte("instance-id: x\n"), NetworkConfig: []byte("version: 2\n")},
	}

	// Create
	if err := b.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(ctx, spec); !errors.Is(err, backend.ErrAlreadyExists) {
		t.Fatalf("second create: want ErrAlreadyExists, got %v", err)
	}
	fi, err := os.Stat(filepath.Join(dataDir, "vms", "vm1", "disk.raw"))
	if err != nil || fi.Size() != 5<<30 {
		t.Fatalf("disk not grown: %v %v", fi, err)
	}
	if s, _ := b.Status(ctx, "vm1"); s.State != backend.StateStopped {
		t.Fatalf("status after create = %+v", s)
	}

	// Start
	if err := b.Start(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Status(ctx, "vm1"); s.State != backend.StateRunning {
		t.Fatalf("status after start = %+v", s)
	}
	tap := netx.TapName("vm1")
	if !taps[tap] {
		t.Errorf("tap %s not created", tap)
	}
	cfg := vmm.cfg
	if cfg.Cpus.BootVcpus != 2 || cfg.Memory.Size != 1024<<20 || cfg.Payload.Firmware == "" ||
		len(cfg.Disks) != 2 || !cfg.Disks[1].Readonly || cfg.Net[0].Tap != tap || cfg.Net[0].Mac != spec.NIC.MAC ||
		cfg.Serial.Mode != "File" {
		t.Errorf("unexpected vm config: %+v", cfg)
	}
	// 状態確認を何度繰り返しても接続がたまらない (CH の API サーバーには接続数の上限がある)
	for range 50 {
		if s, _ := b.Status(ctx, "vm1"); s.State != backend.StateRunning {
			t.Fatalf("status = %+v", s)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		vmm.mu.Lock()
		open := vmm.open
		vmm.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections to the VMM API are left open", open)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 2 回目の Start は何もしない
	if err := b.Start(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}

	// Stop (正常終了)
	if err := b.Stop(ctx, "vm1", false); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Status(ctx, "vm1"); s.State != backend.StateStopped {
		t.Fatalf("status after stop = %+v", s)
	}
	if !strings.Contains(strings.Join(vmm.calls, ","), "PUT vm.power-button") {
		t.Errorf("power-button not sent: %v", vmm.calls)
	}
	if taps[tap] {
		t.Errorf("tap %s not deleted", tap)
	}

	// 停止後にもう一度起動できる (新しい VMM プロセスで vm.create からやり直す)
	if err := b.Start(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Status(ctx, "vm1"); s.State != backend.StateRunning {
		t.Fatalf("status after restart = %+v", s)
	}

	// Serial log
	mustWrite(t, filepath.Join(dataDir, "vms", "vm1", "serial.log"), "hello world")
	if out, err := b.SerialLog(ctx, "vm1", 5); err != nil || string(out) != "world" {
		t.Errorf("serial log = %q, %v", out, err)
	}

	// Delete
	if err := b.Delete(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Status(ctx, "vm1"); s.State != backend.StateNotFound {
		t.Fatalf("status after delete = %+v", s)
	}
	if err := b.Start(ctx, "vm1"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("start after delete: want ErrNotFound, got %v", err)
	}
}

func TestCreateMissingImage(t *testing.T) {
	b := New(Config{DataDir: t.TempDir()}, &sys.FakeRunner{})
	err := b.Create(context.Background(), &backend.MachineSpec{Name: "x", Image: "nope", DiskGiB: 5})
	if !errors.Is(err, backend.ErrImageNotFound) {
		t.Fatalf("want ErrImageNotFound, got %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
