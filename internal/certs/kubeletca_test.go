package certs

import (
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// kubeletCAPKI points version.PKIDir at a control plane installed before the
// kubelet-client CA, built by rolling a fresh install back to the root.
func kubeletCAPKI(t *testing.T) {
	t.Helper()
	saved := version.PKIDir
	t.Cleanup(func() { version.PKIDir = saved })
	version.PKIDir = t.TempDir()
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		pki.KubeletClientReAddRoot,
		func() error { return pki.KubeletClientReissueRoot("cp1", "10.0.0.5", nil) },
		pki.KubeletClientRemoveCA,
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	wantKubeletState(t, pki.KubeletStateNotMigrated)
}

// kubeletCAWorkerPKI points version.PKIDir at a worker whose kubelet follows
// the root in ca-bundle.crt.
func kubeletCAWorkerPKI(t *testing.T, root *testCA) {
	t.Helper()
	saved := version.PKIDir
	t.Cleanup(func() { version.PKIDir = saved })
	version.PKIDir = t.TempDir()
	for _, f := range []string{"ca-bundle.crt", pki.KubeletClientBundleBase + "-bundle.crt"} {
		if err := os.WriteFile(filepath.Join(version.PKIDir, f), bundle(root), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func wantKubeletState(t *testing.T, want string) {
	t.Helper()
	tr, err := pki.ReadKubeletClientTrust()
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.State(); got != want {
		t.Fatalf("state = %s, want %s", got, want)
	}
}

type kubeletOpsLog struct{ calls []string }

func (l *kubeletOpsLog) ops(cp bool, published *x509.Certificate) kubeletNodeOps {
	rec := func(name string) func() error {
		return func() error { l.calls = append(l.calls, name); return nil }
	}
	return kubeletNodeOps{
		controlPlane: func() bool { return cp },
		ident:        func() (nodeIdent, error) { return nodeIdent{"cp1", "10.0.0.5", nil}, nil },
		fetchShared:  rec("fetch-shared"),
		fetchPublished: func() (*x509.Certificate, error) {
			l.calls = append(l.calls, "fetch-published")
			if published == nil {
				return nil, errors.New("nothing published")
			}
			return published, nil
		},
		restartKubelet:   rec("restart kubelet"),
		restartAPIServer: rec("restart kube-apiserver"),
		republish:        rec("republish"),
	}
}

// Every forward phase on a control plane restarts what it changed, and the
// trust moves republish the bundle the workers fetch.
func TestKubeletCAPhasesOnAControlPlane(t *testing.T) {
	kubeletCAPKI(t)
	var log kubeletOpsLog
	for _, tc := range []struct {
		phase, state string
		calls        []string
	}{
		{pki.KubeletPhaseTrustBoth, pki.KubeletStateTrustBoth, []string{"restart kubelet", "republish"}},
		{pki.KubeletPhaseIssueNew, pki.KubeletStateIssueNew, []string{"restart kube-apiserver"}},
		{pki.KubeletPhaseDropRoot, pki.KubeletStateMigrated, []string{"restart kubelet", "republish"}},
		{pki.KubeletPhaseReAddRoot, pki.KubeletStateIssueNew, []string{"restart kubelet", "republish"}},
		{pki.KubeletPhaseReissueRoot, pki.KubeletStateTrustBoth, []string{"restart kube-apiserver"}},
		{pki.KubeletPhaseRemoveCA, pki.KubeletStateNotMigrated, []string{"restart kubelet"}},
	} {
		log.calls = nil
		force := tc.phase == pki.KubeletPhaseReAddRoot
		if err := migrateKubeletClientCAWith(io.Discard, KubeletCAOptions{Phase: tc.phase, Force: force, Quiet: true}, log.ops(true, nil)); err != nil {
			t.Fatalf("%s: %v", tc.phase, err)
		}
		wantKubeletState(t, tc.state)
		if !slices.Equal(log.calls, tc.calls) {
			t.Errorf("%s: calls %v, want %v", tc.phase, log.calls, tc.calls)
		}
	}
}

// A resumed run skips a phase a node is already past, without a restart.
func TestKubeletCASkipsAPhaseANodeIsPast(t *testing.T) {
	kubeletCAPKI(t)
	for _, step := range []func() error{
		func() error { return pki.KubeletClientTrustBoth(true) },
		func() error { return pki.KubeletClientIssueNew("cp1", "10.0.0.5", nil) },
		pki.KubeletClientDropRoot,
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{pki.KubeletPhaseTrustBoth, pki.KubeletPhaseIssueNew} {
		var log kubeletOpsLog
		if err := migrateKubeletClientCAWith(io.Discard, KubeletCAOptions{Phase: phase, FetchShared: true, Quiet: true}, log.ops(true, nil)); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		if len(log.calls) != 0 {
			t.Errorf("%s on a migrated node did %v, want nothing", phase, log.calls)
		}
	}
	wantKubeletState(t, pki.KubeletStateMigrated)
}

// re-add-root on a migrated node lets root-signed client certificates reach
// its kubelet again, so it needs --force.
func TestKubeletCAReAddRootNeedsForceOnAMigratedNode(t *testing.T) {
	kubeletCAPKI(t)
	for _, step := range []func() error{
		func() error { return pki.KubeletClientTrustBoth(true) },
		func() error { return pki.KubeletClientIssueNew("cp1", "10.0.0.5", nil) },
		pki.KubeletClientDropRoot,
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	var log kubeletOpsLog
	err := migrateKubeletClientCAWith(io.Discard, KubeletCAOptions{Phase: pki.KubeletPhaseReAddRoot, Quiet: true}, log.ops(true, nil))
	if err == nil || !strings.Contains(err.Error(), "without --force") {
		t.Fatalf("re-add-root = %v, want a refusal without --force", err)
	}
	if len(log.calls) != 0 {
		t.Errorf("the refused re-add-root did %v", log.calls)
	}
	wantKubeletState(t, pki.KubeletStateMigrated)
}

// A follower control plane takes the initiator's CA rather than minting its
// own, which would sign a client certificate no other kubelet trusts.
func TestKubeletCAFollowerFetchesTheSharedCA(t *testing.T) {
	kubeletCAPKI(t)
	var log kubeletOpsLog
	err := migrateKubeletClientCAWith(io.Discard, KubeletCAOptions{Phase: pki.KubeletPhaseTrustBoth, FetchShared: true, Quiet: true}, log.ops(true, nil))
	if err == nil || !strings.Contains(err.Error(), "refusing to mint") {
		t.Fatalf("trust-both = %v, want a refusal to mint on a follower", err)
	}
	if !slices.Equal(log.calls, []string{"fetch-shared"}) {
		t.Errorf("calls %v, want the fetch alone", log.calls)
	}
	wantKubeletState(t, pki.KubeletStateNotMigrated)
}

// A worker takes the one kubelet-client CA the cluster publishes at
// trust-both, and drops the root and later the CA from its own files.
func TestKubeletCAPhasesOnAWorker(t *testing.T) {
	root, kca := newTestCA(t, "stratum-ca"), newTestCA(t, "stratum-kubelet-client-ca")
	kubeletCAWorkerPKI(t, root)
	var log kubeletOpsLog
	for _, tc := range []struct {
		phase, state string
		calls        []string
	}{
		{pki.KubeletPhaseTrustBoth, pki.KubeletStateTrustBoth, []string{"fetch-published", "restart kubelet"}},
		{pki.KubeletPhaseTrustBoth, pki.KubeletStateTrustBoth, []string{"restart kubelet"}},
		{pki.KubeletPhaseIssueNew, pki.KubeletStateTrustBoth, nil},
		{pki.KubeletPhaseDropRoot, pki.KubeletStateMigrated, []string{"restart kubelet"}},
		{pki.KubeletPhaseTrustBoth, pki.KubeletStateMigrated, nil},
		{pki.KubeletPhaseReAddRoot, pki.KubeletStateTrustBoth, []string{"restart kubelet"}},
		{pki.KubeletPhaseReissueRoot, pki.KubeletStateTrustBoth, nil},
		{pki.KubeletPhaseRemoveCA, pki.KubeletStateNotMigrated, []string{"restart kubelet"}},
	} {
		log.calls = nil
		force := tc.phase == pki.KubeletPhaseReAddRoot
		if err := migrateKubeletClientCAWith(io.Discard, KubeletCAOptions{Phase: tc.phase, Force: force, Quiet: true}, log.ops(false, kca.cert)); err != nil {
			t.Fatalf("%s: %v", tc.phase, err)
		}
		wantKubeletState(t, tc.state)
		if !slices.Equal(log.calls, tc.calls) {
			t.Errorf("%s: calls %v, want %v", tc.phase, log.calls, tc.calls)
		}
	}
	b, err := os.ReadFile(pki.KubeletClientBundlePath())
	if err != nil || string(b) != string(bundle(root)) {
		t.Errorf("after the rollback the worker's kubelet bundle is %q (%v), want the root", b, err)
	}
}

// A refresh on a worker part way through the migration would replace its
// kubelet bundle with the root's and drop the kubelet-client CA.
func TestRefreshRefusesAMigratingWorker(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	kca := newTestCA(t, "stratum-kubelet-client-ca")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	if err := os.WriteFile(p.kubeletClientCA, bundle(kca, oldCA), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, p)
	_, err := planTrustRefresh(url, trustWiden, p)
	if err == nil || !strings.Contains(err.Error(), "part way through the kubelet client-CA migration") {
		t.Fatalf("planTrustRefresh = %v, want a refusal naming the migration", err)
	}
	assertCleanAbort(t, p, before)
}

func TestKubeletCAFlagsValidate(t *testing.T) {
	for _, tc := range []struct {
		f    KubeletCAFlags
		want string
	}{
		{KubeletCAFlags{Cluster: true}, ""},
		{KubeletCAFlags{Cluster: true, Rollback: true, Force: true}, ""},
		{KubeletCAFlags{Phase: pki.KubeletPhaseTrustBoth, FetchShared: true}, ""},
		{KubeletCAFlags{Phase: pki.KubeletPhaseReAddRoot, Force: true}, ""},
		{KubeletCAFlags{}, "pass --cluster, or --phase"},
		{KubeletCAFlags{Cluster: true, Phase: pki.KubeletPhaseDropRoot}, "not both"},
		{KubeletCAFlags{Rollback: true, Phase: pki.KubeletPhaseRemoveCA}, "--rollback only applies to --cluster"},
		{KubeletCAFlags{Phase: "rollback"}, "unknown phase"},
		{KubeletCAFlags{Cluster: true, Force: true}, "--force only applies to a rollback"},
		{KubeletCAFlags{Phase: pki.KubeletPhaseDropRoot, Force: true}, "--force only applies to a rollback"},
		{KubeletCAFlags{Phase: pki.KubeletPhaseIssueNew, FetchShared: true}, "--fetch-shared only applies"},
	} {
		err := tc.f.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: Validate = %v, want %q", tc.f, err, tc.want)
		}
	}
}

// Every argv the cluster run hands a node passes the flag rules the node's
// command applies.
func TestKubeletCANodeArgvPassesValidation(t *testing.T) {
	for _, phase := range append(slices.Clone(KubeletCAPhases), KubeletCARollbackPhases...) {
		for _, fetch := range []bool{false, phase == pki.KubeletPhaseTrustBoth} {
			for _, force := range []bool{false, true} {
				argv := kubeletCANodeArgv(phase, fetch, force)
				i := slices.Index(argv, version.StratumInstallPath)
				if i < 0 || !slices.Equal(argv[i+1:i+3], []string{"certs", "migrate-kubelet-client-ca"}) {
					t.Fatalf("argv %v does not run certs migrate-kubelet-client-ca", argv)
				}
				f := KubeletCAFlags{
					Phase:       argv[slices.Index(argv, "--phase")+1],
					FetchShared: slices.Contains(argv, "--fetch-shared"),
					Force:       slices.Contains(argv, "--force"),
				}
				if err := f.Validate(); err != nil {
					t.Errorf("argv %v: %v", argv, err)
				}
				if f.Force != (force && phase == pki.KubeletPhaseReAddRoot) {
					t.Errorf("argv %v: --force=%v", argv, f.Force)
				}
			}
		}
	}
}
