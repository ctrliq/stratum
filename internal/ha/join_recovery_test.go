package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

func TestJoinResumesCommittedMemberWithoutQuorum(t *testing.T) {
	joinFixture(t)
	etcdDir := t.TempDir()
	r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"),
		quorumHeldErr: errors.New("one running voter out of two")}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
		t.Fatal("the first join must fail after adding its member")
	}
	members := listedMembers()
	members.Quorum = false
	r2 := &recordedJoin{listMembers: members, failProbe: errors.New("no quorum")}
	fx := fakeJoinEffects(r2)
	fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
		t.Error("committed-member recovery attempted a Lease API write without quorum")
		return nil, errors.New("API unavailable without quorum")
	}
	fx.discover = func(string, string, string) ([]byte, error) {
		t.Error("resume attempted API discovery although the local preparation is recorded")
		return nil, errors.New("API unavailable without quorum")
	}
	fx.downloadGrant = func(string, string, func(bundleProfile) error) (bundleGrant, error) {
		t.Error("resume tried to read the shared-certificate Secret without quorum")
		return bundleGrant{}, errors.New("API unavailable without quorum")
	}
	fx.controllerNodeIPs = func(context.Context, string, []string) ([]string, error) {
		t.Error("resume tried to read node addresses without quorum")
		return nil, errors.New("API unavailable without quorum")
	}
	st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fx)
	if err != nil {
		t.Fatalf("resume of the committed member refused: %v", err)
	}
	if st.EtcdInitialCluster == "" || r2.addCalls != 0 || r2.removeByIDCalls != 0 || r2.removeCalled {
		t.Fatalf("resume must retain membership and build the initial cluster: state=%+v add=%d remove=%d", st, r2.addCalls, r2.removeByIDCalls)
	}
	// The journal survives the resumed save too: it ends when a later start
	// confirms the node's local etcd data, not when node.yaml lands.
	if _, err := os.Stat(joinIntentPath); err != nil {
		t.Fatalf("completed resume lost its journal: %v", err)
	}
}

