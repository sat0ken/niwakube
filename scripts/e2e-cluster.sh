#!/usr/bin/env bash
# shellcheck disable=SC2016  # ssh_vm に渡すコマンドは VM 側で展開させるため
# niwakubed でクラスタを作る E2E テスト。実際に VM を作って Kubernetes を構築し、Pod を動かす。
#
#   make build && sudo ./scripts/e2e-cluster.sh bin/niwakubed
#
# 前提: scripts/host-setup.sh を実行済みであること。
# 環境変数: DISTRO (既定 k3s), NAME (e2ek), CLUSTER_TIMEOUT (900), LISTEN (127.0.0.1:18081)
set -euo pipefail

BIN=${1:?usage: $0 path/to/niwakubed}
DISTRO=${DISTRO:-k3s}
NAME=${NAME:-e2ek}
CLUSTER_TIMEOUT=${CLUSTER_TIMEOUT:-900}
LISTEN=${LISTEN:-127.0.0.1:18081}
NIWAKUBE_DIR=${NIWAKUBE_DIR:-/var/lib/niwakube}
LOG_DIR=${LOG_DIR:-$PWD/e2e-logs}
API=http://$LISTEN/api
SSH_KEY=$NIWAKUBE_DIR/ssh/id_ed25519
KUBECONFIG_FILE=$LOG_DIR/$NAME.kubeconfig
TEST_IMAGE=registry.k8s.io/e2e-test-images/agnhost:2.39

log() { printf '\e[1;34m==>\e[0m %s\n' "$*"; }
die() { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "root で実行してください"
[[ -x $BIN ]] || die "$BIN がありません (make build)"
mkdir -p "$LOG_DIR"

ssh_to() {
  local ip=$1
  shift
  ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR -o ConnectTimeout=5 "ubuntu@$ip" "$@"
}

"$BIN" --listen "$LISTEN" --data-dir "$NIWAKUBE_DIR" >"$LOG_DIR/niwakubed-cluster.log" 2>&1 &
DAEMON_PID=$!

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    log "失敗したのでログを集めます ($LOG_DIR)"
    curl -s "$API/clusters/$NAME" >"$LOG_DIR/cluster.json" 2>/dev/null || true
    for m in $(jq -r '.machines[]? | "\(.name)=\(.ip)"' "$LOG_DIR/cluster.json" 2>/dev/null); do
      local name=${m%=*} ip=${m#*=}
      curl -s "$API/vms/$name/serial?bytes=2097152" >"$LOG_DIR/$name-serial.log" || true
      ssh_to "$ip" 'sudo cat /var/log/cloud-init-output.log; sudo journalctl --no-pager -u "k3s*" | tail -300' \
        >"$LOG_DIR/$name-guest.log" 2>&1 || true
    done
    # Artifacts を取れない環境でも原因が分かるように、要点をジョブのログにも出す
    for f in "$LOG_DIR"/*-serial.log "$LOG_DIR"/*-guest.log; do
      [[ -s $f ]] || continue
      echo "----- tail $f"
      tail -n 80 "$f"
    done
    echo "----- systemctl status niwakube-vm-*"
    systemctl status --no-pager -l 'niwakube-vm-*' 2>&1 | tail -n 60 || true
    echo "----- journalctl -u niwakube-vm-*"
    journalctl --no-pager -u 'niwakube-vm-*' 2>&1 | tail -n 80 || true
    echo "----- niwakubed"
    grep -v 'msg=http ' "$LOG_DIR/niwakubed-cluster.log" | tail -n 40 || true
  fi
  if [[ ${KEEP:-} != 1 ]]; then
    curl -s -o /dev/null -X DELETE "$API/clusters/$NAME" || true
  fi
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

if ! command -v kubectl >/dev/null; then
  log "kubectl をダウンロード"
  ver=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
  curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/$ver/bin/linux/amd64/kubectl"
  chmod +x /usr/local/bin/kubectl
fi
k() { kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"; }

# --- クラスタを作る ---
log "$DISTRO クラスタを作成: $NAME (CP 1 + ワーカー 1)"
body=$(cat <<EOF
{"name":"$NAME","distro":"$DISTRO","workers":1,
 "controlPlane":{"cpus":2,"memoryMiB":3072,"diskGiB":15},
 "worker":{"cpus":2,"memoryMiB":2048,"diskGiB":15}}
EOF
)
out=$(curl -s -w '\n%{http_code}' -X POST "$API/clusters" -d "$body")
code=${out##*$'\n'}
[[ $code == 201 ]] || die "HTTP $code: ${out%$'\n'*}"
jq -c '{name, distro, version, subnet, endpoint, machines: [.machines[] | {name, ip, role}]}' <<<"${out%$'\n'*}"

log "準備完了を待つ (最大 ${CLUSTER_TIMEOUT}s)"
start=$SECONDS
last=""
degraded_since=""
while :; do
  st=$(curl -s "$API/clusters/$NAME" | jq -r '"\(.status.phase) \(.status.readyNodes)/\(.status.nodes) \(.status.message // "")"')
  if [[ $st != "$last" ]]; then
    printf '    [%4ds] %s\n' $((SECONDS - start)) "$st"
    last=$st
  fi
  [[ $st == ready* ]] && break
  # VM が止まった状態が続いたら、待たずに失敗にする
  if [[ $st == degraded* ]]; then
    degraded_since=${degraded_since:-$SECONDS}
    (( SECONDS - degraded_since < 60 )) || die "VM が止まったままです: $st"
  else
    degraded_since=""
  fi
  (( SECONDS - start < CLUSTER_TIMEOUT )) || die "${CLUSTER_TIMEOUT}s 待っても準備完了になりませんでした"
  sleep 5
done
log "クラスタの準備完了 ($((SECONDS - start))s)"

# --- kubeconfig で操作する ---
curl -fs "$API/clusters/$NAME/kubeconfig" >"$KUBECONFIG_FILE"
k get nodes -o wide
[[ $(k get nodes --no-headers | wc -l) == 2 ]] || die "ノード数が違います"

log "Deployment を作って両方のノードに Pod を置く"
k create deployment hello --image="$TEST_IMAGE" --replicas=2 --port=8080 -- /agnhost netexec --http-port=8080
k rollout status deployment/hello --timeout=300s
k get pods -o wide

log "Service とクラスタ内 DNS 経由で Pod に接続する"
k expose deployment hello --port=8080
k run probe --image="$TEST_IMAGE" --restart=Never --command -- /agnhost connect --timeout=10s hello.default.svc.cluster.local:8080
if ! k wait --for=jsonpath='{.status.phase}'=Succeeded pod/probe --timeout=180s; then
  k describe pod probe || true
  k logs probe || true
  die "Service に接続できませんでした"
fi

log "クラスタを削除"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$API/clusters/$NAME")
[[ $code == 204 ]] || die "削除に失敗しました: HTTP $code"
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/clusters/$NAME")
[[ $code == 404 ]] || die "クラスタが残っています: HTTP $code"
for d in "$NIWAKUBE_DIR/vms/$NAME"-*; do
  [[ -e $d ]] && die "VM のディレクトリが残っています: $d"
done

log "すべて成功しました"
