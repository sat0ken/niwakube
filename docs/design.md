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
internal/webui/          Web UI。素の HTML / CSS / JavaScript を embed で取り込む (ビルドの手順は無い)
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
| small | 2 | 2GB | 30GB |
| medium | 2 | 4GB | 30GB |
| large | 4 | 8GB | 30GB |

例: 「CP 1台 small + ワーカー 2台 medium」のクラスタ (メモリ 10GB) なら 6 つ同時に動かせる。
ディスクは見かけ上 1 クラスタ 90GB になるが、raw イメージは疎ファイル (XFS なら reflink) なので、実際に使うのは書き込んだ分だけ。

## フェーズ

0. ホストのセットアップと、VM を手で1台起動 (`scripts/host-setup.sh`、`scripts/test-vm.sh`)
1. niwakubed: VM 単体の作成・削除 (CH クライアント、systemd、IPAM、SQLite) (完了)
2. niwakubed: k3s クラスタの払い出し (一番単純なので最初に)。次に kubeadm、最後に Talos
   - 2a: クラスタの仕組み + k3s (CP 1 台 + ワーカー N 台) (完了)
   - 2b: kubeadm (完了)
   - 2b+: CNI の選択 (Cilium を既定、kube-proxy の置き換え、Hubble) とコンテナランタイムの選択 (runc / crun / youki) (完了)
   - 2c: Talos (完了)
   - 2d: LoadBalancer (Cilium LB IPAM + L2 アナウンス、ホストでの DNAT) (完了)
   - 2e: PV (local-path + ホストの NFS) (完了)
   - その後 CP の HA (VIP)、ワーカー数の変更
3. Web UI (v0.1.0 に入れる) ← いまここ
4. systemd-nspawn バックエンド (Backend インターフェースが正しく切れているかの検証を兼ねる)
5. 発展: Firecracker / libkrun バックエンド、対話できるシリアルコンソール、スケール、事前ビルドしたイメージ、スナップショット

## 未検証 (実機で確認すること)

- Talos の nocloud イメージが CLOUDHV.fd で起動するか (起動しなければ BIOS 用ファームウェアか、カーネルを直接起動する方法を試す)
- kube-vip (ARP) が nkbr0 上で問題なく動くか

## 検証済み (GitHub Actions の E2E、ubuntu-26.04 ランナー + nested KVM、Cloud Hypervisor v55.0)

- Ubuntu 26.04 クラウドイメージが CLOUDHV.fd で起動し、cloud-init (NoCloud, seed ISO) で固定 IP・ホスト名・SSH 鍵が設定される
- nkbr0 + nftables の NAT で VM から外部に出られる
- ACPI の電源ボタンで正常停止し、その後 vm.create からやり直して再起動できる
- v55 からディスクに `image_type` の指定が必須 (指定しないと `Image type required for disk`)
- k3s (v1.37.2+k3s1) が niwakubed の作ったカスタム CA で起動し、niwakubed が発行した admin 証明書で API を操作できる。
  CP 1 + ワーカー 1 が約 40 秒で Ready になり、Service・クラスタ内 DNS・ノードをまたぐ Pod 間通信 (flannel VXLAN) も動く
- Cloud Hypervisor の API サーバーは同時接続数に上限がある。keep-alive で接続を残すと約 40 秒で新しい接続が拒否されるため、
  クライアントはリクエストごとに接続を閉じる
- kubeadm (v1.37.2、containerd 2.2) が niwakubed の作った CA・SA 鍵で init でき、ワーカーは事前に計算した CA hash で join できる。
  CP 1 + ワーカー 1 が約 70 秒で Ready になり、Service・クラスタ内 DNS・ノードをまたぐ Pod 間通信 (flannel VXLAN) も動く

## Web UI

niwakubed が `/` で配信する。React などのフレームワークやビルドの手順は使わず、素の HTML / CSS / JavaScript を
`internal/webui/static/` に置いて `go:embed` で埋め込む (Node.js が無くてもビルドでき、バイナリ 1 つで完結する)。

- 画面: ダッシュボード (ホストの CPU・メモリ・ディスクとクラスタの一覧)、クラスタの作成、クラスタの詳細
  (ノード、kubeconfig / talosconfig のダウンロード、起動・停止・削除、ポートの転送、シリアルログ)、VM の一覧と作成
- 表示中の画面は 5 秒ごとに更新する
- 認証はまだ無い。既定は `127.0.0.1:8080` で待ち受けるので、LAN の PC からは SSH のポート転送で開く
- ブラウザ経由の攻撃への対策 (`internal/api/security.go`):
  - DNS リバインディング: loopback で待ち受けているときは Host ヘッダーが localhost / 127.0.0.1 / ::1 以外なら拒否
  - CSRF: 変更を伴うリクエストは Origin が自分と違えば拒否、Sec-Fetch-Site が same-origin / none 以外なら拒否
    (curl などこれらを付けないクライアントはそのまま通す)
  - CSP (`default-src 'self'`) で外部のスクリプトとインラインのスクリプト・スタイル、フレームへの埋め込みを禁止
- KVM の無い PC で UI を触るには `niwakubed --dev-fake-backend` (VM を実際には作らない) を使う
- ターミナル (`internal/terminal`): ブラウザの xterm.js と WebSocket でつなぎ、niwakubed が PTY を作って
  `systemd-run --pty -p DynamicUser=yes ...` でシェルを起動する。クラスタの kubeconfig / talosconfig は
  `LoadCredential` で渡し、データディレクトリと API socket のディレクトリは `InaccessiblePaths` で隠す。
  クラスタへの権限は kubeconfig のダウンロードと同じで、増えるのは「ホストの権限の無い一時ユーザーのシェル」だけ。
  WebSocket の Origin は Host と同じであること (Cross-Site WebSocket Hijacking の対策)。
  xterm.js は `internal/webui/static/vendor/xterm/` に置いて埋め込む (MIT。CDN は使わない)。
  xterm.js が `<style>` を作るので CSP は `style-src 'self' 'unsafe-inline'` (スクリプトは `'self'` のみ)
