package ha

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

// TestSharedCertsCarryTheEtcdCA is the etcd trust hole regression test for HA
// join (etcd accepting any client certificate signed by the cluster root, which
// also signs every kubelet's certificate).
//
// Fails if the etcd CA is not in the shared set. The etcd CA has to be
// byte-identical on every control plane, because each member authorizes its
// peers against it. A joining CP without it falls through to
// EnsureCA("etcd-ca") in pki.GenerateAll and mints its OWN anchor — one no
// existing member trusts. Its etcd process starts cleanly and is rejected at
// the peer handshake, so the failure surfaces later and elsewhere, after
// MemberAdd has already registered it as a VOTING member. On a 1→2 CP
// expansion that moves quorum to 2-of-2 with one member dead, and the
// surviving control plane goes read-only.
//
// This is REQUIRED material, not rotationCerts: an optional entry would be
// skipped when absent, which is precisely the silent divergence above.
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
	// The trust bundle carries the cluster's migration state and IS optional: a
	// cluster that has not migrated has none, and a joining node must not be
	// blocked on it.
	if !slices.Contains(rotationCerts, pki.EtcdBundleBase+"-bundle.crt") {
		t.Error("rotationCerts omits the etcd trust bundle, so a CP joining mid-migration would " +
			"guess its anchor set instead of receiving the cluster's")
	}
}

// TestRequireEtcdCARefusesPreFixMaterial: a control plane running a pre-etcd-CA-split
// binary publishes shared certs with no etcd CA. Joining against it must fail
// loudly HERE rather than let this node mint a divergent anchor and discover it
// after the etcd data dir has been wiped.
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
		// The operator is on the JOINING node; the thing to fix is elsewhere. A
		// message that does not say so sends them to debug the wrong machine.
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

// TestWriteCertFilesRefusesMaterialWithoutTheEtcdCA covers the CALL SITE, not
// the predicate.
//
// TestRequireEtcdCARefusesPreFixMaterial above proves requireEtcdCA returns an
// error for material with no etcd CA. That proves nothing about whether the
// download path ever calls it — delete the call from writeCertFiles and that
// test stays green while a joining control plane once again writes shared certs
// with no etcd anchor, mints a divergent one, and is rejected by its peers after
// it is already a voting member.
//
// This is exactly the gap the merge of the path-containment guard and the etcd
// CA guard could have left: both add a guard to the same function, and a
// resolution that keeps one and drops the other still compiles and still passes every predicate-level test.
//
// It also pins the all-or-nothing property: a refusal must leave the PKI
// directory untouched, not half-written.
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
			// Nothing may have been written: the refusal happens before the first
			// os.WriteFile, so the directory should not exist at all.
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
