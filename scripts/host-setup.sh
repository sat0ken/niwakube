#!/usr/bin/env bash
# niwakube ホストセットアップ (Ubuntu Server 26.04 LTS 想定。24.04 でも動く)
#
#   sudo ./scripts/host-setup.sh
#   sudo BACKEND=nspawn ./scripts/host-setup.sh   # VM ではなく systemd-nspawn のコンテナでノードを作る
#
# 何度実行しても同じ結果になるように書いている (冪等)。
set -euo pipefail

NIWAKUBE_DIR=${NIWAKUBE_DIR:-/var/lib/niwakube}
# ノードを作るバックエンド: cloud-hypervisor (VM。Intel VT-x / AMD-V が要る) か nspawn (systemd-nspawn のコンテナ)
BACKEND=${BACKEND:-cloud-hypervisor}
BRIDGE=${BRIDGE:-nkbr0}
BRIDGE_ADDR=${BRIDGE_ADDR:-172.30.0.1/16}
VM_SUBNET=${VM_SUBNET:-172.30.0.0/16}
# VM 用に取得する Ubuntu クラウドイメージのバージョン (スペース区切りで複数可。例: "26.04 24.04")
UBUNTU_IMAGES=${UBUNTU_IMAGES:-26.04}
# Talos のイメージのバージョン (niwakubed に組み込んだ Talos の machinery と同じにする)。空にすると取得しない
TALOS_VERSIONS=${TALOS_VERSIONS-v1.14.7}
# Image Factory の schematic。既定は拡張機能なし
TALOS_SCHEMATIC=${TALOS_SCHEMATIC:-376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba}

# Web UI のターミナルで使う kubectl のバージョン (niwakubed の client-go に合わせる)。talosctl は TALOS_VERSIONS の最初のもの
KUBECTL_VERSION=${KUBECTL_VERSION:-v1.37.2}
# Web UI のターミナルで使う helm のバージョン。latest なら get.helm.sh の最新。空にすると入れない
HELM_VERSION=${HELM_VERSION-latest}
# CloudNative Days のハンズオン (クラスタの profile: cnd-handson) で使う helmfile・cilium・hubble を入れる。空にすると入れない
HANDSON_TOOLS=${HANDSON_TOOLS-1}

# 動作確認したバージョンに固定する (CI の E2E で検証済み)。最新を試すなら CH_VERSION=latest
CH_VERSION=${CH_VERSION:-v55.0}
if [[ $CH_VERSION == latest ]]; then
  CH_RELEASES=https://github.com/cloud-hypervisor/cloud-hypervisor/releases/latest/download
else
  CH_RELEASES=https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CH_VERSION
fi
EDK2_RELEASES=https://github.com/cloud-hypervisor/edk2/releases/latest/download

