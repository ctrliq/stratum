package certs

// The multi-node harness for `stratum certs rotate-ca --cluster`.
//
// RotateCACluster drives the rotation through the engine/driver split the
// etcd-CA migration uses (etcdDriver, etcdMigration), so a test can halt it
// mid-rotation and re-run it:
//
//   - `caDriver` (cacluster.go) is the seam between the --cluster phase
//     engine and the cluster. The production implementation answers Survey
//     from the node roster (classifyForCA), runs phases through RotateCA
//     in-process on the initiator and node Jobs on every other node,
//     publishes the staged material and its key Secret (ha.NewCertKey,
//     ha.ReUploadCerts, stageCarotateKey), rebuilds the admin client from the
//     reissued admin.kubeconfig, rolls the in-cluster API clients, runs the
//     workload preflight and the finalize preflights, and keeps the shared
//     rotation record. Tests supply one backed by one REAL PKI directory per
//     node, so the per-node state machine and the caPhaseApplied skip
//     decision are the production ones.
//
//   - `caCluster` is the phase engine, holding no client and doing no I/O of
//     its own — the same contract as etcdMigration. The tests drive it as
//     &caCluster{drv: f, o: CARotateOptions{...}, log: ...}.run(); the
//     confirmLease func(string) error field, when set, renews the shared
//     maintenance Lease before each dispatch and is nil in tests.
//
//     The engine's decisions, which the etcd engine already pins as a shape:
//     survey the roster first (a roster without a labeled control plane
//     refuses); run the phases PHASE-MAJOR, initiator first, publishing the
//     staged key after the initiator's trust-new and before the followers
//     fetch it; rebuild the client and roll the in-cluster clients between
//     trust-new and sign-new; run the finalize preflights before finalize;
//     check Interrupted between nodes; clean up this run's key on every exit.
//     A phase a node has already applied (caPhaseApplied — the node path
//     decides skip, exactly as migrateEtcdCAWith does before it fetches) is
//     dispatched again and returns nil without applying, which is what makes
//     a second run after a halt a RESUME instead of a refusal.
//
//     Every halt and every refusal names the command that works in that
//     state — most often a re-run of `stratum certs rotate-ca --cluster` from
//     this control plane, which skips the applied phases and finishes. The old
//     guidance ("run `stratum certs check` on every node ... To abandon the
//     rotation, run `--rollback --yes` on every node") pointed at work the
//     operator then had to do node by node, and the up-front refusal sent a
//     halted run to `--rollback` as if the re-run were a fresh rotation.
//
// version.PKIDir is global, so none of these may run in parallel — the same
// constraint the etcd fake lives under.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestCheckNoPendingCSRFailsClosed pins the fail-closed rule: the CSR arm of
// FinalizePreflight — the hard gate before a CA rotation drops the old CA — must
// REFUSE when it cannot list CSRs, not fall through to "safe to finalize". A
// swallowed list error (a transient apiserver blip mid-rotation) would let
// finalize collapse the trust bundle to the new CA while a worker's new-CA CSR
// was still pending, locking that node out.
func TestCheckNoPendingCSRFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("a list error fails closed", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("list", "certificatesigningrequests",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("apiserver mid-restart: 503")
			})
		err := checkNoPendingCSR(ctx, cs)
		if err == nil {
			t.Fatal("the gate proceeded despite being unable to list CSRs — a rotation could drop the old CA with a worker still pending")
		}
		if !strings.Contains(err.Error(), "cannot verify") {
			t.Errorf("error should explain the gate could not evaluate: %v", err)
		}
	})

	t.Run("a pending CSR is refused", func(t *testing.T) {
		cs := fake.NewSimpleClientset(&certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "csr-pending"}, // no cert, no terminal condition
		})
		if err := checkNoPendingCSR(ctx, cs); err == nil {
			t.Fatal("a pending CSR must block finalize (a node is awaiting its new-CA cert)")
		}
	})

	t.Run("issued and terminal CSRs do not block", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			&certificatesv1.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "csr-issued"},
				Status:     certificatesv1.CertificateSigningRequestStatus{Certificate: []byte("a-cert")},
			},
			&certificatesv1.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "csr-denied"},
				Status: certificatesv1.CertificateSigningRequestStatus{
					Conditions: []certificatesv1.CertificateSigningRequestCondition{{Type: certificatesv1.CertificateDenied}},
				},
			},
		)
		if err := checkNoPendingCSR(ctx, cs); err != nil {
			t.Errorf("issued/denied CSRs must not block a finalize: %v", err)
		}
	})
}

// fakeCACluster is a caDriver backed by one real PKI directory per node. The
// INITIATOR's phases run the production per-node path (cpTrustNew, cpSignNew,
// cpFinalize); a follower's phases run the same state machine through the pki
// primitives with the staged-material fetch standing in for
// ha.DownloadCertsAdmin, because the follower path's fetch reaches the API
// server directly and this seam is the cluster, not the client. Every node
// consults caPhaseApplied before it applies, which is the node-path contract
// TestSignNewIsSkippedWhenAlreadyApplied pins for the real paths.
type fakeCACluster struct {
	t     *testing.T
	cs    *fake.Clientset
	order []string          // node names, initiator first
	ips   map[string]string // node -> its address
	dirs  map[string]string // node -> its PKI directory
	bases map[string]string // node -> its config base (kubeconfig, kubernetes, socket)
	calls []string          // every (node, phase) dispatch, in order
	skips []string          // every "node:phase" the node path skipped as applied

	failAt map[string]bool // "node:phase" -> RunPhase returns an error

	published    int
	publishedIDs []string
	cleaned      []string
	stagedKey    bool   // the fake cluster's staged key Secret: set by a publish, cleared by a cleanup
	rotationRun  string // the identity ResolveRotation resolved for the current run
	rebuilt      int
	rolled       int

	// publishedFiles is the shared material the last publish handed the
	// followers: every shared and rotation entry of the trust layout the
	// initiator held at that moment, as ha.ReUploadCerts uploads it.
	publishedFiles map[string][]byte
	// resolved is the production driver state ResolveRotation left for the
	// current run, which the initiator's phase options are built from.
	resolved *prodCADriver

	interrupt error
	// interruptAfter is "node:phase" -> the operator's Ctrl-C lands while
	// that dispatch runs: the dispatch completes (the initiator's phase does
	// not observe ctx) and every boundary check after it sees the interrupt.
	interruptAfter map[string]bool
}

