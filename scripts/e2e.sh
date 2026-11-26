#!/usr/bin/env bash
# shellcheck disable=SC2016  # ssh_vm に渡すコマンドは VM 側で展開させるため
# niwakubed の E2E テスト。実際に Cloud Hypervisor で VM を作り、API を一通り叩く。
# ミニPC の実機でも、KVM の使える GitHub Actions でも同じように動く。
#
#   make build && sudo ./scripts/e2e.sh bin/niwakubed
#
# 前提: scripts/host-setup.sh を実行済みであること (イメージ・ファームウェア・ブリッジ・NAT)。
# 環境変数: NAME (既定 e2e), MEMORY_MIB (2048), BOOT_TIMEOUT (300), LISTEN (127.0.0.1:18080)
set -euo pipefail

BIN=${1:?usage: $0 path/to/niwakubed}
NAME=${NAME:-e2e}
# cloud-hypervisor (VM) か nspawn (systemd-nspawn のコンテナ)
BACKEND=${BACKEND:-cloud-hypervisor}
MEMORY_MIB=${MEMORY_MIB:-2048}
BOOT_TIMEOUT=${BOOT_TIMEOUT:-300}
LISTEN=${LISTEN:-127.0.0.1:18080}
NIWAKUBE_DIR=${NIWAKUBE_DIR:-/var/lib/niwakube}
LOG_DIR=${LOG_DIR:-$PWD/e2e-logs}
API=http://$LISTEN/api
SSH_KEY=$NIWAKUBE_DIR/ssh/id_ed25519

