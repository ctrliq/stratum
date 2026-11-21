package ha

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Every file pki writes must land in sharedCerts, rotationCerts or here.
var nodeLocalPKIFiles = func() map[string]bool {
	m := map[string]bool{}
	// Kept literal: a new leaf must be classified here on purpose, not silently inherited.
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
	// Derived on each node from ca-bundle.crt, which a joiner does receive.
	m[pki.KubeletClientBundleBase+"-bundle.crt"] = true
	return m
}()

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
	// A vacuous pass would look clean: the run must have written a shared CA, a leaf, a backup.
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
