package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ctrliq/stratum/internal/profile/profiletest"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	corev1 "k8s.io/api/core/v1"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// Retained member data is refused before even parsing the server URL. The
// invalid URL keeps a regression from proceeding to real host writes.
func TestJoinRefusesRetainedMemberBeforePreparation(t *testing.T) {
	dir := t.TempDir()
	member := filepath.Join(dir, "member", "wal")
	if err := os.MkdirAll(member, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(member, "retained")
	if err := os.WriteFile(sentinel, []byte("original member data"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := JoinOptions{Server: "https://%", Token: "token", CAHash: "pin", CertKey: "key"}
	// runUp threads a live context through preparation; a Background context
	// keeps this test focused on the refusal.
	_, err := prepareControlPlaneJoin(context.Background(), opts, dir, productionJoinEffects())
	if err == nil {
		t.Fatal("accepted retained member")
	}
	for _, want := range []string{"refusing to overwrite", "trust material", "no --join", "member remove <ID>", "stratum uninstall --yes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "original member data" {
		t.Fatalf("retained data changed: %q %v", data, err)
	}
	// A partial directory must fail at this same early gate, before URL
	// validation and before certificate download can alter recovery material.
	residueDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(residueDir, "leftover"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareControlPlaneJoin(context.Background(), opts, residueDir, productionJoinEffects()); err == nil || !strings.Contains(err.Error(), "residue") {
		t.Fatalf("non-member residue passed early preflight: %v", err)
	}
	// With no member data, preparation gets as far as URL validation.
	_, err = prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), productionJoinEffects())
	if err == nil || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("fresh preflight: %v", err)
	}
}

func TestAddMemberIfEtcdDataDirEmptyRollsBackConcurrentData(t *testing.T) {
	dir := t.TempDir()
	added, removed := false, false
	_, err := addMemberIfEtcdDataDirEmpty(dir, func() (string, error) {
		added = true
		if err := os.Mkdir(filepath.Join(dir, "member"), 0o700); err != nil {
			t.Fatal(err)
		}
		return "cluster", nil
	}, func() error { removed = true; return nil })
	if err == nil || !added || !removed {
		t.Fatalf("concurrent data: add=%v remove=%v err=%v", added, removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "member")); err != nil {
		t.Fatalf("concurrent member data removed: %v", err)
	}
}

// Any entry, including a non-member file, blocks the join before it can change
// shared PKI material. A missing or empty directory is safe to prepare.
func TestRefuseRetainedMember(t *testing.T) {
	check := func(dir string, want bool, what string) {
		t.Helper()
		err := refuseRetainedMember(dir)
		if (err != nil) != want {
			t.Errorf("%s: refusal = %v, want %v", what, err, want)
		}
	}
	d := t.TempDir()
	check(filepath.Join(d, "absent"), false, "a missing dir")
	check(d, false, "an empty dir")
	file := filepath.Join(t.TempDir(), "etcd")
	if err := os.MkdirAll(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, "member"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check(file, true, "a file named member")
	if err := os.WriteFile(filepath.Join(d, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check(d, true, "non-member residue")
	if err := os.Remove(filepath.Join(d, "leftover")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, "member", "wal"), 0o700); err != nil {
		t.Fatal(err)
	}
	check(d, true, "a dir with member/")
}

// A data dir that cannot be inspected is not an empty one.
func TestRefuseRetainedMemberFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through directory permissions")
	}
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
	if err := refuseRetainedMember(d); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("an unreadable data dir: err = %v, want a refusal", err)
	}
}

// A member arriving after the early preflight must survive the late empty-dir
// replacement. Non-member residue is also retained for explicit inspection.
func TestPrepareEmptyEtcdDataDirPreservesConcurrentEntries(t *testing.T) {
	for _, name := range []string{"member", "leftover"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "etcd")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := refuseRetainedMember(dir); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(dir, name)
			if err := os.Mkdir(entry, 0700); err != nil {
				t.Fatal(err)
			}
			if err := prepareEmptyEtcdDataDir(dir); err == nil {
				t.Fatal("replaced nonempty directory")
			}
			if _, err := os.Stat(entry); err != nil {
				t.Fatalf("concurrent entry lost: %v", err)
			}
		})
	}
	for _, exists := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "etcd")
		if exists {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := prepareEmptyEtcdDataDir(dir); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("prepared dir: %v %v", entries, err)
		}
	}
}

// keyBundle is a downloaded bundle that carried the cis profile's at-rest key,
// or none, from an uploader whose profile it does not say.
func keyBundle(carried bool) bundleProfile {
	if carried {
		return bundleProfile{Secrets: []string{encryptionConfigEntry}}
	}
	return bundleProfile{}
}

// cisFor is the profile a join with the CIS flag runs.
func cisFor(cis bool) profile.Profile {
	if cis {
		return profiletest.Profile()
	}
	return profile.Profile{}
}

func TestRequireSharedSecrets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cis        bool
		carriedKey bool
		refuse     bool
	}{
		{"no cis, no key", false, false, false},
		{"no cis, but the bundle carried a key", false, true, true},
		{"cis with the key the bundle carried", true, true, false},
		{"cis and the bundle carried no key", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireSharedSecrets(cisFor(tc.cis), keyBundle(tc.carriedKey))
			if (err != nil) != tc.refuse {
				t.Fatalf("err = %v, want refusal: %v", err, tc.refuse)
			}
			if !tc.refuse {
				return
			}
			wants := []string{"carry no at-rest encryption key", "Nothing has been added to etcd",
				"join it without --profile cis or --cis (and without spec.profile: cis or spec.cis: true)", "CIS has to be turned on there first",
				profile.ProDocsURL, "stratum token create --control-plane"}
			if !tc.cis {
				wants = []string{"carry an at-rest encryption key", "Re-run this join with --profile cis",
					"Nothing has been added to etcd"}
			}
			for _, want := range wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal is missing %q: %v", want, err)
				}
			}
		})
	}
}

// A bundle names its uploader's profile, and a join of any other is refused
// whatever secrets it carries: a profile without shared secrets is told from
// none only by its name.
func TestRequireSharedSecretsComparesTheUploadersProfile(t *testing.T) {
	plain := profile.Profile{Name: "plain"}
	for _, tc := range []struct {
		name   string
		prof   profile.Profile
		b      bundleProfile
		refuse []string
	}{
		{"the same profile", plain, bundleProfile{Profile: "plain", ProfileKnown: true}, nil},
		{"none on both sides", profile.Profile{}, bundleProfile{ProfileKnown: true}, nil},
		{"cis with its key", cisFor(true), bundleProfile{Secrets: []string{encryptionConfigEntry}, Profile: "cis", ProfileKnown: true}, nil},
		{"a profile the cluster does not run", plain, bundleProfile{ProfileKnown: true},
			[]string{"runs no node profile", "would run the plain profile", "Re-run this join with no --profile or --cis (and no spec.profile or spec.cis: true)"}},
		{"no profile on a cluster that runs one", profile.Profile{}, bundleProfile{Profile: "plain", ProfileKnown: true},
			[]string{"runs the plain profile", "would run no node profile", "Re-run this join with --profile plain"}},
		{"cis with its key on a cluster that says another", cisFor(true), bundleProfile{Secrets: []string{encryptionConfigEntry}, Profile: "plain", ProfileKnown: true},
			[]string{"runs the plain profile", "would run the cis profile"}},
		{"an older uploader that does not say", plain, bundleProfile{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireSharedSecrets(tc.prof, tc.b)
			if (err != nil) != (tc.refuse != nil) {
				t.Fatalf("err = %v, want refusal %v", err, tc.refuse)
			}
			for _, want := range tc.refuse {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal is missing %q: %v", want, err)
				}
			}
		})
	}
}

