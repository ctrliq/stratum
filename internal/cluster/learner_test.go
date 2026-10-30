package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/nodestate"
)

// joinedLearnerState is a control plane a join just added as a learner.
func joinedLearnerState() *NodeState {
	st := seedState()
	st.Hostname, st.NodeIP, st.EtcdName = "cp2", "10.0.0.2", "cp2"
	st.EtcdState, st.EtcdInitialCluster, st.EtcdPromotePending = "existing", grownThree, true
	return st
}

// stubLearnerPromotion saves st as this node's state and points the learner
// task's seams at answer and a lock that is free unless held is set or the
// task already holds it, and that answer must be asked without. It returns
// the number of promote attempts made.
func stubLearnerPromotion(t *testing.T, st *NodeState, held *bool, answer func() (ha.LearnerState, error)) *int {
	t.Helper()
	redirectStateArea(t)
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	oldPromote, oldLock := promoteLocalLearner, lockEtcdData
	t.Cleanup(func() { promoteLocalLearner, lockEtcdData = oldPromote, oldLock })
	var locked bool
	lockEtcdData = func() (func(), error) {
		if locked || (held != nil && *held) {
			return nil, errors.New("etcd data lock: another operation is changing the data dir")
		}
		locked = true
		return func() { locked = false }, nil
	}
	var attempts int
	promoteLocalLearner = func(nodeIP, initialCluster string) (ha.LearnerState, error) {
		attempts++
		if locked {
			t.Error("the promote ran under the etcd data lock, refusing every other operation that needs it")
		}
		if nodeIP != st.NodeIP || initialCluster != st.EtcdInitialCluster {
			t.Errorf("promote asked about %s in %q, want this node's %s in %q", nodeIP, initialCluster, st.NodeIP, st.EtcdInitialCluster)
		}
		return answer()
	}
	return &attempts
}

func pendingOnDisk(t *testing.T) bool {
	t.Helper()
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.EtcdPromotePending
}

// TestPromoteLearnerOnceClearsThePendingRecordOnlyOnceAVoter pins one look of
// the controller's learner task: it keeps the pending record while the learner
// waits or etcd refuses, and clears it once the member is a voter, whether this
// look promoted it or an earlier one whose answer was lost did.
func TestPromoteLearnerOnceClearsThePendingRecordOnlyOnceAVoter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       ha.LearnerState
		err         error
		wantDone    bool
		wantPending bool
	}{
		{"a learner still catching up", ha.LearnerWaiting, nil, false, true},
		{"a refused promote", ha.LearnerWaiting, errors.New("promote etcd learner c2: permission denied"), false, true},
		{"a promoted learner", ha.LearnerPromoted, nil, true, false},
		{"already a voter", ha.MemberIsVoter, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) { return tc.state, tc.err })
			var look learnerLook
			if done := promoteLearnerOnce(&look); done != tc.wantDone {
				t.Errorf("done = %v, want %v", done, tc.wantDone)
			}
			if *attempts != 1 {
				t.Errorf("promote attempts = %d, want 1", *attempts)
			}
			if got := pendingOnDisk(t); got != tc.wantPending {
				t.Errorf("EtcdPromotePending on disk = %v, want %v", got, tc.wantPending)
			}
			if (look.warned != "") != (tc.err != nil) {
				t.Errorf("warned = %q for error %v", look.warned, tc.err)
			}
		})
	}
}

// TestPromoteLearnerOnceLeavesARejoinsLearnerAlone: a snapshot rejoin holds
// the etcd data lock while it replaces this node's member and promotes its own
// learner, and clears the pending record, so the task neither promotes under a
// held lock nor promotes a member nothing records pending.
func TestPromoteLearnerOnceLeavesARejoinsLearnerAlone(t *testing.T) {
	t.Run("the data lock is held", func(t *testing.T) {
		held := true
		attempts := stubLearnerPromotion(t, joinedLearnerState(), &held, func() (ha.LearnerState, error) { return ha.LearnerPromoted, nil })
		var look learnerLook
		if promoteLearnerOnce(&look) || *attempts != 0 || !pendingOnDisk(t) {
			t.Fatalf("a held lock must skip the look: attempts = %d, pending = %v", *attempts, pendingOnDisk(t))
		}
	})
	t.Run("nothing is pending", func(t *testing.T) {
		st := joinedLearnerState()
		st.EtcdPromotePending = false
		attempts := stubLearnerPromotion(t, st, nil, func() (ha.LearnerState, error) { return ha.LearnerPromoted, nil })
		var look learnerLook
		if !promoteLearnerOnce(&look) || *attempts != 0 {
			t.Fatalf("a member nothing records pending is not the task's: attempts = %d", *attempts)
		}
	})
}

// TestPromoteLearnerOnceKeepsARejoinsRecord: the promote runs without the
// etcd data lock, so a snapshot rejoin can land meanwhile and clear the
// pending record itself. The look then has nothing to record and is done.
func TestPromoteLearnerOnceKeepsARejoinsRecord(t *testing.T) {
	rejoined := joinedLearnerState()
	rejoined.EtcdPromotePending = false
	rejoined.EtcdInitialCluster = grownThree + ",cp4=https://10.0.0.4:2380"
	stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) {
		if err := SaveState(rejoined); err != nil {
			t.Fatal(err)
		}
		return ha.LearnerPromoted, nil
	})
	var look learnerLook
	if !promoteLearnerOnce(&look) {
		t.Fatal("a look whose promote a rejoin overtook is not done")
	}
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.EtcdPromotePending || st.EtcdInitialCluster != rejoined.EtcdInitialCluster {
		t.Fatalf("saved state = %+v, want the rejoin's record untouched", st)
	}
}

