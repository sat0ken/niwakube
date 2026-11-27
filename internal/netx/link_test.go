package netx

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// vanishRun は veth が「show では見えたが、del の時点では消えていた」状況をまねる。
type vanishRun struct {
	calls []string
	shows int
}

func (v *vanishRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	v.calls = append(v.calls, cmd)
	switch {
	case strings.HasPrefix(cmd, "ip link show"):
		v.shows++
		if v.shows == 1 {
			return nil, nil // 1 回目は見える
		}
		return nil, errors.New("Device does not exist")
	case strings.HasPrefix(cmd, "ip link del"):
		return nil, errors.New(`Cannot find device "nkv1"`)
	}
	return nil, nil
}

func TestDeleteToleratesVanishedLink(t *testing.T) {
	r := &vanishRun{}
	if err := (Links{Run: r}).Delete(context.Background(), "nkv1"); err != nil {
		t.Errorf("a link that vanished meanwhile must count as deleted: %v (%v)", err, r.calls)
	}
	// まだあるのに消せないのはエラー
	stuck := &stuckRun{}
	if err := (Links{Run: stuck}).Delete(context.Background(), "nkv1"); err == nil {
		t.Error("want an error when the link is still there")
	}
}

type stuckRun struct{}

func (stuckRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if strings.Join(args, " ") == "link del dev nkv1" {
		return nil, errors.New("Operation not permitted")
	}
	return nil, nil
}