// A journal an earlier binary wrote says only whether the bundle carried the
// cis profile's key; read back, that is the key's entry. A journal this binary
// writes says which secrets, and still sets the earlier field for that binary.
func TestJoinRecoveryCarriedReadsAnEarlierJournal(t *testing.T) {
	var r joinRecovery
	if err := json.Unmarshal([]byte(`{"carriedKey":true}`), &r); err != nil {
		t.Fatal(err)
	}
	if got := r.carried(); !slices.Equal(got, []string{encryptionConfigEntry}) {
		t.Errorf("an earlier journal with the key reads as %v", got)
	}
	if got := (joinRecovery{}).carried(); len(got) != 0 {
		t.Errorf("an earlier journal without the key reads as %v", got)
	}
	b, err := json.Marshal(joinRecovery{CarriedKey: true, CarriedSecrets: []string{encryptionConfigEntry}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"carriedKey":true`) || !strings.Contains(string(b), `"carriedSecrets":["`+encryptionConfigEntry+`"]`) {
		t.Errorf("journal = %s, want both fields", b)
	}
}

// A profile other than cis gets the same comparison of the secrets it
// declares with the ones the bundle carried, by name.
func TestRequireSharedSecretsForAnotherProfile(t *testing.T) {
	mint := func() ([]byte, error) { return []byte("s"), nil }
	two := profile.Profile{Name: "two", SharedSecrets: []profile.Secret{{Name: "b.yaml", Mint: mint}, {Name: "a.yaml", Mint: mint}}}
	if err := requireSharedSecrets(two, bundleProfile{Secrets: []string{"a.yaml", "b.yaml"}}); err != nil {
		t.Fatalf("both secrets carried: %v", err)
	}
	err := requireSharedSecrets(two, bundleProfile{Secrets: []string{"a.yaml"}})
	if err == nil || !strings.Contains(err.Error(), "profile two") || !strings.Contains(err.Error(), "carry a.yaml of the shared secrets the profile declares (a.yaml, b.yaml)") {
		t.Fatalf("one secret missing = %v", err)
	}
	err = requireSharedSecrets(profile.Profile{}, bundleProfile{Secrets: []string{"a.yaml", "b.yaml"}})
	if err == nil || !strings.Contains(err.Error(), "carry the shared secrets a.yaml, b.yaml") || !strings.Contains(err.Error(), "--profile") {
		t.Fatalf("no profile, secrets carried = %v", err)
	}
}

// TestApplyBundleReportsWhatTheBundleCarried: a key left on
// disk by an earlier failed join survives a keyless bundle, because a bundle
// does not remove the optional entries it leaves out. Asking the disk afterwards
// would call that join keyed; the answer has to come from the bundle.
func TestApplyBundleReportsWhatTheBundleCarried(t *testing.T) {
	keyless := uploadSet(t)
	delete(keyless, encryptionConfigEntry)

	t.Run("a keyless bundle over a stale key on disk", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, encryptionConfigEntry), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		carried, err := applyBundle(dir, keyless)
		if err != nil {
			t.Fatal(err)
		}
		if carried {
			t.Fatal("reported the stale on-disk key as carried by a bundle that had none")
		}
		// applyBundle is also CA rotation's path (DownloadCertsAdmin), on a
		// joined control plane whose key this is: it must never remove it.
		if b, err := os.ReadFile(filepath.Join(dir, encryptionConfigEntry)); err != nil || string(b) != "stale" {
			t.Fatalf("applyBundle touched the key on disk: %q, %v", b, err)
		}
	})
	t.Run("a bundle with the key", func(t *testing.T) {
		carried, err := applyBundle(t.TempDir(), uploadSet(t))
		if err != nil || !carried {
			t.Fatalf("got %v, %v; want true, nil", carried, err)
		}
	})
}

// --- the join's effects, recorded ---
//
// The join's safety order — every step that can refuse returns before the etcd
// member-add — is a property of prepareControlPlaneJoin's behaviour, not of its
// source text. These helpers run the join with an effect set that records each
// call instead of touching the host or the cluster, so the guards below assert
// on what ran, in what order, with which arguments.

// recordedJoin is what a recording effect set captured: the order the effects
// ran in, and the arguments of the ones the guards pin. failCIS and failProbe
// inject the refusals those gates produce in production; carriedKey is what the
// download reports the bundle carried, which the key gate decides on. The
// recovery guards inject the post-add failures (failSaveState,
// failRuntimeConfigs) and answer for the cluster (listMembers, quorumHeldErr,
// failRemoveByID) the compensation asks; the intent snapshots record whether
// the journal file existed, with its bytes and permissions, when the member-add
// and the state save ran. The resume guards inject the fresh add's lost reply
// (failAdd) and answer the member-list reads dynamically (clusterMembersFn);
// addCalls and memberListReads count what the reconciliation decided on. The
// assembly guards record how often the join ran the shared node assembly
// (assembleCalls) and the request it handed it (assembleReq).
type recordedJoin struct {
	order           []string
	carriedKey      bool
	failCiliumClear error
	failCIS         error
	profileConfigs  string
	failProbe       error
	clearCiliumPath string

	assembleCalls int
	assembleReq   *AssembleRequest

	addCalled, removeCalled bool
	addIPs                  []string
	addName, addPeer        string
	removePeer              string

	certCalled           bool
	certHost, certNodeIP string
	certSANs             *pki.APISANs
	kcCalled             bool
	kcHost, kcServer     string
	kcVIP                bool
	saved                *nodestate.NodeState
	// rtState is the NodeState the runtime-config write received. The join
	// builds the node state before membership, so the runtime configs run
	// before the member-add on the state whose fields they render from.
	rtState *nodestate.NodeState

	failSaveState      error
	failRuntimeConfigs error
	listMembers        etcdMembership
	quorumHeldErr      error
	failRemoveByID     error
	// detectIP is the address detection answers with; empty is the fixture's
	// 192.0.2.9. A re-run with a different address drives the intent
	// reconciliation against a journal recorded for another one.
	detectIP string
	// failAdd is the member-add's answer on its first call: the lost-reply
	// case the reconciliation decides on. A call after the first succeeds,
	// unless failRetry is set: then it is the second call's answer.
	failAdd   error
	failRetry error
	// clusterMembersFn, when set, answers every member-list read in place of
	// the static listMembers, so a guard can make the cluster's answer change
	// between reads.
	clusterMembersFn func() (etcdMembership, error)

	quorumSeen         bool
	quorumHeldCalls    int
	removeByIDCalls    int
	removeByIDID       uint64
	quorumBeforeRemove bool
	addCalls           int
	memberListReads    int
	intentAtAdd        bool
	intentAtAddRaw     []byte
	intentAtAddPerm    os.FileMode
	intentAtSave       bool
	intentAtSaveRaw    []byte

	// failVIP is the VIP check's answer: nil for a VIP that serves this
	// cluster's apiserver. vipCalls and vipAddr record how often the check ran
	// and the address it was asked about; vipAfterKubeconfigs and intentAtVIP
	// record, at the moment it ran, whether the kubeconfigs it reads the
	// cluster CA from were written and whether the intent file existed.
	failVIP             error
	vipCalls            int
	vipAddr             string
	vipAfterKubeconfigs bool
	intentAtVIP         bool
}

// fakeJoinEffects is a joinEffects set that records each call, in order, and
// succeeds. resolveNodeIP calls the detection it is handed — as the production
// one does — so the wiring is exercised, and returns the detected address.
func fakeJoinEffects(r *recordedJoin) joinEffects {
	return joinEffects{
		verifyResumeTrust:  func(string) error { return nil },
		persistPreparation: func() error { return nil },
		stage:              func(string) ([]string, error) { r.order = append(r.order, "stage"); return nil, nil },
		hostDirs:           func() error { r.order = append(r.order, "host directories"); return nil },
		prereqs:            func(host.PrereqOptions) error { r.order = append(r.order, "prerequisites"); return nil },
		discover: func(server, token, caHash string) ([]byte, error) {
			r.order = append(r.order, "discover")
			return []byte("ca"), nil
		},
		writeBootstrapKubeconfig: func(server, token string, ca []byte) error {
			r.order = append(r.order, "bootstrap kubeconfig")
			return nil
		},
		clearCiliumSettings: func(path string) error {
			r.order = append(r.order, "clear cilium settings")
			r.clearCiliumPath = path
			return r.failCiliumClear
		},
		downloadGrant: func(bootstrapKubeconfig, certKey string, check func(bundleProfile) error) (bundleGrant, error) {
			r.order = append(r.order, "download")
			g := bundleGrant{Published: []string{"10.0.0.11"}, CarriedKey: r.carriedKey}
			if r.carriedKey {
				g.Secrets = []string{encryptionConfigEntry}
			}
			return g, nil
		},
		trustPodCIDRs: func(string) { r.order = append(r.order, "trust pod cidrs") },
		resolveNodeIP: func(flag, config, state string, detect func() (string, error)) (host.NodeIPChoice, error) {
			r.order = append(r.order, "resolve node ip")
			ip, err := detect()
			if err != nil {
				return host.NodeIPChoice{}, err
			}
			return host.NodeIPChoice{IP: ip, Source: "detect"}, nil
		},
		detectNodeIPs: func(bool) (string, error) {
			if r.detectIP == "" {
				return "192.0.2.9", nil
			}
			return r.detectIP, nil
		},
		issueCerts: func(hostname, nodeIP string, sans *pki.APISANs) error {
			r.order = append(r.order, "issue certificates")
			r.certCalled, r.certHost, r.certNodeIP, r.certSANs = true, hostname, nodeIP, sans
			return nil
		},
		writeKubeconfigs: func(hostname, server string, vip bool) error {
			r.order = append(r.order, "write kubeconfigs")
			r.kcCalled, r.kcHost, r.kcServer, r.kcVIP = true, hostname, server, vip
			return nil
		},
		writeProfileConfigs: func(p profile.Profile) error {
			r.order = append(r.order, "profile configs")
			r.profileConfigs = p.Name
			return r.failCIS
		},
		probeEtcd: func([]string) ([]string, error) {
			r.order = append(r.order, "probe etcd")
			return []string{"10.0.0.11"}, r.failProbe
		},
		controllerNodeIPs: func(context.Context, string, []string) ([]string, error) {
			r.order = append(r.order, "controller addresses")
			return []string{"10.0.0.11"}, nil
		},
		vipServes: func(vip string) error {
			r.order = append(r.order, "vip check")
			r.vipCalls++
			r.vipAddr = vip
			r.vipAfterKubeconfigs = r.kcCalled
			r.intentAtVIP, _, _ = snapshotIntent()
			return r.failVIP
		},
		memberAdd: func(cpIPs []string, name, peerURL string) (string, error) {
			r.order = append(r.order, "member-add")
			r.addCalled, r.addIPs, r.addName, r.addPeer = true, cpIPs, name, peerURL
			r.addCalls++
			r.intentAtAdd, r.intentAtAddRaw, r.intentAtAddPerm = snapshotIntent()
			if r.failAdd != nil && r.addCalls == 1 {
				return "", r.failAdd
			}
			if r.failRetry != nil && r.addCalls == 2 {
				return "", r.failRetry
			}
			return "cp1=https://10.0.0.11:2380,cp9=https://192.0.2.9:2380", nil
		},
		memberRemove: func(cpIPs []string, peerURL string) error {
			r.order = append(r.order, "member-remove")
			r.removeCalled, r.removePeer = true, peerURL
			return nil
		},
		clusterMembers: func([]string) (etcdMembership, error) {
			r.memberListReads++
			if r.clusterMembersFn != nil {
				return r.clusterMembersFn()
			}
			return r.listMembers, nil
		},
		memberRemoveByID: func(_ []string, id uint64) error {
			r.removeByIDCalls++
			r.removeByIDID = id
			r.quorumBeforeRemove = r.quorumSeen
			return r.failRemoveByID
		},
		confirmQuorumHeld: func() error {
			r.quorumHeldCalls++
			r.quorumSeen = true
			return r.quorumHeldErr
		},
		saveState: func(st *nodestate.NodeState) error {
			r.order = append(r.order, "save state")
			r.saved = st
			r.intentAtSave, r.intentAtSaveRaw, _ = snapshotIntent()
			return r.failSaveState
		},
		writeRuntimeConfigs: func(st *nodestate.NodeState) error {
			r.order = append(r.order, "runtime configs")
			r.rtState = st
			return r.failRuntimeConfigs
		},
		// The real shared node assembly, behind a recorder: the guards
		// below drive the join's common steps THROUGH the assembly, so a
		// fold that stops routing them there leaves the effects unrun and
		// fails the order pins.
		assemble: func(ctx context.Context, a NodeAssembly, req AssembleRequest) (host.NodeIPChoice, error) {
			r.assembleCalls++
			r.assembleReq = &req
			return AssembleNodeContext(ctx, a, req)
		},
	}
}

// joinFixture points the paths a fresh join's gates read at temporary ones and
// saves the netconfig the (recorded) download is taken to have delivered, as
// TestCheckJoinDomain does: the domain gate, the key gate's stat and the
// netconfig resolve all stay inside the test's temp tree.
func joinFixture(t *testing.T) {
	t.Helper()
	origNet := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = origNet })
	if err := netcfg.Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
		t.Fatalf("netcfg.Save: %v", err)
	}
	origEnc := version.EncryptionConfigPath
	version.EncryptionConfigPath = filepath.Join(t.TempDir(), "encryption-config.yaml")
	t.Cleanup(func() { version.EncryptionConfigPath = origEnc })
	origConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = origConf })
	origCilium := version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.CiliumSettingsPath = origCilium })
	origIntent := joinIntentPath
	joinIntentPath = filepath.Join(t.TempDir(), "join-intent.json")
	t.Cleanup(func() { joinIntentPath = origIntent })
}

// freshJoinOpts is a complete JoinOptions for the documented join shape: an
// IPv4 node joining through the VIP 10.0.0.10, whose published etcd endpoints
// (the recorded download reports 10.0.0.11) win over the join host.
func freshJoinOpts() JoinOptions {
	return JoinOptions{
		Server:  "https://10.0.0.10:6443",
		Token:   "bootstrap-token",
		CAHash:  "pin",
		CertKey: "certificate-key",
		VIP:     nodestate.VIP{Address: "10.0.0.10"},
	}
}

// TestControlPlaneJoinRunsEffectsInSafetyOrder pins the fresh join's whole step
// order by behaviour: stage, host directories, prerequisites, discover,
// bootstrap kubeconfig, clear stale Cilium settings, download, trust pod CIDRs, resolve node IP, issue
// certificates, write kubeconfigs, CIS configs (a --cis join only), probe etcd,
// VIP check (the join has a VIP), runtime configs, member-add, save state.
// Every fallible effect that needs no
// member-add result runs before the add — runtime configs included, after the
// etcd probe, so the node state it renders from can carry an EtcdInitialCluster
// derived from the cluster's member list rather than the add's return. The
// member-add is the first membership effect; save state follows it, because the
// persisted state asserts the membership the add established and carries the
// initial-cluster string the add returned (or the adopted membership gave).
// Any refactor that reorders these effects, drops one, or runs one twice fails
// here — including one that moves a refusing step below the member-add.
func TestControlPlaneJoinRunsEffectsInSafetyOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		cis  bool
	}{
		{"without cis", false},
		{"with cis", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{carriedKey: tc.cis}
			opts := freshJoinOpts()
			opts.CIS = tc.cis
			if tc.cis {
				opts.Profile = nodestate.CISProfile
			}
			st, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r))
			if err != nil {
				t.Fatalf("fresh join: %v", err)
			}
			want := []string{
				"stage", "host directories", "prerequisites", "discover", "bootstrap kubeconfig",
				"clear cilium settings", "download", "trust pod cidrs", "resolve node ip", "issue certificates", "write kubeconfigs",
			}
			if tc.cis {
				want = append(want, "profile configs")
			}
			want = append(want, "probe etcd", "controller addresses", "vip check", "runtime configs", "member-add", "save state")
			if !slices.Equal(r.order, want) {
				t.Fatalf("effects ran in another order:\n got %v\nwant %v", r.order, want)
			}
			if tc.cis && r.profileConfigs != nodestate.CISProfile {
				t.Errorf("the join wrote the config files of profile %q, want cis", r.profileConfigs)
			}
			if r.clearCiliumPath != version.CiliumSettingsPath {
				t.Errorf("cleared Cilium path %q, want %q", r.clearCiliumPath, version.CiliumSettingsPath)
			}
			// The member-add hands etcd the peer URL this node's member will be
			// removed under again: the primary IPv4, not the VIP it joined via.
			if !r.addCalled || r.addPeer != "https://192.0.2.9:2380" {
				t.Errorf("member-add peer URL = %q (called %v), want this node's https://192.0.2.9:2380", r.addPeer, r.addCalled)
			}
			if r.saved == nil {
				t.Fatal("no node state was saved")
			}
			if r.saved.EtcdState != "existing" || st.EtcdState != "existing" {
				t.Errorf("saved EtcdState = %q, returned %q, want existing", r.saved.EtcdState, st.EtcdState)
			}
			// The runtime configs run before the add on the node state the join
			// built before membership, not on a post-add patch.
			if r.rtState == nil || r.rtState.Role != "controller" || r.rtState.EtcdName != r.saved.EtcdName {
				t.Errorf("the runtime-config write got %+v, want the node state built before membership", r.rtState)
			}
		})
	}
}

// TestControlPlaneJoinAddressInputs pins the addresses a fresh IPv4 join with
// VIP 10.0.0.10 hands downstream: the member-add receives this node's peer URL
// under its own IP, and the certificate effect receives the resolved node IP
// with the netconfig's VIP SANs and the cluster domain. The kubeconfig endpoint
// the join writes, the server every controller start uses, is pinned by
// TestControlPlaneJoinTargetsTheControllerStartsServer.
func TestControlPlaneJoinAddressInputs(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r)); err != nil {
		t.Fatalf("fresh join: %v", err)
	}
	if !r.addCalled || r.addPeer != "https://192.0.2.9:2380" {
		t.Errorf("member-add peer URL = %q (called %v), want https://192.0.2.9:2380", r.addPeer, r.addCalled)
	}
	if !r.certCalled || r.certNodeIP != "192.0.2.9" {
		t.Errorf("certificate effect got node IP %q (called %v), want the resolved node IP 192.0.2.9", r.certNodeIP, r.certCalled)
	}
	if r.certSANs == nil {
		t.Fatal("certificate effect got no SANs")
	}
	netc, err := netcfg.Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := netc.APIServerSANs("10.0.0.10"); !slices.Equal(r.certSANs.IPs, want) {
		t.Errorf("certificate SANs = %q, want the netconfig's %q with the VIP", r.certSANs.IPs, want)
	}
	if r.certSANs.Domain != netc.ClusterDomain {
		t.Errorf("certificate domain = %q, want the netconfig's %q", r.certSANs.Domain, netc.ClusterDomain)
	}
}

// TestControlPlaneJoinRecordsTheBringUpEndpoint pins that a joined control
// plane's kubeconfigs keep their endpoint across its first restart, because the
// join records and writes the endpoint by the same rule bring-up uses
// (serverURL, pinned by TestBringUpAddressesFromNodeState in
// internal/cluster/addresses_test.go: the VIP when one is configured, the local
// apiserver otherwise) — not loopback kubeconfigs plus the join URL recorded as
// the node's Server, which today move the node's clients to the VIP only when
// bring-up rewrites them at the first restart. The join here runs through a
// control plane's node address (joinEtcdEndpoints documents it as a supported
// shape), so neither the join URL nor loopback can pass for the VIP cluster:
// the kubeconfig write and the saved node state must both carry the endpoint
// bring-up's rule returns for the same state, and agree with each other.
func TestControlPlaneJoinRecordsTheBringUpEndpoint(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{}
	opts := freshJoinOpts()
	opts.Server = "https://10.0.0.11:6443" // a control plane's address, not the VIP
	if _, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r)); err != nil {
		t.Fatalf("fresh join: %v", err)
	}
	// Bring-up's endpoint rule for this state: the configured VIP.
	want := fmt.Sprintf("https://%s:%d", opts.VIP.Address, version.APIServerPort)
	if !r.kcCalled || r.kcServer != want {
		t.Errorf("kubeconfig effect got server %q (called %v), want bring-up's endpoint %q, not loopback",
			r.kcServer, r.kcCalled, want)
	}
	if !r.kcVIP {
		t.Error("kubeconfig effect got vip false, want true: kube-vip's kubeconfig must still be written")
	}
	if r.saved == nil {
		t.Fatal("no node state was saved")
	}
	if r.saved.Server != want {
		t.Errorf("saved node state Server = %q, want bring-up's endpoint %q — the join URL %q is not the rule",
			r.saved.Server, want, opts.Server)
	}
	if r.kcCalled && r.saved.Server != r.kcServer {
		t.Errorf("the kubeconfigs target %q but the node state records %q: two endpoint rules, not one",
			r.kcServer, r.saved.Server)
	}
}

// TestControlPlaneJoinRefusesBeforeTheMemberAdd drives the join into each
// refusal and requires it to return before the etcd member-add: nothing after
// the add removes the member again, so a gate that moves below it leaves a
// voting member behind for a node that never starts. The moved steps are
// covered too: a runtime-config write that fails refuses the join before the
// add, because the runtime configs need no member-add result. The domain row
// also requires the refusal before any certificate is issued, and no row may
// save node state or run the post-add compensation on its way out.
func TestControlPlaneJoinRefusesBeforeTheMemberAdd(t *testing.T) {
	retained := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "etcd")
		if err := os.MkdirAll(filepath.Join(dir, "member", "wal"), 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, tc := range []struct {
		name       string
		etcdDir    func(t *testing.T) string
		setup      func(o *JoinOptions, r *recordedJoin)
		wantErr    string
		noCertsYet bool
	}{
		{"retained member data", retained, func(*JoinOptions, *recordedJoin) {}, "refusing to overwrite", false},
		{"a Cilium-settings cleanup that fails", nil, func(o *JoinOptions, r *recordedJoin) {
			r.failCiliumClear = errors.New("permission denied")
		}, "clear a previous join's Cilium settings", true},
		{"a configured domain that differs from the cluster's", nil, func(o *JoinOptions, r *recordedJoin) {
			o.ClusterDomain = "other.internal"
		}, "other.internal", true},
		{"a cis join whose bundle carried no key", nil, func(o *JoinOptions, r *recordedJoin) {
			o.CIS, o.Profile = true, nodestate.CISProfile
		}, "carry no at-rest encryption key", false},
		{"a non-cis join whose bundle carried the key", nil, func(o *JoinOptions, r *recordedJoin) {
			r.carriedKey = true
		}, "Re-run this join with --profile cis", false},
		{"a cis-config effect that fails", nil, func(o *JoinOptions, r *recordedJoin) {
			o.CIS, o.Profile = true, nodestate.CISProfile
			r.carriedKey = true
			r.failCIS = errors.New("an unstamped binary cannot pin Pod Security")
		}, "profile configs", false},
		{"an etcd probe that fails", nil, func(o *JoinOptions, r *recordedJoin) {
			r.failProbe = errors.New("connection refused")
		}, "cannot reach etcd", false},
		{"a VIP that does not answer as the cluster's apiserver", nil, func(o *JoinOptions, r *recordedJoin) {
			r.failVIP = errors.New("connection refused")
		}, "10.0.0.10", false},
		{"a runtime-config write that fails", nil, func(o *JoinOptions, r *recordedJoin) {
			r.failRuntimeConfigs = errors.New("permission denied")
		}, "permission denied", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{}
			opts := freshJoinOpts()
			tc.setup(&opts, r)
			etcdDir := t.TempDir()
			if tc.etcdDir != nil {
				etcdDir = tc.etcdDir(t)
			}
			_, err := prepareControlPlaneJoin(context.Background(), opts, etcdDir, fakeJoinEffects(r))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.wantErr)
			}
			if r.addCalled || r.removeCalled {
				t.Errorf("the member-add/remove ran although the join refused: add=%v remove=%v", r.addCalled, r.removeCalled)
			}
			if r.saved != nil {
				t.Errorf("node state was saved although the join refused: %+v", r.saved)
			}
			if r.removeByIDCalls != 0 || r.quorumHeldCalls != 0 {
				t.Errorf("the post-add compensation ran although the join refused before the member-add: remove-by-ID=%d quorum=%d",
					r.removeByIDCalls, r.quorumHeldCalls)
			}
			if tc.noCertsYet && (r.certCalled || r.kcCalled) {
				t.Errorf("certificates (%v) or kubeconfigs (%v) were issued before the domain refusal", r.certCalled, r.kcCalled)
			}
		})
	}
}

// TestEtcdProbeRefusalSaysToJoinOneAtATime: a second controller joined while
// another's member-add takes effect times out in the etcd probe, and re-running
// it alone succeeds. The refusal says so, on top of the probe's own error and
// the note that nothing was added.
func TestEtcdProbeRefusalSaysToJoinOneAtATime(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{failProbe: errors.New("context deadline exceeded")}
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
	if err == nil {
		t.Fatal("the join did not refuse a failed etcd probe")
	}
	for _, want := range []string{"cannot reach etcd", "context deadline exceeded", "nothing was added", concurrentJoinHint} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if r.addCalled {
		t.Error("the member-add ran although the probe failed")
	}
}

// customBundleNetwork overwrites the netconfig joinFixture saved with one for a
// cluster initialised on custom ranges, so a join that falls back to the
// built-in defaults anywhere cannot pass for one that took the bundle's.
func customBundleNetwork(t *testing.T) {
	t.Helper()
	if err := netcfg.Save("10.42.0.0/16", "10.43.0.0/16", "fuzz.internal"); err != nil {
		t.Fatalf("netcfg.Save: %v", err)
	}
}

// TestControlPlaneJoinRefusesADifferingNetwork: a joining control plane whose
// spec.network.podCIDR or serviceCIDR differs from the range the cluster
// recorded in the netconfig the shared certs delivered is refused, each field
// on its own, before it trusts the pod range, issues a certificate or touches
// etcd. Without the refusal a lone range is dropped for the bundle's and a pair
// silently replaces the cluster's, and the node's first restart is the first
// notice either way.
func TestControlPlaneJoinRefusesADifferingNetwork(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		podCIDR, serviceCIDR string
		field, given, bundle string
	}{
		{"a lone differing service CIDR", "", "10.98.0.0/16", "serviceCIDR", "10.98.0.0/16", "10.43.0.0/16"},
		{"a lone differing pod CIDR", "10.99.0.0/16", "", "podCIDR", "10.99.0.0/16", "10.42.0.0/16"},
		{"a differing service CIDR beside the cluster's pod CIDR", "10.42.0.0/16", "10.98.0.0/16", "serviceCIDR", "10.98.0.0/16", "10.43.0.0/16"},
		{"a differing pod CIDR beside the cluster's service CIDR", "10.99.0.0/16", "10.43.0.0/16", "podCIDR", "10.99.0.0/16", "10.42.0.0/16"},
		{"the built-in service CIDR on a cluster that runs another", "", "10.96.0.0/12", "serviceCIDR", "10.96.0.0/12", "10.43.0.0/16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			customBundleNetwork(t)
			r := &recordedJoin{}
			opts := freshJoinOpts()
			opts.PodCIDR, opts.ServiceCIDR = tc.podCIDR, tc.serviceCIDR
			_, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatalf("the join succeeded with %s %s on a cluster that records %s; want a refusal",
					tc.field, tc.given, tc.bundle)
			}
			for _, want := range []string{tc.field, tc.given, tc.bundle, "no certificates were issued and etcd is untouched"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal missing %q: %v", want, err)
				}
			}
			// Each value in its role: the field carries the value the node was
			// given, and the remedy names the value the cluster recorded. A
			// refusal that swaps them tells the user to set the value that
			// was just refused.
			for _, want := range []string{
				fmt.Sprintf("%s %q", tc.field, tc.given),
				fmt.Sprintf("set it to %q", tc.bundle),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not put each value in its role, missing %q: %v", want, err)
				}
			}
			// The refusal stands where the domain's does: after the download
			// delivered the cluster's netconfig, before every later effect.
			want := []string{
				"stage", "host directories", "prerequisites", "discover", "bootstrap kubeconfig",
				"clear cilium settings", "download",
			}
			if !slices.Equal(r.order, want) {
				t.Errorf("effects ran past the refusal:\n got %v\nwant %v", r.order, want)
			}
			if r.certCalled || r.kcCalled || r.addCalled || r.saved != nil {
				t.Errorf("the refused join issued certificates (%v), wrote kubeconfigs (%v), added a member (%v) or saved node state (%v)",
					r.certCalled, r.kcCalled, r.addCalled, r.saved != nil)
			}
			if exists, _, _ := snapshotIntent(); exists {
				t.Errorf("the refused join wrote the intent journal %s", joinIntentPath)
			}
		})
	}
}

// TestControlPlaneJoinInheritsTheBundlesNetwork: a joining control plane whose
// spec.network ranges are unset or match the cluster's, together or one alone,
// joins on the ranges and domain the shared certs delivered. Its apiserver
// certificate carries the cluster's kubernetes Service IP, its node state
// records the cluster's network, and its firewall trusts the cluster's pod
// range, none of them a built-in default.
func TestControlPlaneJoinInheritsTheBundlesNetwork(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		podCIDR, serviceCIDR string
	}{
		{"both unset", "", ""},
		{"both matching", "10.42.0.0/16", "10.43.0.0/16"},
		{"a lone matching pod CIDR", "10.42.0.0/16", ""},
		{"a lone matching service CIDR", "", "10.43.0.0/16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			customBundleNetwork(t)
			r := &recordedJoin{}
			fx := fakeJoinEffects(r)
			trusted := ""
			fx.trustPodCIDRs = func(podCIDR string) { r.order = append(r.order, "trust pod cidrs"); trusted = podCIDR }
			opts := freshJoinOpts()
			opts.PodCIDR, opts.ServiceCIDR = tc.podCIDR, tc.serviceCIDR
			if _, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx); err != nil {
				t.Fatalf("join with pod CIDR %q and service CIDR %q: %v", tc.podCIDR, tc.serviceCIDR, err)
			}
			if r.certSANs == nil || len(r.certSANs.IPs) == 0 {
				t.Fatalf("certificate effect got no IP SANs: %+v", r.certSANs)
			}
			if r.certSANs.IPs[0] != "10.43.0.1" {
				t.Errorf("first apiserver IP SAN = %q (all %q), want the cluster's kubernetes Service IP 10.43.0.1",
					r.certSANs.IPs[0], r.certSANs.IPs)
			}
			if slices.Contains(r.certSANs.IPs, "10.96.0.1") {
				t.Errorf("apiserver IP SANs %q carry the default Service IP 10.96.0.1", r.certSANs.IPs)
			}
			if r.saved == nil {
				t.Fatal("no node state was saved")
			}
			if r.saved.PodCIDR != "10.42.0.0/16" || r.saved.ServiceCIDR != "10.43.0.0/16" || r.saved.ClusterDomain != "fuzz.internal" {
				t.Errorf("saved node state network = %q, %q, %q; want the bundle's 10.42.0.0/16, 10.43.0.0/16, fuzz.internal",
					r.saved.PodCIDR, r.saved.ServiceCIDR, r.saved.ClusterDomain)
			}
			if trusted != "10.42.0.0/16" {
				t.Errorf("trusted pod CIDR %q, want the bundle's 10.42.0.0/16", trusted)
			}
		})
	}
}

// A join compares the networks a config names, while retaining the spelling
// delivered by the shared certs or the recovery journal. Alternate CIDR
// spellings must work on both paths; different or invalid ranges still refuse
// before issuing certificates or changing membership.
func TestControlPlaneJoinComparesNetworkRanges(t *testing.T) {
	const pods = "10.42.0.0/16,fd00:42::/56"
	const services = "10.43.0.0/16,fd00:43::/108"
	for _, resumed := range []bool{false, true} {
		for _, tc := range []struct {
			name                 string
			podCIDR, serviceCIDR string
			wantRefusal          bool
		}{
			{"expanded pod IPv6", "10.42.0.0/16,fd00:0042:0:0::/56", "", false},
			{"expanded service IPv6", "", "10.43.0.0/16,fd00:0043:0:0::/108", false},
			{"whitespace", " 10.42.0.0/16, fd00:42::/56 ", "10.43.0.0/16, fd00:43::/108", false},
			{"host bits", "10.42.5.7/16,fd00:42::a/56", "10.43.5.7/16,fd00:43::a/108", false},
			{"different pod range", "10.44.0.0/16,fd00:42::/56", "", true},
			{"different service prefix", "", "10.43.0.0/16,fd00:43::/112", true},
			{"missing family", "10.42.0.0/16", "", true},
			{"reversed families", "fd00:42::/56,10.42.0.0/16", "", true},
			{"invalid CIDR", "", "not-a-cidr", true},
		} {
			t.Run(fmt.Sprintf("resume=%t/%s", resumed, tc.name), func(t *testing.T) {
				joinFixture(t)
				if err := netcfg.Save(pods, services, "fuzz.internal"); err != nil {
					t.Fatal(err)
				}
				etcdDir := t.TempDir()
				var journal []byte
				if resumed {
					first := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"),
						quorumHeldErr: errors.New("no quorum")}
					if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(first)); err == nil {
						t.Fatal("the first join must fail after adding its member")
					}
					var err error
					if journal, err = os.ReadFile(joinIntentPath); err != nil {
						t.Fatal(err)
					}
				}
				r := &recordedJoin{listMembers: listedMembers()}
				opts := freshJoinOpts()
				opts.PodCIDR, opts.ServiceCIDR = tc.podCIDR, tc.serviceCIDR
				_, err := prepareControlPlaneJoin(context.Background(), opts, etcdDir, fakeJoinEffects(r))
				if resumed && (slices.Contains(r.order, "discover") || slices.Contains(r.order, "download")) {
					t.Fatalf("the re-run did not resume from the journal: %v", r.order)
				}
				if tc.wantRefusal {
					if err == nil {
						t.Fatal("join accepted a different or invalid network")
					}
					if r.certCalled || r.kcCalled || r.addCalls != 0 || r.removeByIDCalls != 0 || r.saved != nil {
						t.Fatalf("refused join changed certificates, kubeconfigs, membership or state: %+v", r)
					}
					if resumed {
						if kept, err := os.ReadFile(joinIntentPath); err != nil || !bytes.Equal(kept, journal) {
							t.Fatalf("refusal changed the recovery journal: %v", err)
						}
					} else if exists, _, _ := snapshotIntent(); exists {
						t.Fatal("refused join wrote an intent journal")
					}
					return
				}
				if err != nil {
					t.Fatalf("join rejected equivalent CIDRs: %v", err)
				}
				if r.saved == nil || r.saved.PodCIDR != pods || r.saved.ServiceCIDR != services {
					t.Fatalf("join did not retain the recorded CIDRs: %+v", r.saved)
				}
				if resumed && r.addCalls != 0 {
					t.Fatalf("resumed join added %d members instead of adopting the recorded member", r.addCalls)
				}
			})
		}
	}
}

// TestResumedJoinRefusesADifferingNetworkNamingTheFieldAndBothValues: a join
// resumed from its recovery journal refuses a spec.network.podCIDR or
// serviceCIDR that differs from the range the journal recorded, each field on
// its own, and the refusal names the field, the configured value and the
// recorded value. Up passes spec.network into the join, so a config edited
// between the failed run and its re-run reaches this refusal, and a message
// that names neither leaves the user to guess which line to fix. The refusal
// changes nothing — no certificate, kubeconfig, pod-range trust, member-add or
// state save — and keeps the journal, so the re-run with the recorded ranges
// resumes the join.
func TestResumedJoinRefusesADifferingNetworkNamingTheFieldAndBothValues(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		podCIDR, serviceCIDR     string
		field, given, recorded   string
		matchingOrUnsetFieldName string
	}{
		{"a lone differing pod CIDR", "10.99.0.0/16", "",
			"spec.network.podCIDR", "10.99.0.0/16", "10.42.0.0/16", "spec.network.serviceCIDR"},
		{"a lone differing service CIDR", "", "10.98.0.0/16",
			"spec.network.serviceCIDR", "10.98.0.0/16", "10.43.0.0/16", "spec.network.podCIDR"},
		{"a differing pod CIDR beside the recorded service CIDR", "10.99.0.0/16", "10.43.0.0/16",
			"spec.network.podCIDR", "10.99.0.0/16", "10.42.0.0/16", "spec.network.serviceCIDR"},
		{"a differing service CIDR beside the recorded pod CIDR", "10.42.0.0/16", "10.98.0.0/16",
			"spec.network.serviceCIDR", "10.98.0.0/16", "10.43.0.0/16", "spec.network.podCIDR"},
		{"the built-in service CIDR on a journal that records another", "", netdefault.ServiceCIDR,
			"spec.network.serviceCIDR", netdefault.ServiceCIDR, "10.43.0.0/16", "spec.network.podCIDR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			customBundleNetwork(t)
			etcdDir := t.TempDir()
			// The wedge: the first run, with spec.network unset, adds its
			// member and fails the state save with quorum unconfirmed, so the
			// journal survives as the recovery record of the cluster's ranges.
			r1 := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full"),
				quorumHeldErr: errors.New("no quorum")}
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("the first join must fail after adding its member")
			}
			j, err := readJoinIntent()
			if err != nil || j == nil || j.Recovery == nil {
				t.Fatalf("the failed join left no recovery journal: %+v, %v", j, err)
			}
			if j.Recovery.State.PodCIDR != "10.42.0.0/16" || j.Recovery.State.ServiceCIDR != "10.43.0.0/16" {
				t.Fatalf("the journal records pod CIDR %q and service CIDR %q, want the cluster's 10.42.0.0/16 and 10.43.0.0/16",
					j.Recovery.State.PodCIDR, j.Recovery.State.ServiceCIDR)
			}
			wedged, err := os.ReadFile(joinIntentPath)
			if err != nil {
				t.Fatal(err)
			}

			r2 := &recordedJoin{listMembers: listedMembers()}
			fx := fakeJoinEffects(r2)
			fx.trustPodCIDRs = func(string) {
				r2.order = append(r2.order, "trust pod cidrs")
				t.Error("the refused resume trusted a pod range")
			}
			opts := freshJoinOpts()
			opts.PodCIDR, opts.ServiceCIDR = tc.podCIDR, tc.serviceCIDR
			_, err = prepareControlPlaneJoin(context.Background(), opts, etcdDir, fx)
			// The re-run resumed from the journal: a fresh join would reach
			// the bundle's netconfig check, whose refusal is another one.
			if slices.Contains(r2.order, "discover") || slices.Contains(r2.order, "download") {
				t.Fatalf("the re-run did not resume from the journal: effects %v", r2.order)
			}
			if err == nil {
				t.Fatalf("the resumed join succeeded with %s %s on a journal that records %s; want a refusal",
					tc.field, tc.given, tc.recorded)
			}
			for _, want := range []string{tc.field, tc.given, tc.recorded, joinIntentPath} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal missing %q: %v", want, err)
				}
			}
			// Each value stands in its role: the configured one beside its
			// field, the recorded one as the value to set. A refusal that
			// swapped them would tell the user to keep the wrong range.
			for _, want := range []string{fmt.Sprintf("%s %q", tc.field, tc.given), fmt.Sprintf("set it to %q", tc.recorded)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not give %q in its role: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), tc.matchingOrUnsetFieldName) {
				t.Errorf("refusal blames %s, which is unset or matches the journal: %v", tc.matchingOrUnsetFieldName, err)
			}
			if r2.certCalled || r2.kcCalled || r2.addCalls != 0 || r2.removeByIDCalls != 0 || r2.removeCalled || r2.saved != nil {
				t.Errorf("the refused resume issued certificates (%v), wrote kubeconfigs (%v), added (%d) or removed (%d, %v) a member, or saved node state (%v)",
					r2.certCalled, r2.kcCalled, r2.addCalls, r2.removeByIDCalls, r2.removeCalled, r2.saved != nil)
			}
			if kept, err := os.ReadFile(joinIntentPath); err != nil || !bytes.Equal(kept, wedged) {
				t.Fatalf("the refused resume changed or lost the journal (%v):\n got %s\nwant %s", err, kept, wedged)
			}

			// The remedy: the re-run with the recorded ranges resumes the join
			// on them, adopting the journal's member without a second add.
			r3 := &recordedJoin{listMembers: listedMembers()}
			opts.PodCIDR, opts.ServiceCIDR = "10.42.0.0/16", "10.43.0.0/16"
			if _, err := prepareControlPlaneJoin(context.Background(), opts, etcdDir, fakeJoinEffects(r3)); err != nil {
				t.Fatalf("the re-run with the recorded ranges refused: %v", err)
			}
			if r3.addCalls != 0 || r3.saved == nil || r3.saved.PodCIDR != "10.42.0.0/16" || r3.saved.ServiceCIDR != "10.43.0.0/16" {
				t.Errorf("the re-run with the recorded ranges ran %d member-adds and saved node state %+v, want none and the recorded ranges",
					r3.addCalls, r3.saved)
			}
		})
	}
}

// --- the VIP check ---
//
// A joining controller with a VIP writes its kubeconfigs against the VIP, so a
// VIP that is not this cluster's apiserver (a mistyped spec.vip.address, or an
// address another service answers on) would leave the new member's clients
// dialing an address that does not serve the cluster. The join checks the VIP
// with VIPServes, a TLS handshake that trusts only the cluster CA the
// kubeconfigs carry and requires the VIP in the certificate, after those
// kubeconfigs are written and before anything about membership moves: the
// intent journal, the etcd data directory, the member-add. A member the intent
// journal records and the cluster still lists is adopted rather than added,
// and adoption skips the check (join_recovery_test.go).

// TestControlPlaneJoinChecksTheVIPBeforeTheMemberAdd: a fresh join whose VIP
// serves the cluster checks it once, for the VIP's address, after the
// kubeconfigs it reads the cluster CA from are written and before the intent
// file exists, and then proceeds to the member-add as before.
func TestControlPlaneJoinChecksTheVIPBeforeTheMemberAdd(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
	if err != nil {
		t.Fatalf("fresh join through a VIP that serves the cluster: %v", err)
	}
	if r.vipCalls != 1 || r.vipAddr != "10.0.0.10" {
		t.Fatalf("the VIP check ran %d times for %q, want once for the join's VIP 10.0.0.10", r.vipCalls, r.vipAddr)
	}
	if !r.vipAfterKubeconfigs {
		t.Error("the VIP check ran before the kubeconfigs were written, so it could not verify against the CA they carry")
	}
	if r.intentAtVIP {
		t.Error("the intent file existed when the VIP check ran: a refusal would leave a journal for a member never added")
	}
	kc, check, add := slices.Index(r.order, "write kubeconfigs"), slices.Index(r.order, "vip check"), slices.Index(r.order, "member-add")
	if kc < 0 || check < 0 || add < 0 || kc > check || check > add {
		t.Errorf("effects ran in order %v, want the kubeconfigs written, then the VIP check, then the member-add", r.order)
	}
	if r.addCalls != 1 || r.addPeer != "https://192.0.2.9:2380" {
		t.Errorf("the join ran %d member-adds for %q, want one for this node's https://192.0.2.9:2380", r.addCalls, r.addPeer)
	}
	if r.saved == nil || st == nil || st.EtcdState != "existing" {
		t.Fatalf("the join saved no joined node state: saved=%v returned=%v", r.saved, st)
	}
}

// TestControlPlaneJoinRefusesAVIPThatDoesNotServe: a VIP that does not answer
// as this cluster's apiserver refuses the join before membership, both for a
// fresh join and for one replacing a stale intent (a journal whose member the
// cluster no longer lists, which proceeds as a fresh member-add). The join goes
// through cp1's own address, so only the VIP puts 10.0.0.10 in the refusal. The
// refusal names the VIP, carries the check's error, says nothing was added to
// etcd, and says to set spec.vip.address to the cluster's VIP. Nothing about
// membership moved: no member-add and no saved state, the intent file as it was
// (absent, or the stale journal byte for byte), and the etcd data directory the
// same directory at the same mode, where prepareEmptyEtcdDataDir would have
// replaced it with a new one at 0700.
func TestControlPlaneJoinRefusesAVIPThatDoesNotServe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		intent string // the journal on disk before the join; "" for none
	}{
		{"a fresh join", ""},
		{"a join replacing a stale intent", `{"memberName":"cp9","peerURL":"https://198.51.100.7:2380","memberID":77,"phase":"added"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			if tc.intent != "" {
				if err := os.WriteFile(joinIntentPath, []byte(tc.intent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			etcdDir := filepath.Join(t.TempDir(), "etcd")
			if err := os.Mkdir(etcdDir, 0o750); err != nil {
				t.Fatal(err)
			}
			// The umask may have narrowed the mode Mkdir was given.
			if err := os.Chmod(etcdDir, 0o750); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(etcdDir)
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("x509: certificate signed by unknown authority")
			r := &recordedJoin{listMembers: listedMembers(), failVIP: cause}
			opts := freshJoinOpts()
			opts.Server = "https://10.0.0.11:6443"
			_, err = prepareControlPlaneJoin(context.Background(), opts, etcdDir, fakeJoinEffects(r))
			if err == nil {
				t.Fatal("the join proceeded although its VIP does not answer as the cluster's apiserver")
			}
			if r.vipCalls != 1 || r.vipAddr != "10.0.0.10" {
				t.Errorf("the VIP check ran %d times for %q, want once for the join's VIP 10.0.0.10", r.vipCalls, r.vipAddr)
			}
			if !errors.Is(err, cause) {
				t.Errorf("the refusal does not carry the check's error %q: %v", cause, err)
			}
			for _, want := range []string{"10.0.0.10", "spec.vip.address"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is missing %q: %v", want, err)
				}
			}
			if !strings.Contains(strings.ToLower(err.Error()), "nothing was added to etcd") {
				t.Errorf("the refusal does not say nothing was added to etcd: %v", err)
			}
			if r.addCalled || r.addCalls != 0 || r.removeCalled || r.removeByIDCalls != 0 || r.saved != nil {
				t.Errorf("the refused join touched membership: member-adds=%d remove=%v remove-by-ID=%d saved=%v",
					r.addCalls, r.removeCalled, r.removeByIDCalls, r.saved)
			}
			raw, readErr := os.ReadFile(joinIntentPath)
			switch {
			case tc.intent == "" && !errors.Is(readErr, fs.ErrNotExist):
				t.Errorf("the refused join wrote an intent file (%v): %q", readErr, raw)
			case tc.intent != "" && string(raw) != tc.intent:
				t.Errorf("the refused join replaced the stale journal (%v): got %q, want %q", readErr, raw, tc.intent)
			}
			after, err := os.Stat(etcdDir)
			if err != nil {
				t.Fatalf("the refused join removed the etcd data directory: %v", err)
			}
			if !os.SameFile(before, after) || after.Mode().Perm() != 0o750 {
				t.Errorf("the refused join replaced the etcd data directory: same directory %v, mode %o, want the original at 0750",
					os.SameFile(before, after), after.Mode().Perm())
			}
		})
	}
}

// TestControlPlaneJoinRefusesALocalCAProblemAsSuch: a VIP check that never
// reached the VIP, because no cluster CA could be read from this node's
// kubeconfigs, refuses the join before membership as a check that could not
// run. The refusal names the VIP, carries the check's error and says nothing
// was added to etcd, but it neither reports the VIP as not answering nor tells
// the operator to change spec.vip.address: the address may be right, and
// changing it would not fix a local problem. Any other failure of the check
// keeps the refusal that says the VIP does not answer, and gives both ways
// out: set spec.vip.address when it is wrong, or, when it is right, bring the
// VIP up on the existing controllers by the steps ha-repoint's refusal gives.
// The join goes through cp1's own address, so only the VIP puts 10.0.0.10 in
// the refusal, and neither row adds a member, saves state or writes the
// intent.
func TestControlPlaneJoinRefusesALocalCAProblemAsSuch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		want    []string
		wantNot []string
	}{
		{"a check that could not read the cluster CA",
			fmt.Errorf("%w: kubelet.kubeconfig: permission denied", errNoClusterCA),
			[]string{"10.0.0.10", "cannot check"}, []string{"spec.vip.address", "does not answer", "systemctl restart stratum"}},
		{"a VIP that does not answer as the cluster's apiserver",
			errors.New("x509: certificate signed by unknown authority"),
			[]string{"10.0.0.10", "does not answer", "spec.vip.address", "on every controller",
				"sudo systemctl restart stratum", "curl --cacert /var/lib/stratum/pki/ca.crt https://10.0.0.10:6443/healthz"},
			[]string{"cannot check", "ha-repoint"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{listMembers: listedMembers(), failVIP: tc.cause}
			opts := freshJoinOpts()
			opts.Server = "https://10.0.0.11:6443"
			_, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatal("the join proceeded although its VIP check failed")
			}
			if r.vipCalls != 1 || r.vipAddr != "10.0.0.10" {
				t.Errorf("the VIP check ran %d times for %q, want once for the join's VIP 10.0.0.10", r.vipCalls, r.vipAddr)
			}
			if !errors.Is(err, tc.cause) {
				t.Errorf("the refusal does not carry the check's error %q: %v", tc.cause, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is missing %q: %v", want, err)
				}
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("the refusal says %q, which does not describe this failure: %v", unwanted, err)
				}
			}
			if !strings.Contains(strings.ToLower(err.Error()), "nothing was added to etcd") {
				t.Errorf("the refusal does not say nothing was added to etcd: %v", err)
			}
			if r.addCalled || r.addCalls != 0 || r.removeCalled || r.removeByIDCalls != 0 || r.saved != nil {
				t.Errorf("the refused join touched membership: member-adds=%d remove=%v remove-by-ID=%d saved=%v",
					r.addCalls, r.removeCalled, r.removeByIDCalls, r.saved)
			}
			if raw, readErr := os.ReadFile(joinIntentPath); !errors.Is(readErr, fs.ErrNotExist) {
				t.Errorf("the refused join wrote an intent file (%v): %q", readErr, raw)
			}
		})
	}
}

