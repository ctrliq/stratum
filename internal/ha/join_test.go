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
	residueDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(residueDir, "leftover"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareControlPlaneJoin(context.Background(), opts, residueDir, productionJoinEffects()); err == nil || !strings.Contains(err.Error(), "residue") {
		t.Fatalf("non-member residue passed early preflight: %v", err)
	}
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

// keyBundle is a downloaded bundle that carried the cis key, or none, with an unnamed uploader.
func keyBundle(carried bool) bundleProfile {
	if carried {
		return bundleProfile{Secrets: []string{encryptionConfigEntry}}
	}
	return bundleProfile{}
}

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

// A bundle names its uploader's profile; a join of any other is refused whatever it carries.
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

// An earlier journal says only whether the bundle carried the cis key; that reads as the entry.
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

// A profile other than cis gets the same comparison of its declared secrets, by name.
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

// The answer has to come from the bundle: asking the disk would call an earlier failed join keyed.
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
		// applyBundle is also CA rotation's path, on a control plane whose key this is: never remove it.
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

// What a recording effect set captured: the order the effects ran in and the pinned arguments.
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
	rtState              *nodestate.NodeState

	failSaveState      error
	failRuntimeConfigs error
	listMembers        etcdMembership
	quorumHeldErr      error
	failRemoveByID     error
	detectIP           string
	failAdd            error
	failRetry          error
	clusterMembersFn   func() (etcdMembership, error)

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

	failVIP             error
	vipCalls            int
	vipAddr             string
	vipAfterKubeconfigs bool
	intentAtVIP         bool
}

// A joinEffects set that records each call in order and succeeds.
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
		// The real assembly behind a recorder, so a fold off the shared path fails the order pins.
		assemble: func(ctx context.Context, a NodeAssembly, req AssembleRequest) (host.NodeIPChoice, error) {
			r.assembleCalls++
			r.assembleReq = &req
			return AssembleNodeContext(ctx, a, req)
		},
	}
}

// Points the version paths and the intent path at temporary ones.
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

// A complete JoinOptions for an IPv4 node joining through the VIP 10.0.0.10.
func freshJoinOpts() JoinOptions {
	return JoinOptions{
		Server:  "https://10.0.0.10:6443",
		Token:   "bootstrap-token",
		CAHash:  "pin",
		CertKey: "certificate-key",
		VIP:     nodestate.VIP{Address: "10.0.0.10"},
	}
}

// Any reorder, drop or double-run of the effects fails here, including one below the member-add.
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
			if !r.addCalled || r.addPeer != "https://192.0.2.9:2380" {
				t.Errorf("member-add peer URL = %q (called %v), want this node's https://192.0.2.9:2380", r.addPeer, r.addCalled)
			}
			if r.saved == nil {
				t.Fatal("no node state was saved")
			}
			if r.saved.EtcdState != "existing" || st.EtcdState != "existing" {
				t.Errorf("saved EtcdState = %q, returned %q, want existing", r.saved.EtcdState, st.EtcdState)
			}
			if r.rtState == nil || r.rtState.Role != "controller" || r.rtState.EtcdName != r.saved.EtcdName {
				t.Errorf("the runtime-config write got %+v, want the node state built before membership", r.rtState)
			}
		})
	}
}

// Pins the addresses a fresh IPv4 join with VIP 10.0.0.10 hands downstream.
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
	netc, err := netcfg.Load()
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

// The kubeconfigs and the saved server must agree with bring-up's endpoint rule, not the join URL.
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

// Every gate must refuse before the member-add; no row may save state or compensate.
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

// A parallel controller's join can time out the probe; re-running it alone succeeds.
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

// Overwrites the fixture's netconfig with custom ranges, so defaults cannot pass for the bundle's.
func customBundleNetwork(t *testing.T) {
	t.Helper()
	if err := netcfg.Save("10.42.0.0/16", "10.43.0.0/16", "fuzz.internal"); err != nil {
		t.Fatalf("netcfg.Save: %v", err)
	}
}

