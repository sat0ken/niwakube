package addons

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/kube"
)

func TestLoadBalancer(t *testing.T) {
	ms := LoadBalancer(netip.MustParseAddr("172.30.1.100"), netip.MustParseAddr("172.30.1.199"))
	if len(ms) != 2 || len(ms[0].Variants) != 2 {
		t.Fatalf("manifests: %+v", ms)
	}
	if !strings.Contains(ms[0].Variants[0], `start: "172.30.1.100"`) || !strings.Contains(ms[0].Variants[1], "cilium.io/v2alpha1") {
		t.Errorf("pool: %s", ms[0].Variants[0])
	}
	for _, v := range append(ms[0].Variants, ms[1].Variants...) {
		if _, err := kube.Decode(v); err != nil {
			t.Errorf("invalid YAML: %v\n%s", err, v)
		}
	}
}

func TestStorage(t *testing.T) {
	ms, err := Storage(StorageOptions{
		LocalPath: true, LocalPathDir: "/var/mnt/local-path-provisioner", PrivilegedNamespace: true,
		NFSServer: "172.30.0.1", NFSShare: "/var/lib/niwakube/nfs/demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "local-path-provisioner" || ms[1].Name != "csi-driver-nfs" {
		t.Fatalf("manifests: %+v", ms)
	}

	lp, err := kube.Decode(ms[0].Variants[0])
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range lp {
		seen[o.GetKind()] = true
		switch o.GetKind() {
		case "Namespace":
			if o.GetLabels()["pod-security.kubernetes.io/enforce"] != "privileged" {
				t.Error("namespace must be privileged on Talos")
			}
		case "ConfigMap":
			cfg := o.Object["data"].(map[string]any)["config.json"].(string)
			if !strings.Contains(cfg, "/var/mnt/local-path-provisioner") || strings.Contains(cfg, "/opt/local-path-provisioner") {
				t.Errorf("config.json: %s", cfg)
			}
		case "StorageClass":
			if o.GetAnnotations()["storageclass.kubernetes.io/is-default-class"] != "true" {
				t.Error("local-path must be the default StorageClass")
			}
		}
	}
	for _, k := range []string{"Namespace", "Deployment", "ConfigMap", "StorageClass", "ClusterRole"} {
		if !seen[k] {
			t.Errorf("local-path lacks %s", k)
		}
	}

	nfs := ms[1].Variants[0]
	if strings.Contains(nfs, "csi-snapshotter") {
		t.Error("snapshotter sidecar must be removed")
	}
	objs, err := kube.Decode(nfs)
	if err != nil {
		t.Fatal(err)
	}
	var sc map[string]any
	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o.GetKind()] = true
		if o.GetKind() == "StorageClass" {
			sc = o.Object
		}
	}
	for _, k := range []string{"CSIDriver", "Deployment", "DaemonSet", "ServiceAccount", "ClusterRole", "StorageClass"} {
		if !kinds[k] {
			t.Errorf("csi-driver-nfs lacks %s", k)
		}
	}
	params := sc["parameters"].(map[string]any)
	if sc["provisioner"] != "nfs.csi.k8s.io" || params["server"] != "172.30.0.1" || params["share"] != "/var/lib/niwakube/nfs/demo" {
		t.Errorf("StorageClass: %v", sc)
	}

	// k3s (local-path が組み込み) で NFS も無いときは何も入れない
	if ms, _ := Storage(StorageOptions{}); len(ms) != 0 {
		t.Errorf("expected nothing, got %+v", ms)
	}
}
