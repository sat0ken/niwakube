// Package nspawn は systemd-nspawn のバックエンド (コンテナ型のノード)。Docker は使わない。
//
// ノード 1 台につき、systemd-nspawn を systemd の transient unit (niwakube-vm-<name>) として
// --boot で起動する (中で systemd が PID 1 として動く「VM のような」コンテナ)。
//
//   - ルート: Ubuntu のクラウドイメージのルートファイルシステム (images/<image>.rootfs) を下に、
//     ノードごとの upper を上に重ねた overlayfs (vms/<name>/rootfs)。下の層はノード間で共有する
//   - ネットワーク: ホスト側でノードごとのネットワーク名前空間と veth を作り、nkbr0 につなぐ。
//     中の eth0 には niwakube の IPAM どおりの MAC・アドレス・デフォルトルートを付けておく
//     (--network-namespace-path で、その名前空間に入って起動する)
//   - cloud-init: シードのディレクトリを NoCloud として /var/lib/cloud/seed/nocloud に見せる。
//     VM と同じ user-data / network-config をそのまま使える
//   - ボリューム: コンテナの中の containerd / kubelet のディレクトリはホストのディレクトリにする
//     (containerd の overlayfs は overlayfs の上に置けないため。kind も同じ理由で分けている)
//
// Talos (イメージ名が talos-<バージョン>) は Talos 公式のコンテナイメージ (ghcr.io/siderolabs/talos) を
// レジストリから取得して下の層にし、--boot ではなく Talos の /sbin/init を PID 1 として起動する
// (PLATFORM=container。Talos の Docker 用の作りと同じ tmpfs とボリュームを用意する)。
//
// Kubernetes を中で動かすため特権 (全 capability、seccomp なし、/proc/sys 書き込み可) で動かす。
// ホストのカーネルを共有するので、Cilium のようにカーネルの状態を持つ CNI は使えない。
package nspawn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sat0ken/niwakube/internal/backend"
	"github.com/sat0ken/niwakube/internal/netx"
	"github.com/sat0ken/niwakube/internal/oci"
	"github.com/sat0ken/niwakube/internal/seed"
	"github.com/sat0ken/niwakube/internal/sys"
)

const Name = "nspawn"

type Config struct {
	DataDir string // 既定: /var/lib/niwakube
	// Pull は OCI イメージを取得して dir に展開する (既定: oci.Pull。テストで差し替える)
	Pull        func(ctx context.Context, ref, dir string) error
	StopTimeout time.Duration // 正常シャットダウンを待つ時間。既定: 60s
	// Poll は停止を待つときの間隔 (テスト用。既定 1s)
	Poll time.Duration
}

type Backend struct {
	cfg   Config
	run   sys.Runner
	links netx.Links

	pullMu sync.Mutex // イメージの取得を直列化する
}

var (
	_ backend.Backend         = (*Backend)(nil)
	_ backend.SerialLogReader = (*Backend)(nil)
	_ backend.Resizer         = (*Backend)(nil)
)

func New(cfg Config, run sys.Runner) *Backend {
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/niwakube"
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 60 * time.Second
	}
	if cfg.Poll == 0 {
		cfg.Poll = time.Second
	}
	if cfg.Pull == nil {
		cfg.Pull = func(ctx context.Context, ref, dir string) error { return oci.Pull(ctx, ref, dir) }
	}
	return &Backend{cfg: cfg, run: run, links: netx.Links{Run: run}}
}

func (b *Backend) Name() string { return Name }

func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{Isolation: backend.IsolationContainer, OwnKernel: false, NICAttach: backend.NICVeth, Privileged: true}
}

func (b *Backend) vmDir(name string) string     { return filepath.Join(b.cfg.DataDir, "vms", name) }
func (b *Backend) rootfs(name string) string    { return filepath.Join(b.vmDir(name), "rootfs") }
func (b *Backend) statePath(name string) string { return filepath.Join(b.vmDir(name), "state.json") }
func (b *Backend) unit(name string) string      { return "niwakube-vm-" + name }
func (b *Backend) baseRootfs(img string) string {
	return filepath.Join(b.cfg.DataDir, "images", img+".rootfs")
}

