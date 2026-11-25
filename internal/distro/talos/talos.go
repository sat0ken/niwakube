// Package talos は Talos Linux のクラスタを作る Distro。
//
// Talos は SSH もシェルも無く、API だけで操作する OS。niwakube では次のようにする。
//   - イメージ: Image Factory の nocloud イメージ (host-setup.sh が images/talos-<version>.raw に置く)
//   - 設定: niwakubed が Talos の machinery ライブラリで machine config を生成し、
//     NoCloud の user-data としてそのまま渡す (固定 IP とホスト名は network-config / meta-data で渡る)
//   - 初期化: CP が起動したら niwakubed が Talos API で etcd を bootstrap する
//   - kubeconfig: secrets bundle の Kubernetes CA で admin 証明書に署名して発行する (k3s / kubeadm と同じ)
//   - Cilium: Talos にはシェルが無いので、inline manifest の Job で cilium CLI を動かしてインストールする
package talos

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config"
	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/version"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/pki"
)

const (
	Name    = "talos"
	apiPort = 6443
	// k8sCA は Secrets.CAs に入れる Kubernetes CA のキー。
	k8sCA = "k8s-ca"
	// bundleKey は Secrets.Extra に入れる secrets bundle (YAML) のキー。
	bundleKey = "bundle"
	// installDisk は VM の中から見たシステムディスク (virtio-blk)。
	installDisk = "/dev/vda"
	// ciliumCLIImage は Cilium をインストールする Job のイメージ。
	ciliumCLIImage = "quay.io/cilium/cilium-cli-ci:latest"
)

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

type Talos struct{}

var (
	_ distro.Distro               = Talos{}
	_ distro.RawUserDataProvider  = Talos{}
	_ distro.Bootstrapper         = Talos{}
	_ distro.ClientConfigProvider = Talos{}
)

func New() Talos { return Talos{} }

func (Talos) Name() string { return Name }

// DefaultVersion は niwakube に組み込んだ machinery と同じ Talos のバージョン (例: v1.14.7)。
func (Talos) DefaultVersion() string { return version.Tag }

func (Talos) ValidateVersion(v string) error {
	if v == "" || versionRe.MatchString(v) {
		return nil
	}
	return fmt.Errorf("talos version must be like %s: %q", version.Tag, v)
}

func (Talos) Requirements() distro.Requirements {
	// 専用のカーネルが必要。コンテナランタイムは Talos に組み込まれた runc のみ
	return distro.Requirements{NeedsOwnKernel: true, OnlyRuntimes: []string{distro.RuntimeRunc}}
}

func (t Talos) Image(v string) string {
	if v == "" {
		v = t.DefaultVersion()
	}
	return "talos-" + v
}

func (Talos) AdminCAs() (server, client string) { return k8sCA, k8sCA }
func (Talos) APIPort() int                      { return apiPort }

// UserData は使わない (RawUserData を使う)。
func (Talos) UserData(*distro.Cluster, *distro.Node) (map[string]any, error) {
	return nil, errors.New("talos uses RawUserData")
}

func (Talos) GenerateSecrets(string) (*distro.Secrets, error) {
	bundle, err := secrets.NewBundle(secrets.NewClock(), config.TalosVersionCurrent)
	if err != nil {
		return nil, err
	}
	raw, err := yaml.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	return &distro.Secrets{
		CAs: map[string]pki.KeyPair{
			k8sCA: {CertPEM: string(bundle.Certs.K8s.Crt), KeyPEM: string(bundle.Certs.K8s.Key)},
		},
		Extra: map[string]string{bundleKey: string(raw)},
	}, nil
}

func loadBundle(c *distro.Cluster) (*secrets.Bundle, error) {
	if c.Secrets == nil || c.Secrets.Extra[bundleKey] == "" {
		return nil, fmt.Errorf("cluster %s has no talos secrets bundle", c.Name)
	}
	b := &secrets.Bundle{Clock: secrets.NewClock()}
	if err := yaml.Unmarshal([]byte(c.Secrets.Extra[bundleKey]), b); err != nil {
		return nil, fmt.Errorf("parse talos secrets bundle: %w", err)
	}
	return b, nil
}

