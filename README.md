# niwakube

Cloud Hypervisor などの仮想化／コンテナ基盤の上に Kubernetes クラスタ (kubeadm / k3s / Talos) を払い出す自宅ラボ用の仕組み。
設計は [docs/design.md](docs/design.md)、バックエンド差し替えの検討は [docs/backends.md](docs/backends.md) を参照。

## フェーズ0: ホストのセットアップ

1. BIOS で Intel VT-x を有効にし、Ubuntu Server 24.04 をインストールする
   (できれば `/var/lib/niwakube` を XFS のパーティションか LV にする)
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
