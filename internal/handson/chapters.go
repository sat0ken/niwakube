// Package handson は CloudNative Days のハンズオン (cloudnativedaysjp/cnd-handson) の資料を Web UI に出す。
//
// 資料 (chapter ごとの README) は GitHub から取ってきて、サーバーで HTML にする。Web UI はそれを左に、
// ハンズオン用のクラスタのターミナルを右に並べる。niwakube での読み替えは chapter ごとの Notes にまとめる
// (docs/cnd-handson.md と同じ内容)。
package handson

// Doc は chapter の中の Markdown のファイル 1 つ。
type Doc struct {
	File  string `json:"file"`
	Title string `json:"title"`
}

// Chapter はハンズオンの章。
type Chapter struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Docs  []Doc    `json:"docs"`
	Notes []string `json:"notes,omitempty"` // niwakube での読み替え
}

func readme(title string) []Doc { return []Doc{{File: "README.md", Title: title}} }

// Chapters はハンズオンの章 (README の順)。
var Chapters = []Chapter{
	{ID: "chapter_setup", Title: "事前準備", Docs: readme("事前準備"), Notes: []string{
		"この章は全部飛ばします。VM の作成・apt・hosts の書き換え・git clone は niwakube で済んでいます。",
		"ブラウザで開く PC の hosts は、クラスタの画面に出る行を貼ります (ミニPC の LAN の IP に向けます)。",
		"右のターミナルで、最初に git clone https://github.com/cloudnativedaysjp/cnd-handson.git を実行します (ホームは閉じても残ります)。",
	}},
	{ID: "chapter_cluster-create", Title: "Kubernetes クラスターの作成", Docs: readme("Kubernetes クラスターの作成"), Notes: []string{
		"「アプリケーションのデプロイ」から始めます。それより前 (kind・Gateway API の CRD・Cilium・Envoy Gateway・Gateway) は niwakube が済ませています。",
		"helmfile sync -f helm/helmfile.yaml は実行しないでください。Cilium が kind-control-plane 宛ての設定で上書きされ、クラスタが壊れます。",
		"kubectl apply -f manifest/gateway/gateway.yaml も不要です (同じものが入っています)。確認の kubectl get gatewayclass,gateway -n gateway はそのまま使えます。",
		"出力の kind-kind や 127.0.0.1:44707、172.18.0.3 は、niwakube のコンテキスト名や CP の IP になります。",
	}},
	{ID: "chapter_docker", Title: "Docker", Docs: readme("Docker"), Notes: []string{
		"ターミナルでは docker を動かせません (sudo も docker も無い一時ユーザー)。この章はご自分の PC の Docker で進めてください。",
	}},
	{ID: "chapter_kubernetes-introduction", Title: "Kubernetes 入門", Docs: readme("Kubernetes 入門"), Notes: []string{
		"出力のノード名は kind-control-plane / kind-worker / kind-worker2 ではなく、<クラスタ名>-cp-1 / -worker-1 / -worker-2 です。コンテキスト名も kind-kind ではありません。",
	}},
	{ID: "chapter_kubernetes-overview", Title: "Kubernetes 概論", Docs: readme("Kubernetes 概論"), Notes: []string{
		"kubectl config set-context handson-user --cluster=kind-kind は、--cluster=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}') にします。",
		"kubectl config use-context kind-kind の前に ORIG_CTX=$(kubectl config current-context) で控えておき、kubectl config use-context \"$ORIG_CTX\" で戻します (ターミナルを開き直しても戻ります)。",
		"hostPath の /tmp に書いたファイルは、Pod が動いたノードに ssh worker (または ssh worker2) で入って確かめます。",
	}},
	{ID: "chapter_prometheus", Title: "Prometheus", Docs: readme("Prometheus")},
	{ID: "chapter_grafana", Title: "Grafana", Docs: readme("Grafana"), Notes: []string{
		"Git Sync には資料のとおり GitHub のアカウントとトークンが要ります。",
	}},
	{ID: "chapter_opentelemetry", Title: "OpenTelemetry", Docs: readme("OpenTelemetry"), Notes: []string{
		"docker exec -it kind-worker sh -c \"echo ... >> /tmp/cndt-1.json\" は、ssh worker \"echo '{\\\"key1\\\": \\\"value1\\\", \\\"key2\\\": \\\"value2\\\"}' >> /tmp/cndt-1.json\" にします。",
		"docker exec -it kind-worker sh -c \"cat /tmp/all.json | jq .\" は、ssh worker cat /tmp/all.json | jq . にします。",
	}},
	{ID: "chapter_argocd", Title: "Argo CD", Docs: []Doc{{File: "README.md", Title: "Argo CD"}, {File: "README_cmp.md", Title: "Argo CD with CMP"}}, Notes: []string{
		"資料のとおり、GitHub でリポジトリを fork します。",
	}},
	{ID: "chapter_argocd-image-updater", Title: "Argo CD Image Updater", Docs: []Doc{{File: "README_ArgoCD_Image_Updater.md", Title: "Argo CD Image Updater"}}},
	{ID: "chapter_cicd", Title: "CI/CD", Docs: []Doc{{File: "README.md", Title: "CI/CD"}, {File: "README_CI.md", Title: "CI"}, {File: "README_CD.md", Title: "CD"}}, Notes: []string{
		"ターミナルから git push するので、GitHub のトークン (HTTPS) か SSH の鍵を用意します。git config --global user.name / user.email も設定します。",
	}},
	{ID: "chapter_istio", Title: "Istio", Docs: readme("Istio")},
	{ID: "chapter_cilium", Title: "Cilium", Docs: readme("Cilium"), Notes: []string{
		"manifest/l2announcement.yaml の 172.18.0.200〜250 (kind の docker ネットワーク) は、クラスタのサブネットの 200〜250 に変えて適用します。例: sed 's/172\\.18\\.0\\./172.30.1./' manifest/l2announcement.yaml | kubectl apply -f - (172.30.1 はクラスタの画面の「サブネット」に合わせる)",
		"niwakube はもともと 172.30.N.100-199 の IP プールを作っているので、資料で <pending> になるところで、すぐに IP が付きます。",
		"LoadBalancer の IP には、PC のブラウザからではなく、ターミナル (ミニPC) から curl します。",
	}},
	{ID: "chapter_argo-rollouts", Title: "Argo Rollouts", Docs: readme("Argo Rollouts")},
	{ID: "chapter_istio-ambient-mode", Title: "Istio Ambient Mode", Docs: readme("Istio Ambient Mode"), Notes: []string{
		"この章は Cilium の無い 2 つ目のクラスタを使います。ハンズオン用のクラスタでは、この章の helmfile sync を実行しないでください (prometheus の章の設定が上書きされます)。",
		"今は手で用意します: 普通のクラスタ (kubeadm、v1.36、CNI flannel、ワーカー 1 台) を作り、ホストの 28080 を CP の 32766 に転送し、そのクラスタのターミナルで進めます。手順は docs/cnd-handson.md にあります。",
		"helm/values/prometheus-stack.values.yaml の storageClassName: standard は local-path に変えます。",
	}},
	{ID: "chapter_hubble", Title: "Hubble", Docs: readme("Hubble"), Notes: []string{
		"install-tools.sh は飛ばします (hubble は入っています)。",
	}},
	{ID: "chapter_loki", Title: "Grafana Loki", Docs: readme("Grafana Loki")},
	{ID: "chapter_tempo", Title: "Grafana Tempo", Docs: readme("Grafana Tempo")},
	{ID: "chapter_pyroscope", Title: "Pyroscope", Docs: readme("Pyroscope")},
	{ID: "chapter_troubleshoot", Title: "トラブルシューティング", Docs: []Doc{{File: "README.md", Title: "トラブルシューティング"}, {File: "ANSWER.md", Title: "解決方法"}}},
	{ID: "chapter_kubevirt", Title: "KubeVirt", Docs: readme("KubeVirt"), Notes: []string{
		"virtctl は sudo mv virtctl /usr/local/bin ではなく mkdir -p ~/bin && mv virtctl ~/bin/ にします。",
		"ノードの VM で KVM が使えるかは ssh worker 'ls -l /dev/kvm' で確かめられます。使えなければ資料どおり useEmulation: true にします (Fedora の起動は遅くなります)。",
	}},
}

// find は id の章を返す。
func find(id string) (*Chapter, bool) {
	for i := range Chapters {
		if Chapters[i].ID == id {
			return &Chapters[i], true
		}
	}
	return nil, false
}

func (c *Chapter) hasDoc(file string) bool {
	for _, d := range c.Docs {
		if d.File == file {
			return true
		}
	}
	return false
}
