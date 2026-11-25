// Package fake はメモリ上だけで動く Backend。テストや、KVM の無い環境での UI 開発に使う。
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/sat0ken/niwakube/internal/backend"
)

const Name = "fake"

type Backend struct {
	// CreateErr / StartErr を設定すると、その操作が失敗する。
	CreateErr error
	StartErr  error

	mu       sync.Mutex
	machines map[string]*entry
}

type entry struct {
	spec    backend.MachineSpec
	running bool
}

var _ backend.Backend = (*Backend)(nil)

func New() *Backend { return &Backend{machines: map[string]*entry{}} }

func (b *Backend) Name() string { return Name }

func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{Isolation: backend.IsolationVM, OwnKernel: true, NICAttach: backend.NICTap}
}

func (b *Backend) Create(_ context.Context, spec *backend.MachineSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.CreateErr != nil {
		return b.CreateErr
	}
	if _, ok := b.machines[spec.Name]; ok {
		return fmt.Errorf("%s: %w", spec.Name, backend.ErrAlreadyExists)
	}
	b.machines[spec.Name] = &entry{spec: *spec}
	return nil
}

func (b *Backend) get(name string) (*entry, error) {
	e, ok := b.machines[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, backend.ErrNotFound)
	}
	return e, nil
}

func (b *Backend) Start(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.get(name)
	if err != nil {
		return err
	}
	if b.StartErr != nil {
		return b.StartErr
	}
	e.running = true
	return nil
}

func (b *Backend) Stop(_ context.Context, name string, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, err := b.get(name)
	if err != nil {
		return err
	}
	e.running = false
	return nil
}

func (b *Backend) Delete(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.machines, name)
	return nil
}

func (b *Backend) Status(_ context.Context, name string) (backend.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.machines[name]
	switch {
	case !ok:
		return backend.Status{State: backend.StateNotFound}, nil
	case e.running:
		return backend.Status{State: backend.StateRunning}, nil
	default:
		return backend.Status{State: backend.StateStopped}, nil
	}
}

// Spec はテスト用に、Create で渡された MachineSpec を返す。
func (b *Backend) Spec(name string) (backend.MachineSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.machines[name]
	if !ok {
		return backend.MachineSpec{}, false
	}
	return e.spec, true
}