// newFakeCACluster mints the per-node PKI: each node gets a full, self-
// consistent control plane (root, front-proxy CA, SA, leaves) in its own
// directory, the way a real cluster's nodes each hold the shared material —
// the signers byte-identical on every control plane, as a join hands them
// over, and the leaves each node's own. The rotation replaces every node's
// root with the INITIATOR's staged one — the follower fetch copies it — so the
// final cross-node root identity is a real property of the run, not of the
// fixture.
func newFakeCACluster(t *testing.T, names ...string) *fakeCACluster {
	t.Helper()
	f := &fakeCACluster{
		t:      t,
		cs:     fake.NewSimpleClientset(),
		ips:    map[string]string{},
		dirs:   map[string]string{},
		bases:  map[string]string{},
		failAt: map[string]bool{},
	}
	saved := version.PKIDir
	t.Cleanup(func() { version.PKIDir = saved })
	// No node is a CIS bootstrap node here, so finalize never owes a kubelet
	// serving-cert renewal.
	savedBootstraps := kubeletBootstrapsServing
	kubeletBootstrapsServing = func() (bool, error) { return false, nil }
	t.Cleanup(func() { kubeletBootstrapsServing = savedBootstraps })

	// The fake has no apiserver to answer /healthz, so the real gate before
	// the leftover-key reap (reapLeftoverCarotateKey -> waitAPIServer) burns
	// its full 60s carotateGateBudget per finalize or rollback before the
	// read fails cleanly — dead time that pushed the package suite past its
	// budget. Stand the gate in for every fake-driven run: production keeps
	// it, and the reap's behavior — gate, spent-key proof, preconditions — is
	// pinned by the ownership tests on their own clientsets
	// (carotate_cleanup_test.go), which stub the same var themselves.
	savedWait := waitAPIServer
	waitAPIServer = func(context.Context, kubernetes.Interface) error { return nil }
	t.Cleanup(func() { waitAPIServer = savedWait })

	// The fake has no apiserver for the staged-key read either. The cluster's
	// staged key Secret is what the node paths scope their applied-phase
	// decision by when the run arrives without the orchestrator's identity —
	// the follower Job path — so the stand-in reports the run the fake has
	// staged and not yet cleaned, exactly what the real Secret answers.
	savedStagedRun := stagedRotationRunID
	stagedRotationRunID = func() (string, error) {
		cm, err := f.cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
		if err == nil {
			if cm.Data["complete"] == "true" {
				return "", nil
			}
			return cm.Data["run"], nil
		}
		if !f.stagedKey {
			return "", nil
		}
		return f.publishedIDs[len(f.publishedIDs)-1], nil
	}
	t.Cleanup(func() { stagedRotationRunID = savedStagedRun })

	for i, name := range names {
		base := t.TempDir()
		f.dirs[name] = filepath.Join(base, "pki")
		f.bases[name] = base
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		f.ips[name] = ip
		f.order = append(f.order, name)
		if i > 0 {
			// A control plane joins holding the cluster's signers; GenerateAll
			// reuses them and mints only this node's leaves.
			f.copyTrust(names[0], name, pki.TrustShared)
		}
		f.in(name, func() {
			if err := pki.GenerateAll(name, ip, nil); err != nil {
				t.Fatal(err)
			}
			if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", NodeIP: ip, KubeletNodeIP: ip}); err != nil {
				t.Fatal(err)
			}
		})
	}
	return f
}

type caNodeEnv struct {
	pkiDir, kubeconfigDir, k8sConfDir, adminConf, netConfig, controlSocket, nodeIP string
}

