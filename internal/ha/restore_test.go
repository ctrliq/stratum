package ha

import (
	"io"

	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestRejoinReachesTheSeedBeforeWipingAnything is the ordering property.
//
// RejoinEtcd's wipe is unconditional: it stops etcd and removes the data
// directory, and only afterwards does MemberAdd contact the seed. A wrong
// or unreachable --cp1 must not destroy this node's etcd data only to
// discover afterwards that it had nowhere to rejoin. Asserting that a
// probe exists proves nothing -- a probe below the wipe would satisfy that
// and protect nothing. What this pins is the order.
func TestRejoinReachesTheSeedBeforeWipingAnything(t *testing.T) {
	t.Run("an unreachable seed leaves local state alone", func(t *testing.T) {
		wiped := false
		restoreSeams(t, func() error { return nil },
			func(string) error { return errors.New("connection refused") },
			func(w io.Writer) error { wiped = true; return nil })

		err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false)
		if err == nil {
			t.Fatal("an unreachable seed was accepted")
		}
		if wiped {
			t.Error("the etcd data directory was wiped before the seed was reached")
		}
		if !strings.Contains(err.Error(), "left alone") {
			t.Errorf("error %q does not tell the operator their data survived", err)
		}
	})

	t.Run("the PKI preflight still runs first", func(t *testing.T) {
		reached, wiped := false, false
		restoreSeams(t, func() error { return errors.New("no trust bundle") },
			func(string) error { reached = true; return nil },
			func(w io.Writer) error { wiped = true; return nil })

		if err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false); err == nil {
			t.Fatal("a missing trust bundle was accepted")
		}
		if reached || wiped {
			t.Errorf("work continued past the PKI preflight: reached=%v wiped=%v", reached, wiped)
		}
	})
}

func TestRejoinRefusesMissingStateBeforeWipeOrMemberAdd(t *testing.T) {
	wiped := false
	restoreSeams(t, func() error { return nil }, func(string) error { return nil },
		func(w io.Writer) error { wiped = true; return nil })
	rejoinLoadState = func() (*nodestate.NodeState, error) { return nil, errors.New("missing state") }
	if err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false); err == nil || !strings.Contains(err.Error(), "missing state") {
		t.Fatalf("missing state: %v", err)
	}
	if wiped {
		t.Fatal("wiped local etcd before verifying saved node state")
	}
}

