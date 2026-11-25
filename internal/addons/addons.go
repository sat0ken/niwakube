// Package addons は、クラスタができたあとに niwakubed が入れるもののマニフェスト。
package addons

import (
	"fmt"
	"net/netip"
)

// Manifest は適用するマニフェスト。Variants は順に試す候補 (CRD の API バージョンが Cilium のバージョンで違うため)。
type Manifest struct {
	Name     string
	Variants []string
}

// LoadBalancer は Cilium の LB IPAM と L2 アナウンスの設定を返す。
// Service type: LoadBalancer に start〜stop の IP を割り当て、ノードが ARP に応答する (ホストと同じ L2 の nkbr0 上で届く)。
func LoadBalancer(start, stop netip.Addr) []Manifest {
	pool := func(apiVersion string) string {
		return fmt.Sprintf(`apiVersion: %s
kind: CiliumLoadBalancerIPPool
metadata:
  name: niwakube
  labels:
    app.kubernetes.io/managed-by: niwakube
spec:
  blocks:
    - start: "%s"
      stop: "%s"
`, apiVersion, start, stop)
	}
	policy := func(apiVersion string) string {
		return fmt.Sprintf(`apiVersion: %s
kind: CiliumL2AnnouncementPolicy
metadata:
  name: niwakube
  labels:
    app.kubernetes.io/managed-by: niwakube
spec:
  interfaces:
    - ^eth[0-9]+
  externalIPs: true
  loadBalancerIPs: true
`, apiVersion)
	}
	return []Manifest{
		{Name: "lb-ip-pool", Variants: []string{pool("cilium.io/v2"), pool("cilium.io/v2alpha1")}},
		{Name: "l2-announcement-policy", Variants: []string{policy("cilium.io/v2alpha1"), policy("cilium.io/v2")}},
	}
}
