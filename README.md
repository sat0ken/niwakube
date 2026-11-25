# niwakube

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
