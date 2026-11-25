// Package ch は Cloud Hypervisor のバックエンド。
//
// VM 1 台につき cloud-hypervisor プロセスを 1 つ、systemd の transient unit
// (niwakube-vm-<name>) として起動し、API socket 経由で vm.create / vm.boot する。
// niwakubed を再起動しても VM は落ちない。
package ch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/seed"
	"github.com/sat0ken/niwakube/internal/sys"
)

const Name = "cloud-hypervisor"

type Config struct {
	Binary      string        // 既定: /usr/local/bin/cloud-hypervisor
	DataDir     string        // 既定: /var/lib/niwakube
	RunDir      string        // 既定: /run/niwakube
	Firmware    string        // 既定: <DataDir>/firmware/CLOUDHV.fd
	StopTimeout time.Duration // 正常シャットダウンを待つ時間。既定: 60s
}

type Backend struct {
	cfg   Config
	run   sys.Runner
	links netx.Links
}

var _ backend.Backend = (*Backend)(nil)
var _ backend.SerialLogReader = (*Backend)(nil)

func New(cfg Config, run sys.Runner) *Backend {
	if cfg.Binary == "" {
		cfg.Binary = "/usr/local/bin/cloud-hypervisor"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/niwakube"
	}
	if cfg.RunDir == "" {
		cfg.RunDir = "/run/niwakube"
	}
	if cfg.Firmware == "" {
		cfg.Firmware = filepath.Join(cfg.DataDir, "firmware", "CLOUDHV.fd")
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 60 * time.Second
	}
	return &Backend{cfg: cfg, run: run, links: netx.Links{Run: run}}
}

func (b *Backend) Name() string { return Name }

func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{Isolation: backend.IsolationVM, OwnKernel: true, NICAttach: backend.NICTap}
}

func (b *Backend) vmDir(name string) string  { return filepath.Join(b.cfg.DataDir, "vms", name) }
func (b *Backend) socket(name string) string { return filepath.Join(b.cfg.RunDir, name+".sock") }
func (b *Backend) unit(name string) string   { return "niwakube-vm-" + name }
func (b *Backend) baseImage(img string) string {
	return filepath.Join(b.cfg.DataDir, "images", img+".raw")
}

// state は VM ディレクトリに保存する、Start に必要な情報。
type state struct {
	Bridge string   `json:"bridge"`
	VM     VmConfig `json:"vm"`
}

func (b *Backend) statePath(name string) string { return filepath.Join(b.vmDir(name), "state.json") }

func (b *Backend) loadState(name string) (*state, error) {
	raw, err := os.ReadFile(b.statePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, backend.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", b.statePath(name), err)
	}
	return &st, nil
}

