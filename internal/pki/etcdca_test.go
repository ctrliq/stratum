package pki

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// etcdTrusts runs the decision etcd actually makes for an incoming client under
// `--client-cert-auth --trusted-ca-file etcd-ca-bundle.crt`: verify the chain
// against a pool built from that file, with the client-auth EKU. This is not a
// stand-in for etcd — it is the same crypto/x509 call path etcd's TLS stack
// takes, so a leaf this rejects is a leaf etcd rejects.
func etcdTrusts(t *testing.T, leaf string) error {
	t.Helper()
	_, err := loadCert(t, leaf).Verify(x509.VerifyOptions{
		Roots:     poolFromBundle(t, EtcdBundleBase),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err
}

// migrateAll brings a node to the fully-migrated end state, walking the phases
// when there is anything to walk. A fresh install is already there — GenerateAll
// splits it in one pass — and the replay guard correctly refuses trust-both on a
// node that has no root left in its etcd bundle.
func migrateAll(t *testing.T) {
	t.Helper()
	if ReadEtcdTrust().State() == EtcdStateMigrated {
		return
	}
	if err := MigrateEtcdTrustBoth(true); err != nil {
		t.Fatalf("trust-both: %v", err)
	}
	if err := MigrateEtcdIssueNew(tHost, tIP, nil); err != nil {
		t.Fatalf("issue-new: %v", err)
	}
	if err := MigrateEtcdDropRoot(); err != nil {
		t.Fatalf("drop-root: %v", err)
	}
}

// A node past drop-root whose etcd leaf no longer pairs with its key cannot
// start etcd. Issue-new is owed again there, and running it rewrites the pair
// under the etcd CA without re-adding the root.
func TestIssueNewRepairsAMismatchedPairAfterDropRoot(t *testing.T) {
	genInto(t)
	migrateAll(t)
	if applied, err := EtcdPhaseApplied(EtcdPhaseIssueNew); err != nil || !applied {
		t.Fatalf("EtcdPhaseApplied(issue-new) on a migrated node = %v, %v; want true", applied, err)
	}

	// Another leaf's key under etcd's certificate.
	other, err := os.ReadFile(path("apiserver-etcd-client", "key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("etcd", "key"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if applied, err := EtcdPhaseApplied(EtcdPhaseIssueNew); err != nil || applied {
		t.Fatalf("EtcdPhaseApplied(issue-new) with etcd's key mismatched = %v, %v; want false, so the repair runs", applied, err)
	}

	if err := MigrateEtcdIssueNew(tHost, tIP, nil); err != nil {
		t.Fatalf("issue-new on a migrated node with a mismatched pair: %v", err)
	}
	if !keyMatches(path("etcd", "crt"), path("etcd", "key")) {
		t.Error("issue-new left etcd's certificate and key mismatched")
	}
	if got := ReadEtcdTrust().State(); got != EtcdStateMigrated {
		t.Errorf("after the repair the node is %s, want %s: the root must stay out of etcd's trust", got, EtcdStateMigrated)
	}
	if applied, err := EtcdPhaseApplied(EtcdPhaseIssueNew); err != nil || !applied {
		t.Errorf("EtcdPhaseApplied(issue-new) after the repair = %v, %v; want true", applied, err)
	}
}

// unmigratedNode reproduces a cluster installed before the etcd CA existed: the
// etcd leaves are signed by the cluster root and there is no etcd-ca on disk.
// This is the state every existing cluster upgrades from, so most of the risk
// lives in what happens to a node in exactly this shape.
func unmigratedNode(t *testing.T) {
	t.Helper()
	version.PKIDir = t.TempDir()
	if err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	ca, err := EnsureCA("ca", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCA("front-proxy-ca", "front-proxy-ca"); err != nil {
		t.Fatal(err)
	}
	// Every leaf from the root, including etcd's — the pre-etcd-CA-split layout.
	for _, l := range LeafSpecs(tHost, tIP, nil) {
		if err := ca.Issue(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnsureSAKeypair(); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"ca", "front-proxy-ca"} {
		if err := WriteCABundle(b, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteSABundle("sa"); err != nil {
		t.Fatal(err)
	}
}

// TestKubeletCertRejectedByEtcdAfterMigration is THE test for the etcd trust hole. The
// reported vulnerability is that a kubelet client certificate — readable as root
// on any worker — authenticates to etcd, because etcd trusted the same root that
// signs it. After the migration it must not.
//
// It starts from unmigratedNode, NOT from a fresh install, and that is the whole
// point: a fresh install is already in the end state (GenerateAll splits it in one
// pass), so migrateAll returns immediately and the test asserts nothing the
// one-pass test does not already cover. The upgrade path — an existing cluster
// walked through all three phases — is the ONLY path on which the etcd trust hole was ever
// open, so it is the one that has to be pinned here.
//
// Asserting UnknownAuthorityError specifically (not merely "some error") is
// deliberate: an expiry or EKU mismatch would also produce an error and would
// pass a weaker check while leaving the trust domains joined.
func TestKubeletCertRejectedByEtcdAfterMigration(t *testing.T) {
	unmigratedNode(t)
	// A pre-etcd-CA-split node has no etcd bundle at all — etcd's --trusted-ca-file was
	// ca-bundle.crt — so that is the trust set the vulnerability lives in, and it
	// must accept the kubelet leaf here or this test starts from the wrong place.
	if _, err := loadCert(t, "kubelet").Verify(x509.VerifyOptions{
		Roots:     poolFromBundle(t, "ca"),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("precondition: a node that predates the etcd trust split must START vulnerable, or this test proves nothing: %v", err)
	}
	migrateAll(t)

	err := etcdTrusts(t, "kubelet")
	if err == nil {
		t.Fatal("kubelet cert still authenticates to etcd after migration — the etcd trust hole is NOT closed")
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("kubelet cert rejected, but not as an untrusted issuer (%T: %v) — the trust domains may still overlap", err, err)
	}
	// The legitimate client must still work, or we have broken the cluster.
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("apiserver-etcd-client rejected by etcd after migration: %v", err)
	}
}

// TestTrustBothStillAcceptsKubeletCert pins the uncomfortable half of the design:
// phase trust-both is a security NO-OP. A cluster parked there is fully healthy
// and fully exploitable.
//
// This exists so "we ran phase 1, the hole is closed" cannot be claimed in code. If a
// future change makes trust-both start rejecting root-signed clients, that is a
// behaviour change in the migration's safety story — peers still presenting
// root-signed leaves would be cut off — and this test should fail loudly rather
// than quietly bless it.
func TestTrustBothStillAcceptsKubeletCert(t *testing.T) {
	unmigratedNode(t)
	if err := MigrateEtcdTrustBoth(true); err != nil {
		t.Fatalf("trust-both: %v", err)
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Fatalf("state = %q, want %q", got, EtcdStateTrustBoth)
	}
	if err := etcdTrusts(t, "kubelet"); err != nil {
		t.Fatalf("trust-both must still accept root-signed clients (it is a no-op by design), got: %v", err)
	}
}

// TestFreshInstallSplitsInOnePass: a brand-new cluster is never vulnerable and
// never needs the migration — GenerateAll issues etcd's leaves from etcd-ca and
// the bundle carries that anchor alone.
func TestFreshInstallSplitsInOnePass(t *testing.T) {
	genInto(t)
	if got := EtcdMigrationState(); got != EtcdStateMigrated {
		t.Errorf("fresh install state = %q, want %q", got, EtcdStateMigrated)
	}
	if n := countPEMBlocks(t, EtcdBundleBase+"-bundle.crt"); n != 1 {
		t.Errorf("fresh install etcd bundle has %d CAs, want 1 (etcd-ca only)", n)
	}
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Error("fresh install: kubelet cert authenticates to etcd — the etcd trust hole is present on a NEW cluster")
	}
}

// TestExistingClusterLandsInTrustBoth covers the upgrade hazard that makes this a
// staged migration rather than a flag flip.
//
// GenerateAll is skip-if-exists, so on an existing node it mints etcd-ca but
// leaves the live etcd leaves root-signed — while `stratum server` re-renders the
// etcd unit unconditionally, pointing --trusted-ca-file at the etcd bundle. If
// that bundle held etcd-ca alone, etcd would restart trusting a CA that signed
// none of the live certificates and the apiserver would lose its own etcd.
//
// So: both anchors, and the leaves untouched.
func TestExistingClusterLandsInTrustBoth(t *testing.T) {
	unmigratedNode(t)
	before := loadCert(t, "apiserver-etcd-client").SerialNumber

	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatalf("GenerateAll on an existing node: %v", err)
	}

	if n := countPEMBlocks(t, EtcdBundleBase+"-bundle.crt"); n != 2 {
		t.Fatalf("existing cluster etcd bundle has %d CAs, want 2 (etcd-ca‖root) — a 1-CA bundle here bricks the node", n)
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Errorf("state = %q, want %q", got, EtcdStateTrustBoth)
	}
	// The live leaves must be left exactly as they were: reissuing them here would
	// be a unilateral phase issue-new on one node, breaking peer TLS against every
	// control plane that has not migrated yet.
	if after := loadCert(t, "apiserver-etcd-client").SerialNumber; before.Cmp(after) != 0 {
		t.Error("GenerateAll reissued apiserver-etcd-client on an existing node — that is issue-new, and it is not safe to do unilaterally")
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Errorf("existing root-signed client rejected after upgrade — the node is broken: %v", err)
	}
}

// TestMigrationStateMachine walks the three phases and checks the reported state
// at each step — the surface an operator uses to answer "am I done?", which is
// otherwise unanswerable because phases 1 and 2 change nothing observable.
func TestMigrationStateMachine(t *testing.T) {
	unmigratedNode(t)
	if got := EtcdMigrationState(); got != EtcdStateNotMigrated {
		t.Fatalf("initial state = %q, want %q", got, EtcdStateNotMigrated)
	}
	if err := MigrateEtcdTrustBoth(true); err != nil {
		t.Fatal(err)
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Errorf("after trust-both: %q, want %q", got, EtcdStateTrustBoth)
	}
	if err := MigrateEtcdIssueNew(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if got := EtcdMigrationState(); got != EtcdStateIssueNew {
		t.Errorf("after issue-new: %q, want %q", got, EtcdStateIssueNew)
	}
	// Still vulnerable here: the root is in the bundle, so a kubelet cert works.
	if err := etcdTrusts(t, "kubelet"); err != nil {
		t.Errorf("issue-new should still trust the root (it is a no-op by design): %v", err)
	}
	if err := MigrateEtcdDropRoot(); err != nil {
		t.Fatal(err)
	}
	if got := EtcdMigrationState(); got != EtcdStateMigrated {
		t.Errorf("after drop-root: %q, want %q", got, EtcdStateMigrated)
	}
	// The state string is a label this package computes about itself; the property
	// the etcd trust hole is about is that the kubelet leaf no longer chains to an anchor etcd
	// trusts. Assert the outcome, on the upgrade path, at the end of the walk —
	// otherwise every assertion in this test is self-reported.
	err := etcdTrusts(t, "kubelet")
	if err == nil {
		t.Fatal("after a full upgrade-path migration a kubelet cert still authenticates to etcd — the etcd trust hole is NOT closed on upgraded clusters")
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("kubelet cert rejected, but not as an untrusted issuer (%T: %v) — the trust domains may still overlap", err, err)
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("apiserver-etcd-client rejected by etcd after the migration: %v", err)
	}
}

// TestPhasesRefuseOutOfOrder: each phase guards its own precondition, so an
// operator driving phases by hand cannot lock a node out of its own etcd.
func TestPhasesRefuseOutOfOrder(t *testing.T) {
	unmigratedNode(t)
	if err := MigrateEtcdIssueNew(tHost, tIP, nil); err == nil {
		t.Error("issue-new should refuse before trust-both")
	}
	if err := MigrateEtcdDropRoot(); err == nil {
		t.Error("drop-root should refuse before the etcd CA exists")
	}
	if err := MigrateEtcdTrustBoth(true); err != nil {
		t.Fatal(err)
	}
	// Leaves are still root-signed: dropping the root now strands the apiserver.
	if err := MigrateEtcdDropRoot(); err == nil {
		t.Error("drop-root should refuse while this node's etcd leaves are still root-signed")
	}
}

// TestRotateCAKeepsEtcdLeavesOnEtcdCA is the FORWARD-path regression test for
// the trap: etcd's leaves signed by the cluster root on a migrated cluster.
//
// Four callers reach RotateLeaves: certs.Rotate (the daily renew timer),
// `stratum server` (which runs certs.Rotate with --if-expiring=45 before writing
// units), rotate-ca's sign-new phase, and rotate-ca's rollback. Any of them
// re-signing etcd's leaves with the root on a migrated cluster would lock each
// apiserver out of its own etcd — so the fuse is not "a year until the renew
// timer fires", it is "the next time anyone runs `stratum certs rotate-ca`".
func TestRotateCAKeepsEtcdLeavesOnEtcdCA(t *testing.T) {
	genInto(t)
	migrateAll(t)
	etcdCABefore := loadCert(t, EtcdCAName)

	// A full root rotation: trust-new -> sign-new -> finalize.
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil { // <- the sign-new caller
		t.Fatal(err)
	}
	if err := FinalizeCA(); err != nil {
		t.Fatal(err)
	}

	if got := loadCert(t, EtcdCAName); got.SerialNumber.Cmp(etcdCABefore.SerialNumber) != 0 {
		t.Error("root rotation replaced the etcd CA — it must be untouched")
	}
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(etcdCABefore); err != nil {
			t.Errorf("%s left etcd-ca during a root rotation: %v", leaf, err)
		}
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("apiserver locked out of its own etcd by a root rotation: %v", err)
	}
	// And the split must still hold afterward.
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Error("a root rotation reopened the etcd trust hole")
	}
}

// TestRotateCARollbackKeepsEtcdLeavesOnEtcdCA covers the ROLLBACK branch, which
// needs its own test rather than a parameter on the forward one: it is reached
// only after something has already gone wrong, a second failure there compounds,
// and a trust-new -> sign-new -> finalize test passes while it is broken.
//
// certs.RollbackCA calls RotateLeaves whenever sign-new was reached (detected via
// the .pre-carotate backup), so the same trap applies on the recovery path.
func TestRotateCARollbackKeepsEtcdLeavesOnEtcdCA(t *testing.T) {
	genInto(t)
	migrateAll(t)
	etcdCABefore := loadCert(t, EtcdCAName)

	// Partial rotation: staged and promoted, then abandoned.
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if err := RollbackCA(); err != nil {
		t.Fatalf("RollbackCA: %v", err)
	}
	// certs.RollbackCA reissues the leaves under the RESTORED root when sign-new
	// was reached; reproduce that call here (the certs layer owns the detection).
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}

	if got := loadCert(t, EtcdCAName); got.SerialNumber.Cmp(etcdCABefore.SerialNumber) != 0 {
		t.Error("rollback replaced the etcd CA")
	}
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(etcdCABefore); err != nil {
			t.Errorf("%s left etcd-ca during a rotation ROLLBACK: %v", leaf, err)
		}
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("rollback locked the apiserver out of its own etcd: %v", err)
	}
}

// TestRotateLeavesPreservesRootSignerPreMigration is the mirror image: on a node
// that has NOT migrated, a leaf rotation must leave etcd's certs root-signed.
// Silently upgrading them to etcd-ca would be a unilateral phase issue-new — the
// node's peers would still trust only the root and would reject it.
func TestRotateLeavesPreservesRootSignerPreMigration(t *testing.T) {
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil { // mints etcd-ca, lands in trust-both
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	ca := loadCert(t, "ca")
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(ca); err != nil {
			t.Errorf("%s moved off the cluster root during a leaf rotation on an unmigrated node: %v", leaf, err)
		}
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Errorf("state = %q, want %q — a leaf rotation must not advance the migration", got, EtcdStateTrustBoth)
	}
}

// TestEnsureEtcdTrustBundleNeverNarrows: the safety net must not silently perform
// drop-root. Collapsing the bundle is gated on cluster-wide health and belongs to
// the migration command alone.
func TestEnsureEtcdTrustBundleNeverNarrows(t *testing.T) {
	unmigratedNode(t)
	if err := EnsureEtcdTrustBundle(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := EnsureEtcdTrustBundle(); err != nil {
			t.Fatal(err)
		}
	}
	if n := countPEMBlocks(t, EtcdBundleBase+"-bundle.crt"); n != 2 {
		t.Fatalf("bundle narrowed to %d CAs on re-run — that would drop the root without a health gate", n)
	}
}

// TestEtcdCAKeyPerms: the etcd CA key is as sensitive as the root's.
func TestEtcdCAKeyPerms(t *testing.T) {
	genInto(t)
	fi, err := os.Stat(filepath.Join(version.PKIDir, EtcdCAName+".key"))
	if err != nil {
		t.Fatalf("etcd-ca.key missing: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("etcd-ca.key is %o, want 600", fi.Mode().Perm())
	}
}

// A joining control plane has no etcd.crt, so GenerateAll mints one. Minting it
// under the etcd CA while the rest of the cluster is still root-signed is a
// unilateral issue-new: the live peers trust only the root, they reject the new
// member's peer certificate, it never enters raft — and because MemberAdd has
// already registered a VOTING member, a 1-CP→2-CP expansion moves quorum to
// 2-of-2 with one member dead and the surviving control plane goes read-only.
//
// The downloaded trust bundle is what says which CA the live members accept.
func TestJoiningNodeSignsEtcdLeavesTheClusterAccepts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clusterCAs []string // what the cluster's etcd-ca-bundle.crt carries
		wantSigner string
	}{
		{"cluster mid-migration (root still trusted)", []string{EtcdCAName, "ca"}, "ca"},
		{"cluster fully migrated", []string{EtcdCAName}, EtcdCAName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A joining node: shared CA material downloaded, no node-local leaves.
			version.PKIDir = t.TempDir()
			if err := EnsureDir(); err != nil {
				t.Fatal(err)
			}
			for _, n := range []struct{ name, cn string }{
				{"ca", "stratum-ca"}, {"front-proxy-ca", "front-proxy-ca"}, {EtcdCAName, "stratum-etcd-ca"},
			} {
				if _, err := EnsureCA(n.name, n.cn); err != nil {
					t.Fatal(err)
				}
			}
			if err := EnsureSAKeypair(); err != nil {
				t.Fatal(err)
			}
			// The cluster's trust state arrives with the shared certs.
			if err := WriteCABundle(EtcdBundleBase, tc.clusterCAs...); err != nil {
				t.Fatal(err)
			}

			if err := GenerateAll("cp2", "10.0.0.9", nil); err != nil {
				t.Fatalf("join-time GenerateAll: %v", err)
			}

			signer := loadCert(t, tc.wantSigner)
			for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
				if err := loadCert(t, leaf).CheckSignatureFrom(signer); err != nil {
					t.Errorf("%s was not signed by %s — the live members would reject this node: %v", leaf, tc.wantSigner, err)
				}
			}
			// And the node must not have quietly rewritten the cluster's bundle.
			if got := ReadEtcdTrust().Anchors; got != len(tc.clusterCAs) {
				t.Errorf("joining node changed the cluster trust bundle: %d anchors, want %d", got, len(tc.clusterCAs))
			}
		})
	}
}