func TestJoinResumesAnInheritedVIPWithoutQuorum(t *testing.T) {
	for _, tc := range []struct {
		name          string
		profile       string
		resumeProfile string
		configuredVIP nodestate.VIP
		resumeVIP     nodestate.VIP
		wantRefusal   bool
	}{
		{name: "repeat the original omitted VIP"},
		{name: "named profile with inherited VIP", profile: "hardened", resumeProfile: "hardened"},
		{name: "changed profile is refused", profile: "hardened", resumeProfile: "other", wantRefusal: true},
		{name: "an explicit matching VIP", resumeVIP: nodestate.VIP{Address: "10.0.0.10"}},
		{name: "an explicit conflicting VIP", resumeVIP: nodestate.VIP{Address: "10.0.0.99"}, wantRefusal: true},
		{name: "configured local interface and BGP", configuredVIP: nodestate.VIP{Interface: "ens3", BGP: true}, resumeVIP: nodestate.VIP{Interface: "ens3", BGP: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			if tc.profile != "" {
				// The named-profile subtests run against a binary's registry:
				// both the first attempt's and the resume's profile must be
				// registered for profile.ForNode to resolve them, as a real
				// binary registers its profiles at the root command.
				prev := profile.All()
				names := []profile.Profile{{Name: "cis"}, {Name: tc.profile}}
				if tc.resumeProfile != tc.profile {
					names = append(names, profile.Profile{Name: tc.resumeProfile})
				}
				if err := profile.SetRegistered(names); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = profile.SetRegistered(prev) })
			}
			dir := t.TempDir()
			opts := freshJoinOpts()
			opts.VIP = tc.configuredVIP
			opts.Profile = tc.profile
			first := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"), quorumHeldErr: errors.New("no quorum")}
			firstFX := fakeJoinEffects(first)
			firstFX.downloadGrant = vipGrant([]string{"10.0.0.11"}, "10.0.0.10")
			if _, err := prepareControlPlaneJoin(context.Background(), opts, dir, firstFX); err == nil || first.addCalls != 1 {
				t.Fatalf("first attempt must fail after member admission: adds=%d err=%v", first.addCalls, err)
			}
			members := listedMembers()
			members.Quorum = false
			resume := &recordedJoin{listMembers: members, failProbe: errors.New("no quorum")}
			resumeFX := fakeJoinEffects(resume)
			resumeFX.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
				t.Error("committed-member recovery attempted a Lease API write without quorum")
				return nil, errors.New("API unavailable without quorum")
			}
			resumeFX.discover = func(string, string, string) ([]byte, error) {
				t.Fatal("recovery attempted online discovery without quorum")
				return nil, nil
			}
			resumeFX.downloadGrant = func(string, string, func(bundleProfile) error) (bundleGrant, error) {
				t.Fatal("recovery attempted to download the bundle without quorum")
				return bundleGrant{}, nil
			}
			opts.VIP = tc.resumeVIP
			opts.Profile = tc.resumeProfile
			st, err := prepareControlPlaneJoin(context.Background(), opts, dir, resumeFX)
			if tc.wantRefusal {
				if err == nil || !strings.Contains(err.Error(), "options differ") {
					t.Fatalf("conflicting VIP must refuse recovery: %v", err)
				}
				if resume.certCalled || resume.kcCalled || resume.addCalls != 0 || resume.saved != nil {
					t.Fatal("conflicting recovery changed certificates, kubeconfigs, or membership")
				}
				return
			}
			if err != nil {
				t.Fatalf("resume with inherited VIP: %v", err)
			}
			if st.ProfileName() != tc.profile {
				t.Errorf("resumed profile = %q, want %q", st.ProfileName(), tc.profile)
			}
			wantVIP := tc.configuredVIP
			wantVIP.Address = "10.0.0.10"
			const wantServer = "https://10.0.0.10:6443"
			if st.VIP != wantVIP || st.Server != wantServer || resume.kcServer != wantServer || !resume.kcVIP {
				t.Errorf("resumed state VIP=%+v server=%q, kubeconfigs server=%q vip=%v; want VIP=%+v server=%q", st.VIP, st.Server, resume.kcServer, resume.kcVIP, wantVIP, wantServer)
			}
			if resume.certSANs == nil || !slices.Contains(resume.certSANs.IPs, "10.0.0.10") {
				t.Errorf("resumed certificates lost inherited VIP: %+v", resume.certSANs)
			}
			if resume.addCalls != 0 || resume.removeByIDCalls != 0 || resume.removeCalled {
				t.Fatal("recovery changed existing membership")
			}
		})
	}
}

func TestJoinCompletesCommittedCancellationAndResumes(t *testing.T) {
	for _, ip := range []string{"192.0.2.9", "fd00::9"} {
		t.Run(ip, func(t *testing.T) {
			joinFixture(t)
			dir := t.TempDir()
			members := listedMembers()
			peer := nodestate.EtcdPeerURL(ip)
			members.Members[1].PeerURLs = []string{peer}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &recordedJoin{detectIP: ip, listMembers: members}
			fx := fakeJoinEffects(r)
			fx.memberAdd = func(_ []string, name, gotPeer string) (string, error) {
				r.addCalls++
				if gotPeer != peer {
					t.Fatalf("member-add peer = %q, want %q", gotPeer, peer)
				}
				cancel() // cancellation arrives after the remote membership commit
				return initialClusterFor(members.Members, peer, name), nil
			}
			fx.saveState = saveJoinState
			if _, err := prepareControlPlaneJoin(ctx, freshJoinOpts(), dir, fx); !errors.Is(err, context.Canceled) {
				t.Fatalf("committed cancellation = %v, want context.Canceled", err)
			}
			st, err := nodestate.LoadState()
			if err != nil || !strings.Contains(st.EtcdInitialCluster, "="+peer) {
				t.Fatalf("cancellation lost committed state: %+v, %v", st, err)
			}
			j, err := readJoinIntent()
			if err != nil || j == nil || j.Recovery == nil || j.PeerURL != peer {
				t.Fatalf("cancellation lost recovery intent: %+v, %v", j, err)
			}
			members.Quorum = false
			resume := &recordedJoin{detectIP: ip, listMembers: members}
			resumeFX := fakeJoinEffects(resume)
			resumeFX.saveState = saveJoinState
			resumeFX.discover = func(string, string, string) ([]byte, error) {
				t.Fatal("recovery attempted online discovery without quorum")
				return nil, nil
			}
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, resumeFX); err != nil {
				t.Fatal(err)
			}
			if r.addCalls != 1 || resume.addCalls != 0 || resume.removeByIDCalls != 0 {
				t.Fatalf("recovery changed membership: first adds=%d, resume adds=%d/removes=%d", r.addCalls, resume.addCalls, resume.removeByIDCalls)
			}
		})
	}
}

