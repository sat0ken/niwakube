package distro

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestHandsonCiliumScript(t *testing.T) {
	plain := Addons{CNI: CNICilium, Hubble: true, DefaultRuntime: RuntimeRunc}.CiliumInstallScript("/etc/kubernetes/admin.conf", "172.30.1.11", 6443)
	if strings.Contains(plain, "gateway-api") || strings.Contains(plain, "--values") {
		t.Error("a normal cluster must not get the hands-on setup")
	}
	s := Addons{CNI: CNICilium, Hubble: true, DefaultRuntime: RuntimeRunc, Handson: true}.CiliumInstallScript("/etc/kubernetes/admin.conf", "172.30.1.11", 6443)
	for _, want := range []string{
		"gateway-api/releases/download/" + HandsonGatewayAPIVersion + "/experimental-install.yaml",
		"cilium install --version " + HandsonCiliumVersion + " --values " + handsonValuesPath,
		"--set k8sServiceHost=172.30.1.11",
		"oci://docker.io/envoyproxy/gateway-helm --version " + HandsonEnvoyGatewayVersion,
		"name: handson-gateway",
		"nodePort: 30080",
		"insecureNodePort: 31080",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("hands-on script lacks %q", want)
		}
	}
	// CRD は Cilium より先、Envoy Gateway は Cilium のあと
	crd, cil, eg := strings.Index(s, "experimental-install.yaml"), strings.Index(s, "cilium install"), strings.Index(s, "gateway-helm")
	if !(crd < cil && cil < eg) {
		t.Errorf("order: crd %d, cilium %d, envoy gateway %d", crd, cil, eg)
	}
	if err := (Addons{CNI: CNIFlannel, DefaultRuntime: RuntimeRunc, Handson: true}).Validate(); err == nil {
		t.Error("hands-on with flannel must be rejected")
	}
	if _, err := exec.LookPath("bash"); err == nil {
		f := filepath.Join(t.TempDir(), "s.sh")
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("bash -n: %v\n%s", err, out)
		}
	}
}

func TestHandsonEmbeddedYAML(t *testing.T) {
	var v map[string]any
	if err := yaml.Unmarshal([]byte(handsonCiliumValues), &v); err != nil {
		t.Fatalf("cilium values: %v", err)
	}
	for _, doc := range strings.Split(handsonGateway, "\n---\n") {
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("gateway: %v", err)
		}
	}
}