// ノードの種類。
const (
	kindSystemd = "systemd" // Ubuntu など。systemd を --boot で起動し、cloud-init で設定する
	kindTalos   = "talos"   // Talos のコンテナ。Talos の init を PID 1 で起動し、設定は USERDATA で渡す
)

func kindOf(image string) string {
	if strings.HasPrefix(image, "talos-") {
		return kindTalos
	}
	return kindSystemd
}

// ociRef はレジストリから取得するイメージの名前を返す (talos-v1.14.7 → ghcr.io/siderolabs/talos:v1.14.7)。
// それ以外のイメージは host-setup.sh が用意する。
func ociRef(image string) (string, bool) {
	if v, ok := strings.CutPrefix(image, "talos-"); ok && v != "" {
		return "ghcr.io/siderolabs/talos:" + v, true
	}
	return "", false
}

// volumes はホストのディレクトリにするコンテナの中のパス (vms/<name>/volumes/<名前> に置く)。
var volumes = map[string]map[string]string{
	kindSystemd: {
		"rancher":    "/var/lib/rancher",    // k3s (containerd と kubelet のデータもここ)
		"containerd": "/var/lib/containerd", // kubeadm の containerd
		"kubelet":    "/var/lib/kubelet",
	},
	// Talos が Docker で動くときの EPHEMERAL と STATE。/opt はボリュームにしない:
	// イメージの /opt/cni/bin に CNI プラグイン (flannel など) があり、空のディレクトリを重ねると隠れてしまう
	// (Docker のボリュームと違って中身はコピーされない)。overlay の upper に書けるので、ノードごとに残る
	kindTalos: {
		"var":   "/var",
		"state": "/system/state",
	},
}

// cloudCfg は cloud-init の設定。シードのディレクトリ (NoCloud) だけを見て、ネットワークは設定しない
// (eth0 のアドレスとルートはホスト側で付けてある。DNS は /etc/resolv.conf に書く)。
const cloudCfg = `# niwakube (nspawn): シードは /var/lib/cloud/seed/nocloud にバインドしてある
datasource_list: [ NoCloud, None ]
# ネットワークはホスト側で設定済み (ネットワーク名前空間の eth0)
network:
  config: disabled
`

// state は vms/<name>/state.json に保存する、Start に必要な情報。
type state struct {
	Kind      string       `json:"kind,omitempty"` // 空は systemd
	Image     string       `json:"image"`
	CPUs      int          `json:"cpus"`
	MemoryMiB int          `json:"memoryMiB"`
	Bridge    string       `json:"bridge"`
	MAC       string       `json:"mac"`
	Address   netip.Prefix `json:"address"`
	Gateway   netip.Addr   `json:"gateway"`
}

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

func (b *Backend) saveState(name string, st *state) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.statePath(name) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.statePath(name))
}

// Create はノードのディレクトリ (overlayfs の upper / work、シード、ボリューム) を用意する。
// ディスクの大きさ (DiskGiB) はホストのディスクを共有するので使わない。
func (b *Backend) Create(ctx context.Context, spec *backend.MachineSpec) (err error) {
	if err := b.ensureImage(ctx, spec.Image); err != nil {
		return err
	}
	kind := kindOf(spec.Image)
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
	dirs := []string{"upper", "work", "rootfs", "seed"}
	for v := range volumes[kind] {
		dirs = append(dirs, filepath.Join("volumes", v))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	dns, err := seed.Nameservers(spec.Seed.NetworkConfig)
	if err != nil {
		return err
	}
	var resolv strings.Builder
	resolv.WriteString("# niwakube (nspawn)\n")
	for _, d := range dns {
		fmt.Fprintf(&resolv, "nameserver %s\n", d)
	}
	st := &state{
		Kind: kind, Image: spec.Image, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB,
		Bridge: spec.NIC.Bridge, MAC: spec.NIC.MAC, Address: spec.NIC.Address, Gateway: spec.NIC.Gateway,
	}
	if kind == kindTalos {
		// Talos は /etc/hostname と /etc/resolv.conf を読み、設定は環境変数 USERDATA から読む
		files := map[string][]byte{
			"hostname":    []byte(spec.Name + "\n"),
			"resolv.conf": []byte(resolv.String()),
			"hosts":       []byte(fmt.Sprintf("127.0.0.1 localhost\n::1 localhost\n%s %s\n", spec.NIC.Address.Addr(), spec.Name)),
			"talos.yaml":  spec.Seed.UserData,
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				return err
			}
		}
		return b.saveState(spec.Name, st)
	}
	// cloud-init はシードのディレクトリ (NoCloud) だけを見る。upper に置けば合成したルートに見える
	cfgDir := filepath.Join(dir, "upper", "etc", "cloud", "cloud.cfg.d")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "90-niwakube-nspawn.cfg"), []byte(cloudCfg), 0o644); err != nil {
		return err
	}
	// ネットワークはホスト側で設定するので、中ではネットワークの準備を待たない (待つと既定で 2 分かかる)。
	// DNS は network-config の DNS サーバーを /etc/resolv.conf に直接書く (どちらも upper に置く)
	mask := filepath.Join(dir, "upper", "etc", "systemd", "system")
	if err := os.MkdirAll(mask, 0o755); err != nil {
		return err
	}
	if err := os.Symlink("/dev/null", filepath.Join(mask, "systemd-networkd-wait-online.service")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "upper", "etc", "resolv.conf"), []byte(resolv.String()), 0o644); err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"user-data": spec.Seed.UserData, "meta-data": spec.Seed.MetaData, "network-config": spec.Seed.NetworkConfig,
	} {
		if err := os.WriteFile(filepath.Join(dir, "seed", name), data, 0o600); err != nil {
			return err
		}
	}
	return b.saveState(spec.Name, st)
}

