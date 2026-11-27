#!/usr/bin/env bash
# shellcheck disable=SC2016  # ssh_vm に渡すコマンドは VM 側で展開させるため
# niwakubed でクラスタを作る E2E テスト。実際に VM を作って Kubernetes を構築し、Pod を動かす。
#
#   make build && sudo ./scripts/e2e-cluster.sh bin/niwakubed
#
# 前提: scripts/host-setup.sh を実行済みであること。
# 環境変数: DISTRO (既定 k3s), CNI (cilium), RUNTIME (既定のランタイム、runc), RUNTIMES (追加で入れるもの、空白区切り),
#           NAME (e2ek), CLUSTER_TIMEOUT (900), LISTEN (127.0.0.1:18081)
set -euo pipefail

BIN=${1:?usage: $0 path/to/niwakubed}
DISTRO=${DISTRO:-k3s}
CNI=${CNI:-cilium}
# cloud-hypervisor (VM) か nspawn (systemd-nspawn のコンテナ。CNI は flannel)
BACKEND=${BACKEND:-cloud-hypervisor}
RUNTIME=${RUNTIME:-runc}
RUNTIMES=${RUNTIMES:-}
NAME=${NAME:-e2ek}
CLUSTER_TIMEOUT=${CLUSTER_TIMEOUT:-900}
LISTEN=${LISTEN:-127.0.0.1:18081}
NIWAKUBE_DIR=${NIWAKUBE_DIR:-/var/lib/niwakube}
LOG_DIR=${LOG_DIR:-$PWD/e2e-logs}
API=http://$LISTEN/api
SSH_KEY=$NIWAKUBE_DIR/ssh/id_ed25519
KUBECONFIG_FILE=$LOG_DIR/$NAME.kubeconfig
TEST_IMAGE=registry.k8s.io/e2e-test-images/agnhost:2.39
BUSYBOX_IMAGE=registry.k8s.io/e2e-test-images/busybox:1.36.1-1

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

