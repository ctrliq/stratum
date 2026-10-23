package ha

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
)

func TestJoinStopsAfterLosingMaintenanceLease(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(map[bool]string{false: "new member", true: "adopted member"}[adopted], func(t *testing.T) {
			joinFixture(t)
			dir := t.TempDir()
			if adopted {
				first := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"), quorumHeldErr: errors.New("no quorum")}
				if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fakeJoinEffects(first)); err == nil {
					t.Fatal("expected retained member")
				}
			}
			r := &recordedJoin{listMembers: listedMembers()}
			fx := fakeJoinEffects(r)
			released, tookFence := false, false
			fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
				tookFence = true
				guard := &maintlease.Guard{}
				guard.Observe(maintlease.ErrTaken, time.Now())
				return &k8sutil.MaintenanceLease{Guard: guard, Release: func() { released = true }}, nil
			}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fx)
			if adopted {
				if err != nil || r.addCalls != 0 || r.saved == nil || r.removeCalled || r.removeByIDCalls != 0 || tookFence || released {
					t.Fatalf("retained member must recover without an API fence: err=%v adds=%d saved=%v tookFence=%v released=%v", err, r.addCalls, r.saved, tookFence, released)
				}
				return
			}
			if !errors.Is(err, maintlease.ErrTaken) || r.addCalls != 0 || r.saved != nil || r.removeCalled || r.removeByIDCalls != 0 || !released {
				t.Fatalf("lease loss: err=%v adds=%d saved=%v removed=%v byID=%d released=%v", err, r.addCalls, r.saved, r.removeCalled, r.removeByIDCalls, released)
			}
		})
	}
}
