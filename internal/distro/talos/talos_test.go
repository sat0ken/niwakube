package talos

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/distro"
)

func docs(t *testing.T, b []byte) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		kind, _ := m["kind"].(string)
		if kind == "" {
			kind = "v1alpha1"
		}
		out[kind] = m
	}
}

func newCluster(t *testing.T, cni string) *distro.Cluster {
	t.Helper()
	sec, err := New().GenerateSecrets("demo")
	if err != nil {
		t.Fatal(err)
	}
	return &distro.Cluster{Name: "demo", APIAddr: netip.MustParseAddr("172.30.1.11"), Secrets: sec,
		Addons: distro.Addons{CNI: cni, Hubble: cni == distro.CNICilium, DefaultRuntime: distro.RuntimeRunc}}
}

var (
	cpNode = &distro.Node{Name: "demo-cp-1", Role: distro.RoleControlPlane, Index: 1, IP: netip.MustParseAddr("172.30.1.11")}
	wNode  = &distro.Node{Name: "demo-worker-1", Role: distro.RoleWorker, Index: 1, IP: netip.MustParseAddr("172.30.1.21")}
)

func TestMachineConfigCilium(t *testing.T) {
	k := New()
	c := newCluster(t, distro.CNICilium)

	b, err := k.RawUserData(c, cpNode)
	if err != nil {
		t.Fatal(err)
	}
	d := docs(t, b)
	if d["v1alpha1"]["machine"].(map[string]any)["type"] != "controlplane" {
		t.Errorf("machine type: %v", d["v1alpha1"]["machine"])
	}
	if d["KubeClusterConfig"]["endpoint"] != "https://172.30.1.11:6443" {
		t.Errorf("endpoint: %v", d["KubeClusterConfig"])
	}
	if d["HostnameConfig"]["hostname"] != "demo-cp-1" || d["HostnameConfig"]["auto"] != nil {
		t.Errorf("hostname: %v", d["HostnameConfig"])
	}
	if _, ok := d["KubeFlannelCNIConfig"]; ok {
		t.Error("flannel must be removed with cilium")
	}
	if uv := d["UserVolumeConfig"]; uv["name"] != localPathVolume || uv["volumeType"] != "directory" {
		t.Errorf("local-path user volume: %v", uv)
	}
	if New().LocalStorage().Dir != "/var/mnt/local-path-provisioner" {
		t.Errorf("local-path dir: %s", New().LocalStorage().Dir)
	}
	if _, ok := d["UnattendedInstallConfig"]; ok {
		t.Error("the nocloud image is already installed; no unattended install config")
	}
	if d["KubeProxyConfig"]["enabled"] != false {
		t.Errorf("kube-proxy must be disabled: %v", d["KubeProxyConfig"])
	}
	im := d["KubeInlineManifestConfig"]
	manifest, _ := im["manifest"].(string)
	for _, want := range []string{"kind: Job", "kubeProxyReplacement=true", "ipam.mode=kubernetes", "k8sServicePort=7445",
		"cgroup.autoMount.enabled=false", "hubble.ui.enabled=true", "hostNetwork: true", ciliumCLIImage} {
		if !strings.Contains(manifest, want) {
			t.Errorf("cilium install manifest lacks %q", want)
		}
	}

	// ワーカーには Cilium のインストール用マニフェストは入れない (CP で一度だけ)
	b, err = k.RawUserData(c, wNode)
	if err != nil {
		t.Fatal(err)
	}
	d = docs(t, b)
	if d["v1alpha1"]["machine"].(map[string]any)["type"] != "worker" || d["HostnameConfig"]["hostname"] != "demo-worker-1" {
		t.Errorf("worker: %v %v", d["v1alpha1"]["machine"], d["HostnameConfig"])
	}
	if _, ok := d["KubeInlineManifestConfig"]; ok {
		t.Error("worker must not carry the cilium install manifest")
	}

	if _, err := k.RawUserData(c, &distro.Node{Role: distro.RoleControlPlane, Index: 2}); err == nil {
		t.Error("second control plane should be rejected for now")
	}
}