// in runs fn with the version paths and the node address pointed at one
// node's fixture, restoring them afterwards.
func (f *fakeCACluster) in(node string, fn func()) {
	f.t.Helper()
	savedPKI := pkiDir
	pkiDir = f.dirs[node]
	defer func() { pkiDir = savedPKI }()
	saved := caNodeEnv{
		pkiDir: version.PKIDir, kubeconfigDir: version.KubeconfigDir,
		k8sConfDir: version.K8sConfDir, adminConf: version.AdminConf,
		netConfig: version.NetConfigPath, controlSocket: version.ControlSocket,
		nodeIP: os.Getenv("NODE_IP"),
	}
	defer func() {
		version.PKIDir, version.KubeconfigDir, version.K8sConfDir = saved.pkiDir, saved.kubeconfigDir, saved.k8sConfDir
		version.AdminConf, version.NetConfigPath, version.ControlSocket = saved.adminConf, saved.netConfig, saved.controlSocket
		os.Setenv("NODE_IP", saved.nodeIP)
	}()
	version.PKIDir = f.dirs[node]
	version.KubeconfigDir = filepath.Join(f.bases[node], "kubeconfig")
	version.K8sConfDir = filepath.Join(f.bases[node], "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	version.NetConfigPath = filepath.Join(f.dirs[node], "netconfig")     // absent: the IPv4 defaults
	version.ControlSocket = filepath.Join(f.bases[node], "stratum.sock") // absent: no supervisor to reach
	os.Setenv("NODE_IP", f.ips[node])
	fn()
}

func (f *fakeCACluster) initiator() string { return f.order[0] }

func (f *fakeCACluster) Survey() ([]cnode, error) {
	out := make([]cnode, 0, len(f.order))
	for _, name := range f.order {
		out = append(out, cnode{Name: name, IsCP: true})
	}
	return out, nil
}

func (f *fakeCACluster) RunPhase(n cnode, phase string, fetchShared, force bool) error {
	f.calls = append(f.calls, n.Name+":"+phase)
	if f.interruptAfter[n.Name+":"+phase] {
		defer func() { f.interrupt = context.Canceled }()
	}
	if f.failAt[n.Name+":"+phase] {
		return fmt.Errorf("node %s is unreachable", n.Name)
	}
	var err error
	f.in(n.Name, func() {
		// The node path decides skip BEFORE it fetches or guards, the way
		// migrateEtcdCAWith does: a phase already applied here writes nothing.
		// The consult is scoped the way the production paths scope it: the
		// initiator carries the orchestrator's identity in its options; a
		// follower (the node Job path) scopes by the staged key the cluster
		// holds.
		runID, readErr := stagedRotationRunID()
		if readErr != nil {
			err = readErr
			return
		}
		if n.Name == f.initiator() {
			runID = f.rotationRun
		}
		applied, aerr := phaseAppliedForRun(phase, runID)
		if aerr != nil {
			err = aerr
			return
		}
		if applied {
			f.skips = append(f.skips, n.Name+":"+phase)
			return
		}
		if fetchShared {
			// Stands in for ha.DownloadCertsAdmin: the follower receives the
			// initiator's staged CA material, so every node promotes the SAME
			// new root.
			if err = f.fetchStaged(n.Name); err != nil {
				return
			}
		}
		if n.Name == f.initiator() {
			err = f.initiatorPhase(phase, force)
			return
		}
		err = f.followerPhase(n, phase, force)
	})
	return err
}

// fetchStaged writes the last publish's material onto node — what
// ha.DownloadCertsAdmin writes from the stratum-certs Secret on a follower
// control plane in trust-new: every shared and rotation entry, the live
// signers and the trust bundles as well as the staged material, each
// replacing the node's own.
func (f *fakeCACluster) fetchStaged(node string) error {
	f.t.Helper()
	if f.publishedFiles == nil {
		return fmt.Errorf("no staged material was published")
	}
	for name, b := range f.publishedFiles {
		if err := os.WriteFile(filepath.Join(f.dirs[node], name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// trustFiles reads every entry of the given trust-layout classes node holds,
// the way the join bundle's upload collects them: an entry the node does not
// hold is left out.
func (f *fakeCACluster) trustFiles(node string, classes ...pki.TrustFileClass) map[string][]byte {
	f.t.Helper()
	files := map[string][]byte{}
	for _, tf := range pki.TrustLayout() {
		if !slices.Contains(classes, tf.Class) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dirs[node], tf.Name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			f.t.Fatal(err)
		}
		files[tf.Name] = b
	}
	return files
}

// copyTrust copies from's entries of the given classes onto to.
func (f *fakeCACluster) copyTrust(from, to string, classes ...pki.TrustFileClass) {
	f.t.Helper()
	if err := os.MkdirAll(f.dirs[to], 0o700); err != nil {
		f.t.Fatal(err)
	}
	for name, b := range f.trustFiles(from, classes...) {
		if err := os.WriteFile(filepath.Join(f.dirs[to], name), b, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
}

// initiatorPhase runs the production per-node path for one phase, with the
// options the production driver gives the initiator's in-process phase.
func (f *fakeCACluster) initiatorPhase(phase string, force bool) error {
	d := f.resolved
	if d == nil {
		d = &prodCADriver{rotationRun: f.rotationRun}
	}
	o := d.initiatorOptions(phase, force)
	switch phase {
	case PhaseTrustNew:
		return cpTrustNew(io.Discard, o)
	case PhaseSignNew:
		return cpSignNew(io.Discard, o)
	case PhaseFinalize:
		return cpFinalize(io.Discard, o)
	}
	return fmt.Errorf("unknown phase %q", phase)
}

// followerPhase applies one phase through the pki state machine the node
// paths drive — the fixture's stand-in for the per-node Job's `stratum certs
// rotate-ca --phase`, whose promotion, leaf reissue and finalize are these
// same primitives plus restarts this fixture deliberately has none of.
func (f *fakeCACluster) followerPhase(n cnode, phase string, force bool) error {
	switch phase {
	case PhaseTrustNew:
		return pki.RotateCAMaterial(force)
	case PhaseSignNew:
		if err := pki.PromoteCA(); err != nil {
			return err
		}
		if err := pki.PromoteSA(); err != nil {
			return err
		}
		if err := pki.RotateLeaves(n.Name, f.ips[n.Name], nil); err != nil {
			return err
		}
		return pki.MarkCAPromoted()
	case PhaseFinalize:
		if err := pki.FinalizeCA(); err != nil {
			return err
		}
		// The production finalize is cpFinalize, which records the finished
		// rotation's identity — under the orchestrator's identity, or the run
		// this node recorded at its sign-new when the phase arrives without
		// one (a node Job). The follower path mirrors that record; the
		// applied-phase scoping reads it back.
		markCarotateFinishedRun(io.Discard, f.rotationRun)
		return nil
	}
	return fmt.Errorf("unknown phase %q", phase)
}

func (f *fakeCACluster) InitiatorStaged() bool {
	staged := false
	f.in(f.initiator(), func() { staged = pki.CARotationStaged() })
	return staged
}

func (f *fakeCACluster) PublishStagedKey() (string, error) {
	f.published++
	f.publishedFiles = f.trustFiles(f.initiator(), pki.TrustShared, pki.TrustRotation)
	id := f.rotationRun
	if id == "" {
		id = fmt.Sprintf("run-%d", f.published)
	}
	f.publishedIDs = append(f.publishedIDs, id)
	f.stagedKey = true
	if err := putStagedKey(context.Background(), f.cs, "fake-key", rotationStagedKeyOwner(id)); err != nil {
		return "", err
	}
	return id, nil
}

// ResolveRotation delegates to the production driver's checkpoint resolution,
// run on the initiator's PKI: an unfinished stratum-ca-rotation record names
// the rotation in flight and its completed phase; with no record, a staged
// rotate-ca key's run annotation is adopted; otherwise the rotation is fresh.
func (f *fakeCACluster) ResolveRotation() (string, bool, error) {
	d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: f.cs}
	var id string
	var resumed bool
	var err error
	f.in(f.initiator(), func() { id, resumed, err = d.ResolveRotation() })
	f.rotationRun = id
	f.resolved = d
	return id, resumed, err
}

func (f *fakeCACluster) CompletedPhase() string {
	cm, err := f.cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
	if err != nil || cm.Data["complete"] == "true" {
		return ""
	}
	return cm.Data["phase"]
}

func (f *fakeCACluster) RecordPhase(phase string) error {
	var err error
	f.in(f.initiator(), func() {
		err = (&prodCADriver{w: io.Discard, ctx: context.Background(), cs: f.cs, rotationRun: f.rotationRun}).RecordPhase(phase)
	})
	return err
}

func (f *fakeCACluster) FinalizeStarted() bool {
	cm, err := f.cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
	return err == nil && cm.Data["complete"] != "true" && cm.Data[finalizeStartedKey] == "true"
}

func (f *fakeCACluster) RecordFinalizeStarted() error {
	var err error
	f.in(f.initiator(), func() {
		err = (&prodCADriver{w: io.Discard, ctx: context.Background(), cs: f.cs, rotationRun: f.rotationRun}).RecordFinalizeStarted()
	})
	return err
}

func (f *fakeCACluster) ClearFinishedRotation() error {
	var err error
	f.in(f.initiator(), func() {
		d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: f.cs, rotationRun: f.rotationRun}
		err = d.ClearFinishedRotation()
	})
	return err
}

func (f *fakeCACluster) CleanupStagedKey(runID string) {
	f.cleaned = append(f.cleaned, runID)
	f.stagedKey = false
	_ = f.cs.CoreV1().Secrets(caNamespace).Delete(context.Background(), caRotateKeySecret, metav1.DeleteOptions{})
}

func (f *fakeCACluster) RebuildClient() error { f.rebuilt++; return nil }

func (f *fakeCACluster) RestartClients() error { f.rolled++; return nil }

func (f *fakeCACluster) WorkloadsSettled() error { return nil }

func (f *fakeCACluster) ClusterHealthy([]cnode) error { return nil }

func (f *fakeCACluster) TokensChecked([]cnode, string) error { return nil }

func (f *fakeCACluster) Interrupted() error { return f.interrupt }

// callsFor returns the nodes one phase was dispatched to, in order.
func (f *fakeCACluster) callsFor(phase string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasSuffix(c, ":"+phase) {
			out = append(out, strings.TrimSuffix(c, ":"+phase))
		}
	}
	return out
}

// assertFinalizedOnOneRoot checks the finished state a completed run must
// leave: every node finalized, trusting the initiator's staged root alone.
func assertFinalizedOnOneRoot(t *testing.T, f *fakeCACluster) {
	t.Helper()
	var shared []byte
	f.in(f.initiator(), func() {
		var err error
		shared, err = os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
		if err != nil {
			t.Fatal(err)
		}
	})
	for _, name := range f.order {
		f.in(name, func() {
			if !pki.CAFinalized() {
				t.Errorf("%s did not finalize", name)
			}
			n, err := pki.CABundleCertCount("ca")
			if err != nil || n != 1 {
				t.Errorf("%s finalized with %d CAs in its bundle (%v), want 1", name, n, err)
			}
			live, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(live) != string(shared) {
				t.Errorf("%s finalized on a root the initiator never staged — a divergent CA split the trust domain", name)
			}
		})
	}
}

// caCRT reads node's live root certificate.
func (f *fakeCACluster) caCRT(t *testing.T, node string) []byte {
	t.Helper()
	var b []byte
	f.in(node, func() {
		var err error
		b, err = os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
		if err != nil {
			t.Fatal(err)
		}
	})
	return b
}

// TestInterruptMidPhaseNamesThePhaseInProgress: a Ctrl-C that lands while the
// initiator runs a phase lets that phase finish on the initiator — its local
// work does not observe ctx — and the run stops at the next boundary. The halt
// must describe the phase in progress, not the last one every node completed:
// after the initiator finalized, its old CA is gone, so guidance that tells
// the operator to roll back every node would split the cluster across two
// roots. Each interrupted run must also resume to one root.
func TestInterruptMidPhaseNamesThePhaseInProgress(t *testing.T) {
	for _, tc := range []struct {
		after   string   // the dispatch the interrupt lands in
		want    []string // the halt must say these
		wantNot []string // and must not say these
	}{
		{"cp1:trust-new", []string{"halted in " + PhaseTrustNew, "rotate-ca --cluster --yes"}, nil},
		{"cp1:sign-new", []string{"halted in " + PhaseSignNew, "rotate-ca --cluster --yes"}, []string{"halted in " + PhaseTrustNew}},
		{"cp1:finalize", []string{"halted in " + PhaseFinalize, "Do NOT roll back a node that already finalized", "rotate-ca --cluster --yes"},
			[]string{"--rollback --yes` on every node", "halted in " + PhaseSignNew}},
		// A follower's dispatch: the boundary before the next follower.
		{"cp2:trust-new", []string{"halted in " + PhaseTrustNew, "rotate-ca --cluster --yes"}, nil},
	} {
		t.Run(tc.after, func(t *testing.T) {
			f := newFakeCACluster(t, "cp1", "cp2", "cp3")
			f.interruptAfter = map[string]bool{tc.after: true}
			err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
			if err == nil {
				t.Fatal("the interrupted run completed")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("halt guidance missing %q: %v", w, err)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(err.Error(), w) {
					t.Errorf("halt guidance says %q, which is wrong in this state: %v", w, err)
				}
			}
			if n := strings.Count(err.Error(), "the run halted in"); n != 1 {
				t.Errorf("the halt carries its guidance %d times, want once: %v", n, err)
			}
			f.interruptAfter, f.interrupt = nil, nil
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
				t.Fatalf("the resumed run: %v", err)
			}
			assertFinalizedOnOneRoot(t, f)
		})
	}
}

// TestRotateCAClusterResumesAfterAHalt: a follower fails in sign-new; a second
// run skips the phases every node has applied and finishes; and every halt on
// the way names the command that works in that state — a re-run of
// `rotate-ca --cluster` from this control plane.
func TestRotateCAClusterResumesAfterAHalt(t *testing.T) {
	t.Run("a halt in sign-new resumes and finishes", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp3:sign-new"] = true

		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil {
			t.Fatal("the run completed although cp3 failed sign-new")
		}
		// Phase-major, initiator first, halting at the failed follower.
		wantCalls := []string{
			"cp1:trust-new", "cp2:trust-new", "cp3:trust-new",
			"cp1:sign-new", "cp2:sign-new", "cp3:sign-new",
		}
		if len(f.calls) != len(wantCalls) {
			t.Fatalf("dispatches = %v, want %v", f.calls, wantCalls)
		}
		for i, want := range wantCalls {
			if f.calls[i] != want {
				t.Fatalf("dispatch %d = %s, want %s (all: %v)", i, f.calls[i], want, f.calls)
			}
		}
		for _, want := range []string{"cp3", PhaseSignNew, "rotate-ca --cluster"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("halt guidance missing %q: %v", want, err)
			}
		}
		if f.published != 1 || len(f.cleaned) != 1 {
			t.Errorf("the halted run published %d key(s) and cleaned %d, want one published and its cleanup on the way out", f.published, len(f.cleaned))
		}
		// The halt leaves cp1 and cp2 past sign-new and cp3 still only
		// trusting old‖new — the state the second run must resume.
		f.in("cp1", func() {
			if !pki.CAPromoted() {
				t.Error("the initiator's completed sign-new left no promotion marker")
			}
		})
		f.in("cp3", func() {
			if pki.CAPromoted() {
				t.Error("cp3 was recorded as promoted although its sign-new failed")
			}
			if n, err := pki.CABundleCertCount("ca"); err != nil || n != 2 {
				t.Errorf("cp3 holds %d CAs (%v), want the widened old‖new bundle", n, err)
			}
		})
		cp1Root := func(t *testing.T) []byte {
			t.Helper()
			var b []byte
			f.in("cp1", func() {
				var err error
				b, err = os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
				if err != nil {
					t.Fatal(err)
				}
			})
			return b
		}
		beforeResume := cp1Root(t)

		// The resume: the same command, no flags added. The phases cp1 and
		// cp2 applied are dispatched again and skipped; cp3 runs sign-new
		// where it stopped; finalize runs everywhere.
		f.failAt["cp3:sign-new"] = false
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the resumed run: %v", err)
		}
		wantSkips := map[string]bool{
			"cp1:sign-new": true, "cp2:sign-new": true,
		}
		gotSkips := map[string]bool{}
		for _, s := range f.skips {
			gotSkips[s] = true
		}
		for skip := range wantSkips {
			if !gotSkips[skip] {
				t.Errorf("the resumed run did not skip %s (skips: %v)", skip, f.skips)
			}
		}
		for _, skip := range []string{"cp3:trust-new", "cp3:sign-new", "cp1:finalize", "cp2:finalize", "cp3:finalize"} {
			if gotSkips[skip] {
				t.Errorf("the resumed run skipped %s, which no node had applied (skips: %v)", skip, f.skips)
			}
		}
		if f.published != 1 || len(f.cleaned) != 2 {
			t.Errorf("the resume published %d key(s) and cleaned %d, want no republication of promoted material", f.published, len(f.cleaned))
		}
		if f.rebuilt != 1 || f.rolled != 1 {
			t.Errorf("the admin client was rebuilt %d time(s) and the in-cluster clients rolled %d, want once for the completed trust-new phase", f.rebuilt, f.rolled)
		}
		assertFinalizedOnOneRoot(t, f)
		// The resume must not have rotated the initiator's signer again: a
		// re-run of trust-new past a promoted signer stages a DIVERGENT root,
		// which is what the refusals halt to prevent.
		if after := cp1Root(t); string(after) != string(beforeResume) {
			t.Error("the resumed run replaced the initiator's promoted root — it re-ran a phase cp1 had applied")
		}
	})

	t.Run("a halt in trust-new resumes and finishes", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp2:trust-new"] = true

		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil {
			t.Fatal("the run completed although cp2 failed trust-new")
		}
		for _, want := range []string{"cp2", PhaseTrustNew, "rotate-ca --cluster"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("halt guidance missing %q: %v", want, err)
			}
		}
		f.in("cp1", func() {
			if n, err := pki.CABundleCertCount("ca"); err != nil || n != 2 {
				t.Errorf("the initiator holds %d CAs (%v), want the staged old‖new bundle", n, err)
			}
		})

		f.failAt["cp2:trust-new"] = false
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the resumed run: %v", err)
		}
		assertFinalizedOnOneRoot(t, f)
		// The halted run kept its key for the follower that had not finished
		// trust-new; the resume stages its own key in the kept
		// one's place and removes its own when the rotation completes.
		if f.published != 2 || len(f.cleaned) != 1 || f.cleaned[0] != f.publishedIDs[1] {
			t.Errorf("the two runs published %v and cleaned %v, want one publish per run and only the resumed run's cleanup", f.publishedIDs, f.cleaned)
		}
	})

	// A run that halted INSIDE finalize — some nodes finalized, one stopped —
	// resumes by skipping trust-new and sign-new on every node that finished
	// them, for the rotation the resume continues. This is the semantics the
	// applied-scoping must not lose: a finalized marker cleared at the
	// finalize restart, instead of scoped to the rotation it finished, would
	// send the resume back through trust-new on nodes whose old CA is gone —
	// staging a second root the finished nodes' leaves were never reissued
	// under.
	t.Run("a halt inside finalize resumes without re-running the finished phases", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp3:finalize"] = true // the last node's finalize stops the run

		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
			t.Fatal("the run completed although cp3 failed finalize")
		}
		beforeResume := f.caCRT(t, f.initiator())

		f.failAt["cp3:finalize"] = false
		f.calls, f.skips = nil, nil
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the resumed run: %v", err)
		}
		assertFinalizedOnOneRoot(t, f)
		for _, call := range f.calls {
			if strings.HasSuffix(call, ":trust-new") || strings.HasSuffix(call, ":sign-new") {
				t.Errorf("resumed a globally complete phase: %s", call)
			}
		}
		if after := f.caCRT(t, f.initiator()); string(after) != string(beforeResume) {
			t.Error("the resumed run replaced the root — it re-ran trust-new on a node that had finalized this rotation")
		}
	})
}