// TestRejoinRefusesANonControllerBeforeAnything: a worker, or
// an empty node.yaml, is not a rejoin target. The refusal comes straight after
// the lock, before the supervisor, PKI or seed preflights, and nothing is
// wiped or added.
func TestRejoinRefusesANonControllerBeforeAnything(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *nodestate.NodeState
		role string
	}{
		{"a worker", &nodestate.NodeState{Role: "worker", EtcdName: "w1"}, `"worker"`},
		{"an empty node.yaml", &nodestate.NodeState{}, `""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			touched := []string{}
			restoreSeams(t,
				func() error { touched = append(touched, "pki"); return nil },
				func(string) error { touched = append(touched, "seed"); return nil },
				func(w io.Writer) error { touched = append(touched, "wipe"); return nil })
			rejoinSupervisorRunning = func() bool { touched = append(touched, "supervisor"); return true }
			rejoinLoadState = func() (*nodestate.NodeState, error) { return tc.st, nil }

			err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false)
			if err == nil || !strings.Contains(err.Error(), "role "+tc.role+", not controller") {
				t.Fatalf("rejoin of %s = %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), "Nothing was changed") {
				t.Errorf("the refusal does not say nothing was changed: %v", err)
			}
			if len(touched) != 0 {
				t.Errorf("work ran before the role check: %v", touched)
			}
		})
	}
}

func TestRejoinAcquiresDataLockBeforePreflight(t *testing.T) {
	oldLock, oldSupervisor := rejoinAcquireDataLock, rejoinSupervisorRunning
	t.Cleanup(func() { rejoinAcquireDataLock, rejoinSupervisorRunning = oldLock, oldSupervisor })
	rejoinAcquireDataLock = func() (func(), error) { return nil, errors.New("data lock busy") }
	rejoinSupervisorRunning = func() bool { t.Fatal("checked supervisor before the data lock"); return false }
	if err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false); err == nil || !strings.Contains(err.Error(), "data lock busy") {
		t.Fatalf("busy data lock: %v", err)
	}
}

func TestRejoinHoldsDataLockThroughWipeAndReleasesOnError(t *testing.T) {
	held := false
	wipeErr := errors.New("wipe stopped")
	restoreSeams(t, func() error { return nil }, func(string) error { return nil },
		func(w io.Writer) error {
			if !held {
				t.Fatal("wiped etcd after releasing the data lock")
			}
			return wipeErr
		})
	rejoinAcquireDataLock = func() (func(), error) {
		held = true
		return func() { held = false }, nil
	}
	rejoinLoadState = func() (*nodestate.NodeState, error) {
		return &nodestate.NodeState{Role: "controller"}, nil
	}
	if err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false); !errors.Is(err, wipeErr) {
		t.Fatalf("rejoin wipe error = %v", err)
	}
	if held {
		t.Fatal("data lock not released after wipe failure")
	}
}

func restoreSeams(t *testing.T, pkiFn func() error, reachFn func(string) error, wipeFn func(io.Writer) error) {
	t.Helper()
	saveRejoinSeams(t)
	// Asserted, not skipped: a machine without a supervisor is where this
	// test normally runs, and skipping there reads the same as passing.
	rejoinSupervisorRunning = func() bool { return true }
	rejoinAcquireDataLock = func() (func(), error) { return func() {}, nil }
	// A valid rejoin target unless a test says otherwise, so each test fails on
	// the preflight it is about and not on the role check that now runs first.
	rejoinLoadState = func() (*nodestate.NodeState, error) { return &nodestate.NodeState{Role: "controller"}, nil }
	rejoinHostname = func() (string, error) { return "cp2", nil }
	rejoinNodeIP = func() (string, error) { return "10.0.0.2", nil }
	rejoinPKIPreflight, rejoinWipeLocalState = pkiFn, wipeFn
	rejoinSeedMembers = func(ip string) (etcdMembership, error) {
		if err := reachFn(ip); err != nil {
			return etcdMembership{}, err
		}
		return etcdMembership{ClusterID: 0xc1, Quorum: true, Members: []etcdMember{seedMember}}, nil
	}
	// Nothing past the preflights reaches a real etcd from these tests.
	rejoinLocalStatus = func(string) (localEtcdStatus, error) { return localEtcdStatus{}, errors.New("connection refused") }
	rejoinMemberHealthy = func(string) (localEtcdStatus, bool) { return localEtcdStatus{}, false }
	rejoinAddLearner = func(io.Writer, string, string, string) (uint64, string, error) {
		return 0, "", errors.New("unexpected add")
	}
	rejoinRemoveMember = func(string, uint64) error { return errors.New("unexpected remove") }
	rejoinPromote = func(string, uint64) error { return errors.New("unexpected promote") }
	rejoinSaveState = func(*nodestate.NodeState) error { return nil }
	rejoinStartPod = func(string) error { return nil }
	rejoinStopPod = func(string) error { return nil }
	rejoinEtcdRunning = func() bool { return false }
	rejoinDataMark = func() etcdDataMark { return etcdDataMark{} }
	// A seed that is another node, a node whose cluster has lost quorum, and an
	// etcd data dir holding member data, so the wipe guards run and pass; the
	// tests about those guards say otherwise.
	rejoinSeedIsThisNode = func(string, []string) (bool, error) { return false, nil }
	rejoinConfirmQuorumLost = func() error { return nil }
	rejoinHoldsMemberData = func() (bool, error) { return true, nil }
}

var seedMember = etcdMember{ID: 1, Name: "cp1", PeerURLs: []string{"https://10.0.0.1:2380"},
	ClientURLs: []string{"https://10.0.0.1:2379"}}

// saveRejoinSeams puts every rejoin seam back when the test ends.
func saveRejoinSeams(t *testing.T) {
	t.Helper()
	oldSup, oldPKI := rejoinSupervisorRunning, rejoinPKIPreflight
	oldSeed, oldWipe := rejoinSeedMembers, rejoinWipeLocalState
	oldLock, oldLoad := rejoinAcquireDataLock, rejoinLoadState
	oldHost, oldIP := rejoinHostname, rejoinNodeIP
	oldLocal, oldHealthy := rejoinLocalStatus, rejoinMemberHealthy
	oldAdd, oldRemove, oldPromote := rejoinAddLearner, rejoinRemoveMember, rejoinPromote
	oldSave, oldStart := rejoinSaveState, rejoinStartPod
	oldNow, oldSleep := rejoinNow, rejoinSleep
	oldStop, oldRunning, oldMark := rejoinStopPod, rejoinEtcdRunning, rejoinDataMark
	oldQuorum, oldSelf, oldHeld := rejoinConfirmQuorumLost, rejoinSeedIsThisNode, rejoinHoldsMemberData
	t.Cleanup(func() {
		rejoinSupervisorRunning, rejoinPKIPreflight = oldSup, oldPKI
		rejoinSeedMembers, rejoinWipeLocalState = oldSeed, oldWipe
		rejoinAcquireDataLock, rejoinLoadState = oldLock, oldLoad
		rejoinHostname, rejoinNodeIP = oldHost, oldIP
		rejoinLocalStatus, rejoinMemberHealthy = oldLocal, oldHealthy
		rejoinAddLearner, rejoinRemoveMember, rejoinPromote = oldAdd, oldRemove, oldPromote
		rejoinSaveState, rejoinStartPod = oldSave, oldStart
		rejoinNow, rejoinSleep = oldNow, oldSleep
		rejoinStopPod, rejoinEtcdRunning, rejoinDataMark = oldStop, oldRunning, oldMark
		rejoinConfirmQuorumLost, rejoinSeedIsThisNode, rejoinHoldsMemberData = oldQuorum, oldSelf, oldHeld
	})
}

// TestRejoinWipesOnlyOnConfirmedQuorumLoss pins the rejoin wipe guard: the
// wipe needs a confirmed loss of quorum in the local cluster. Anything short
// of that (it still has quorum, or that cannot be told) needs --force. Fails
// if the wipe runs without a confirmed quorum loss.
func TestRejoinWipesOnlyOnConfirmedQuorumLoss(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verdict  error // what the probe reports; nil is "quorum lost, confirmed"
		force    bool
		wantWipe bool
	}{
		{"quorum still held", errors.New("this node's etcd cluster still has quorum"), false, false},
		{"cannot tell", errors.New("cannot read this node's etcd membership"), false, false},
		{"quorum held, override", errors.New("this node's etcd cluster still has quorum"), true, true},
		{"cannot tell, override", errors.New("cannot read this node's etcd membership"), true, true},
		{"quorum confirmed lost", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wiped := false
			restoreSeams(t, func() error { return nil }, func(string) error { return nil },
				func(w io.Writer) error { wiped = true; return errors.New("stop after the wipe") })
			rejoinConfirmQuorumLost = func() error { return tc.verdict }

			err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", tc.force)
			if err == nil {
				t.Fatal("rejoin succeeded with every step stubbed to fail")
			}
			if wiped != tc.wantWipe {
				t.Fatalf("wiped = %v, want %v (err %v)", wiped, tc.wantWipe, err)
			}
			if !tc.wantWipe && (!strings.Contains(err.Error(), tc.verdict.Error()) || !strings.Contains(err.Error(), "--force")) {
				t.Errorf("refusal %q does not carry the probe's reason and the override", err)
			}
		})
	}
}

// TestRejoinRefusesItsOwnAddressBeforeAnything: the
// restore prints `rejoin --server https://<this-seed-ip>:6443 --yes --force` for
// the OTHER control planes. Pasted on the seed it passed every check, reached its
// own etcd, and with --force skipped the quorum check and wiped the restored data
// the cluster runs on. --server naming this node, by any address recorded for it,
// is refused first, with or without --force, before the supervisor, PKI or seed
// are looked at and before anything is wiped.
func TestRejoinRefusesItsOwnAddressBeforeAnything(t *testing.T) {
	for _, tc := range []struct {
		name  string
		to    string
		state *nodestate.NodeState
	}{
		{"the node address", "10.0.0.11", &nodestate.NodeState{Role: "controller"}},
		{"the address in node state", "10.0.0.12", &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.12"}},
		{"the kubelet node IP", "10.0.0.13", &nodestate.NodeState{Role: "controller", KubeletNodeIP: "10.0.0.13"}},
		// On a dual-stack node the kubelet node IP is a comma-joined v4,v6 pair, which
		// is not an IP literal; each half is this node's address.
		{"the v4 half of a dual-stack kubelet node IP", "10.0.0.14", &nodestate.NodeState{Role: "controller", KubeletNodeIP: "10.0.0.14,fd00::14"}},
		{"the v6 half of a dual-stack kubelet node IP", "fd00::14", &nodestate.NodeState{Role: "controller", KubeletNodeIP: "10.0.0.14,fd00::14"}},
		{"the v6 half, in another notation", "FD00:0:0:0:0:0:0:14", &nodestate.NodeState{Role: "controller", KubeletNodeIP: "10.0.0.14, fd00::14"}},
	} {
		for _, force := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "", true: " with --force"}[force], func(t *testing.T) {
				touched := []string{}
				restoreSeams(t,
					func() error { touched = append(touched, "pki"); return nil },
					func(string) error { touched = append(touched, "seed"); return nil },
					func(w io.Writer) error { touched = append(touched, "wipe"); return nil })
				rejoinSupervisorRunning = func() bool { touched = append(touched, "supervisor"); return true }
				rejoinLoadState = func() (*nodestate.NodeState, error) { return tc.state, nil }
				rejoinNodeIP = func() (string, error) { return "10.0.0.11", nil }
				rejoinSeedIsThisNode = func(string, []string) (bool, error) { touched = append(touched, "member-id"); return false, nil }

				err := RejoinEtcd(context.Background(), io.Discard, tc.to, force)
				if err == nil || !strings.Contains(err.Error(), "this node is the seed; run rejoin on the OTHER control planes") {
					t.Fatalf("rejoin --server %s = %v", tc.to, err)
				}
				if !strings.Contains(err.Error(), "Nothing was changed") {
					t.Errorf("the refusal does not say nothing was changed: %v", err)
				}
				if len(touched) != 0 {
					t.Errorf("work ran before the guard: %v", touched)
				}
			})
		}
	}
}

