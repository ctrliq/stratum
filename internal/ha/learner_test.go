package ha

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

// stubLearner points PromoteLearner's etcd seams at status and promote, and
// returns the voters each promote was sent through.
func stubLearner(t *testing.T, status localEtcdStatus, statusErr, promoteErr error) *[][]string {
	t.Helper()
	oldStatus, oldPromote := learnerLocalStatus, learnerPromote
	t.Cleanup(func() { learnerLocalStatus, learnerPromote = oldStatus, oldPromote })
	var calls [][]string
	learnerLocalStatus = func(string) (localEtcdStatus, error) { return status, statusErr }
	learnerPromote = func(id uint64, voters ...string) error {
		if id != status.MemberID {
			t.Errorf("promoted member %x, want this node's member %x", id, status.MemberID)
		}
		calls = append(calls, voters)
		return promoteErr
	}
	return &calls
}

const threeMembers = "cp1=https://10.0.0.1:2380,cp2=https://10.0.0.2:2380,cp3=https://10.0.0.3:2380"

// TestPromoteLearnerPromotesOnlyACaughtUpLearner pins what the controller's
// start does with this node's etcd member: a voter is left alone, a learner
// is promoted through the other members once its etcd answers and knows a
// leader, and a promote etcd can still accept later waits instead of failing.
func TestPromoteLearnerPromotesOnlyACaughtUpLearner(t *testing.T) {
	learner := localEtcdStatus{MemberID: 0xc2, Leader: 0xa1, IsLearner: true}
	for _, tc := range []struct {
		name       string
		status     localEtcdStatus
		statusErr  error
		promoteErr error
		want       LearnerState
		waiting    bool
		wantErr    string
		promotes   int
	}{
		{"a voter is left alone", localEtcdStatus{MemberID: 0xc2, Leader: 0xa1}, nil, nil, MemberIsVoter, false, "", 0},
		{"an etcd that does not answer yet waits", localEtcdStatus{}, context.DeadlineExceeded, nil, LearnerWaiting, true, "does not answer", 0},
		{"a learner with no leader yet waits", localEtcdStatus{MemberID: 0xc2, IsLearner: true}, nil, nil, LearnerWaiting, true, "member c2 knows no leader", 0},
		{"a caught-up learner is promoted", learner, nil, nil, LearnerPromoted, false, "", 1},
		{"a promote whose reply was lost is a promote", learner, nil, rpctypes.ErrGRPCMemberNotLearner, LearnerPromoted, false, "", 1},
		{"a learner still catching up waits", learner, nil, rpctypes.ErrGRPCLearnerNotReady, LearnerWaiting, true, "did not promote member c2 yet", 1},
		{"a promote that timed out waits", learner, nil, context.DeadlineExceeded, LearnerWaiting, true, "did not promote member c2 yet", 1},
		{"any other refusal is an error", learner, nil, rpctypes.ErrGRPCPermissionDenied, LearnerWaiting, false, "promote etcd learner c2", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubLearner(t, tc.status, tc.statusErr, tc.promoteErr)
			got, err := PromoteLearner("10.0.0.2", threeMembers)
			if got != tc.want {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("error = %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrLearnerWaiting); got != tc.waiting {
				t.Errorf("error %v wraps ErrLearnerWaiting = %v, want %v", err, got, tc.waiting)
			}
			if len(*calls) != tc.promotes {
				t.Fatalf("promotes = %d, want %d", len(*calls), tc.promotes)
			}
			for _, voters := range *calls {
				if !slices.Equal(voters, []string{"10.0.0.1", "10.0.0.3"}) {
					t.Errorf("promoted through %v, want the other members 10.0.0.1 and 10.0.0.3", voters)
				}
			}
		})
	}
}

// TestPromoteLearnerRefusesWithNoOtherMember: a learner must be promoted
// through another member, so node state that names none is an error, not a
// promote sent to the learner itself.
func TestPromoteLearnerRefusesWithNoOtherMember(t *testing.T) {
	calls := stubLearner(t, localEtcdStatus{MemberID: 0xc2, Leader: 0xa1, IsLearner: true}, nil, nil)
	got, err := PromoteLearner("10.0.0.2", "cp2=https://10.0.0.2:2380")
	if got != LearnerWaiting || err == nil || len(*calls) != 0 {
		t.Fatalf("state = %v, error = %v, promotes = %d; want waiting, an error and no promote", got, err, len(*calls))
	}
}

func TestOtherMemberHosts(t *testing.T) {
	for _, tc := range []struct {
		name, initial, self string
		want                []string
	}{
		{"the others, in order", threeMembers, "10.0.0.2", []string{"10.0.0.1", "10.0.0.3"}},
		{"IPv6, whatever the spelling", "a=https://[fd00::1]:2380,b=https://[FD00:0:0::2]:2380", "fd00::2", []string{"fd00::1"}},
		{"a host named twice once", "a=https://10.0.0.1:2380,a=https://10.0.0.1:2381,b=https://10.0.0.2:2380", "10.0.0.2", []string{"10.0.0.1"}},
		{"malformed entries skipped", "nonsense, =,b=https://10.0.0.1:2380", "10.0.0.2", []string{"10.0.0.1"}},
		{"nothing recorded", "", "10.0.0.2", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := otherMemberHosts(tc.initial, tc.self); !slices.Equal(got, tc.want) {
				t.Errorf("otherMemberHosts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJoinRecordsItsLearnerForPromotion: the join adds its member as a
// learner, and the controller's start promotes only what node state records
// pending, so the saved state must record it.
func TestJoinRecordsItsLearnerForPromotion(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r)); err != nil {
		t.Fatal(err)
	}
	if r.saved == nil || !r.saved.EtcdPromotePending {
		t.Fatalf("saved state = %+v, want EtcdPromotePending", r.saved)
	}
}

func TestIsTooManyLearnersIsARefusal(t *testing.T) {
	if !memberAddRefused(rpctypes.ErrGRPCTooManyLearners) || !isTooManyLearners(fmt.Errorf("etcd member add: %w", rpctypes.ErrTooManyLearners)) {
		t.Fatal("etcd's one-learner-at-a-time refusal must read as a refusal that added nothing")
	}
}
