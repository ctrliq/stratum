package certs

// version.PKIDir is global, so none of these may run in parallel.

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

// one real PKI directory per node; the follower fetch stands in for ha.DownloadCertsAdmin.
type fakeCACluster struct {
	t     *testing.T
	cs    *fake.Clientset
	order []string
	ips   map[string]string
	dirs  map[string]string
	bases map[string]string
	calls []string
	skips []string

	failAt map[string]bool

	published    int
	publishedIDs []string
	cleaned      []string
	stagedKey    bool
	rotationRun  string
	rebuilt      int
	rolled       int

	// the shared material the last publish handed the followers.
	publishedFiles map[string][]byte
	resolved       *prodCADriver

	interrupt error
	// the dispatch completes; the initiator's phase does not observe ctx.
	interruptAfter map[string]bool
}

// per-node fixtures mirror a real cluster: byte-identical signers, per-node leaves.
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
	// no CIS bootstrap node here, so finalize never owes a kubelet serving-cert renewal.
	savedBootstraps := kubeletBootstrapsServing
	kubeletBootstrapsServing = func() (bool, error) { return false, nil }
	t.Cleanup(func() { kubeletBootstrapsServing = savedBootstraps })

	// the fake has no apiserver for the /healthz gate before the leftover-key reap.
	savedWait := waitAPIServer
	waitAPIServer = func(context.Context, kubernetes.Interface) error { return nil }
	t.Cleanup(func() { waitAPIServer = savedWait })

	// the stand-in reports the run the fake has staged and not yet cleaned.
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
			// a joining CP holds the cluster's signers; GenerateAll reuses them, minting only leaves.
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
	version.NetConfigPath = filepath.Join(f.dirs[node], "netconfig")
	version.ControlSocket = filepath.Join(f.bases[node], "stratum.sock")
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
		// the initiator carries the run id; a follower scopes by the staged key the cluster holds.
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
			// the follower receives the initiator's staged material, so every node promotes the same root.
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

// the fixture's stand-in for the node Job's per-node phase; restarts deliberately absent.
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
		// mirrors cpFinalize's finished-run record, which the applied-phase scoping reads back.
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

func (f *fakeCACluster) callsFor(phase string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasSuffix(c, ":"+phase) {
			out = append(out, strings.TrimSuffix(c, ":"+phase))
		}
	}
	return out
}

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