func (t Talos) input(c *distro.Cluster) (*generate.Input, error) {
	v := c.Version
	if v == "" {
		v = t.DefaultVersion()
	}
	contract, err := config.ParseContractFromVersion(v)
	if err != nil {
		return nil, err
	}
	bundle, err := loadBundle(c)
	if err != nil {
		return nil, err
	}
	return generate.NewInput(c.Name, fmt.Sprintf("https://%s:%d", c.APIAddr, apiPort), constants.DefaultKubernetesVersion,
		generate.WithSecretsBundle(bundle),
		generate.WithVersionContract(contract),
		generate.WithInstallDisk(installDisk),
		// nocloud のイメージはインストール済みのディスクとして起動するので、自動インストールの設定は要らない
		generate.WithSkipUnattendedInstallConfig(true),
		generate.WithEndpointList([]string{c.APIAddr.String()}),
		// .10 は将来の VIP 用。証明書の SAN にだけ入れておく
		generate.WithAdditionalSubjectAltNames([]string{c.APIAddr.String(), c.APIAddr.Prev().String()}),
	)
}

// RawUserData はノードの machine config を返す。
func (t Talos) RawUserData(c *distro.Cluster, n *distro.Node) ([]byte, error) {
	if err := c.Addons.Validate(); err != nil {
		return nil, err
	}
	in, err := t.input(c)
	if err != nil {
		return nil, err
	}
	typ := machine.TypeWorker
	if n.Role == distro.RoleControlPlane {
		if n.Index != 1 {
			return nil, fmt.Errorf("talos: only one control plane is supported for now")
		}
		typ = machine.TypeControlPlane
	}
	provider, err := in.Config(typ)
	if err != nil {
		return nil, err
	}
	docs, err := customize(provider.Documents(), c, n, typ == machine.TypeControlPlane)
	if err != nil {
		return nil, err
	}
	cfg, err := container.New(docs...)
	if err != nil {
		return nil, err
	}
	return cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
}

// customize は生成した machine config に niwakube の設定 (ホスト名、Cilium) を反映する。
// Talos 1.14 以降は Kubernetes の設定が種類ごとのドキュメントに分かれている。それより前は v1alpha1 の cluster: にある。
func customize(docs []configdoc.Document, c *distro.Cluster, n *distro.Node, controlPlane bool) ([]configdoc.Document, error) {
	for _, d := range docs {
		if h, ok := d.(*network.HostnameConfigV1Alpha1); ok {
			// 自動生成 (talos-xxxx) ではなく niwakube のノード名にする
			h.ConfigAuto = nil
			h.ConfigHostname = n.Name
		}
	}
	if c.Addons.CNI != distro.CNICilium {
		return docs, nil // flannel は Talos の既定のまま
	}
	no := false
	yes := true
	var out []configdoc.Document
	multidoc := false
	for _, d := range docs {
		switch v := d.(type) {
		case *k8s.KubeFlannelCNIConfigV1Alpha1:
			multidoc = true
			continue // flannel を入れない
		case *k8s.KubeProxyConfigV1Alpha1:
			multidoc = true
			v.ProxyEnabled = &no // kube-proxy は Cilium で置き換える
		case *v1alpha1.Config:
			if v.ClusterConfig != nil {
				if v.ClusterConfig.ClusterNetwork != nil {
					v.ClusterConfig.ClusterNetwork.CNI = &v1alpha1.CNIConfig{CNIName: "none"}
				}
			}
		}
		out = append(out, d)
	}
	if !controlPlane {
		return out, nil
	}
	manifest := ciliumInstallManifest(c.Addons)
	if multidoc {
		im := k8s.NewKubeInlineManifestConfigV1Alpha1()
		im.MetaName = "niwakube-cilium-install"
		im.ManifestSpec = manifest
		out = append(out, im)
		return out, nil
	}
	// 古い形式 (v1alpha1 の cluster:)
	for _, d := range out {
		if v, ok := d.(*v1alpha1.Config); ok && v.ClusterConfig != nil {
			v.ClusterConfig.ProxyConfig = &v1alpha1.ProxyConfig{Disabled: &yes}
			v.ClusterConfig.ClusterInlineManifests = append(v.ClusterConfig.ClusterInlineManifests,
				v1alpha1.ClusterInlineManifest{InlineManifestName: "niwakube-cilium-install", InlineManifestContents: manifest})
		}
	}
	return out, nil
}

