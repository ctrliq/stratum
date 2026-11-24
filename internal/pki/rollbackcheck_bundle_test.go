package pki

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

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

// The bundle is read against the restored root: a corrupt live ca.crt still rolls back.
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
