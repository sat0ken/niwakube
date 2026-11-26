// Package kubeadm は kubeadm で素の Kubernetes クラスタを作る Distro。
//
// CA (cluster / front-proxy / etcd)、ServiceAccount の鍵、ブートストラップトークンは niwakubed が作り、
// cloud-init で CP の /etc/kubernetes/pki に置いてから kubeadm init する。
// CA が先に決まっているので、ワーカーの join に必要な CA の hash も最初から分かる。
// CNI は flannel (Pod CIDR 10.244.0.0/16)。
package kubeadm

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/pki"
)

const (
	Name        = "kubeadm"
	podSubnet   = "10.244.0.0/16"
	svcSubnet   = "10.96.0.0/12"
	apiPort     = 6443
	pkiDir      = "/etc/kubernetes/pki"
	flannelURL  = "https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml"
	installPath = "/usr/local/sbin/niwakube-install-k8s.sh"
	runtimePath = "/usr/local/sbin/niwakube-install-runtimes.sh"
	ciliumPath  = "/usr/local/sbin/niwakube-install-cilium.sh"

	runtimeClassPath = "/etc/kubernetes/niwakube-runtimeclasses.yaml"
	initPath         = "/usr/local/sbin/niwakube-kubeadm.sh"
	configPath       = "/etc/kubernetes/niwakube-kubeadm.yaml"
)

// Secrets.CAs のキー → pkiDir からの相対パス
var caFiles = map[string]string{
	"ca":             "ca",
	"front-proxy-ca": "front-proxy-ca",
	"etcd-ca":        "etcd/ca",
}

