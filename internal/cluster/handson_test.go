package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sat0ken/niwakube/internal/machine"
	"github.com/sat0ken/niwakube/internal/store"
)

func TestHandsonProfile(t *testing.T) {
	ctx := context.Background()
	cs, _, fb, _ := newTest(t)
	c, err := cs.Create(ctx, CreateRequest{Name: "cnd", Profile: ProfileHandson})
	if err != nil {
		t.Fatal(err)
	}
	if c.Distro != "kubeadm" || c.Version != "v1.36" || c.CNI != "cilium" || !c.Hubble || c.Workers != 2 || c.Profile != ProfileHandson {
		t.Fatalf("unexpected cluster: %+v", c)
	}
	cp, _ := fb.Spec("cnd-cp-1")
	if ud := string(cp.Seed.UserData); !strings.Contains(ud, "gateway-helm") || !strings.Contains(ud, "cilium-handson.values.yaml") {
		t.Error("the CP must install the hands-on Cilium and Envoy Gateway")
	}
	fs, err := cs.ListForwards(ctx, "cnd")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, fmt.Sprintf("%d->%s:%d", f.HostPort, f.IP, f.Port))
	}
	slices.Sort(got)
	want := []string{"18080->172.30.1.11:32080", "18443->172.30.1.11:32443", "80->172.30.1.11:30080", "8080->172.30.1.11:31080", "8443->172.30.1.11:31443"}
	if !slices.Equal(got, want) {
		t.Errorf("forwards: %v, want %v", got, want)
	}
	if fw := cs.cfg.Forwarder.(*fakeForwarder); len(fw.last) != 5 {
		t.Errorf("forwards applied: %d", len(fw.last))
	}

	// ポートは 1 つのクラスタしか使えない。断ったときは何も残さない
	if _, err := cs.Create(ctx, CreateRequest{Name: "cnd2", Profile: ProfileHandson}); !errors.Is(err, store.ErrExists) || !strings.Contains(err.Error(), "cnd") {
		t.Errorf("second hands-on cluster: want ErrExists, got %v", err)
	}
	if _, err := cs.Get(ctx, "cnd2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cnd2 must not exist: %v", err)
	}
	// 消すと転送も消え、次のクラスタが使える
	if err := cs.Delete(ctx, "cnd"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Create(ctx, CreateRequest{Name: "cnd2", Profile: ProfileHandson}); err != nil {
		t.Errorf("after deleting the first: %v", err)
	}

	no := false
	for _, bad := range []CreateRequest{
		{Name: "x1", Profile: "nope"},
		{Name: "x2", Profile: ProfileHandson, Distro: "k3s"},
		{Name: "x3", Profile: ProfileHandson, CNI: "flannel"},
		{Name: "x4", Profile: ProfileHandson, Hubble: &no},
	} {
		if _, err := cs.Create(ctx, bad); !errors.Is(err, machine.ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", bad, err)
		}
	}
}

func TestHandsonTerminalOptions(t *testing.T) {
	ctx := context.Background()
	cs, _, _, _ := newTest(t)
	one := 1
	if _, err := cs.Create(ctx, CreateRequest{Name: "plain", Workers: &one}); err != nil {
		t.Fatal(err)
	}
	if o, err := cs.TerminalOptions(ctx, "plain"); err != nil || o.HomeID != "" || len(o.HostNames) != 0 {
		t.Errorf("plain cluster: %+v %v", o, err)
	}
	if _, err := cs.Create(ctx, CreateRequest{Name: "cnd", Profile: ProfileHandson}); err != nil {
		t.Fatal(err)
	}
	o, err := cs.TerminalOptions(ctx, "cnd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.HomeID, "cnd-") || o.HostIP.String() != "172.30.0.1" || !slices.Contains(o.HostNames, "app.example.com") {
		t.Errorf("hands-on options: %+v", o)
	}
	homes, err := cs.TerminalHomes(ctx)
	if err != nil || len(homes) != 1 || homes[0] != o.HomeID {
		t.Errorf("homes: %v %v", homes, err)
	}
}
