package kube

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func pod(name, node string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func TestDrain(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}},
		pod("app", "w1", nil),
		pod("pdb-protected", "w1", nil),
		pod("ds", "w1", func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "cilium", APIVersion: "apps/v1"}}
		}),
		pod("static", "w1", func(p *corev1.Pod) { p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"} }),
		pod("done", "w1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		pod("other-node", "w2", nil),
	)
	// fake は fieldSelector を見ないので、ノードで絞る
	cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sel := a.(k8stesting.ListAction).GetListRestrictions().Fields
		objs, err := cs.Tracker().List(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, "")
		if err != nil {
			return true, nil, err
		}
		list := objs.(*corev1.PodList)
		out := &corev1.PodList{}
		for _, p := range list.Items {
			if sel.Matches(fieldsSet(p)) {
				out.Items = append(out.Items, p)
			}
		}
		return true, out, nil
	})
	// eviction で Pod を消す。PDB で守られた Pod は 1 回目だけ 429 を返す
	refused := false
	var evicted []string
	cs.PrependReactor("create", "pods/eviction", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ev := a.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if ev.Name == "pdb-protected" && !refused {
			refused = true
			return true, nil, apierrors.NewTooManyRequests("pdb", 1)
		}
		evicted = append(evicted, ev.Name)
		return true, nil, cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ev.Namespace, ev.Name)
	})

	n := NewNodesFor(cs)
	n.pollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Cordon(ctx, "w1", true); err != nil {
		t.Fatal(err)
	}
	if nd, _ := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{}); !nd.Spec.Unschedulable {
		t.Error("node must be unschedulable after cordon")
	}
	if err := n.Drain(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 2 || !refused {
		t.Errorf("evicted %v (pdb refused once: %v)", evicted, refused)
	}
	for _, keep := range []string{"ds", "static", "done", "other-node"} {
		if _, err := cs.CoreV1().Pods("default").Get(ctx, keep, metav1.GetOptions{}); err != nil {
			t.Errorf("%s must not be evicted: %v", keep, err)
		}
	}
	if err := n.Cordon(ctx, "w1", false); err != nil {
		t.Fatal(err)
	}
	if nd, _ := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{}); nd.Spec.Unschedulable {
		t.Error("node must be schedulable after uncordon")
	}
}

func fieldsSet(p corev1.Pod) fieldsMap { return fieldsMap{"spec.nodeName": p.Spec.NodeName} }

type fieldsMap map[string]string

func (f fieldsMap) Has(k string) bool   { _, ok := f[k]; return ok }
func (f fieldsMap) Get(k string) string { return f[k] }

func TestDrainTimesOut(t *testing.T) {
	cs := fake.NewSimpleClientset(pod("stuck", "w1", nil))
	cs.PrependReactor("create", "pods/eviction", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequests("pdb", 1) // ずっと追い出せない
	})
	n := NewNodesFor(cs)
	n.pollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := n.Drain(ctx, "w1"); err == nil {
		t.Fatal("drain must time out")
	}
}

func TestReadySince(t *testing.T) {
	restart := time.Now()
	node := func(status corev1.ConditionStatus, hb time.Time) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: status, LastHeartbeatTime: metav1.NewTime(hb)},
		}}}
	}
	for name, c := range map[string]struct {
		node *corev1.Node
		want bool
	}{
		"ready after restart": {node(corev1.ConditionTrue, restart.Add(30*time.Second)), true},
		"stale heartbeat":     {node(corev1.ConditionTrue, restart.Add(-time.Minute)), false},
		"not ready yet":       {node(corev1.ConditionFalse, restart.Add(30*time.Second)), false},
	} {
		got, err := NewNodesFor(fake.NewSimpleClientset(c.node)).ReadySince(context.Background(), "w1", restart)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v %v, want %v", name, got, err, c.want)
		}
	}
}