// TestHaltedRotationKeepsTheKeyForUnfinishedFollowers: a rotation that halts
// while a follower has not finished trust-new — the phase whose
// --fetch-shared reads the staged key — leaves that run's key Secret in
// place, and the halt message says so. A halt after trust-new has completed
// on every node has no reader left and removes the run's own key, as a
// completing run does.
func TestHaltedRotationKeepsTheKeyForUnfinishedFollowers(t *testing.T) {
	t.Run("a halt in trust-new keeps the staged key and says so", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp2:trust-new"] = true // a follower control plane mid trust-new

		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil {
			t.Fatal("the run completed although cp2 failed trust-new")
		}
		if f.published != 1 {
			t.Fatalf("published %d key(s), want the halted run's one publish", f.published)
		}
		if len(f.cleaned) != 0 {
			t.Errorf("the halted run cleaned %v, want nothing: a follower has not finished the phase that reads the staged key", f.cleaned)
		}
		for _, want := range []string{caRotateKeySecret, "left in place", "trust-new"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("halt message = %q, want it to say the staged key Secret is left in place (missing %q)", err, want)
			}
		}
	})

	t.Run("the resumed run removes its own key when it completes", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp2:trust-new"] = true
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
			t.Fatal("the first run completed although cp2 failed trust-new")
		}
		f.failAt["cp2:trust-new"] = false
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the resumed run: %v", err)
		}
		assertFinalizedOnOneRoot(t, f)
		// The halted run kept its key for the unfinished follower; the resume
		// stages its own key in the kept one's place and removes its own when
		// the rotation completes.
		if f.published != 2 || len(f.cleaned) != 1 || f.cleaned[0] != f.publishedIDs[1] {
			t.Errorf("published %v, cleaned %v, want one publish per run and only the resumed run's cleanup", f.publishedIDs, f.cleaned)
		}
	})

	t.Run("a halt after trust-new removes the run's own key", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp3:sign-new"] = true // every node finished trust-new: no reader of the staged key is left

		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil {
			t.Fatal("the run completed although cp3 failed sign-new")
		}
		if f.published != 1 || len(f.cleaned) != 1 || f.cleaned[0] != f.publishedIDs[0] {
			t.Errorf("published %v, cleaned %v, want the sign-new halt to remove the run's own key", f.publishedIDs, f.cleaned)
		}
	})
}