log()  { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
warn() { printf '\e[1;33m!!\e[0m %s\n' "$*" >&2; }
die()  { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "root で実行してください (sudo $0)"

check_os() {
  log "ホスト OS の確認"
  # shellcheck disable=SC1091
  . /etc/os-release
  echo "    $PRETTY_NAME"
  if [[ ${ID:-} != ubuntu ]]; then
    warn "Ubuntu 以外では試していません"
  elif [[ ${VERSION_ID:-} != 26.04 && ${VERSION_ID:-} != 24.04 ]]; then
    warn "Ubuntu 26.04 / 24.04 以外では試していません"
  fi
}

check_kvm() {
  log "KVM の確認"
  grep -Eqw 'vmx|svm' /proc/cpuinfo \
    || die "CPU の仮想化支援が見えません。BIOS で Intel VT-x を有効にしてください"
  modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
  [[ -c /dev/kvm ]] || die "/dev/kvm がありません"
  lscpu | grep -E '^(Model name|CPU\(s\)|Thread\(s\) per core)' | sed 's/^/    /'
  free -h | sed 's/^/    /'
}

install_packages() {
  log "パッケージのインストール"
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    qemu-utils cloud-image-utils nftables curl jq xz-utils iproute2 openssh-client netcat-openbsd nfs-kernel-server \
    bash-completion git openssl procps
  if [[ $BACKEND == nspawn ]]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends systemd-container
  fi
}

install_cloud_hypervisor() {
  log "Cloud Hypervisor のインストール ($CH_VERSION)"
  curl -fsSL -o /usr/local/bin/cloud-hypervisor.new "$CH_RELEASES/cloud-hypervisor-static"
  curl -fsSL -o /usr/local/bin/ch-remote.new "$CH_RELEASES/ch-remote-static"
  chmod 0755 /usr/local/bin/cloud-hypervisor.new /usr/local/bin/ch-remote.new
  mv -f /usr/local/bin/cloud-hypervisor.new /usr/local/bin/cloud-hypervisor
  mv -f /usr/local/bin/ch-remote.new /usr/local/bin/ch-remote
  echo "    $(cloud-hypervisor --version)"

  log "UEFI ファームウェア (CLOUDHV.fd) の取得"
  mkdir -p "$NIWAKUBE_DIR/firmware"
  curl -fsSL -o "$NIWAKUBE_DIR/firmware/CLOUDHV.fd" "$EDK2_RELEASES/CLOUDHV.fd"
}

# install_verified URL SHA256 DEST: ダウンロードして SHA256 を確かめてから置く
install_verified() {
  local tmp
  tmp=$(mktemp)
  curl -fsSL --retry 3 -o "$tmp" "$1"
  if ! echo "$2  $tmp" | sha256sum -c --quiet -; then
    rm -f "$tmp"
    die "$1 の SHA256 が一致しません"
  fi
  install -m 0755 "$tmp" "$3"
  rm -f "$tmp"
}

# Web UI のターミナル (と手元の操作) で使う kubectl / talosctl
# install_helm は helm を入れる (get.helm.sh の tar.gz を、同じ場所の SHA256 で確かめる)。
install_helm() {
  local arch=$1 ver=$HELM_VERSION sum tmp
  [[ -n $ver ]] || return 0
  if [[ $ver == latest ]]; then
    ver=$(curl -fsSL --retry 3 https://get.helm.sh/helm-latest-version)
  fi
  log "helm のインストール ($ver)"
  sum=$(curl -fsSL --retry 3 "https://get.helm.sh/helm-$ver-linux-$arch.tar.gz.sha256sum" | awk '{print $1}')
  [[ -n $sum ]] || die "helm $ver の SHA256 が見つかりません"
  tmp=$(mktemp -d)
  curl -fsSL --retry 3 -o "$tmp/helm.tar.gz" "https://get.helm.sh/helm-$ver-linux-$arch.tar.gz"
  if ! echo "$sum  $tmp/helm.tar.gz" | sha256sum -c --quiet -; then
    rm -rf "$tmp"
    die "helm の SHA256 が一致しません"
  fi
  tar -xzf "$tmp/helm.tar.gz" -C "$tmp" "linux-$arch/helm"
  install -m 0755 "$tmp/linux-$arch/helm" /usr/local/bin/helm
  rm -rf "$tmp"
  echo "    $(helm version --short 2>/dev/null)"
}

# latest_tag <owner/repo> は GitHub の最新リリースのタグを返す (API を使わず、latest のリダイレクト先から)。
latest_tag() {
  curl -fsSIL --retry 3 -o /dev/null -w '%{url_effective}' "https://github.com/$1/releases/latest" | sed 's#.*/tag/##'
}

# install_tarball <url> <sha256> <tar の中のファイル> <入れる先> は、tar.gz を確かめて 1 つのファイルを入れる。
install_tarball() {
  local tmp
  tmp=$(mktemp -d)
  curl -fsSL --retry 3 -o "$tmp/a.tar.gz" "$1"
  if ! echo "$2  $tmp/a.tar.gz" | sha256sum -c --quiet -; then
    rm -rf "$tmp"
    die "$1 の SHA256 が一致しません"
  fi
  tar -xzf "$tmp/a.tar.gz" -C "$tmp" "$3"
  install -m 0755 "$tmp/$3" "$4"
  rm -rf "$tmp"
}

# install_handson_tools はハンズオンで使うコマンドを入れる (ターミナルでは sudo が使えないので、先に入れておく)。
install_handson_tools() {
  local arch=$1 ver url sum
  ver=$(latest_tag helmfile/helmfile)
  [[ $ver == v* ]] || die "helmfile の最新バージョンがわかりません ($ver)"
  log "helmfile のインストール ($ver)"
  url=https://github.com/helmfile/helmfile/releases/download/$ver
  sum=$(curl -fsSL --retry 3 "$url/helmfile_${ver#v}_checksums.txt" | awk -v f="helmfile_${ver#v}_linux_$arch.tar.gz" '$2 == f {print $1}')
  [[ -n $sum ]] || die "helmfile $ver の SHA256 が見つかりません"
  install_tarball "$url/helmfile_${ver#v}_linux_$arch.tar.gz" "$sum" helmfile /usr/local/bin/helmfile

  local tool repo
  for tool in cilium hubble; do
    repo=cilium/$tool
    [[ $tool == cilium ]] && repo=cilium/cilium-cli
    log "$tool CLI のインストール ($(latest_tag "$repo"))"
    url=https://github.com/$repo/releases/latest/download/$tool-linux-$arch.tar.gz
    sum=$(curl -fsSL --retry 3 "$url.sha256sum" | awk '{print $1}')
    [[ -n $sum ]] || die "$tool の SHA256 が見つかりません"
    install_tarball "$url" "$sum" "$tool" "/usr/local/bin/$tool"
  done
}

install_clients() {
  local arch sum talos
  arch=$(dpkg --print-architecture)
  log "kubectl のインストール ($KUBECTL_VERSION)"
  sum=$(curl -fsSL --retry 3 "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$arch/kubectl.sha256")
  install_verified "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$arch/kubectl" "$sum" /usr/local/bin/kubectl
  echo "    $(kubectl version --client 2>/dev/null | head -n1)"

  install_helm "$arch"

  [[ -z $HANDSON_TOOLS ]] || install_handson_tools "$arch"

  talos=${TALOS_VERSIONS%% *}
  [[ -n $talos ]] || return 0
  log "talosctl のインストール ($talos)"
  sum=$(curl -fsSL --retry 3 "https://github.com/siderolabs/talos/releases/download/$talos/sha256sum.txt" |
    awk -v f="talosctl-linux-$arch" '$2 == f || $2 == "*"f {print $1}')
  [[ -n $sum ]] || die "talosctl-linux-$arch の SHA256 が見つかりません"
  install_verified "https://github.com/siderolabs/talos/releases/download/$talos/talosctl-linux-$arch" "$sum" /usr/local/bin/talosctl
  echo "    $(talosctl version --client --short 2>/dev/null | tail -n1)"
}

setup_dirs() {
  log "ディレクトリ作成: $NIWAKUBE_DIR"
  mkdir -p "$NIWAKUBE_DIR"/{images,firmware,vms,ssh} /etc/niwakube
  chmod 0700 "$NIWAKUBE_DIR/ssh"
  if [[ ! -f $NIWAKUBE_DIR/ssh/id_ed25519 ]]; then
    ssh-keygen -q -t ed25519 -N '' -C niwakube -f "$NIWAKUBE_DIR/ssh/id_ed25519"
  fi

  # reflink が効くファイルシステムだと VM ディスクのコピーが一瞬で終わる
  local fstype
  fstype=$(stat -f -c %T "$NIWAKUBE_DIR")
  case $fstype in
    xfs|btrfs) echo "    filesystem: $fstype (reflink OK)" ;;
    *) warn "$NIWAKUBE_DIR は $fstype です。XFS/btrfs だとディスクの複製が速く省容量になります (必須ではありません)" ;;
  esac
}

setup_bridge() {
  log "NAT 用ブリッジ $BRIDGE ($BRIDGE_ADDR)"
  # Ubuntu Server は netplan -> systemd-networkd。物理 NIC を持たないブリッジは
  # キャリアが立たないので ConfigureWithoutCarrier を付けて networkd で直接定義する。
  cat >"/etc/systemd/network/50-$BRIDGE".netdev <<EOF
[NetDev]
Name=$BRIDGE
Kind=bridge
EOF
  cat >"/etc/systemd/network/50-$BRIDGE".network <<EOF
[Match]
Name=$BRIDGE

[Link]
RequiredForOnline=no

[Network]
Address=$BRIDGE_ADDR
ConfigureWithoutCarrier=yes
EOF
  systemctl enable --now systemd-networkd >/dev/null 2>&1
  networkctl reload
  for _ in $(seq 1 20); do ip -br addr show "$BRIDGE" 2>/dev/null | grep -q "${BRIDGE_ADDR%/*}" && break; sleep 0.5; done
  ip -br addr show "$BRIDGE" | sed 's/^/    /' || die "$BRIDGE が作成できませんでした"
}

setup_nat() {
  log "IP フォワードと NAT (nftables)"
  echo 'net.ipv4.ip_forward = 1' >/etc/sysctl.d/90-niwakube.conf
  sysctl -q --system

  # 他の nftables / ufw / Docker のルールと独立させるため専用テーブルにする
  cat >/etc/niwakube/nat.nft <<EOF
table ip niwakube
delete table ip niwakube
table ip niwakube {
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr $VM_SUBNET ip daddr != $VM_SUBNET masquerade
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    iifname "$BRIDGE" accept
    oifname "$BRIDGE" ct state established,related accept
  }
}
EOF
  cat >/etc/systemd/system/niwakube-nat.service <<'EOF'
[Unit]
Description=niwakube NAT rules
After=network-pre.target nftables.service
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/niwakube/nat.nft
ExecStop=/usr/sbin/nft delete table ip niwakube

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable niwakube-nat.service >/dev/null 2>&1
  systemctl restart niwakube-nat.service

  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q 'Status: active'; then
    warn "ufw が有効です。'ufw route allow in on $BRIDGE' が必要になる場合があります"
  fi
  if command -v docker >/dev/null; then
    warn "Docker が入っていると FORWARD が DROP になり VM が外に出られないことがあります"
  fi
}