// TestControlPlaneJoinRefusesAVIPThatIsANodeAddress: a spec.vip.address that
// is an existing controller's own address, such as the host copied from a
// --join URL, passes the TLS check, because every controller's apiserver
// certificate names that controller's node addresses. A join that took it
// would point every kubeconfig on the new controller at that one controller,
// and its kube-vip would announce that controller's address as the VIP. The
// published etcd endpoints identify their primary addresses; the node records
// add the secondary addresses. A VIP among them, or the joining node's own address, is refused
// before membership, with the address it is and what to set instead. The VIP
// check answers that the VIP serves, as cp1's apiserver would.
func TestControlPlaneJoinRefusesAVIPThatIsANodeAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		vip  string
		want string
	}{
		{"an existing controller's address", "10.0.0.11", "an existing controller's node address"},
		{"this node's own address", "192.0.2.9", "this node's own address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{listMembers: listedMembers()}
			opts := freshJoinOpts()
			opts.Server = "https://10.0.0.11:6443"
			opts.VIP = nodestate.VIP{Address: tc.vip}
			_, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatalf("the join proceeded with spec.vip.address %s, which is %s", tc.vip, tc.want)
			}
			for _, want := range []string{tc.vip, tc.want, "spec.vip.address", "the cluster's VIP"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is missing %q: %v", want, err)
				}
			}
			if !strings.Contains(strings.ToLower(err.Error()), "nothing was added to etcd") {
				t.Errorf("the refusal does not say nothing was added to etcd: %v", err)
			}
			if r.addCalled || r.addCalls != 0 || r.saved != nil {
				t.Errorf("the refused join touched membership: member-adds=%d saved=%v", r.addCalls, r.saved)
			}
			if raw, readErr := os.ReadFile(joinIntentPath); !errors.Is(readErr, fs.ErrNotExist) {
				t.Errorf("the refused join wrote an intent file (%v): %q", readErr, raw)
			}
		})
	}
}

