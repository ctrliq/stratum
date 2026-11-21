package ha

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

func TestSharedCertsCarryTheEtcdCA(t *testing.T) {
	for _, want := range []string{pki.EtcdCAName + ".crt", pki.EtcdCAName + ".key"} {
		if !slices.Contains(sharedCerts, want) {
			t.Errorf("sharedCerts is missing %q — a joining control plane would mint a divergent "+
				"etcd anchor and be rejected by its peers after it is already a voting member", want)
		}
		if slices.Contains(rotationCerts, want) {
			t.Errorf("%q is in rotationCerts, which is skipped when absent; it must be REQUIRED", want)
		}
	}
	// Optional: a cluster that has not migrated has none, and a joiner must not block on it.
	if !slices.Contains(rotationCerts, pki.EtcdBundleBase+"-bundle.crt") {
		t.Error("rotationCerts omits the etcd trust bundle, so a CP joining mid-migration would " +
			"guess its anchor set instead of receiving the cluster's")
	}
}

func TestRequireEtcdCARefusesPreFixMaterial(t *testing.T) {
	full := map[string][]byte{
		pki.EtcdCAName + ".crt": []byte("x"),
		pki.EtcdCAName + ".key": []byte("y"),
	}
	if err := requireEtcdCA(full); err != nil {
		t.Fatalf("complete material rejected: %v", err)
	}
	for _, drop := range []string{pki.EtcdCAName + ".crt", pki.EtcdCAName + ".key"} {
		partial := map[string][]byte{}
		for k, v := range full {
			if k != drop {
				partial[k] = v
			}
		}
		err := requireEtcdCA(partial)
		if err == nil {
			t.Errorf("material missing %s was accepted", drop)
			continue
		}
		// The operator is on the joining node; the thing to fix is elsewhere.
		if !strings.Contains(err.Error(), "republish") {
			t.Errorf("refusal for missing %s does not tell the operator to republish from the "+
				"upgraded control plane: %v", drop, err)
		}
	}
	// An empty value must count as absent: a zero-length key is not a CA.
	if err := requireEtcdCA(map[string][]byte{
		pki.EtcdCAName + ".crt": {}, pki.EtcdCAName + ".key": []byte("y"),
	}); err == nil {
		t.Error("a zero-length etcd CA certificate was accepted as present")
	}
}

// Pins the call site: writeCertFiles refusing material with no etcd CA, all or nothing.
func TestWriteCertFilesRefusesMaterialWithoutTheEtcdCA(t *testing.T) {
	for _, drop := range []string{pki.EtcdCAName + ".crt", pki.EtcdCAName + ".key"} {
		t.Run("missing "+drop, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "pki")
			files := uploadSet(t)
			if _, ok := files[drop]; !ok {
				t.Fatalf("fixture has no %s, so this test would pass vacuously", drop)
			}
			delete(files, drop)

			if err := writeCertFiles(dir, files); err == nil {
				t.Fatal("writeCertFiles accepted shared certs with no etcd CA — requireEtcdCA is " +
					"not wired into the write path")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				entries, _ := os.ReadDir(dir)
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("a refused write left %d file(s) behind: %v", len(names), names)
			}
		})
	}
}