func (b *Backend) Create(ctx context.Context, spec *backend.MachineSpec) (err error) {
	base := b.baseImage(spec.Image)
	if _, err := os.Stat(base); err != nil {
		return fmt.Errorf("%s (%s): %w", spec.Image, base, backend.ErrImageNotFound)
	}
	if _, err := os.Stat(b.cfg.Firmware); err != nil {
		return fmt.Errorf("firmware %s: %w", b.cfg.Firmware, err)
	}
	dir := b.vmDir(spec.Name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s: %w", dir, backend.ErrAlreadyExists)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	// ディスク: ベースイメージを複製して指定サイズまで広げる (パーティションはゲストの cloud-init が広げる)
	disk := filepath.Join(dir, "disk.raw")
	if _, err := b.run.Run(ctx, "cp", "--reflink=auto", "--sparse=always", base, disk); err != nil {
		return fmt.Errorf("copy base image: %w", err)
	}
	if err := growFile(disk, int64(spec.DiskGiB)<<30); err != nil {
		return err
	}

	iso, err := seed.BuildISO(ctx, b.run, dir, spec.Seed)
	if err != nil {
		return err
	}

	st := state{
		Bridge: spec.NIC.Bridge,
		VM: VmConfig{
			Cpus:    CpusConfig{BootVcpus: spec.CPUs, MaxVcpus: spec.CPUs},
			Memory:  MemoryConfig{Size: int64(spec.MemoryMiB) << 20},
			Payload: PayloadConfig{Firmware: b.cfg.Firmware},
			Disks: []DiskConfig{
				{Path: disk},
				{Path: iso, Readonly: true},
			},
			Net:     []NetConfig{{Tap: netx.TapName(spec.Name), Mac: spec.NIC.MAC}},
			Rng:     &RngConfig{Src: "/dev/urandom"},
			Serial:  &ConsoleConfig{Mode: "File", File: filepath.Join(dir, "serial.log")},
			Console: &ConsoleConfig{Mode: "Off"},
		},
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.statePath(spec.Name), raw, 0o600)
}

func growFile(path string, size int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() > size {
		return fmt.Errorf("disk size %d GiB is smaller than the base image (%d bytes)", size>>30, fi.Size())
	}
	return os.Truncate(path, size)
}

func (b *Backend) unitActive(ctx context.Context, name string) bool {
	out, _ := b.run.Run(ctx, "systemctl", "is-active", b.unit(name))
	return strings.TrimSpace(string(out)) == "active"
}

func (b *Backend) Start(ctx context.Context, name string) error {
	st, err := b.loadState(name)
	if err != nil {
		return err
	}
	if s, _ := b.Status(ctx, name); s.State == backend.StateRunning {
		return nil
	}
	if len(st.VM.Net) > 0 {
		if err := b.links.EnsureTap(ctx, st.VM.Net[0].Tap, st.Bridge); err != nil {
			return err
		}
	}

	sock := b.socket(name)
	if !b.unitActive(ctx, name) {
		if err := os.MkdirAll(b.cfg.RunDir, 0o750); err != nil {
			return err
		}
		_ = os.Remove(sock)
		if _, err := b.run.Run(ctx, "systemd-run",
			"--unit="+b.unit(name), "--collect", "--quiet",
			"--property=Description=niwakube VM "+name,
			b.cfg.Binary, "--api-socket", "path="+sock,
		); err != nil {
			return fmt.Errorf("start vmm: %w", err)
		}
	}

	c := NewClient(sock)
	if err := waitPing(ctx, c, 10*time.Second); err != nil {
		return fmt.Errorf("vmm did not come up (journalctl -u %s): %w", b.unit(name), err)
	}

	// VMM が VM を持っていなければ作る。Shutdown 状態なら boot し直すだけでよい。
	if _, err := c.Info(ctx); err != nil {
		if err := c.CreateVM(ctx, &st.VM); err != nil {
			return err
		}
	}
	return c.Boot(ctx)
}

func waitPing(ctx context.Context, c *Client, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := c.Ping(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *Backend) Stop(ctx context.Context, name string, force bool) error {
	st, err := b.loadState(name)
	if err != nil {
		return err
	}
	c := NewClient(b.socket(name))

	if b.unitActive(ctx, name) && !force {
		if s, _ := b.Status(ctx, name); s.State == backend.StateRunning {
			if err := c.PowerButton(ctx); err == nil {
				b.waitNotRunning(ctx, name)
			}
		}
	}
	if b.unitActive(ctx, name) {
		_ = c.ShutdownVMM(ctx)
		// 応答しない場合に備えて unit も止める (SIGTERM → 一定時間後 SIGKILL)
		_, _ = b.run.Run(ctx, "systemctl", "stop", b.unit(name))
	}
	_ = os.Remove(b.socket(name))
	if len(st.VM.Net) > 0 {
		return b.links.Delete(ctx, st.VM.Net[0].Tap)
	}
	return nil
}

func (b *Backend) waitNotRunning(ctx context.Context, name string) {
	deadline := time.Now().Add(b.cfg.StopTimeout)
	for time.Now().Before(deadline) {
		if s, _ := b.Status(ctx, name); s.State != backend.StateRunning {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (b *Backend) Delete(ctx context.Context, name string) error {
	if err := b.Stop(ctx, name, true); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return err
	}
	return os.RemoveAll(b.vmDir(name))
}

func (b *Backend) Status(ctx context.Context, name string) (backend.Status, error) {
	if _, err := os.Stat(b.statePath(name)); err != nil {
		return backend.Status{State: backend.StateNotFound}, nil
	}
	if !b.unitActive(ctx, name) {
		return backend.Status{State: backend.StateStopped}, nil
	}
	info, err := NewClient(b.socket(name)).Info(ctx)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// VMM は動いているが VM がまだ作られていない
			return backend.Status{State: backend.StateStopped, Detail: "vmm running, vm not created"}, nil
		}
		return backend.Status{State: backend.StateUnknown, Detail: err.Error()}, nil
	}
	switch info.State {
	case "Running":
		return backend.Status{State: backend.StateRunning}, nil
	case "Paused":
		return backend.Status{State: backend.StatePaused}, nil
	case "Created", "Shutdown":
		return backend.Status{State: backend.StateStopped, Detail: "vm " + strings.ToLower(info.State)}, nil
	default:
		return backend.Status{State: backend.StateUnknown, Detail: "vm state " + info.State}, nil
	}
}

// SerialLog はシリアルコンソールのログの末尾 maxBytes を返す。
func (b *Backend) SerialLog(_ context.Context, name string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(b.vmDir(name), "serial.log"))
	if errors.Is(err, os.ErrNotExist) {
		if _, serr := os.Stat(b.statePath(name)); serr != nil {
			return nil, fmt.Errorf("%s: %w", name, backend.ErrNotFound)
		}
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if off := fi.Size() - maxBytes; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
