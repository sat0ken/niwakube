# niwakube

[![CI](https://github.com/sat0ken/niwakube/actions/workflows/ci.yml/badge.svg)](https://github.com/sat0ken/niwakube/actions/workflows/ci.yml)
[![E2E (KVM)](https://github.com/sat0ken/niwakube/actions/workflows/e2e.yml/badge.svg)](https://github.com/sat0ken/niwakube/actions/workflows/e2e.yml)

niwa = 日本語の「庭」。自宅の庭で Kubernetes クラスタを育てる。

Cloud Hypervisor などの仮想化／コンテナ基盤の上に Kubernetes クラスタ (kubeadm / k3s / Talos) を払い出す自宅ラボ用の仕組み。
設計は [docs/design.md](docs/design.md)、バックエンド差し替えの検討は [docs/backends.md](docs/backends.md) を参照。

## フェーズ0: ホストのセットアップ

1. Ubuntu Server 26.04 LTS をインストールする。BIOS の設定や `/var/lib/niwakube` を XFS にする方法も含めて、
   手順は [docs/install.md](docs/install.md) を参照。
   `installer/config.yaml` を書いて PR を出すと、**全自動でインストールする ISO** を CI が作る ([docs/installer.md](docs/installer.md))
2. このリポジトリを clone して、以下を実行する

```bash
sudo ./scripts/host-setup.sh
```

```bash
sudo ./scripts/test-vm.sh up test
```

```bash
sudo ./scripts/test-vm.sh down test
```

`up` が「OK!」で終われば、Cloud Hypervisor・UEFI ファームウェア・cloud-init・ブリッジ・NAT がすべて動いている。

## フェーズ1: niwakubed (VM の REST API)

ビルドには Go 1.26.5 以上が必要 (Talos の machinery ライブラリの要件)。`CGO_ENABLED=0` の静的バイナリなので、別のマシンでビルドしてコピーしてもよい。

```bash
make build
```

```bash
sudo make install
```

```bash
sudo systemctl enable --now niwakubed
```

既定では `127.0.0.1:8080` で待ち受ける (認証がまだ無いため)。LAN から使う場合は
`deploy/niwakubed.service` の `--listen` を変える。

### Web UI

niwakubed は `127.0.0.1:8080` で Web UI も配信する。クラスタの作成・削除・起動・停止、kubeconfig のダウンロード、
ポートの転送、シリアルログの確認、単体 VM の作成ができる。

LAN の PC からは SSH のポート転送で開く (niwakubed は認証が無いので、既定では LAN に公開しない):

```bash
ssh -L 18080:localhost:8080 niwa
```

そのまま PC のブラウザで `http://localhost:18080/` を開く (PC 側のポートは 18080。ほかのアプリの 8080 とぶつからないように)。

KVM の無い PC で UI だけを触るときは、VM を実際には作らない fake バックエンドで起動できる:

```bash
go run ./cmd/niwakubed --dev-fake-backend --data-dir /tmp/niwakube-dev --run-dir /tmp/niwakube-dev/run
```

### API

| メソッド | パス | 説明 |
|---|---|---|
| GET | `/api/host` | ホストの CPU・メモリ・ディスクと、VM に割り当てた量 |
| GET | `/api/backends` | 使えるバックエンドの一覧 |
| GET | `/api/vms` | VM 一覧 (望ましい状態と現在の状態) |
| POST | `/api/vms` | VM を作成して起動する |
| GET | `/api/vms/{name}` | VM の詳細 |
| DELETE | `/api/vms/{name}` | VM を停止して削除する |
| POST | `/api/vms/{name}/start` | 起動 |
| POST | `/api/vms/{name}/stop` | 停止 (`?force=true` で強制停止) |
| GET | `/api/vms/{name}/serial` | シリアルコンソールのログ (`?bytes=N` で末尾 N バイト) |

```bash
curl -s -X POST localhost:8080/api/vms -d '{"name":"vm1","cpus":2,"memoryMiB":4096,"diskGiB":20}'
```

```bash
curl -s localhost:8080/api/vms/vm1
```

```bash
curl -s localhost:8080/api/vms/vm1/serial
```

```bash
sudo ssh -i /var/lib/niwakube/ssh/id_ed25519 ubuntu@172.30.0.10
```

```bash
curl -s -X DELETE localhost:8080/api/vms/vm1
```

`POST /api/vms` の項目 (省略時の既定値): `name` (必須)、`backend` (cloud-hypervisor)、`image` (ubuntu-26.04)、
`cpus` (2)、`memoryMiB` (4096)、`diskGiB` (20)、`sshAuthorizedKeys` (なし。`/var/lib/niwakube/ssh/id_ed25519.pub` は常に入る)。

### 仕組み

- VM ごとに `cloud-hypervisor --api-socket` を systemd の transient unit `niwakube-vm-<name>` として起動し、
  API socket (`/run/niwakube/<name>.sock`) に `vm.create` → `vm.boot` を送る
- niwakubed を止めたり再起動したりしても VM は動き続ける。ホストの再起動後は、niwakubed が起動時に
  「起動しているはずの VM」を起動し直す
- 状態は `/var/lib/niwakube/niwakube.db` (SQLite)、VM のファイルは `/var/lib/niwakube/vms/<name>/` にある
- 動きを見るには `journalctl -u niwakubed -f`、VM のプロセスは `systemctl status niwakube-vm-<name>`

## フェーズ2: クラスタの払い出し

**k3s**、**kubeadm**、**Talos** に対応 (コントロールプレーン 1 台 + ワーカー N 台)。

```bash
curl -s -X POST localhost:8080/api/clusters -d '{"name":"demo","distro":"k3s","workers":2}'
```

```bash
curl -s localhost:8080/api/clusters/demo | jq .status
```

`status.phase` が `ready` になったら kubeconfig を取得して使う (ホストから。LAN からの接続は今後対応):

```bash
curl -s localhost:8080/api/clusters/demo/kubeconfig > demo.kubeconfig
```

```bash
kubectl --kubeconfig demo.kubeconfig get nodes
```

```bash
curl -s -X DELETE localhost:8080/api/clusters/demo
```

| ディストリ | ノードの OS | 構築のしかた |
|---|---|---|
| k3s | Ubuntu 26.04 | `get.k3s.io` で server / agent をインストール |
| kubeadm | Ubuntu 26.04 | containerd と `pkgs.k8s.io` の kubeadm を入れて `kubeadm init` / `join` |
| talos | Talos Linux (nocloud イメージ) | niwakubed が machine config を生成して渡し、Talos API で etcd を bootstrap する |

Talos はシェルも SSH も無く、`talosctl` で操作する。talosconfig は `GET /api/clusters/{name}/talosconfig` で取得できる。
コンテナランタイムは runc のみ。イメージは `host-setup.sh` が Image Factory から取得する
(`TALOS_VERSIONS` で変更できる。niwakubed に組み込んだ Talos のバージョンと合わせること)。

```bash
curl -s -X POST localhost:8080/api/clusters -d '{"name":"tal","distro":"talos","workers":2}'
```

```bash
curl -s localhost:8080/api/clusters/tal/talosconfig > tal.talosconfig
```

```bash
talosctl --talosconfig tal.talosconfig dashboard
```

**CNI** は既定で **Cilium** (kube-proxy を eBPF で置き換え、Hubble の relay と UI も入れる)。`"cni":"flannel"` も選べる。

**コンテナランタイム** は既定で **runc**。crun と youki を追加で入れて既定にしたり、Pod ごとに `runtimeClassName` で選んだりできる
(入れたランタイムと同じ名前の RuntimeClass を作る)。

```bash
curl -s -X POST localhost:8080/api/clusters -d '{"name":"demo","runtime":{"default":"crun","install":["youki"]}}'
```

クラスタを作るときに省略した場合の既定値は、niwakubed の起動オプションで変えられる
(`deploy/niwakubed.service` の `ExecStart` に足す)。今の既定値は `GET /api/cluster-defaults` で確認できる。

| オプション | 既定値 | 説明 |
|---|---|---|
| `--default-cni` | `cilium` | `cilium` / `flannel` |
| `--default-runtime` | `runc` | `runc` / `crun` / `youki` |
| `--default-runtimes` | (なし) | 既定で追加するランタイム (カンマ区切り。例: `crun,youki`) |

```bash
niwakubed --listen 127.0.0.1:8080 --default-runtime crun --default-runtimes youki
```

Hubble UI はクラスタの中にあるので、ホストで port-forward して見る (LAN の PC からは SSH のポート転送を使う):

```bash
kubectl --kubeconfig demo.kubeconfig -n kube-system port-forward svc/hubble-ui 12000:80
```

| メソッド | パス | 説明 |
|---|---|---|
| GET | `/api/distros` | 使えるディストリビューションの一覧 |
| GET | `/api/cluster-defaults` | 省略したときの CNI とランタイム |
| GET | `/api/clusters` | クラスタ一覧 |
| POST | `/api/clusters` | クラスタを作成する (ノードを作って起動し、構築は VM の中で非同期に進む) |
| GET | `/api/clusters/{name}` | 詳細 (`status.phase`: provisioning / ready / degraded / stopping / stopped) |
| DELETE | `/api/clusters/{name}` | ノードをすべて削除する |
| POST | `/api/clusters/{name}/start` | すべてのノードを起動 (CP から) |
| POST | `/api/clusters/{name}/stop` | すべてのノードを正常停止 (ワーカーから) |
| GET | `/api/clusters/{name}/kubeconfig` | admin の kubeconfig |
| GET | `/api/clusters/{name}/talosconfig` | Talos の talosconfig (Talos のみ) |
| GET / POST / DELETE | `/api/clusters/{name}/forwards` | ホストのポートからクラスタへの転送 (下記) |

`POST /api/clusters` の項目 (省略時の既定値): `name` (必須、20 文字以内)、`distro` (k3s)、
`version` (既定は `stable`。k3s は `latest` や `v1.37.2+k3s1`、kubeadm は `v1.37` (その minor の最新) や `v1.37.2` も可)、`controlPlanes` (1。いまは 1 のみ)、`workers` (2)、
`controlPlane` / `worker` (`{"cpus":2,"memoryMiB":4096,"diskGiB":30}`)、`sshAuthorizedKeys`、
`cni` (`cilium` / `flannel`)、`hubble` (Cilium なら `true`)、`runtime.default` (`runc` / `crun` / `youki`)、
`runtime.install` (追加で入れるランタイム。既定にしたものは自動で入る)。

### SSH でつないでいる PC から kubectl を使う

クラスタはミニPC の中の NAT (`172.30.0.0/16`) にあるので、PC からは SSH の SOCKS プロキシを通して使う。
Web UI のクラスタの画面の「SSH 経由用の kubeconfig」(API では `GET /api/clusters/{name}/kubeconfig?proxy=socks5://localhost:1080`)
は、`server` はそのままで `proxy-url: socks5://localhost:1080` を入れた kubeconfig を返す。

```bash
ssh -N -D 1080 -L 18080:localhost:8080 niwadmin@192.168.0.50
```

```bash
kubectl --kubeconfig k3s-1-ssh.kubeconfig get nodes
```

クラスタがいくつあっても SSH は 1 本でよい (どのクラスタも同じ SOCKS プロキシで届く)。
ダッシュボードでクラスタを選んで「選んだクラスタの kubeconfig (SSH 経由用)」を押すと、選んだクラスタを
1 つの kubeconfig にまとめてダウンロードできる (API では `GET /api/kubeconfig?cluster=a&cluster=b&proxy=socks5://localhost:1080`)。
クラスタごとに context が入るので、`kubectl config use-context <名前>` で切り替える。

Talos のクラスタは「SSH 経由用の talosconfig」(`GET /api/clusters/{name}/talosconfig?proxy=socks5://localhost:1080`) も使える。
各 context に `proxy-url` が入り、talosctl も同じ SOCKS プロキシを通して Talos API につなぐ
(talosctl は talosconfig の `proxy-url` に対応したバージョン (v1.14 以降) を使う)。

```bash
talosctl --talosconfig tal-ssh.talosconfig version
```

PC の `~/.ssh/config` に書いておくと `ssh -N niwak8s` だけでつながる:

```
Host niwak8s
    HostName 192.168.0.50
    User niwadmin
    DynamicForward 1080
    LocalForward 18080 localhost:8080
```

### LoadBalancer と LAN からの接続

Cilium のクラスタでは、`type: LoadBalancer` の Service に `172.30.N.100〜199` の IP が割り当てられる
(Cilium の LB IPAM と L2 アナウンス。niwakubed がクラスタの準備ができたあとに IP プールとポリシーを入れる。
範囲は `GET /api/clusters/{name}` の `loadBalancerPool`、適用状況は `addons` で確認できる)。
ミニPC (ホスト) からはその IP にそのまま届く。

自宅の LAN の PC からは NAT の内側にあるので、ホストのポートを転送する:

```bash
curl -s -X POST localhost:8080/api/clusters/demo/forwards -d '{"hostPort":8443,"ip":"172.30.1.100","port":443}'
```

これで `http(s)://<ミニPC の IP>:8443` が `172.30.1.100:443` に届く (nftables の専用テーブル `niwakube_fwd` の DNAT)。

| メソッド | パス | 説明 |
|---|---|---|
| GET | `/api/clusters/{name}/forwards` | 転送の一覧 |
| POST | `/api/clusters/{name}/forwards` | 追加 (`proto`: tcp / udp、`hostPort`、`ip` はクラスタのサブネット内、`port`) |
| DELETE | `/api/clusters/{name}/forwards/{proto}/{port}` | 削除 |

転送はクラスタを消すと一緒に消え、niwakubed の起動時に設定し直される。ホストに Docker が入っていると
FORWARD が DROP になるので、`DOCKER-USER` チェーンで `nkbr0` 向けの転送を許可すること。

### PV (永続ボリューム)

| StorageClass | 保存先 | 特徴 |
|---|---|---|
| `local-path` (既定) | ノードのディスク | k3s は組み込み、kubeadm / Talos は niwakubed が local-path-provisioner を入れる。ノードを消すとデータも消える |
| `niwakube-nfs` | ホストの `/var/lib/niwakube/nfs/<クラスタ名>` | ホストの NFS サーバー + csi-driver-nfs。ノードを作り直してもデータが残り、`ReadWriteMany` も使える |

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  storageClassName: niwakube-nfs
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
```

NFS サーバーは `host-setup.sh` が用意する (`/var/lib/niwakube/nfs` を `172.30.0.0/16` に公開)。PV ごとに
`<namespace>-<PVC 名>-<PV 名>` のサブディレクトリができるので、ホストから中身を確認・バックアップできる。
**クラスタを削除すると、そのクラスタの NFS のディレクトリも削除される。**

### 仕組み

- クラスタ N には `172.30.N.0/24` を割り当てる。CP は `.11`、ワーカーは `.21` から順に使う (`.10` は将来の VIP 用)
- CA などの秘密情報は niwakubed が作り、cloud-init で CP に渡す (k3s は「カスタム CA」、kubeadm は `/etc/kubernetes/pki` に事前に置く)。
  そのため niwakubed が admin の kubeconfig を自分で発行でき、VM に SSH して取りに行く必要がない
- `status` は取得のたびに API サーバー (`/api/v1/nodes`) に問い合わせて、Ready なノードを数える
- クラスタに属する VM は `/api/vms` に `cluster` / `role` 付きで表示される。単体では削除できない

## CI

| ワークフロー | 内容 |
|---|---|
| `CI` (`.github/workflows/ci.yml`) | `go vet` / `gofmt` / `go test -race` / shellcheck、linux/amd64・arm64 のバイナリをビルドして Artifacts に保存。`v*` のタグを push すると Release を作ってバイナリと `SHA256SUMS` を添付する |
| `E2E (KVM)` (`.github/workflows/e2e.yml`) | ランナーの `/dev/kvm` を使い (nested virtualization)、実際に VM を作って試す。`VM` ジョブは `test-vm.sh` と `scripts/e2e.sh`、`Cluster` ジョブは `scripts/e2e-cluster.sh` で k3s クラスタを作り、Deployment・Service・クラスタ内 DNS まで確かめる |

E2E は実機でも同じスクリプトで試せる (`host-setup.sh` を済ませておくこと):

```bash
make e2e
```

```bash
make e2e-cluster DISTRO=k3s
```
