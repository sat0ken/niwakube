#!/usr/bin/env bash
# shellcheck disable=SC2016  # ssh_vm に渡すコマンドは VM 側で展開させるため
# インストーラー ISO の E2E テスト。QEMU (UEFI) の VM で実際に無人インストールし、
# インストール後のディスクから起動して、設定どおりになっているかを確かめる。
#
#   ./scripts/e2e-installer.sh WORKDIR
#     WORKDIR には installer/fetch-ubuntu.sh が作った mini.iso と server.env があること
#
# 必要なもの: /dev/kvm、qemu-system-x86、ovmf、qemu-utils、xorriso、dosfstools、mtools、go
# 環境変数: CONFIG (既定 installer/config.yaml)、INSTALL_TIMEOUT (秒、既定 3600)
set -euo pipefail

WORK=${1:?usage: $0 WORKDIR}
CONFIG=${CONFIG:-installer/config.yaml}
INSTALL_TIMEOUT=${INSTALL_TIMEOUT:-3600}
LOG_DIR=${LOG_DIR:-$PWD/e2e-logs}
T=$WORK/e2e
SSH_PORT=2222
DISK_SERIAL=niwakube-ci-disk # internal/installer.TestDiskSerial と同じ

log() { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
die() { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

[[ -c /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm が使えません"
mkdir -p "$T" "$LOG_DIR"

OVMF_CODE=""
OVMF_VARS=""
for f in /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd; do [[ -f $f ]] && { OVMF_CODE=$f; break; }; done
for f in /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/OVMF/OVMF_VARS.fd; do [[ -f $f ]] && { OVMF_VARS=$f; break; }; done
[[ -n $OVMF_CODE && -n $OVMF_VARS ]] || die "OVMF が見つかりません (apt install ovmf)"

QEMU_PID=""
cleanup() {
  local rc=$?
  [[ -n $QEMU_PID ]] && kill "$QEMU_PID" 2>/dev/null
  cp "$T"/*-serial.log "$LOG_DIR/" 2>/dev/null || true
  if [[ $rc -ne 0 ]]; then
    for f in "$T"/*-serial.log; do
      [[ -s $f ]] || continue
      echo "----- tail $f"
      tail -n 120 "$f" | sed 's/\x1b\[[0-9;?]*[A-Za-z]//g'
    done
  fi
  exit "$rc"
}
trap cleanup EXIT

# --- テスト用の ISO を作る (実機向けと同じコードで、ネットワーク・ディスク・無人インストールだけ変える) ---
log "テスト用の SSH 鍵と ISO を作る"
rm -f "$T/id_ed25519" "$T/id_ed25519.pub"
ssh-keygen -q -t ed25519 -N '' -C niwakube-e2e -f "$T/id_ed25519"
go run ./cmd/niwakube-iso build -config "$CONFIG" -mini "$WORK/mini.iso" -server "$WORK/server.env" \
  -test -extra-ssh-key "$(cat "$T/id_ed25519.pub")" -out "$T/test.iso"

USER_NAME=$(go run ./cmd/niwakube-iso get -config "$CONFIG" user.name)
HOST_NAME=$(go run ./cmd/niwakube-iso get -config "$CONFIG" hostname)
SETUP=$(go run ./cmd/niwakube-iso get -config "$CONFIG" niwakube.setup)

qemu-img create -q -f qcow2 "$T/disk.qcow2" 60G
cp "$OVMF_VARS" "$T/vars.fd"

qemu() { # 追加の引数をそのまま渡す
  qemu-system-x86_64 -enable-kvm -machine q35 -cpu host -smp 4 -m 8192 \
    -drive if=pflash,format=raw,unit=0,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,unit=1,file="$T/vars.fd" \
    -drive if=none,id=disk0,file="$T/disk.qcow2",format=qcow2 \
    -device nvme,serial=$DISK_SERIAL,drive=disk0,bootindex=1 \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22 -device virtio-net-pci,netdev=n0 \
    -display none -monitor none "$@"
}

# --- 1. USB から起動して無人インストール (終わると電源が切れる) ---
log "USB (ISO) から起動して無人インストール (最大 ${INSTALL_TIMEOUT}s)"
qemu -device qemu-xhci \
  -drive if=none,id=usb0,file="$T/test.iso",format=raw,readonly=on \
  -device usb-storage,drive=usb0,bootindex=0 \
  -serial file:"$T/install-serial.log" &
QEMU_PID=$!
start=$SECONDS
last=""
last_size=0
last_change=$SECONDS
while kill -0 "$QEMU_PID" 2>/dev/null; do
  (( SECONDS - start < INSTALL_TIMEOUT )) || die "${INSTALL_TIMEOUT}s 待ってもインストールが終わりませんでした"
  clean=$(sed 's/\x1b\[[0-9;?]*[A-Za-z]//g' "$T/install-serial.log" 2>/dev/null || true)
  # mini ISO のスクリプトが失敗して debug shell で止まったら、待たずに失敗にする
  if grep -a -q 'niwakube: ERROR' <<<"$clean"; then
    sleep 5
    die "mini ISO のスクリプトが失敗しました"
  fi
  # インストーラー (subiquity) がエラーになったら、待たずに失敗にする
  if grep -a -q -E 'An error occurred\. Press enter to start a shell|matched no disk' <<<"$clean"; then
    sleep 5
    die "インストーラーがエラーで止まりました"
  fi
  # シリアルの出力が長く止まったら失敗にする
  size=$(stat -c %s "$T/install-serial.log" 2>/dev/null || echo 0)
  if (( size != last_size )); then
    last_size=$size
    last_change=$SECONDS
  elif (( SECONDS - last_change > 900 )); then
    die "15 分間シリアルコンソールの出力がありません"
  fi
  # 進み具合を 30 秒ごとに表示する
  line=$(grep -a -v -E '^\s*$' <<<"$clean" | tail -n1 | tr -d '\r' | cut -c1-150 || true)
  if [[ -n $line && $line != "$last" ]]; then
    printf '    [%4ds] %s\n' $((SECONDS - start)) "$line"
    last=$line
  fi
  sleep 30
done
wait "$QEMU_PID" || true
QEMU_PID=""
log "インストールが終わり、VM の電源が切れました ($((SECONDS - start))s)"
grep -a -q -E 'Booting the installer: .*autoinstall' "$T/install-serial.log" \
  || echo "    (注意: シリアルログに niwakube のブートメッセージが見つかりません)"

# --- 2. インストールしたディスクから起動する ---
log "インストールしたディスクから起動"
qemu -serial file:"$T/boot-serial.log" &
QEMU_PID=$!

ssh_vm() {
  ssh -i "$T/id_ed25519" -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes "$USER_NAME@127.0.0.1" "$@"
}
start=$SECONDS
until ssh_vm true 2>/dev/null; do
  (( SECONDS - start < 600 )) || die "600s 待っても SSH でログインできません"
  kill -0 "$QEMU_PID" 2>/dev/null || die "VM が停止しました"
  sleep 5
done
log "SSH でログインできました ($((SECONDS - start))s)"

log "初回起動の処理 (cloud-init) の完了待ち"
ssh_vm 'cloud-init status --wait >/dev/null; rc=$?; cloud-init status --long | sed "s/^/    /"; [ $rc -ne 1 ]' \
  || die "cloud-init がエラーで終わりました"

log "設定どおりか確かめる"
check() { # 説明 期待値 実際の値
  if [[ $2 == "$3" ]]; then
    printf '    %-28s OK (%s)\n' "$1" "$3"
  else
    die "$1: 期待値 '$2'、実際は '$3'"
  fi
}
check "ホスト名" "$HOST_NAME" "$(ssh_vm hostname)"
check "パスワードはロック" "L" "$(ssh_vm "sudo passwd -S $USER_NAME | awk '{print \$2}'")"
check "SSH のパスワード認証は無効" "passwordauthentication no" "$(ssh_vm 'sudo sshd -T | grep -i "^passwordauthentication"')"
check "/var/lib/niwakube は XFS" "xfs" "$(ssh_vm 'findmnt -n -o FSTYPE /var/lib/niwakube')"
check "ルートは LVM 上の ext4" "ext4" "$(ssh_vm 'findmnt -n -o FSTYPE /')"
check "UEFI で起動している" "yes" "$(ssh_vm '[ -d /sys/firmware/efi ] && echo yes || echo no')"
if [[ $SETUP == true ]]; then
  if ! ssh_vm 'test -f /var/lib/niwakube/.firstboot-done'; then
    ssh_vm 'sudo tail -n 60 /var/log/niwakube-firstboot.log' || true
    die "初回起動の niwakube のセットアップが終わっていません"
  fi
  check "niwakube のブリッジ" "172.30.0.1/16" "$(ssh_vm "ip -4 -br addr show nkbr0 | awk '{print \$3}'")"
fi
ssh_vm 'df -h / /var/lib/niwakube | sed "s/^/    /"'

ssh_vm 'sudo systemctl poweroff' || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""
log "すべて成功しました"
