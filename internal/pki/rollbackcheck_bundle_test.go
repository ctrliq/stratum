package pki

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// RollbackCA reads the kubelet client-CA bundle before it restores anything,
// and a bundle it cannot read refuses the rollback there. The check must give
// the same answer: with every pair matching, a corrupt bundle would otherwise
// pass the check and fail the rollback, and the cluster-wide check would clear
// a node that cannot roll back.
func TestRollbackCheckRefusesWhatRollbackCAReadsFirst(t *testing.T) {
	genInto(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if pairs := RollbackUnsafePairs(); len(pairs) != 0 {
		t.Fatalf("the pairs match, RollbackUnsafePairs = %v", pairs)
	}
	if err := os.WriteFile(KubeletClientBundlePath(), []byte("not PEM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := pkiListing(t)

	err := RollbackCheck()
	if !errors.Is(err, ErrRollbackUnsafe) || !strings.Contains(err.Error(), KubeletClientBundlePath()) {
		t.Fatalf("RollbackCheck = %v, want ErrRollbackUnsafe naming the kubelet client-CA bundle", err)
	}
	if after := pkiListing(t); !slices.Equal(before, after) {
		t.Errorf("the check changed the PKI directory:\n%v\n%v", before, after)
	}

	rbErr := RollbackCA()
	if !errors.Is(rbErr, ErrRollbackUnsafe) || rbErr.Error() != err.Error() {
		t.Fatalf("RollbackCA = %v, want the check's refusal word for word", rbErr)
	}
	if after := pkiListing(t); !slices.Equal(before, after) {
		t.Errorf("the refused rollback changed the PKI directory:\n%v\n%v", before, after)
	}
}

// RollbackCA re-derives etcd's bundle after restoring the CA files and refuses
// an etcd bundle it cannot read there, past the point of no return. The check
// makes the same read first, so both refuse with nothing changed.
func TestRollbackCheckRefusesAnEtcdBundleRollbackCACannotRead(t *testing.T) {
	genInto(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleCrtPath(EtcdBundleBase), []byte("not PEM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := pkiListing(t)

	err := RollbackCheck()
	if !errors.Is(err, ErrRollbackUnsafe) || !strings.Contains(err.Error(), bundleCrtPath(EtcdBundleBase)) {
		t.Fatalf("RollbackCheck = %v, want ErrRollbackUnsafe naming the etcd bundle", err)
	}
	rbErr := RollbackCA()
	if !errors.Is(rbErr, ErrRollbackUnsafe) || rbErr.Error() != err.Error() {
		t.Fatalf("RollbackCA = %v, want the check's refusal word for word", rbErr)
	}
	if after := pkiListing(t); !slices.Equal(before, after) {
		t.Errorf("the refused rollback changed the PKI directory:\n%v\n%v", before, after)
	}
}

// The etcd bundle is read against the root the rollback restores, not the
// live one: a corrupt live ca.crt with intact backups still rolls back, as it
// did before the etcd check, and the restore repairs it.
func TestRollbackRestoresADamagedLiveRootPastTheEtcdCheck(t *testing.T) {
	genInto(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("ca", "crt"), []byte("not PEM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RollbackCheck(); err != nil {
		t.Fatalf("RollbackCheck = %v, want the backups to clear it", err)
	}
	if err := RollbackCA(); err != nil {
		t.Fatalf("RollbackCA = %v, want the restore", err)
	}
	if _, err := readCert("ca"); err != nil {
		t.Errorf("the restored ca.crt does not parse: %v", err)
	}
}