// TestSecondRotationRestagesAndChangesTheRoot pins that a `rotate-ca --cluster`
// run AFTER a completed one rotates again: the carotate-finalized marker
// survives a finished rotation, and an applied-phase decision that reads it
// unscoped makes the second run skip trust-new and sign-new on every control
// plane, pass finalize on the marker it wrote last time, and report success
// with the root byte-identical — a silent no-op. The applied decision is
// therefore scoped to the rotation it finished: a fresh run's identity matches
// no finished marker, so every phase runs and the root changes.
func TestSecondRotationRestagesAndChangesTheRoot(t *testing.T) {
	for _, next := range []string{"cp1", "cp2", "cp3"} {
		t.Run(next, func(t *testing.T) {
			f := newFakeCACluster(t, "cp1", "cp2", "cp3")
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
				t.Fatalf("the first rotation: %v", err)
			}
			assertFinalizedOnOneRoot(t, f)
			firstRoot := f.caCRT(t, f.initiator())

			// Start the next rotation from each possible control plane.
			for i, name := range f.order {
				if name == next {
					f.order[0], f.order[i] = f.order[i], f.order[0]
					break
				}
			}
			f.calls, f.skips = nil, nil
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
				t.Fatalf("the second rotation: %v", err)
			}
			assertFinalizedOnOneRoot(t, f)
			for _, skip := range f.skips {
				if strings.HasSuffix(skip, ":"+PhaseTrustNew) || strings.HasSuffix(skip, ":"+PhaseSignNew) {
					t.Errorf("the second rotation skipped %s as already applied: that decision was scoped to the finished rotation, not this one", skip)
				}
			}
			if secondRoot := f.caCRT(t, f.initiator()); string(secondRoot) == string(firstRoot) {
				t.Error("the second rotation completed with the first rotation's root byte-identical — it rotated nothing")
			}

		})
	}
}

// TestRotateCAClusterResumesItsOwnStagedRotation pins the re-pointed refusal
// at the RotateCACluster entry: a re-run on a control plane whose prior run
// halted mid-rotation must not be refused AS IF a fresh rotation had been
// requested ("already staged ... before starting a new one", "mid CA
// rotation ... before starting a new rotation"). The resumed run proceeds
// past the state guards and halts where this fixture cannot continue — the
// staged material cannot be re-published through the absent admin.conf —
// with the halt guidance naming the resuming command.
func TestRotateCAClusterResumesItsOwnStagedRotation(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf") // the run halts at the re-publish, before any node Job
	cs := fakeAdminClientset(t, thisCPNode(t)...)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	before := readStagedCARotation(t)

	err := RotateCACluster(CARotateOptions{Quiet: true})
	if err == nil {
		t.Fatal("the fixture run should halt once the cluster material cannot be re-published")
	}
	for _, refused := range []string{"already staged", "before starting a new one", "before starting a new rotation"} {
		if strings.Contains(err.Error(), refused) {
			t.Errorf("the re-run was refused as a fresh rotation (%q in %v)", refused, err)
		}
	}
	if !strings.Contains(err.Error(), "rotate-ca --cluster") {
		t.Errorf("the halt guidance does not name the resuming command: %v", err)
	}
	assertCARotationUnchanged(t, before)
	assertNoNodeJob(t, cs)
	if _, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the halted run left the transient key Secret behind: %v", err)
	}
}

// TestRotateCAClusterResumesAStagingItsRecordVouchesFor is the entry check's
// side of TestAnInitiatorStagingStoppedBeforeItsBundlesResumes: a staging
// whose widened bundles were never written is resumed, not refused, when an
// unfinished shared record with no phase completed names the run that left
// it. The run proceeds past the entry check and the lease, adopts that
// record, and halts in the initiator's trust-new where this fixture cannot
// continue (it is no control plane, so the node path reads it as a worker).
func TestRotateCAClusterResumesAStagingItsRecordVouchesFor(t *testing.T) {
	prepPinned(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf")
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: caRotationStateName, Namespace: caNamespace},
		Data:       map[string]string{"run": "run-1"},
	}
	cs := fakeAdminClientset(t, append(thisCPNode(t), record)...)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), live, 0o644); err != nil {
		t.Fatal(err)
	}

	err = RotateCACluster(CARotateOptions{Quiet: true})
	if err == nil {
		t.Fatal("the fixture run should halt once the cluster material cannot be re-published")
	}
	if strings.Contains(err.Error(), "already staged") {
		t.Errorf("the re-run of the run that left the staging was refused as a rotation someone else left: %v", err)
	}
	if !strings.Contains(err.Error(), "initiator "+PhaseTrustNew) {
		t.Errorf("the run did not reach the initiator's trust-new: %v", err)
	}
	cm, err := cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
	if err != nil || cm.Data["run"] != "run-1" {
		t.Errorf("the run did not resume the recorded rotation: %v, %v", cm, err)
	}
	assertNoNodeJob(t, cs)
}

// A manual phase sequence must be able to start another rotation after finalize.
func TestBareTrustNewAfterCompletedRotation(t *testing.T) {
	f := newFakeCACluster(t, "cp1", "cp2")
	if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
		t.Fatal(err)
	}
	for _, node := range f.order {
		f.in(node, func() {
			if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}); err != nil {
				t.Fatal(err)
			}
			if !pki.CARotationStaged() {
				t.Errorf("%s: bare trust-new skipped a fresh rotation", node)
			}
		})
	}
}

func TestRotationCompletionFailureResumesFromAnyControlPlane(t *testing.T) {
	f := newFakeCACluster(t, "cp1", "cp2", "cp3")
	f.cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		cm := a.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if cm.Data["complete"] == "true" {
			return true, nil, errors.New("completion unavailable")
		}
		return false, nil, nil
	})
	if err := (&caCluster{drv: f}).run(); err == nil {
		t.Fatal("completion failure reported success")
	}
	// Simulate a failed key cleanup as well as the failed completion write.
	if err := putStagedKey(context.Background(), f.cs, "leftover-key", rotationStagedKeyOwner(f.rotationRun)); err != nil {
		t.Fatal(err)
	}
	cleanedBefore := len(f.cleaned)
	root := f.caCRT(t, "cp1")
	f.cs.ReactionChain = f.cs.ReactionChain[1:]
	f.order[0], f.order[1] = f.order[1], f.order[0]
	f.skips = nil
	// Every node had finalized, so the retry records the completion and
	// refuses rather than report a rotation it did not make.
	if err := (&caCluster{drv: f}).run(); err == nil || !strings.Contains(err.Error(), "had already finished on every node") {
		t.Fatalf("completion retry = %v, want the finished rotation recorded and reported", err)
	}
	if string(root) != string(f.caCRT(t, "cp1")) {
		t.Fatal("completion retry started a new rotation")
	}
	if len(f.cleaned) != cleanedBefore+1 {
		t.Fatal("completion retry did not retry staged-key cleanup")
	}
	if _, err := f.cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("completion retry retained its leftover key: %v", err)
	}
	assertFinalizedOnOneRoot(t, f)
}

