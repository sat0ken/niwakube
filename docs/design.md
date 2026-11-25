# niwakube 設計メモ

名前の由来: niwa = 日本語の「庭」。自宅の庭でクラスタを育てるイメージ。

ミニPC (ASRock DeskMini 310 / RAM 64GB) 上の Cloud Hypervisor などで、
kubeadm / k3s / Talos の Kubernetes クラスタを払い出す仕組み。用途は学習と自宅の基盤の両方。

## 決定事項

| 項目 | 決定 |
|---|---|
| ホスト OS | Ubuntu Server 26.04 LTS (インストール手順は [install.md](install.md)) |
| VM の OS | Ubuntu 26.04 のクラウドイメージ (kubeadm / k3s 用) |
| Backend | インターフェースで差し替え可能。実装順は **Cloud Hypervisor → systemd-nspawn** (詳細は [backends.md](backends.md)) |
| ファームウェア | CLOUDHV.fd (EDK2 / UEFI) |
| ネットワーク | ホスト内の NAT ブリッジ `nkbr0` (172.30.0.0/16) |
| k8s ディストリビューション | kubeadm / k3s / Talos をクラスタごとに選べる |
| サーバー | Go の単一バイナリ `niwakubed` (API・Web UI・Reconciler) |
| 状態の保存 | SQLite |
| VM プロセスの管理 | systemd の transient unit (`niwakube-vm-<name>`)。niwakubed を再起動しても VM は落ちない |

## ネットワーク

```
LAN (192.168.x.x) ── enp* [ホスト] nkbr0 172.30.0.1/16 ── tap ── VM 群
                              └ nftables: masquerade (VM→外)
                                          DNAT host:16443+N → クラスタ N の API VIP:6443
```

- VM 用のサブネットに `172.30.0.0/16` を選んだのは、k8s の既定 CIDR と重ならないようにするため。
  kubeadm の Service CIDR は `10.96.0.0/12` (10.96〜10.111) なので、10.100.x.x なども使えない。
  - Pod: kubeadm/Talos は 10.244.0.0/16、k3s は 10.42.0.0/16
  - Service: kubeadm/Talos は 10.96.0.0/12、k3s は 10.43.0.0/16
- IP の払い出し方 (IPAM)
  - `172.30.0.0/24`: ホストと予約 (.1 = ゲートウェイ)
  - `172.30.N.0/24`: クラスタ N 用。`.10` が API の VIP、`.11〜` がコントロールプレーン、`.21〜` がワーカー
- VM の IP は DHCP を使わず、cloud-init (Talos は nocloud) の network-config で固定する。
- **API エンドポイントはクラスタ内の VIP にする**。kubeadm/k3s は kube-vip、Talos は組み込みの VIP 機能を使う。
  niwakubed が止まってもクラスタは動き続けるようにするため、ホスト側でロードバランサーは持たない。
- LAN の PC から kubectl を使うときは、ホストで `16443+N → VIP:6443` を DNAT する。
  証明書の SAN にはホストの LAN IP を入れる。
- LoadBalancer Service と Ingress は後で考える (MetalLB と DNAT の組み合わせなど)。

## ディストリビューションの抽象化

```go
type Distro interface {
    Name() string                                         // "kubeadm" | "k3s" | "talos"
    BaseImage(version string) ImageRef                    // 元にするディスクイメージ
    PrepareCluster(ctx context.Context, c *Cluster) error // CA・トークン・シークレットを生成して DB に保存
    NodeSeed(c *Cluster, n *Node) (*Seed, error)          // user-data / meta-data / network-config
    Bootstrap(ctx context.Context, c *Cluster) error      // Talos だけ: bootstrap API を呼ぶ
    Kubeconfig(ctx context.Context, c *Cluster) ([]byte, error)
}
```

| | kubeadm | k3s | Talos |
|---|---|---|---|
| イメージ | Ubuntu クラウドイメージ | Ubuntu クラウドイメージ | Image Factory の `nocloud-amd64.raw` |
| ノードの設定 | cloud-init (containerd・kubelet・kubeadm を入れて init / join) | cloud-init (`get.k3s.io` に `K3S_TOKEN` を渡す) | Go で machine config を生成し、nocloud の user-data として渡す |
| join に使う情報 | サーバーが事前に CA とトークンを生成するので、discovery hash が最初から分かる | 事前に生成したトークン | secrets bundle |
| kubeconfig の取得 | 自分で持っている CA で admin 証明書に署名して作る | 事前に CA を渡しておいて同じ方法で作る | Talos API (machinery クライアント) で取得 |
| VIP | kube-vip (static pod) | kube-vip | machine config の `vip` |

