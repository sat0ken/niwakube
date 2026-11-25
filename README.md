# niwakube

[![CI](https://github.com/sat0ken/niwakube/actions/workflows/ci.yml/badge.svg)](https://github.com/sat0ken/niwakube/actions/workflows/ci.yml)
[![E2E (KVM)](https://github.com/sat0ken/niwakube/actions/workflows/e2e.yml/badge.svg)](https://github.com/sat0ken/niwakube/actions/workflows/e2e.yml)

niwa = 日本語の「庭」。自宅の庭で Kubernetes クラスタを育てる。

Cloud Hypervisor などの仮想化／コンテナ基盤の上に Kubernetes クラスタ (kubeadm / k3s / Talos) を払い出す自宅ラボ用の仕組み。
設計は [docs/design.md](docs/design.md)、バックエンド差し替えの検討は [docs/backends.md](docs/backends.md) を参照。

## フェーズ0: ホストのセットアップ

1. Ubuntu Server 26.04 LTS をインストールする。BIOS の設定や `/var/lib/niwakube` を XFS にする方法も含めて、
   手順は [docs/install.md](docs/install.md) を参照
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

ビルドには Go 1.26 以上が必要。`CGO_ENABLED=0` の静的バイナリなので、別のマシンでビルドしてコピーしてもよい。

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

### API

| メソッド | パス | 説明 |
|---|---|---|
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

いまは **k3s** に対応 (コントロールプレーン 1 台 + ワーカー N 台)。kubeadm と Talos はこれから。

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

| メソッド | パス | 説明 |
|---|---|---|
| GET | `/api/distros` | 使えるディストリビューションの一覧 |
| GET | `/api/clusters` | クラスタ一覧 |
| POST | `/api/clusters` | クラスタを作成する (ノードを作って起動し、構築は VM の中で非同期に進む) |
| GET | `/api/clusters/{name}` | 詳細 (`status.phase`: provisioning / ready / degraded / stopping / stopped) |
| DELETE | `/api/clusters/{name}` | ノードをすべて削除する |
| POST | `/api/clusters/{name}/start` | すべてのノードを起動 (CP から) |
| POST | `/api/clusters/{name}/stop` | すべてのノードを正常停止 (ワーカーから) |
| GET | `/api/clusters/{name}/kubeconfig` | admin の kubeconfig |

`POST /api/clusters` の項目 (省略時の既定値): `name` (必須、20 文字以内)、`distro` (k3s)、
`version` (k3s なら `stable`。`latest` や `v1.34.2+k3s1` も可)、`controlPlanes` (1。いまは 1 のみ)、`workers` (2)、
`controlPlane` / `worker` (`{"cpus":2,"memoryMiB":4096,"diskGiB":20}`)、`sshAuthorizedKeys`。

### 仕組み

- クラスタ N には `172.30.N.0/24` を割り当てる。CP は `.11`、ワーカーは `.21` から順に使う (`.10` は将来の VIP 用)
- CA などの秘密情報は niwakubed が作り、cloud-init で CP に渡す (k3s の「カスタム CA」の仕組み)。
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
