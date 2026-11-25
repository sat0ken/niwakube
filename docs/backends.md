# バックエンド差し替えの検討

「裏の仮想化（またはコンテナ）基盤を差し替えられるようにする」ための検討メモ。実装前に方針を決めるのが目的。

対象: Cloud Hypervisor / Firecracker / libkrun / LXC (Incus) / systemd-nspawn

## 1. k8s ノードに必要なもの

バックエンドを選ぶときは、ここに挙げた要件を満たせるかどうかで判断する。

| 要件 | 主に必要とするもの |
|---|---|
| PID 1 が systemd (kubeadm / k3s が使う) | 全ディストリ (Talos は独自の init) |
| 自前のカーネル | Talos (必須)。他は無くてもよい |
| カーネル機能: overlayfs, br_netfilter, nf_conntrack, iptables/nftables, (Cilium なら eBPF) | containerd, kube-proxy, CNI |
| cgroup v2 の書き込み権限 (delegation) | kubelet, containerd |
| `/dev/kmsg` と sysctl の変更 | kubelet, kube-proxy |
| 他ノードと L2/L3 で双方向に通信できること | クラスタとして動くために必須 |
| 固定 IP と、cloud-init などで初期設定を渡せること | niwakube の払い出し方式 |

VM 型ならこれらは自然に満たせる。コンテナ型の場合は「特権コンテナでホストのカーネルを共有する」形になり、
kind / k3d / Talos の docker provisioner が実際にこの形で k8s ノードを動かしている。

## 2. 各バックエンドの評価

### Cloud Hypervisor (基準にする)
- 制御: VM ごとのプロセスに Unix socket の REST API がある
- 起動方式: UEFI (CLOUDHV.fd) とカーネル直接起動の両方に対応。Ubuntu クラウドイメージがそのまま起動する
- デバイス: virtio-pci。CPU・メモリ・ディスクのホットプラグ、スナップショット、ライブマイグレーションに対応
- 評価: ◎ 全ディストリで本命

### Firecracker
- 制御: Cloud Hypervisor とよく似ている (VM ごとのプロセス + Unix socket の REST API、tap は事前に作る)。
  `PUT /boot-source`, `/drives/{id}`, `/network-interfaces/{id}`, `/machine-config`, `/actions` で操作する
- 起動方式: **UEFI や BIOS のファームウェアは無く、カーネル直接起動のみ**。x86_64 では非圧縮の ELF (`vmlinux`) しか起動できず、bzImage は不可
  → Ubuntu クラウドイメージは「ディスクのまま」では起動できない。カーネル、initrd、rootfs を別々に用意する必要がある
- カーネル: 公式が推奨する microVM 用のカーネル設定は最小限なので、k8s に必要な netfilter / overlay / br_netfilter などを足して**自前でビルドする**必要がありそう
- デバイス: 既定は virtio-mmio。v1.13 から `--enable-pci` で virtio-pci も使える (ホットプラグはこれが前提で、まだ開発者プレビュー)
- シャットダウン: ACPI が無いので `SendCtrlAltDel` を送る。ゲストが再起動しようとすると Firecracker プロセスは終了する
- 評価: ○ kubeadm / k3s は「カーネルを用意する作業」さえやれば動く。Talos は公開されているカーネルと initramfs を直接起動する形になり △ (要検証)
- 先行例: Weaveworks の Ignite / Firekube、Liquid Metal の flintlock (Firecracker と Cloud Hypervisor を切り替えられる)

### libkrun
- 性質: デーモンでも API サーバーでもなく、**ライブラリ**。プロセスが `krun_start_enter()` を呼ぶと、そのプロセス自体が VM になる。
  カーネルは libkrunfw に同梱されている。もともとは「コンテナ 1 つを軽量 VM で包む」用途 (crun の krun ハンドラ、krunvm、muvm など)
- 制御: niwakube 用に小さなランチャー (cgo か Rust) を自作し、systemd の unit として動かす
- ネットワーク: TSI (vsock でソケット呼び出しを肩代わりする方式) と、virtio-net + passt/gvproxy の 2 通りで、同時には使えない。
  **tap を直接ブリッジにつなぐ公式の方法は見当たらない** (下流のプロジェクトが tap とソケットを中継している例はある)
  → ノード同士が nkbr0 上の同じ L2 にいる、という前提が崩れる
