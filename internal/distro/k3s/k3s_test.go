package k3s

import (
	"net/netip"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/distro"
)

func TestUserData(t *testing.T) {
	k := New()
	sec, err := k.GenerateSecrets("demo")
	if err != nil {
		t.Fatal(err)
	}
	c := &distro.Cluster{Name: "demo", Version: "v1.34.2+k3s1", APIAddr: netip.MustParseAddr("172.30.1.11"), Secrets: sec,
		Addons: distro.Addons{CNI: distro.CNIFlannel, DefaultRuntime: distro.RuntimeRunc}}

	cp, err := k.UserData(c, &distro.Node{Name: "demo-cp-1", Role: distro.RoleControlPlane, Index: 1, IP: netip.MustParseAddr("172.30.1.11")})
	if err != nil {
		t.Fatal(err)
	}
	files := cp["write_files"].([]writeFile)
	paths := map[string]writeFile{}
	for _, f := range files {
		paths[f.Path] = f
	}
	for _, p := range []string{
		tlsDir + "/server-ca.crt", tlsDir + "/server-ca.key", tlsDir + "/client-ca.key",
		tlsDir + "/request-header-ca.crt", tlsDir + "/etcd/peer-ca.crt", tlsDir + "/etcd/server-ca.key",
		tlsDir + "/service.key", "/etc/rancher/k3s/config.yaml",
	} {
		if _, ok := paths[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	if paths[tlsDir+"/server-ca.key"].Permissions != "0600" {
		t.Error("CA key must be 0600")
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(paths["/etc/rancher/k3s/config.yaml"].Content), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != sec.Token || cfg["node-ip"] != "172.30.1.11" || cfg["server"] != nil {
		t.Errorf("server config: %v", cfg)
	}
	cmd := strings.Join(cp["runcmd"].([]any)[0].([]string), " ")
	if !strings.Contains(cmd, "INSTALL_K3S_VERSION=v1.34.2+k3s1") || !strings.HasSuffix(cmd, "sh -s - server") {
		t.Errorf("server install: %s", cmd)
	}

	w, err := k.UserData(c, &distro.Node{Name: "demo-worker-1", Role: distro.RoleWorker, Index: 1, IP: netip.MustParseAddr("172.30.1.21")})
	if err != nil {
		t.Fatal(err)
	}
	wfiles := w["write_files"].([]writeFile)
	if len(wfiles) != 1 {
		t.Fatalf("worker must not receive CA files: %d files", len(wfiles))
	}
	if err := yaml.Unmarshal([]byte(wfiles[0].Content), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["server"] != "https://172.30.1.11:6443" || cfg["node-ip"] != "172.30.1.21" {
		t.Errorf("agent config: %v", cfg)
	}
	if cmd := strings.Join(w["runcmd"].([]any)[0].([]string), " "); !strings.HasSuffix(cmd, "sh -s - agent") {
		t.Errorf("agent install: %s", cmd)
	}

	if _, err := k.UserData(c, &distro.Node{Role: distro.RoleControlPlane, Index: 2}); err == nil {
		t.Error("second control plane should be rejected for now")
	}
}

func TestValidateVersion(t *testing.T) {
	k := New()
	for _, v := range []string{"", "stable", "latest", "v1.34.2+k3s1"} {
		if err := k.ValidateVersion(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"1.34", "v1.34.2", "v1.34.2+k3s1; rm -rf /"} {
		if err := k.ValidateVersion(v); err == nil {
			t.Errorf("%q should be invalid", v)
		}
	}
}

func TestUserDataCiliumAndRuntimes(t *testing.T) {
	k := New()
	sec, _ := k.GenerateSecrets("demo")
	c := &distro.Cluster{Name: "demo", APIAddr: netip.MustParseAddr("172.30.1.11"), Secrets: sec,
		Addons: distro.Addons{CNI: distro.CNICilium, Hubble: true, DefaultRuntime: distro.RuntimeYouki, Runtimes: []string{distro.RuntimeCrun, distro.RuntimeYouki}}}

	cp, err := k.UserData(c, &distro.Node{Name: "demo-cp-1", Role: distro.RoleControlPlane, Index: 1, IP: netip.MustParseAddr("172.30.1.11")})
	if err != nil {
		t.Fatal(err)
	}
	fs := map[string]writeFile{}
	for _, f := range cp["write_files"].([]writeFile) {
		fs[f.Path] = f
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(fs["/etc/rancher/k3s/config.yaml"].Content), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["flannel-backend"] != "none" || cfg["disable-kube-proxy"] != true || cfg["disable-network-policy"] != true || cfg["default-runtime"] != "youki" {
		t.Errorf("server config for cilium: %v", cfg)
	}
	if !strings.Contains(fs[containerdTmpl].Content, "runtimes.youki") || !strings.Contains(fs[containerdTmpl].Content, `{{ template "base" . }}`) {
		t.Error("youki must be added through the containerd template")
	}
	if !strings.Contains(fs[manifestsDir+"/niwakube-runtimeclasses.yaml"].Content, "handler: youki") {
		t.Error("RuntimeClasses must be deployed through the manifests directory")
	}
	if !strings.Contains(fs[ciliumScript].Content, "KUBECONFIG=/etc/rancher/k3s/k3s.yaml") {
		t.Error("cilium script must use the k3s kubeconfig")
	}
	// 実行順: ランタイム → k3s → Cilium
	var cmds []string
	for _, c := range cp["runcmd"].([]any) {
		cmds = append(cmds, strings.Join(c.([]string), " "))
	}
	if len(cmds) != 3 || !strings.Contains(cmds[0], runtimeScript) || !strings.Contains(cmds[1], "get.k3s.io") || !strings.Contains(cmds[2], ciliumScript) {
		t.Errorf("runcmd order: %v", cmds)
	}

	// ワーカーにはランタイムと default-runtime は入るが、Cilium のインストールや flannel の設定は入らない
	w, err := k.UserData(c, &distro.Node{Name: "demo-worker-1", Role: distro.RoleWorker, Index: 1, IP: netip.MustParseAddr("172.30.1.21")})
	if err != nil {
		t.Fatal(err)
	}
	var wcfg map[string]any
	for _, f := range w["write_files"].([]writeFile) {
		if f.Path == "/etc/rancher/k3s/config.yaml" {
			_ = yaml.Unmarshal([]byte(f.Content), &wcfg)
		}
		if f.Path == ciliumScript {
			t.Error("worker must not install cilium")
		}
	}
	if wcfg["default-runtime"] != "youki" || wcfg["flannel-backend"] != nil {
		t.Errorf("agent config: %v", wcfg)
	}
}

func TestUserDataContainer(t *testing.T) {
	k := New()
	sec, err := k.GenerateSecrets("demo")
	if err != nil {
		t.Fatal(err)
	}
	c := &distro.Cluster{Name: "demo", APIAddr: netip.MustParseAddr("172.30.1.11"), Secrets: sec, Container: true,
		Addons: distro.Addons{CNI: distro.CNIFlannel, DefaultRuntime: distro.RuntimeRunc}}
	for _, n := range []*distro.Node{
		{Name: "demo-cp-1", Role: distro.RoleControlPlane, Index: 1, IP: netip.MustParseAddr("172.30.1.11")},
		{Name: "demo-worker-1", Role: distro.RoleWorker, Index: 1, IP: netip.MustParseAddr("172.30.1.21")},
	} {
		ud, err := k.UserData(c, n)
		if err != nil {
			t.Fatal(err)
		}
		var cfg string
		for _, f := range ud["write_files"].([]writeFile) {
			if f.Path == "/etc/rancher/k3s/config.yaml" {
				cfg = f.Content
			}
		}
		for _, want := range []string{"conntrack-max-per-core=0", "eviction-hard="} {
			if !strings.Contains(cfg, want) {
				t.Errorf("%s: config.yaml does not contain %q:\n%s", n.Name, want, cfg)
			}
		}
	}
	// VM のときは入れない
	c.Container = false
	ud, _ := k.UserData(c, &distro.Node{Name: "demo-cp-1", Role: distro.RoleControlPlane, Index: 1, IP: netip.MustParseAddr("172.30.1.11")})
	for _, f := range ud["write_files"].([]writeFile) {
		if f.Path == "/etc/rancher/k3s/config.yaml" && strings.Contains(f.Content, "conntrack") {
			t.Errorf("VM config must not have container settings:\n%s", f.Content)
		}
	}
}