# バージョン → コードネーム
ubuntu_codename() {
  case $1 in
    24.04) echo noble ;;
    26.04) echo resolute ;;
    *) die "未対応の Ubuntu バージョンです: $1" ;;
  esac
}

fetch_ubuntu_images() {
  local ver codename raw url tmp
  for ver in $UBUNTU_IMAGES; do
    codename=$(ubuntu_codename "$ver")
    raw=$NIWAKUBE_DIR/images/ubuntu-$ver.raw
    if [[ -f $raw ]]; then
      log "Ubuntu $ver クラウドイメージは取得済み: $raw"
      continue
    fi
    url=https://cloud-images.ubuntu.com/releases/$codename/release/ubuntu-$ver-server-cloudimg-amd64.img
    log "Ubuntu $ver ($codename) クラウドイメージの取得と raw 変換"
    tmp=$(mktemp -d)
    curl -fL --progress-bar -o "$tmp/ubuntu.img" "$url"
    qemu-img convert -p -O raw "$tmp/ubuntu.img" "$raw.tmp"
    mv "$raw.tmp" "$raw"
    rm -f "$tmp/ubuntu.img"
    rmdir "$tmp"
  done
}

# fetch_ubuntu_rootfs は nspawn のノードの土台にする Ubuntu のルートファイルシステムを用意する
# (ノードごとに overlayfs で重ねるので、ここは読み取り専用の共有の層になる)
fetch_ubuntu_rootfs() {
  local ver codename arch dir base file tmp sum
  arch=$(dpkg --print-architecture)
  for ver in $UBUNTU_IMAGES; do
    codename=$(ubuntu_codename "$ver")
    dir=$NIWAKUBE_DIR/images/ubuntu-$ver.rootfs
    if [[ -d $dir/etc ]]; then
      log "Ubuntu $ver のルートファイルシステムは用意済み: $dir"
      continue
    fi
    base=https://cloud-images.ubuntu.com/releases/$codename/release
    file=ubuntu-$ver-server-cloudimg-$arch-root.tar.xz
    log "Ubuntu $ver ($codename, $arch) のルートファイルシステムの取得と展開"
    tmp=$(mktemp -d)
    curl -fL --progress-bar -o "$tmp/$file" "$base/$file"
    sum=$(curl -fsSL "$base/SHA256SUMS" | awk -v f="$file" '$2 == f || $2 == "*"f {print $1}')
    [[ -n $sum ]] || die "$file の SHA256 が見つかりません"
    echo "$sum  $tmp/$file" | sha256sum -c --quiet - || die "$file の SHA256 が一致しません"
    rm -rf "$dir.tmp"
    mkdir -p "$dir.tmp"
    tar --numeric-owner --xattrs --xattrs-include='*' -xJf "$tmp/$file" -C "$dir.tmp"
    mv "$dir.tmp" "$dir"
    rm -rf "$tmp"
  done
}

