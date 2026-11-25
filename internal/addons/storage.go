package addons

import (
	"embed"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/sat0ken/niwakube/internal/kube"
)

// 同梱しているマニフェスト (上流の deploy/ のものをそのまま置いている。変更は Go で行う)
//
//go:embed manifests/*.yaml
var manifests embed.FS

const (
	localPathFile = "manifests/local-path-storage-v0.0.37.yaml"
	nfsPrefix     = "manifests/csi-driver-nfs-v4.13.2-"
)

var nfsFiles = []string{"rbac-csi-nfs.yaml", "csi-nfs-driverinfo.yaml", "csi-nfs-controller.yaml", "csi-nfs-node.yaml"}

// StorageOptions はストレージのアドオンの設定。
type StorageOptions struct {
	// LocalPath が true なら local-path-provisioner を入れて既定の StorageClass にする (k3s は組み込みなので false)
	LocalPath bool
	// LocalPathDir はノード上の保存先 (例: /opt/local-path-provisioner、Talos は /var/mnt/local-path-provisioner)
	LocalPathDir string
	// PrivilegedNamespace は local-path-storage の Namespace を Pod Security の privileged にする (Talos は既定で厳しい)
	PrivilegedNamespace bool
	// NFSServer / NFSShare が空でなければ csi-driver-nfs を入れ、StorageClass niwakube-nfs を作る
	NFSServer string
	NFSShare  string
}

// Storage はストレージのアドオンを返す。
func Storage(o StorageOptions) ([]Manifest, error) {
	var out []Manifest
	if o.LocalPath {
		m, err := localPath(o)
		if err != nil {
			return nil, err
		}
		out = append(out, Manifest{Name: "local-path-provisioner", Variants: []string{m}})
	}
	if o.NFSServer != "" && o.NFSShare != "" {
		m, err := nfs(o)
		if err != nil {
			return nil, err
		}
		out = append(out, Manifest{Name: "csi-driver-nfs", Variants: []string{m}})
	}
	return out, nil
}

func load(name string) ([]*unstructured.Unstructured, error) {
	b, err := manifests.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return kube.Decode(string(b))
}

func encode(objs []*unstructured.Unstructured) (string, error) {
	var b strings.Builder
	for _, o := range objs {
		y, err := yaml.Marshal(o.Object)
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		b.Write(y)
	}
	return b.String(), nil
}

func localPath(o StorageOptions) (string, error) {
	objs, err := load(localPathFile)
	if err != nil {
		return "", err
	}
	for _, obj := range objs {
		switch obj.GetKind() {
		case "Namespace":
			if o.PrivilegedNamespace {
				labels := obj.GetLabels()
				if labels == nil {
					labels = map[string]string{}
				}
				labels["pod-security.kubernetes.io/enforce"] = "privileged"
				obj.SetLabels(labels)
			}
		case "ConfigMap":
			cfg, _, _ := unstructured.NestedString(obj.Object, "data", "config.json")
			if !strings.Contains(cfg, "/opt/local-path-provisioner") {
				return "", fmt.Errorf("local-path config.json has an unexpected format")
			}
			cfg = strings.ReplaceAll(cfg, "/opt/local-path-provisioner", o.LocalPathDir)
			if err := unstructured.SetNestedField(obj.Object, cfg, "data", "config.json"); err != nil {
				return "", err
			}
		case "StorageClass":
			ann := obj.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["storageclass.kubernetes.io/is-default-class"] = "true"
			obj.SetAnnotations(ann)
		}
	}
	return encode(objs)
}

func nfs(o StorageOptions) (string, error) {
	var objs []*unstructured.Unstructured
	for _, f := range nfsFiles {
		docs, err := load(nfsPrefix + f)
		if err != nil {
			return "", err
		}
		objs = append(objs, docs...)
	}
	for _, obj := range objs {
		if obj.GetKind() != "Deployment" || obj.GetName() != "csi-nfs-controller" {
			continue
		}
		// スナップショットのサイドカーは VolumeSnapshot の CRD が無いと起動に失敗し続けるので外す
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		var kept []any
		for _, c := range containers {
			if m, ok := c.(map[string]any); ok && m["name"] == "csi-snapshotter" {
				continue
			}
			kept = append(kept, c)
		}
		if err := unstructured.SetNestedSlice(obj.Object, kept, "spec", "template", "spec", "containers"); err != nil {
			return "", err
		}
	}
	sc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata": map[string]any{
			"name":   "niwakube-nfs",
			"labels": map[string]any{"app.kubernetes.io/managed-by": "niwakube"},
		},
		"provisioner": "nfs.csi.k8s.io",
		"parameters": map[string]any{
			"server": o.NFSServer,
			"share":  o.NFSShare,
			// PV ごとにサブディレクトリを作る。ホスト側で何のデータか分かるように名前空間と PVC 名を入れる
			"subDir": "${pvc.metadata.namespace}-${pvc.metadata.name}-${pv.metadata.name}",
		},
		"reclaimPolicy":        "Delete",
		"volumeBindingMode":    "Immediate",
		"allowVolumeExpansion": true,
		"mountOptions":         []any{"nfsvers=4.1"},
	}}
	objs = append(objs, sc)
	return encode(objs)
}