var (
	minorRe = regexp.MustCompile(`^v\d+\.\d+$`)
	patchRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

type Kubeadm struct{}

var _ distro.Distro = Kubeadm{}

func New() Kubeadm { return Kubeadm{} }

func (Kubeadm) Name() string                      { return Name }
func (Kubeadm) DefaultVersion() string            { return "stable" }
func (Kubeadm) Requirements() distro.Requirements { return distro.Requirements{} }
func (Kubeadm) Image(string) string               { return "ubuntu-26.04" }
func (Kubeadm) AdminCAs() (server, client string) { return "ca", "ca" }
func (Kubeadm) APIPort() int                      { return apiPort }

func (Kubeadm) ValidateVersion(v string) error {
	if v == "" || v == "stable" || minorRe.MatchString(v) || patchRe.MatchString(v) {
		return nil
	}
	return fmt.Errorf("kubeadm version must be like v1.37 (latest patch), v1.37.2 or \"stable\": %q", v)
}

func (Kubeadm) GenerateSecrets(clusterName string) (*distro.Secrets, error) {
	s := &distro.Secrets{CAs: map[string]pki.KeyPair{}}
	var err error
	if s.Token, err = pki.BootstrapToken(); err != nil {
		return nil, err
	}
	for name := range caFiles {
		if s.CAs[name], err = pki.NewCA(fmt.Sprintf("niwakube-%s-%s", clusterName, name)); err != nil {
			return nil, err
		}
	}
	if s.ServiceAccountKey, err = pki.NewRSAKey(2048); err != nil {
		return nil, err
	}
	return s, nil
}

type writeFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

func (k Kubeadm) UserData(c *distro.Cluster, n *distro.Node) (map[string]any, error) {
	if c.Secrets == nil || c.Secrets.Token == "" {
		return nil, fmt.Errorf("cluster %s has no secrets", c.Name)
	}
	if err := k.ValidateVersion(c.Version); err != nil {
		return nil, err
	}
	if err := c.Addons.Validate(); err != nil {
		return nil, err
	}
	install, err := render(installScript, map[string]any{"Version": c.Version, "Containerd": containerdRuntimes(c.Addons), "Container": c.Container})
	if err != nil {
		return nil, err
	}
	files := []writeFile{{Path: installPath, Permissions: "0755", Content: install}}
	runcmd := []any{}
	if len(c.Addons.Runtimes) > 0 {
		// containerd に登録する前にランタイムを入れておく
		files = append(files, writeFile{Path: runtimePath, Permissions: "0755", Content: c.Addons.RuntimeInstallScript()})
		runcmd = append(runcmd, []string{"/bin/bash", runtimePath})
	}
	runcmd = append(runcmd, []string{"/bin/bash", installPath}, []string{"/bin/bash", initPath})
	endpoint := fmt.Sprintf("%s:%d", c.APIAddr, apiPort)

	var cfg string
	switch n.Role {
	case distro.RoleControlPlane:
		if n.Index != 1 {
			return nil, fmt.Errorf("kubeadm: only one control plane is supported for now")
		}
		for name, rel := range caFiles {
			ca, ok := c.Secrets.CAs[name]
			if !ok {
				return nil, fmt.Errorf("kubeadm: CA %s is missing", name)
			}
			files = append(files,
				writeFile{Path: pkiDir + "/" + rel + ".crt", Permissions: "0644", Content: ca.CertPEM},
				writeFile{Path: pkiDir + "/" + rel + ".key", Permissions: "0600", Content: ca.KeyPEM},
			)
		}
		pub, err := pki.PublicKeyPEM(c.Secrets.ServiceAccountKey)
		if err != nil {
			return nil, fmt.Errorf("kubeadm: service account key: %w", err)
		}
		files = append(files,
			writeFile{Path: pkiDir + "/sa.key", Permissions: "0600", Content: c.Secrets.ServiceAccountKey},
			writeFile{Path: pkiDir + "/sa.pub", Permissions: "0644", Content: pub},
		)
		if cfg, err = initConfig(c, n, endpoint); err != nil {
			return nil, err
		}
	case distro.RoleWorker:
		hash, err := pki.CACertHash(c.Secrets.CAs["ca"].CertPEM)
		if err != nil {
			return nil, fmt.Errorf("kubeadm: CA hash: %w", err)
		}
		if cfg, err = joinConfig(c, n, endpoint, hash); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown role %q", n.Role)
	}

	if n.Role == distro.RoleControlPlane {
		if len(c.Addons.Runtimes) > 0 {
			files = append(files, writeFile{Path: runtimeClassPath, Permissions: "0644", Content: c.Addons.RuntimeClassManifest()})
		}
		if c.Addons.CNI == distro.CNICilium {
			files = append(files, writeFile{Path: ciliumPath, Permissions: "0755",
				Content: c.Addons.CiliumInstallScript("/etc/kubernetes/admin.conf", c.APIAddr.String(), apiPort)})
		}
	}
	script, err := render(bootstrapScript, map[string]any{
		"Role": string(n.Role), "Config": configPath, "Endpoint": endpoint, "Flannel": flannelURL, "Container": c.Container,
		"CNI": c.Addons.CNI, "Cilium": ciliumPath, "RuntimeClasses": len(c.Addons.Runtimes) > 0, "RuntimeClassPath": runtimeClassPath,
	})
	if err != nil {
		return nil, err
	}
	files = append(files,
		writeFile{Path: configPath, Permissions: "0600", Content: cfg},
		writeFile{Path: initPath, Permissions: "0755", Content: script},
	)
	return map[string]any{
		"write_files": files,
		"runcmd":      runcmd,
	}, nil
}

func nodeRegistration(n *distro.Node) map[string]any {
	return map[string]any{
		"name":             n.Name,
		"criSocket":        "unix:///run/containerd/containerd.sock",
		"kubeletExtraArgs": []map[string]string{{"name": "node-ip", "value": n.IP.String()}},
	}
}

func marshalDocs(docs ...any) (string, error) {
	var buf bytes.Buffer
	for i, d := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		b, err := yaml.Marshal(d)
		if err != nil {
			return "", err
		}
		buf.Write(b)
	}
	return buf.String(), nil
}