// A differing lone range would be dropped for the bundle's; a pair would replace the cluster's.
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
			// Each value in its role: the given one beside its field, the recorded one as the value to set.
			for _, want := range []string{
				fmt.Sprintf("%s %q", tc.field, tc.given),
				fmt.Sprintf("set it to %q", tc.bundle),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not put each value in its role, missing %q: %v", want, err)
				}
			}
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

// An unset or matching spec.network joins on the ranges and domain the shared certs delivered.
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

// Alternate CIDR spellings must be equal by value; different or invalid ranges still refuse.
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

// The refusal names field, configured and recorded value; the re-run with recorded ones resumes.
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
			// The first run adds its member and fails the state save, so the journal survives.
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
			// The re-run resumed from the journal; a fresh join would reach another refusal.
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
			// Each value in its role: a swap would tell the user to keep the wrong range.
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

			// The remedy: the re-run with the recorded ranges resumes, adopting the member without an add.
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

// The check runs once, for the VIP, after the kubeconfigs and before the intent file exists.
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

// Nothing about membership moves: no add, no state, the intent and etcd dir as they were.
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

// A check that never reached the VIP must not tell the operator to change spec.vip.address.
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

// A controller's own address passes the TLS check, so it is refused before membership.
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

// A reused bundle can predate a newer controller; identify it from the live etcd probe.
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

// The join's refusal rests on the same TLS handshake ha-repoint runs.
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

// A key the bundle did not supply, or a refused join downloaded, is removed and said so.
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

// The bundle entry is the key path only while EncryptionConfigPath sits in PKIDir under its name.
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

// A secret named like an engine entry is refused: the bundle never carries one file for both.
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

const intentMemberID = 1234

// A two-member cluster whose second member is this join's node, unstarted.
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

// The intent file's JSON contract, as a re-run of `stratum up --join` and an operator read it.
type joinIntentFile struct {
	MemberName string `json:"memberName"`
	PeerURL    string `json:"peerURL"`
	MemberID   uint64 `json:"memberID"`
	Phase      string `json:"phase"`
}

func parseIntent(t *testing.T, raw []byte) joinIntentFile {
	t.Helper()
	var f joinIntentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("the intent file is not the recorded JSON shape: %v (%q)", err, raw)
	}
	return f
}

// The intent file's existence, bytes and permissions at the moment an effect runs.
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

// The journal brackets the add: on disk before it and at the save, recording the member ID after.
func TestControlPlaneJoinWritesTheIntentBeforeTheMemberAdd(t *testing.T) {
	joinFixture(t)
	// The journal's decisions are printed; the test captures the seam.
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
	// The journal outlives the save: without it a crash before etcd's first WAL is unrecoverable.
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

// Fsync and dir sync pin: a power loss in the member-add window cannot lose the journal.
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

// A failed rename is cleaned up: the directory is left as it was found.
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

// A directory that cannot be synced is a refusal with its cause, not a silent maybe.
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

// The remove runs once, by ID, after the quorum gate; the peer-URL remove is the add's rollback.
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

// The gate fails closed, keeping the intent: removing a member the cluster may still need is worse.
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

// The compensation learns the ID from the member list and never advises member remove 0.
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
	// What the compensation's re-read answers when the added member is not there.
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

// A committed second-voter add needs both voters for quorum until the joiner starts.
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

// An unusable intent refuses the join: absence would run a second member-add over it.
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

// One member-add across both runs, whatever phase the journal was left in.
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
			// The first run adds the member and fails before the state save, leaving the journal.
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
			// The re-run: the list answers, and the recorded member is still in it; adopt and finish.
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
			// The adopted membership is what etcd must boot with.
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
			// The journal outlives the resumed save too; a later start confirms local etcd data.
			if _, statErr := os.Stat(joinIntentPath); statErr != nil {
				t.Fatalf("the journal did not survive a resumed join that finished: %v", statErr)
			}
		})
	}
}