- カーネル: libkrunfw のカーネルにコンテナ実行に必要な機能がどこまで入っているか不明 (要調査)
- 評価: △ ノードのバックエンドとしては実験扱い (k3s のシングルノードなら可能性あり)。
  **libkrun が本領を発揮するのは「Pod の裏」の方**。クラスタ内で crun + libkrun を RuntimeClass として使うと面白い

### LXC (Incus を使う)
- 制御: Incus に REST API と公式の Go クライアント (`github.com/lxc/incus/client`) がある。ライフサイクルは Incus に任せる
- イメージ: images.linuxcontainers.org に `ubuntu/24.04/cloud` (cloud-init 入り) がある。cloud-init のデータは
  `cloud-init.user-data` などの設定キーでネイティブに渡せる
- ネットワーク: `nictype=bridged, parent=nkbr0` で既存のブリッジにつなげる → IPAM と NAT はそのまま流用できる
- k8s を動かすための調整: `security.nesting=true`、`security.privileged=true` (または非特権で KubeletInUserNamespace を使う)、
  `linux.kernel_modules=overlay,br_netfilter,ip_tables,...`、AppArmor を unconfined にする、`/dev/kmsg` を渡す、
  kube-proxy の `conntrack.maxPerCore: 0`、kubeadm では一部の preflight を無視する
- おまけ: **Incus は QEMU の VM も同じ API で扱える**。そのため「incus-vm」バックエンドもほぼタダで手に入り、比較の基準として便利
- 評価: k3s ◎ / kubeadm ○ / Talos ✕

### systemd-nspawn
- 制御: systemd-machined の D-Bus API (`org.freedesktop.machine1`)、`.nspawn` ファイル、`systemd-nspawn@.service` の組み合わせ。
  Go からは go-systemd の dbus パッケージで操作できる
- イメージ: rootfs をディレクトリか btrfs のサブボリュームとして持つ。raw のディスクイメージをそのまま起動することもできる
  (Ubuntu クラウドイメージで動くかは要検証)
- ネットワーク: `--network-bridge=nkbr0` で veth を作って nkbr0 につなぐ (コンテナ内のインターフェース名は `host0`)
- 初期設定の渡し方: `/var/lib/cloud/seed/nocloud/` に bind mount すれば、cloud-init の NoCloud がそのまま読む
- k8s を動かすための調整: `Capability=all`、cgroup の書き込み権限、`/dev/kmsg` の bind、`/lib/modules` の読み取り専用 bind、
  システムコールフィルタの緩和。cgroup v2 との相性は良い (systemd 同士なので)
- 評価: k3s ○ / kubeadm ○ / Talos ✕。追加で入れるものがほぼ無く (`systemd-container` パッケージのみ)、仕組みを学ぶにはとても良い

## 3. 対応可否マトリクス

| バックエンド | 分離 | kubeadm | k3s | Talos | 主な追加作業 |
|---|---|---|---|---|---|
| Cloud Hypervisor | VM | ◎ | ◎ | ○ 要検証 | なし |
| Firecracker | microVM | ○ | ○ | △ | k8s 向けカーネルのビルド、rootfs の切り出し |
| libkrun | microVM | △ | △ | ✕ | ランチャーの自作、ネットワーク |
| Incus (LXC) | コンテナ | ○ | ◎ | ✕ | 特権・nesting のプロファイル |
| Incus (VM) | VM (QEMU) | ◎ | ◎ | ◎ | なし |
| systemd-nspawn | コンテナ | ○ | ○ | ✕ | 権限とデバイスの設定 |

コンテナ型で注意すること:
- ホストのカーネルを共有するので、カーネルモジュールや一部の sysctl・conntrack はホスト全体で共通になり、クラスタ間で影響し合う
- 特権コンテナはほぼホストの root と同じ → **「学習用・信頼できるワークロード向け」と位置づけ、基盤用途は VM 型にする**
- containerd のスナップショッター: overlay の上に overlay は重ねられない。rootfs を置くファイルシステムに注意 (ZFS は 2.2 以降なら可)
- メモリの「割り当て」の意味が違う: VM は固定で確保されるが、コンテナは cgroup の上限にすぎない。容量計算をバックエンドごとに分ける

## 4. 抽象化の設計

### レイヤー分け

```
Cluster ──(Distro: kubeadm/k3s/talos)──▶ NodeSpec (役割, Seed の内容, 必要な Image)
                                              │
Node    ──(Backend: ch/fc/krun/incus/nspawn)──▶ Machine (起動・停止・コンソール)
                                              │
共通    ── IPAM / ブリッジ nkbr0 / NAT / Image のキャッシュ / Seed の内容生成
```