// initConfig は kubeadm init の設定。kubernetesVersion はインストールした kubeadm のバージョンに
// 合わせるため、ブートストラップスクリプトが __KUBERNETES_VERSION__ を置き換える。
func initConfig(c *distro.Cluster, n *distro.Node, endpoint string) (string, error) {
	vip := c.APIAddr.Prev() // .10 (将来の VIP 用。証明書の SAN にだけ入れておく)
	docs := []any{
		map[string]any{
			"apiVersion": "kubeadm.k8s.io/v1beta4",
			"kind":       "InitConfiguration",
			"bootstrapTokens": []map[string]any{{
				"token":  c.Secrets.Token,
				"ttl":    "0s", // 期限なし (あとからワーカーを足せるように)
				"usages": []string{"signing", "authentication"},
				"groups": []string{"system:bootstrappers:kubeadm:default-node-token"},
			}},
			"localAPIEndpoint": map[string]any{"advertiseAddress": n.IP.String(), "bindPort": apiPort},
			"nodeRegistration": nodeRegistration(n),
			"skipPhases":       skipPhases(c.Addons),
		},
		map[string]any{
			"apiVersion":           "kubeadm.k8s.io/v1beta4",
			"kind":                 "ClusterConfiguration",
			"clusterName":          c.Name,
			"kubernetesVersion":    "__KUBERNETES_VERSION__",
			"controlPlaneEndpoint": endpoint,
			"apiServer":            map[string]any{"certSANs": []string{n.IP.String(), c.APIAddr.String(), vip.String()}},
			"networking":           map[string]any{"podSubnet": podSubnet, "serviceSubnet": svcSubnet},
		},
	}
	return marshalDocs(append(docs, containerDocs(c)...)...)
}

// containerDocs はコンテナの中で動かすときの kube-proxy と kubelet の設定 (kind と同じ調整)。
func containerDocs(c *distro.Cluster) []any {
	if !c.Container {
		return nil
	}
	return []any{
		map[string]any{
			"apiVersion": "kubeproxy.config.k8s.io/v1alpha1",
			"kind":       "KubeProxyConfiguration",
			// conntrack の上限はホスト全体の設定なので変えない
			"conntrack": map[string]any{"maxPerCore": 0},
		},
		map[string]any{
			"apiVersion": "kubelet.config.k8s.io/v1beta1",
			"kind":       "KubeletConfiguration",
			// ディスクはホストと共有なので、ディスク不足による追い出しを止める
			"evictionHard": map[string]string{"imagefs.available": "0%", "nodefs.available": "0%", "nodefs.inodesFree": "0%"},
			"failSwapOn":   false,
		},
	}
}

func joinConfig(c *distro.Cluster, n *distro.Node, endpoint, caHash string) (string, error) {
	return marshalDocs(map[string]any{
		"apiVersion": "kubeadm.k8s.io/v1beta4",
		"kind":       "JoinConfiguration",
		"discovery": map[string]any{
			"bootstrapToken": map[string]any{
				"apiServerEndpoint": endpoint,
				"token":             c.Secrets.Token,
				"caCertHashes":      []string{caHash},
			},
		},
		"timeouts":         map[string]any{"discovery": "15m0s"},
		"nodeRegistration": nodeRegistration(n),
	})
}

