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

// The same crypto/x509 call path etcd's TLS stack takes: a leaf this rejects, etcd rejects.
func etcdTrusts(t *testing.T, leaf string) error {
	t.Helper()
	_, err := loadCert(t, leaf).Verify(x509.VerifyOptions{
		Roots:     poolFromBundle(t, EtcdBundleBase),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err
}

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

// A node past drop-root with a mismatched etcd leaf pair cannot start etcd; issue-new repairs it.
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

// The pre-split layout every existing cluster upgrades from, so most risk lives here.
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

// The upgrade path is the only state where the hole was ever open, pinned as UnknownAuthorityError.
func TestKubeletCertRejectedByEtcdAfterMigration(t *testing.T) {
	unmigratedNode(t)
	// No etcd bundle yet: --trusted-ca-file was ca-bundle.crt, the trust set the hole lives in.
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
	// The legitimate client must still work, or the cluster is broken.
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("apiserver-etcd-client rejected by etcd after migration: %v", err)
	}
}

// trust-both stays fully exploitable; rejecting root-signed clients there would cut off peers.
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

// GenerateAll is skip-if-exists: a 1-CA bundle on an existing node bricks the node at restart.
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
	// Reissuing here would be a unilateral issue-new breaking peer TLS against unmigrated peers.
	if after := loadCert(t, "apiserver-etcd-client").SerialNumber; before.Cmp(after) != 0 {
		t.Error("GenerateAll reissued apiserver-etcd-client on an existing node — that is issue-new, and it is not safe to do unilaterally")
	}
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Errorf("existing root-signed client rejected after upgrade — the node is broken: %v", err)
	}
}

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
	// The state string is self-reported; assert the outcome itself too.
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

// A RotateLeaves caller re-signing etcd's leaves with the root locks out each apiserver.
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
	// And the split must still hold.
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Error("a root rotation reopened the etcd trust hole")
	}
}

// Reached only after something already went wrong, where a second failure compounds.
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
	// RollbackCA reissues under the restored root when sign-new was reached.
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

// A leaf rotation on an unmigrated node must leave etcd's certs root-signed, or it is issue-new.
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

// Minting a joining node's etcd.crt under the etcd CA while the cluster is root-signed strands it.
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
			// Shared CA material downloaded, no node-local leaves.
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

// A fan-out minting a CA per control plane looks healthy until issue-new breaks peer TLS.
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

	// With the cluster's anchor distributed (i.e. --fetch-shared worked), adopt it.
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

// Orchestrator, join signer and rotation anchors read one trust state, so answers cannot drift.
func TestTrustStateIsOneAnswer(t *testing.T) {
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	trust := ReadEtcdTrust()
	if !trust.TrustsRoot() || trust.State() != EtcdStateTrustBoth || trust.EtcdSignerName() != "ca" {
		t.Fatalf("trust-both must be self-consistent, got %+v state=%s signer=%s", trust, trust.State(), trust.EtcdSignerName())
	}
	// A rotation-replaced root is still a valid etcd client-auth anchor; counting anchors misses it.
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

// etcd trusts etcd-ca-bundle.crt, which the rotation does not maintain; sign-new is an outage.
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
	// The assertion: etcd still accepts its own apiserver, else it is locked out — the outage.
	if err := etcdTrusts(t, "apiserver-etcd-client"); err != nil {
		t.Fatalf("after rotate-ca sign-new the apiserver is locked out of its own etcd — this is the outage: %v", err)
	}
	if err := etcdTrusts(t, "etcd"); err != nil {
		t.Fatalf("after rotate-ca sign-new etcd's own server/peer cert is untrusted by its peers: %v", err)
	}

	// The old root must leave etcd's trust as it leaves Kubernetes trust: the --force case.
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

// After a rolled-back rotation the etcd leaves return to the old root, still in the stale bundle.
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

// Replayed past drop-root, trust-both re-opens the etcd trust hole while reporting success.
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

// Refusing the wrong node strands it: it never migrates and a restart then locks its apiserver out.
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

// Nothing but re-derivation puts etcd.crt back on the root; no leaf is backed up anywhere.
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
	// Peers past issue-new still present etcd-CA-signed certs, so trust stays wide.
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

// A presence-only check passes on a migrated node, where rollback also costs quorum.
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
	// The refusal must name the escape hatch as a whole command; pkg/cli resolves it through cobra.
	wantHatch := EtcdMigrateCommand + " --cluster --rollback --force --yes"
	if !strings.Contains(err.Error(), wantHatch) {
		t.Errorf("refusal should name the escape hatch as a whole command %q, got: %v", wantHatch, err)
	}
	// The bundle widens first, so the guard must fire before that step.
	if err := etcdTrusts(t, "kubelet"); err == nil {
		t.Fatal("the refused rollback still widened the bundle — a kubelet cert authenticates to etcd again")
	}
	if got := EtcdMigrationState(); got != EtcdStateMigrated {
		t.Errorf("state after a refused rollback = %q, want %q", got, EtcdStateMigrated)
	}

	// The explicit opt-in really does un-migrate the node, hence a named entry point.
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

// A suppressed error becomes a plausible wrong state that trust-both acts on.
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
	// Rollback on a migrated node re-derives its leaves under the cluster root.
	err = MigrateEtcdRollback(tHost, tIP, nil)
	if err == nil {
		t.Fatal("rollback proceeded against an unreadable bundle — on a migrated node that re-opens the etcd trust hole and costs the node quorum")
	}
	if !strings.Contains(err.Error(), "cannot determine") {
		t.Errorf("refusal should say the state is undeterminable, got: %v", err)
	}
}

// A resumed run restarts at trust-both, so an applied phase must be skippable.
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
	// ...but drop-root is never skipped: re-running it re-verifies rather than re-does.
	if applied, err := EtcdPhaseApplied(EtcdPhaseDropRoot); err != nil || applied {
		t.Errorf("drop-root reported as skippable (%v, err=%v) — it is the phase that proves the migration landed", applied, err)
	}

	// On an unmigrated node nothing is applied, so a resumed run does the work.
	unmigratedNode(t)
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	// A node AT trust-both is not PAST it: its restart may never have happened.
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
