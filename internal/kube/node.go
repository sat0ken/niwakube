package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Nodes はノードの cordon / drain / 状態確認をする (ノードの大きさを変えるときに使う)。
type Nodes struct {
	cs           kubernetes.Interface
	pollInterval time.Duration
}

// NewNodes は kubeconfig からクライアントを作る。
func NewNodes(kubeconfig []byte) (*Nodes, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 30 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewNodesFor(cs), nil
}

// NewNodesFor は既存のクライアントから作る (テスト用)。
func NewNodesFor(cs kubernetes.Interface) *Nodes {
	return &Nodes{cs: cs, pollInterval: 2 * time.Second}
}

// Cordon はノードにスケジュールしない (unschedulable=true) / する (false) ようにする。
func (n *Nodes) Cordon(ctx context.Context, node string, unschedulable bool) error {
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable)
	_, err := n.cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// evictable は drain で追い出す Pod か (kubectl drain --ignore-daemonsets --delete-emptydir-data と同じ)。
// DaemonSet の Pod と static Pod (mirror Pod) は追い出さない。終わった Pod は放っておく。
func evictable(p *corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}

// Drain はノードの Pod を eviction で追い出し、いなくなるまで待つ (PodDisruptionBudget は守る)。
// ctx の期限までに終わらなければエラーを返す。
func (n *Nodes) Drain(ctx context.Context, node string) error {
	for {
		pods, err := n.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
		})
		if err != nil {
			return err
		}
		remaining := 0
		for i := range pods.Items {
			p := &pods.Items[i]
			if !evictable(p) {
				continue
			}
			remaining++
			if p.DeletionTimestamp != nil {
				continue // 終了待ち
			}
			err := n.cs.PolicyV1().Evictions(p.Namespace).Evict(ctx, &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
			})
			switch {
			case err == nil, apierrors.IsNotFound(err):
			case apierrors.IsTooManyRequests(err):
				// PodDisruptionBudget で今は追い出せない。あとでもう一度試す
			default:
				return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
			}
		}
		if remaining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain %s: %d pod(s) still running: %w", node, remaining, ctx.Err())
		case <-time.After(n.pollInterval):
		}
	}
}

// ReadySince はノードが Ready で、since より後に kubelet から状態の報告があったかを返す
// (再起動したノードの kubelet が戻ってきたことを確かめる)。
func (n *Nodes) ReadySince(ctx context.Context, node string, since time.Time) (bool, error) {
	nd, err := n.cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, c := range nd.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue && !c.LastHeartbeatTime.Time.Before(since.Truncate(time.Second)), nil
		}
	}
	return false, nil
}

// DeleteNode は Node オブジェクトを消す (ワーカーを減らすとき、マシンを消す前に呼ぶ)。無ければ何もしない。
func (n *Nodes) DeleteNode(ctx context.Context, node string) error {
	err := n.cs.CoreV1().Nodes().Delete(ctx, node, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