// Adoption at another address would finish into a node state etcd cannot start with.
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
			// The first run adds the member at 192.0.2.9 and fails before the state save.
			r1 := &recordedJoin{}
			tc.wedge(r1)
			if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), etcdDir, fakeJoinEffects(r1)); err == nil {
				t.Fatal("the first join succeeded although its state save failed or its member-add was lost")
			}
			if _, err := os.Stat(joinIntentPath); err != nil {
				t.Fatalf("the intent journal did not survive the failed join: %v", err)
			}
			// The re-run resolves another node IP; the cluster still lists the recorded peer URL.
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
			// The journal survives the refusal with the identity the first run left.
			raw, readErr := os.ReadFile(joinIntentPath)
			if readErr != nil {
				t.Fatalf("the intent journal did not survive the refusal: %v", readErr)
			}
			if kept := parseIntent(t, raw); kept.MemberID != tc.wedgedID || kept.PeerURL != "https://192.0.2.9:2380" {
				t.Errorf("the kept journal records %+v, want the first run's member at ID %d", kept, tc.wedgedID)
			}
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

// The stale intent is replaced by a fresh one; the recorded member is already gone.
func TestControlPlaneJoinReplacesAStaleIntent(t *testing.T) {
	joinFixture(t)
	// Discarding the stale journal is said, with the identity it recorded.
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
	// At member-add time the journal already records this run's member, not the removed one.
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
	// The replacement journal outlives the save and ends when a later start confirms etcd.
	fresh, readErr := os.ReadFile(joinIntentPath)
	if readErr != nil {
		t.Fatalf("the journal did not survive a completed join: %v", readErr)
	}
	if done := parseIntent(t, fresh); done.MemberName != r.addName || done.PeerURL != r.addPeer {
		t.Errorf("the surviving journal records %+v, want the fresh member %q at %q",
			done, r.addName, r.addPeer)
	}
}

// A refusal etcd answered outright cannot have committed; a lost reply may have.
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

// No member-list read, no retry; etcd's own answer is the error, and the journal stays.
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
		{"another controller's learner not promoted yet", rpctypes.ErrTooManyLearners, "too many learner members", true},
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

// The member list re-read decides: adopt, retry once, or refuse — never a guess.
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

// Only the retry's not-started refusals carry the concurrent-join hint.
func TestAmbiguousMemberAddRetryRefusedByAnUnstartedMemberSaysToJoinOneAtATime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retryErr error
		wantHint bool
	}{
		{"another controller's member not started yet", rpctypes.ErrMemberNotEnoughStarted, true},
		{"another controller's learner not promoted yet", rpctypes.ErrTooManyLearners, true},
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

// pkg/cli's dependency; the shared handler threads the run's interrupt context through it.
var _ func(context.Context, JoinOptions) (*nodestate.NodeState, error) = PrepareControlPlaneJoin

// Preparation touches no membership; the commit runs the member-add, then the state save.
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
	// Opaque: a composite literal naming an unexported field does not compile outside internal/ha.
	typ := reflect.TypeOf(prepared)
	if typ.NumField() == 0 {
		t.Fatal("the prepared value carries no fields between the phases")
	}
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.PkgPath == "" {
			t.Errorf("the prepared value's field %q is exported: a populated value could be constructed outside internal/ha", f.Name)
		}
	}
	// The zero value is inert: the commit refuses it rather than save on a guess.
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
	// The journal outlives the save until a later start confirms the local etcd data.
	if _, statErr := os.Stat(joinIntentPath); statErr != nil {
		t.Fatalf("the intent file did not survive a completed join: %v", statErr)
	}
}

// What the shared assembly ran, in order, with the refusal hooks' errors to inject.
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

// The built-in v4 CIDRs under a test domain, so SAN and CIDR assertions have fixed values.
func assemblyNetc() netcfg.Config {
	netc, err := netcfg.Parse("10.245.0.0/16", "10.96.0.0/12")
	if err != nil {
		panic(err) // literal CIDRs, always parse
	}
	netc.ClusterDomain = "fuzz.internal"
	return netc
}

// A NodeAssembly that records each call, plus a request with every input distinct.
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

