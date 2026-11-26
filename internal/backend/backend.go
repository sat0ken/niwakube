// Package backend はノード (VM やコンテナ) を作る基盤を抽象化する。
//
// Backend は「箱をどう作るか」だけを担当し、中で何を動かすか (Distro) は知らない。
// 詳細な検討は docs/backends.md を参照。
package backend

import (
	"context"
	"errors"
	"net/netip"

	"github.com/sat0ken/niwakube/internal/seed"
)

var (
	ErrNotFound      = errors.New("machine not found")
	ErrAlreadyExists = errors.New("machine already exists")
	ErrImageNotFound = errors.New("image not found")
	ErrNotSupported  = errors.New("not supported by this backend")
)

// Isolation は分離の方式。
type Isolation string

const (
	IsolationVM        Isolation = "vm"
	IsolationContainer Isolation = "container"
)

// NICAttach はブリッジへのつなぎ方。
type NICAttach string

const (
	NICTap     NICAttach = "tap"
	NICVeth    NICAttach = "veth"
	NICManaged NICAttach = "managed"
)

// Capabilities はバックエンドができること。Distro の要件と突き合わせて使う。
type Capabilities struct {
	Isolation  Isolation `json:"isolation"`
	OwnKernel  bool      `json:"ownKernel"`
	NICAttach  NICAttach `json:"nicAttach"`
	Privileged bool      `json:"privileged"` // コンテナ型で特権が必要か (UI で警告を出す)
}

// NIC はノードのネットワーク設定。
type NIC struct {
	Bridge  string
	MAC     string
	Address netip.Prefix
	Gateway netip.Addr
}

// MachineSpec はノードを作るのに必要な情報。
type MachineSpec struct {
	Name      string
	CPUs      int
	MemoryMiB int
	DiskGiB   int
	Image     string // 論理名 (例: ubuntu-26.04)。実体はバックエンドが解決する
	NIC       NIC
	Seed      seed.Data
}

// State はノードの状態。
type State string

const (
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StatePaused   State = "paused"
	StateUnknown  State = "unknown"
	StateNotFound State = "not-found"
)

// Status はノードの現在の状態。
type Status struct {
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Backend はノードのライフサイクルを管理する。
type Backend interface {
	Name() string
	Capabilities() Capabilities
	// Create はディスク / rootfs・Seed など起動に必要なものを用意する (起動はしない)。
	Create(ctx context.Context, spec *MachineSpec) error
	// Start は起動する。すでに動いていれば何もしない。
	Start(ctx context.Context, name string) error
	// Stop は停止する。force が false ならゲストに正常終了を依頼する。
	Stop(ctx context.Context, name string, force bool) error
	// Delete は停止して、Create で作ったものをすべて消す。
	Delete(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (Status, error)
}

// Resources はノードの大きさ。
type Resources struct {
	CPUs      int
	MemoryMiB int
	DiskGiB   int
}

// Resizer は止まっているノードの大きさを変えられるバックエンドが実装する (任意)。
// ディスクは広げるだけ (小さくはできない)。止まっていなければエラーにする。
type Resizer interface {
	Resize(ctx context.Context, name string, r Resources) error
}

// ErrRunning は動いているノードに対してできない操作をしたときのエラー。
var ErrRunning = errors.New("machine is running")

// SerialLogReader はシリアルコンソールのログを読めるバックエンドが実装する (任意)。
type SerialLogReader interface {
	SerialLog(ctx context.Context, name string, maxBytes int64) ([]byte, error)
}