// A follower must not mint its own etcd CA.
//
// A bare command fanned out by `migrate-etcd-ca --cluster` would hit EnsureCA
// on every control plane, each generating its OWN self-signed anchor. Phase
// trust-both still looks healthy on all of them — every leaf is root-signed, so
// nothing is observably wrong — and the cluster only comes apart at issue-new,
// when each member presents a leaf its peers could not verify. Failing here
// turns a silent quorum loss into a distribution error.
func TestFollowerRefusesToMintEtcdCA(t *testing.T) {
	unmigratedNode(t) // no etcd-ca on disk: material was not distributed

	err := MigrateEtcdTrustBoth(false)
	if err == nil {
		t.Fatal("a follower minted its own etcd CA — every control plane would end up on a different anchor and peer TLS breaks at issue-new")
	}
	if !strings.Contains(err.Error(), EtcdPhaseIssueNew) {
		t.Errorf("error should name the phase where this surfaces, got: %v", err)
	}
	if exists(EtcdCAName) {
		t.Error("refused but minted the CA anyway")
	}

	// With the cluster's anchor present (i.e. --fetch-shared worked), it proceeds
	// and adopts that anchor rather than making a new one.
	distributed, err := NewCA(EtcdCAName, "stratum-etcd-ca")
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateEtcdTrustBoth(false); err != nil {
		t.Fatalf("follower with distributed material must proceed: %v", err)
	}
	if got := loadCert(t, EtcdCAName); got.SerialNumber.Cmp(distributed.Cert.SerialNumber) != 0 {
		t.Error("follower replaced the distributed etcd CA with one of its own")
	}
}