// ensureImage は下の層のルートファイルシステムを用意する。無ければ、レジストリから取れるイメージなら取得する。
func (b *Backend) ensureImage(ctx context.Context, image string) error {
	base := b.baseRootfs(image)
	exists := func() bool { fi, err := os.Stat(base); return err == nil && fi.IsDir() }
	if exists() {
		return nil
	}
	ref, ok := ociRef(image)
	if !ok {
		return fmt.Errorf("%s (%s): %w", image, base, backend.ErrImageNotFound)
	}
	b.pullMu.Lock()
	defer b.pullMu.Unlock()
	if exists() {
		return nil
	}
	if err := b.cfg.Pull(ctx, ref, base); err != nil {
		return fmt.Errorf("%s: %w", image, err)
	}
	return nil
}

func (b *Backend) unitActive(ctx context.Context, name string) bool {
	out, _ := b.run.Run(ctx, "systemctl", "is-active", b.unit(name))
	return strings.TrimSpace(string(out)) == "active"
}

// mounted はノードのルートの overlayfs がマウントされているかを返す。
func (b *Backend) mounted(ctx context.Context, name string) bool {
	_, err := b.run.Run(ctx, "mountpoint", "-q", b.rootfs(name))
	return err == nil
}

func (b *Backend) mount(ctx context.Context, name string, st *state) error {
	if b.mounted(ctx, name) {
		return nil
	}
	dir := b.vmDir(name)
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		b.baseRootfs(st.Image), filepath.Join(dir, "upper"), filepath.Join(dir, "work"))
	if _, err := b.run.Run(ctx, "mount", "-t", "overlay", "overlay", "-o", opts, b.rootfs(name)); err != nil {
		return fmt.Errorf("mount rootfs of %s: %w", name, err)
	}
	return nil
}

func (b *Backend) unmount(ctx context.Context, name string) error {
	if !b.mounted(ctx, name) {
		return nil
	}
	if _, err := b.run.Run(ctx, "umount", b.rootfs(name)); err != nil {
		return fmt.Errorf("unmount rootfs of %s: %w", name, err)
	}
	return nil
}

func (b *Backend) netns(name string, st *state) netx.NetnsSpec {
	return netx.NetnsSpec{
		Name: netx.NetnsName(name), HostVeth: netx.VethName(name), Bridge: st.Bridge,
		MAC: st.MAC, Address: st.Address, Gateway: st.Gateway,
	}
}

