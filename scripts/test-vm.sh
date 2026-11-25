#!/usr/bin/env bash
# shellcheck disable=SC2016  # ssh_vm に渡すコマンドは VM 側で展開させるため
# フェーズ0: Cloud Hypervisor で VM を 1 台手で起動して、
# ファームウェア / cloud-init / ブリッジ / NAT が動くことを確認するスクリプト。
# 後で Go のサーバーがやることを、そのまま shell で書いたもの。
#
#   sudo ./scripts/test-vm.sh up   test            # 起動して SSH 疎通と外向き通信を確認
#   sudo ./scripts/test-vm.sh ssh  test            # VM に SSH
#   sudo ./scripts/test-vm.sh info test            # CH API (vm.info) を叩く
#   sudo ./scripts/test-vm.sh down test            # 停止して後片付け
#
# 環境変数: IP (既定 172.30.0.10), CPUS (2), MEM (4G), DISK_SIZE (20G), BOOT_TIMEOUT (180),
#           BASE_IMAGE (既定 /var/lib/niwakube/images/ubuntu-26.04.raw)
set -euo pipefail

NIWAKUBE_DIR=${NIWAKUBE_DIR:-/var/lib/niwakube}
BRIDGE=${BRIDGE:-nkbr0}
GATEWAY=${GATEWAY:-172.30.0.1}
PREFIX=${PREFIX:-16}
IP=${IP:-172.30.0.10}
CPUS=${CPUS:-2}
MEM=${MEM:-4G}
BOOT_TIMEOUT=${BOOT_TIMEOUT:-180} # SSH が通るまで待つ秒数
DISK_SIZE=${DISK_SIZE:-20G}
BASE_IMAGE=${BASE_IMAGE:-$NIWAKUBE_DIR/images/ubuntu-26.04.raw}
FIRMWARE=$NIWAKUBE_DIR/firmware/CLOUDHV.fd
SSH_KEY=$NIWAKUBE_DIR/ssh/id_ed25519
RUN_DIR=/run/niwakube

log() { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
die() { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "root で実行してください"
CMD=${1:-}
NAME=${2:-test}
[[ $NAME =~ ^[a-z0-9-]{1,20}$ ]] || die "名前は英小文字・数字・ハイフンで 20 文字以内"

VM_DIR=$NIWAKUBE_DIR/vms/$NAME
SOCK=$RUN_DIR/$NAME.sock
UNIT=niwakube-vm-$NAME
# インターフェース名は 15 文字制限があるので名前のハッシュから作る
TAP=nkt$(printf '%s' "$NAME" | sha1sum | cut -c1-8)

ssh_vm() {
  ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR -o ConnectTimeout=3 "ubuntu@$IP" "$@"
}

up() {
  [[ -f $BASE_IMAGE ]] || die "$BASE_IMAGE がありません。先に host-setup.sh を実行してください"
  systemctl is-active --quiet "$UNIT" && die "$UNIT はすでに動いています"
  mkdir -p "$VM_DIR" "$RUN_DIR"

  log "ディスク作成 ($DISK_SIZE)"
  cp --reflink=auto --sparse=always "$BASE_IMAGE" "$VM_DIR/disk.raw"
  qemu-img resize -q -f raw "$VM_DIR/disk.raw" "$DISK_SIZE"

  # 52:54:00 はローカル管理用の慣例的な prefix
  local mac
  mac=52:54:00:$(printf '%s' "$NAME" | sha1sum | sed -E 's/^(..)(..)(..).*/\1:\2:\3/')
  echo "$mac" >"$VM_DIR/mac"

  log "cloud-init seed 作成 (IP=$IP, MAC=$mac)"
  cat >"$VM_DIR/meta-data" <<EOF
instance-id: $NAME-$(date +%s)
local-hostname: $NAME
EOF
  cat >"$VM_DIR/user-data" <<EOF
#cloud-config
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat "$SSH_KEY.pub")
ssh_pwauth: false
EOF
  cat >"$VM_DIR/network-config" <<EOF
version: 2
ethernets:
  eth0:
    match:
      macaddress: "$mac"
    set-name: eth0
    addresses: [$IP/$PREFIX]
    routes:
      - to: default
        via: $GATEWAY
    nameservers:
      addresses: [1.1.1.1, 8.8.8.8]
EOF
  cloud-localds -N "$VM_DIR/network-config" "$VM_DIR/seed.iso" "$VM_DIR/user-data" "$VM_DIR/meta-data"

  log "tap デバイス $TAP を $BRIDGE に接続"
  ip link show "$TAP" >/dev/null 2>&1 || ip tuntap add dev "$TAP" mode tap
  ip link set "$TAP" master "$BRIDGE" up

  log "cloud-hypervisor を systemd unit ($UNIT) として起動"
  rm -f "$SOCK"
  systemd-run --unit="$UNIT" --collect --quiet \
    -p Description="niwakube VM $NAME" \
    /usr/local/bin/cloud-hypervisor \
      --api-socket "path=$SOCK" \
      --firmware "$FIRMWARE" \
      --cpus "boot=$CPUS" \
      --memory "size=$MEM" \
      --disk "path=$VM_DIR/disk.raw" "path=$VM_DIR/seed.iso,readonly=on" \
      --net "tap=$TAP,mac=$mac" \
      --serial "file=$VM_DIR/serial.log" \
      --console off

  log "SSH が通るまで待機 (シリアルログ: tail -f $VM_DIR/serial.log)"
  local tries=$((BOOT_TIMEOUT / 2))
  for i in $(seq 1 "$tries"); do
    if ssh_vm true 2>/dev/null; then break; fi
    systemctl is-active --quiet "$UNIT" || { journalctl -u "$UNIT" --no-pager | tail -20; die "VMM が終了しました"; }
    [[ $i -eq $tries ]] && die "タイムアウト。シリアルログを確認してください"
    sleep 2
  done

  log "cloud-init の完了待ち"
  # 終了コード: 0 = 成功, 1 = エラー, 2 = 警告つきで完了 (degraded)。1 だけを失敗にする
  ssh_vm 'cloud-init status --wait >/dev/null; rc=$?; cloud-init status; [ $rc -ne 1 ]'

  log "VM からインターネットへの疎通 (NAT の確認)"
  ssh_vm 'curl -fsS -o /dev/null -w "    https://cloud-images.ubuntu.com -> HTTP %{http_code}\n" https://cloud-images.ubuntu.com/'

  log "VM の中から見たリソース"
  ssh_vm 'echo "    vCPU: $(nproc)  MEM: $(free -h | awk "/Mem:/{print \$2}")  DISK: $(df -h / | awk "NR==2{print \$2}")"'

  log "OK! 'sudo $0 ssh $NAME' でログイン、'sudo $0 down $NAME' で削除"
}

down() {
  if systemctl is-active --quiet "$UNIT"; then
    log "ACPI 電源ボタンでシャットダウン"
    ch-remote --api-socket "$SOCK" power-button 2>/dev/null || true
    for _ in $(seq 1 30); do systemctl is-active --quiet "$UNIT" || break; sleep 1; done
    if systemctl is-active --quiet "$UNIT"; then
      log "応答がないので強制停止"
      systemctl stop "$UNIT"
    fi
  fi
  ip link del "$TAP" 2>/dev/null || true
  rm -rf "$VM_DIR" "$SOCK"
  log "削除しました: $NAME"
}

case $CMD in
  up)   up ;;
  down) down ;;
  ssh)  shift $(( $# < 2 ? $# : 2 )); ssh_vm "$@" ;;
  info) ch-remote --api-socket "$SOCK" info | jq . ;;
  *)    sed -n '2,12p' "$0"; exit 1 ;;
esac