log()  { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
die()  { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "root で実行してください"
[[ -x $BIN ]] || die "$BIN がありません (make build)"
mkdir -p "$LOG_DIR"

# --- niwakubed を起動 ---
"$BIN" --listen "$LISTEN" --data-dir "$NIWAKUBE_DIR" --backend "$BACKEND" >"$LOG_DIR/niwakubed.log" 2>&1 &
DAEMON_PID=$!

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    log "失敗したのでログを集めます ($LOG_DIR)"
    curl -s "$API/vms/$NAME/serial?bytes=1048576" >"$LOG_DIR/serial.log" 2>/dev/null || true
    journalctl --no-pager -u "niwakube-vm-$NAME" >"$LOG_DIR/vmm.log" 2>/dev/null || true
    curl -s "$API/vms" >"$LOG_DIR/vms.json" 2>/dev/null || true
    ip -br addr >"$LOG_DIR/ip-addr.txt" 2>/dev/null || true
    nft list ruleset >"$LOG_DIR/nft.txt" 2>/dev/null || true
    tail -50 "$LOG_DIR/niwakubed.log" || true
    tail -50 "$LOG_DIR/serial.log" || true
  fi
  curl -s -o /dev/null -X DELETE "$API/vms/$NAME" || true
  kill "$DAEMON_PID" 2>/dev/null || true
  wait "$DAEMON_PID" 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT

for _ in $(seq 1 50); do
  curl -fs "http://$LISTEN/healthz" >/dev/null && break
  sleep 0.2
done
curl -fs "http://$LISTEN/healthz" >/dev/null || die "niwakubed が起動しませんでした"
log "niwakubed $("$BIN" -version) が起動しました"

# Web UI が配信されていること
curl -fs "http://$LISTEN/" | grep -q '/ui/app.js' || die "Web UI (/) が返りません"
curl -fs "http://$LISTEN/ui/app.js" | grep -q 'function route' || die "Web UI (/ui/app.js) が返りません"
curl -fs "http://$LISTEN/api/host" | grep -q '"cpus"' || die "/api/host が返りません"

# api METHOD PATH [BODY] → 標準出力に body、変数 CODE に HTTP ステータス
api() {
  local out
  out=$(curl -s -w '\n%{http_code}' -X "$1" "$API$2" ${3:+-d "$3"})
  CODE=${out##*$'\n'}
  BODY=${out%$'\n'*}
}

expect() {
  [[ $CODE == "$1" ]] || die "HTTP $CODE (期待値 $1): $BODY"
}

state() {
  api GET "/vms/$NAME"
  expect 200
  jq -r .status.state <<<"$BODY"
}

wait_state() {
  local want=$1 timeout=$2 s
  for _ in $(seq 1 "$timeout"); do
    s=$(state)
    [[ $s == "$want" ]] && return 0
    sleep 1
  done
  die "状態が $want になりませんでした (現在: $s)"
}

ssh_vm() {
  ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR -o ConnectTimeout=5 "ubuntu@$IP" "$@"
}

wait_ssh() {
  local start=$SECONDS
  until ssh_vm true 2>/dev/null; do
    (( SECONDS - start < BOOT_TIMEOUT )) || die "${BOOT_TIMEOUT}s 待っても SSH がつながりません"
    sleep 3
  done
  log "SSH 接続 OK ($((SECONDS - start))s)"
}

# --- テスト本体 ---
log "入力チェック"
api POST /vms '{"name":"Bad_Name"}'
expect 400

log "VM を作成: $NAME"
api POST /vms "{\"name\":\"$NAME\",\"cpus\":2,\"memoryMiB\":$MEMORY_MIB,\"diskGiB\":10}"
expect 201
IP=$(jq -r .ip <<<"$BODY")
echo "    $BODY"

log "同じ名前はもう作れない"
api POST /vms "{\"name\":\"$NAME\"}"
expect 409

wait_state running 30
wait_ssh

log "cloud-init の完了待ち"
# 終了コード: 0 = 成功, 1 = エラー, 2 = 警告つきで完了 (degraded)。1 だけを失敗にする
ssh_vm 'cloud-init status --wait >/dev/null; rc=$?; cloud-init status --long; [ $rc -ne 1 ]' | sed 's/^/    /'

log "VM の中身を確認"
ssh_vm 'echo "    hostname=$(hostname) vcpu=$(nproc) ip=$(ip -4 -br addr show eth0 | awk "{print \$3}")"'
[[ $(ssh_vm hostname) == "$NAME" ]] || die "ホスト名が違います"
# コンテナの中の nproc にはホストの CPU が見えるので、nspawn は unit の CPU の上限で確かめる
unit_prop() { systemctl show -p "$1" --value "niwakube-vm-$NAME"; }
if [[ $BACKEND == nspawn ]]; then
  [[ $(unit_prop CPUQuotaPerSecUSec) == 2s ]] || die "CPU の上限が違います: $(unit_prop CPUQuotaPerSecUSec)"
  echo "    nspawn: $(machinectl list --no-legend | grep -w "$NAME" || true)"
  ssh_vm 'ip -br addr; ip route; cat /etc/resolv.conf; systemctl --failed --no-legend; systemd-analyze 2>/dev/null | head -n1' | sed 's/^/    /' || true
else
  [[ $(ssh_vm nproc) == 2 ]] || die "vCPU 数が違います"
fi

log "VM から外部への疎通 (NAT)"
ssh_vm 'curl -fsS -o /dev/null -w "    HTTP %{http_code}\n" --max-time 20 https://cloud-images.ubuntu.com/'

log "シリアルログが読める"
api GET "/vms/$NAME/serial?bytes=4096"
expect 200
[[ -n $BODY ]] || die "シリアルログが空です"

log "正常停止 (ACPI)"
api POST "/vms/$NAME/stop"
expect 200
wait_state stopped 120

log "再起動"
api POST "/vms/$NAME/start"
expect 200
wait_state running 30
wait_ssh

log "サイズの変更 (動いている VM を止めて、vCPU・メモリ・ディスクを増やして起動し直す)"
root0=$(ssh_vm "df -BM --output=size / | tail -n1 | tr -dc 0-9")
api POST "/vms/$NAME/resize" "{\"cpus\":3,\"memoryMiB\":$((MEMORY_MIB + 1024)),\"diskGiB\":14}"
expect 200
echo "    $BODY"
wait_state running 30
wait_ssh
# パーティションとファイルシステムは cloud-init (growpart / resizefs) が起動時に広げる
ssh_vm 'cloud-init status --wait >/dev/null; true'
if [[ $BACKEND == nspawn ]]; then
  # コンテナはホストの CPU・メモリ・ディスクが見えるので、unit の上限で確かめる (ディスクはホストと共有)
  echo "    CPUQuota=$(unit_prop CPUQuotaPerSecUSec) MemoryMax=$(unit_prop MemoryMax)"
  [[ $(unit_prop CPUQuotaPerSecUSec) == 3s ]] || die "CPU の上限が変わっていません"
  [[ $(unit_prop MemoryMax) == $(( (MEMORY_MIB + 1024) * 1024 * 1024 )) ]] || die "メモリの上限が変わっていません"
else
  vcpu=$(ssh_vm nproc)
  mem=$(ssh_vm "awk '/MemTotal/ {print int(\$2/1024)}' /proc/meminfo")
  root1=$(ssh_vm "df -BM --output=size / | tail -n1 | tr -dc 0-9")
  echo "    vcpu=$vcpu memTotal=${mem}MiB root=${root0}MiB -> ${root1}MiB"
  [[ $vcpu == 3 ]] || die "vCPU 数が変わっていません ($vcpu)"
  (( mem > MEMORY_MIB + 512 )) || die "メモリが増えていません (${mem}MiB)"
  (( root1 > root0 + 3000 )) || die "ルートのファイルシステムが広がっていません (${root0}MiB -> ${root1}MiB)"
fi
api POST "/vms/$NAME/resize" '{"diskGiB":10}'
expect 400

log "強制停止"
api POST "/vms/$NAME/stop?force=true"
expect 200
wait_state stopped 30

log "削除"
api DELETE "/vms/$NAME"
expect 204
api GET "/vms/$NAME"
expect 404
[[ ! -e $NIWAKUBE_DIR/vms/$NAME ]] || die "VM のディレクトリが残っています"

log "すべて成功しました"