func render(tmpl string, data any) (string, error) {
	t, err := template.New("").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// installScript は containerd と kubelet / kubeadm / kubectl を入れる (CP とワーカー共通)。
const installScript = `#!/bin/bash
# niwakube: containerd と kubeadm のインストール
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
VERSION="{{.Version}}"

cat >/etc/modules-load.d/k8s.conf <<EOF
overlay
br_netfilter
EOF
modprobe overlay{{if .Container}} || true{{end}}
modprobe br_netfilter{{if .Container}} || true{{end}}
cat >/etc/sysctl.d/90-k8s.conf <<EOF
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system >/dev/null{{if .Container}} || true # コンテナの中では書けない項目がある (ホストで設定済み){{end}}
swapoff -a

apt-get update -q
apt-get install -y -q containerd apt-transport-https ca-certificates curl gpg
mkdir -p /etc/containerd
containerd config default >/etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
{{.Containerd}}systemctl restart containerd
systemctl enable containerd

# バージョン: stable → dl.k8s.io の stable、v1.X → その minor の最新、v1.X.Y → 固定
case "$VERSION" in
  ""|stable) FULL=$(curl -fsSL https://dl.k8s.io/release/stable.txt); MINOR=${FULL%.*}; PIN="" ;;
  v*.*.*)    MINOR=${VERSION%.*}; PIN="=${VERSION#v}-*" ;;
  *)         MINOR=$VERSION; PIN="" ;;
esac

mkdir -p /etc/apt/keyrings
curl -fsSL "https://pkgs.k8s.io/core:/stable:/${MINOR}/deb/Release.key" | gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${MINOR}/deb/ /" >/etc/apt/sources.list.d/kubernetes.list
apt-get update -q
apt-get install -y -q "kubelet${PIN}" "kubeadm${PIN}" "kubectl${PIN}"
apt-mark hold kubelet kubeadm kubectl
systemctl enable kubelet
`

// bootstrapScript は CP なら kubeadm init + CNI (Cilium か flannel)、ワーカーなら API を待って kubeadm join する。
const bootstrapScript = `#!/bin/bash
# niwakube: kubeadm init / join
set -euxo pipefail
ROLE="{{.Role}}"
CONFIG="{{.Config}}"

if [ "$ROLE" = "control-plane" ]; then
  sed -i "s/__KUBERNETES_VERSION__/$(kubeadm version -o short)/" "$CONFIG"
  kubeadm init --config "$CONFIG"{{if .Container}} --ignore-preflight-errors=all{{end}}
  export KUBECONFIG=/etc/kubernetes/admin.conf
{{- if .RuntimeClasses}}
  kubectl apply -f "{{.RuntimeClassPath}}"
{{- end}}
{{- if eq .CNI "cilium"}}
  /bin/bash "{{.Cilium}}"
{{- else}}
  for i in $(seq 1 30); do
    kubectl apply -f "{{.Flannel}}" && break
    sleep 10
  done
{{- end}}
else
  # CP の API が上がるまで待ってから join する (CP 側もパッケージのインストール中)
  until curl -ks --max-time 5 -o /dev/null "https://{{.Endpoint}}/livez"; do sleep 5; done
  kubeadm join --config "$CONFIG"{{if .Container}} --ignore-preflight-errors=all{{end}}
fi
`

// skipPhases は kubeadm init で飛ばすフェーズ。Cilium は kube-proxy を置き換えるので入れない。
func skipPhases(a distro.Addons) []string {
	if a.CNI == distro.CNICilium {
		return []string{"addon/kube-proxy"}
	}
	return []string{}
}

// containerdRuntimes は containerd の設定に crun / youki を登録し、既定のランタイムを変えるシェルの断片を返す。
// containerd 2.x (設定のバージョン 3) が前提。
func containerdRuntimes(a distro.Addons) string {
	if len(a.Runtimes) == 0 && (a.DefaultRuntime == "" || a.DefaultRuntime == distro.RuntimeRunc) {
		return ""
	}
	var b strings.Builder
	b.WriteString("grep -q '^version = 3' /etc/containerd/config.toml || { echo 'containerd の設定のバージョンが 3 ではありません'; exit 1; }\n")
	for _, rt := range a.Runtimes {
		fmt.Fprintf(&b, `cat >>/etc/containerd/config.toml <<'EOF'

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.%[1]s]
  runtime_type = 'io.containerd.runc.v2'

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.%[1]s.options]
  BinaryName = '%[2]s'
  SystemdCgroup = true
EOF
`, rt, distro.RuntimeBinary(rt))
	}
	if rt := a.DefaultRuntime; rt != "" && rt != distro.RuntimeRunc {
		fmt.Fprintf(&b, "sed -i -E \"s/^([[:space:]]*default_runtime_name = )['\\\"][^'\\\"]*['\\\"]/\\1'%[1]s'/\" /etc/containerd/config.toml\n", rt)
		fmt.Fprintf(&b, "grep -q \"default_runtime_name = '%s'\" /etc/containerd/config.toml\n", rt)
	}
	return b.String()
}