func TestJoinCannotDiscardIntentOnQuorumlessAbsence(t *testing.T) {
	joinFixture(t)
	etcdDir := t.TempDir()
	r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"),
		quorumHeldErr: errors.New("no quorum")}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
		t.Fatal("the first join must fail")
	}
	r2 := &recordedJoin{listMembers: listedMembers()}
	r2.listMembers.Quorum = false
	r2.listMembers.Members = r2.listMembers.Members[:1]
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r2))
	if err == nil || !strings.Contains(err.Error(), "quorum") || !strings.Contains(err.Error(), concurrentJoinHint) {
		t.Fatalf("quorumless absence must refuse, with the concurrent-join hint, rather than establish stale intent: %v", err)
	}
	if r2.addCalls != 0 || r2.removeByIDCalls != 0 {
		t.Fatalf("quorumless absence changed membership: add=%d remove=%d", r2.addCalls, r2.removeByIDCalls)
	}
	j, err := readJoinIntent()
	if err != nil || j == nil || j.MemberID != intentMemberID {
		t.Fatalf("refusal lost the original journal: %+v %v", j, err)
	}
}

func TestAmbiguousJoinDoesNotRetryOnQuorumlessAbsence(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{failAdd: context.DeadlineExceeded, listMembers: listedMembers()}
	r.listMembers.Quorum = false
	r.listMembers.Members = r.listMembers.Members[:1]
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
	if err == nil || r.addCalls != 1 {
		t.Fatalf("quorumless absence cannot prove the first add failed: error=%v adds=%d", err, r.addCalls)
	}
	if _, err := os.Stat(joinIntentPath); err != nil {
		t.Fatalf("ambiguous add must keep recovery journal: %v", err)
	}
}

func TestJoinKeepsMembershipWhenPublishedStateSyncFails(t *testing.T) {
	for _, failOn := range []string{"node.yaml", "directory", "none"} {
		t.Run(failOn, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{listMembers: listedMembers()}
			fx := fakeJoinEffects(r)
			var synced []string
			fx.saveState = func(st *nodestate.NodeState) error {
				return saveJoinStateWithSync(st, func(path string) error {
					if _, err := os.Stat(joinIntentPath); err != nil {
						t.Fatalf("journal cleared before state durability: %v", err)
					}
					synced = append(synced, path)
					if failOn == filepath.Base(path) || (failOn == "directory" && path == version.K8sConfDir) {
						return errors.New("sync EIO")
					}
					return syncJoinFile(path)
				})
			}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx)
			if failOn == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(synced, []string{nodestate.StatePath(), version.K8sConfDir}) {
					t.Fatalf("durability order: %v", synced)
				}
				// A durable save does not end the journal: it survives until a
				// later start confirms the node's local etcd data.
				if _, statErr := os.Stat(joinIntentPath); statErr != nil {
					t.Fatalf("completed save lost the journal: %v", statErr)
				}
			} else {
				if !errors.Is(err, errJoinStatePublished) {
					t.Fatalf("lost published-state error: %v", err)
				}
				if _, err := os.Stat(joinIntentPath); err != nil {
					t.Fatalf("failed durability lost journal: %v", err)
				}
				if r.removeCalled || r.removeByIDCalls != 0 || r.quorumHeldCalls != 0 {
					t.Fatal("published state must not refer to compensated membership")
				}
			}
			if st, err := nodestate.LoadState(); err != nil || st.EtcdInitialCluster == "" {
				t.Fatalf("published state missing: %+v %v", st, err)
			}
		})
	}
}