// TestByHandFinishedRotationIsNotReportedAsANewRotation: the shared record is
// marked complete only at the end of a --cluster run, so a rotation finished
// another way — the finalize halt's by-hand finish, or a run whose completion
// write failed after every node finalized — leaves it unfinished, and the next
// `rotate-ca --cluster` adopts it, skips every phase it recorded and re-runs
// finalize. That run stages no root. It must not say the cluster is now on a
// new CA as if it had rotated, and when the record itself shows every node
// finalized it records the completion and refuses, so the operator who wanted
// a new rotation runs the command again and gets one.
func TestByHandFinishedRotationIsNotReportedAsANewRotation(t *testing.T) {
	t.Run("finished by hand after a finalize halt", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp2:"+PhaseFinalize] = true
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
			t.Fatal("the run completed although cp2 failed finalize")
		}
		delete(f.failAt, "cp2:"+PhaseFinalize)
		// The halt's by-hand finish on each node that has not finalized: what
		// `--phase=finalize` leaves, the run it finished recorded.
		for _, n := range []string{"cp2", "cp3"} {
			f.in(n, func() {
				if err := pki.FinalizeCA(); err != nil {
					t.Fatal(err)
				}
				markCarotateFinishedRun(io.Discard, f.rotationRun)
			})
		}
		root := f.caCRT(t, "cp1")

		var log []string
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}, log: func(m string) { log = append(log, m) }}).run(); err != nil {
			t.Fatalf("the next run: %v", err)
		}
		if string(f.caCRT(t, "cp1")) != string(root) {
			t.Fatal("fixture: the next run rotated the root")
		}
		out := strings.Join(log, "\n")
		if strings.Contains(out, "now on the new CA") {
			t.Errorf("a run that staged no root reported the cluster now on a new CA:\n%s", out)
		}
		if !strings.Contains(out, "rotate-ca --cluster --yes` again") {
			t.Errorf("the run does not say how to start a new rotation:\n%s", out)
		}
	})
	t.Run("a completion write that failed after every node finalized", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2")
		f.cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap).Data["complete"] == "true" {
				return true, nil, errors.New("completion unavailable")
			}
			return false, nil, nil
		})
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
			t.Fatal("completion failure reported success")
		}
		f.cs.ReactionChain = f.cs.ReactionChain[1:]
		root := f.caCRT(t, "cp1")

		var log []string
		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}, log: func(m string) { log = append(log, m) }}).run()
		if err == nil || !strings.Contains(err.Error(), "had already finished on every node") || !strings.Contains(err.Error(), "rotate-ca --cluster --yes` again") {
			t.Fatalf("the next run = %v, want it to record the finished rotation and refuse, naming the run that rotates", err)
		}
		if out := strings.Join(log, "\n"); strings.Contains(out, "now on the new CA") {
			t.Errorf("a run that staged no root reported the cluster now on a new CA:\n%s", out)
		}
		cm, gerr := f.cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
		if gerr != nil || cm.Data["complete"] != "true" {
			t.Errorf("the finished rotation was not recorded complete: %v, %v", cm, gerr)
		}
		if string(f.caCRT(t, "cp1")) != string(root) {
			t.Error("the refused run rotated the root")
		}
		// The command it names rotates.
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the run after the refusal: %v", err)
		}
		if string(f.caCRT(t, "cp1")) == string(root) {
			t.Error("the run after the refusal did not rotate the root")
		}
		assertFinalizedOnOneRoot(t, f)
	})
}

// unhealthyCluster is the fake cluster whose finalize preflight's health arm
// refuses while err is set.
type unhealthyCluster struct {
	*fakeCACluster
	err error
}

func (f *unhealthyCluster) ClusterHealthy(nodes []cnode) error {
	if f.err != nil {
		return f.err
	}
	return f.fakeCACluster.ClusterHealthy(nodes)
}

// TestAResumeAfterAPartialFinalizeHaltsWithFinalizeGuidance: the record's
// phase is the last one EVERY node completed, so a run that halted inside
// finalize still reads sign-new, though some nodes have finalized and dropped
// their old CA. A resume that then stops before the initiator's finalize —
// the finalize preflight refusing (the node whose finalize failed is the
// likeliest to be NotReady), or an interrupt — must carry finalize guidance:
// sign-new's "roll back every node" restores the old root only on the nodes
// that never finalized, which have backups, and splits the cluster across two
// roots. The resume may run from any control plane, so the guidance cannot
// depend on whether the initiator itself finalized.
func TestAResumeAfterAPartialFinalizeHaltsWithFinalizeGuidance(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(f *unhealthyCluster)
	}{
		{"the finalize preflight refuses", func(f *unhealthyCluster) { f.err = errors.New("node cp3 is not Ready") }},
		{"an interrupt before the initiator's finalize", func(f *unhealthyCluster) { f.interrupt = context.Canceled }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &unhealthyCluster{fakeCACluster: newFakeCACluster(t, "cp1", "cp2", "cp3")}
			f.failAt["cp3:"+PhaseFinalize] = true
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
				t.Fatal("the run completed although cp3 failed finalize")
			}
			delete(f.failAt, "cp3:"+PhaseFinalize)
			f.in("cp1", func() {
				if !pki.CAFinalized() {
					t.Fatal("fixture: the initiator did not finalize")
				}
			})
			// The resume runs from a control plane that never finalized.
			f.order[0], f.order[2] = f.order[2], f.order[0]
			tc.stop(f)
			err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
			if err == nil {
				t.Fatal("the resume completed")
			}
			if !strings.Contains(err.Error(), "Do NOT roll back a node that already finalized") {
				t.Errorf("halt is missing the finalize guidance: %v", err)
			}
			for _, wrong := range []string{"--rollback --yes` on every node", "safe old‖new", "halted in " + PhaseSignNew} {
				if strings.Contains(err.Error(), wrong) {
					t.Errorf("halt says %q, which splits a cluster some of whose nodes finalized: %v", wrong, err)
				}
			}
			f.err, f.interrupt = nil, nil
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
				t.Fatalf("the resume after the stop: %v", err)
			}
			assertFinalizedOnOneRoot(t, f.fakeCACluster)
		})
	}
}

func TestResolveRotationFailsClosed(t *testing.T) {
	for _, verb := range []string{"get", "create"} {
		t.Run(verb, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.PrependReactor(verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
			d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: cs}
			if _, _, err := d.ResolveRotation(); err == nil {
				t.Fatal("unavailable rotation record allowed a new run")
			}
		})
	}
}

func TestResumeDoesNotRedistributePromotedMaterial(t *testing.T) {
	f := newFakeCACluster(t, "cp1", "cp2", "cp3")
	f.failAt["cp3:sign-new"] = true
	if err := (&caCluster{drv: f}).run(); err == nil {
		t.Fatal("expected halt")
	}
	f.failAt["cp3:sign-new"] = false
	f.calls = nil
	if err := (&caCluster{drv: f}).run(); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.HasSuffix(call, ":trust-new") {
			t.Errorf("resume redispatched %s after every node completed trust-new", call)
		}
	}
	if f.published != 1 {
		t.Errorf("published %d bundles, want no promoted material redistribution", f.published)
	}
}

// restartClientsFailsOnce is the fake cluster whose in-cluster client
// rollout fails the first time: the halt after trust-new, before the run
// records trust-new complete.
type restartClientsFailsOnce struct {
	*fakeCACluster
	failed bool
}

func (f *restartClientsFailsOnce) RestartClients() error {
	if !f.failed {
		f.failed = true
		return errors.New("rollout of the in-cluster clients timed out")
	}
	return f.fakeCACluster.RestartClients()
}

// trustsRoot reports whether node's live root and trust bundle still carry
// root.
func (f *fakeCACluster) trustsRoot(t *testing.T, node string, root []byte) (live, bundled bool) {
	t.Helper()
	want, err := pki.DecodePEMBundle(root, pki.CountPolicy)
	if err != nil || len(want) != 1 {
		t.Fatalf("parse the root: %d cert(s), %v", len(want), err)
	}
	f.in(node, func() {
		live = string(f.caCRT(t, node)) == string(root)
		b, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-bundle.crt"))
		if err != nil {
			t.Fatal(err)
		}
		certs, err := pki.DecodePEMBundle(b, pki.CountPolicy)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range certs {
			bundled = bundled || c.Equal(want[0])
		}
	})
	return live, bundled
}