# setup_container_kernel はコンテナの中で Kubernetes を動かすためのホストのカーネルの設定をする
# (コンテナはホストのカーネルを共有するので、モジュールの読み込みと一部の sysctl はホストで行う)
setup_container_kernel() {
  log "コンテナで Kubernetes を動かすためのカーネルの設定"
  local m mods=(overlay br_netfilter vxlan nf_conntrack ip_tables iptable_nat iptable_filter)
  printf '%s\n' "${mods[@]}" >/etc/modules-load.d/niwakube.conf
  for m in "${mods[@]}"; do
    modprobe "$m" 2>/dev/null || warn "カーネルモジュール $m を読み込めません"
  done
  cat >/etc/sysctl.d/90-niwakube-containers.conf <<'EOF'
# niwakube (nspawn): コンテナの中の Kubernetes のため
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
# kubelet や containerd がコンテナの数だけ inotify を使う (ホストの root で共有)
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 524288
EOF
  sysctl --system >/dev/null
}

# write_niwakubed_defaults は niwakubed の起動オプション (バックエンド) を書く
write_niwakubed_defaults() {
  log "niwakubed の設定 (/etc/default/niwakubed): --backend=$BACKEND"
  cat >/etc/default/niwakubed <<EOF
# niwakube: scripts/host-setup.sh が書く。niwakubed の起動オプション
NIWAKUBED_OPTS="--backend=$BACKEND"
EOF
}

