package ha

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// nodeLocalPKIFiles are the PKI files a control plane deliberately keeps to
// itself: its leaf certificates (minted per node from the shared CAs) and the
// .pre-carotate backups pki's PromoteCA/PromoteSA write (pki keeps them in
// carotateScratch for RollbackCA). Every file pki writes must be in
// sharedCerts, in rotationCerts, or here — a name in none of the three is a
// trust file no joiner ever receives.
var nodeLocalPKIFiles = func() map[string]bool {
	m := map[string]bool{}
	// The leaves of LeafSpecs, plus front-proxy-client (front-proxy-CA-signed,
	// handled separately by pki). Kept literal: a new leaf must be classified
	// here on purpose, not silently inherited.
	for _, leaf := range []string{
		"etcd", "apiserver", "apiserver-etcd-client", "apiserver-kubelet-client",
		"admin", "super-admin", "controller-manager", "scheduler", "kubelet", "kube-vip",
		"front-proxy-client",
	} {
		m[leaf+".crt"] = true
		m[leaf+".key"] = true
	}
	for _, backup := range []string{
		"ca.pre-carotate.crt", "ca.pre-carotate.key",
		"front-proxy-ca.pre-carotate.crt", "front-proxy-ca.pre-carotate.key",
		"sa.pre-carotate.key", "sa.pre-carotate.pub",
		pki.KubeletClientCAName + ".pre-carotate.crt", pki.KubeletClientCAName + ".pre-carotate.key",
	} {
		m[backup] = true
	}
	// The kubelet's clientCAFile is derived on each node from ca-bundle.crt,
	// which a joiner does receive.
	m[pki.KubeletClientBundleBase+"-bundle.crt"] = true
	return m
}()

// TestEveryPKIFileIsSharedOrNodeLocal drives pki through install (GenerateAll)
// and a CA rotation up to sign-new (RotateCAMaterial, PromoteCA, PromoteSA) and
// then requires every file in the PKI directory to be classified: in the bundle
// a join receives (sharedCerts, rotationCerts) or on this node's local list
// above. A new anchor, staged file or bundle that nobody classifies — the
// defect this guards against — names itself here instead of reaching production
// and staying off every joiner unnoticed.
func TestEveryPKIFileIsSharedOrNodeLocal(t *testing.T) {
	orig := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = orig })

	if err := pki.GenerateAll("cp9", "192.0.2.9", nil); err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatalf("RotateCAMaterial: %v", err)
	}
	if err := pki.PromoteCA(); err != nil {
		t.Fatalf("PromoteCA: %v", err)
	}
	if err := pki.PromoteSA(); err != nil {
		t.Fatalf("PromoteSA: %v", err)
	}

	entries, err := os.ReadDir(version.PKIDir)
	if err != nil {
		t.Fatal(err)
	}
	// The run must have exercised the classes, or the check below passes
	// vacuously: a shared CA, a trust bundle, a leaf and a promote backup.
	for _, mustExist := range []string{"ca.crt", "ca-bundle.crt", "apiserver.crt", "ca.pre-carotate.crt"} {
		if _, err := os.Stat(filepath.Join(version.PKIDir, mustExist)); err != nil {
			t.Fatalf("the pki run wrote no %s, so the classification was not exercised: %v", mustExist, err)
		}
	}

	var unclassified []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case slices.Contains(sharedCerts, name), slices.Contains(rotationCerts, name), nodeLocalPKIFiles[name]:
		default:
			unclassified = append(unclassified, name)
		}
	}
	if len(unclassified) > 0 {
		slices.Sort(unclassified)
		t.Errorf("pki writes %q, which is neither shared with joiners (sharedCerts/rotationCerts) nor deliberately node-local — add it to the join bundle or classify it as node-local on purpose", unclassified)
	}
}