func TestJoinPreparationMustPersistBeforeAdd(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{}
	fx := fakeJoinEffects(r)
	fx.persistPreparation = func() error {
		if r.rtState == nil {
			t.Error("retained preparation synced before runtime configs existed")
		}
		return errors.New("preparation sync EIO")
	}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx); err == nil || !strings.Contains(err.Error(), "preparation sync EIO") {
		t.Fatalf("lost sync failure: %v", err)
	}
	if r.addCalls != 0 || r.saved != nil {
		t.Fatal("membership committed despite nondurable preparation")
	}
}

func TestJoinIntentCarriesPreparationBeforeAdd(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r)); err != nil {
		t.Fatal(err)
	}
	var j joinIntent
	if err := json.Unmarshal(r.intentAtAddRaw, &j); err != nil {
		t.Fatal(err)
	}
	if j.Recovery == nil || j.Recovery.CAHash != freshJoinOpts().CAHash ||
		!slices.Equal(j.Recovery.EtcdIPs, []string{"10.0.0.11"}) ||
		j.Recovery.State.NodeIP != "192.0.2.9" || j.Recovery.State.Domain() != "fuzz.internal" {
		t.Fatalf("pre-add journal cannot resume without API: %+v", j.Recovery)
	}
	var atSave joinIntent
	if err := json.Unmarshal(r.intentAtSaveRaw, &atSave); err != nil {
		t.Fatal(err)
	}
	if atSave.Recovery == nil {
		t.Fatal("post-add update lost preparation")
	}
}

func TestJoinMembershipPrefersQuorumButPreservesSerializableView(t *testing.T) {
	for _, quorum := range []bool{false, true} {
		got, err := joinMembershipVia([]string{"down", "survivor", "majority"}, func(ip string) (etcdMembership, error) {
			if ip == "down" {
				return etcdMembership{}, errors.New("unreachable")
			}
			m := listedMembers()
			m.Quorum = ip == "majority" && quorum
			return m, nil
		})
		if err != nil || got.Quorum != quorum || got.memberByPeerURL("https://192.0.2.9:2380") == nil {
			t.Fatalf("membership=%+v error=%v", got, err)
		}
	}
}