"$BIN" --listen "$LISTEN" --data-dir "$NIWAKUBE_DIR" --backend "$BACKEND" >"$LOG_DIR/niwakubed-cluster.log" 2>&1 &
DAEMON_PID=$!

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    log "失敗したのでログを集めます ($LOG_DIR)"
    curl -s "$API/clusters/$NAME" >"$LOG_DIR/cluster.json" 2>/dev/null || true
    for m in $(jq -r '.machines[]? | "\(.name)=\(.ip)"' "$LOG_DIR/cluster.json" 2>/dev/null); do
      local name=${m%=*} ip=${m#*=}
      curl -s "$API/vms/$name/serial?bytes=2097152" >"$LOG_DIR/$name-serial.log" || true
      ssh_to "$ip" 'sudo cat /var/log/cloud-init-output.log; sudo journalctl --no-pager -u "k3s*" -u kubelet -u containerd | tail -300; sudo kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get pods -A -o wide 2>/dev/null || sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get pods -A -o wide 2>/dev/null' \
        >"$LOG_DIR/$name-guest.log" 2>&1 || true
    done
    # Artifacts を取れない環境でも原因が分かるように、要点をジョブのログにも出す
    for f in "$LOG_DIR"/*-serial.log "$LOG_DIR"/*-guest.log; do
      [[ -s $f ]] || continue
      echo "----- tail $f"
      tail -n 80 "$f"
    done
    for f in "$LOG_DIR"/*-guest.log; do
      [[ -s $f ]] || continue
      echo "----- errors in $f"
      grep -n -i -E 'error|fail|not found|exit [1-9]' "$f" | grep -v -E 'level=info' | tail -n 40 || true
      # set -x のスクリプトが最後に実行したコマンド (メッセージを出さずに失敗したときの手がかり)
      echo "----- last commands in $f"
      grep -a -E '^\+ ' "$f" | tail -n 15 || true
    done
    if [[ -s $KUBECONFIG_FILE ]]; then
      echo "----- pods / events"
      kubectl --kubeconfig "$KUBECONFIG_FILE" get pods -A -o wide 2>&1 | tail -n 40 || true
      kubectl --kubeconfig "$KUBECONFIG_FILE" get events -A --sort-by=.lastTimestamp 2>&1 | tail -n 40 || true
    fi
    echo "----- machine status (niwakubed)"
    jq -c '.machines[]? | {name, ip, status}' "$LOG_DIR/cluster.json" 2>/dev/null || true
    echo "----- vm.info (直接)"
    for sock in /run/niwakube/"$NAME"-*.sock; do
      [[ -S $sock ]] || continue
      printf '%s: ' "$sock"
      curl -s --max-time 5 --unix-socket "$sock" http://localhost/api/v1/vm.info | jq -c '{state}' 2>&1 || echo "(failed)"
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
log "$DISTRO クラスタを作成: $NAME (CP 1 + ワーカー 1、CNI $CNI、ランタイム $RUNTIME${RUNTIMES:+ + $RUNTIMES})"
# shellcheck disable=SC2086  # 空白で分割したい
install_json=$(printf '%s\n' $RUNTIMES | jq -R . | jq -s -c 'map(select(. != ""))')
body=$(cat <<EOF
{"name":"$NAME","distro":"$DISTRO","workers":1,"cni":"$CNI",
 "runtime":{"default":"$RUNTIME","install":$install_json},
 "controlPlane":{"cpus":2,"memoryMiB":3072,"diskGiB":15},
 "worker":{"cpus":2,"memoryMiB":3072,"diskGiB":15}}
EOF
)
out=$(curl -s -w '\n%{http_code}' -X POST "$API/clusters" -d "$body")
code=${out##*$'\n'}
[[ $code == 201 ]] || die "HTTP $code: ${out%$'\n'*}"
jq -c '{name, distro, version, cni, hubble, runtime, subnet, endpoint, machines: [.machines[] | {name, ip, role}]}' <<<"${out%$'\n'*}"

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

# --- Web UI のターミナル (WebSocket → systemd-run の一時ユーザーのサンドボックスのシェル) ---
# 入力はそのまま画面に返る (エコー) ので、確かめる値は計算させて、入力と区別できるようにする
log "Web UI のターミナルで kubectl を使う (root ではなく、データディレクトリは見えない)"
TERMCHECK=$(dirname "$BIN")/termcheck
term_cmd='echo "U=$(id -u)"; ls '"$NIWAKUBE_DIR"' >/dev/null 2>&1 && echo "H=$((1+1))" || echo "H=$((2+2))"; kubectl get nodes -o name | sed "s/^/N:/"; exit'
if ! "$TERMCHECK" -url "ws://$LISTEN/api/clusters/$NAME/terminal" -send "$term_cmd" \
  -expect "N:node/" -expect "H=4" -reject "U=0"$'\r' -timeout 90s >"$LOG_DIR/terminal.log" 2>&1; then
  cat "$LOG_DIR/terminal.log"
  journalctl --no-pager -n 50 -u 'niwakube-term-*' || true
  die "ターミナルで kubectl が使えません"
fi
if [[ $DISTRO != talos ]]; then
  # クラスタの鍵で、ターミナルからノードに ssh できること (CKA の練習問題で使う)
  ssh_cmd='ssh -o ConnectTimeout=10 worker "echo SSH=\$(hostname)"; exit'
  if ! "$TERMCHECK" -url "ws://$LISTEN/api/clusters/$NAME/terminal" -send "$ssh_cmd" \
    -expect "SSH=$NAME-worker-1" -timeout 90s >"$LOG_DIR/terminal-ssh.log" 2>&1; then
    cat "$LOG_DIR/terminal-ssh.log"
    die "ターミナルからノードに ssh できません"
  fi
  echo "    ssh worker: OK"
fi
grep -a 'U=\|H=\|N:node' "$LOG_DIR/terminal.log" | grep -v 'echo' | sed 's/^/    /'

if [[ $CNI == cilium ]]; then
  log "Cilium (kube-proxy の置き換え) と Hubble"
  k -n kube-system rollout status daemonset/cilium --timeout=300s
  kpr=$(k -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status | grep -i '^KubeProxyReplacement' || true)
  echo "    $kpr"
  [[ $kpr == *True* ]] || die "Cilium が kube-proxy を置き換えていません"
  if k -n kube-system get daemonset kube-proxy >/dev/null 2>&1; then
    die "kube-proxy が動いています"
  fi
  k -n kube-system rollout status deployment/hubble-relay --timeout=300s
  k -n kube-system rollout status deployment/hubble-ui --timeout=300s
fi

log "Deployment を作る (kubeadm と Talos は CP に taint があるので、Pod はワーカーに載る)"
k create deployment hello --image="$TEST_IMAGE" --replicas=2 --port=8080 -- /agnhost netexec --http-port=8080
k rollout status deployment/hello --timeout=300s
k get pods -o wide

# probe NAME TARGET NODE: NODE 上の Pod から TARGET (host:port) に TCP 接続できるかを確かめる。
# restartPolicy=OnFailure なので、つながるまでコンテナが再起動して再試行する。
probe() {
  local name=$1 target=$2 node=$3
  k run "$name" --image="$TEST_IMAGE" --restart=OnFailure \
    --overrides="{\"spec\":{\"nodeName\":\"$node\"}}" \
    --command -- /agnhost connect --timeout=10s "$target" >/dev/null
  if ! k wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$name" --timeout=180s; then
    k describe "pod/$name" | tail -n 20 || true
    k logs "pod/$name" || true
    die "$node から $target に接続できませんでした"
  fi
  echo "    $node -> $target: OK"
}

log "Service を作り、エンドポイントが揃うのを待つ"
k expose deployment hello --port=8080
for _ in $(seq 1 60); do
  n=$(k get endpointslice -l kubernetes.io/service-name=hello \
        -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{"\n"}{end}' | grep -c . || true)
  [[ $n == 2 ]] && break
  sleep 2
done
[[ $n == 2 ]] || die "Service のエンドポイントが揃いません ($n/2)"

NODES=$(k get nodes -o jsonpath='{.items[*].metadata.name}')
POD_IPS=$(k get pods -l app=hello -o jsonpath='{.items[*].status.podIP}')

log "クラスタ内 DNS + Service 経由で接続する (両方のノードから)"
for node in $NODES; do
  probe "svc-$node" hello.default.svc.cluster.local:8080 "$node"
done

log "各ノードから全 Pod に直接接続する (ノードをまたぐ Pod 間通信)"
i=0
for node in $NODES; do
  for ip in $POD_IPS; do
    probe "pod-$i-$node" "$ip:8080" "$node"
    i=$((i + 1))
  done
done

log "niwakubed が入れるアドオン (LoadBalancer の IP プール、ストレージ) の適用を待つ"
for _ in $(seq 1 90); do
  addons=$(curl -s "$API/clusters/$NAME" | jq -r '.addons // ""')
  [[ $addons == applied ]] && break
  sleep 5
done
[[ $addons == applied ]] || die "アドオンが適用されません: $addons"
curl -s "$API/clusters/$NAME" | jq -c '{storageClasses, loadBalancerPool}'

if [[ $CNI == cilium ]]; then
  log "LoadBalancer (Cilium の LB IPAM + L2 アナウンス)"
  pool=$(curl -s "$API/clusters/$NAME" | jq -r .loadBalancerPool)
  echo "    pool: $pool"
  k expose deployment hello --name hello-lb --type LoadBalancer --port 8080 >/dev/null
  lbip=""
  for _ in $(seq 1 60); do
    lbip=$(k get svc hello-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    [[ -n $lbip ]] && break
    sleep 3
  done
  [[ -n $lbip ]] || die "LoadBalancer に IP が割り当てられません"
  [[ $lbip == "${pool%.*-*}".1[0-9][0-9] ]] || die "LoadBalancer の IP $lbip がプール $pool の外です"
  echo "    hello-lb: $lbip"

  # ホストは VM と同じ L2 (nkbr0) にいるので、L2 アナウンス (ARP) で直接届く
  ok=""
  for _ in $(seq 1 30); do
    if name=$(curl -fsS --max-time 5 "http://$lbip:8080/hostname"); then ok=1; break; fi
    sleep 2
  done
  [[ -n $ok ]] || die "ホストから LoadBalancer ($lbip) に届きません"
  echo "    host -> $lbip:8080: OK ($name)"

  log "ホストのポートを LoadBalancer に転送する (LAN からの入口)"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/clusters/$NAME/forwards" -d "{\"hostPort\":18480,\"ip\":\"$lbip\",\"port\":8080}")
  [[ $code == 201 ]] || die "転送を追加できません: HTTP $code"
  host_ip=$(hostname -I | awk '{print $1}')
  name=$(curl -fsS --max-time 5 "http://$host_ip:18480/hostname") || die "ホストのポート ($host_ip:18480) から届きません"
  echo "    $host_ip:18480 -> $lbip:8080: OK ($name)"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$API/clusters/$NAME/forwards/tcp/18480")
  [[ $code == 204 ]] || die "転送を削除できません: HTTP $code"
  if curl -fsS --max-time 3 "http://$host_ip:18480/hostname" >/dev/null 2>&1; then
    die "転送を削除しても届いてしまいます"
  fi
  echo "    転送を削除すると届かなくなる: OK"
fi

# pv_pod NAME PVC: PVC をマウントして /data/hello に書き込む Pod を作る
pv_pod() {
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  containers:
    - name: writer
      image: $BUSYBOX_IMAGE
      command: ["sh", "-c", "echo hello-from-$1 > /data/hello && sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $2
EOF
}

log "PV: local-path (ノードのディスク、既定の StorageClass)"
k get storageclass
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pvc-local
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 64Mi
EOF
pv_pod pv-local pvc-local
if ! k wait --for=condition=Ready pod/pv-local --timeout=300s; then
  k describe pvc pvc-local | tail -n 15 || true
  k describe pod pv-local | tail -n 15 || true
  die "local-path の PV を使う Pod が起動しません"
fi
[[ $(k exec pv-local -- cat /data/hello) == hello-from-pv-local ]] || die "local-path の PV に書き込めません"
echo "    local-path: OK ($(k get pvc pvc-local -o jsonpath='{.spec.storageClassName}'))"

log "PV: NFS (ホストの NFS サーバー、StorageClass niwakube-nfs、ReadWriteMany)"
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pvc-nfs
spec:
  storageClassName: niwakube-nfs
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 64Mi
EOF
pv_pod pv-nfs pvc-nfs
if ! k wait --for=condition=Ready pod/pv-nfs --timeout=300s; then
  k describe pvc pvc-nfs | tail -n 15 || true
  k describe pod pv-nfs | tail -n 15 || true
  k -n kube-system logs deploy/csi-nfs-controller -c nfs --tail=30 || true
  die "NFS の PV を使う Pod が起動しません"
fi
# ホスト側の NFS のディレクトリにファイルができている
host_file=$(find "$NIWAKUBE_DIR/nfs/$NAME" -name hello -type f 2>/dev/null | head -n1)
[[ -n $host_file && $(cat "$host_file") == hello-from-pv-nfs ]] || die "ホストの NFS のディレクトリにファイルがありません"
echo "    NFS: OK (ホストの $host_file)"

if [[ -n $RUNTIMES || $RUNTIME != runc ]]; then
  log "RuntimeClass ごとに Pod を動かす"
  k get runtimeclass
  for rt in runc $RUNTIMES; do
    k run "rt-$rt" --image="$TEST_IMAGE" --restart=Never \
      --overrides="{\"spec\":{\"runtimeClassName\":\"$rt\"}}" --command -- /agnhost pause >/dev/null
  done
  for rt in runc $RUNTIMES; do
    if ! k wait --for=condition=Ready "pod/rt-$rt" --timeout=180s; then
      k describe "pod/rt-$rt" | tail -n 20 || true
      die "RuntimeClass $rt の Pod が起動しません"
    fi
    echo "    $rt: OK"
  done
fi

# --- ノードのサイズの変更 (drain → 止めて変える → 起動 → Ready → uncordon) ---
log "ワーカーのサイズを変える (メモリ 3GiB → 4GiB、ディスク 15GiB → 20GiB)"
# 量 (例: 3016908Ki、16Gi、12345) を KiB にする
to_kib() {
  local q=$1
  case $q in
    *Ki) echo "${q%Ki}" ;;
    *Mi) echo $(( ${q%Mi} * 1024 )) ;;
    *Gi) echo $(( ${q%Gi} * 1024 * 1024 )) ;;
    *) echo $(( q / 1024 )) ;;
  esac
}
worker=$NAME-worker-1
mem0=$(to_kib "$(k get node "$worker" -o jsonpath='{.status.capacity.memory}')")
eph0=$(to_kib "$(k get node "$worker" -o jsonpath='{.status.capacity.ephemeral-storage}')")
code=$(curl -s -o "$LOG_DIR/resize.json" -w '%{http_code}' -X POST "$API/clusters/$NAME/resize" \
  -d '{"role":"worker","size":{"memoryMiB":4096,"diskGiB":20}}')
[[ $code == 202 ]] || die "サイズの変更を受け付けません: $code $(cat "$LOG_DIR/resize.json")"
start=$SECONDS
while :; do
  op=$(curl -fs "$API/clusters/$NAME" | jq -r '.operation // ""')
  [[ -z $op ]] && break
  [[ $op == failed:* ]] && die "サイズの変更に失敗しました: $op"
  (( SECONDS - start < 900 )) || die "900s 待ってもサイズの変更が終わりません: $op"
  sleep 10
done
log "サイズの変更が終わりました ($((SECONDS - start))s)"
k wait --for=condition=Ready "node/$worker" --timeout=300s >/dev/null
mem1=$(to_kib "$(k get node "$worker" -o jsonpath='{.status.capacity.memory}')")
eph1=$(to_kib "$(k get node "$worker" -o jsonpath='{.status.capacity.ephemeral-storage}')")
unsched=$(k get node "$worker" -o jsonpath='{.spec.unschedulable}')
if [[ $BACKEND == nspawn ]]; then
  # コンテナの kubelet にはホストのメモリとディスクが見えるので、unit のメモリの上限で確かめる
  memmax=$(systemctl show -p MemoryMax --value "niwakube-vm-$worker")
  echo "    MemoryMax: $memmax, unschedulable: ${unsched:-false}"
  [[ $memmax == $((4096 * 1024 * 1024)) ]] || die "コンテナのメモリの上限が変わっていません: $memmax"
else
  echo "    memory: $((mem0 / 1024))MiB -> $((mem1 / 1024))MiB, ephemeral-storage: $((eph0 / 1024 / 1024))GiB -> $((eph1 / 1024 / 1024))GiB, unschedulable: ${unsched:-false}"
  (( mem1 > mem0 + 800 * 1024 )) || die "ノードのメモリが増えていません"
  (( eph1 > eph0 + 3 * 1024 * 1024 )) || die "ノードのディスク (ephemeral-storage) が広がっていません"
fi
[[ -z $unsched || $unsched == false ]] || die "ノードが cordon されたままです"
[[ $(curl -fs "$API/clusters/$NAME" | jq -r '.worker.memoryMiB') == 4096 ]] || die "クラスタの設定 (worker の大きさ) が更新されていません"

# --- ワーカーの台数の変更 (1 → 2 → 1) ---
# wait_op <説明>: クラスタの operation が空になるまで待つ
wait_op() {
  local what=$1 start=$SECONDS op
  while :; do
    op=$(curl -fs "$API/clusters/$NAME" | jq -r '.operation // ""')
    [[ -z $op ]] && break
    [[ $op == failed:* ]] && die "$what に失敗しました: $op"
    (( SECONDS - start < 900 )) || die "900s 待っても $what が終わりません: $op"
    sleep 5
  done
  log "$what が終わりました ($((SECONDS - start))s)"
}
scale() {
  local code
  code=$(curl -s -o "$LOG_DIR/scale.json" -w '%{http_code}' -X POST "$API/clusters/$NAME/scale" -d "{\"workers\":$1}")
  [[ $code == 202 ]] || die "台数の変更を受け付けません: $code $(cat "$LOG_DIR/scale.json")"
}
log "ワーカーを 2 台に増やす"
scale 2
wait_op "ワーカーの追加"
# kubectl wait は Node がまだ無いとすぐに失敗するので、先に登録 (kubelet の参加) を待つ
start=$SECONDS
until k get node "$NAME-worker-2" >/dev/null 2>&1; do
  if (( SECONDS - start > 900 )); then
    k get nodes -o wide || true
    die "900s 待っても追加したワーカーが参加しません"
  fi
  sleep 5
done
log "追加したワーカーが参加しました ($((SECONDS - start))s)"
if ! k wait --for=condition=Ready "node/$NAME-worker-2" --timeout=600s; then
  k get nodes -o wide || true
  die "追加したワーカーが Ready になりません"
fi
k get nodes -o wide
[[ $(k get nodes --no-headers | wc -l) == 3 ]] || die "ノードが 3 台になっていません"
[[ $(curl -fs "$API/clusters/$NAME" | jq -r '.workers') == 2 ]] || die "クラスタの設定 (workers) が 2 になっていません"
# 追加したワーカーで Pod が動くこと (CNI が使えること)
k run on-new-worker --image="$TEST_IMAGE" --restart=Never \
  --overrides="{\"spec\":{\"nodeName\":\"$NAME-worker-2\"}}" --command -- /agnhost pause
if ! k wait --for=condition=Ready pod/on-new-worker --timeout=300s; then
  k describe pod/on-new-worker | tail -n 20 || true
  die "追加したワーカーで Pod が動きません"
fi
k delete pod/on-new-worker --wait=false >/dev/null

log "ワーカーを 1 台に減らす (番号の大きい $NAME-worker-2 を drain して消す)"
scale 1
wait_op "ワーカーの削除"
k get nodes -o wide
k get node "$NAME-worker-2" >/dev/null 2>&1 && die "消したワーカーの Node が残っています"
[[ ! -e $NIWAKUBE_DIR/vms/$NAME-worker-2 ]] || die "消したワーカーのディレクトリが残っています"
k get node "$NAME-worker-1" >/dev/null || die "$NAME-worker-1 が消えています"
k rollout status deployment/hello --timeout=300s || die "ワーカーを減らしたあと Deployment が戻りません"

# --- 練習問題 (CKA / CKAD): 出題 → 解答を当てる → 採点が満点 ---
log "練習問題: 出題直後は満点でなく、解答を当てると満点になること"
practice() { curl -s -o "$LOG_DIR/practice.json" -w '%{http_code}' -X POST "$API/practice/problems/$1/$2" -d "{\"cluster\":\"$NAME\"}"; }
# requires (kubeadm など) が合わない問題は飛ばす。ノードに SSH する問題 (CKA) も、ここで作ったクラスタなら鍵がある
mapfile -t problem_ids < <(curl -fs "$API/practice/problems" | jq -r --arg d "$DISTRO" '.[] | select(.requires == null or .requires == $d) | .id')
(( ${#problem_ids[@]} > 0 )) || die "練習問題がありません"
for id in "${problem_ids[@]}"; do
  code=$(practice "$id" setup)
  [[ $code == 204 ]] || die "$id: 出題できません: $code $(cat "$LOG_DIR/practice.json")"
  code=$(practice "$id" check)
  [[ $code == 200 ]] || die "$id: 採点できません: $code $(cat "$LOG_DIR/practice.json")"
  [[ $(jq -r '.passed < .total' "$LOG_DIR/practice.json") == true ]] || die "$id: 出題直後なのに満点です"
  code=$(practice "$id" solve)
  [[ $code == 204 ]] || die "$id: 模範解答を当てられません: $code $(cat "$LOG_DIR/practice.json")"
  start=$SECONDS
  while :; do
    code=$(practice "$id" check)
    [[ $code == 200 && $(jq -r '.passed == .total' "$LOG_DIR/practice.json") == true ]] && break
    if (( SECONDS - start > 300 )); then
      jq . "$LOG_DIR/practice.json" || true
      die "$id: 解答を当てても 300s のうちに満点になりません"
    fi
    sleep 5
  done
  echo "    $id: $(jq -r '"\(.score)/\(.weight) 点 (\(.passed)/\(.total) 項目)"' "$LOG_DIR/practice.json"), $((SECONDS - start))s"
done
# もう一度出題すると、解いた状態は消えて出題直後に戻る
id=${problem_ids[0]}
[[ $(practice "$id" setup) == 204 ]] || die "$id: やり直せません: $(cat "$LOG_DIR/practice.json")"
practice "$id" check >/dev/null
[[ $(jq -r '.passed < .total' "$LOG_DIR/practice.json") == true ]] || die "$id: やり直したのに解いた状態が残っています"

# --- スナップショット: 取る → 変える → 戻す ---
log "スナップショット: 取って、変えて、戻すと、取ったときの状態に戻ること"
# wait_ready <説明>: クラスタが ready に戻るまで待つ (止めて起動し直したあと)
wait_ready() {
  local what=$1 start=$SECONDS st
  while :; do
    st=$(curl -s "$API/clusters/$NAME" | jq -r '"\(.status.phase) \(.status.readyNodes)/\(.status.nodes)"')
    [[ $st == ready* ]] && break
    (( SECONDS - start < CLUSTER_TIMEOUT )) || die "$what: ${CLUSTER_TIMEOUT}s 待っても ready に戻りません ($st)"
    sleep 5
  done
  log "$what: ready に戻りました ($((SECONDS - start))s)"
}
k create configmap snap-marker --from-literal=when=before >/dev/null
code=$(curl -s -o "$LOG_DIR/snapshot.json" -w '%{http_code}' -X POST "$API/clusters/$NAME/snapshots" -d '{"name":"e2e"}')
[[ $code == 202 ]] || die "スナップショットを受け付けません: $code $(cat "$LOG_DIR/snapshot.json")"
wait_op "スナップショット"
[[ -f $NIWAKUBE_DIR/snapshots/$NAME/e2e/meta.json ]] || die "スナップショットのファイルがありません"
du -sh --apparent-size "$NIWAKUBE_DIR/snapshots/$NAME/e2e" | sed 's/^/    見かけの大きさ: /'
du -sh "$NIWAKUBE_DIR/snapshots/$NAME/e2e" | sed 's/^/    実際に使っている容量: /'
wait_ready "スナップショットのあと"
k get configmap snap-marker >/dev/null || die "スナップショットのあと、ConfigMap が消えています"

# 変える: マーカーを消し、別の ConfigMap を作る
k delete configmap snap-marker >/dev/null
k create configmap snap-after --from-literal=when=after >/dev/null
code=$(curl -s -o "$LOG_DIR/snapshot.json" -w '%{http_code}' -X POST "$API/clusters/$NAME/snapshots/e2e/restore")
[[ $code == 202 ]] || die "復元を受け付けません: $code $(cat "$LOG_DIR/snapshot.json")"
wait_op "復元"
wait_ready "復元のあと"
[[ $(k get configmap snap-marker -o jsonpath='{.data.when}') == before ]] || die "復元したのに、スナップショットのときの ConfigMap が戻っていません"
k get configmap snap-after >/dev/null 2>&1 && die "復元したのに、スナップショットのあとに作った ConfigMap が残っています"
k get nodes -o wide
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$API/clusters/$NAME/snapshots/e2e")
[[ $code == 204 ]] || die "スナップショットを削除できません: $code"
[[ ! -e $NIWAKUBE_DIR/snapshots/$NAME/e2e ]] || die "スナップショットのファイルが残っています"

log "クラスタを削除"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$API/clusters/$NAME")
[[ $code == 204 ]] || die "削除に失敗しました: HTTP $code"
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/clusters/$NAME")
[[ $code == 404 ]] || die "クラスタが残っています: HTTP $code"
for d in "$NIWAKUBE_DIR/vms/$NAME"-*; do
  [[ -e $d ]] && die "VM のディレクトリが残っています: $d"
done
[[ ! -e $NIWAKUBE_DIR/nfs/$NAME ]] || die "NFS のディレクトリが残っています"

log "すべて成功しました"
