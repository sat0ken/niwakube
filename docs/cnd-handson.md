# CloudNative Days のハンズオンを niwakube で進める

CloudNative Days Winter 2026 実行委員会による『一日で学ぶクラウドネイティブ技術実践ハンズオン』
([cloudnativedaysjp/cnd-handson](https://github.com/cloudnativedaysjp/cnd-handson)) を、niwakube のクラスタで進めるための手引きです。
ハンズオンは AWS の VM の上に kind でクラスタを作る前提で書かれているので、そのための準備 (chapter_setup・chapter_cluster-create) を
niwakube が代わりに済ませ、残りの違いをこのページで読み替えます。

## クラスタを作る

Web UI の「クラスタを作る」で、用途に「CloudNative Days のハンズオン (cnd-handson)」を選びます。API なら:

```bash
curl -s -X POST localhost:8080/api/clusters -d '{"name":"cnd","profile":"cnd-handson"}'
```

作られるもの (chapter_cluster-create で kind に入れるものと同じ):

| もの | 内容 |
|---|---|
| ノード | kubeadm、Kubernetes v1.36 (Cilium 1.20 と Envoy Gateway 1.9 が対応する範囲)、コントロールプレーン 1 台 + ワーカー 2 台 |
| Gateway API の CRD | v1.6.1 (experimental) |
| Cilium | 1.20.4。ハンズオンの `cilium.values.yaml` と同じ設定 (kube-proxy の置き換え、Gateway API、Ingress を NodePort 31080/31443、Prometheus のメトリクス、Hubble の relay・UI・メトリクス、`socketLB.hostNamespaceOnly`、`cni.exclusive: false`)。`k8sServiceHost` は `kind-control-plane` ではなく CP の IP |
| Envoy Gateway | v1.9.1 と、Gateway `gateway/handson-gateway` (GatewayClass `eg`、Envoy は NodePort 30080) |
| ホストのポートの転送 | kind の `extraPortMappings` の代わり。ミニPC の 80 → CP:30080 (Envoy Gateway)、8080 → 31080 / 8443 → 31443 (Cilium Ingress)、18080 → 32080 / 18443 → 32443 (Istio) |
| StorageClass | `local-path` (既定)。kind の `standard` の代わり |

- ポートの転送があるので、ハンズオン用のクラスタは同時に 1 つまでです。
- 元の環境は 4 vCPU / 16 GiB の VM 1 台です。Loki・KubeVirt・Prometheus まで進めるなら、ワーカーは large (4 vCPU / 8 GiB) がおすすめです。
- Envoy Gateway は Cilium のあとに入れるので、クラスタが「準備完了」になってから数分遅れて揃います。`kubectl get gateway -n gateway` で `PROGRAMMED` が `True` になれば使えます。

## 作業する場所: 資料とターミナルを並べた画面

クラスタの画面の「資料とターミナルを並べて開く」を押すと、練習問題の画面のように、左にハンズオンの資料、右にターミナルが並びます。

- 資料は GitHub (raw.githubusercontent.com) からミニPC が取ってきて表示します (1 時間覚えておき、GitHub に届かないときは覚えているものを出します)。
  章は上のメニューで選び、資料の中のほかの章へのリンクもこの画面で開きます。最後に開いた章はブラウザが覚えています。
- 章ごとの niwakube での読み替え (このページの下の「章ごとの読み替え」と同じもの) を、資料の上に出します。
- コマンドのブロックの「ターミナルに貼る」で右のターミナルに貼ります (実行はしないので、確かめてから Enter を押します)。「コピー」もあります。
- 章を変えてもターミナルはつないだままです。

ハンズオンの「VM のターミナル」は、この右側のターミナル (クラスタの画面の「ターミナル」と同じシェル) です。

- **ホームは閉じても残ります** (ハンズオン用のクラスタだけ)。最初に `git clone https://github.com/cloudnativedaysjp/cnd-handson.git` します。
  資料の `cd ~/cnd-handson/...` はそのまま使えます。クラスタを消すとホームも消えます。
- **kubectl・helm・helmfile・cilium・hubble・git・jq・curl・openssl・watch が入っています**。kubeconfig も設定済みです
  (ターミナルを開くたびに `~/.kube/config` を作り直します)。
- **sudo と docker は使えません**。資料の `sudo ... /usr/local/bin` で入れる手順は飛ばし、入っていないもの (virtctl など) は `~/bin` に置きます (PATH に入っています)。
- **`*.example.com` はミニPC に向いています**。資料の `curl http://app.example.com` などはそのまま動きます (VM の /etc/hosts を書き換える手順は要りません)。
- **ノードには `ssh cp` / `ssh worker` / `ssh worker2` で入れます** (ノードの中では sudo が使えます)。
- ノード名は kind の `kind-control-plane` / `kind-worker` / `kind-worker2` ではなく、`<クラスタ名>-cp-1` / `-worker-1` / `-worker-2` です。

## ブラウザで開く PC の hosts

ブラウザで開くのはハンズオンを進める PC です。hosts (Windows は `C:\Windows\System32\drivers\etc\hosts`、Mac / Linux は `/etc/hosts`) に、
ミニPC の LAN の IP (例: 192.168.0.50) で次の名前を足します。クラスタの画面にも、そのまま貼れる形で出ます。

```text
192.168.0.50    hello-world.example.com rollout.example.com blue-green.example.com app.example.com
192.168.0.50    prometheus.example.com grafana.example.com jaeger.example.com argocd.example.com
192.168.0.50    app-argocd.example.com app-cicd.example.com dev-kustomize-argocd.example.com prd-kustomize-argocd.example.com
192.168.0.50    helm-argocd.example.com app-preview-argocd.example.com headlamp.example.com argocdupdate.example.com
192.168.0.50    app-rollouts.example.com app-preview-rollouts.example.com kiali.example.com kiali-ambient.example.com
192.168.0.50    app-cilium.example.com hubble-cilium.example.com pyroscope.example.com troubleshoot.example.com
192.168.0.50    cnd-web.example.com
```

## 章ごとの読み替え

書いていない章 (prometheus・loki・tempo・pyroscope・argocd・argocd-image-updater・argo-rollouts・cicd・troubleshoot など) は、
上の「ターミナル」と「hosts」さえ揃っていれば資料のまま進められます。argocd 系と cicd は資料のとおり GitHub のアカウント (fork、cicd は push 用のトークン) が要ります。

### chapter_setup

全部飛ばします (VM の作成、apt、hosts、clone は上で済ませました)。

### chapter_cluster-create

「アプリケーションのデプロイ」から始めます。それより前は niwakube が済ませています。

| 資料 | niwakube |
|---|---|
| `./install-tools.sh`、sysctl、`kind create cluster` | 飛ばす |
| Gateway API の CRD、Envoy Gateway の CRD の apply | 飛ばす (入っています) |
| `helmfile sync -f helm/helmfile.yaml` | **実行しない**。Cilium を `k8sServiceHost: kind-control-plane` の設定で上書きして、クラスタが壊れます |
| `kubectl apply -f manifest/gateway/gateway.yaml` | 飛ばす (同じものが入っています。確認の `kubectl get gatewayclass,gateway -n gateway` はそのまま) |
| 出力の `kind-kind` や `127.0.0.1:44707`、`172.18.0.3` | niwakube のコンテキスト名や CP の IP (`172.30.N.11` など) になります |

### chapter_docker

ターミナルでは docker を動かせないので、ご自分の PC の Docker で進めてください (Docker Hub への push はありません)。

### chapter_kubernetes-introduction / chapter_kubernetes-overview

- 出力のノード名とコンテキスト名 (`kind-kind`) が変わります。
- overview の `kubectl config set-context handson-user --cluster=kind-kind` は、`--cluster=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')` にします。
  `kubectl config use-context kind-kind` は、先に `ORIG_CTX=$(kubectl config current-context)` を控えておき、`kubectl config use-context "$ORIG_CTX"` にします
  (戻し忘れても、ターミナルを開き直せば元の kubeconfig に戻ります)。
- `hostPath: /tmp` に書いたファイルは、Pod が動いたノードに `ssh worker` (または `worker2`) で入って確かめます。

### chapter_opentelemetry

- `docker exec -it kind-worker sh -c "echo '{...}' >> /tmp/cndt-1.json"` は `ssh worker "echo '{\"key1\": \"value1\", \"key2\": \"value2\"}' >> /tmp/cndt-1.json"`
- `docker exec -it kind-worker sh -c "cat /tmp/all.json | jq ."` は `ssh worker cat /tmp/all.json | jq .`

### chapter_cilium

- `manifest/l2announcement.yaml` の IP の範囲 `172.18.0.200`〜`172.18.0.250` は kind の docker ネットワークのものです。クラスタのサブネット (クラスタの画面の「サブネット」、例: `172.30.1.0/24`) の
  200〜250 に変えて適用します。`sed 's/172\.18\.0\./172.30.1./' manifest/l2announcement.yaml | kubectl apply -f -`
- niwakube はもともと `172.30.N.100-199` の IP プールを作っているので、資料で `<pending>` になるところで、すぐに IP が付きます。
- LoadBalancer の IP には、LAN の PC からではなく、ターミナル (ミニPC) から `curl` します。

### chapter_istio

資料のまま進められます (Istio の Ingress Gateway は ClusterIP で、入口は Envoy Gateway)。

### chapter_istio-ambient-mode

この章は Cilium の無い 2 つ目のクラスタ (kind の既定の CNI と kube-proxy、ワーカー 1 台、ホストの 28080 → Kiali の NodePort 32766) を作ります。
niwakube で専用に作る機能はまだ無いので、今は次のように用意します。

1. 普通のクラスタを作る: `{"name":"ambient","distro":"kubeadm","version":"v1.36","cni":"flannel","workers":1}` (flannel と kube-proxy。Gateway API の CRD も Envoy Gateway も入りません)
2. 28080 を Kiali につなぐ: `curl -X POST localhost:8080/api/clusters/ambient/forwards -d '{"hostPort":28080,"ip":"<ambient の CP の IP>","port":32766}'`
3. そのクラスタの「ターミナル」で cnd-handson を clone し直し、`helm/values/prometheus-stack.values.yaml` の `storageClassName: standard` を `local-path` に変えてから進めます
   (このターミナルのホームは閉じると消えます)。
4. `kind-istio-ambient` コンテキストはそのクラスタの kubeconfig、`istio-ambient-worker` は `ambient-worker-1` に読み替えます。最後の `kind delete cluster` は niwakube でクラスタを削除します。

**ハンズオン用のクラスタでは、この章の `helmfile sync` を実行しないでください** (prometheus の章の kube-prometheus-stack が別の設定で上書きされます)。

### chapter_hubble

`install-tools.sh` は飛ばします (hubble は入っています)。`cilium hubble port-forward &` はそのまま使えます。

### chapter_kubevirt

- virtctl は `sudo mv virtctl /usr/local/bin` ではなく `mkdir -p ~/bin && mv virtctl ~/bin/` にします。
- ノードの VM の中で KVM が使えるか、`ssh worker 'ls -l /dev/kvm'` で確かめられます。使えなければ資料どおり `useEmulation: true` にします (Fedora の起動は遅くなります)。
- Fedora の VM は 2 GiB ずつ使うので、ワーカーは large にしておくと安心です。出力の StorageClass `standard` は `local-path` になります。
