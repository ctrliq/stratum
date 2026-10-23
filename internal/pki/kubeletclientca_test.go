package pki

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// assertKubeletTrustsAPIServer checks the kubelet's clientCAFile at one point
// of a rotation: it carries what ca-bundle.crt carries, and the apiserver's
// kubelet-client cert verifies against it, so logs, exec and port-forward
// keep working.
func assertKubeletTrustsAPIServer(t *testing.T, when string) {
	t.Helper()
	if got, want := readPKI(t, KubeletClientBundleBase+"-bundle.crt"), readPKI(t, "ca-bundle.crt"); string(got) != string(want) {
		t.Errorf("%s: kubelet clientCAFile differs from ca-bundle.crt", when)
	}
	if _, err := loadCert(t, "apiserver-kubelet-client").Verify(x509.VerifyOptions{
		Roots: poolFromBundle(t, KubeletClientBundleBase), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("%s: apiserver-kubelet-client does not verify against the kubelet clientCAFile: %v", when, err)
	}
}

// genLegacy builds a control plane installed before the kubelet-client CA: no
// kubelet-client-ca, apiserver-kubelet-client signed by the cluster root, and a
// kubelet bundle that follows ca-bundle.crt.
func genLegacy(t *testing.T) {
	t.Helper()
	genInto(t)
	for _, ext := range []string{"crt", "key"} {
		if err := os.Remove(path(KubeletClientCAName, ext)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := loadCA("ca")
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range LeafSpecs(tHost, tIP, nil) {
		if sp.Name == "apiserver-kubelet-client" {
			if err := root.Reissue(sp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(KubeletClientBundlePath(), readPKI(t, "ca-bundle.crt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if split, err := KubeletClientSplit(); err != nil || split {
		t.Fatalf("genLegacy: KubeletClientSplit = %v, %v, want an unsplit node", split, err)
	}
}

// assertSplit checks a split node: the kubelet trusts the kubelet-client CA and
// nothing in ca-bundle.crt, and apiserver-kubelet-client verifies against the
// kubelet's bundle and not against the root.
func assertSplit(t *testing.T, when string) {
	t.Helper()
	if split, err := KubeletClientSplit(); err != nil || !split {
		t.Errorf("%s: KubeletClientSplit = %v, %v, want split", when, split, err)
	}
	kubelet := loadBundle(t, KubeletClientBundleBase)
	if len(kubelet) != 1 || !kubelet[0].Equal(loadCert(t, KubeletClientCAName)) {
		t.Errorf("%s: the kubelet clientCAFile holds %d CAs, want %s alone", when, len(kubelet), KubeletClientCAName)
	}
	leaf := loadCert(t, "apiserver-kubelet-client")
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: poolFromBundle(t, KubeletClientBundleBase), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("%s: apiserver-kubelet-client does not verify against the kubelet clientCAFile: %v", when, err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: poolFromBundle(t, "ca"), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Errorf("%s: apiserver-kubelet-client verifies against ca-bundle.crt; it must be signed by %s alone", when, KubeletClientCAName)
	}
}

// A new cluster's first control plane is split from birth: no certificate the
// cluster root signs can authenticate to a kubelet.
func TestGenerateAllSplitsANewCluster(t *testing.T) {
	genInto(t)
	assertSplit(t, "fresh install")
	before := readPKI(t, KubeletClientBundleBase+"-bundle.crt")
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readPKI(t, KubeletClientBundleBase+"-bundle.crt")) {
		t.Error("a second GenerateAll rewrote the split kubelet bundle")
	}
	assertSplit(t, "restart")
}

// A rotation on a split node rotates the kubelet-client CA with the root:
// trust-new widens the kubelet bundle to new‖old, sign-new reissues
// apiserver-kubelet-client under the new CA, and finalize drops the old one.
// The root never enters the kubelet bundle.
func TestCARotationRotatesTheKubeletClientCA(t *testing.T) {
	genInto(t)
	old := loadCert(t, KubeletClientCAName)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	staged := loadCert(t, KubeletClientCAName+"-new")
	if b := loadBundle(t, KubeletClientBundleBase); len(b) != 2 || !b[0].Equal(staged) || !b[1].Equal(old) {
		t.Fatalf("trust-new: the kubelet bundle holds %d CAs, want the staged and the live kubelet-client CA", len(b))
	}
	if split, err := KubeletClientSplit(); err != nil || !split {
		t.Fatalf("trust-new: KubeletClientSplit = %v, %v", split, err)
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
	if !loadCert(t, KubeletClientCAName).Equal(staged) {
		t.Fatal("sign-new did not promote the staged kubelet-client CA")
	}
	if err := loadCert(t, "apiserver-kubelet-client").CheckSignatureFrom(staged); err != nil {
		t.Errorf("sign-new: apiserver-kubelet-client is not signed by the new kubelet-client CA: %v", err)
	}
	if err := FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	assertSplit(t, "finalize")
	for _, c := range loadBundle(t, KubeletClientBundleBase) {
		if c.Equal(old) {
			t.Error("finalize: the kubelet bundle still trusts the old kubelet-client CA")
		}
	}
	for _, f := range []string{KubeletClientCAName + "-new.crt", KubeletClientCAName + ".pre-carotate.key"} {
		if _, err := os.Stat(path(f[:len(f)-4], f[len(f)-3:])); err == nil {
			t.Errorf("finalize left %s behind", f)
		}
	}
}

// A rollback after sign-new restores the old kubelet-client CA and a bundle of
// it alone, and the leaf reissue puts apiserver-kubelet-client back under it.
func TestCARollbackRestoresTheKubeletClientCA(t *testing.T) {
	genInto(t)
	old := loadCert(t, KubeletClientCAName)
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
	if err := RollbackCA(); err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if !loadCert(t, KubeletClientCAName).Equal(old) {
		t.Fatal("rollback did not restore the old kubelet-client CA")
	}
	assertSplit(t, "rollback")
}

// A trust-new that stopped before writing the kubelet bundle is repaired by
// sign-new, so the reissued apiserver-kubelet-client is accepted.
func TestPromoteCARepairsAKubeletBundleTrustNewMissed(t *testing.T) {
	genInto(t)
	want := readPKI(t, KubeletClientBundleBase+"-bundle.crt")
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KubeletClientBundlePath(), want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if got := len(loadBundle(t, KubeletClientBundleBase)); got != 2 {
		t.Fatalf("sign-new left the kubelet bundle with %d CAs, want new‖old", got)
	}
}

// A split control plane that lost kubelet-client-ca.key rotates only with
// --force, and a lost kubelet-client-ca.crt refuses even then.
func TestRotateCAMaterialNeedsTheKubeletClientCA(t *testing.T) {
	genInto(t)
	if err := os.Remove(path(KubeletClientCAName, "key")); err != nil {
		t.Fatal(err)
	}
	if err := RotateCAMaterial(false); err == nil {
		t.Fatal("trust-new went ahead without kubelet-client-ca.key and without --force")
	}
	if err := RotateCAMaterial(true); err != nil {
		t.Fatalf("forced trust-new without kubelet-client-ca.key: %v", err)
	}

	genInto(t)
	if err := os.Remove(path(KubeletClientCAName, "crt")); err != nil {
		t.Fatal(err)
	}
	if err := RotateCAMaterial(true); !errors.Is(err, ErrNoOldPublicHalf) {
		t.Fatalf("forced trust-new without kubelet-client-ca.crt = %v, want ErrNoOldPublicHalf", err)
	}
}

// A split control plane that lost its kubelet bundle rebuilds it from the
// kubelet-client CA on its next start, never from the root.
func TestGenerateAllRebuildsALostSplitBundleFromTheKubeletClientCA(t *testing.T) {
	genInto(t)
	if err := os.Remove(KubeletClientBundlePath()); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	assertSplit(t, "rebuilt")
}

// A split control plane that lost its bundle still reads as split, so nothing
// publishes, shares or re-syncs it as if the cluster predated the split.
func TestKubeletClientSplitWithoutItsBundle(t *testing.T) {
	genInto(t)
	if err := os.Remove(KubeletClientBundlePath()); err != nil {
		t.Fatal(err)
	}
	if split, err := KubeletClientSplit(); err != nil || !split {
		t.Fatalf("KubeletClientSplit = %v, %v; want split", split, err)
	}
	if err := SyncKubeletClientBundle(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(KubeletClientBundlePath()); !os.IsNotExist(err) {
		t.Error("SyncKubeletClientBundle wrote a bundle from the root on a split control plane")
	}
}

// An unreadable bundle on a split control plane is refused, not re-synced from
// the root, which would undo the split.
func TestSyncRefusesAnUnreadableSplitBundle(t *testing.T) {
	genInto(t)
	if err := os.WriteFile(KubeletClientBundlePath(), []byte("not PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncKubeletClientBundle(); err == nil {
		t.Fatal("SyncKubeletClientBundle re-synced an unreadable bundle on a split control plane")
	}
	if got := readPKI(t, KubeletClientBundleBase+"-bundle.crt"); string(got) != "not PEM" {
		t.Errorf("the refused sync still rewrote the bundle to %q", got)
	}
}

func TestKubeletClientBundleFollowsTheRootThroughRotation(t *testing.T) {
	genLegacy(t)
	assertKubeletTrustsAPIServer(t, "fresh install")

	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	assertKubeletTrustsAPIServer(t, "trust-new")
	if got := len(loadBundle(t, KubeletClientBundleBase)); got != 2 {
		t.Errorf("trust-new: kubelet clientCAFile has %d CAs, want old+new", got)
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
	assertKubeletTrustsAPIServer(t, "sign-new")

	oldCA := loadBundle(t, "ca")[1]
	if err := FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	assertKubeletTrustsAPIServer(t, "finalize")
	for _, c := range loadBundle(t, KubeletClientBundleBase) {
		if c.Equal(oldCA) {
			t.Error("finalize: the kubelet clientCAFile still trusts the dropped root")
		}
	}
}

func TestKubeletClientBundleFollowsARollback(t *testing.T) {
	genLegacy(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := RollbackCA(); err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	assertKubeletTrustsAPIServer(t, "rollback")
	if got := len(loadBundle(t, KubeletClientBundleBase)); got != 1 {
		t.Errorf("rollback: kubelet clientCAFile has %d CAs, want the restored root only", got)
	}
}

// A control plane installed before the kubelet had its own clientCAFile has a
// ca-bundle.crt and no kubelet bundle; GenerateAll on its next start seeds one.
func TestGenerateAllSeedsTheKubeletClientBundleOnAnExistingNode(t *testing.T) {
	genLegacy(t)
	if err := os.Remove(bundleCrtPath(KubeletClientBundleBase)); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	assertKubeletTrustsAPIServer(t, "upgraded control plane")
}

func TestEnsureKubeletClientBundleKeepsAnExistingOne(t *testing.T) {
	version.PKIDir = t.TempDir()
	if err := os.WriteFile(bundleCrtPath("ca"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KubeletClientBundlePath(), []byte("own"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureKubeletClientBundle(); err != nil {
		t.Fatal(err)
	}
	if got := readPKI(t, KubeletClientBundleBase+"-bundle.crt"); string(got) != "own" {
		t.Errorf("EnsureKubeletClientBundle rewrote an existing bundle to %q", got)
	}
}

func TestEnsureKubeletClientBundleNeedsTheRootBundle(t *testing.T) {
	version.PKIDir = t.TempDir()
	if err := EnsureKubeletClientBundle(); err == nil {
		t.Fatal("EnsureKubeletClientBundle succeeded with no ca-bundle.crt to copy")
	}
	if _, err := os.Stat(KubeletClientBundlePath()); !os.IsNotExist(err) {
		t.Errorf("a kubelet clientCAFile was written with nothing to fill it: %v", err)
	}
}

// The bundle is replaced by rename, not rewritten in place: an in-place write
// interrupted part way would leave a truncated file that
// EnsureKubeletClientBundle keeps and the kubelet cannot start on.
func TestSyncKubeletClientBundleReplacesByRename(t *testing.T) {
	genLegacy(t)
	before, err := os.Stat(KubeletClientBundlePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := SyncKubeletClientBundle(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(KubeletClientBundlePath())
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the kubelet clientCAFile was rewritten in place, not replaced by rename")
	}
	if after.Mode().Perm() != 0o644 {
		t.Errorf("the kubelet clientCAFile is %o, want 644", after.Mode().Perm())
	}
	assertKubeletTrustsAPIServer(t, "after a re-sync")
}

// A trust-new that stopped after widening ca-bundle.crt but before syncing the
// kubelet's bundle leaves the kubelet trusting only the old root; the check
// sign-new runs first re-syncs it, so the reissued apiserver-kubelet-client is
// accepted.
func TestEnsureKubeletClientTrustsResyncsAStaleBundle(t *testing.T) {
	genLegacy(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KubeletClientBundlePath(), readPKI(t, "ca.crt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureKubeletClientTrusts("ca", "ca-new"); err != nil {
		t.Fatal(err)
	}
	if got := len(loadBundle(t, KubeletClientBundleBase)); got != 2 {
		t.Errorf("kubelet clientCAFile has %d CAs after the re-sync, want old+new", got)
	}

	if err := os.WriteFile(KubeletClientBundlePath(), []byte("not PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureKubeletClientTrusts("ca", "ca-new"); err != nil {
		t.Fatalf("an unreadable kubelet bundle is re-synced, not refused: %v", err)
	}

	for _, p := range []string{bundleCrtPath("ca"), KubeletClientBundlePath()} {
		if err := os.WriteFile(p, readPKI(t, "ca.crt"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnsureKubeletClientTrusts("ca", "ca-new"); err == nil || !strings.Contains(err.Error(), "ca-new.crt") {
		t.Errorf("a root ca-bundle.crt lacks too must be refused, naming it; got %v", err)
	}
}

// PromoteCA runs the check before it promotes anything, so sign-new never
// reissues apiserver-kubelet-client under a root the kubelet does not trust.
func TestPromoteCAResyncsAStaleKubeletClientBundle(t *testing.T) {
	genLegacy(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KubeletClientBundlePath(), readPKI(t, "ca.crt"), 0o644); err != nil {
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
	assertKubeletTrustsAPIServer(t, "sign-new after a stopped trust-new")
}