// TestRejoinForceRefusesOnlyTheSeedBeingThisNode: --force lifts the quorum check, so
// it must not also lift the question of whether --server is this very node under
// a name the own-address check does not know. The seed answering as this node's
// own etcd member is refused before the wipe. A different member, even of the same
// cluster, is not: that is the documented repair of a diverged member against its
// healthy majority, and the retry of a rejoin that joined and then failed to start
// the apiserver. A local etcd that cannot be read, which is the half-joined
// re-run --force is for, is not refused either.
func TestRejoinForceRefusesOnlyTheSeedBeingThisNode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		self     bool
		selfErr  error
		force    bool
		wantWipe bool
		wantCall bool // whether the seed's identity is read at all
		wantIn   string
	}{
		{"the seed is this node's own member, --force", true, nil, true, false, true, "this node is the seed"},
		{"the seed is another member, --force", false, nil, true, true, true, ""},
		{"the seed's identity unreadable, --force", false, errors.New("deadline exceeded"), true, false, true, "left alone"},
		{"the seed is this node's own member, no --force: the quorum check decides", true, nil, false, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wiped, called := false, false
			restoreSeams(t, func() error { return nil }, func(string) error { return nil },
				func(w io.Writer) error { wiped = true; return errors.New("stop after the wipe") })
			rejoinSeedIsThisNode = func(string, []string) (bool, error) { called = true; return tc.self, tc.selfErr }

			err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", tc.force)
			if err == nil {
				t.Fatal("rejoin succeeded with every step stubbed to fail")
			}
			if wiped != tc.wantWipe {
				t.Fatalf("wiped = %v, want %v (err %v)", wiped, tc.wantWipe, err)
			}
			if called != tc.wantCall {
				t.Errorf("seed identity read = %v, want %v", called, tc.wantCall)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal %q lacks %q", err, tc.wantIn)
			}
			if !tc.wantWipe && !strings.Contains(err.Error(), "left alone") && !strings.Contains(err.Error(), "Nothing was changed") {
				t.Errorf("refusal %q does not say the data was not touched", err)
			}
		})
	}
}