func TestControlPlaneJoinRefusesAControllerSecondaryAddressAsVIP(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale intent=%t", stale), func(t *testing.T) {
			joinFixture(t)
			var intent []byte
			if stale {
				intent = []byte(`{"memberName":"cp9","peerURL":"https://198.51.100.7:2380","memberID":77,"phase":"added"}`)
				if err := os.WriteFile(joinIntentPath, intent, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			before, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			r := &recordedJoin{listMembers: listedMembers()}
			fx := fakeJoinEffects(r)
			opts := freshJoinOpts()
			opts.Server = "https://10.0.0.11:6443"
			opts.VIP.Address = "fd00:0:0:0:0:0:0:11"
			reads := 0
			fx.controllerNodeIPs = func(ctx context.Context, server string, primary []string) ([]string, error) {
				reads++
				if server != opts.Server || !slices.Equal(primary, []string{"10.0.0.11"}) || !r.kcCalled || ctx.Err() != nil {
					t.Fatalf("controller address read: server=%s primary=%v kubeconfigs=%t context=%v", server, primary, r.kcCalled, ctx.Err())
				}
				return []string{"10.0.0.11", "fd00::11"}, nil
			}
			_, err = prepareControlPlaneJoin(t.Context(), opts, dir, fx)
			if err == nil || !strings.Contains(err.Error(), "an existing controller's node address") || !strings.Contains(err.Error(), "nothing was added to etcd") {
				t.Fatalf("secondary IPv6 VIP: err=%v; want node-address refusal before member-add", err)
			}
			if reads != 1 || r.vipCalls != 0 || r.addCalls != 0 || r.removeByIDCalls != 0 || r.removeCalled || r.saved != nil {
				t.Fatalf("refusal effects: reads=%d TLS checks=%d adds=%d removes=%d remove=%t saved=%v", reads, r.vipCalls, r.addCalls, r.removeByIDCalls, r.removeCalled, r.saved)
			}
			raw, readErr := os.ReadFile(joinIntentPath)
			if stale && (readErr != nil || !bytes.Equal(raw, intent)) || !stale && !errors.Is(readErr, fs.ErrNotExist) {
				t.Fatalf("refusal changed the join intent: %q, %v", raw, readErr)
			}
			after, statErr := os.Stat(dir)
			if statErr != nil || !os.SameFile(before, after) {
				t.Fatalf("refusal replaced the etcd directory: %v", statErr)
			}
		})
	}
}

// A reused certificate bundle can predate an unlabeled controller's join.
// Identify that controller from the live etcd probe, rather than the bundle.
func TestControlPlaneJoinChecksControllersAddedAfterBundlePublication(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	fx := fakeJoinEffects(r)
	fx.probeEtcd = func(seed []string) ([]string, error) {
		if !slices.Equal(seed, []string{"10.0.0.11"}) {
			t.Fatalf("probe endpoints = %v, want the published seed", seed)
		}
		return []string{"10.0.0.11", "10.0.0.12"}, nil
	}
	srv := joinAddressServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(corev1.NodeList{Items: []corev1.Node{
			{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
			}}},
			{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.12"},
				{Type: corev1.NodeInternalIP, Address: "fd00::12"},
			}}},
		}})
	})
	fx.controllerNodeIPs = controllerNodeIPsForJoin
	o := freshJoinOpts()
	o.Server, o.VIP.Address = srv.URL, "fd00::12"
	_, err := prepareControlPlaneJoin(t.Context(), o, t.TempDir(), fx)
	if err == nil || !strings.Contains(err.Error(), "an existing controller's node address") {
		t.Fatalf("newer controller's secondary VIP: %v; want node-address refusal", err)
	}
	if r.addCalls != 0 || r.vipCalls != 0 || r.saved != nil {
		t.Fatalf("unsafe join effects: adds=%d TLS checks=%d saved=%v", r.addCalls, r.vipCalls, r.saved)
	}
}

func TestControlPlaneJoinRefusesWhenControllerAddressesCannotBeRead(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	fx := fakeJoinEffects(r)
	cause := errors.New("node list unavailable")
	fx.controllerNodeIPs = func(context.Context, string, []string) ([]string, error) { return nil, cause }
	_, err := prepareControlPlaneJoin(t.Context(), freshJoinOpts(), t.TempDir(), fx)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "nothing was added to etcd") {
		t.Fatalf("node-address read failure: %v; want refusal carrying the cause", err)
	}
	if r.vipCalls != 0 || r.addCalls != 0 || r.saved != nil {
		t.Fatalf("refusal effects: TLS checks=%d adds=%d saved=%v", r.vipCalls, r.addCalls, r.saved)
	}
	if raw, readErr := os.ReadFile(joinIntentPath); !errors.Is(readErr, fs.ErrNotExist) {
		t.Fatalf("refusal created an intent: %q, %v", raw, readErr)
	}
}

// TestControlPlaneJoinWithoutAVIPSkipsTheVIPCheck: a join without a VIP writes
// its kubeconfigs against the local apiserver and has no VIP to check, so the
// check never runs (one that would refuse proves it) and the join proceeds to
// the member-add.
func TestControlPlaneJoinWithoutAVIPSkipsTheVIPCheck(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers(), failVIP: errors.New("there is no VIP to check")}
	opts := freshJoinOpts()
	opts.Server = "https://10.0.0.11:6443"
	opts.VIP = nodestate.VIP{}
	fx := fakeJoinEffects(r)
	fx.controllerNodeIPs = func(context.Context, string, []string) ([]string, error) {
		t.Fatal("a join without a VIP read controller addresses")
		return nil, nil
	}
	if _, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx); err != nil {
		t.Fatalf("join without a VIP: %v", err)
	}
	if r.vipCalls != 0 {
		t.Errorf("a join without a VIP ran the VIP check %d times, for %q", r.vipCalls, r.vipAddr)
	}
	if r.addCalls != 1 || r.saved == nil {
		t.Errorf("the join without a VIP ran %d member-adds and saved %v, want one add and a saved state", r.addCalls, r.saved)
	}
}

// TestProductionJoinEffectsCheckTheVIPWithVIPServes: the production effect set
// checks the VIP with VIPServes, the TLS check ha-repoint runs, so the join's
// refusal rests on the same handshake, and a test that stands in for VIPServes
// stands in for the join's check too.
func TestProductionJoinEffectsCheckTheVIPWithVIPServes(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.10:6443: connect: connection refused")
	var asked []string
	old := VIPServes
	VIPServes = func(vip string) error { asked = append(asked, vip); return cause }
	t.Cleanup(func() { VIPServes = old })
	fx := productionJoinEffects()
	if fx.vipServes == nil {
		t.Fatal("the production join effects have no VIP check")
	}
	if err := fx.vipServes("10.0.0.10"); !errors.Is(err, cause) {
		t.Errorf("the production VIP check answered %v, want VIPServes's %v", err, cause)
	}
	if !slices.Equal(asked, []string{"10.0.0.10"}) {
		t.Errorf("VIPServes was asked about %q, want only the VIP 10.0.0.10", asked)
	}
}

// TestSettleJoinSecrets: a join keeps exactly the key its bundle carried. A key on disk that the bundle did not supply, or one a
// refused join just downloaded, is removed, and the removal is said: a line on
// the join's output, and in the refusal when there is one.
func TestSettleJoinSecrets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cis        bool
		carriedKey bool
		keyOnDisk  bool
		refusal    string // "" for none
		keyLeft    bool
		said       string // "" for no line
	}{
		{"cis, the bundle carried the key", true, true, true, "", true, ""},
		{"no cis, keyless bundle, no key on disk", false, false, false, "", false, ""},
		{"no cis, keyless bundle over a stale key: removed", false, false, true, "", false,
			"left by an earlier attempt on this node"},
		{"cis, keyless bundle over a stale key: removed and refused", true, false, true,
			"carry no at-rest encryption key", false, "left by an earlier attempt on this node"},
		{"cis, keyless bundle, no key on disk: refused", true, false, false,
			"carry no at-rest encryption key", false, ""},
		{"no cis, the bundle carried a key: removed and refused", false, true, true,
			"Re-run this join with --profile cis", false, "this join is refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := filepath.Join(t.TempDir(), "encryption-config.yaml")
			if tc.keyOnDisk {
				if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var synced []string
			saved := syncJoinKeyDir
			syncJoinKeyDir = func(d string) error { synced = append(synced, d); return nil }
			t.Cleanup(func() { syncJoinKeyDir = saved })
			var out bytes.Buffer
			err := settleJoinSecrets(cisFor(tc.cis), keyBundle(tc.carriedKey), true, filepath.Dir(key), &out)
			if (err != nil) != (tc.refusal != "") {
				t.Fatalf("err = %v, want refusal %q", err, tc.refusal)
			}
			if tc.refusal != "" && !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("refusal is missing %q: %v", tc.refusal, err)
			}
			removed := tc.keyOnDisk && !tc.keyLeft
			if want := []string{filepath.Dir(key)}; removed && !slices.Equal(synced, want) {
				t.Errorf("synced %v after removing the key, want %v", synced, want)
			} else if !removed && len(synced) != 0 {
				t.Errorf("synced %v without removing anything", synced)
			}
			if tc.refusal != "" && removed && !strings.Contains(err.Error(), "was removed from "+key) {
				t.Errorf("refusal does not say the key was removed: %v", err)
			}
			if !tc.carriedKey && (strings.Contains(out.String(), "downloaded") ||
				err != nil && strings.Contains(err.Error(), "key this join downloaded")) {
				t.Errorf("says the key was downloaded from a keyless bundle: %q, %v", out.String(), err)
			}
			_, statErr := os.Stat(key)
			if left := statErr == nil; left != tc.keyLeft {
				t.Errorf("key left on disk = %v, want %v", left, tc.keyLeft)
			}
			if tc.said == "" {
				if out.Len() != 0 {
					t.Errorf("printed %q, want nothing", out.String())
				}
				return
			}
			if !strings.HasPrefix(out.String(), "stratum HA join: removed "+key) || !strings.Contains(out.String(), tc.said) {
				t.Errorf("printed %q, want a join line saying %q", out.String(), tc.said)
			}
		})
	}
	t.Run("an unsynced removal is refused, not reported done", func(t *testing.T) {
		saved := syncJoinKeyDir
		syncJoinKeyDir = func(string) error { return errors.New("EIO") }
		t.Cleanup(func() { syncJoinKeyDir = saved })
		key := filepath.Join(t.TempDir(), "encryption-config.yaml")
		if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := settleJoinSecrets(profile.Profile{}, bundleProfile{}, true, filepath.Dir(key), &out)
		if err == nil || !strings.Contains(err.Error(), "could not be made durable") || !strings.Contains(err.Error(), "EIO") {
			t.Fatalf("err = %v, want the unsynced removal refused with its cause", err)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q for a removal that is not durable", out.String())
		}
	})
}

// TestCISSecretIsTheKeyPath: a joiner writes each bundle entry to
// PKIDir/<entry>, and the apiserver reads the cis profile's key from
// version.EncryptionConfigPath. One is the other only while that path sits
// directly in PKIDir under the secret's name.
func TestCISSecretIsTheKeyPath(t *testing.T) {
	if got := filepath.Dir(version.EncryptionConfigPath); got != version.PKIDir {
		t.Fatalf("EncryptionConfigPath is in %s, not PKIDir %s: a downloaded key would land where the apiserver does not read it", got, version.PKIDir)
	}
	names := cisFor(true).SecretNames()
	if len(names) != 1 || version.ProfileSecretFile(names[0]) != version.EncryptionConfigPath || names[0] != encryptionConfigEntry {
		t.Fatalf("the cis profile's secrets %v land at %s, the apiserver reads %s", names, version.ProfileSecretFile(names[0]), version.EncryptionConfigPath)
	}
	secrets, err := profileSecretEntries()
	if err != nil || !secrets[encryptionConfigEntry] {
		t.Fatalf("profileSecretEntries = %v, %v; want %s in the bundle", secrets, err, encryptionConfigEntry)
	}
}

// A registered profile whose secret takes the name of an entry the engine
// shares itself is refused, so the bundle never carries one file for both.
func TestProfileSecretEntriesRefuseAnEngineName(t *testing.T) {
	prev := profile.All()
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
	clash := profile.Profile{Name: "clash", SharedSecrets: []profile.Secret{{Name: "netconfig", Mint: func() ([]byte, error) { return nil, nil }}}}
	if err := profile.SetRegistered([]profile.Profile{clash}); err == nil || !strings.Contains(err.Error(), "netconfig") {
		t.Fatalf("SetRegistered = %v, want registration to refuse a secret named netconfig before any bundle is built", err)
	}
	if _, err := profileSecretEntries(); err != nil {
		t.Fatalf("profileSecretEntries after a refused registration = %v, want the previous profiles' entries", err)
	}
}

// A joining control plane whose config sets a cluster domain other
// than the cluster's is refused, before it issues certificates or touches etcd.
func TestCheckJoinDomain(t *testing.T) {
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })
	// What DownloadCerts leaves behind from a cluster initialised with fuzz.internal.
	if err := netcfg.Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
		t.Fatal(err)
	}

	for _, configured := range []string{"", "fuzz.internal"} { // unset, matching
		if err := checkJoinDomain(configured); err != nil {
			t.Errorf("checkJoinDomain(%q) = %v, want nil", configured, err)
		}
	}
	for _, configured := range []string{"other.internal", "cluster.local"} { // conflicting
		err := checkJoinDomain(configured)
		if err == nil || !strings.Contains(err.Error(), configured) || !strings.Contains(err.Error(), "fuzz.internal") {
			t.Errorf("checkJoinDomain(%q) = %v, want an error naming both domains", configured, err)
		}
	}
}

// --- the durable pre-add intent journal and post-add compensation ---
//
// A join's member-add is followed by fallible state writes with no rollback: a
// failure or a kill between them leaves a voting etcd member for a node that
// never starts. The join therefore journals its intent durably BEFORE the
// member-add — in the marker shape of internal/upgrade/resume.go:88-102
// (atomic rename, 0600) — clears it only once the node state has been
// persisted, and on a post-add failure compensates by removing the member by
// ID, but only when the cluster provably still has quorum. When the removal is
// refused or fails, the intent file survives as the recovery record and the
// error says how recovery proceeds.

// intentMemberID is the member ID the recorded member list answers with for
// this join's node, and the one the intent file must end up recording.
const intentMemberID = 1234

// listedMembers is what the recorded clusterMembers effect answers: a
// two-member cluster whose second member is this join's node — unstarted, so
// it advertises the join's peer URL and no name yet. The cluster's member
// list, not the intent file, is the truth about membership
// (internal/ha/etcd.go:405-416).
func listedMembers() etcdMembership {
	return etcdMembership{
		ClusterID: 1, Responder: 2, Quorum: true,
		Members: []etcdMember{
			{ID: 11, Name: "cp1", PeerURLs: []string{"https://10.0.0.11:2380"},
				ClientURLs: []string{"https://10.0.0.11:2379"}},
			{ID: intentMemberID, PeerURLs: []string{"https://192.0.2.9:2380"}},
		},
	}
}

// joinIntentFile is the intent file's on-disk JSON contract: the member
// identity a re-run needs to reconcile or remove this join's member, and the
// resumable phase it resumes from. Pinned here as plain JSON because the file
// outlives the process that wrote it — a re-run of `stratum up --join` reads
// it, and so does an operator.
type joinIntentFile struct {
	MemberName string `json:"memberName"`
	PeerURL    string `json:"peerURL"`
	MemberID   uint64 `json:"memberID"`
	Phase      string `json:"phase"`
}

// parseIntent parses an intent file snapshot, failing the test when the bytes
// do not have the recorded shape.
func parseIntent(t *testing.T, raw []byte) joinIntentFile {
	t.Helper()
	var f joinIntentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("the intent file is not the recorded JSON shape: %v (%q)", err, raw)
	}
	return f
}

// snapshotIntent reports whether the intent file exists at the moment an
// effect runs, with its bytes and permissions when it does, so the guards can
// pin the journal's place in the join without depending on what the join did
// to the file afterwards.
func snapshotIntent() (bool, []byte, os.FileMode) {
	fi, err := os.Stat(joinIntentPath)
	if err != nil {
		return false, nil, 0
	}
	raw, err := os.ReadFile(joinIntentPath)
	if err != nil {
		return true, nil, fi.Mode().Perm()
	}
	return true, raw, fi.Mode().Perm()
}

// TestControlPlaneJoinWritesTheIntentBeforeTheMemberAdd pins the journal's
// place in the join. The intent file is on disk before the member-add runs,
// still on disk when the node state is saved — clearing it any earlier would
// leave a voting member with nothing to recover from — and gone once the join
// has finished persisting its state. The pre-add file records the member name
// and peer URL; after the add it also records the member ID the cluster's own
// member list gives for this node's peer URL, and the phase says the member is
// added.
func TestControlPlaneJoinWritesTheIntentBeforeTheMemberAdd(t *testing.T) {
	joinFixture(t)
	// The journal's lifecycle is operator-visible: its decisions are printed,
	// and a test captures the seam the same way TestSettleJoinKey does.
	savedOut := joinOut
	var out bytes.Buffer
	joinOut = &out
	t.Cleanup(func() { joinOut = savedOut })
	r := &recordedJoin{listMembers: listedMembers()}
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r)); err != nil {
		t.Fatalf("fresh join: %v", err)
	}
	if !strings.Contains(out.String(), "recorded the join intent in "+joinIntentPath) ||
		!strings.Contains(out.String(), "before the member-add") {
		t.Errorf("the join does not say it recorded the pre-add intent: %q", out.String())
	}
	if !r.addCalled {
		t.Fatal("the member-add never ran")
	}
	if !r.intentAtAdd {
		t.Fatal("no intent file existed when the member-add ran")
	}
	if perm := r.intentAtAddPerm.Perm(); perm != 0o600 {
		t.Errorf("intent file mode = %o, want 0600", perm)
	}
	pre := parseIntent(t, r.intentAtAddRaw)
	if pre.MemberName != r.addName || pre.PeerURL != r.addPeer {
		t.Errorf("pre-add intent named %q at %q, want the member-add's %q at %q",
			pre.MemberName, pre.PeerURL, r.addName, r.addPeer)
	}
	if pre.MemberID != 0 {
		t.Errorf("pre-add intent recorded member ID %d before the add returned one", pre.MemberID)
	}
	if pre.Phase != "prepared" {
		t.Errorf("pre-add intent phase = %q, want \"prepared\"", pre.Phase)
	}
	if !r.intentAtSave {
		t.Fatal("the intent file was gone before the node state was saved")
	}
	post := parseIntent(t, r.intentAtSaveRaw)
	if post.MemberID != intentMemberID {
		t.Errorf("intent recorded member ID %d, want %d — the ID the cluster's member list gave for %q",
			post.MemberID, intentMemberID, r.addPeer)
	}
	if post.Phase != "added" {
		t.Errorf("post-add intent phase = %q, want \"added\"", post.Phase)
	}
	// The journal outlives the state save on purpose: etcd only starts later,
	// and a crash before its first WAL would leave a published node.yaml that
	// bare `stratum up` refuses and a re-run routes to the member restart.
	// With the journal in place, that state resumes offline instead.
	kept, readErr := os.ReadFile(joinIntentPath)
	if readErr != nil {
		t.Fatalf("the journal did not survive a completed join: %v", readErr)
	}
	if done := parseIntent(t, kept); done.MemberID != intentMemberID || done.Phase != "added" {
		t.Errorf("the surviving journal records %+v, want the joined member ID %d at phase \"added\"",
			done, intentMemberID)
	}
	if r.removeByIDCalls != 0 || r.quorumHeldCalls != 0 {
		t.Errorf("a successful join compensated: remove-by-ID=%d quorum checks=%d",
			r.removeByIDCalls, r.quorumHeldCalls)
	}
}

