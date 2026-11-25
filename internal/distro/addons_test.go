package distro

import (
	"strings"
	"testing"
)

func TestAddonsValidate(t *testing.T) {
	ok := []Addons{
		{CNI: CNICilium, Hubble: true, DefaultRuntime: RuntimeRunc},
		{CNI: CNIFlannel, DefaultRuntime: RuntimeRunc},
		{CNI: CNICilium, DefaultRuntime: RuntimeCrun, Runtimes: []string{RuntimeCrun, RuntimeYouki}},
	}
	for _, a := range ok {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []Addons{
		{CNI: "calico", DefaultRuntime: RuntimeRunc},
		{CNI: CNIFlannel, Hubble: true, DefaultRuntime: RuntimeRunc},
		{CNI: CNICilium, DefaultRuntime: RuntimeYouki},
		{CNI: CNICilium, DefaultRuntime: RuntimeRunc, Runtimes: []string{"kata"}},
	}
	for _, a := range bad {
		if err := a.Validate(); err == nil {
			t.Errorf("%+v: expected error", a)
		}
	}
}

func TestAddonsScripts(t *testing.T) {
	a := Addons{CNI: CNICilium, Hubble: true, DefaultRuntime: RuntimeCrun, Runtimes: []string{RuntimeCrun, RuntimeYouki}}
	rt := a.RuntimeInstallScript()
	if !strings.Contains(rt, "apt-get install -y -q crun") || !strings.Contains(rt, "youki-dev/youki/releases/latest") {
		t.Errorf("runtime script:\n%s", rt)
	}
	rc := a.RuntimeClassManifest()
	for _, name := range []string{"runc", "crun", "youki"} {
		if !strings.Contains(rc, "name: "+name+"\n") || !strings.Contains(rc, "handler: "+name+"\n") {
			t.Errorf("RuntimeClass %s missing:\n%s", name, rc)
		}
	}
	cs := a.CiliumInstallScript("/etc/kubernetes/admin.conf", "172.30.1.11", 6443)
	for _, want := range []string{
		"--set ipam.mode=kubernetes", "--set kubeProxyReplacement=true", "--set k8sServiceHost=172.30.1.11",
		"--set k8sServicePort=6443", "--set hubble.relay.enabled=true", "--set hubble.ui.enabled=true",
		"sha256sum --check", "cilium status --wait",
	} {
		if !strings.Contains(cs, want) {
			t.Errorf("cilium script lacks %q", want)
		}
	}
	a.Hubble = false
	if strings.Contains(a.CiliumInstallScript("k", "h", 1), "hubble") {
		t.Error("hubble must not be enabled when Hubble is false")
	}
	if (Addons{CNI: CNICilium, DefaultRuntime: RuntimeRunc}).RuntimeInstallScript() == "" {
		t.Error("script must at least have a shebang")
	}
}