// TestTrustStateIsOneAnswer: orchestrator distribution, the join-time signer
// and the rotation-time anchors all turn on "what etcd trust state is this
// cluster in". Three independent notions of that would drift, so they read one.
func TestTrustStateIsOneAnswer(t *testing.T) {
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	trust := ReadEtcdTrust()
	if !trust.TrustsRoot() || trust.State() != EtcdStateTrustBoth || trust.EtcdSignerName() != "ca" {
		t.Fatalf("trust-both must be self-consistent, got %+v state=%s signer=%s", trust, trust.State(), trust.EtcdSignerName())
	}
	// A stale root — one a rotation replaced — is still a valid etcd client-auth
	// anchor, and must not be mistaken for a migrated cluster just because it is
	// no longer ca.crt. Counting anchors would miss exactly this.
	if _, err := NewCA("ca", "stratum-ca-rotated"); err != nil {
		t.Fatal(err)
	}
	rotated := ReadEtcdTrust()
	if rotated.TrustsLiveRoot {
		t.Error("the bundle should no longer carry the LIVE root after a rotation")
	}
	if rotated.StaleAnchors != 1 || !rotated.TrustsRoot() {
		t.Fatalf("a root left behind by a rotation is still a trusted anchor, got %+v", rotated)
	}
	if rotated.State() == EtcdStateMigrated {
		t.Error("a cluster still trusting a stale root reported as migrated — the etcd trust hole would read as closed while any pre-rotation kubelet cert still works")
	}
}