func TestJoinResumeTrustRequiresPinnedSharedKeys(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	var pin string
	for _, name := range []string{"ca", "front-proxy-ca", pki.EtcdCAName} {
		ca, err := pki.NewCA(name, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "ca" {
			pin = clusterinfo.PinFor(ca.Cert)
		}
	}
	if err := pki.EnsureSAKeypair(); err != nil {
		t.Fatal(err)
	}
	if err := verifyJoinResumeTrust(pin); err != nil {
		t.Fatalf("complete pinned material refused: %v", err)
	}
	if err := verifyJoinResumeTrust("sha256:" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("wrong cluster pin accepted")
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new.crt"), []byte("partial rotation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyJoinResumeTrust(pin); err == nil {
		t.Fatal("partial rotation trust accepted")
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "ca-new.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(version.PKIDir, pki.EtcdCAName+".key")); err != nil {
		t.Fatal(err)
	}
	if err := verifyJoinResumeTrust(pin); err == nil {
		t.Fatal("missing shared etcd signer accepted")
	}
}

func TestJoinPreparationSyncsParentDirectoryEntries(t *testing.T) {
	oldData, oldPKI, oldConfig, oldKube := version.DataDir, version.PKIDir, version.K8sConfDir, version.KubeconfigDir
	t.Cleanup(func() {
		version.DataDir, version.PKIDir, version.K8sConfDir, version.KubeconfigDir = oldData, oldPKI, oldConfig, oldKube
	})
	version.DataDir = filepath.Join(t.TempDir(), "new-node")
	version.PKIDir, version.K8sConfDir, version.KubeconfigDir = filepath.Join(version.DataDir, "pki"), filepath.Join(version.DataDir, "config"), filepath.Join(version.DataDir, "kubeconfig")
	for _, dir := range []string{version.PKIDir, version.K8sConfDir, version.KubeconfigDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "prepared"), []byte("prepared"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var synced []string
	if err := persistJoinPreparationWithSync(func(path string) error { synced = append(synced, path); return syncJoinFile(path) }); err != nil {
		t.Fatal(err)
	}
	if len(synced) < 2 || !slices.Equal(synced[len(synced)-2:], []string{version.DataDir, filepath.Dir(version.DataDir)}) {
		t.Fatalf("new directory entries were not synced after their contents: %v", synced)
	}
}

func TestJoinCannotCompensateOverPreviouslyPublishedState(t *testing.T) {
	joinFixture(t)
	st := &nodestate.NodeState{Role: "controller", EtcdState: "existing", EtcdName: "cp2", EtcdInitialCluster: "cp2=https://192.0.2.9:2380"}
	if err := saveJoinState(st); err != nil {
		t.Fatal(err)
	}
	// Make the next replacement fail after the state exists: with the state
	// directory read-only, SaveState cannot create its temp file (it writes a
	// uniquely named one, so occupying its old fixed .tmp name no longer
	// blocks a save). A failed replacement must be classified as published,
	// not compensated over.
	if err := os.Chmod(filepath.Dir(nodestate.StatePath()), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(nodestate.StatePath()), 0o700) })
	if err := saveJoinState(st); !errors.Is(err, errJoinStatePublished) {
		t.Fatalf("failed replacement must retain published membership: %v", err)
	}
}

func TestJoinResumeRefusesChangedIdentityAndTrust(t *testing.T) {
	for _, failure := range []string{"member ID", "CA pin", "local trust"} {
		t.Run(failure, func(t *testing.T) {
			joinFixture(t)
			dir := t.TempDir()
			r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"), quorumHeldErr: errors.New("no quorum")}
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("first join must fail")
			}
			r2 := &recordedJoin{listMembers: listedMembers()}
			fx, o := fakeJoinEffects(r2), freshJoinOpts()
			switch failure {
			case "member ID":
				r2.listMembers.Members[1].ID++
			case "CA pin":
				o.CAHash = "another-cluster-pin"
			case "local trust":
				fx.verifyResumeTrust = func(string) error { return errors.New("missing signer") }
			}
			if _, err := prepareControlPlaneJoin(context.Background(), o, dir, fx); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if r2.addCalls != 0 || r2.removeByIDCalls != 0 || r2.saved != nil {
				t.Fatal("refused recovery changed membership or saved state")
			}
			if j, err := readJoinIntent(); err != nil || j == nil || j.MemberID != intentMemberID {
				t.Fatalf("refusal lost original journal: %+v %v", j, err)
			}
		})
	}
}

func TestJoinRefreshesPreparationAfterQuorumConfirmsStaleIntent(t *testing.T) {
	joinFixture(t)
	dir := t.TempDir()
	r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"), quorumHeldErr: errors.New("no quorum")}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fakeJoinEffects(r1)); err == nil {
		t.Fatal("first join must fail")
	}
	r2 := &recordedJoin{}
	r2.clusterMembersFn = func() (etcdMembership, error) {
		m := listedMembers()
		if r2.addCalls == 0 {
			m.Members = m.Members[:1]
		}
		return m, nil
	}
	fx := fakeJoinEffects(r2)
	downloaded := false
	fenced := false
	fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
		fenced = true
		return &k8sutil.MaintenanceLease{Release: func() {}}, nil
	}
	download := fx.downloadGrant
	fx.downloadGrant = func(kc, key string, check func(bundleProfile) error) (bundleGrant, error) {
		downloaded = true
		return download(kc, key, check)
	}
	add := fx.memberAdd
	fx.memberAdd = func(ips []string, name, peer string) (string, error) {
		if !downloaded {
			t.Error("stale preparation reused for a new add instead of refreshing cluster configuration")
		}
		if !fenced {
			t.Error("a stale intent's replacement member bypassed the admission fence")
		}
		return add(ips, name, peer)
	}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fx); err != nil {
		t.Fatal(err)
	}
	if !downloaded || r2.addCalls != 1 {
		t.Fatalf("fresh preparation=%v add count=%d", downloaded, r2.addCalls)
	}
}