// TestResumeDoesNotPublishAPromotedInitiatorsSigner: a run that halts after
// trust-new but before it records the phase offers a by-hand sign-new, and an
// operator who starts it on the initiator and then re-runs `rotate-ca
// --cluster` resumes with trust-new unrecorded. The initiator skips its
// trust-new as applied, so its live signers ARE the rotation's new ones and
// nothing is staged; publishing them as the staged material hands every
// follower still at trust-new the new root as its old one, and its trust-new
// then writes new‖new bundles — the old root gone from a node whose leaves it
// signed, with no backup to roll back to. The resume must refuse before it
// publishes, and the way out it names must finish the rotation.
func TestResumeDoesNotPublishAPromotedInitiatorsSigner(t *testing.T) {
	f := &restartClientsFailsOnce{fakeCACluster: newFakeCACluster(t, "cp1", "cp2", "cp3")}
	oldRoot := f.caCRT(t, "cp1")
	err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
	if err == nil || !strings.Contains(err.Error(), "halted after trust-new") {
		t.Fatalf("first run = %v, want the halt after trust-new", err)
	}
	f.in("cp1", func() {
		if err := cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true}); err != nil {
			t.Fatalf("the by-hand sign-new on the initiator: %v", err)
		}
	})

	// A follower's sign-new stops a resume that did publish, so what its
	// trust-new left behind is what the checks below read.
	f.failAt["cp2:"+PhaseSignNew] = true
	published := f.published
	err = (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
	delete(f.failAt, "cp2:"+PhaseSignNew)
	if f.published != published {
		t.Errorf("the resume published the promoted initiator's material as staged (%d publish(es), want %d)", f.published, published)
	}
	for _, n := range []string{"cp2", "cp3"} {
		if live, bundled := f.trustsRoot(t, n, oldRoot); !live || !bundled {
			t.Errorf("%s: live root is the old one = %v, trust bundle carries it = %v; want both", n, live, bundled)
		}
	}
	if err == nil || strings.Contains(err.Error(), "node cp2 is unreachable") {
		t.Fatalf("the resume from the promoted initiator = %v, want it refused before it publishes", err)
	}
	for _, want := range []string{"rotate-ca --cluster --yes", "has not promoted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}

	// The way out it names: the same command from a control plane still at
	// trust-new.
	f.order[0], f.order[1] = f.order[1], f.order[0]
	if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
		t.Fatalf("the resume from a control plane that has not promoted: %v", err)
	}
	assertFinalizedOnOneRoot(t, f.fakeCACluster)
}

// TestAnInitiatorStagingStoppedBeforeItsBundlesResumes: the initiator's
// RotateCAMaterial stages the new root before it writes the widened bundles,
// so a write that fails in between — a full disk, a read-only remount, here a
// directory where a staged file goes — leaves it staged with a one-root
// bundle. The halt says to re-run `rotate-ca --cluster`; the re-run of that
// same run must resume the staging (RotateCAMaterial reuses what is staged),
// not refuse it as a rotation someone else left behind. A staging no
// unfinished record vouches for is still refused, with the node's own way out
// and not wrapped in re-run guidance that would only be refused again.
func TestAnInitiatorStagingStoppedBeforeItsBundlesResumes(t *testing.T) {
	t.Run("the run that stopped resumes it", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		planted := filepath.Join(f.dirs["cp1"], "front-proxy-ca-new.crt")
		if err := os.Mkdir(planted, 0o700); err != nil {
			t.Fatal(err)
		}
		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil || !strings.Contains(err.Error(), "halted in "+PhaseTrustNew) {
			t.Fatalf("first run = %v, want the initiator's trust-new halt", err)
		}
		if err := os.Remove(planted); err != nil {
			t.Fatal(err)
		}
		f.in("cp1", func() {
			if rot := pki.ReadCARotation(); !rot.Staged || rot.BundleCAs != 1 || rot.Err != nil {
				t.Fatalf("fixture: cp1 = %+v, want staged with the one-root bundle", rot)
			}
		})
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the re-run the halt names: %v", err)
		}
		assertFinalizedOnOneRoot(t, f)
	})
	t.Run("a staging no record vouches for is refused with its own way out", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2")
		f.in("cp1", func() {
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			live, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), live, 0o644); err != nil {
				t.Fatal(err)
			}
		})
		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil || !strings.Contains(err.Error(), "already staged on") {
			t.Fatalf("run = %v, want the node's staged-rotation refusal", err)
		}
		if strings.Contains(err.Error(), "Re-run `stratum certs rotate-ca --cluster --yes`") {
			t.Errorf("the refusal is wrapped in re-run guidance, and the re-run refuses the same way: %v", err)
		}
	})
}

func TestCompletedRotationDoesNotAdoptALeftoverKey(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: caRotationStateName, Namespace: caNamespace},
		Data:       map[string]string{"run": "finished-run", "complete": "true"},
	}, stagedKeySecret("leftover-key", rotationStagedKeyOwner("finished-run")))
	d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: cs}
	run, resumed, err := d.ResolveRotation()
	if err != nil {
		t.Fatal(err)
	}
	if resumed || run == "finished-run" {
		t.Fatalf("adopted completed identity %q (resumed=%v)", run, resumed)
	}
}

func TestRolledBackClusterExplainsSharedCheckpointCleanup(t *testing.T) {
	f := newFakeCACluster(t, "cp1", "cp2")
	f.failAt["cp2:sign-new"] = true
	if err := (&caCluster{drv: f}).run(); err == nil {
		t.Fatal("expected sign-new halt")
	}
	for _, node := range f.order {
		f.in(node, func() {
			if err := pki.RollbackCA(); err != nil {
				t.Fatal(err)
			}
			if err := pki.RotateLeaves(node, f.ips[node], nil); err != nil {
				t.Fatal(err)
			}
			if err := pki.RemoveRotationScratch(); err != nil {
				t.Fatal(err)
			}
		})
	}
	_, _, err := f.ResolveRotation()
	if err == nil || !strings.Contains(err.Error(), "after rollback succeeds on every control plane and worker") || !strings.Contains(err.Error(), caRotationStateName) {
		t.Fatalf("missing safe full-rollback cleanup guidance: %v", err)
	}
}

func TestFinalizedManualTrustNewRefusesAnUnreadableRun(t *testing.T) {
	f := newFakeCACluster(t, "cp1")
	if err := (&caCluster{drv: f}).run(); err != nil {
		t.Fatal(err)
	}
	before := f.caCRT(t, "cp1")
	f.in("cp1", func() {
		saved := stagedRotationRunID
		defer func() { stagedRotationRunID = saved }()
		boom := errors.New("cannot read shared run")
		stagedRotationRunID = func() (string, error) { return "", boom }
		if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}); !errors.Is(err, boom) {
			t.Fatalf("unreadable run did not refuse: %v", err)
		}
		if pki.CARotationStaged() {
			t.Fatal("unreadable run staged a divergent signer")
		}
	})
	if string(f.caCRT(t, "cp1")) != string(before) {
		t.Fatal("unreadable run changed root")
	}
}

