# 自動インストール用 ISO (installer)

`installer/config.yaml` を書いて PR を出すと、GitHub Actions がホストを自動インストールする ISO を作る。
ISO は USB メモリに書き込んで使う。RHEL の Kickstart のように、インストーラーの画面操作は不要。

手動でインストールする場合の手順は [install.md](install.md) を参照。

## 流れ

```
installer/config.yaml を編集して PR を出す
        │
        ▼
GitHub Actions (Installer ISO)
  1. Ubuntu mini ISO を取得し、SHA256SUMS の GPG 署名を検証する
  2. その時点で最新の Ubuntu Server ISO の URL・サイズ・SHA256 を調べる (同じく署名を検証)
  3. config.yaml から autoinstall の設定を作り、mini ISO に組み込む
  4. QEMU (UEFI) の VM で実際に無人インストールし、再起動後に SSH・XFS・niwakube の状態を確かめる
  5. Artifacts の niwakube-installer に ISO を置く (14 日間)
        │
        ▼
ISO をダウンロード → USB に書き込む → DeskMini に挿して起動
```

## config.yaml

```yaml
hostname: niwa
timezone: Asia/Tokyo
keyboard: jp

user:
  name: sat0ken
  github_keys: sat0ken          # GitHub に登録した公開鍵を取り込む
  # ssh_authorized_keys:        # 鍵を直接書くこともできる
  #   - ssh-ed25519 AAAA...

# network を書かなければ DHCP
# network:
#   address: 192.168.1.50/24
#   gateway: 192.168.1.1
#   dns: [192.168.1.1]

storage:
  disk: largest                 # internal / {model: "..."} / {serial: "..."} でも指定できる
  root_size: 100G               # 残りはすべて /var/lib/niwakube (XFS)

install:
  mode: confirm                 # confirm / unattended (GRUB の既定の項目)
  after: poweroff               # poweroff / reboot

niwakube:
  setup: true                   # 初回起動時に host-setup.sh を実行する
  ref: main                     # v* のタグにすると niwakubed もリリースから入れる
```

| 項目 | 既定値 | 説明 |
|---|---|---|
| `hostname` | (必須) | ホスト名 |
| `timezone` / `keyboard` / `locale` | `Etc/UTC` / `us` / `en_US.UTF-8` | |
| `ubuntu.release` | `26.04` | Ubuntu のリリース。インストール時はその最新のポイントリリースを使う |
| `user.name` | (必須) | ログインするユーザー。sudo はパスワードなしで使える |
| `user.github_keys` / `user.ssh_authorized_keys` | どちらか必須 | **ログインは SSH 鍵のみ。パスワードはロックする** |
| `network` | DHCP | 固定 IP にする場合に書く |
| `storage.disk` | `largest` | インストール先。`largest` / `internal` / `{model: ...}` / `{serial: ...}` (下記) |
| `storage.root_size` | `100G` | ルートの LV のサイズ。残りは `/var/lib/niwakube` (XFS) |
| `install.mode` | `confirm` | GRUB の既定の項目 (下記) |
| `install.after` | `poweroff` | インストール後に電源を切るか再起動するか |
| `install.timeout` | `10` | GRUB のメニューの待ち時間 (秒) |
| `install.kernel_args` | なし | インストーラーのカーネルに足す引数 |
| `install.iso_mirror` | なし (`releases.ubuntu.com`) | Server ISO を取るミラー (例: `https://ftp.jaist.ac.jp/pub/Linux/ubuntu-releases`)。下記 |
| `niwakube.setup` / `repo` / `ref` | `true` / `sat0ken/niwakube` / `main` | 初回起動時のセットアップ |
| `autoinstall` | なし | 生成した autoinstall の設定に深くマージして上書きする (上級者向け) |

設定の確認と、生成される autoinstall の中身は手元でも見られる:

```bash
go run ./cmd/niwakube-iso check -config installer/config.yaml
```

```bash
go run ./cmd/niwakube-iso render -config installer/config.yaml
```

## 確認ありのインストールと無人インストール

GRUB のメニューには次の項目が出る。

| 項目 | 動き |
|---|---|
| install Ubuntu Server ... (asks before erasing the disk) | インストーラーが「Continue with autoinstall?」と聞くので `yes` と入力すると始まる |
| UNATTENDED install - ERASES ... | 確認なしで始まる。**`storage.disk` が `internal` かモデル名 / シリアル番号のときだけ出る** |
| Choose an Ubuntu version to install | 元の mini ISO のメニュー |

`install.mode: unattended` にすると無人の項目が既定になり、USB を挿して電源を入れるだけでインストールが終わる。
`unattended` では `storage.disk` を `largest` 以外にする (外付けの USB ディスクを消さないため)。

| `storage.disk` | 選ばれるディスク |
|---|---|
| `largest` | 一番大きいディスク (USB のディスクも含む)。確認ありのインストール専用 |
| `internal` | 内蔵の SATA / NVMe のうち一番大きいディスク。USB メモリや外付けの USB ディスクは選ばない。ディスクを交換しても設定を変えなくてよい |
| `{model: ...}` / `{serial: ...}` | そのモデル名 / シリアル番号のディスク。そのディスクが無い PC では何も消さずに止まる |

`internal` は udev の `ID_PATH` が `…-ata-…` (SATA) か `…-nvme-…` (NVMe) のディスクから、一番大きいものを選ぶ。
**`unattended` + `internal` の USB は、どの PC で起動しても、その PC の内蔵ディスクを確認なしで消す**ので、
インストールが終わったら USB を抜いておくこと。