// after the initiator finalized, rollback guidance would split the cluster across two roots.
func TestInterruptMidPhaseNamesThePhaseInProgress(t *testing.T) {
	for _, tc := range []struct {
		after   string
		want    []string
		wantNot []string
	}{
		{"cp1:trust-new", []string{"halted in " + PhaseTrustNew, "rotate-ca --cluster --yes"}, nil},
		{"cp1:sign-new", []string{"halted in " + PhaseSignNew, "rotate-ca --cluster --yes"}, []string{"halted in " + PhaseTrustNew}},
		{"cp1:finalize", []string{"halted in " + PhaseFinalize, "Do NOT roll back a node that already finalized", "rotate-ca --cluster --yes"},
			[]string{"--rollback --yes` on every node", "halted in " + PhaseSignNew}},
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

func TestRotateCAClusterResumesAfterAHalt(t *testing.T) {
	t.Run("a halt in sign-new resumes and finishes", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp3:sign-new"] = true

		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil {
			t.Fatal("the run completed although cp3 failed sign-new")
		}
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
		// the halt leaves cp1 and cp2 past sign-new and cp3 still only trusting old‖new.
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

		// The resume: the same command, no flags added.
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
		// trust-new past a promoted signer stages a divergent root; the refusals halt to prevent it.
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
		// the resume stages its own key in the kept one's place and removes its own on completion.
		if f.published != 2 || len(f.cleaned) != 1 || f.cleaned[0] != f.publishedIDs[1] {
			t.Errorf("the two runs published %v and cleaned %v, want one publish per run and only the resumed run's cleanup", f.publishedIDs, f.cleaned)
		}
	})

	// unscoped finalize markers would send a resume back through trust-new past dropped old CAs.
	t.Run("a halt inside finalize resumes without re-running the finished phases", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp3:finalize"] = true

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

func TestHaltedRotationKeepsTheKeyForUnfinishedFollowers(t *testing.T) {
	t.Run("a halt in trust-new keeps the staged key and says so", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp2:trust-new"] = true

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
		// the resume stages its own key in the kept one's place and removes its own on completion.
		if f.published != 2 || len(f.cleaned) != 1 || f.cleaned[0] != f.publishedIDs[1] {
			t.Errorf("published %v, cleaned %v, want one publish per run and only the resumed run's cleanup", f.publishedIDs, f.cleaned)
		}
	})

	t.Run("a halt after trust-new removes the run's own key", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp3:sign-new"] = true

		err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
		if err == nil {
			t.Fatal("the run completed although cp3 failed sign-new")
		}
		if f.published != 1 || len(f.cleaned) != 1 || f.cleaned[0] != f.publishedIDs[0] {
			t.Errorf("published %v, cleaned %v, want the sign-new halt to remove the run's own key", f.publishedIDs, f.cleaned)
		}
	})
}

// an unscoped applied decision makes the second run a silent no-op; it is scoped per rotation.
func TestSecondRotationRestagesAndChangesTheRoot(t *testing.T) {
	for _, next := range []string{"cp1", "cp2", "cp3"} {
		t.Run(next, func(t *testing.T) {
			f := newFakeCACluster(t, "cp1", "cp2", "cp3")
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
				t.Fatalf("the first rotation: %v", err)
			}
			assertFinalizedOnOneRoot(t, f)
			firstRoot := f.caCRT(t, f.initiator())

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

// a re-run of its own halted rotation must not be refused as a fresh rotation.
func TestRotateCAClusterResumesItsOwnStagedRotation(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf")
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

// an unfinished record with no completed phase vouches for the staging on this node.
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
	if err := putStagedKey(context.Background(), f.cs, "leftover-key", rotationStagedKeyOwner(f.rotationRun)); err != nil {
		t.Fatal(err)
	}
	cleanedBefore := len(f.cleaned)
	root := f.caCRT(t, "cp1")
	f.cs.ReactionChain = f.cs.ReactionChain[1:]
	f.order[0], f.order[1] = f.order[1], f.order[0]
	f.skips = nil
	// the retry records the completion and refuses rather than report a rotation it did not make.
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

// a by-hand finish leaves the record unfinished; the next run must not claim a new rotation.
func TestByHandFinishedRotationIsNotReportedAsANewRotation(t *testing.T) {
	t.Run("finished by hand after a finalize halt", func(t *testing.T) {
		f := newFakeCACluster(t, "cp1", "cp2", "cp3")
		f.failAt["cp2:"+PhaseFinalize] = true
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
			t.Fatal("the run completed although cp2 failed finalize")
		}
		delete(f.failAt, "cp2:"+PhaseFinalize)
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
		if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
			t.Fatalf("the run after the refusal: %v", err)
		}
		if string(f.caCRT(t, "cp1")) == string(root) {
			t.Error("the run after the refusal did not rotate the root")
		}
		assertFinalizedOnOneRoot(t, f)
	})
}

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

// sign-new's rollback guidance would split a cluster whose nodes finalized; carry finalize's.
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

// publishing a promoted initiator's live signers hands followers the new root as their old one.
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

	// a follower's failed sign-new stops a resume that did publish, keeping the state readable.
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

	f.order[0], f.order[1] = f.order[1], f.order[0]
	if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
		t.Fatalf("the resume from a control plane that has not promoted: %v", err)
	}
	assertFinalizedOnOneRoot(t, f.fakeCACluster)
}

// RotateCAMaterial stages the root before the widened bundles; the run's own re-run must resume it.
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

// the phase refusal must precede the client build, so it cannot read like a kubeconfig fault.
func TestRotateCACluster_RefusesAPhase(t *testing.T) {
	oldConf := version.AdminConf
	t.Cleanup(func() { version.AdminConf = oldConf })
	version.AdminConf = filepath.Join(t.TempDir(), "absent-admin.conf")

	for name, call := range map[string]func() error{
		"RotateCACluster": func() error { return RotateCACluster(CARotateOptions{Phase: PhaseSignNew}) },
		"RotateCAClusterContext": func() error {
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

type recordingBuilder struct {
	calls int
	cs    kubernetes.Interface
	err   error
}

func (b *recordingBuilder) build() (kubernetes.Interface, error) {
	b.calls++
	return b.cs, b.err
}

// a rebuild must not silently fall back to admin.conf; all clients ride the injected builder.
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