// TestWriteJoinIntentSyncsBeforeItReportsDone pins the journal's durability:
// the staged file is fsynced and the staging directory is synced after the
// rename, so a power loss inside the member-add window cannot lose the
// journal's directory entry — the journal is the node's only record of the
// member between the add and the node-state save, and a lost journal reads as
// "no unfinished join". The staged file is a hidden, uniquely named sibling
// at 0600, and a completed write leaves no staging file behind.
func TestWriteJoinIntentSyncsBeforeItReportsDone(t *testing.T) {
	joinFixture(t)
	var synced []string
	var presentAtSync []bool
	saved := syncJoinIntentDir
	syncJoinIntentDir = func(d string) error {
		_, err := os.Stat(joinIntentPath)
		presentAtSync = append(presentAtSync, err == nil)
		synced = append(synced, d)
		return nil
	}
	t.Cleanup(func() { syncJoinIntentDir = saved })
	if err := writeJoinIntent(joinIntent{MemberName: "cp9", PeerURL: "https://192.0.2.9:2380",
		Phase: intentPhasePrepared}); err != nil {
		t.Fatalf("writeJoinIntent: %v", err)
	}
	if want := []string{filepath.Dir(joinIntentPath)}; !slices.Equal(synced, want) {
		t.Errorf("synced %v, want %v", synced, want)
	}
	if len(presentAtSync) != 1 || !presentAtSync[0] {
		t.Errorf("the staging directory was synced %d time(s) with the renamed intent present %v",
			len(presentAtSync), presentAtSync)
	}
	fi, err := os.Stat(joinIntentPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("intent file mode = %o, want 0600", perm)
	}
	raw, err := os.ReadFile(joinIntentPath)
	if err != nil {
		t.Fatal(err)
	}
	if kept := parseIntent(t, raw); kept.MemberName != "cp9" || kept.PeerURL != "https://192.0.2.9:2380" {
		t.Errorf("the written journal records %+v, want the written intent", kept)
	}
	entries, err := os.ReadDir(filepath.Dir(joinIntentPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != intentFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the completed write left staging files behind: %v", names)
	}
}

// TestWriteJoinIntentLeavesNoStagingFile: a rename that fails is cleaned up —
// a failed write leaves the directory as it found it, not littered with
// staging siblings.
func TestWriteJoinIntentLeavesNoStagingFile(t *testing.T) {
	joinFixture(t)
	if err := os.MkdirAll(filepath.Dir(joinIntentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the intent file goes makes the rename fail.
	if err := os.Mkdir(joinIntentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJoinIntent(joinIntent{MemberName: "cp9", PeerURL: "https://192.0.2.9:2380",
		Phase: intentPhasePrepared}); err == nil {
		t.Fatal("writeJoinIntent reported success renaming into a directory")
	}
	entries, err := os.ReadDir(filepath.Dir(joinIntentPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != intentFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the failed write left staging files behind: %v", names)
	}
}

// TestWriteJoinIntentRefusesWhenTheDirectoryCannotBeMadeDurable: the journal
// is only as good as its durability, so a directory that cannot be synced is
// a refusal with its cause, not a silent maybe — the join that follows the
// write trusts the file to be there after a crash.
func TestWriteJoinIntentRefusesWhenTheDirectoryCannotBeMadeDurable(t *testing.T) {
	joinFixture(t)
	saved := syncJoinIntentDir
	syncJoinIntentDir = func(string) error { return errors.New("EIO") }
	t.Cleanup(func() { syncJoinIntentDir = saved })
	err := writeJoinIntent(joinIntent{MemberName: "cp9", PeerURL: "https://192.0.2.9:2380",
		Phase: intentPhasePrepared})
	if err == nil || !strings.Contains(err.Error(), "could not be made durable") || !strings.Contains(err.Error(), "EIO") {
		t.Fatalf("err = %v, want the unsynced intent refused with its cause", err)
	}
}

// TestControlPlaneJoinCompensatesForPostAddFailures drives a join that added
// its member and then failed writing its node state — the one fallible write
// that remains after the member-add, because the persisted state carries the
// initial-cluster string the add returned (or the adopted membership's). The
// join must attempt the member remove exactly once — by ID, once the quorum
// gate has authorised it — report the member removed, clear the intent file,
// and still fail with the cause. The peer-URL memberRemove is the member-add's
// own concurrent-data rollback, not this compensation. A runtime-config write
// that fails is a refusal before the add (it needs no add result), covered by
// TestControlPlaneJoinRefusesBeforeTheMemberAdd.
func TestControlPlaneJoinCompensatesForPostAddFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
		fail  func(r *recordedJoin)
	}{
		{"a node-state save that fails", "disk full",
			func(r *recordedJoin) { r.failSaveState = errors.New("disk full") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{listMembers: listedMembers()}
			tc.fail(r)
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatal("the join succeeded although a post-add write failed")
			}
			if !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("the error lost the original cause: %v", err)
			}
			if !strings.Contains(err.Error(), "removed") {
				t.Errorf("the error does not report the member removed: %v", err)
			}
			if r.quorumHeldCalls != 1 {
				t.Errorf("the quorum gate ran %d times, want once before the removal", r.quorumHeldCalls)
			}
			if r.removeByIDCalls != 1 {
				t.Fatalf("the member remove by ID ran %d times, want exactly once", r.removeByIDCalls)
			}
			if r.removeByIDID != intentMemberID {
				t.Errorf("the remove took member ID %d, want %d from the cluster's member list",
					r.removeByIDID, intentMemberID)
			}
			if !r.quorumBeforeRemove {
				t.Error("the member was removed before the quorum gate authorised it")
			}
			if r.removeCalled {
				t.Error("the peer-URL member remove ran instead of the by-ID compensation")
			}
			if _, err := os.Stat(joinIntentPath); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the intent file survived although the member was removed: %v", err)
			}
		})
	}
}

// TestControlPlaneJoinKeepsTheIntentWhenCompensationIsRefused drives a join
// whose member was added, whose state write failed, and whose removal is then
// refused by the quorum gate or fails outright. Removing a member a cluster
// may still need is worse than leaving one a re-run can reconcile, so the gate
// fails closed (confirmQuorumLost's semantics, internal/ha/quorum.go:39-69):
// the intent file with the member identity and resumable phase survives, and
// the error names how recovery proceeds.
func TestControlPlaneJoinKeepsTheIntentWhenCompensationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cause     string
		setup     func(r *recordedJoin)
		removeQty int
	}{
		{"a quorum check that cannot rule out quorum", "cannot rule out",
			func(r *recordedJoin) {
				r.failSaveState = errors.New("disk full")
				r.quorumHeldErr = errors.New("cannot rule out that the cluster still has quorum")
			},
			0},
		{"a member remove that fails", "EIO",
			func(r *recordedJoin) {
				r.failSaveState = errors.New("disk full")
				r.failRemoveByID = errors.New("EIO")
			},
			1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{listMembers: listedMembers()}
			tc.setup(r)
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatal("the join succeeded although its compensation was refused")
			}
			if !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("the error lost the compensation failure: %v", err)
			}
			if !strings.Contains(err.Error(), "re-run") || !strings.Contains(err.Error(), "intent") {
				t.Errorf("the error does not name the recovery path and the kept intent file: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("member remove %x", intentMemberID)) {
				t.Errorf("the error does not name the member ID the journal recorded: %v", err)
			}
			if strings.Contains(err.Error(), "member remove 0") {
				t.Errorf("the error advises removing member ID 0, which names no member: %v", err)
			}
			if r.removeByIDCalls != tc.removeQty {
				t.Errorf("the member remove by ID ran %d times, want %d",
					r.removeByIDCalls, tc.removeQty)
			}
			raw, readErr := os.ReadFile(joinIntentPath)
			if readErr != nil {
				t.Fatalf("the intent file did not survive the refused compensation: %v", readErr)
			}
			kept := parseIntent(t, raw)
			if kept.MemberName != r.addName || kept.PeerURL != r.addPeer || kept.MemberID != intentMemberID {
				t.Errorf("the kept intent lost the member identity: %+v (want %q at %q, ID %d)",
					kept, r.addName, r.addPeer, intentMemberID)
			}
			if kept.Phase != "added" {
				t.Errorf("kept intent phase = %q, want \"added\"", kept.Phase)
			}
		})
	}
}

// TestControlPlaneJoinCompensationWhenTheJournalNeverLearnedTheID drives the
// compensation whose journal sits at phase "prepared": the post-add read of
// the cluster's member list failed or did not yet show the member, so the
// journal carries no member ID. The compensation must then learn the ID from
// the cluster's own list — the list, not the file, is the truth about the
// member — refuse the removal fail closed when the list cannot be read or
// does not show the member, and never advise `member remove 0`, which names
// no member: until the ID is known the remedy is to find this node's member
// in the list by its peer URL, and once the list has answered, the remedy
// names the ID it gave. The first read here is the journal read after the
// member-add; the second is the compensation's own.
func TestControlPlaneJoinCompensationWhenTheJournalNeverLearnedTheID(t *testing.T) {
	neverAnswers := func() (etcdMembership, error) { return etcdMembership{}, errors.New("connection refused") }
	answersOnSecondRead := func(m etcdMembership) func() (etcdMembership, error) {
		calls := 0
		return func() (etcdMembership, error) {
			calls++
			if calls == 1 {
				return etcdMembership{}, errors.New("connection refused")
			}
			return m, nil
		}
	}
	// A member list that does not list this join's node: what the
	// compensation's re-read answers when the member it added is not there.
	withoutThisNode := etcdMembership{ClusterID: 1, Responder: 2, Quorum: true,
		Members: []etcdMember{{ID: 11, Name: "cp1", PeerURLs: []string{"https://10.0.0.11:2380"}}}}
	for _, tc := range []struct {
		name           string
		refusal        string
		members        func() (etcdMembership, error)
		quorumRefused  bool
		wantRemedy     string
		wantRemoveByID int
	}{
		{"the member list cannot be read to settle the ID", "cannot be learned",
			neverAnswers, false, "by its peer URL", 0},
		{"the member list does not show the member", "does not show it at",
			answersOnSecondRead(withoutThisNode), false, "by its peer URL", 0},
		{"the member list names the member, but the gate refuses the removal", "removing it is refused",
			answersOnSecondRead(listedMembers()), true,
			fmt.Sprintf("member remove %x", intentMemberID), 0},
		{"the member list names the member and the removal succeeds", "",
			answersOnSecondRead(listedMembers()), false, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{failSaveState: errors.New("disk full"), clusterMembersFn: tc.members}
			if tc.quorumRefused {
				r.quorumHeldErr = errors.New("cannot rule out that the cluster still has quorum")
			}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatal("the join succeeded although a post-add write failed")
			}
			if !strings.Contains(err.Error(), "disk full") {
				t.Errorf("the error lost the original cause: %v", err)
			}
			if strings.Contains(err.Error(), "member remove 0") {
				t.Errorf("the error advises removing member ID 0, which names no member: %v", err)
			}
			if tc.refusal != "" && !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("the error does not report the refusal (%q): %v", tc.refusal, err)
			}
			if tc.wantRemedy != "" && !strings.Contains(err.Error(), tc.wantRemedy) {
				t.Errorf("the error's remedy should say %q: %v", tc.wantRemedy, err)
			}
			if r.removeByIDCalls != tc.wantRemoveByID {
				t.Errorf("the member remove by ID ran %d times, want %d", r.removeByIDCalls, tc.wantRemoveByID)
			}
			if tc.wantRemoveByID == 1 {
				if r.removeByIDID != intentMemberID {
					t.Errorf("the remove took member ID %d, want the %d the member list gave",
						r.removeByIDID, intentMemberID)
				}
				if !r.quorumBeforeRemove {
					t.Error("the member was removed before the quorum gate authorised it")
				}
				if _, statErr := os.Stat(joinIntentPath); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("the intent file survived although the member was removed: %v", statErr)
				}
				return
			}
			if !strings.Contains(err.Error(), "re-run") || !strings.Contains(err.Error(), "intent") {
				t.Errorf("the error does not name the recovery path and the kept intent file: %v", err)
			}
			raw, readErr := os.ReadFile(joinIntentPath)
			if readErr != nil {
				t.Fatalf("the intent file did not survive the refused compensation: %v", readErr)
			}
			kept := parseIntent(t, raw)
			if kept.MemberName != r.addName || kept.PeerURL != r.addPeer {
				t.Errorf("the kept intent lost the member identity: %+v (want %q at %q)",
					kept, r.addName, r.addPeer)
			}
			if kept.MemberID != 0 {
				t.Errorf("the kept intent records member ID %d, want 0: the journal never learned one", kept.MemberID)
			}
			if kept.Phase != "prepared" {
				t.Errorf("kept intent phase = %q, want \"prepared\"", kept.Phase)
			}
		})
	}
}

// A committed second-voter add needs both voters for quorum. Until the
// joiner starts, even a healthy first member cannot commit compensation.
func TestControlPlaneJoinOneToTwoVoterCompensation(t *testing.T) {
	joinFixture(t)
	members := listedMembers()
	members.Quorum = false
	r := &recordedJoin{failSaveState: errors.New("disk full"), listMembers: members,
		quorumHeldErr: errors.New("one of two voters cannot answer a quorum read")}
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
	if err == nil {
		t.Fatal("join succeeded despite failed state save")
	}
	if r.addCalls != 1 || r.removeByIDCalls != 0 || r.removeCalled || r.quorumHeldCalls != 1 {
		t.Fatalf("unsafe compensation: adds=%d removes=%d peerRemove=%v gates=%d", r.addCalls, r.removeByIDCalls, r.removeCalled, r.quorumHeldCalls)
	}
	for _, want := range []string{"disk full", "removing it is refused", "quorum", "re-run", "intent", "stratum etcdctl member remove 4d2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in recovery diagnostics: %v", want, err)
		}
	}
	kept, err := readJoinIntent()
	if err != nil || kept == nil || kept.MemberID != intentMemberID || kept.Phase != intentPhaseAdded || kept.Recovery == nil {
		t.Fatalf("failed compensation lost recovery record: %+v %v", kept, err)
	}
}

// TestControlPlaneJoinRefusesAnUnreadableIntent pins the journal's fail-closed
// read: an intent file that exists but cannot be used — not valid JSON, naming
// no member, or not readable — refuses the join before the member-add.
// Treating it as absent would run a second member-add over the very membership
// the file exists to reconcile.
func TestControlPlaneJoinRefusesAnUnreadableIntent(t *testing.T) {
	write := func(t *testing.T, raw []byte, perm os.FileMode) {
		t.Helper()
		if err := os.WriteFile(joinIntentPath, raw, perm); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T)
	}{
		{"an intent file that is not valid JSON", func(t *testing.T) {
			write(t, []byte("{not json"), 0o600)
		}},
		{"an intent file that names no member", func(t *testing.T) {
			write(t, []byte(`{"memberName":"cp9","peerURL":"","phase":"prepared"}`), 0o600)
		}},
		{"an intent file that cannot be read", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root reads through file permissions")
			}
			write(t, []byte(`{"memberName":"cp9","peerURL":"https://192.0.2.9:2380","phase":"prepared"}`), 0o000)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			tc.write(t)
			r := &recordedJoin{}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if err == nil || !strings.Contains(err.Error(), joinIntentPath) {
				t.Fatalf("err = %v, want a refusal naming the intent file %s", err, joinIntentPath)
			}
			if r.addCalled {
				t.Error("the member-add ran although the intent file could not be read")
			}
			if r.saved != nil {
				t.Errorf("node state was saved although the intent file could not be read: %+v", r.saved)
			}
		})
	}
}

// --- resume on re-run and the ambiguous member-add ---
//
// Before the journal was reconciled, a failed join wedged the node: with no
// node.yaml a re-run of `stratum up --join` was treated as fresh, the
// retained-member guard passed on the empty etcd directory, and the retried
// member-add hit etcd's duplicate-peer-URL refusal, which left the stale
// member in place and the node unjoinable. The join now consults the intent
// journal against the cluster's own member list — the list, not the file, is
// the truth about the member (memberByPeerURL, internal/ha/etcd.go:405-416):
// an intent whose peer URL is still a member is adopted and the join finishes
// without a second add; an intent whose member is gone is stale and is
// replaced by a fresh one. The ambiguous in-run case is resolved the same way:
// a member-add whose reply was lost is settled by re-reading the member list,
// never guessed.

// TestControlPlaneJoinResumesFromTheIntentFile drives the re-run of a join
// that failed or was killed after its member-add: no node.yaml, an empty etcd
// data directory, the intent journal still on disk. The re-run adopts the
// recorded member from the cluster's list and finishes the join — one
// member-add across both runs — whatever phase the journal was left in: the
// state save was refused with the cluster's quorum unconfirmable, or the node
// died before the list could be read a second time.
func TestControlPlaneJoinResumesFromTheIntentFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wedge func(r *recordedJoin)
	}{
		{"the journal recorded the added member", func(r *recordedJoin) {
			r.listMembers = listedMembers()
			r.failSaveState = errors.New("disk full")
			r.quorumHeldErr = errors.New("cannot rule out that the cluster still has quorum")
		}},
		{"the journal never learned the member ID", func(r *recordedJoin) {
			r.failAdd = context.DeadlineExceeded
			r.clusterMembersFn = func() (etcdMembership, error) {
				return etcdMembership{}, errors.New("the cluster's member list cannot be read")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			etcdDir := t.TempDir()
			// The wedge: the first run adds the member and fails before the
			// node state is saved, leaving the journal as this node's recovery
			// record.
			r1 := &recordedJoin{}
			tc.wedge(r1)
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("the first join succeeded although its state save failed or its member-add was lost")
			}
			if r1.addCalls != 1 {
				t.Fatalf("the first join ran %d member-adds, want one", r1.addCalls)
			}
			wedged, err := os.ReadFile(joinIntentPath)
			if err != nil {
				t.Fatalf("the intent journal did not survive the failed join: %v", err)
			}
			if pre := parseIntent(t, wedged); pre.MemberName != r1.addName || pre.PeerURL != r1.addPeer {
				t.Errorf("the journal left by the failed join names %+v, want the added member %q at %q",
					pre, r1.addName, r1.addPeer)
			}
			// The re-run: the cluster's member list answers, and the recorded
			// member is still in it. The join adopts it and finishes.
			savedOut := joinOut
			var out bytes.Buffer
			joinOut = &out
			t.Cleanup(func() { joinOut = savedOut })
			r2 := &recordedJoin{listMembers: listedMembers()}
			st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r2))
			if err != nil {
				t.Fatalf("the re-run refused to resume: %v", err)
			}
			if !strings.Contains(out.String(), "adopting it instead of adding a second member") ||
				!strings.Contains(out.String(), joinIntentPath) ||
				!strings.Contains(out.String(), "https://192.0.2.9:2380") {
				t.Errorf("the resumed join does not say it adopted the recorded member: %q", out.String())
			}
			if r2.addCalls != 0 {
				t.Fatalf("the re-run ran %d member-adds; adopting the recorded member needs none", r2.addCalls)
			}
			if total := r1.addCalls + r2.addCalls; total != 1 {
				t.Errorf("the two runs ran %d member-adds in total, want one", total)
			}
			if r2.saved == nil || st == nil || st.EtcdState != "existing" {
				t.Fatalf("the resumed join saved no node state: saved=%v returned=%v", r2.saved, st)
			}
			// The adopted membership is what etcd must boot with: the cluster's
			// list, with this node's member under the peer URL the journal
			// recorded.
			if !strings.Contains(r2.saved.EtcdInitialCluster, "=https://192.0.2.9:2380") {
				t.Errorf("saved initial cluster %q does not name the adopted member at its peer URL",
					r2.saved.EtcdInitialCluster)
			}
			atSave := parseIntent(t, r2.intentAtSaveRaw)
			if atSave.MemberID != intentMemberID || atSave.Phase != "added" {
				t.Errorf("the journal at the state save records %+v, want the adopted member ID %d at phase \"added\"",
					atSave, intentMemberID)
			}
			if r2.removeByIDCalls != 0 || r2.quorumHeldCalls != 0 || r2.removeCalled {
				t.Errorf("the resumed join compensated although nothing failed: byID=%d quorum=%d remove=%v",
					r2.removeByIDCalls, r2.quorumHeldCalls, r2.removeCalled)
			}
			// The journal outlives the resumed save too: it ends when a later
			// start confirms this node's local etcd data, not when node.yaml
			// lands.
			if _, statErr := os.Stat(joinIntentPath); statErr != nil {
				t.Fatalf("the journal did not survive a resumed join that finished: %v", statErr)
			}
		})
	}
}