// TestRootRotationOnUnmigratedClusterKeepsEtcdWorking is the forward-path
// regression test, and the failure it catches is the OUTAGE, not a bundle
// mismatch.
//
// etcd trusts etcd-ca-bundle.crt, which the rotation state machine does not
// maintain — it maintains ca-bundle.crt. So on an unmigrated cluster — every
// existing cluster, immediately after the binary upgrade — rotate-ca's sign-new
// promotes a new root, etcdLeafSigner correctly keeps etcd's leaves on the root
// (now the NEW one), and etcd's stale bundle cannot verify it. Cluster-wide
// control-plane outage from the documented rotation command.
func TestRootRotationOnUnmigratedClusterKeepsEtcdWorking(t *testing.T) {
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil { // lands in trust-both
		t.Fatal(err)
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Fatalf("precondition: state = %q, want %q", got, EtcdStateTrustBoth)
	}

	// trust-new: etcd must trust the new root BEFORE any leaf moves.
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	// sign-new: leaves are reissued under the promoted root.
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	// THE assertion: etcd still accepts its own apiserver. A miss here is
	// "certificate signed by unknown authority" — the apiserver locked out
	// of its own etcd, which is the outage.
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("after rotate-ca sign-new the apiserver is locked out of its own etcd — this is the outage: %v", err)
	}
	if err := etcdTrusts(t, "etcd"); err != nil {
		t.Fatalf("after rotate-ca sign-new etcd's own server/peer cert is untrusted by its peers: %v", err)
	}

	// finalize: the old root must leave etcd's trust at the same instant it leaves
	// Kubernetes trust — this is the --force / compromised-root case.
	oldRoot := loadCert(t, "ca.pre-carotate")
	if err := FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("finalize locked the apiserver out of its own etcd: %v", err)
	}
	for _, c := range loadBundle(t, EtcdBundleBase) {
		if c.Equal(oldRoot) {
			t.Error("the old root is STILL an etcd client-auth anchor after finalize — under --force that is the compromised key, and every certificate it signed still reaches etcd")
		}
	}
}