ポイント: **秘密情報 (CA など) はすべて niwakubed 側で生成する**。こうすると、VM から kubeconfig を受け取る
コールバックや SSH が要らなくなり、再実行もしやすい。

kubeadm は起動のたびに apt でパッケージを入れるので遅い。慣れてきたら、パッケージを入れ済みのイメージを事前に作る
(フェーズ4)。

## ストレージ

```
/var/lib/niwakube/
  firmware/CLOUDHV.fd
  images/ubuntu-26.04.raw, talos-<ver>.raw
  vms/<vm>/disk.raw, seed.iso, serial.log
  ssh/id_ed25519            # VM にログインするための鍵
  niwakube.db                 # SQLite
/run/niwakube/<vm>.sock       # CH API socket
```

- ディスクは `cp --reflink=auto` で複製し、raw のまま使う。`/var/lib/niwakube` を XFS にすると、複製が一瞬で終わり容量もほとんど使わない。
- qcow2 は使わない (CH では raw のほうが性能も実績もあるため)。

## コンポーネント (Go)

```
cmd/niwakubed/            エントリーポイント
internal/ch/            Cloud Hypervisor REST クライアント (Unix socket 上の HTTP)
internal/backend/        Backend インターフェースと実装 (ch/ 、後で nspawn/ firecracker/ incus/ krun/)
internal/netx/           ブリッジ・tap/veth・NAT の共通処理
internal/ipam/          IP と MAC の払い出し
internal/store/         SQLite
internal/distro/        kubeadm/ k3s/ talos/
internal/reconcile/     望ましい状態 (DB) と実際の VM を一致させるループ
internal/api/           REST API と WebSocket (シリアルコンソール)
web/                    React + Vite。ビルド結果を embed で取り込む
scripts/                ホストのセットアップと検証用スクリプト
```

### REST API (案)

```
POST   /api/clusters                 {name, distro, version, controlPlanes, workers, cpPlan, workerPlan}
GET    /api/clusters
GET    /api/clusters/{name}
DELETE /api/clusters/{name}
PATCH  /api/clusters/{name}          {workers: 3}  ← スケール
GET    /api/clusters/{name}/kubeconfig
GET    /api/vms
GET    /api/vms/{name}/console       WebSocket (シリアルコンソール → xterm.js)
GET    /api/host                     CPU / メモリ / ディスクの使用状況と割り当て状況
```

## リソースの目安 (64GB)

ホスト用に 4GB を残し、約 60GB を VM に割り当てる。CPU は 4〜8 コア程度の見込みなので、vCPU はオーバーコミットする前提にする。

| プラン | vCPU | メモリ | ディスク |
|---|---|---|---|
| small | 2 | 4GB | 20GB |
| medium | 2 | 8GB | 40GB |
| large | 4 | 16GB | 80GB |

例: 「CP 1台 small + ワーカー 2台 medium」のクラスタ (20GB) なら 3 つ同時に動かせる。

## フェーズ

0. ホストのセットアップと、VM を手で1台起動 (`scripts/host-setup.sh`、`scripts/test-vm.sh`)
1. niwakubed: VM 単体の作成・削除 (CH クライアント、systemd、IPAM、SQLite) ← いまここ (実装済み・実機未検証)
2. niwakubed: k3s クラスタの払い出し (一番単純なので最初に)。次に kubeadm、最後に Talos
3. systemd-nspawn バックエンド (Backend インターフェースが正しく切れているかの検証を兼ねる)
4. Web UI
5. 発展: Firecracker / libkrun バックエンド、シリアルコンソール、スケール、事前ビルドしたイメージ、LAN 公開 (DNAT)、スナップショット

## 未検証 (実機で確認すること)

- Talos の nocloud イメージが CLOUDHV.fd で起動するか (起動しなければ BIOS 用ファームウェアか、カーネルを直接起動する方法を試す)
- ゲストからの ACPI シャットダウンで VMM プロセスがきちんと終了するか
- kube-vip (ARP) が nkbr0 上で問題なく動くか