// TestControlPlaneJoinRefusesToAdoptAMemberAtAnotherAddress drives a re-run
// whose journal records a member the cluster still lists, but whose node IP
// this run resolves to another address — detection variance on a multi-NIC
// host, or a different --node-ip after the failed run. Adopting the recorded
// member would finish the join into a node state whose initial cluster names
// the recorded address while the node advertises the resolved one, which etcd
// refuses to start, and clearing the journal would destroy the recovery
// record; the join refuses instead, names both addresses and the removal
// remedy — the recorded ID when the journal learned one, never ID 0 — and
// leaves the journal on disk.
func TestControlPlaneJoinRefusesToAdoptAMemberAtAnotherAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wedge    func(r *recordedJoin)
		wedgedID uint64
		wantID   string // "" for the never-learned case, where the remedy must not name an ID
		detected string
	}{
		{"the journal recorded the added member", func(r *recordedJoin) {
			r.listMembers = listedMembers()
			r.failSaveState = errors.New("disk full")
			r.quorumHeldErr = errors.New("cannot rule out that the cluster still has quorum")
		}, intentMemberID, fmt.Sprintf("member remove %x", intentMemberID), "192.0.2.99"},
		{"the journal never learned the member ID", func(r *recordedJoin) {
			r.failAdd = context.DeadlineExceeded
			r.clusterMembersFn = func() (etcdMembership, error) {
				return etcdMembership{}, errors.New("the cluster's member list cannot be read")
			}
		}, 0, "", "192.0.2.99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			etcdDir := t.TempDir()
			// The wedge: the first run adds the member at 192.0.2.9 and fails
			// before the node state is saved, leaving the journal as this
			// node's recovery record.
			r1 := &recordedJoin{}
			tc.wedge(r1)
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("the first join succeeded although its state save failed or its member-add was lost")
			}
			if _, err := os.Stat(joinIntentPath); err != nil {
				t.Fatalf("the intent journal did not survive the failed join: %v", err)
			}
			// The re-run resolves a different node IP: the cluster still lists
			// the member at the recorded peer URL, but this run's own peer URL
			// is another one.
			r2 := &recordedJoin{listMembers: listedMembers(), detectIP: tc.detected}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r2))
			if err == nil {
				t.Fatal("the re-run adopted the recorded member although it resolved another address")
			}
			for _, want := range []string{
				joinIntentPath,
				"https://192.0.2.9:2380",
				"https://192.0.2.99:2380",
				"stratum etcdctl member",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is missing %q: %v", want, err)
				}
			}
			if tc.wantID != "" {
				if !strings.Contains(err.Error(), tc.wantID) {
					t.Errorf("the refusal does not name the recorded member ID: %v", err)
				}
			} else {
				if !strings.Contains(err.Error(), "stratum etcdctl member list") {
					t.Errorf("the refusal does not name the member-list lookup the ID-0 remedy needs: %v", err)
				}
				if strings.Contains(err.Error(), "member remove 0") {
					t.Errorf("the refusal advises removing member ID 0, which names no member: %v", err)
				}
			}
			// The journal is the recovery record: it survives the refusal with
			// the identity the first run left.
			raw, readErr := os.ReadFile(joinIntentPath)
			if readErr != nil {
				t.Fatalf("the intent journal did not survive the refusal: %v", readErr)
			}
			if kept := parseIntent(t, raw); kept.MemberID != tc.wedgedID || kept.PeerURL != "https://192.0.2.9:2380" {
				t.Errorf("the kept journal records %+v, want the first run's member at ID %d", kept, tc.wedgedID)
			}
			// The refusal is the whole action: no second add, nothing removed.
			if r2.addCalls != 0 {
				t.Errorf("the refused re-run ran %d member-adds", r2.addCalls)
			}
			if r2.removeCalled || r2.removeByIDCalls != 0 || r2.quorumHeldCalls != 0 {
				t.Errorf("the refused re-run touched membership: remove=%v byID=%d quorum=%d",
					r2.removeCalled, r2.removeByIDCalls, r2.quorumHeldCalls)
			}
		})
	}
}

// TestControlPlaneJoinReplacesAStaleIntent drives the re-run whose recorded
// member is no longer in the cluster — an operator removed it by hand, per the
// recovery advice. The journal is the recovery record, not the truth: the
// cluster's member list is (memberByPeerURL, internal/ha/etcd.go:405-416), so
// the stale intent is replaced by a fresh one for this run's member and the
// join proceeds as a fresh add. Nothing removes the recorded member — it is
// already gone.
func TestControlPlaneJoinReplacesAStaleIntent(t *testing.T) {
	joinFixture(t)
	// The stale journal is the failed join's only diagnostics: discarding it
	// is said, with the identity it recorded.
	savedOut := joinOut
	var out bytes.Buffer
	joinOut = &out
	t.Cleanup(func() { joinOut = savedOut })
	stale := `{"memberName":"cp9","peerURL":"https://198.51.100.7:2380","memberID":77,"phase":"added"}`
	if err := os.WriteFile(joinIntentPath, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recordedJoin{listMembers: listedMembers()} // the cluster no longer lists the recorded member
	st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
	if err != nil {
		t.Fatalf("the re-run with a stale intent refused: %v", err)
	}
	if r.addCalls != 1 || !r.addCalled || r.addPeer != "https://192.0.2.9:2380" {
		t.Fatalf("the re-run ran %d member-adds (peer %q), want one fresh add of this node's peer URL",
			r.addCalls, r.addPeer)
	}
	// The stale intent was replaced before the add: at member-add time the
	// journal already records THIS run's member, not the removed one.
	pre := parseIntent(t, r.intentAtAddRaw)
	if pre.MemberName != r.addName || pre.PeerURL != r.addPeer || pre.MemberID != 0 || pre.Phase != "prepared" {
		t.Errorf("the journal at member-add time records %+v, want a fresh intent for %q at %q",
			pre, r.addName, r.addPeer)
	}
	if r.removeCalled || r.removeByIDCalls != 0 {
		t.Errorf("the join tried to remove the member the stale intent recorded: remove=%v byID=%d",
			r.removeCalled, r.removeByIDCalls)
	}
	for _, want := range []string{"stale", "cp9", "https://198.51.100.7:2380", "77", "added",
		"joining as a fresh member", joinIntentPath} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the join's output does not say the stale intent was replaced: missing %q in %q", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "recorded the join intent in "+joinIntentPath) {
		t.Errorf("the join does not say it recorded the fresh intent: %q", out.String())
	}
	if r.saved == nil || st == nil || st.EtcdState != "existing" {
		t.Fatalf("the fresh add saved no node state: saved=%v returned=%v", r.saved, st)
	}
	// The replacement journal outlives the save: it now records the fresh
	// member, and ends only when a later start confirms local etcd data.
	fresh, readErr := os.ReadFile(joinIntentPath)
	if readErr != nil {
		t.Fatalf("the journal did not survive a completed join: %v", readErr)
	}
	if done := parseIntent(t, fresh); done.MemberName != r.addName || done.PeerURL != r.addPeer {
		t.Errorf("the surviving journal records %+v, want the fresh member %q at %q",
			done, r.addName, r.addPeer)
	}
}

// TestMemberAddRefusedClassifiesEtcdsOutrightRefusals pins the split the
// commit phase reconciles on: a refusal etcd answered outright — the request
// was rejected before any proposal, so it cannot have committed and its reply
// was not lost — versus a possibly-lost reply, where the proposal may have
// landed and only the answer dropped. isPromoteNotYet classifies the same
// split for MemberPromote.
func TestMemberAddRefusedClassifiesEtcdsOutrightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		refused bool
	}{
		{"a peer URL that already exists", rpctypes.ErrPeerURLExist, true},
		{"a wrapped peer URL refusal", fmt.Errorf("member add via https://192.0.2.9:2380: %w", rpctypes.ErrPeerURLExist), true},
		{"the strict reconfiguration refusal after its retries", rpctypes.ErrUnhealthy, true},
		{"the strict reconfiguration refusal while an added member has not started", rpctypes.ErrMemberNotEnoughStarted, true},
		{"the learner-cap refusal", rpctypes.ErrTooManyLearners, true},
		{"a follower that does not propose", rpctypes.ErrNotLeader, true},
		{"a cluster with no leader", rpctypes.ErrNoLeader, true},
		{"a throttled request", rpctypes.ErrTooManyRequests, true},
		{"an auth-refused request", rpctypes.ErrPermissionDenied, true},
		{"nil is no refusal at all", nil, false},
		{"a deadline may still have committed", context.DeadlineExceeded, false},
		{"a request timeout may still have committed", rpctypes.ErrTimeout, false},
		{"an unapplied index may still have committed", rpctypes.ErrTimeoutWaitAppliedIndex, false},
		{"a leader change may still have committed", rpctypes.ErrLeaderChanged, false},
		{"a transport failure may still have committed", errors.New("connection refused"), false},
		{"a canceled call may still have committed", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := memberAddRefused(tc.err); got != tc.refused {
				t.Errorf("memberAddRefused(%v) = %v, want %v", tc.err, got, tc.refused)
			}
		})
	}
}

// TestControlPlaneJoinFailsADeterministicMemberAddRefusalDirectly drives a
// member-add etcd refused outright: the request was rejected before any
// proposal, so there is no lost reply to reconcile — the member list must not
// be read, the add must not be retried, and etcd's own answer is the join's
// error. The journal stays as the recovery record: for a peer-URL refusal it
// records the member a prior add committed, and the re-run's reconciliation
// — the one with the identity checks — adopts it.
func TestControlPlaneJoinFailsADeterministicMemberAddRefusalDirectly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		refusal  error
		wantErr  string
		wantHint bool
	}{
		{"a duplicate peer URL", rpctypes.ErrPeerURLExist, "Peer URLs already exists", false},
		{"the strict reconfiguration refusal", rpctypes.ErrUnhealthy, "unhealthy cluster", false},
		{"another controller's member not started yet", rpctypes.ErrMemberNotEnoughStarted, "not enough started members", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{failAdd: fmt.Errorf("member add via https://192.0.2.9:2380: %w", tc.refusal),
				listMembers: listedMembers()}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if err == nil {
				t.Fatal("the join finished although etcd refused the member-add outright")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the error does not surface etcd's own answer (%q): %v", tc.wantErr, err)
			}
			if got := strings.Contains(err.Error(), concurrentJoinHint); got != tc.wantHint {
				t.Errorf("concurrent-join hint present = %v, want %v: %v", got, tc.wantHint, err)
			}
			if strings.Contains(err.Error(), "reply was lost") {
				t.Errorf("a refusal etcd answered is no lost reply: %v", err)
			}
			if r.addCalls != 1 {
				t.Errorf("the member-add ran %d times, want exactly the refused one", r.addCalls)
			}
			if r.memberListReads != 0 {
				t.Errorf("the member list was read %d times to settle a request etcd refused; "+
					"the refusal itself settles it", r.memberListReads)
			}
			if r.removeCalled || r.removeByIDCalls != 0 || r.quorumHeldCalls != 0 {
				t.Errorf("a refusal that added nothing must not remove or compensate: remove=%v byID=%d quorum=%d",
					r.removeCalled, r.removeByIDCalls, r.quorumHeldCalls)
			}
			raw, readErr := os.ReadFile(joinIntentPath)
			if readErr != nil {
				t.Fatalf("the journal did not survive the refused add: %v", readErr)
			}
			kept := parseIntent(t, raw)
			if kept.MemberName != r.addName || kept.PeerURL != r.addPeer {
				t.Errorf("the kept journal records %+v, want the member the add ran for %q at %q",
					kept, r.addName, r.addPeer)
			}
			if kept.Phase != "prepared" {
				t.Errorf("kept journal phase = %q, want \"prepared\"", kept.Phase)
			}
		})
	}
}

// TestControlPlaneJoinReconcilesAnAmbiguousMemberAdd drives a member-add whose
// reply was lost: the deadline fired with the request possibly committed. The
// member list is re-read to decide, not guessed at: the member is in the
// cluster and the join finishes on it without a second add and without
// removing anything; it is not, and the add is retried once; the list cannot
// answer, and the join refuses — one add, nothing removed, the journal kept as
// the re-run's recovery record.
func TestControlPlaneJoinReconcilesAnAmbiguousMemberAdd(t *testing.T) {
	for _, tc := range []struct {
		name           string
		list           func(r *recordedJoin) (etcdMembership, error)
		wantErr        string // "" for none
		wantAdds       int
		wantRemoveByID int
		wantJournal    bool
	}{
		{"the member landed: the join finishes on it", func(*recordedJoin) (etcdMembership, error) {
			return listedMembers(), nil
		}, "", 1, 0, true},
		{"the member did not land: one retry", func(r *recordedJoin) (etcdMembership, error) {
			if r.memberListReads == 1 {
				return etcdMembership{ClusterID: 1, Quorum: true, Members: listedMembers().Members[:1]}, nil
			}
			return listedMembers(), nil
		}, "", 2, 0, true},
		{"the list cannot answer: refuse, do not guess", func(*recordedJoin) (etcdMembership, error) {
			return etcdMembership{}, errors.New("the member list cannot be read")
		}, "the member list cannot be read", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{failAdd: context.DeadlineExceeded}
			r.clusterMembersFn = func() (etcdMembership, error) { return tc.list(r) }
			st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want a refusal that reconciles the lost reply with %q", err, tc.wantErr)
				}
				if r.memberListReads == 0 {
					t.Error("the join gave up without re-reading the cluster's member list")
				}
			} else if err != nil {
				t.Fatalf("the join did not reconcile the lost reply: %v", err)
			}
			if r.addCalls != tc.wantAdds {
				t.Errorf("the join ran %d member-adds, want %d", r.addCalls, tc.wantAdds)
			}
			if r.addCalls > 0 && r.addPeer != "https://192.0.2.9:2380" {
				t.Errorf("the member-add used peer URL %q, want this node's https://192.0.2.9:2380", r.addPeer)
			}
			if r.removeCalled || r.removeByIDCalls != tc.wantRemoveByID {
				t.Errorf("the join removed a member on a guess: remove=%v byID=%d (want %d)",
					r.removeCalled, r.removeByIDCalls, tc.wantRemoveByID)
			}
			if _, statErr := os.Stat(joinIntentPath); (statErr == nil) != tc.wantJournal {
				t.Errorf("the journal left behind = %v, want %v", statErr == nil, tc.wantJournal)
			}
			if tc.wantErr == "" {
				if r.saved == nil || st == nil {
					t.Fatalf("the reconciled join saved no node state: saved=%v returned=%v", r.saved, st)
				}
				if !strings.Contains(r.saved.EtcdInitialCluster, "=https://192.0.2.9:2380") {
					t.Errorf("saved initial cluster %q does not name this node's member", r.saved.EtcdInitialCluster)
				}
				if r.quorumHeldCalls != 0 {
					t.Errorf("the join compensated although it finished: %d quorum checks", r.quorumHeldCalls)
				}
			}
		})
	}
}

// TestAmbiguousMemberAddRetryRefusedByAnUnstartedMemberSaysToJoinOneAtATime:
// the lost reply's one retry can meet another controller's member-add landing
// in between, which etcd's strict reconfiguration check refuses until that
// member starts. The refusal carries the concurrent-join hint; no other retry
// failure does.
func TestAmbiguousMemberAddRetryRefusedByAnUnstartedMemberSaysToJoinOneAtATime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retryErr error
		wantHint bool
	}{
		{"another controller's member not started yet", rpctypes.ErrMemberNotEnoughStarted, true},
		{"a transport failure", errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			r := &recordedJoin{failAdd: context.DeadlineExceeded, failRetry: tc.retryErr}
			r.clusterMembersFn = func() (etcdMembership, error) {
				return etcdMembership{ClusterID: 1, Quorum: true, Members: listedMembers().Members[:1]}, nil
			}
			_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
			if err == nil || !strings.Contains(err.Error(), "the one retry failed") {
				t.Fatalf("err = %v, want the failed retry's refusal", err)
			}
			if got := strings.Contains(err.Error(), concurrentJoinHint); got != tc.wantHint {
				t.Errorf("concurrent-join hint present = %v, want %v: %v", got, tc.wantHint, err)
			}
			if r.addCalls != 2 {
				t.Errorf("the join ran %d member-adds, want the lost one and its one retry", r.addCalls)
			}
		})
	}
}

// --- preparation and membership commit are separate phases ---
//
// prepareControlPlaneJoin runs in two phases: a preparation phase that runs
// every fallible effect which needs no member-add result — the runtime configs
// included, on a node state whose EtcdInitialCluster may be derived from the
// cluster's member list read before the add — and a membership-commit phase
// that takes the opaque value the preparation returned, runs the member-add,
// and then the writes that genuinely need the add's result: the node-state
// save, whose EtcdInitialCluster is the string the member-add returned or the
// adopted membership's, and the intent-clear, which ends the journal only
// once that state is persisted. The prepared value's fields are unexported, so
// a populated one cannot be constructed outside internal/ha — the pattern of
// clusterinfo.Authenticated's unexported field
// (internal/bootstrap/clusterinfo/clusterinfo.go:118-122); the zero value is
// inert, and the commit phase rejects it like TrustBundle rejects an
// unauthenticated Authenticated.

// PrepareControlPlaneJoin is pkg/cli's dependency. The shared handler threads
// the run's interrupt context through it: func(context.Context, JoinOptions)
// (*nodestate.NodeState, error) — an internal-package signature, so pkg/cli's
// exported API is untouched.
var _ func(context.Context, JoinOptions) (*nodestate.NodeState, error) = PrepareControlPlaneJoin

// TestJoinSplitsIntoPreparationAndMembershipCommit drives the two phases
// directly. The preparation phase runs every fallible effect that needs no
// member-add result and never touches membership; it returns the opaque
// prepared value the membership-commit phase consumes. The commit phase runs
// exactly the membership effects — the member-add, then the state save — and
// the intent journal still brackets the add: on disk before it, gone once the
// join has finished persisting its state.
func TestJoinSplitsIntoPreparationAndMembershipCommit(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	prepared, err := prepareJoinNode(context.Background(), freshJoinOpts(), t.TempDir(), fakeJoinEffects(r))
	if err != nil {
		t.Fatalf("preparation: %v", err)
	}
	if r.addCalled || r.removeCalled || r.saved != nil {
		t.Fatalf("preparation touched membership: add=%v remove=%v saved=%+v", r.addCalled, r.removeCalled, r.saved)
	}
	if r.rtState == nil || r.rtState.Role != "controller" {
		t.Errorf("the runtime-config write got %+v, want the node state built before membership", r.rtState)
	}
	prepareWant := []string{
		"stage", "host directories", "prerequisites", "discover", "bootstrap kubeconfig",
		"clear cilium settings", "download", "trust pod cidrs", "resolve node ip",
		"issue certificates", "write kubeconfigs", "probe etcd", "controller addresses", "vip check", "runtime configs",
	}
	if !slices.Equal(r.order, prepareWant) {
		t.Fatalf("preparation ran another order:\n got %v\nwant %v", r.order, prepareWant)
	}
	if reflect.ValueOf(prepared).IsZero() {
		t.Fatal("preparation returned the zero value")
	}
	// Opaque: every field unexported, so a composite literal naming any of them
	// does not compile outside internal/ha, and the preparation phase is the
	// only thing that returns a populated one.
	typ := reflect.TypeOf(prepared)
	if typ.NumField() == 0 {
		t.Fatal("the prepared value carries no fields between the phases")
	}
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.PkgPath == "" {
			t.Errorf("the prepared value's field %q is exported: a populated value could be constructed outside internal/ha", f.Name)
		}
	}
	// The zero value is inert: the commit phase refuses it rather than
	// reconciling, adding, or saving on a guess.
	if _, err := commitJoinMembership(context.Background(), preparedJoin{}); err == nil {
		t.Fatal("the membership-commit phase accepted the zero prepared value")
	}
	st, err := commitJoinMembership(context.Background(), prepared)
	if err != nil {
		t.Fatalf("membership commit: %v", err)
	}
	if !slices.Equal(r.order, append(slices.Clone(prepareWant), "member-add", "save state")) {
		t.Fatalf("the commit ran another order:\n got %v\nwant %v with member-add then save state",
			r.order, prepareWant)
	}
	if !r.intentAtAdd {
		t.Fatal("no intent file existed when the member-add ran")
	}
	if atAdd := parseIntent(t, r.intentAtAddRaw); atAdd.MemberName != r.addName || atAdd.PeerURL != r.addPeer {
		t.Errorf("the intent at the member-add names %q at %q, want the member the add ran for %q at %q",
			atAdd.MemberName, atAdd.PeerURL, r.addName, r.addPeer)
	}
	if r.saved == nil || st == nil || st.EtcdState != "existing" {
		t.Fatalf("the commit saved/returned no joined node state: saved=%v returned=%v", r.saved, st)
	}
	// The journal outlives the save: the crash window between the state save
	// and etcd's first WAL keeps its recovery record until a later start
	// confirms the local etcd data.
	if _, statErr := os.Stat(joinIntentPath); statErr != nil {
		t.Fatalf("the intent file did not survive a completed join: %v", statErr)
	}
}