// ciliumInstallManifest は Cilium をインストールする Job を返す。
// Talos の推奨設定 (cgroup は Talos がマウント済み、capabilities を絞る) と、KubePrism (localhost:7445) を使う。
func ciliumInstallManifest(a distro.Addons) string {
	args := []string{
		"install",
		"--set", "ipam.mode=kubernetes",
		"--set", "kubeProxyReplacement=true",
		"--set", "securityContext.capabilities.ciliumAgent={CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}",
		"--set", "securityContext.capabilities.cleanCiliumState={NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}",
		"--set", "cgroup.autoMount.enabled=false",
		"--set", "cgroup.hostRoot=/sys/fs/cgroup",
		"--set", "k8sServiceHost=localhost",
		"--set", "k8sServicePort=7445",
	}
	if a.Hubble {
		args = append(args, "--set", "hubble.relay.enabled=true", "--set", "hubble.ui.enabled=true")
	}
	var cmd strings.Builder
	for _, a := range append([]string{"cilium"}, args...) {
		fmt.Fprintf(&cmd, "            - %q\n", a)
	}
	return `apiVersion: v1
kind: ServiceAccount
metadata:
  name: niwakube-cilium-install
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: niwakube-cilium-install
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: niwakube-cilium-install
    namespace: kube-system
---
apiVersion: batch/v1
kind: Job
metadata:
  name: niwakube-cilium-install
  namespace: kube-system
spec:
  backoffLimit: 20
  template:
    metadata:
      labels:
        app: niwakube-cilium-install
    spec:
      restartPolicy: OnFailure
      serviceAccountName: niwakube-cilium-install
      hostNetwork: true
      tolerations:
        - operator: Exists
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: node-role.kubernetes.io/control-plane
                    operator: Exists
      containers:
        - name: cilium-install
          image: ` + ciliumCLIImage + `
          env:
            - name: KUBERNETES_SERVICE_HOST
              valueFrom:
                fieldRef:
                  apiVersion: v1
                  fieldPath: status.podIP
            - name: KUBERNETES_SERVICE_PORT
              value: "6443"
          command:
` + cmd.String()
}

// talosconfig は talosctl の設定を返す (CP の Talos API を向く)。
func (t Talos) talosconfig(c *distro.Cluster) ([]byte, error) {
	in, err := t.input(c)
	if err != nil {
		return nil, err
	}
	tc, err := in.Talosconfig()
	if err != nil {
		return nil, err
	}
	for _, ctx := range tc.Contexts {
		ctx.Endpoints = []string{c.APIAddr.String()}
		ctx.Nodes = []string{c.APIAddr.String()}
	}
	return tc.Bytes()
}

func (t Talos) ClientConfig(c *distro.Cluster) (string, []byte, error) {
	b, err := t.talosconfig(c)
	return c.Name + ".talosconfig", b, err
}

// Bootstrap は CP の Talos API で etcd を bootstrap する。初期化済みなら nil を返す。
func (t Talos) Bootstrap(ctx context.Context, c *distro.Cluster) error {
	raw, err := t.talosconfig(c)
	if err != nil {
		return err
	}
	tc, err := clientconfig.FromBytes(raw)
	if err != nil {
		return err
	}
	cl, err := client.New(ctx, client.WithConfig(tc), client.WithEndpoints(c.APIAddr.String()))
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck
	err = cl.Bootstrap(ctx, &machineapi.BootstrapRequest{})
	if err == nil || isAlreadyBootstrapped(err) {
		return nil
	}
	return err
}

func isAlreadyBootstrapped(err error) bool {
	if s, ok := status.FromError(err); ok && s.Code() == codes.AlreadyExists {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "AlreadyExists") || strings.Contains(msg, "etcd data directory is not empty")
}