// TestRootRotationRollbackDropsTheNewRootFromEtcdTrust is the site that
// fails silently: after a rolled-back rotation the etcd leaves go
// back to the old root, which is still in the stale bundle, so the cluster keeps
// running while etcd goes on trusting the root the rollback existed to un-trust.
func TestRootRotationRollbackDropsTheNewRootFromEtcdTrust(t *testing.T) {
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	oldRoot := loadCert(t, "ca")

	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	newRoot := loadCert(t, "ca")
	if err := RollbackCA(); err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil { // certs.RollbackCA does this
		t.Fatal(err)
	}

	for _, c := range loadBundle(t, EtcdBundleBase) {
		if c.Equal(newRoot) {
			t.Error("the rolled-back root is still an etcd client-auth anchor — the rollback un-trusted it everywhere except etcd")
		}
	}
	// And the restored root must still work, or the rollback broke the cluster.
	if !loadCert(t, "ca").Equal(oldRoot) {
		t.Fatal("rollback did not restore the original root")
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("rollback left the apiserver locked out of its own etcd: %v", err)
	}
}

// TestRootRotationLeavesMigratedClustersAlone: once the split is complete, a root
// rotation must not touch etcd at all — that is the point of separating the
// domains.
func TestRootRotationLeavesMigratedClustersAlone(t *testing.T) {
	genInto(t)
	migrateAll(t)
	before := loadBundle(t, EtcdBundleBase)

	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeCA(); err != nil {
		t.Fatal(err)
	}

	after := loadBundle(t, EtcdBundleBase)
	if len(after) != len(before) || len(after) != 1 || !after[0].Equal(before[0]) {
		t.Errorf("a root rotation altered a migrated cluster's etcd trust: %d anchors before, %d after", len(before), len(after))
	}
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Error("a root rotation re-opened the etcd trust hole on a migrated cluster")
	}
}