// TestPromoteLearnerWaitsAFullPollAfterASlowLook: a look whose etcd calls
// take longer than the poll still leaves a full poll before the next one, so
// an operation that needs the etcd data lock gets a window to take it.
func TestPromoteLearnerWaitsAFullPollAfterASlowLook(t *testing.T) {
	stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) { return ha.LearnerWaiting, nil })
	var starts, ends []time.Time
	promoteLocalLearner = func(string, string) (ha.LearnerState, error) {
		starts = append(starts, time.Now())
		time.Sleep(2 * learnerPromotePoll)
		ends = append(ends, time.Now())
		return ha.LearnerWaiting, nil
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { promoteLearner(ctx); close(done) }()
		time.Sleep(10 * learnerPromotePoll)
		synctest.Wait()
		cancel()
		<-done
		if len(starts) < 2 {
			t.Fatalf("looks = %d, want several", len(starts))
		}
		for i := 1; i < len(starts) && i <= len(ends); i++ {
			if gap := starts[i].Sub(ends[i-1]); gap < learnerPromotePoll {
				t.Errorf("look %d started %v after the last one ended, want at least %v", i+1, gap, learnerPromotePoll)
			}
		}
	})
}

// TestPromoteLearnerRetriesUntilPromoted drives the task: it looks again every
// poll, through refusals, until the learner is promoted, and then holds until
// shutdown without looking again.
func TestPromoteLearnerRetriesUntilPromoted(t *testing.T) {
	answers := []error{errors.New("refused"), errors.New("refused"), nil}
	attempts := stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) {
		return ha.LearnerWaiting, nil
	})
	n := 0
	promoteLocalLearner = func(string, string) (ha.LearnerState, error) {
		*attempts++
		if n < len(answers)-1 {
			n++
			return ha.LearnerWaiting, answers[n-1]
		}
		return ha.LearnerPromoted, nil
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { promoteLearner(ctx); close(done) }()
		time.Sleep(10 * learnerPromotePoll)
		synctest.Wait()
		if *attempts != len(answers) {
			t.Errorf("promote attempts = %d, want %d: one per poll until promoted, none after", *attempts, len(answers))
		}
		if pendingOnDisk(t) {
			t.Error("the pending record survived the promote")
		}
		cancel()
		<-done
	})
}

// TestPromoteLearnerWarnsOnceWhenALearnerKeepsWaiting: etcd allows one
// learner at a time, so a learner that never catches up blocks every other
// control-plane join. The task stays quiet through the ordinary catch-up, and
// once the learner has waited learnerWarnAfter it says so, once, with etcd's
// reason.
func TestPromoteLearnerWarnsOnceWhenALearnerKeepsWaiting(t *testing.T) {
	reason := fmt.Errorf("%w: this node's etcd does not answer: connection refused", ha.ErrLearnerWaiting)
	stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) { return ha.LearnerWaiting, reason })
	run := func(d time.Duration) string {
		return captureWorkerOutput(t, func() {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { promoteLearner(ctx); close(done) }()
				time.Sleep(d)
				synctest.Wait()
				cancel()
				<-done
			})
		})
	}
	if out := run(learnerWarnAfter - 2*learnerPromotePoll); strings.Contains(out, "still a learner after") {
		t.Errorf("warned before the learner had waited %v:\n%s", learnerWarnAfter, out)
	}
	out := run(3 * learnerWarnAfter)
	if got := strings.Count(out, "still a learner after"); got != 1 {
		t.Fatalf("printed %d still-a-learner warnings over %v, want 1:\n%s", got, 3*learnerWarnAfter, out)
	}
	if !strings.Contains(out, "connection refused") || !strings.Contains(out, "no other control plane can join") {
		t.Errorf("the warning lacks etcd's reason or what it blocks:\n%s", out)
	}
}

// TestPromoteLearnerWarnsWhenNodeStateCannotBeRead: a node state the task
// cannot read leaves the learner unpromoted for as long as it lasts, so it is
// printed, once.
func TestPromoteLearnerWarnsWhenNodeStateCannotBeRead(t *testing.T) {
	attempts := stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) { return ha.LearnerPromoted, nil })
	if err := os.WriteFile(nodestate.StatePath(), []byte("role: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var look learnerLook
	out := captureWorkerOutput(t, func() {
		for range 3 {
			if promoteLearnerOnce(&look) {
				t.Fatal("a look that could not read node state is done")
			}
		}
	})
	if *attempts != 0 {
		t.Errorf("promote attempts = %d, want none without node state", *attempts)
	}
	if got := strings.Count(out, "read node state to promote this node's etcd learner"); got != 1 {
		t.Errorf("printed the unreadable node state %d times over 3 looks, want 1:\n%s", got, out)
	}
}

// TestPostStartPromotesOnlyARecordedLearner: the learner task runs on a
// control plane whose node state records a pending promote, and on no other.
func TestPostStartPromotesOnlyARecordedLearner(t *testing.T) {
	fakeHostKubectl(t)
	cfg := loadClusterConfig(t, "spec: {}\n")
	for _, pending := range []bool{true, false} {
		st := joinedLearnerState()
		st.EtcdPromotePending = pending
		stubLoops(t, time.Hour)
		attempts := stubLearnerPromotion(t, st, nil, func() (ha.LearnerState, error) { return ha.LearnerWaiting, nil })
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			g := newRunGroup(ctx)
			postStart(g, cfg, st)
			time.Sleep(learnerPromotePoll / 2)
			synctest.Wait()
			if got := *attempts > 0; got != pending {
				t.Errorf("pending = %v: promote attempts = %d", pending, *attempts)
			}
			cancel()
			g.wait()
		})
	}
}