// nspawnArgs は systemd-run に渡す引数 (unit の設定と systemd-nspawn のコマンド)。
func (b *Backend) nspawnArgs(name string, st *state) []string {
	dir := b.vmDir(name)
	kind := st.Kind
	if kind == "" {
		kind = kindSystemd
	}
	args := []string{
		"--unit=" + b.unit(name), "--collect", "--quiet",
		"--property=Description=niwakube container " + name,
		// コンテナの中の systemd / containerd / kubelet が cgroup を使えるように委譲する
		"--property=Delegate=yes",
		"--property=KillMode=mixed",
		"--property=TasksMax=infinity",
		fmt.Sprintf("--property=CPUQuota=%d%%", st.CPUs*100),
		fmt.Sprintf("--property=MemoryMax=%dM", st.MemoryMiB),
		// kubelet が sysctl を書けるように /proc/sys などを書き込み可にし、seccomp の制限を外す
		"--setenv=SYSTEMD_NSPAWN_API_VFS_WRITABLE=yes",
		"--setenv=SYSTEMD_SECCOMP=0",
	}
	if kind == kindTalos {
		// Talos の設定 (クラスタの秘密鍵を含む) は unit の Environment= ではなく root だけが読めるファイルで渡す
		// (Environment= は一般ユーザーからも見える)。nspawn は --setenv=NAME で自分の環境変数を中に渡す
		args = append(args, "--property=EnvironmentFile="+filepath.Join(dir, "talos.env"))
	}
	args = append(args,
		"systemd-nspawn", "--quiet", "--keep-unit",
		"--machine="+name,
		"--directory="+b.rootfs(name),
		"--network-namespace-path="+netx.NetnsPath(name),
		"--capability=all", "--private-users=no",
		"--console=read-only", "--link-journal=no", "--resolv-conf=off", "--timezone=off",
		"--bind=/dev/kmsg",
	)
	for _, v := range sortedVolumes(kind) {
		args = append(args, "--bind="+filepath.Join(dir, "volumes", v)+":"+volumes[kind][v])
	}
	if kind == kindTalos {
		return append(args,
			"--setenv=PLATFORM", "--setenv=USERDATA", "--setenv=TALOSSKU",
			"--tmpfs=/system", "--tmpfs=/tmp",
			"--bind-ro="+filepath.Join(dir, "hostname")+":/etc/hostname",
			"--bind-ro="+filepath.Join(dir, "resolv.conf")+":/etc/resolv.conf",
			"--bind-ro="+filepath.Join(dir, "hosts")+":/etc/hosts",
			"/sbin/init",
		)
	}
	return append(args,
		"--boot",
		"--bind-ro="+filepath.Join(dir, "seed")+":/var/lib/cloud/seed/nocloud",
		"--bind-ro=/lib/modules",
	)
}

func sortedVolumes(kind string) []string {
	out := make([]string, 0, len(volumes[kind]))
	for v := range volumes[kind] {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// writeTalosEnv は Talos に渡す環境変数のファイル (unit の EnvironmentFile=) を書く。
func (b *Backend) writeTalosEnv(name string, st *state) error {
	cfg, err := os.ReadFile(filepath.Join(b.vmDir(name), "talos.yaml"))
	if err != nil {
		return err
	}
	env := fmt.Sprintf("PLATFORM=container\nUSERDATA=%s\nTALOSSKU=%dCPU-%dRAM\n",
		base64.StdEncoding.EncodeToString(cfg), st.CPUs, st.MemoryMiB)
	return os.WriteFile(filepath.Join(b.vmDir(name), "talos.env"), []byte(env), 0o600)
}

// Start はネットワーク名前空間とルートを用意して、systemd-nspawn を起動する。
func (b *Backend) Start(ctx context.Context, name string) error {
	st, err := b.loadState(name)
	if err != nil {
		return err
	}
	if b.unitActive(ctx, name) {
		return nil
	}
	if err := b.links.EnsureNetns(ctx, b.netns(name, st)); err != nil {
		return err
	}
	if err := b.mount(ctx, name, st); err != nil {
		return err
	}
	if st.Kind == kindTalos {
		if err := b.writeTalosEnv(name, st); err != nil {
			return err
		}
	}
	// 前回失敗した unit が残っていると同じ名前で起動できない
	_, _ = b.run.Run(ctx, "systemctl", "reset-failed", b.unit(name))
	if _, err := b.run.Run(ctx, "systemd-run", b.nspawnArgs(name, st)...); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// Stop はコンテナの systemd に電源断を依頼し (force なら待たずに)、止まったらルートとネットワークを片付ける。
func (b *Backend) Stop(ctx context.Context, name string, force bool) error {
	st, err := b.loadState(name)
	if err != nil {
		return err
	}
	if b.unitActive(ctx, name) && !force {
		// systemd は電源断の依頼 (SIGRTMIN+4)、Talos は SIGTERM で正常終了する
		cmd := []string{"poweroff", name}
		if st.Kind == kindTalos {
			cmd = []string{"kill", "--kill-whom=leader", "--signal=SIGTERM", name}
		}
		if _, err := b.run.Run(ctx, "machinectl", cmd...); err == nil {
			b.waitInactive(ctx, name)
		}
	}
	if b.unitActive(ctx, name) {
		_, _ = b.run.Run(ctx, "systemctl", "stop", b.unit(name))
	}
	_, _ = b.run.Run(ctx, "systemctl", "reset-failed", b.unit(name))
	if err := b.unmount(ctx, name); err != nil {
		return err
	}
	return b.links.DeleteNetns(ctx, b.netns(name, st).Name, b.netns(name, st).HostVeth)
}

func (b *Backend) waitInactive(ctx context.Context, name string) {
	deadline := time.Now().Add(b.cfg.StopTimeout)
	for time.Now().Before(deadline) && b.unitActive(ctx, name) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.cfg.Poll):
		}
	}
}