func TestFailedStaleJoinRefreshKeepsPublishedStateRecoverable(t *testing.T) {
	joinFixture(t)
	dir := t.TempDir()
	r1 := &recordedJoin{listMembers: listedMembers()}
	fx := fakeJoinEffects(r1)
	fx.saveState = func(st *nodestate.NodeState) error {
		return saveJoinStateWithSync(st, func(string) error { return errors.New("sync EIO") })
	}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fx); !errors.Is(err, errJoinStatePublished) {
		t.Fatalf("first join must leave published state: %v", err)
	}
	original, err := os.ReadFile(joinIntentPath)
	if err != nil {
		t.Fatal(err)
	}
	r2 := &recordedJoin{failProbe: errors.New("fresh probe failed"), listMembers: listedMembers()}
	r2.listMembers.Members = r2.listMembers.Members[:1]
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fakeJoinEffects(r2)); err == nil ||
		!strings.Contains(err.Error(), "fresh probe failed") || !strings.Contains(err.Error(), concurrentJoinHint) {
		t.Fatalf("fresh preparation did not refuse with the concurrent-join hint: %v", err)
	}
	retained, err := os.ReadFile(joinIntentPath)
	if err != nil || string(retained) != string(original) {
		t.Fatalf("failed refresh lost recovery marker: %v", err)
	}
	if r2.addCalls != 0 {
		t.Fatal("failed preparation added a new member")
	}
}

// TestResumedJoinRecordsTheControllerStartsServer: a join resumed from its
// recovery journal saves the server the controller start's rule gives
// (nodestate.ServerURL), even when the journal's recorded state carries the
// --join URL, as a journal an older build wrote does. The resume copies the
// recorded state whole, so a join that does not re-apply the rule after the
// copy saves the --join URL, and the first restart rewrites every kubeconfig.
func TestResumedJoinRecordsTheControllerStartsServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		vip  nodestate.VIP
		want string
	}{
		{"with a VIP", nodestate.VIP{Address: "10.0.0.10"}, fmt.Sprintf("https://10.0.0.10:%d", version.APIServerPort)},
		{"without a VIP", nodestate.VIP{}, fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rule := nodestate.ServerURL(tc.vip); rule != tc.want {
				t.Fatalf("nodestate.ServerURL(%+v) = %q, want %q", tc.vip, rule, tc.want)
			}
			joinFixture(t)
			etcdDir := t.TempDir()
			opts := freshJoinOpts()
			opts.Server = "https://10.0.0.11:6443"
			opts.VIP = tc.vip
			// The wedge: the first run adds its member and fails the state save
			// with quorum unconfirmed, so the journal survives as the recovery
			// record.
			r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"),
				quorumHeldErr: errors.New("no quorum")}
			if _, err := prepareControlPlaneJoin(context.Background(), opts, etcdDir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("the first join must fail after adding its member")
			}
			// The journal as an older build wrote it: its recorded state names
			// the --join URL as the server.
			j, err := readJoinIntent()
			if err != nil || j == nil || j.Recovery == nil {
				t.Fatalf("the failed join left no recovery journal: %+v, %v", j, err)
			}
			j.Recovery.State.Server = opts.Server
			if err := writeJoinIntent(*j); err != nil {
				t.Fatal(err)
			}

			r2 := &recordedJoin{listMembers: listedMembers()}
			st, err := prepareControlPlaneJoin(context.Background(), opts, etcdDir, fakeJoinEffects(r2))
			if err != nil {
				t.Fatalf("the resumed join refused: %v", err)
			}
			// The resume ran from the journal: no online discovery, no second add.
			if slices.Contains(r2.order, "discover") || r2.addCalls != 0 {
				t.Fatalf("the re-run did not resume from the journal: effects %v, member-adds %d", r2.order, r2.addCalls)
			}
			if r2.saved == nil {
				t.Fatal("the resumed join saved no node state")
			}
			if r2.saved.Server != tc.want {
				t.Errorf("the resumed join saved server %q, want the controller start's %q, not the journal's --join URL %q",
					r2.saved.Server, tc.want, opts.Server)
			}
			if st == nil || st.Server != tc.want {
				t.Errorf("the resumed join returned node state %+v, want server %q", st, tc.want)
			}
			if !r2.kcCalled || r2.kcServer != tc.want {
				t.Errorf("the resumed join wrote kubeconfigs against %q (called %v), want %q", r2.kcServer, r2.kcCalled, tc.want)
			}
			// The journal the resume leaves for a later resume records the
			// rule's server too, so the --join URL does not come back from it.
			var atSave joinIntent
			if err := json.Unmarshal(r2.intentAtSaveRaw, &atSave); err != nil {
				t.Fatal(err)
			}
			if atSave.Recovery == nil || atSave.Recovery.State.Server != tc.want {
				t.Errorf("the journal at the resumed save records %+v, want server %q", atSave.Recovery, tc.want)
			}
		})
	}
}

