package kubeadm

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/pki"
)

func decodeDocs(t *testing.T, s string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewBufferString(s))
	var out []map[string]any
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
}

func files(t *testing.T, ud map[string]any) map[string]writeFile {
	t.Helper()
	m := map[string]writeFile{}
	for _, f := range ud["write_files"].([]writeFile) {
		m[f.Path] = f
	}
	return m
}

func TestUserData(t *testing.T) {
	k := New()
	sec, err := k.GenerateSecrets("demo")
	if err != nil {
		t.Fatal(err)
	}
	c := &distro.Cluster{Name: "demo", Version: "v1.37", APIAddr: netip.MustParseAddr("172.30.1.11"), Secrets: sec}

	// コントロールプレーン
	cp, err := k.UserData(c, &distro.Node{Name: "demo-cp-1", Role: distro.RoleControlPlane, Index: 1, IP: netip.MustParseAddr("172.30.1.11")})
	if err != nil {
		t.Fatal(err)
	}
	fs := files(t, cp)
	for _, p := range []string{
		pkiDir + "/ca.crt", pkiDir + "/ca.key", pkiDir + "/front-proxy-ca.crt", pkiDir + "/front-proxy-ca.key",
		pkiDir + "/etcd/ca.crt", pkiDir + "/etcd/ca.key", pkiDir + "/sa.key", pkiDir + "/sa.pub",
		configPath, installPath, initPath,
	} {
		if _, ok := fs[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	if fs[pkiDir+"/ca.key"].Permissions != "0600" || fs[pkiDir+"/sa.key"].Permissions != "0600" {
		t.Error("private keys must be 0600")
	}
	if !strings.Contains(fs[pkiDir+"/sa.pub"].Content, "BEGIN PUBLIC KEY") {
		t.Error("sa.pub must be a public key")
	}
	if !strings.Contains(fs[installPath].Content, `VERSION="v1.37"`) {
		t.Error("install script must carry the version")
	}
	docs := decodeDocs(t, fs[configPath].Content)
	if len(docs) != 2 || docs[0]["kind"] != "InitConfiguration" || docs[1]["kind"] != "ClusterConfiguration" {
		t.Fatalf("init config: %v", docs)
	}
	tok := docs[0]["bootstrapTokens"].([]any)[0].(map[string]any)
	if tok["token"] != sec.Token || tok["ttl"] != "0s" {
		t.Errorf("bootstrap token: %v", tok)
	}
	if docs[0]["localAPIEndpoint"].(map[string]any)["advertiseAddress"] != "172.30.1.11" {
		t.Errorf("advertise address: %v", docs[0]["localAPIEndpoint"])
	}
	cc := docs[1]
	if cc["controlPlaneEndpoint"] != "172.30.1.11:6443" || cc["kubernetesVersion"] != "__KUBERNETES_VERSION__" ||
		cc["networking"].(map[string]any)["podSubnet"] != podSubnet {
		t.Errorf("cluster config: %v", cc)
	}
	sans := cc["apiServer"].(map[string]any)["certSANs"].([]any)
	if len(sans) != 3 || sans[2] != "172.30.1.10" {
		t.Errorf("certSANs: %v", sans)
	}
	if !strings.Contains(fs[initPath].Content, `ROLE="control-plane"`) {
		t.Error("bootstrap script role")
	}

	// ワーカー
	w, err := k.UserData(c, &distro.Node{Name: "demo-worker-1", Role: distro.RoleWorker, Index: 1, IP: netip.MustParseAddr("172.30.1.21")})
	if err != nil {
		t.Fatal(err)
	}
	wf := files(t, w)
	if _, ok := wf[pkiDir+"/ca.key"]; ok {
		t.Error("worker must not receive CA keys")
	}
	jd := decodeDocs(t, wf[configPath].Content)
	if len(jd) != 1 || jd[0]["kind"] != "JoinConfiguration" {
		t.Fatalf("join config: %v", jd)
	}
	bt := jd[0]["discovery"].(map[string]any)["bootstrapToken"].(map[string]any)
	wantHash, _ := pki.CACertHash(sec.CAs["ca"].CertPEM)
	if bt["apiServerEndpoint"] != "172.30.1.11:6443" || bt["token"] != sec.Token || bt["caCertHashes"].([]any)[0] != wantHash {
		t.Errorf("discovery: %v", bt)
	}
	nr := jd[0]["nodeRegistration"].(map[string]any)
	arg := nr["kubeletExtraArgs"].([]any)[0].(map[string]any)
	if nr["name"] != "demo-worker-1" || arg["name"] != "node-ip" || arg["value"] != "172.30.1.21" {
		t.Errorf("nodeRegistration: %v", nr)
	}
	if !strings.Contains(wf[initPath].Content, "https://172.30.1.11:6443/livez") {
		t.Error("worker must wait for the API before joining")
	}

	if _, err := k.UserData(c, &distro.Node{Role: distro.RoleControlPlane, Index: 2}); err == nil {
		t.Error("second control plane should be rejected for now")
	}
}

func TestValidateVersion(t *testing.T) {
	k := New()
	for _, v := range []string{"", "stable", "v1.37", "v1.37.2"} {
		if err := k.ValidateVersion(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"1.37", "latest", "v1.37.2+k3s1", "v1.37; reboot"} {
		if err := k.ValidateVersion(v); err == nil {
			t.Errorf("%q should be invalid", v)
		}
	}
}