// TestTrustBothRefusesOnAMigratedNode is the trust-both replay guard. trust-both
// re-adds the cluster root as an etcd client-auth anchor; replayed against a node
// that has completed drop-root it re-opens the etcd trust hole — reported as success, by the
// command named for closing it. A complete re-run would narrow again; the hazard
// is one that stops early.
//
// rotate-ca solves the same problem the same way, at the start.
func TestTrustBothRefusesOnAMigratedNode(t *testing.T) {
	genInto(t)
	migrateAll(t)
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Fatal("precondition: node should be migrated")
	}

	err := MigrateEtcdTrustBoth(true)
	if err == nil {
		t.Fatal("trust-both re-widened a migrated node's etcd trust — the etcd trust hole re-opened by the command that closes it, reported as success")
	}
	if !strings.Contains(err.Error(), EtcdPhaseDropRoot) {
		t.Errorf("refusal should say the node already completed drop-root, got: %v", err)
	}
	// And it must not have written anything.
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Fatal("the refused phase still widened the bundle")
	}
}

// TestTrustBothRunsOnANodeThatOnlyLooksMigrated pins the OTHER half of the
// replay guard: refusing the wrong node is its own defect.
//
// Asking only "does this node have the etcd anchor and no root?" also matches
// a node holding an etcd-ca-only bundle whose leaves are still root-signed —
// a node that has migrated NOTHING. A follower lands in exactly
// that shape running `--phase=trust-both --fetch-shared` against a cluster whose
// initiator has already finished: certs.migrateEtcdCAWith fetches before it migrates,
// and the fetched material includes the cluster's current, narrow
// etcd-ca-bundle.crt (ha.rotationCerts), while this node's own leaves are
// untouched. Refused there, it never migrates at all — and it is now carrying a
// bundle that does not accept its own apiserver's etcd client cert, so the next
// etcd restart locks the apiserver out of its own etcd.
func TestTrustBothRunsOnANodeThatOnlyLooksMigrated(t *testing.T) {
	unmigratedNode(t)
	if _, err := EnsureCA(EtcdCAName, etcdCACommonName); err != nil {
		t.Fatal(err)
	}
	if err := WriteCABundle(EtcdBundleBase, EtcdCAName); err != nil { // the cluster's narrow bundle
		t.Fatal(err)
	}
	if got := EtcdMigrationState(); got != EtcdStateNotMigrated {
		t.Fatalf("precondition: this node has migrated nothing, state = %q want %q", got, EtcdStateNotMigrated)
	}

	if err := MigrateEtcdTrustBoth(true); err != nil {
		t.Fatalf("trust-both refused a node that has migrated nothing: %v", err)
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Errorf("state = %q, want %q", got, EtcdStateTrustBoth)
	}
	// The point of trust-both: this node's own root-signed etcd client works again.
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Errorf("the node's own root-signed etcd client is still rejected after trust-both — it cannot reach its own etcd: %v", err)
	}
}

