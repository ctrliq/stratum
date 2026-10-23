package certs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

var rootBackups = []string{"ca.pre-carotate.crt", "ca.pre-carotate.key"}

func pkiBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.PKIDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// completedRotation runs A→B through finalize and returns B's certificate and the
// backups of A that an older release's unchecked unlink left behind.
func completedRotation(t *testing.T) (liveB []byte, residue map[string][]byte) {
	t.Helper()
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	residue = map[string][]byte{}
	for _, f := range rootBackups {
		residue[f] = pkiBytes(t, f)
	}
	if err := pki.FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	return pkiBytes(t, "ca.crt"), residue
}

func restoreResidue(t *testing.T, residue map[string][]byte) {
	t.Helper()
	for f, b := range residue {
		if err := os.WriteFile(filepath.Join(version.PKIDir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A→B finished, but its finalize left A's backups; B→C then rolls back before
// sign-new. The rollback must land on B, the rotation's starting signer.
func TestObsoleteBackupsDoNotRestoreWrongCA(t *testing.T) {
	kubeVIPNode(t)
	liveB, residue := completedRotation(t)
	restoreResidue(t, residue)

	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	for _, f := range rootBackups {
		if fileExists(filepath.Join(version.PKIDir, f)) {
			t.Errorf("trust-new kept %s from the finished A→B rotation", f)
		}
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if got := pkiBytes(t, "ca.crt"); !bytes.Equal(got, liveB) {
		t.Fatal("rollback after only trust-new restored the obsolete CA instead of the rotation's starting CA")
	}
	assertLeafSignedByLiveRoot(t)
}

// The residue is also reconciled when finalize's own marker is what is left.
func TestObsoleteBackupsReconciled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"finalized marker left", func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(version.PKIDir, "carotate-finalized"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeVIPNode(t)
			_, residue := completedRotation(t)
			restoreResidue(t, residue)
			tc.setup(t)
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			for _, f := range rootBackups {
				if fileExists(filepath.Join(version.PKIDir, f)) {
					t.Errorf("%s survived", f)
				}
			}
		})
	}
}

// Recovery material of an active or half-finished rotation is not residue.
func TestRotateCAMaterialKeepsRecoveryMaterial(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T)
		refuses bool
	}{
		{"promoted and marked", func(t *testing.T) {
			promoteAndReissue(t)
			if err := pki.MarkCAPromoted(); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"promoted, restart failed before the marker", promoteAndReissue, true},
		{"rolled back, leaves and cleanup pending", func(t *testing.T) {
			promoteAndReissue(t)
			if err := pki.RollbackCA(); err != nil { // restored; the repair and cleanup have not run
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeVIPNode(t)
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			tc.setup(t)
			before := map[string][]byte{}
			for _, f := range rootBackups {
				before[f] = pkiBytes(t, f)
			}
			marked := pki.CAPromoted()

			err := pki.RotateCAMaterial(false)
			if tc.refuses != (err != nil) {
				t.Fatalf("RotateCAMaterial err = %v, refuses = %v", err, tc.refuses)
			}
			for f, b := range before {
				if got, err := os.ReadFile(filepath.Join(version.PKIDir, f)); err != nil || !bytes.Equal(got, b) {
					t.Errorf("%s changed or went missing: %v", f, err)
				}
			}
			if pki.CAPromoted() != marked {
				t.Errorf("promoted marker = %v, want %v", pki.CAPromoted(), marked)
			}
		})
	}
}

// --fetch-shared skips checkCARotationState, so a promoted node must be stopped
// before the fetch re-stages ca-new and clears the promoted marker: a following
// sign-new would overwrite ca.pre-carotate.* with the already-new CA.
func TestFetchSharedOnPromotedNodeKeepsRollbackBackup(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	backup := pkiBytes(t, "ca.pre-carotate.crt")

	old := fetchStagedMaterial
	t.Cleanup(func() { fetchStagedMaterial = old })
	fetchStagedMaterial = func() error {
		// What the real fetch does: lay the initiator's staged material down.
		for _, f := range []string{"ca-new.crt", "ca-new.key"} {
			if err := os.WriteFile(filepath.Join(version.PKIDir, f), []byte("staged"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	_ = cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, FetchShared: true, Quiet: true})

	if !pki.CAPromoted() {
		t.Error("fetch-shared trust-new cleared the promoted marker")
	}
	if fileExists(filepath.Join(version.PKIDir, "ca-new.crt")) {
		t.Error("fetch-shared trust-new staged a new CA over a promoted node")
	}
	if got := pkiBytes(t, "ca.pre-carotate.crt"); !bytes.Equal(got, backup) {
		t.Error("the rollback backup changed")
	}
	if err := checkCPFinalize(); err != nil {
		t.Errorf("finalize refused after the no-op: %v", err)
	}
	if err := pki.RotateCAMaterial(false); err == nil || !strings.Contains(err.Error(), "promoted") {
		t.Errorf("RotateCAMaterial on a promoted node = %v, want a refusal naming the promotion", err)
	}
}

// A follower's --fetch-shared lays the initiator's staged root down before
// RotateCAMaterial runs, which would make an idle node look mid-rotation and
// keep the obsolete backups. They must go first, so a rollback before sign-new
// lands on B.
func TestFetchSharedReconcilesObsoleteBackups(t *testing.T) {
	kubeVIPNode(t)
	liveB, residue := completedRotation(t)
	// Capture the initiator's C material, then return this node to idle B.
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	staged := map[string][]byte{}
	for _, name := range pki.CAStagedFiles() {
		staged[name] = pkiBytes(t, name)
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	restoreResidue(t, residue)

	old := fetchStagedMaterial
	t.Cleanup(func() { fetchStagedMaterial = old })
	fetchStagedMaterial = func() error {
		for name, b := range staged {
			if err := os.WriteFile(filepath.Join(version.PKIDir, name), b, 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, FetchShared: true, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pkiBytes(t, "ca.crt"), liveB) {
		t.Fatal("fetch-shared rollback restored the obsolete CA instead of the rotation's starting CA")
	}
}

// A promoted node that already holds a full staged pair again (a repeated
// staging) must be refused before the fetch writes anything, or the refusal's
// "nothing has been changed" is false.
func TestFetchSharedRefusesPromotedNodeBeforeFetching(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	staged := map[string][]byte{}
	for _, name := range pki.CAStagedFiles() {
		staged[name] = pkiBytes(t, name)
	}
	promoteAndReissue(t)
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	for name, b := range staged {
		if err := os.WriteFile(filepath.Join(version.PKIDir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backup := pkiBytes(t, "ca.pre-carotate.crt")

	old := fetchStagedMaterial
	t.Cleanup(func() { fetchStagedMaterial = old })
	fetched := false
	fetchStagedMaterial = func() error {
		fetched = true
		return os.WriteFile(filepath.Join(version.PKIDir, "ca-new.key"), []byte("changed by fetch"), 0o600)
	}
	err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, FetchShared: true, Quiet: true})
	if err == nil || !strings.Contains(err.Error(), "nothing has been changed") {
		t.Fatalf("err = %v, want the promoted refusal", err)
	}
	if fetched {
		t.Error("the fetch ran before the refusal")
	}
	if !bytes.Equal(pkiBytes(t, "ca-new.key"), staged["ca-new.key"]) {
		t.Error("ca-new.key changed")
	}
	if !pki.CAPromoted() || !bytes.Equal(pkiBytes(t, "ca.pre-carotate.crt"), backup) {
		t.Error("the promotion marker or the rollback backup changed")
	}
}

// A node marked promoted with new CA material staged again (a repeated
// staging) cannot finish forward: sign-new skips as applied and finalize refuses
// the staged files. The supported way out is the rollback, which restores the
// original CA from the backups the staging refusal protects.
func TestRestagedPromotedNodeRollsBackToOriginalCA(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	liveA := pkiBytes(t, "ca.crt")
	staged := map[string][]byte{}
	for _, name := range pki.CAStagedFiles() {
		staged[name] = pkiBytes(t, name)
	}
	promoteAndReissue(t)
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	for name, b := range staged {
		if err := os.WriteFile(filepath.Join(version.PKIDir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Neither forward phase can finish this node, and the refusal says so.
	var out bytes.Buffer
	if err := cpSignNew(&out, CARotateOptions{Phase: PhaseSignNew, Quiet: false}); err != nil || !strings.Contains(out.String(), "sign-new is already applied") {
		t.Fatalf("sign-new = %v, %q; want it skipped as applied", err, out.String())
	}
	if err := checkCPFinalize(); err == nil || !strings.Contains(err.Error(), "still staged") {
		t.Fatalf("finalize = %v, want it refused for the staged files", err)
	}
	err := pki.PrepareStaging(false)
	if err == nil || !strings.Contains(err.Error(), "--rollback --cluster --check") || !strings.Contains(err.Error(), "do not finalize") || strings.Contains(err.Error(), "Finish the rotation with") {
		t.Fatalf("PrepareStaging = %v, want the rollback route and no finalize advice", err)
	}

	// The route: the node's own check, then the rollback.
	if err := cpRollbackCheck(); err != nil { // what `--rollback --check` answers on a control plane
		t.Fatalf("cpRollbackCheck = %v", err)
	}
	target := pki.RollbackTarget()
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pkiBytes(t, "ca.crt"), liveA) {
		t.Fatal("rollback did not restore the original CA")
	}
	if got := pki.RollbackTarget(); got != target {
		t.Errorf("the check named %s as the rollback target, the node holds %s", target, got)
	}
	assertLeafSignedByLiveRoot(t)
	if left := pki.CAStagedFiles(); len(left) > 0 {
		t.Errorf("staged files remain: %v", left)
	}
	if pki.CAPromoted() || pki.CAFinalized() || fileExists(filepath.Join(version.PKIDir, "ca.pre-carotate.crt")) {
		t.Error("a marker or backup survived the rollback")
	}
	if n, err := pki.CABundleCertCount("ca"); err != nil || n != 1 {
		t.Errorf("bundle anchors = %d, %v; want 1", n, err)
	}

	// Rotation can start again from the restored CA.
	if err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}); err != nil {
		t.Fatalf("trust-new after the rollback: %v", err)
	}
	if !pki.CARotationStaged() {
		t.Error("trust-new staged nothing")
	}
}

// A promoted node with nothing staged still finishes forward, so its refusal
// names finalize as well as the rollback.
func TestPrepareStagingRefusalOnPromotedNodeWithoutStagedFiles(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	err := pki.PrepareStaging(false)
	if err == nil || !strings.Contains(err.Error(), "--phase=finalize") || !strings.Contains(err.Error(), "--rollback --cluster --check") {
		t.Fatalf("PrepareStaging = %v, want finalize and the rollback route", err)
	}
}

// A live file that cannot be read says nothing about its backup, so none are
// removed: deleting the one that differs would destroy the only copy of a key.
func TestObsoleteBackupsKeptWhenLiveFileCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		brk  func(t *testing.T, live string)
	}{
		{"missing", func(t *testing.T, live string) {
			if err := os.Remove(live); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", func(t *testing.T, live string) {
			if err := os.Remove(live); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(live, "keep"), 0o700); err != nil { // reading a directory fails everywhere
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeVIPNode(t)
			_, residue := completedRotation(t)
			restoreResidue(t, residue)
			live := filepath.Join(version.PKIDir, "ca.key")
			liveKey := pkiBytes(t, "ca.key")
			tc.brk(t, live)

			err := pki.PrepareStaging(false)
			if err == nil || !strings.Contains(err.Error(), "nothing has been changed") {
				t.Fatalf("PrepareStaging = %v, want a refusal that removed nothing", err)
			}
			for f, b := range residue {
				if !bytes.Equal(pkiBytes(t, f), b) {
					t.Errorf("%s changed", f)
				}
			}

			// With the live file readable again, the obsolete backups go.
			if err := os.RemoveAll(live); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(live, liveKey, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := pki.PrepareStaging(false); err != nil {
				t.Fatal(err)
			}
			for _, f := range rootBackups {
				if fileExists(filepath.Join(version.PKIDir, f)) {
					t.Errorf("%s survived once the live file could be read", f)
				}
			}
		})
	}
}

// A node whose sign-new promoted but stopped before it recorded the promotion
// resumes with sign-new, and the files still staged are the rest of that
// promotion: its refusal sends the operator to sign-new, not to a rollback alone.
func TestPrepareStagingOnUnmarkedPromotionPointsToSignNew(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteCA(); err != nil { // sign-new stopped here: sa-new.* is still staged
		t.Fatal(err)
	}
	if !pki.SignNewResumable() || len(pki.CAStagedFiles()) == 0 || pki.CAPromoted() {
		t.Fatalf("setup: resumable=%v staged=%v marked=%v", pki.SignNewResumable(), pki.CAStagedFiles(), pki.CAPromoted())
	}
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted an unrecorded promotion")
	}
	err := pki.PrepareStaging(false)
	if err == nil || !strings.Contains(err.Error(), "--phase=sign-new") || strings.Contains(err.Error(), "do not finalize this node") {
		t.Fatalf("PrepareStaging = %v, want the sign-new retry and no do-not-finalize", err)
	}
	if !strings.Contains(err.Error(), "--rollback --cluster --check") {
		t.Errorf("PrepareStaging = %v, want the rollback route as the other way out", err)
	}
}

// A forced recovery may have lost an old key, and the backup of it may be the
// only copy: with --force a missing live file keeps every backup and does not
// block staging, locally or through --fetch-shared. Without it, the same state
// refuses (TestObsoleteBackupsKeptWhenLiveFileCannotBeRead).
func TestForcedStagingKeepsBackupsWhenLiveKeyIsMissing(t *testing.T) {
	setup := func(t *testing.T) map[string][]byte {
		t.Helper()
		kubeVIPNode(t)
		_, residue := completedRotation(t)
		restoreResidue(t, residue)
		if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
			t.Fatal(err)
		}
		return residue
	}
	intact := func(t *testing.T, residue map[string][]byte) {
		t.Helper()
		for f, b := range residue {
			if !bytes.Equal(pkiBytes(t, f), b) {
				t.Errorf("%s changed", f)
			}
		}
	}

	t.Run("local staging", func(t *testing.T) {
		residue := setup(t)
		if err := pki.RotateCAMaterial(true); err != nil {
			t.Fatalf("forced staging with the live ca.key missing: %v", err)
		}
		intact(t, residue)
	})
	t.Run("fetch-shared", func(t *testing.T) {
		residue := setup(t)
		oldRun := stagedRotationRunID // a finalized node asks the cluster which run is staged
		t.Cleanup(func() { stagedRotationRunID = oldRun })
		stagedRotationRunID = func() (string, error) { return "", nil }
		old := fetchStagedMaterial
		t.Cleanup(func() { fetchStagedMaterial = old })
		fetched := false
		fetchStagedMaterial = func() error { fetched = true; return errors.New("stop after the fetch is reached") }
		err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, FetchShared: true, Force: true, Quiet: true})
		if !fetched {
			t.Fatalf("a forced --fetch-shared was stopped before the fetch: %v", err)
		}
		intact(t, residue)
	})
}

// One backup equal to its live file shows a rollback that restored but has not
// finished (its retry repairs the leaves from the backups), so the backups that
// still differ are its too: interrupted finalize, then a rollback that restored
// the root, then trust-new --fetch-shared must delete none of them.
func TestUnfinishedRollbackKeepsEveryBackup(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	backups := map[string][]byte{}
	for _, f := range []string{"ca", "front-proxy-ca"} {
		for _, ext := range []string{"crt", "key"} {
			backups[f+".pre-carotate."+ext] = pkiBytes(t, f+".pre-carotate."+ext)
		}
	}
	for _, ext := range []string{"key", "pub"} {
		backups["sa.pre-carotate."+ext] = pkiBytes(t, "sa.pre-carotate."+ext)
	}
	if err := pki.FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	restoreResidue(t, backups)
	// The rollback restored the root only: it is equal to its backup, while the
	// front-proxy and SA backups still differ from their live files.
	for _, ext := range []string{"crt", "key"} {
		if err := os.WriteFile(filepath.Join(version.PKIDir, "ca."+ext), backups["ca.pre-carotate."+ext], 0o600); err != nil {
			t.Fatal(err)
		}
	}

	oldRun := stagedRotationRunID // a finalized node asks the cluster which run is staged
	t.Cleanup(func() { stagedRotationRunID = oldRun })
	stagedRotationRunID = func() (string, error) { return "", nil }
	old := fetchStagedMaterial
	t.Cleanup(func() { fetchStagedMaterial = old })
	fetched := false
	fetchStagedMaterial = func() error { fetched = true; return errors.New("stop after the fetch is reached") }
	err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, FetchShared: true, Quiet: true})
	if !fetched {
		t.Fatalf("--fetch-shared was stopped before the fetch: %v", err)
	}
	for f, b := range backups {
		got, rerr := os.ReadFile(filepath.Join(version.PKIDir, f))
		if rerr != nil || !bytes.Equal(got, b) {
			t.Errorf("%s was deleted or changed: %v", f, rerr)
		}
	}
}

// A crash mid-staging leaves half of a staged pair. That node is not idle, so
// its backups are kept even though the bundle has one anchor, no marker is set
// and every backup differs from its live file.
func TestHalfStagedPairKeepsBackups(t *testing.T) {
	for _, name := range []string{"ca-new.crt", "ca-new.key", "sa-new.key"} {
		t.Run(name, func(t *testing.T) {
			kubeVIPNode(t)
			_, residue := completedRotation(t)
			restoreResidue(t, residue)
			if err := os.WriteFile(filepath.Join(version.PKIDir, name), []byte("half a pair"), 0o600); err != nil {
				t.Fatal(err)
			}
			if pki.CARotationStaged() {
				t.Fatal("setup: the staged pair is complete")
			}

			if err := pki.PrepareStaging(false); err != nil {
				t.Fatal(err)
			}
			for f, b := range residue {
				if got, err := os.ReadFile(filepath.Join(version.PKIDir, f)); err != nil || !bytes.Equal(got, b) {
					t.Errorf("%s was deleted or changed: %v", f, err)
				}
			}
		})
	}
}
