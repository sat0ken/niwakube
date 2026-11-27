package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// resource は apiVersion と kind から、操作する API (名前空間つきなら ns の中) を返す。
func (c *Client) resource(apiVersion, kind, ns string) (dynamic.ResourceInterface, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	m, err := c.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			c.mapper.Reset()
		}
		return nil, fmt.Errorf("%s %s: %w", apiVersion, kind, err)
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		if ns == "" {
			ns = "default"
		}
		return c.dyn.Resource(m.Resource).Namespace(ns), nil
	}
	return c.dyn.Resource(m.Resource), nil
}

// Get はオブジェクトを 1 つ読む。無ければ (nil, nil)。
func (c *Client) Get(ctx context.Context, apiVersion, kind, ns, name string) (map[string]any, error) {
	ri, err := c.resource(apiVersion, kind, ns)
	if err != nil {
		return nil, err
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return obj.Object, nil
}

// List はラベルセレクター・フィールドセレクターに合うオブジェクトの一覧を {"items": [...]} の形で返す。
func (c *Client) List(ctx context.Context, apiVersion, kind, ns, selector, fieldSelector string) (map[string]any, error) {
	ri, err := c.resource(apiVersion, kind, ns)
	if err != nil {
		return nil, err
	}
	l, err := ri.List(ctx, metav1.ListOptions{LabelSelector: selector, FieldSelector: fieldSelector})
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(l.Items))
	for i := range l.Items {
		items = append(items, l.Items[i].Object)
	}
	return map[string]any{"items": items}, nil
}

var nsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// DeleteNamespace は Namespace を消す (中身ごと)。無ければ何もしない。消え終わるのは待たない。
func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	err := c.dyn.Resource(nsGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// NamespaceExists は Namespace がまだあるか (消している途中も含む) を返す。
func (c *Client) NamespaceExists(ctx context.Context, name string) (bool, error) {
	_, err := c.dyn.Resource(nsGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

var sarGVR = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "subjectaccessreviews"}

// CanI は user (と groups) が操作をできるかを API サーバーに問い合わせる (SubjectAccessReview。kubectl auth can-i と同じ)。
func (c *Client) CanI(ctx context.Context, user string, groups []string, verb, group, resource, ns string) (bool, error) {
	gs := make([]any, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, g)
	}
	sar := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SubjectAccessReview",
		"spec": map[string]any{
			"user":   user,
			"groups": gs,
			"resourceAttributes": map[string]any{
				"verb": verb, "group": group, "resource": resource, "namespace": ns,
			},
		},
	}}
	out, err := c.dyn.Resource(sarGVR).Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	allowed, _, err := unstructured.NestedBool(out.Object, "status", "allowed")
	return allowed, err
}
