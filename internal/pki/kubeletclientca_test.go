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

// The kubelet's clientCAFile matches ca-bundle.crt and verifies apiserver-kubelet-client.
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

// Installed before the kubelet-client CA: the leaf is root-signed, the bundle follows the root.
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

// A split node: the kubelet trusts only the kubelet-client CA, not the root.
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

// trust-new widens the bundle to new‖old, sign-new reissues, finalize narrows it.
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

// By rename: an interrupted in-place write would leave a truncated file the kubelet cannot start.
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

// A trust-new stopped mid-way leaves the kubelet on the old root; the sign-new check re-syncs.
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

// PromoteCA checks before promoting, so sign-new never reissues under an untrusted root.
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