// Once Reissue has overwritten etcd.crt under the etcd CA, nothing else in this
// tree puts it back on the root — a failed issue-new would otherwise be
// unrecoverable, landing first on the initiator (the node the operator is
// sitting on). Recovery is by re-derivation, matching rotate-ca's --rollback;
// no leaf is backed up anywhere here.
func TestRollbackRederivesEtcdLeavesUnderTheRoot(t *testing.T) {
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if err := MigrateEtcdIssueNew(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if !ReadEtcdTrust().LeavesMigrated {
		t.Fatal("precondition: leaves should be on the etcd CA")
	}

	if err := MigrateEtcdRollback(tHost, tIP, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	root := loadCert(t, "ca")
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(root); err != nil {
			t.Errorf("%s was not re-derived under the cluster root: %v", leaf, err)
		}
	}
	// Trust must be WIDE after a rollback, not narrow: peers that already
	// completed issue-new are still presenting etcd-CA-signed certs.
	tr := ReadEtcdTrust()
	if !tr.HasEtcdAnchor || !tr.TrustsRoot() {
		t.Errorf("rollback left trust at %+v, want both anchors so mixed peers still verify", tr)
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Errorf("rolled-back apiserver cannot reach its own etcd: %v", err)
	}
	if got := EtcdMigrationState(); got != EtcdStateTrustBoth {
		t.Errorf("state after rollback = %q, want %q", got, EtcdStateTrustBoth)
	}
}

// TestRollbackRefusesOnAMigratedNode is the replay guard applied to rollback.
//
// Rollback is the only path in this package back to a root-trusting etcd bundle,
// and a presence-only check — a root CA is here, an etcd CA is here — passes on
// a fully migrated node. Run there, one command re-opens the etcd trust hole on
// a cluster the operator believes is fixed, and does it more thoroughly than a
// replayed trust-both: the bundle widens AND this node's etcd.crt goes back to
// the cluster root, while its peers enforce etcd-CA-only peer trust, so the node
// also drops out of raft.
func TestRollbackRefusesOnAMigratedNode(t *testing.T) {
	unmigratedNode(t)
	migrateAll(t)
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Fatal("precondition: node should be migrated")
	}

	err := MigrateEtcdRollback(tHost, tIP, nil)
	if err == nil {
		t.Fatal("rollback re-derived a migrated node's etcd leaves under the cluster root — the etcd trust hole re-opened by a recovery command, on a node that was already fixed")
	}
	if !strings.Contains(err.Error(), EtcdPhaseDropRoot) {
		t.Errorf("refusal should say the node already completed drop-root, got: %v", err)
	}
	// The refusal is the only place an operator learns there IS a way through, so
	// it must name the escape hatch — as a WHOLE COMMAND, not a loose flag.
	//
	// A loose --force substring check passes on the prose while the command
	// itself drifts, and the failure lands in front of an operator mid-incident
	// who has been handed a command that does not exist. The command is named
	// here, so this pins the command rather than the exported Go function.
	//
	// Pinning a string here can only prove the message is self-consistent. That
	// the string is a command the BINARY actually has is asserted where the
	// command tree lives, by resolving this same constant through cobra:
	// pkg/cli, TestRefusalCommandsResolveThroughTheCommandTree. Together they
	// close the loop that single assertion alone left half-open.
	wantHatch := EtcdMigrateCommand + " --cluster --rollback --force --yes"
	if !strings.Contains(err.Error(), wantHatch) {
		t.Errorf("refusal should name the escape hatch as a whole command %q, got: %v", wantHatch, err)
	}
	// It must refuse BEFORE writing: the bundle widens first, so a guard that
	// fired after that step would have re-opened the vulnerability on its way to
	// reporting a refusal.
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Fatal("the refused rollback still widened the bundle — a kubelet cert authenticates to etcd again")
	}
	if got := EtcdMigrationState(); got != EtcdStateMigrated {
		t.Errorf("state after a refused rollback = %q, want %q", got, EtcdStateMigrated)
	}

	// The explicit opt-in is the only way through, and it really does un-migrate
	// the node — which is why it is a separate, named entry point rather than a
	// condition someone can fall into.
	if err := MigrateEtcdRollbackFromMigrated(tHost, tIP, nil); err != nil {
		t.Fatalf("explicit un-migration: %v", err)
	}
	if err := etcdTrusts(t, "kubelet"); err != nil {
		t.Errorf("the explicit un-migration did not put the cluster root back in etcd's trust: %v", err)
	}
	root := loadCert(t, "ca")
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(root); err != nil {
			t.Errorf("%s was not re-derived under the cluster root: %v", leaf, err)
		}
	}
}