func TestMachineConfigFlannel(t *testing.T) {
	d := docs(t, must(New().RawUserData(newCluster(t, distro.CNIFlannel), cpNode)))
	if _, ok := d["KubeFlannelCNIConfig"]; !ok {
		t.Error("flannel is Talos's default and must stay")
	}
	if _, ok := d["KubeInlineManifestConfig"]; ok {
		t.Error("no cilium manifest with flannel")
	}
	if d["HostnameConfig"]["hostname"] != "demo-cp-1" {
		t.Errorf("hostname: %v", d["HostnameConfig"])
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestSecretsAndKubeconfigCA(t *testing.T) {
	sec, err := New().GenerateSecrets("demo")
	if err != nil {
		t.Fatal(err)
	}
	ca := sec.CAs[k8sCA]
	// niwakube の pki で admin 証明書に署名できる (kubeconfig の発行に使う)
	if _, err := ca.SignClient("niwakube-admin", []string{"system:masters"}, time.Hour); err != nil {
		t.Fatalf("cannot sign with the Talos Kubernetes CA: %v", err)
	}
	// secrets bundle は保存して読み戻せる
	c := &distro.Cluster{Name: "demo", Secrets: sec}
	b, err := loadBundle(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(b.Certs.K8s.Crt) != ca.CertPEM {
		t.Error("bundle round trip changed the Kubernetes CA")
	}
}

func TestTalosconfig(t *testing.T) {
	c := newCluster(t, distro.CNICilium)
	name, b, err := New().ClientConfig(c, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo.talosconfig" || !strings.Contains(string(b), "172.30.1.11") || !strings.Contains(string(b), "context: demo") ||
		strings.Contains(string(b), "proxy-url") {
		t.Errorf("talosconfig %s:\n%s", name, b)
	}

	// SSH の SOCKS プロキシ経由用: endpoints はそのまま、各 context に proxy-url
	name, b, err = New().ClientConfig(c, "socks5://localhost:1080")
	if err != nil {
		t.Fatal(err)
	}
	tc, err := clientconfig.FromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo-ssh.talosconfig" || len(tc.Contexts) == 0 {
		t.Fatalf("talosconfig %s:\n%s", name, b)
	}
	for n, ctx := range tc.Contexts {
		if ctx.ProxyURL != "socks5://localhost:1080" || len(ctx.Endpoints) != 1 || ctx.Endpoints[0] != "172.30.1.11" {
			t.Errorf("context %s: %+v", n, ctx)
		}
	}
}

func TestVersionAndRequirements(t *testing.T) {
	k := New()
	if !versionRe.MatchString(k.DefaultVersion()) || k.Image("") != "talos-"+k.DefaultVersion() || k.Image("v1.13.0") != "talos-v1.13.0" {
		t.Errorf("version %s image %s", k.DefaultVersion(), k.Image(""))
	}
	for _, v := range []string{"1.14", "stable", "v1.14"} {
		if err := k.ValidateVersion(v); err == nil {
			t.Errorf("%q should be invalid", v)
		}
	}
	r := k.Requirements()
	// コンテナモードでも動くので専用のカーネルは要らない。ランタイムは runc のみ
	if r.NeedsOwnKernel || len(r.OnlyRuntimes) != 1 || r.OnlyRuntimes[0] != "runc" {
		t.Errorf("requirements: %+v", r)
	}
}

func TestIsAlreadyBootstrapped(t *testing.T) {
	if !isAlreadyBootstrapped(status.Error(codes.AlreadyExists, "etcd data directory is not empty")) {
		t.Error("AlreadyExists means bootstrapped")
	}
	if isAlreadyBootstrapped(errors.New("connection refused")) {
		t.Error("connection errors must be retried")
	}
}

func TestMachineConfigRegistryMirror(t *testing.T) {
	c := newCluster(t, distro.CNIFlannel)
	c.RegistryMirror = "http://172.30.0.1:5000"
	b := must(New().RawUserData(c, wNode))
	for _, want := range []string{"docker.io", "registry.k8s.io", "http://172.30.0.1:5000"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("machine config does not contain %q", want)
		}
	}
	c.RegistryMirror = ""
	if b := must(New().RawUserData(c, wNode)); bytes.Contains(b, []byte("172.30.0.1:5000")) {
		t.Error("no mirror without RegistryMirror")
	}
}
