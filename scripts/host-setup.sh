#!/usr/bin/env bash
# niwakube ホストセットアップ (Ubuntu Server 26.04 LTS 想定。24.04 でも動く)
#
#   sudo ./scripts/host-setup.sh
#
# 何度実行しても同じ結果になるように書いている (冪等)。
set -euo pipefail

NIWAKUBE_DIR=${NIWAKUBE_DIR:-/var/lib/niwakube}
BRIDGE=${BRIDGE:-nkbr0}
BRIDGE_ADDR=${BRIDGE_ADDR:-172.30.0.1/16}
VM_SUBNET=${VM_SUBNET:-172.30.0.0/16}
# VM 用に取得する Ubuntu クラウドイメージのバージョン (スペース区切りで複数可。例: "26.04 24.04")
UBUNTU_IMAGES=${UBUNTU_IMAGES:-26.04}

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
    qemu-utils cloud-image-utils nftables curl jq xz-utils iproute2 openssh-client netcat-openbsd
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

check_os
check_kvm
install_packages
install_cloud_hypervisor
setup_dirs
setup_bridge
setup_nat
fetch_ubuntu_images

log "完了。次は: sudo ./scripts/test-vm.sh up test"