// Delete は止めて、ノードのディレクトリを消す (下の層の共有のルートは残る)。
// Snapshot は止まっているコンテナのディレクトリ (rootfs の差分、ボリューム、seed、設定) を dest に複製する。
// ベースの rootfs (images/) は共有なので含めない。
func (b *Backend) Snapshot(ctx context.Context, name, dest string) error {
	if b.unitActive(ctx, name) || b.mounted(ctx, name) {
		return fmt.Errorf("%s: stop it before taking a snapshot: %w", name, backend.ErrRunning)
	}
	return backend.SnapshotDir(b.vmDir(name), dest)
}

// Restore はコンテナのディレクトリを src の状態に戻す (無ければ作る)。netns と veth は次の Start で作る。
func (b *Backend) Restore(ctx context.Context, name, src string) error {
	if b.unitActive(ctx, name) || b.mounted(ctx, name) {
		return fmt.Errorf("%s: stop it before restoring: %w", name, backend.ErrRunning)
	}
	return backend.RestoreDir(b.vmDir(name), src)
}

func (b *Backend) Delete(ctx context.Context, name string) error {
	if err := b.Stop(ctx, name, true); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return err
	}
	// マウントが残っていると、消すときに合成したルートをたどってしまうので確かめる
	if b.mounted(ctx, name) {
		return fmt.Errorf("rootfs of %s is still mounted", name)
	}
	return os.RemoveAll(b.vmDir(name))
}

func (b *Backend) Status(ctx context.Context, name string) (backend.Status, error) {
	if _, err := os.Stat(b.statePath(name)); err != nil {
		return backend.Status{State: backend.StateNotFound}, nil
	}
	if b.unitActive(ctx, name) {
		return backend.Status{State: backend.StateRunning}, nil
	}
	return backend.Status{State: backend.StateStopped}, nil
}

// Resize は止まっているノードの vCPU (CPUQuota) とメモリ (MemoryMax) を変える。次の Start から効く。
// ディスクはホストと共有なので変えない。
func (b *Backend) Resize(ctx context.Context, name string, r backend.Resources) error {
	st, err := b.loadState(name)
	if err != nil {
		return err
	}
	if b.unitActive(ctx, name) {
		return fmt.Errorf("%s: stop it before resizing: %w", name, backend.ErrRunning)
	}
	if r.CPUs > 0 {
		st.CPUs = r.CPUs
	}
	if r.MemoryMiB > 0 {
		st.MemoryMiB = r.MemoryMiB
	}
	return b.saveState(name, st)
}

// SerialLog はコンテナのコンソールの出力 (unit のジャーナル) の末尾 maxBytes を返す。
func (b *Backend) SerialLog(ctx context.Context, name string, maxBytes int64) ([]byte, error) {
	if _, err := os.Stat(b.statePath(name)); err != nil {
		return nil, fmt.Errorf("%s: %w", name, backend.ErrNotFound)
	}
	out, err := b.run.Run(ctx, "journalctl", "--unit="+b.unit(name), "--output=cat", "--no-pager", "--lines=5000")
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxBytes {
		out = out[int64(len(out))-maxBytes:]
	}
	return out, nil
}