- **Distro は「中で何を動かすか」、Backend は「箱をどう作るか」** だけを担当し、互いに相手を知らないようにする
- ブリッジ、IPAM、Seed の **中身** は共通部品にする。Seed の **渡し方** だけがバックエンドごとに違う:
  - CH / Firecracker: seed ディスク (vfat か iso)
  - nspawn: `/var/lib/cloud/seed/nocloud` に bind mount
  - Incus: `cloud-init.*` の設定キー
  - (Talos は nocloud の user-data として machine config を入れる)
- 先に決めたように、秘密情報はすべて niwakubed が生成するので、ノードの中に入る手段 (SSH / exec) は必須ではない。これがバックエンド間の差を小さくしてくれる

### Go のインターフェース (案)

```go
type Backend interface {
    Name() string
    Capabilities() Capabilities
    Create(ctx context.Context, m *MachineSpec) error // ディスク/rootfs・Seed・NIC を用意する
    Start(ctx context.Context, name string) error
    Stop(ctx context.Context, name string, force bool) error
    Delete(ctx context.Context, name string) error
    Status(ctx context.Context, name string) (MachineStatus, error)
    Console(ctx context.Context, name string) (io.ReadWriteCloser, error)
}

// 任意の機能は小さなインターフェースに分け、型アサーションで有無を判定する
type Snapshotter interface{ Snapshot(ctx context.Context, name, tag string) error }
type Resizer    interface{ Resize(ctx context.Context, name string, cpus, memMiB int) error }

type Capabilities struct {
    Isolation   Isolation    // VM / Container
    OwnKernel   bool
    BootModes   []BootMode   // UEFIDisk / DirectKernel / ContainerInit
    NICAttach   NICAttach    // Tap / Veth / Managed (Incus) / None
    Privileged  bool         // コンテナ型で特権が要るか (UI に警告を出す)
}

type MachineSpec struct {
    Name      string
    CPUs      int
    MemoryMiB int
    DiskGiB   int
    Image     ImageRef // 論理名 (ubuntu-24.04, talos-v1.x)。実体はバックエンドに合わせて解決する
    NIC       NICSpec  // bridge, MAC, IP/prefix, gateway
    Seed      Seed     // user-data / meta-data / network-config
}
```

Distro 側には要件を宣言させる:

```go
type Requirements struct {
    NeedsOwnKernel bool        // Talos = true
    Images         []ImageKind // 起動に使えるイメージの形式
}
```

クラスタを作るときに `Distro.Requirements` と `Backend.Capabilities` を突き合わせ、組み合わせられないものは UI でグレーアウトする。

### イメージの形式

同じ論理イメージ (例: ubuntu-24.04) を、バックエンドに合わせて変換・キャッシュする:

| 形式 | 用途 | 作り方 |
|---|---|---|
| `disk-uefi` (raw) | CH, Incus VM | クラウドイメージを qemu-img で変換 |
| `kernel+initrd+rootfs` | Firecracker (CH でも可) | 自前でビルドした vmlinux と、rootfs を ext4 に切り出したもの |
| `rootfs-dir` | nspawn | クラウドイメージのルートパーティションを展開し、btrfs のサブボリュームか reflink でコピー |
| (Incus 管理) | Incus コンテナ | Incus のイメージサーバーから取得 |

## 5. 進め方

1. Backend インターフェースは最初から用意するが、実装は Cloud Hypervisor だけで始める
2. **2 つ目には、CH と一番性質が違う systemd-nspawn (または Incus) を選ぶ**。Firecracker は CH と似すぎていて、
   インターフェースの設計が正しいかを確かめる役に立たない
3. その次に Firecracker (カーネルのビルドが学習テーマになる)、最後に libkrun (実験)
4. 1 つのクラスタは 1 つのバックエンドに揃えるのを基本にする。ただし全バックエンドが nkbr0 につながるので、
   「CP は VM、ワーカーはコンテナ」のような混在も原理的にはできる (後で試す)

## 6. 発展: Pod の裏でハイパーバイザーを差し替える

ノードの裏だけでなく、Pod 単位でも RuntimeClass でハイパーバイザーを選べる:
- Kata Containers (裏が cloud-hypervisor / firecracker / qemu)
- crun + libkrun
- gVisor (参考)

VM 型のノードの中で使うと **nested virtualization** が必要になる (ホストで `kvm_intel nested=1`、CH 側も対応している)。
一方、コンテナ型のノードならホストの /dev/kvm を直接渡せるので、入れ子にならない。
この組み合わせは学習テーマとして面白い。