// TestRotateCACluster_RefusesAPhase pins that the cluster orchestrator refuses
// a per-node phase before it builds its admin client, so the refusal cannot be
// moved below the client construction: a `--cluster` run handed a phase must
// error naming the phase, not fail on the kubeconfig first (or worse, run all
// three phases when it was given one). admin.conf points at a missing file, so
// the call cannot reach a cluster and today's "load admin kubeconfig" answer is
// the drift this test fails on.
func TestRotateCACluster_RefusesAPhase(t *testing.T) {
	oldConf := version.AdminConf
	t.Cleanup(func() { version.AdminConf = oldConf })
	version.AdminConf = filepath.Join(t.TempDir(), "absent-admin.conf")

	for name, call := range map[string]func() error{
		"RotateCACluster": func() error { return RotateCACluster(CARotateOptions{Phase: PhaseSignNew}) },
		"RotateCAClusterContext": func() error {
			// The builder must not be consulted before the refusal: a --cluster
			// run handed a phase is refused before any client is built.
			refuse := func() (kubernetes.Interface, error) {
				return nil, errors.New("the admin client must be built after the phase refusal")
			}
			return RotateCAClusterContext(context.Background(), io.Discard, CARotateOptions{Phase: PhaseSignNew}, refuse)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("a cluster rotation accepted a per-node phase")
			}
			if strings.Contains(err.Error(), "load admin kubeconfig") {
				t.Fatalf("the admin client was built before the phase was read: %v", err)
			}
			if !strings.Contains(err.Error(), PhaseSignNew) {
				t.Errorf("the refusal does not name the phase %q: %v", PhaseSignNew, err)
			}
		})
	}
}

// recordingBuilder is a client-builder stub for the cluster CA orchestrators.
// It records every call, so a test can see how many clients a run built and
// hand each acquisition point a client of its own.
type recordingBuilder struct {
	calls int
	cs    kubernetes.Interface
	err   error
}

func (b *recordingBuilder) build() (kubernetes.Interface, error) {
	b.calls++
	return b.cs, b.err
}

// TestClusterCAOrchestrators_BuildTheirClientsThroughTheInjectedBuilder pins
// the two --cluster CA orchestrators: RotateCAClusterContext and
// MigrateEtcdCAClusterContext take a client-builder function and acquire their
// API clients ONLY through it. A CA rotation acquires a client at three points
// — the seam at entry, and the two mid-run rebuilds after trust-new and after
// sign-new, where the admin certificate has been reissued under the new CA —
// and all three must be served by the one builder the caller injects, so a
// rebuild cannot silently fall back to admin.conf. A fixture run cannot
// complete trust-new on a test host (the initiator's own phase restarts this
// node's static pods), so the two rebuild sites are covered by the same trap as
// the seam: the stub records each call, the run is driven as deep as a fixture
// reaches, and version.AdminConf — pointed at a missing file throughout — is
// never read. A direct k8sutil.AdminClientset anywhere on the reachable path
// fails with "load admin kubeconfig", which every subtest refuses.
func TestClusterCAOrchestrators_BuildTheirClientsThroughTheInjectedBuilder(t *testing.T) {
	injected := errors.New("injected: the apiserver is unreachable")

	t.Run("the rotation builds its first client through the injected builder", func(t *testing.T) {
		prep(t)
		redirectVar(t, &version.AdminConf, "missing-admin.conf")
		cs := fake.NewSimpleClientset(thisCPNode(t)...)
		cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, injected
		})
		b := &recordingBuilder{cs: cs}
		err := RotateCAClusterContext(context.Background(), io.Discard, CARotateOptions{Quiet: true}, b.build)
		if err == nil || !strings.Contains(err.Error(), "injected: the apiserver is unreachable") {
			t.Fatalf("RotateCAClusterContext = %v; want the stub client's first API error", err)
		}
		if b.calls != 1 {
			t.Fatalf("the builder was called %d time(s) before the run stopped; the run's first client must come from the injected builder alone", b.calls)
		}
	})

	t.Run("the migration builds its first client through the injected builder", func(t *testing.T) {
		prep(t)
		redirectVar(t, &version.AdminConf, "missing-admin.conf")
		cs := fake.NewSimpleClientset(thisCPNode(t)...)
		cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, injected
		})
		b := &recordingBuilder{cs: cs}
		err := MigrateEtcdCAClusterContext(context.Background(), io.Discard, EtcdCAOptions{Quiet: true}, b.build)
		if err == nil || !strings.Contains(err.Error(), "injected: the apiserver is unreachable") {
			t.Fatalf("MigrateEtcdCAClusterContext = %v; want the stub client's first API error", err)
		}
		if b.calls != 1 {
			t.Fatalf("the builder was called %d time(s) before the run stopped; the run's first client must come from the injected builder alone", b.calls)
		}
	})

	t.Run("the rotation's reachable run builds only through the injected builder", func(t *testing.T) {
		prep(t)
		redirectVar(t, &version.AdminConf, "missing-admin.conf")
		b := &recordingBuilder{cs: fake.NewSimpleClientset(thisCPNode(t)...)}
		err := RotateCAClusterContext(context.Background(), io.Discard, CARotateOptions{Quiet: true}, b.build)
		if err == nil {
			t.Fatal("the fixture run should halt at the initiator's own phase (this node runs no kubelet)")
		}
		if strings.Contains(err.Error(), "load admin kubeconfig") {
			t.Fatalf("the run built a client from admin.conf instead of the injected builder: %v", err)
		}
		if strings.Contains(err.Error(), "in progress") {
			t.Fatalf("the run refused although the fixture cluster was free: %v", err)
		}
		if b.calls != 1 {
			t.Fatalf("the builder was called %d time(s); on the reachable path the run must build no client anywhere else", b.calls)
		}
	})

	t.Run("the migration's reachable run builds only through the injected builder", func(t *testing.T) {
		prep(t)
		redirectVar(t, &version.AdminConf, "missing-admin.conf")
		redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
		b := &recordingBuilder{cs: fake.NewSimpleClientset(thisCPNode(t)...)}
		err := MigrateEtcdCAClusterContext(context.Background(), io.Discard, EtcdCAOptions{Quiet: true}, b.build)
		if err == nil || !strings.Contains(err.Error(), "InternalIP") {
			t.Fatalf("the fixture run should halt at the node roster (the fixture control planes carry no InternalIP): %v", err)
		}
		if strings.Contains(err.Error(), "load admin kubeconfig") {
			t.Fatalf("the run built a client from admin.conf instead of the injected builder: %v", err)
		}
		if b.calls != 1 {
			t.Fatalf("the builder was called %d time(s); on the reachable path the run must build no client anywhere else", b.calls)
		}
	})
}

func TestFollowerTrustNewNeverMintsItsOwnCA(t *testing.T) {
	f := newFakeCACluster(t, "cp1", "cp2")
	f.in("cp1", func() {
		if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}); err != nil {
			t.Fatal(err)
		}
	})
	staged := map[string][]byte{}
	f.in("cp1", func() {
		for _, name := range pki.CAStagedFiles() {
			b, err := os.ReadFile(filepath.Join(version.PKIDir, name))
			if err != nil {
				t.Fatal(err)
			}
			staged[name] = b
		}
	})
	for _, name := range []string{"ca-new.key", "kubelet-client-ca-new.key"} {
		if staged[name] == nil {
			t.Fatalf("the initiator staged no %s: the fixture does not cover this test", name)
		}
	}
	old := fetchStagedMaterial
	t.Cleanup(func() { fetchStagedMaterial = old })
	fetchWithout := func(drop string) {
		fetchStagedMaterial = func() error {
			for name, b := range staged {
				if name == drop {
					continue
				}
				if err := os.WriteFile(filepath.Join(version.PKIDir, name), b, 0o600); err != nil {
					return err
				}
			}
			return nil
		}
	}
	f.in("cp2", func() {
		for _, drop := range []string{"ca-new.key", "front-proxy-ca-new.key", "sa-new.key", "kubelet-client-ca-new.key"} {
			_ = os.Remove(filepath.Join(version.PKIDir, drop))
			fetchWithout(drop)
			err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true, FetchShared: true})
			if err == nil || !strings.Contains(err.Error(), drop) {
				t.Fatalf("trust-new with the initiator's %s missing = %v, want a refusal naming it", drop, err)
			}
			if fileExists(filepath.Join(version.PKIDir, drop)) {
				t.Fatalf("the follower minted a %s of its own", drop)
			}
		}

		fetchWithout("")
		if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true, FetchShared: true}); err != nil {
			t.Fatalf("trust-new with the initiator's full staged material: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-new.crt"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, staged["ca-new.crt"]) {
			t.Error("the follower staged a different new root CA from the initiator's")
		}
	})
}
