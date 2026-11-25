package distro

import (
	"fmt"
	"slices"
	"strings"
)

// CNI の種類
const (
	CNICilium  = "cilium"
	CNIFlannel = "flannel"
)

// コンテナランタイム
const (
	RuntimeRunc  = "runc"
	RuntimeCrun  = "crun"
	RuntimeYouki = "youki"
)

// KnownRuntimes は追加で入れられるランタイム (runc は常に入っている)。
var KnownRuntimes = []string{RuntimeCrun, RuntimeYouki}

// Addons は CNI とコンテナランタイムの設定。
type Addons struct {
	CNI    string // cilium | flannel
	Hubble bool   // Cilium の Hubble (relay + UI) を入れる
	// DefaultRuntime は containerd の既定のランタイム。Runtimes に含まれているか runc であること。
	DefaultRuntime string
	// Runtimes は runc 以外に入れるランタイム。それぞれ同じ名前の RuntimeClass も作る。
	Runtimes []string
}

// Validate は組み合わせを検証する。
func (a Addons) Validate() error {
	switch a.CNI {
	case CNICilium:
	case CNIFlannel:
		if a.Hubble {
			return fmt.Errorf("hubble needs cni: cilium")
		}
	default:
		return fmt.Errorf("cni must be cilium or flannel: %q", a.CNI)
	}
	for _, r := range a.Runtimes {
		if !slices.Contains(KnownRuntimes, r) {
			return fmt.Errorf("runtime %q is not supported (choose from %s)", r, strings.Join(KnownRuntimes, ", "))
		}
	}
	if a.DefaultRuntime != RuntimeRunc && !slices.Contains(a.Runtimes, a.DefaultRuntime) {
		return fmt.Errorf("default runtime %q must be runc or one of the installed runtimes", a.DefaultRuntime)
	}
	return nil
}

func (a Addons) has(rt string) bool { return slices.Contains(a.Runtimes, rt) }

// RuntimeInstallScript は crun / youki をインストールするシェルスクリプトを返す (どのディストリでも共通)。
// crun は apt、youki は apt に無ければ GitHub の最新リリースから入れる。
func (a Addons) RuntimeInstallScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n# niwakube: 追加のコンテナランタイムのインストール\nset -euxo pipefail\nexport DEBIAN_FRONTEND=noninteractive\n")
	if a.has(RuntimeCrun) {
		b.WriteString("apt-get update -q\napt-get install -y -q crun\n")
	}
	if a.has(RuntimeYouki) {
		b.WriteString(youkiInstall)
	}
	return b.String()
}

const youkiInstall = `if apt-cache show youki >/dev/null 2>&1; then
  apt-get install -y -q youki
  ln -sf "$(command -v youki)" /usr/local/bin/youki 2>/dev/null || true
else
  arch=$(uname -m)
  url=""
  # GitHub API (認証なしは IP ごとにレート制限があり、CI では失敗することがある) を何度か試す
  for i in 1 2 3 4 5; do
    url=$(curl -fsSL https://api.github.com/repos/youki-dev/youki/releases/latest 2>/dev/null \
      | grep -o '"browser_download_url": *"[^"]*"' | cut -d'"' -f4 \
      | grep -E "${arch}.*gnu.*\.tar\.gz$" | head -n1 || true)
    [ -n "$url" ] && break
    sleep $((i * 5))
  done
  # API が使えなければ、latest のリダイレクト先からタグを調べて、よくあるファイル名を試す
  if [ -z "$url" ]; then
    tag=$(curl -fsSIL -o /dev/null -w '%{url_effective}' https://github.com/youki-dev/youki/releases/latest | sed 's#.*/tag/##')
    v=${tag#v}
    for name in "youki-${v}-${arch}-gnu.tar.gz" "youki-${v}-${arch}-unknown-linux-gnu.tar.gz" "youki_${v}_linux-${arch}.tar.gz"; do
      candidate="https://github.com/youki-dev/youki/releases/download/${tag}/${name}"
      if curl -fsIL -o /dev/null "$candidate"; then
        url=$candidate
        break
      fi
    done
  fi
  [ -n "$url" ] || { echo "youki のリリースが見つかりません ($arch)"; exit 1; }
  echo "youki: $url"
  tmp=$(mktemp -d)
  curl -fsSL "$url" | tar -xz -C "$tmp"
  install -m 0755 "$(find "$tmp" -type f -name youki | head -n1)" /usr/local/bin/youki
  rm -rf "$tmp"
fi
/usr/local/bin/youki --version
`

// RuntimeBinary は containerd に登録するときのランタイムのバイナリのパス。
func RuntimeBinary(rt string) string {
	switch rt {
	case RuntimeCrun:
		return "/usr/bin/crun"
	case RuntimeYouki:
		return "/usr/local/bin/youki"
	default:
		return "runc"
	}
}

// RuntimeClassManifest は入れたランタイムの RuntimeClass (handler は同じ名前) を返す。runc の分も作る。
func (a Addons) RuntimeClassManifest() string {
	var b strings.Builder
	for _, rt := range append([]string{RuntimeRunc}, a.Runtimes...) {
		fmt.Fprintf(&b, "---\napiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata:\n  name: %s\n  labels:\n    app.kubernetes.io/managed-by: niwakube\nhandler: %s\n", rt, rt)
	}
	return b.String()
}

// CiliumInstallScript は CP で Cilium を入れるスクリプトを返す。
// kube-proxy は使わず Cilium (eBPF) で置き換えるので、API サーバーの場所を直接渡す。
// Pod の IP はノードの podCIDR から割り当てる (ipam.mode=kubernetes)。
func (a Addons) CiliumInstallScript(kubeconfig, apiHost string, apiPort int) string {
	sets := []string{
		"ipam.mode=kubernetes",
		// LoadBalancer: LB IPAM で割り当てた IP を L2 アナウンス (ARP) で広告する
		"l2announcements.enabled=true",
		"externalIPs.enabled=true",
		"k8sClientRateLimit.qps=50",
		"k8sClientRateLimit.burst=100",
		"kubeProxyReplacement=true",
		"k8sServiceHost=" + apiHost,
		fmt.Sprintf("k8sServicePort=%d", apiPort),
	}
	if a.Hubble {
		sets = append(sets, "hubble.relay.enabled=true", "hubble.ui.enabled=true")
	}
	var args strings.Builder
	for _, s := range sets {
		args.WriteString(" --set " + s)
	}
	return fmt.Sprintf(`#!/bin/bash
# niwakube: Cilium のインストール
set -euxo pipefail
export KUBECONFIG=%s
arch=$(dpkg --print-architecture)
tarball=cilium-linux-${arch}.tar.gz
tmp=$(mktemp -d)
cd "$tmp"
curl -fsSLO "https://github.com/cilium/cilium-cli/releases/latest/download/${tarball}"
curl -fsSLO "https://github.com/cilium/cilium-cli/releases/latest/download/${tarball}.sha256sum"
sha256sum --check "${tarball}.sha256sum"
tar -xzf "$tarball" -C /usr/local/bin cilium
cd /
rm -rf "$tmp"

until kubectl get --raw /readyz >/dev/null 2>&1; do sleep 5; done
for i in $(seq 1 10); do
  kubectl -n kube-system get daemonset cilium >/dev/null 2>&1 && break
  cilium install%s && break
  sleep 15
done
cilium status --wait --wait-duration 15m
`, kubeconfig, args.String())
}