// --- the shared node assembly ---
//
// Controller bootstrap (cluster.PrepareController) and the HA join run the
// same node steps — host directories, prerequisites, pod-CIDR trust, node-IP
// resolution, leaf certificates, kubeconfigs, the profile step, and the
// kubelet/containerd configs — through ONE assembly function, not two
// parallel bodies. The caller-specific steps hang off the assembly at the
// three points the two paths' pinned orders fix: ResolveNet between the
// prerequisites and the pod-CIDR trust (the join's shared-cert download and
// cluster gates; the controller's netconfig resolution), BeforeCerts between
// the resolved node IP and the certificates (the controller's IP-move
// refusal and node state), and BeforeRuntimeConfigs between the profile step and
// the runtime configs (the join's retained-data re-check, etcd probe, and
// intent reconciliation). The refusal invariants survive the shared
// assembly: every fallible step returns before the join's member-add, and
// the order the guards record holds.

// assemblyRecord records what the shared node assembly ran, in order, and
// the arguments the pinned steps received. failNet, failBeforeCerts, and
// failBeforeRuntime inject the refusals the caller-owned hooks produce.
type assemblyRecord struct {
	order []string

	prereqsCIS    bool
	trustedCIDR   string
	detectDual    bool
	resolveFlag   string
	resolveConfig string
	resolveSaved  string

	beforeCertsChoice host.NodeIPChoice
	certHost          string
	certNodeIP        string
	certSANs          *pki.APISANs
	kcHost            string
	kcServer          string
	kcVIP             bool
	rtState           *nodestate.NodeState

	failNet           error
	failBeforeCerts   error
	failBeforeRuntime error
}

// assemblyNetc is the netconfig the recorded ResolveNet answers with: the
// built-in v4 CIDRs under a test domain, so the SAN and CIDR assertions have
// fixed expected values.
func assemblyNetc() netcfg.Config {
	netc, err := netcfg.Parse("10.245.0.0/16", "10.96.0.0/12")
	if err != nil {
		panic(err) // literal CIDRs, always parse
	}
	netc.ClusterDomain = "fuzz.internal"
	return netc
}

// recordingAssembly is a NodeAssembly that records each call instead of
// touching the host, plus the request a caller hands AssembleNode: an IPv4
// node with a VIP, every explicit input distinct so a mix-up is visible.
func recordingAssembly(r *assemblyRecord) (NodeAssembly, AssembleRequest) {
	netc := assemblyNetc()
	a := NodeAssembly{
		HostDirs: func() error { r.order = append(r.order, "host directories"); return nil },
		Prereqs: func(o host.PrereqOptions) error {
			r.order = append(r.order, "prerequisites")
			r.prereqsCIS = o.Profile.Name == nodestate.CISProfile
			return nil
		},
		ResolveNet: func() (netcfg.Config, error) {
			r.order = append(r.order, "resolve net")
			if r.failNet != nil {
				return netcfg.Config{}, r.failNet
			}
			return netc, nil
		},
		TrustPodCIDRs: func(podCIDR string) { r.order = append(r.order, "trust pod cidrs"); r.trustedCIDR = podCIDR },
		ResolveNodeIP: func(flag, config, state string, detect func() (string, error)) (host.NodeIPChoice, error) {
			r.order = append(r.order, "resolve node ip")
			r.resolveFlag, r.resolveConfig, r.resolveSaved = flag, config, state
			ip, err := detect()
			if err != nil {
				return host.NodeIPChoice{}, err
			}
			return host.NodeIPChoice{IP: ip, Source: "detect"}, nil
		},
		DetectNodeIPs: func(dualStack bool) (string, error) { r.detectDual = dualStack; return "192.0.2.9", nil },
		BeforeCerts: func(choice host.NodeIPChoice) error {
			r.order = append(r.order, "before certificates")
			r.beforeCertsChoice = choice
			return r.failBeforeCerts
		},
		IssueCerts: func(host, nodeIP string, sans *pki.APISANs) error {
			r.order = append(r.order, "issue certificates")
			r.certHost, r.certNodeIP, r.certSANs = host, nodeIP, sans
			return nil
		},
		WriteKubeconfigs: func(host, server string, vip bool) error {
			r.order = append(r.order, "write kubeconfigs")
			r.kcHost, r.kcServer, r.kcVIP = host, server, vip
			return nil
		},
		WriteProfileConfigs:  func() error { r.order = append(r.order, "profile configs"); return nil },
		BeforeRuntimeConfigs: func() error { r.order = append(r.order, "before runtime configs"); return r.failBeforeRuntime },
		WriteRuntimeConfigs: func(st *nodestate.NodeState) error {
			r.order = append(r.order, "runtime configs")
			r.rtState = st
			return nil
		},
	}
	req := AssembleRequest{
		Hostname:     "cp9",
		NodeIPFlag:   "192.0.2.7",
		ConfigNodeIP: "192.0.2.8",
		SavedIP:      "192.0.2.9",
		VIPAddress:   "10.0.0.10",
		APIServer:    "https://10.0.0.10:6443",
	}
	return a, req
}

// TestNodeAssemblyRunsTheCommonStepsInThePinnedOrder drives the shared
// assembly directly and pins its whole step order: host directories,
// prerequisites, the caller's network resolution, trust pod CIDRs, resolve
// node IP, the caller's before-certificates hook, issue certificates, write
// kubeconfigs, the profile step (a node with a profile only), the caller's
// before-runtime-configs hook, runtime configs. The caller's inputs reach
// the steps that consume them — the IP precedence gets the flag, config, and
// saved address, the certificates get the netconfig's SANs and domain, the
// kubeconfigs get the caller's apiserver endpoint and VIP — and the caller's
// node state is what the runtime configs render from.
func TestNodeAssemblyRunsTheCommonStepsInThePinnedOrder(t *testing.T) {
	netc := assemblyNetc()
	for _, tc := range []struct {
		name string
		cis  bool
	}{
		{"without cis", false},
		{"with cis", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &assemblyRecord{}
			a, req := recordingAssembly(r)
			if tc.cis {
				req.PrereqOptions.Profile = profiletest.Profile()
			}
			req.State = &nodestate.NodeState{Role: "controller"}
			choice, err := AssembleNode(a, req)
			if err != nil {
				t.Fatalf("node assembly: %v", err)
			}
			want := []string{"host directories", "prerequisites", "resolve net", "trust pod cidrs",
				"resolve node ip", "before certificates", "issue certificates", "write kubeconfigs"}
			if tc.cis {
				want = append(want, "profile configs")
			}
			want = append(want, "before runtime configs", "runtime configs")
			if !slices.Equal(r.order, want) {
				t.Fatalf("the assembly ran another order:\n got %v\nwant %v", r.order, want)
			}
			if r.prereqsCIS != tc.cis {
				t.Errorf("prerequisites ran for CIS %v, want %v", r.prereqsCIS, tc.cis)
			}
			if r.trustedCIDR != netc.PodCIDR {
				t.Errorf("trusted pod CIDR %q, want the caller's netconfig %q", r.trustedCIDR, netc.PodCIDR)
			}
			if r.resolveFlag != req.NodeIPFlag || r.resolveConfig != req.ConfigNodeIP || r.resolveSaved != req.SavedIP {
				t.Errorf("the IP precedence got flag %q, config %q, saved %q, want the request's %q, %q, %q",
					r.resolveFlag, r.resolveConfig, r.resolveSaved, req.NodeIPFlag, req.ConfigNodeIP, req.SavedIP)
			}
			if r.detectDual {
				t.Error("address detection ran dual-stack for a v4 netconfig")
			}
			if want := (host.NodeIPChoice{IP: "192.0.2.9", Source: "detect"}); r.beforeCertsChoice != want {
				t.Errorf("the before-certificates hook got %+v, want the resolved choice %+v", r.beforeCertsChoice, want)
			}
			if choice.IP != "192.0.2.9" || choice.Source != "detect" {
				t.Errorf("the assembly returned %+v, want the resolved choice", choice)
			}
			if r.certHost != req.Hostname || r.certNodeIP != "192.0.2.9" {
				t.Errorf("the certificate effect got host %q node IP %q, want %q and the resolved 192.0.2.9",
					r.certHost, r.certNodeIP, req.Hostname)
			}
			if wantSANs := netc.APIServerSANs(req.VIPAddress); r.certSANs == nil ||
				!slices.Equal(r.certSANs.IPs, wantSANs) || r.certSANs.Domain != netc.ClusterDomain {
				t.Errorf("certificate SANs = %+v, want the netconfig's %q with domain %q",
					r.certSANs, wantSANs, netc.ClusterDomain)
			}
			if r.kcHost != req.Hostname || r.kcServer != req.APIServer || !r.kcVIP {
				t.Errorf("the kubeconfig effect got host %q server %q vip %v, want %q, %q, true",
					r.kcHost, r.kcServer, r.kcVIP, req.Hostname, req.APIServer)
			}
			if r.rtState != req.State {
				t.Errorf("the runtime configs rendered %+v, want the caller's node state", r.rtState)
			}
		})
	}
}

// TestNodeAssemblyRefusesBeforeTheStepsThatFollow drives each caller-owned
// hook into a refusal and requires the assembly to stop there: a hook that
// refuses must return before the steps after it run, the same
// refusal-before-the-next-side-effect property the join's member-add pin
// holds. An error a hook returns is the error the assembly reports.
func TestNodeAssemblyRefusesBeforeTheStepsThatFollow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    func(r *assemblyRecord)
		wantErr string
		notRun  string
	}{
		{"the caller's network resolution", func(r *assemblyRecord) { r.failNet = errors.New("the join token is stale") },
			"the join token is stale", "trust pod cidrs"},
		{"the before-certificates hook", func(r *assemblyRecord) { r.failBeforeCerts = errors.New("node IP moved") },
			"node IP moved", "issue certificates"},
		{"the before-runtime-configs hook", func(r *assemblyRecord) { r.failBeforeRuntime = errors.New("etcd unreachable") },
			"etcd unreachable", "runtime configs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &assemblyRecord{}
			tc.fail(r)
			a, req := recordingAssembly(r)
			req.PrereqOptions.Profile = profiletest.Profile()
			req.State = &nodestate.NodeState{}
			if _, err := AssembleNode(a, req); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want a refusal carrying %q", err, tc.wantErr)
			}
			if slices.Contains(r.order, tc.notRun) {
				t.Errorf("%s ran although %s refused", tc.notRun, tc.name)
			}
		})
	}
}

// TestPrepareJoinRunsItsCommonStepsThroughTheSharedAssembly pins the join
// side of the shared assembly: prepareControlPlaneJoin routes its common
// node steps through the shared assembly exactly once, handing it the
// request its own options and state resolve to — the CIS profile, the
// join's VIP, the endpoint bring-up's rule yields for that VIP (what the
// join's kubeconfigs target, not loopback), no saved IP (a fresh join has
// none), the join's flag and config node-IP inputs — and the node state the
// runtime configs render from is the assembled one, carrying the resolved
// node IP.
func TestPrepareJoinRunsItsCommonStepsThroughTheSharedAssembly(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{carriedKey: true, listMembers: listedMembers()}
	opts := freshJoinOpts()
	opts.CIS, opts.Profile = true, nodestate.CISProfile
	st, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r))
	if err != nil {
		t.Fatalf("cis join: %v", err)
	}
	if r.assembleCalls != 1 {
		t.Fatalf("the join ran the shared assembly %d times, want once", r.assembleCalls)
	}
	req := r.assembleReq
	if req == nil {
		t.Fatal("the join called the shared assembly with no request")
	}
	if req.PrereqOptions.Profile.Name != nodestate.CISProfile {
		t.Errorf("the assembly ran with profile %q, want the join's cis", req.PrereqOptions.Profile.Name)
	}
	if req.VIPAddress != "10.0.0.10" {
		t.Errorf("the assembly's VIP was %q, want the join's 10.0.0.10", req.VIPAddress)
	}
	if req.APIServer != fmt.Sprintf("https://%s:%d", opts.VIP.Address, version.APIServerPort) {
		t.Errorf("the assembly's apiserver endpoint was %q, want bring-up's endpoint rule for the join's VIP %q (the kubeconfigs must not target loopback on a VIP node)",
			req.APIServer, fmt.Sprintf("https://%s:%d", opts.VIP.Address, version.APIServerPort))
	}
	if req.SavedIP != "" || req.NodeIPFlag != "" || req.ConfigNodeIP != "" {
		t.Errorf("the assembly got saved IP %q, flag %q, config %q; a fresh join carries none of them",
			req.SavedIP, req.NodeIPFlag, req.ConfigNodeIP)
	}
	if req.Hostname != r.addName {
		t.Errorf("the assembly's hostname was %q, want the join's member name %q", req.Hostname, r.addName)
	}
	if req.State == nil || req.State != r.rtState {
		t.Fatalf("the runtime configs rendered %+v, want the assembled node state", r.rtState)
	}
	if r.rtState.NodeIP != "192.0.2.9" || r.rtState.KubeletNodeIP != "192.0.2.9" {
		t.Errorf("the assembled node state carries node IP %q (kubelet %q), want the resolved 192.0.2.9",
			r.rtState.NodeIP, r.rtState.KubeletNodeIP)
	}
	if st == nil || st.EtcdState != "existing" {
		t.Errorf("the join through the assembly returned %+v, want a joined node state", st)
	}
}

// From here down: the signal handling tests for the join's safe boundaries.
// Every join step must observe the context: a signal that goes unobserved
// leaves the process to die by the default action, potentially with the
// voting member already added and no node state saved.

// TestJoinRefusesToStartWhenTheSignalArrivedBeforeIt is the entry-boundary
// red: a join entered with an already-received signal refuses at its first
// boundary — before staging, certificates, or the live member-add — surfacing
// the signal's own stop (wrapping context.Canceled), the same contract
// internal/certs' runInitiatorPhase gives a --cluster run
// (internal/certs/cacluster.go:388-397). The live-context row is the guard
// that keeps the refusal a signal check, not a blanket one: with no signal
// received the join still starts its first real step.
func TestJoinRefusesToStartWhenTheSignalArrivedBeforeIt(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.BootstrapKubeconfig, filepath.Join(tmp, "bootstrap.conf")},
		{&version.PKIDir, filepath.Join(tmp, "pki")},
		{&version.EncryptionConfigPath, filepath.Join(tmp, "encryption-config.yaml")},
		{&version.NetConfigPath, filepath.Join(tmp, "netconfig")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	opts := JoinOptions{Server: "https://10.0.0.11:6443", Token: "token", CAHash: "sha256:aa", CertKey: "key"}

	t.Run("the signal arrived before the join started", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		etcd := filepath.Join(t.TempDir(), "etcd")
		_, err := prepareControlPlaneJoin(ctx, opts, etcd, productionJoinEffects())
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("join entered after a signal = %v; want the signal's stop (wrapping context.Canceled)", err)
		}
		if _, err := os.Stat(etcd); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused join created the etcd data dir %s: nothing may be touched before the add", etcd)
		}
		if _, err := os.Stat(version.BootstrapKubeconfig); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused join wrote %s: no trust material may be written after a received signal", version.BootstrapKubeconfig)
		}
	})

	t.Run("a live context still starts the join's first real step", func(t *testing.T) {
		etcd := filepath.Join(t.TempDir(), "etcd")
		_, err := prepareControlPlaneJoin(context.Background(), opts, etcd, productionJoinEffects())
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("a live context was mistaken for a stop: %v", err)
		}
		// The test binary carries no payload, so staging is where a live join
		// lands — proof the refusal above fires only once the context is dead.
		if !strings.Contains(err.Error(), "staging payload") {
			t.Fatalf("a live join did not reach its first real step: %v", err)
		}
	})
}

// observesContext reports whether the given source window consults the
// context: an inline ctx.Err()/ctx.Done() check, or any call passing it —
// the two shapes this repo uses (internal/certs/cacluster.go:389 for the
// first, runInitiatorPhase at internal/certs/cacluster.go:401 for the
// second).
func observesContext(window []byte) bool {
	for _, shape := range []string{"ctx.Err(", "ctx.Done(", "(ctx"} {
		if bytes.Contains(window, []byte(shape)) {
			return true
		}
	}
	return false
}

// TestJoinObservesTheContextBetweenTheDownloadAndTheMemberAdd pins the
// boundary a MID-FLIGHT signal reaches. A check at entry alone is not enough:
// preparation continues after the join starts (cert download, PKI
// generation), and the live member-add is the one step a received signal must
// never let happen — a check placed before the download step would already
// have passed by the time a signal lands in the download itself. The pin is
// source order, the house pattern for wiring an in-process test cannot drive
// (TestJoinRefusesAKeylessBundleBeforeTheMemberAdd above does the same for
// the encryption-key gate): some consult of the context must sit between the
// shared-cert download step and the member-add call.
func TestJoinObservesTheContextBetweenTheDownloadAndTheMemberAdd(t *testing.T) {
	src, err := os.ReadFile("join.go")
	if err != nil {
		t.Fatal(err)
	}
	download := bytes.Index(src, []byte(`k8sutil.Step(os.Stdout, "Discover cluster`))
	if download < 0 {
		t.Fatal("the shared-cert download step is gone from the join — re-express this pin")
	}
	add := bytes.LastIndex(src, []byte("addMemberIfEtcdDataDirEmpty("))
	if add < 0 {
		t.Fatal("the member-add call is gone from the join — re-express this pin")
	}
	if add <= download {
		t.Fatal("the member-add call does not follow the download step — re-express this pin")
	}
	if !observesContext(src[download:add]) {
		t.Fatal("nothing between the download step and the member-add observes the context: a signal received during preparation would still perform the live member-add")
	}
}

// TestJoinDefersTheFirstSignalAcrossTheMemberAddToStateSave pins the commit
// window policy (the span from the live member-add to the persisted node
// state): from
// the live member-add to the persisted node state a kill orphans a voting
// member, so the first signal received inside the window is DEFERRED — the
// window consults nothing and runs to the end — and once the state is saved
// the deferred signal stops the join, with a log line that says so. The
// second signal needs no pin of its own: the window runs on the interrupt
// context, whose handler restores the signals' default action before the
// cancellation is visible (internal/interrupt/interrupt.go:93-95), so a
// second signal kills exactly as it does everywhere else.
func TestJoinDefersTheFirstSignalAcrossTheMemberAddToStateSave(t *testing.T) {
	src, err := os.ReadFile("join.go")
	if err != nil {
		t.Fatal(err)
	}
	// The commit window lives in commitJoinMembership since the two-phase
	// split: the member-add runs there (addMemberIfEtcdDataDirEmpty), and the
	// state save is the joinEffects saveState seam. The assembly's runtime
	// configs moved into preparation, so the tail the deferred signal
	// surfaces into is the span from the save to the commit phase's return.
	add := bytes.LastIndex(src, []byte("addMemberIfEtcdDataDirEmpty("))
	if add < 0 {
		t.Fatal("the member-add call is gone from the join — re-express this pin")
	}
	save := bytes.Index(src[add:], []byte("p.fx.saveState(p.st)"))
	if save < 0 {
		t.Fatal("the node-state save is gone from the join — re-express this pin")
	}
	save += add
	ret := bytes.Index(src[save:], []byte("return p.st, nil"))
	if ret < 0 {
		t.Fatal("the commit phase's return is gone from the join — re-express this pin")
	}

	if observesContext(src[add:save]) {
		t.Fatal("the member-add-to-save window consults the context: a signal received inside it would stop the join mid-window and orphan the voting member the add just created")
	}
	tail := src[save : save+ret]
	if !observesContext(tail) {
		t.Fatal("a signal deferred across the window is never observed after the state is saved: the join would finish without reporting the stop")
	}
	for _, logShape := range []string{"log.Printf(", "log.Println(", "fmt.Printf(", "fmt.Println("} {
		if bytes.Contains(tail, []byte(logShape)) {
			return // the deferred stop is announced, not silent
		}
	}
	t.Fatal("the deferral policy has no log line after the window: an operator must see why the join finished the window past their signal")
}

