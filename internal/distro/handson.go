package distro

import (
	_ "embed"
	"fmt"
	"strings"
)

// CloudNative Days のハンズオン (https://github.com/cloudnativedaysjp/cnd-handson) 用のクラスタ。
// ハンズオンの chapter_cluster-create で kind に入れるもの (Gateway API の CRD、Cilium、Envoy Gateway、Gateway) を、
// niwakube がクラスタの作成と一緒に入れる。バージョンはハンズオンに合わせる。
const (
	HandsonCiliumVersion       = "1.20.4"
	HandsonGatewayAPIVersion   = "v1.6.1"
	HandsonEnvoyGatewayVersion = "v1.9.1"
	// HandsonKubernetesVersion は Cilium 1.20 と Envoy Gateway 1.9 が対応している最後のマイナーバージョン
	HandsonKubernetesVersion = "v1.36"
)

// HandsonNodePorts は、ハンズオンが kind の extraPortMappings でホストに出しているポート (ホスト → CP の NodePort)。
var HandsonNodePorts = []struct {
	HostPort, NodePort int
	What               string
}{
	{80, 30080, "Gateway API (Envoy Gateway)"},
	{8080, 31080, "Cilium Ingress"},
	{8443, 31443, "Cilium Ingress (HTTPS)"},
	{18080, 32080, "Istio Ingress Gateway"},
	{18443, 32443, "Istio Ingress Gateway (HTTPS)"},
}

//go:embed handson/cilium-values.yaml
var handsonCiliumValues string

//go:embed handson/gateway.yaml
var handsonGateway string

const handsonValuesPath = "/etc/niwakube/cilium-handson.values.yaml"

// handsonBeforeCilium は Cilium の前に入れるもの。Cilium の Gateway API は、起動したときに CRD が無いと有効にならない。
func handsonBeforeCilium() string {
	return fmt.Sprintf(`
# --- ハンズオン: Cilium の設定と Gateway API の CRD ---
mkdir -p /etc/niwakube
cat > %[1]s <<'NIWAKUBE_EOF'
%[2]sNIWAKUBE_EOF
for i in $(seq 1 10); do
  kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/%[3]s/experimental-install.yaml >/dev/null && break
  sleep 15
done
kubectl get crd gateways.gateway.networking.k8s.io >/dev/null
`, handsonValuesPath, handsonCiliumValues, HandsonGatewayAPIVersion)
}

// handsonAfterCilium は Cilium のあとに入れるもの (helm、Envoy Gateway、ハンズオンの Gateway)。
func handsonAfterCilium() string {
	return fmt.Sprintf(`
# --- ハンズオン: Envoy Gateway と Gateway ---
export HOME=/root
if ! command -v helm >/dev/null 2>&1; then
  ver=$(curl -fsSL --retry 5 https://get.helm.sh/helm-latest-version)
  tmp=$(mktemp -d)
  curl -fsSL --retry 5 -o "$tmp/helm.tar.gz" "https://get.helm.sh/helm-${ver}-linux-${arch}.tar.gz"
  sum=$(curl -fsSL --retry 5 "https://get.helm.sh/helm-${ver}-linux-${arch}.tar.gz.sha256sum" | awk '{print $1}')
  echo "${sum}  $tmp/helm.tar.gz" | sha256sum -c --quiet -
  tar -xzf "$tmp/helm.tar.gz" -C "$tmp" "linux-${arch}/helm"
  install -m 0755 "$tmp/linux-${arch}/helm" /usr/local/bin/helm
  rm -rf "$tmp"
fi
# Envoy Gateway の CRD は大きく、helm のリリースにすると上限を超えるので、ハンズオンと同じく template して適用する
for i in $(seq 1 10); do
  helm template envoy-gateway-crds oci://docker.io/envoyproxy/gateway-crds-helm --version %[1]s \
    --set crds.gatewayAPI.enabled=false --set crds.envoyGateway.enabled=true > /tmp/envoy-gateway-crds.yaml &&
    kubectl apply --server-side -f /tmp/envoy-gateway-crds.yaml >/dev/null && break
  sleep 15
done
for i in $(seq 1 10); do
  helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm --version %[1]s \
    -n envoy-gateway-system --create-namespace --set crds.enabled=false --wait --timeout 10m && break
  sleep 15
done
cat > /etc/niwakube/handson-gateway.yaml <<'NIWAKUBE_EOF'
%[2]sNIWAKUBE_EOF
for i in $(seq 1 30); do
  kubectl apply -f /etc/niwakube/handson-gateway.yaml && break
  sleep 10
done
kubectl -n gateway wait gateway/handson-gateway --for=condition=Programmed --timeout=10m
`, HandsonEnvoyGatewayVersion, handsonGateway)
}

// handsonCiliumArgs は cilium install に足す引数。
func handsonCiliumArgs() string {
	return strings.Join([]string{"", "--version", HandsonCiliumVersion, "--values", handsonValuesPath}, " ")
}