ディスクのモデル名とシリアル番号は、Ubuntu のライブ環境などで確認できる:

```bash
lsblk -d -o NAME,MODEL,SERIAL,SIZE
```

`lsblk` に表示されたままの値を書けばよい。製品名とは違うことがあるので注意 (SATA の SSD はモデル名が 16 文字で切れる。
例: Samsung SSD 860 EVO 1TB は `Samsung SSD 860`)。インストーラーが照合しているのは udev の `ID_MODEL` / `ID_SERIAL` で、
`udevadm info -q property -n /dev/sda | grep -E '^ID_(MODEL|SERIAL)='` で確かめられる。udev の値はスペースが `_` になったり、NVMe ではシリアルの前に
モデル名が付いたりするので、niwakube は前後を `*`、スペースを `?` にしたパターンで照合する
(例: `Samsung SSD 980 1TB` → `*Samsung?SSD?980?1TB*`)。

## USB への書き込みとインストール

1. PR の Checks → 「Installer ISO」→ Summary の Artifacts から `niwakube-installer` をダウンロードして展開する
2. `niwakube-installer.iso` を USB メモリに書き込む ([install.md](install.md#2-インストール用-usb-を作る) と同じ方法)
3. DeskMini に USB と**有線 LAN** を挿して電源を入れ、`F11` で USB から起動する。
   **`UEFI:` で始まり `Partition 2` の項目を選ぶ** (例: `UEFI: TOSHIBA TransMemory 1.00, Partition 2`)。
   `Partition 3` は自動インストールの設定 (CIDATA) が入った FAT のパーティションで、起動はできない。
   UEFI 用のパーティション構成なので、レガシー BIOS (CSM) で起動すると (`UEFI:` の付かない項目)
   ダウンロードの前に `booted the USB in legacy BIOS (CSM) mode` と出て止まる。BIOS の設定で CSM を無効にしてもよい
4. インストールが終わると電源が切れる (`install.after: poweroff`)。USB を抜いてから電源を入れる
5. 初回起動で niwakube のセットアップ (`host-setup.sh`) が走る。終わったら SSH でログインできる

```bash
ssh sat0ken@<サーバーの IP>
```

```bash
sudo tail -f /var/log/niwakube-firstboot.log
```

## 仕組み

Ubuntu の mini ISO は、起動すると選んだ ISO をネットワークからメモリにダウンロードし、kexec でその ISO を起動する。
niwakube はこれを次のように使う。

- **ISO を選ぶメニューを飛ばす**: GRUB で `niwakube-iso-url=` などを渡し、ビルド時に決めた Server ISO をそのまま使う
- **`autoinstall` を最後のカーネルに渡す**: 元の mini ISO は最後のカーネルの引数を固定しているので、
  initrd の末尾に cpio を足して `scripts/casper-premount/30mini-iso-menu` を差し替え、
  `nk.<arg>` を `<arg>` として渡せるようにした (`internal/installer/assets/30mini-iso-menu`)
- **ダウンロードが止まっても取り直す**: 元の mini ISO は wget を 1 回実行するだけなので、途中で LAN のリンクが切れると
  止まったままになる。niwakube は 256MiB ごとに取り、30 秒増えなければ wget を止めて続きから取り直す
  (busybox の wget にはタイムアウトが無いので、`wget -c` と疎なファイルで `Range` を送らせている)
- **autoinstall の設定**: USB に `CIDATA` というラベルの FAT パーティションを足して `user-data` を置く。
  インストーラーの cloud-init (NoCloud) がこれを読む
- **ストレージ**: GPT + ESP 1G + `/boot` 2G + LVM。ルートの LV は `root_size`、残りは late-commands で XFS にして `/var/lib/niwakube` にマウントする

ISO のファイルは公式のまま (GRUB の設定の差し替えと initrd への追加だけ) なので、Secure Boot が有効でも起動できる。

## 注意

- **インストール時はインターネットが必要** (Server ISO の約 3GB をダウンロードする)。有線 LAN と DHCP を用意すること
- `install.iso_mirror` を書くと、Server ISO を日本のミラー (JAIST、理研、山形大学など) から取る。
  ISO を作るときにミラーに同じ大きさのファイルがあるかを確かめ、無ければ (同期の遅れなど) 警告を出して
  `releases.ubuntu.com` から取る。インストール中に止まったときは、ミラーと `releases.ubuntu.com` を交互に試す。
  どちらから取っても、最後にビルド時の SHA256 (Ubuntu の署名を検証した SHA256SUMS) で中身を確かめる
- ダウンロード中に `NIC Link is Down` が出たり、リンクが 100 Mbps になったりする場合は、LAN ケーブル
  (Cat5e 以上) やルーターのポートを替えてみる。リンクが戻れば `retrying from ... MiB` と出て続きから取り直す
- ISO にはビルド時点の最新のポイントリリース (例: 26.04.1) の URL が入る。次のポイントリリースが出ると古い ISO は
  `releases.ubuntu.com` から消えるので、そのときは PR を出し直して (または Actions を手動実行して) ISO を作り直す
- 公開リポジトリの Artifacts は GitHub にログインしている人なら誰でもダウンロードできる。
  ISO には config.yaml の内容 (ホスト名、ユーザー名、IP、SSH 公開鍵) が入るが、パスワードは入れない
- CI のテストでは、実機と違う部分 (ネットワークを DHCP、ディスクを VM のディスク、無人インストール、シリアルコンソール) だけを
  変えた ISO でインストールを試している。Artifacts に置くのは config.yaml どおりの ISO
