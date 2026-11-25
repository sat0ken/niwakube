# ホストのインストール手順 (Ubuntu Server 26.04 LTS)

ASRock DeskMini 310 (RAM 64GB) に Ubuntu Server 26.04 LTS を入れて、niwakube を動かせる状態にするまでの手順。
他のマシンでも流れは同じ。

> 画面を操作せずに全自動でインストールしたい場合は、[installer.md](installer.md) の ISO を使う。

全体の流れ:

1. [BIOS の設定](#1-bios-の設定)
2. [インストール用 USB を作る](#2-インストール用-usb-を作る)
3. [Ubuntu Server のインストール](#3-ubuntu-server-のインストール)
4. [インストール後の設定](#4-インストール後の設定)
5. [VM 用の領域 (/var/lib/niwakube) を XFS で作る](#5-vm-用の領域-varlibniwakube-を-xfs-で作る)
6. [niwakube のセットアップ](#6-niwakube-のセットアップ)

## 0. 用意するもの

- DeskMini 310 本体、モニター (HDMI / DisplayPort / D-Sub)、USB キーボード、有線 LAN
- 8GB 以上の USB メモリ
- 作業用の PC (USB メモリの作成と、インストール後の SSH 接続に使う)
- GitHub アカウントに登録済みの SSH 公開鍵 (インストーラーが取り込める。無くてもよい)

## 1. BIOS の設定

電源を入れて ASRock のロゴが出ている間に `F2` (または `Del`) を押して BIOS に入る。
項目名は BIOS のバージョンによって多少異なる。

| 項目 | 場所 (目安) | 設定 | 理由 |
|---|---|---|---|
| Intel Virtualization Technology (VT-x) | Advanced → CPU Configuration | **Enabled** | KVM に必須 |
| VT-d | Advanced → Chipset Configuration | Enabled | 必須ではない (将来の PCI パススルー用) |
| Restore on AC/Power Loss | Advanced → Chipset Configuration | Power On | 停電から復旧したら自動で起動させる |
| Secure Boot | Security | どちらでもよい | Ubuntu は Secure Boot のままでも起動する |

`F10` で保存して再起動する。

> 補足: DeskMini 310 の公式の最大メモリは 32GB。64GB でも動く例は多いが、気になるなら
> MemTest86 などで一度メモリテストをしておくと安心。

## 2. インストール用 USB を作る

1. https://ubuntu.com/download/server から **Ubuntu Server 26.04 LTS** の ISO をダウンロードする
2. USB メモリに書き込む
   - Windows / macOS: [balenaEtcher](https://etcher.balena.io/) か Rufus (Windows)
   - Linux: 次のコマンド (`/dev/sdX` は USB メモリ。間違えると別のディスクを消すので `lsblk` でよく確認する)

```bash
sudo dd if=ubuntu-26.04-live-server-amd64.iso of=/dev/sdX bs=4M status=progress oflag=sync
```

## 3. Ubuntu Server のインストール

USB を挿して電源を入れ、`F11` で起動デバイスの選択画面を出して USB を選ぶ。
GRUB のメニューで「Try or Install Ubuntu Server」を選ぶ。

インストーラーの各画面での選択:

| 画面 | 選択 |
|---|---|
| Language | **English** (サーバーのコンソールは日本語を表示できず文字化けするため) |
| Keyboard | Layout: **Japanese** (日本語キーボードの場合) |
| Type of installation | **Ubuntu Server** (minimized ではない方) |
| Network | 有線 LAN が DHCP でアドレスを取れていれば、そのまま Done。固定 IP にしたい場合は後述 |
| Proxy | 空欄 |
| Ubuntu archive mirror | 自動で選ばれたもの (日本なら `jp.archive.ubuntu.com`) |
| Storage | **Use an entire disk** + **Set up this disk as an LVM group** (下の注意を参照) |
| Profile | Your name / Server name (例: `niwa`) / Username / Password |
| Ubuntu Pro | Skip for now |
| SSH | **Install OpenSSH server** にチェック。GitHub に鍵を登録しているなら Import SSH key → from GitHub でユーザー名を入れる |
| Featured server snaps | 何も選ばない |

### ストレージについての注意

LVM を選ぶと、インストーラーはルート (`ubuntu-lv`) にディスクの一部しか割り当てないことがある。
**残りの領域はあとで VM 用 (`/var/lib/niwakube`) に XFS で割り当てる** ので、それで問題ない。

確認画面 (Storage configuration) で `ubuntu-lv` のサイズを見て、次のどちらかにしておく:

- ルートは **50〜100GB** 程度で十分。それより大きく取られていたら、`ubuntu-lv` を選んで Edit からサイズを減らす
- ディスクを 2 台積んでいる場合 (M.2 NVMe + 2.5 インチ SATA)、OS は片方に入れ、もう片方を丸ごと VM 用にしてもよい
  (その場合は手順 5 で、もう片方のディスクに XFS を作る)

インストールが終わったら「Reboot Now」を選び、USB を抜く。

## 4. インストール後の設定

ここからは作業用の PC から SSH でつないで作業するとよい。

```bash
ssh <ユーザー名>@<サーバーの IP>
```

### 4.1 パッケージの更新

```bash
sudo apt update && sudo apt full-upgrade -y
```

```bash
sudo reboot
```

### 4.2 タイムゾーン

```bash
sudo timedatectl set-timezone Asia/Tokyo
```

### 4.3 CPU と KVM の確認

```bash
lscpu | grep -E 'Model name|^CPU\(s\)'
```

```bash
ls -l /dev/kvm
```

`/dev/kvm` が無い場合は BIOS で VT-x が有効になっていない。

### 4.4 IP アドレスの固定 (おすすめ)

サーバーの IP が変わると不便なので固定する。**ルーター側の DHCP 予約 (MAC アドレスに IP を紐付ける) が一番簡単**。
サーバー側で固定する場合は netplan の設定を書き換える。まずインターフェース名を確認する (DeskMini 310 のオンボード NIC は `eno1` などになる)。

```bash
ip -br addr
```

`/etc/netplan/50-cloud-init.yaml` を次のように書き換える (アドレスは自宅のネットワークに合わせる):

```yaml
network:
  version: 2
  ethernets:
    eno1:
      dhcp4: false
      addresses: [192.168.1.50/24]
      routes:
        - to: default
          via: 192.168.1.1
      nameservers:
        addresses: [192.168.1.1]
```

`netplan try` は、設定を適用して 120 秒以内に Enter を押さなければ元に戻してくれる (SSH が切れても安全)。

```bash
sudo netplan try
```

### 4.5 自動更新について

Ubuntu Server はセキュリティ更新を自動で入れる (unattended-upgrades)。カーネルの更新は再起動するまで反映されない。
ホストを再起動すると VM も止まるが、niwakubed が起動時に「動いているはずの VM」を起動し直す。

## 5. VM 用の領域 (/var/lib/niwakube) を XFS で作る

VM のディスクは `/var/lib/niwakube` に置く。ここを XFS にすると、ベースイメージの複製が reflink で一瞬で終わり、容量もほとんど使わない。
ext4 のままでも動くが、VM を作るたびにイメージを丸ごとコピーすることになる。

### LVM の空き領域を使う場合 (ディスク 1 台)

空き容量 (`VFree`) を確認する:

```bash
sudo vgs
```

空き領域をすべて使って論理ボリュームを作り、XFS でフォーマットする:

```bash
sudo lvcreate -n niwakube -l 100%FREE ubuntu-vg
```

```bash
sudo mkfs.xfs /dev/ubuntu-vg/niwakube
```

`/etc/fstab` に追記してマウントする:

```bash
sudo mkdir -p /var/lib/niwakube
```

```bash
echo '/dev/ubuntu-vg/niwakube /var/lib/niwakube xfs defaults 0 2' | sudo tee -a /etc/fstab
```

```bash
sudo systemctl daemon-reload && sudo mount /var/lib/niwakube
```

```bash
df -hT /var/lib/niwakube
```

`Type` が `xfs` になっていれば OK。`mkfs.xfs` が無いと言われたら `sudo apt install -y xfsprogs` を実行する。

### 2 台目のディスクを丸ごと使う場合

`lsblk` で 2 台目のディスク名 (例: `/dev/sda`) を確認し、**中身が消えてよいことを確かめてから** 次を実行する:

```bash
sudo mkfs.xfs -f /dev/sda
```

```bash
echo "UUID=$(sudo blkid -s UUID -o value /dev/sda) /var/lib/niwakube xfs defaults 0 2" | sudo tee -a /etc/fstab
```

あとは上と同じく `mkdir`、`systemctl daemon-reload`、`mount` を実行する。

## 6. niwakube のセットアップ

### 6.1 リポジトリの取得

```bash
sudo apt install -y git make
```

```bash
git clone https://github.com/sat0ken/niwakube.git && cd niwakube
```

### 6.2 ホストのセットアップ

Cloud Hypervisor、UEFI ファームウェア、ブリッジ `nkbr0`、NAT、Ubuntu 26.04 のクラウドイメージを用意する。

```bash
sudo ./scripts/host-setup.sh
```

24.04 のイメージも欲しい場合は `UBUNTU_IMAGES="26.04 24.04"` を付けて実行する。

### 6.3 VM を 1 台手で起動して確認する

```bash
sudo ./scripts/test-vm.sh up test
```

「OK!」で終われば、Cloud Hypervisor・ファームウェア・cloud-init・ブリッジ・NAT がすべて動いている。確認したら消しておく。

```bash
sudo ./scripts/test-vm.sh down test
```

### 6.4 niwakubed のインストール

ビルドには Go 1.26.5 以上が必要 (Talos の machinery ライブラリの要件)。snap で入れるのが簡単:

```bash
sudo snap install go --classic
```

```bash
make build && sudo make install
```

```bash
sudo systemctl enable --now niwakubed
```

```bash
curl -s localhost:8080/healthz
```

### 6.5 Web UI を開く

niwakubed は認証が無いので `127.0.0.1:8080` だけで待ち受けている。普段使う PC から SSH のポート転送でつなぐ
(`niwa` はミニPC のホスト名か IP アドレス):

```bash
ssh -L 8080:localhost:8080 niwa
```

PC のブラウザで `http://localhost:8080/` を開く。

使い方は [README](../README.md) を参照。

## トラブルシューティング

| 症状 | 確認すること |
|---|---|
| `/dev/kvm` が無い | BIOS の VT-x。`grep -c vmx /proc/cpuinfo` が 0 なら無効になっている |
| VM がネットワークにつながらない | `ip -br addr show nkbr0` にアドレスがあるか、`sudo nft list table ip niwakube` にルールがあるか、`sysctl net.ipv4.ip_forward` が 1 か |
| VM が起動しない | `/var/lib/niwakube/vms/<name>/serial.log`、`journalctl -u niwakube-vm-<name>` |
| Docker を入れたら VM が外に出られなくなった | Docker が FORWARD を DROP にしている。`DOCKER-USER` チェーンで `nkbr0` を許可する |