// TestUnreadableTrustIsNeverAState is the pattern test: an error suppressed
// becomes a plausible wrong state. Here that means a corrupt bundle on a
// migrated node reading as "not migrated" — so trust-both would re-add the
// cluster root it had just dropped, defeating the replay guard through a
// swallowed parse error rather than by removing it.
func TestUnreadableTrustIsNeverAState(t *testing.T) {
	genInto(t)
	migrateAll(t)
	if got := EtcdMigrationState(); got != EtcdStateMigrated {
		t.Fatalf("precondition: %q", got)
	}

	// Corrupt the bundle the way a truncated write would.
	if err := os.WriteFile(EtcdBundlePath(), []byte("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := ReadEtcdTrust()
	if tr.Err == nil {
		t.Fatal("an unparseable trust bundle was read as a valid state")
	}
	if got := tr.State(); got != EtcdStateUnknown {
		t.Errorf("State() = %q on an unreadable bundle, want %q — any other answer is a guess a guard will act on", got, EtcdStateUnknown)
	}
	// The signer must fall back to the conservative answer, not to etcd-ca.
	if got := tr.EtcdSignerName(); got != "ca" {
		t.Errorf("EtcdSignerName() = %q on an unknown state, want the cluster root", got)
	}
	// And the replay guard must refuse rather than proceed.
	err := MigrateEtcdTrustBoth(true)
	if err == nil {
		t.Fatal("trust-both proceeded against an unreadable bundle — on a migrated node that re-adds the cluster root and re-opens the etcd trust hole")
	}
	if !strings.Contains(err.Error(), "cannot determine") {
		t.Errorf("refusal should say the state is undeterminable, got: %v", err)
	}
	// And so must rollback, for the same reason: "unknown" is not "not migrated",
	// and rollback on a migrated node re-derives its leaves under the cluster root.
	err = MigrateEtcdRollback(tHost, tIP, nil)
	if err == nil {
		t.Fatal("rollback proceeded against an unreadable bundle — on a migrated node that re-opens the etcd trust hole and costs the node quorum")
	}
	if !strings.Contains(err.Error(), "cannot determine") {
		t.Errorf("refusal should say the state is undeterminable, got: %v", err)
	}
}

// TestResumeSkipsAppliedPhases. The orchestrator's loop always restarts at
// trust-both, so after a follower failed at drop-root, re-running --cluster
// would hit the initiator's own replay guard with no way to resume. Skipping an
// applied phase is not weakening that guard — a phase already applied adds
// nothing for the guard to prevent.
func TestResumeSkipsAppliedPhases(t *testing.T) {
	genInto(t)
	migrateAll(t) // fully migrated: root already dropped

	// The two phases whose replay would be destructive are skipped...
	for _, phase := range []string{EtcdPhaseTrustBoth, EtcdPhaseIssueNew} {
		applied, err := EtcdPhaseApplied(phase)
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		if !applied {
			t.Errorf("%s reported as not applied on a fully-migrated node — a resumed --cluster run would re-run it and trip the replay guard", phase)
		}
	}
	// ...but drop-root is never skipped: nothing is past it, it is idempotent,
	// and it ends in the process-derived probe, so re-running re-verifies.
	if applied, err := EtcdPhaseApplied(EtcdPhaseDropRoot); err != nil || applied {
		t.Errorf("drop-root reported as skippable (%v, err=%v) — it is the phase that proves the migration landed", applied, err)
	}

	// On an unmigrated node nothing is applied, so a resumed run does the work.
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	// Nothing is skippable here. A node AT trust-both is not PAST it: its restart
	// may never have happened and the on-disk bundle cannot tell us, so a resumed
	// run re-does the phase — which is idempotent — rather than assuming.
	for phase, want := range map[string]bool{
		EtcdPhaseTrustBoth: false,
		EtcdPhaseIssueNew:  false,
		EtcdPhaseDropRoot:  false,
	} {
		got, err := EtcdPhaseApplied(phase)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("EtcdPhaseApplied(%s) = %v, want %v", phase, got, want)
		}
	}
}
