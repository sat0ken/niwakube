// Package kube はクラスタの Kubernetes API にマニフェストを適用する。
//
// クラスタができたあとに niwakubed が入れるもの (LoadBalancer の IP プール、ストレージなど) は、
// ディストリごとにノードの中で入れるのではなく、ホストから server-side apply する。
// Talos のようにノードにシェルが無くても同じ方法で入れられる。
package kube

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager は server-side apply のフィールドの持ち主の名前。
const FieldManager = "niwakube"

// ErrNotReady は、まだ適用できない (CRD が無い、API が応答しないなど) ときのエラー。再試行するとよい。
var ErrNotReady = errors.New("cluster is not ready for this manifest yet")

// Client はクラスタにマニフェストを適用するクライアント。
type Client struct {
	dyn    dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

// New は kubeconfig からクライアントを作る。
func New(kubeconfig []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst = 20, 40
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{dyn: dyn, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))}, nil
}

// Decode は複数ドキュメントの YAML を読む (空のドキュメントは飛ばす)。
func Decode(manifest string) ([]*unstructured.Unstructured, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))
	var out []*unstructured.Unstructured
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(&obj.Object); err != nil {
			return nil, err
		}
		if len(obj.Object) == 0 {
			continue
		}
		out = append(out, obj)
	}
}

// Apply はマニフェストを server-side apply する。種類 (CRD) がまだ無ければ ErrNotReady を返す。
func (c *Client) Apply(ctx context.Context, manifest string) error {
	objs, err := Decode(manifest)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := c.applyOne(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) applyOne(ctx context.Context, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	m, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			c.mapper.Reset() // CRD が後から入ったときに見つけられるように
			return fmt.Errorf("%s: %w", gvk, ErrNotReady)
		}
		return fmt.Errorf("%s: %w: %v", gvk, ErrNotReady, err)
	}
	var ri dynamic.ResourceInterface = c.dyn.Resource(m.Resource)
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		ri = c.dyn.Resource(m.Resource).Namespace(ns)
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply %s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}