setup_nfs() {
  log "NFS サーバー ($NIWAKUBE_DIR/nfs を $VM_SUBNET に公開)"
  # クラスタごとに $NIWAKUBE_DIR/nfs/<クラスタ名> を niwakubed が作り、csi-driver-nfs の StorageClass (niwakube-nfs) で使う
  mkdir -p "$NIWAKUBE_DIR/nfs"
  mkdir -p /etc/exports.d
  echo "$NIWAKUBE_DIR/nfs $VM_SUBNET(rw,sync,no_subtree_check,no_root_squash)" >/etc/exports.d/niwakube.exports
  systemctl enable --now nfs-server >/dev/null 2>&1
  exportfs -ra
  exportfs -v | grep -F "$NIWAKUBE_DIR/nfs" | sed 's/^/    /'
}

fetch_talos_images() {
  local ver raw url
  for ver in $TALOS_VERSIONS; do
    raw=$NIWAKUBE_DIR/images/talos-$ver.raw
    if [[ -f $raw ]]; then
      log "Talos $ver のイメージは取得済み: $raw"
      continue
    fi
    url=https://factory.talos.dev/image/$TALOS_SCHEMATIC/$ver/nocloud-amd64.raw.xz
    log "Talos $ver の nocloud イメージを取得 ($url)"
    curl -fsSL "$url" | xz -d >"$raw.tmp"
    mv "$raw.tmp" "$raw"
  done
}

check_os
case $BACKEND in
  cloud-hypervisor)
    check_kvm
    install_packages
    install_cloud_hypervisor
    ;;
  nspawn)
    install_packages
    setup_container_kernel
    ;;
  *) die "BACKEND は cloud-hypervisor か nspawn です: $BACKEND" ;;
esac
install_clients
setup_dirs
setup_bridge
setup_nat
if [[ $BACKEND == nspawn ]]; then
  fetch_ubuntu_rootfs
else
  fetch_ubuntu_images
  fetch_talos_images
fi
setup_nfs
write_niwakubed_defaults

if [[ $BACKEND == nspawn ]]; then
  log "完了 (nspawn)。niwakubed を (再) 起動すると、コンテナでノードを作ります: sudo systemctl restart niwakubed"
else
  log "完了。次は: sudo ./scripts/test-vm.sh up test"
fi