// The caller's inputs reach the steps that consume them, and the state renders the configs.
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

// A hook's refusal stops the assembly, and its error is the error reported.
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

// The join hands the assembly the request its own options resolve to, exactly once.
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

// A join entered after a signal refuses before touching anything; a live context still starts.
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
		// The test binary carries no payload, so staging is where a live join lands.
		if !strings.Contains(err.Error(), "staging payload") {
			t.Fatalf("a live join did not reach its first real step: %v", err)
		}
	})
}

// Whether the given source window consults the context at all.
func observesContext(window []byte) bool {
	for _, shape := range []string{"ctx.Err(", "ctx.Done(", "(ctx"} {
		if bytes.Contains(window, []byte(shape)) {
			return true
		}
	}
	return false
}

// A context consult must sit between the download step and the member-add call.
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

// A kill inside the window would orphan the added member, so the first signal is deferred.
func TestJoinDefersTheFirstSignalAcrossTheMemberAddToStateSave(t *testing.T) {
	src, err := os.ReadFile("join.go")
	if err != nil {
		t.Fatal(err)
	}
	// The window spans addMemberIfEtcdDataDirEmpty, the saveState seam, and the commit's return.
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
			// kube-vip's kubeconfig is written exactly when the node has a VIP to elect for.
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

// The take is the latest point that still prevents the add; the release comes after the save.
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
		// The journal was written during preparation; it stays, and a re-run reconciles it.
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

// The refusal repairs only with a new mint; the re-run with it finishes the join past the journal.
func TestControlPlaneJoinRefusesAFenceItsTokenCannotTake(t *testing.T) {
	joinFixture(t)
	etcdDataDir := t.TempDir()
	r := &recordedJoin{}
	// The refused run added nothing, so the cluster lists only cp1 until a member-add lands.
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

	// The re-run, with the new command's token and certificate-key for the same cluster.
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

// Checked as Kubernetes authorizes them: collection create has no resource name.
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

// A downloadGrant stand-in with the metadata's endpoints, VIP and a fixed generation.
func vipGrant(published []string, vip string) func(string, string, func(bundleProfile) error) (bundleGrant, error) {
	return func(string, string, func(bundleProfile) error) (bundleGrant, error) {
		return bundleGrant{Published: published, VIP: vip, Generation: 7}, nil
	}
}

// The VIP comes from the grant's authenticated metadata, never from an annotation.
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
		// The pre-envelope shape: no VIP metadata, so the configured VIP stands.
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

// Authentication of the grant does not prove the VIP is currently serving.
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

// A secret on disk before the bundle is written is an earlier attempt's, removed and said.
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

// The state the join saves and returns carries the spec.node facts, not only its first restart.
func TestControlPlaneJoinCarriesItsNodeFacts(t *testing.T) {
	joinFixture(t)
	r := &recordedJoin{listMembers: listedMembers()}
	fx := fakeJoinEffects(r)
	fx.downloadGrant = vipGrant([]string{"10.0.0.11"}, "10.0.0.10")
	opts := freshJoinOpts()
	opts.NodeLabels = map[string]string{"topology.kubernetes.io/zone": "rack-1"}
	opts.NodeTaints = []string{"stratum.ciq.com/unverified=true:NoSchedule"}
	opts.RuntimeHandlers = map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"}
	st, err := prepareControlPlaneJoin(context.Background(), opts, t.TempDir(), fx)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	for name, s := range map[string]*nodestate.NodeState{"returned": st, "saved": r.saved} {
		if s == nil {
			t.Fatalf("no %s node state", name)
		}
		if !reflect.DeepEqual(s.NodeLabels, opts.NodeLabels) || !reflect.DeepEqual(s.NodeTaints, opts.NodeTaints) || !reflect.DeepEqual(s.RuntimeHandlers, opts.RuntimeHandlers) {
			t.Errorf("%s node state carries labels %v, taints %v, handlers %v; want the join's spec.node", name, s.NodeLabels, s.NodeTaints, s.RuntimeHandlers)
		}
	}
}