func TestNodeAssemblyWritesTheConfigFilesOfAnyProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile profile.Profile
		writes  bool
	}{
		{"no profile", profile.Profile{}, false},
		{"a profile other than cis", profile.Profile{Name: "edge"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &assemblyRecord{}
			a, req := recordingAssembly(r)
			req.PrereqOptions.Profile = tc.profile
			req.State = &nodestate.NodeState{Role: "controller"}
			if _, err := AssembleNode(a, req); err != nil {
				t.Fatalf("node assembly: %v", err)
			}
			if got := slices.Contains(r.order, "profile configs"); got != tc.writes {
				t.Errorf("profile config step ran = %v, want %v (order %v)", got, tc.writes, r.order)
			}
		})
	}
}

func TestControlPlaneJoinTargetsTheControllerStartsServer(t *testing.T) {
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
			r := &recordedJoin{}
			opts := freshJoinOpts()
			opts.Server = "https://10.0.0.11:6443"
			opts.VIP = tc.vip
			st, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fakeJoinEffects(r))
			if err != nil {
				t.Fatalf("join via %s: %v", opts.Server, err)
			}
			if !r.kcCalled || r.kcServer != tc.want {
				t.Errorf("kubeconfigs written against %q (called %v), want the controller start's %q",
					r.kcServer, r.kcCalled, tc.want)
			}
			// kube-vip's own kubeconfig is written, against the local apiserver,
			// exactly when the node has a VIP to elect for.
			if r.kcVIP != (tc.vip.Address != "") {
				t.Errorf("kubeconfig write asked for kube-vip's kubeconfig = %v, want %v", r.kcVIP, tc.vip.Address != "")
			}
			if r.saved == nil {
				t.Fatal("no node state was saved")
			}
			if r.saved.Server != tc.want {
				t.Errorf("saved node state records server %q, want the controller start's %q, not the --join URL %q",
					r.saved.Server, tc.want, opts.Server)
			}
			if st == nil || st.Server != tc.want {
				t.Errorf("the join returned node state %+v, want server %q", st, tc.want)
			}
		})
	}
}

// --- the maintenance fence around the member-add ---
//
// The join's live member-add is member admission, and a member admitted while
// an upgrade, `rotate-ca --cluster` or `migrate-etcd-ca` run is mid-flight
// races the very bundle and staged-key writes those runs own. So the join
// holds the same cluster-wide maintenance fence those orchestrators hold —
// maintlease.AcquireCompatible, both the stratum-cluster-maintenance and the
// legacy stratum-cluster-upgrade lease — from just before the member-add to
// the persisted node state (the join's member-add window), through the
// takeMaintenanceFence effect. The take sits at the command-level seam: the
// join command wires the real fence, and the internal/ha helpers the
// orchestrators call mid-run (UploadCerts, NewCertKey, the downloads) never
// consult it, or the orchestrators would self-refuse inside their own run.
// The joiner's only working cluster credential in this window is its
// bootstrap token, so the fence it takes can be held and renewed only if the
// mint that published its grant also granted the control-plane bootstrap
// group the Lease access pinned by
// TestUploadCertsGrantsTheJoinerTheMaintenanceLeases below.

// TestControlPlaneJoinTakesTheMaintenanceFenceAroundTheMemberAdd pins the
// fence's place in the join. The take happens after every refusing step and
// before the member-add — the latest point that still prevents the add — and
// the release happens after the node state is saved, when the window is over.
// A fence held by another run refuses the join before the add, so nothing is
// added, saved, or compensated, and the intent journal stays as the recovery
// record; the refusal names the holder, because the operator must be able to
// tell whether to wait.
func TestControlPlaneJoinTakesTheMaintenanceFenceAroundTheMemberAdd(t *testing.T) {
	t.Run("taken before the member-add, released after the state save", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
			r.order = append(r.order, "maintenance fence")
			return &k8sutil.MaintenanceLease{Release: func() { r.order = append(r.order, "maintenance fence released") }}, nil
		}
		st, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx)
		if err != nil {
			t.Fatalf("fresh join: %v", err)
		}
		want := []string{
			"stage", "host directories", "prerequisites", "discover", "bootstrap kubeconfig",
			"clear cilium settings", "download", "trust pod cidrs", "resolve node ip",
			"issue certificates", "write kubeconfigs", "probe etcd", "controller addresses", "vip check", "runtime configs",
			"maintenance fence", "member-add", "save state", "maintenance fence released",
		}
		if !slices.Equal(r.order, want) {
			t.Fatalf("the join ran another order:\n got %v\nwant %v", r.order, want)
		}
		if r.saved == nil || st == nil {
			t.Fatalf("the fenced join saved/returned no node state: saved=%v returned=%v", r.saved, st)
		}
	})

	t.Run("a held fence refuses the join before the member-add", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{}
		fx := fakeJoinEffects(r)
		// maintlease's own refusal wording: the holder and its renewal age.
		held := errors.New("another maintenance run is in progress (cp1, renewed 5s ago)")
		fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) { return nil, held }
		_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx)
		if !errors.Is(err, held) {
			t.Fatalf("err = %v, want the fence refusal itself, with the holder's identity", err)
		}
		if r.addCalled || r.removeCalled || r.saved != nil {
			t.Fatalf("the fenced join touched membership: add=%v remove=%v saved=%+v", r.addCalled, r.removeCalled, r.saved)
		}
		if r.removeByIDCalls != 0 || r.quorumHeldCalls != 0 {
			t.Errorf("the post-add compensation ran although nothing was added: byID=%d quorum=%d",
				r.removeByIDCalls, r.quorumHeldCalls)
		}
		// The journal was written during preparation, before the fence could
		// refuse the add: it stays as this node's recovery record, and a re-run
		// of the join reconciles it against the cluster's own member list.
		if _, statErr := os.Stat(joinIntentPath); statErr != nil {
			t.Fatalf("the intent journal did not survive the refused join: %v", statErr)
		}
	})

	t.Run("released even when the state save fails after the member-add", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers(), failSaveState: errors.New("disk full")}
		fx := fakeJoinEffects(r)
		released := false
		fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
			return &k8sutil.MaintenanceLease{Release: func() { released = true }}, nil
		}
		if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx); err == nil {
			t.Fatal("the join succeeded although its state save failed")
		}
		if !released {
			t.Error("the fence outlived the failed join: it would refuse every maintenance run for the lease's whole 60s life")
		}
	})
}

// TestControlPlaneJoinRefusesAFenceItsTokenCannotTake: a join command minted
// by a stratum that predates the member-add fence grant carries a bootstrap
// token the stratum:certs-reader Role gives no Lease access, so the fence take
// is forbidden — after the preparation has issued certificates, written the
// kubeconfigs and recorded the intent journal. The join refuses before the
// member-add, saying why and how to get a command that works: a mint on a
// control plane running this release repairs the Role. Re-running with that
// new command then finishes the join past the journal the refused run left:
// the cluster's member list shows no member for it, so the re-run downloads
// with the new token, replaces the journal, takes the fence with that token,
// and adds the member exactly once.
func TestControlPlaneJoinRefusesAFenceItsTokenCannotTake(t *testing.T) {
	joinFixture(t)
	etcdDataDir := t.TempDir()
	r := &recordedJoin{}
	// The refused run added nothing, so the cluster lists only cp1 until a
	// member-add lands.
	r.clusterMembersFn = func() (etcdMembership, error) {
		if r.addCalls == 0 {
			return etcdMembership{ClusterID: 1, Responder: 11, Quorum: true, Members: listedMembers().Members[:1]}, nil
		}
		return listedMembers(), nil
	}
	fx := fakeJoinEffects(r)
	var bootstrapTokens, certKeys []string
	fx.writeBootstrapKubeconfig = func(server, token string, ca []byte) error {
		r.order = append(r.order, "bootstrap kubeconfig")
		bootstrapTokens = append(bootstrapTokens, token)
		return nil
	}
	download := fx.downloadGrant
	fx.downloadGrant = func(bootstrapKubeconfig, certKey string, check func(bundleProfile) error) (bundleGrant, error) {
		certKeys = append(certKeys, certKey)
		return download(bootstrapKubeconfig, certKey, check)
	}
	// maintlease's wording around the apiserver's refusal of the lease read.
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
		"stratum-cluster-upgrade", errors.New(`User "system:bootstrap:abc123" cannot get resource "leases" in API group "coordination.k8s.io" in the namespace "kube-system"`))
	fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
		return nil, fmt.Errorf("read the lease kube-system/stratum-cluster-upgrade: %w", forbidden)
	}
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDataDir, fx)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want the forbidden fence take, still wrapped", err)
	}
	for _, want := range []string{
		"predates the member-add fence grant",
		"stratum token create --control-plane",
		"control plane running this release",
		"nothing was added to etcd",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if r.addCalls != 0 || r.saved != nil {
		t.Fatalf("the refused join touched membership: adds=%d saved=%+v", r.addCalls, r.saved)
	}
	if _, statErr := os.Stat(joinIntentPath); statErr != nil {
		t.Fatalf("the intent journal did not survive the refused join: %v", statErr)
	}

	// The re-run, with the command a control plane running this release
	// minted: a new token and certificate-key for the same cluster.
	opts := freshJoinOpts()
	opts.Token, opts.CertKey = "new-bootstrap-token", "new-certificate-key"
	tokenAtFence := ""
	fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
		if len(bootstrapTokens) > 0 {
			tokenAtFence = bootstrapTokens[len(bootstrapTokens)-1]
		}
		return &k8sutil.MaintenanceLease{Release: func() {}}, nil
	}
	st, err := prepareControlPlaneJoin(context.Background(), opts, etcdDataDir, fx)
	if err != nil {
		t.Fatalf("the re-run with a new join command: %v", err)
	}
	if r.addCalls != 1 {
		t.Errorf("member-adds across both runs = %d, want exactly one", r.addCalls)
	}
	if tokenAtFence != opts.Token {
		t.Errorf("the fence was taken with the bootstrap kubeconfig for %q, want the new command's token %q", tokenAtFence, opts.Token)
	}
	if len(certKeys) == 0 || certKeys[len(certKeys)-1] != opts.CertKey {
		t.Errorf("the re-run's download used certificate-keys %q, want the new command's %q last", certKeys, opts.CertKey)
	}
	if r.saved == nil || st == nil {
		t.Fatalf("the re-run saved/returned no node state: saved=%v returned=%v", r.saved, st)
	}
}

// TestUploadCertsGrantsTheJoinerTheMaintenanceLeases checks requests as
// Kubernetes authorizes them: collection create has no resource name.
func TestUploadCertsGrantsTheJoinerTheMaintenanceLeases(t *testing.T) {
	cs := uploadFixture(t)
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatal(err)
	}
	role, err := cs.RbacV1().Roles("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !leaseRequestAllowed(role, "create", "") {
		t.Error("bootstrap token cannot create absent maintenance leases: collection create has no resource name")
	}
	for _, name := range []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"} {
		for _, verb := range []string{"get", "update", "delete"} {
			if !leaseRequestAllowed(role, verb, name) {
				t.Errorf("bootstrap token cannot %s maintenance lease %s", verb, name)
			}
		}
	}
	for _, verb := range []string{"get", "update", "delete", "list", "watch", "patch"} {
		if leaseRequestAllowed(role, verb, "unrelated-lease") {
			t.Errorf("bootstrap token may %s an unrelated lease", verb)
		}
	}
}

// --- authenticated bundle metadata ---
//
// The versioned envelope (see certs_upload_test.go) puts the bundle facts a
// join consumes inside the ciphertext, and the download hands them over as
// one bundleGrant: the etcd client endpoints the envelope authenticates
// (moved out of the unauthenticated Secret annotation, which stays published
// only for joiners running a released stratum), the cluster VIP the
// publishing control plane recorded, the publication's generation, and
// whether the bundle carried the at-rest key.
//
// The contract these tests pin:
//
//   - DownloadCerts returns the bundleGrant the envelope's authenticated
//     metadata makes of the download, and the downloadGrant effect, wired to
//     DownloadCerts in productionJoinEffects, is the join's one download
//     effect: the recorders and the recovery sentinels override it, so they
//     pin the path production runs.
//
//   - The join takes the cluster VIP from the grant's authenticated metadata,
//     never from an annotation: a configured VIP that conflicts with it
//     refuses the join before any certificate is issued or etcd is touched,
//     naming both addresses; an unspecified VIP inherits the metadata's, into
//     the node state and the kubeconfig endpoint rule alike; a grant with no
//     metadata (a legacy bundle, or a cluster with no VIP) keeps the
//     configured VIP and the no-VIP shapes this package already pins.

// vipGrant is the downloadGrant effect the tests stand in: the grant the
// envelope's authenticated metadata carried for this join, at a fixed
// generation so the tests can tell metadata absence from metadata zero.
func vipGrant(published []string, vip string) func(string, string, func(bundleProfile) error) (bundleGrant, error) {
	return func(string, string, func(bundleProfile) error) (bundleGrant, error) {
		return bundleGrant{Published: published, VIP: vip, Generation: 7}, nil
	}
}

// TestControlPlaneJoinTakesTheAuthenticatedVIPMetadata pins the join side of
// the versioned envelope: the VIP is a fact the cluster publishes about
// itself, authenticated by the same ciphertext that carries the certificates
// rather than taken unchecked from a joining control plane's own config, and
// the etcd endpoints the join dials come from the grant the metadata
// authenticates.
func TestControlPlaneJoinTakesTheAuthenticatedVIPMetadata(t *testing.T) {
	t.Run("an unspecified VIP inherits the metadata's", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "10.0.0.10")
		opts := freshJoinOpts()
		opts.VIP = nodestate.VIP{} // the joiner's config sets no VIP
		st, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if st.VIP.Address != "10.0.0.10" || r.saved.VIP.Address != "10.0.0.10" {
			t.Errorf("the joined node state records VIP %q (returned %q), want the metadata's 10.0.0.10",
				r.saved.VIP.Address, st.VIP.Address)
		}
		want := fmt.Sprintf("https://%s:%d", "10.0.0.10", version.APIServerPort)
		if r.kcServer != want || r.saved.Server != want {
			t.Errorf("the kubeconfigs target %q and the state records %q, want the endpoint rule over the inherited VIP %q",
				r.kcServer, r.saved.Server, want)
		}
		if r.assembleReq == nil || r.assembleReq.VIPAddress != "10.0.0.10" {
			t.Errorf("the shared assembly got %+v, want the inherited VIP 10.0.0.10", r.assembleReq)
		}
	})

	t.Run("a configured VIP matching the metadata proceeds", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "10.0.0.10")
		opts := freshJoinOpts() // VIP 10.0.0.10, the metadata's
		st, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if r.saved == nil || r.saved.VIP.Address != "10.0.0.10" {
			t.Errorf("the joined node state records VIP %q, want the agreed 10.0.0.10", r.saved.VIP.Address)
		}
		if st != nil && st.EtcdState != "existing" {
			t.Errorf("the agreeing join returned %+v, want a joined node state", st)
		}
	})

	t.Run("a configured VIP conflicting with the metadata is refused", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "10.0.0.10")
		opts := freshJoinOpts()
		opts.VIP = nodestate.VIP{Address: "10.0.0.99"} // the operator's copy disagrees with the cluster
		_, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx)
		if err == nil {
			t.Fatal("the join accepted a configured VIP that conflicts with the cluster's authenticated metadata")
		}
		for _, want := range []string{"10.0.0.99", "10.0.0.10"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name both addresses (missing %q): %v", want, err)
			}
		}
		if r.certCalled || r.kcCalled {
			t.Errorf("certificates (%v) or kubeconfigs (%v) were issued before the VIP refusal", r.certCalled, r.kcCalled)
		}
		if r.addCalled || r.removeCalled || r.saved != nil {
			t.Errorf("the refused join touched membership or state: add=%v remove=%v saved=%+v", r.addCalled, r.removeCalled, r.saved)
		}
		if r.removeByIDCalls != 0 || r.quorumHeldCalls != 0 {
			t.Errorf("the post-add compensation ran although the join refused before the member-add: byID=%d quorum=%d",
				r.removeByIDCalls, r.quorumHeldCalls)
		}
	})

	t.Run("a grant with no metadata keeps the configured VIP", func(t *testing.T) {
		// The pre-envelope shape: the cluster published no VIP metadata, so
		// the operator's configured VIP stands.
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "")
		opts := freshJoinOpts()
		if _, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx); err != nil {
			t.Fatalf("join: %v", err)
		}
		if r.saved == nil || r.saved.VIP.Address != "10.0.0.10" {
			t.Errorf("the joined node state records VIP %q, want the configured 10.0.0.10", r.saved.VIP.Address)
		}
	})

	t.Run("no metadata and no configured VIP is the no-VIP shape", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "")
		opts := freshJoinOpts()
		opts.VIP = nodestate.VIP{}
		if _, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx); err != nil {
			t.Fatalf("join: %v", err)
		}
		if r.saved == nil || r.saved.VIP.Address != "" {
			t.Errorf("the joined node state records VIP %q, want none", r.saved.VIP.Address)
		}
	})

	t.Run("the member-add dials the endpoints the grant authenticates", func(t *testing.T) {
		joinFixture(t)
		r := &recordedJoin{listMembers: listedMembers()}
		fx := fakeJoinEffects(r)
		fx.downloadGrant = vipGrant([]string{"10.0.0.12", "10.0.0.13"}, "10.0.0.10")
		opts := freshJoinOpts()
		opts.VIP = nodestate.VIP{}
		if _, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx); err != nil {
			t.Fatalf("join: %v", err)
		}
		if !r.addCalled || !slices.Equal(r.addIPs, []string{"10.0.0.12", "10.0.0.13"}) {
			t.Errorf("the member-add dialled %v (called %v), want the grant's authenticated endpoints", r.addIPs, r.addCalled)
		}
	})
}

// An inherited VIP needs the same live validation as an explicitly configured
// one. Authentication of the grant does not prove the VIP is currently serving.
func TestInheritedVIPRunsLiveJoinValidation(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	fx := fakeJoinEffects(r)
	fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "10.0.0.10")
	cause := errors.New("VIP is not serving")
	checked := ""
	fx.vipServes = func(vip string) error { checked = vip; return cause }
	opts := freshJoinOpts()
	opts.VIP = nodestate.VIP{}
	_, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx)
	if !errors.Is(err, cause) || checked != "10.0.0.10" {
		t.Fatalf("inherited VIP validation: checked=%q error=%v, want inherited VIP refusal", checked, err)
	}
	if r.addCalled || r.saved != nil {
		t.Fatal("an unavailable inherited VIP was admitted")
	}
}

// A join refused before it wrote the bundle downloaded nothing, so a secret
// on disk is an earlier attempt's: it is removed, and said to be one, even
// when the refused bundle carries a secret of that name.
func TestSettleJoinSecretsBeforeTheBundleIsWritten(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, encryptionConfigEntry)
	if err := os.WriteFile(key, []byte("an earlier attempt's"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := settleJoinSecrets(profile.Profile{}, keyBundle(true), false, dir, &out)
	if err == nil || !strings.Contains(err.Error(), "left by an earlier attempt") {
		t.Fatalf("settleJoinSecrets = %v, want the refusal naming the leftover", err)
	}
	if strings.Contains(out.String(), "it downloaded") || !strings.Contains(out.String(), "before it writes the shared certificates") {
		t.Errorf("the join said %q; nothing was downloaded", out.String())
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Errorf("the leftover key is still at %s: %v", key, err)
	}
}