// TestOwnAddressSplitsADualStackPair: only a name this node does not have is
// someone else's.
func TestOwnAddressSplitsADualStackPair(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"10.0.0.11", true}, {"fd00::11", true}, {"10.0.0.12", false}, {"fd00::12", false}, {"", false}, {"not-an-ip", false},
	} {
		if got := ownAddress(tc.addr, "", "10.0.0.11,fd00::11"); got != tc.want {
			t.Errorf("ownAddress(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestRespondingMemberIsOwn: the seed is identified by the peer URLs of the member
// that answered, and this node's own etcd is not read at all, so a survivor that
// restarted without quorum and answers nothing is not in the way. Everything
// that must go ahead under --force shares this node's cluster ID or none; only
// the member decides.
func TestRespondingMemberIsOwn(t *testing.T) {
	own := []string{"10.0.0.11", "", "10.0.0.11,fd00::11"}
	member := func(id uint64, peers ...string) *etcdserverpb.Member {
		return &etcdserverpb.Member{ID: id, PeerURLs: peers}
	}
	list := func(responding uint64, ms ...*etcdserverpb.Member) *clientv3.MemberListResponse {
		return &clientv3.MemberListResponse{Header: &etcdserverpb.ResponseHeader{MemberId: responding}, Members: ms}
	}
	for _, tc := range []struct {
		name    string
		ml      *clientv3.MemberListResponse
		want    bool
		wantErr bool
	}{
		{"an alias of this node: its member answers on this node's peer address",
			list(1, member(1, "https://10.0.0.11:2380"), member(2, "https://10.0.0.12:2380")), true, false},
		{"an alias of this node, by the v6 half of a dual-stack address",
			list(1, member(1, "https://[fd00::11]:2380")), true, false},
		{"the restored seed: another node's peer address",
			list(2, member(1, "https://10.0.0.11:2380"), member(2, "https://10.0.0.12:2380")), false, false},
		{"repair: another member of the same healthy cluster",
			list(3, member(1, "https://10.0.0.11:2380"), member(2, "https://10.0.0.12:2380"), member(3, "https://10.0.0.13:2380")), false, false},
		{"retry: the member this node re-added is listed too, and another member answers",
			list(2, member(1, "https://10.0.0.11:2380"), member(2, "https://10.0.0.12:2380")), false, false},
		{"a member with no peer URLs",
			list(2, member(1, "https://10.0.0.11:2380"), member(2)), false, true},
		{"a hostname peer URL",
			list(2, member(1, "https://10.0.0.11:2380"), member(2, "https://cp2.example:2380")), false, true},
		{"an unparseable peer URL",
			list(2, member(1, "https://10.0.0.11:2380"), member(2, "https://10.0.0.12:bad:2380")), false, true},
		{"a hostname peer URL after an IP one that is not this node's",
			list(2, member(2, "https://10.0.0.12:2380", "https://localhost:2380")), false, true},
		{"the responding member missing from the list",
			list(9, member(1, "https://10.0.0.11:2380"), member(2, "https://10.0.0.12:2380")), false, true},
		{"a zero responding member",
			list(0, member(1, "https://10.0.0.11:2380")), false, true},
		{"an empty list", list(1), false, true},
		{"no header", &clientv3.MemberListResponse{Members: []*etcdserverpb.Member{member(1, "https://10.0.0.11:2380")}}, false, true},
		{"no reply", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := respondingMemberIsOwn(tc.ml, "10.255.255.1", own)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("own=%v err=%v, want own=%v err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestRejoinForceUsesOnlyTheSeedsAnswer: through RejoinEtcd, the seam is handed this
// node's addresses and nothing about this node's own etcd is consulted, so a
// rebooted survivor, whose etcd never answers, goes on to the wipe against the
// restored seed, and the seed being this node is refused before it.
func TestRejoinForceUsesOnlyTheSeedsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		self     bool
		wantWipe bool
	}{
		{"a rebooted survivor against the restored seed", false, true},
		{"the seed is this node by another name", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wiped := false
			var gotOwn []string
			restoreSeams(t, func() error { return nil }, func(string) error { return nil },
				func(w io.Writer) error { wiped = true; return errors.New("stop after the wipe") })
			rejoinLoadState = func() (*nodestate.NodeState, error) {
				return &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11,fd00::11"}, nil
			}
			rejoinNodeIP = func() (string, error) { return "10.0.0.11", nil }
			rejoinSeedIsThisNode = func(_ string, own []string) (bool, error) { gotOwn = own; return tc.self, nil }
			err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", true)
			if err == nil {
				t.Fatal("rejoin succeeded with every step stubbed to fail")
			}
			if wiped != tc.wantWipe {
				t.Fatalf("wiped = %v, want %v (err %v)", wiped, tc.wantWipe, err)
			}
			if len(gotOwn) != 3 || gotOwn[0] != "10.0.0.11" || gotOwn[2] != "10.0.0.11,fd00::11" {
				t.Errorf("own addresses handed to the seed check = %q", gotOwn)
			}
			if !tc.wantWipe && !strings.Contains(err.Error(), "this node is the seed") {
				t.Errorf("refusal %q", err)
			}
		})
	}
}