// TestJoinAdoptingTheJournalsMemberSkipsTheVIPCheck: a re-run that adopts the
// member its intent journal records, still listed by the cluster, adds nothing
// to etcd, so it does not check the VIP; a check that would refuse proves it
// never ran. That holds whether the journal carries the preparation's recovery
// record or was left before the preparation finished, without one.
func TestJoinAdoptingTheJournalsMemberSkipsTheVIPCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wedge func(t *testing.T, etcdDir string)
	}{
		{"a journal with its recovery record", func(t *testing.T, etcdDir string) {
			r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"),
				quorumHeldErr: errors.New("no quorum")}
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("the first join must fail after adding its member")
			}
		}},
		{"a journal without one", func(t *testing.T, _ string) {
			hostname, err := host.Hostname()
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJoinIntent(joinIntent{MemberName: hostname, PeerURL: "https://192.0.2.9:2380",
				MemberID: intentMemberID, Phase: intentPhaseAdded}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			etcdDir := t.TempDir()
			tc.wedge(t, etcdDir)
			if j, err := readJoinIntent(); err != nil || j == nil {
				t.Fatalf("no intent journal to adopt the member from: %+v, %v", j, err)
			}
			r := &recordedJoin{listMembers: listedMembers(), failVIP: errors.New("the VIP does not serve this cluster")}
			st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r))
			if err != nil {
				t.Fatalf("the re-run refused to adopt the recorded member: %v", err)
			}
			if r.vipCalls != 0 {
				t.Errorf("the adopting re-run checked the VIP %d times; adopting the recorded member adds nothing to etcd", r.vipCalls)
			}
			if r.addCalls != 0 || st == nil || !strings.Contains(st.EtcdInitialCluster, "=https://192.0.2.9:2380") {
				t.Errorf("the re-run did not adopt the recorded member: member-adds=%d state=%+v", r.addCalls, st)
			}
		})
	}
}

// TestStaleJoinRefreshRefusedByTheVIPCheckKeepsTheJournal: a re-run whose
// journal records a member the cluster no longer lists refreshes its
// preparation and proceeds as a fresh member-add, so it checks the VIP. A VIP
// that does not serve the cluster refuses that add before the journal is
// replaced: the journal stays byte for byte the recovery record it was.
func TestStaleJoinRefreshRefusedByTheVIPCheckKeepsTheJournal(t *testing.T) {
	joinFixture(t)
	dir := t.TempDir()
	r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"), quorumHeldErr: errors.New("no quorum")}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fakeJoinEffects(r1)); err == nil {
		t.Fatal("the first join must fail after adding its member")
	}
	original, err := os.ReadFile(joinIntentPath)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("x509: certificate signed by unknown authority")
	r2 := &recordedJoin{listMembers: listedMembers(), failVIP: cause}
	r2.listMembers.Members = r2.listMembers.Members[:1] // the cluster no longer lists the recorded member
	_, err = prepareControlPlaneJoin(context.Background(), freshJoinOpts(), dir, fakeJoinEffects(r2))
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "spec.vip.address") {
		t.Fatalf("the stale journal's fresh member-add was not refused by the VIP check: %v", err)
	}
	if r2.vipCalls != 1 || r2.addCalls != 0 || r2.saved != nil {
		t.Errorf("VIP checks=%d member-adds=%d saved=%v, want one check, no member-add and no saved state",
			r2.vipCalls, r2.addCalls, r2.saved)
	}
	retained, err := os.ReadFile(joinIntentPath)
	if err != nil || string(retained) != string(original) {
		t.Fatalf("the refused refresh replaced the recovery journal (%v): got %s, want %s", err, retained, original)
	}
}
