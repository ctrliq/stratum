package pki

import (
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func pkiListing(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(version.PKIDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Name()+":"+fi.ModTime().String())
	}
	sort.Strings(names)
	return names
}

// RollbackCheck answers what RollbackCA would do and touches nothing: a clean
// staged rotation can roll back; a rotation whose old key is gone, or one
// started with --force, cannot, and the refusal names why.
func TestRollbackCheckAnswersWithoutChangingAnything(t *testing.T) {
	t.Run("a clean staged rotation", func(t *testing.T) {
		genInto(t)
		if err := RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		before := pkiListing(t)
		if err := RollbackCheck(); err != nil {
			t.Fatalf("RollbackCheck = %v, want nil", err)
		}
		if pairs := RollbackUnsafePairs(); len(pairs) != 0 {
			t.Errorf("RollbackUnsafePairs = %v, want none", pairs)
		}
		if after := pkiListing(t); !slices.Equal(before, after) {
			t.Errorf("the check changed the PKI directory:\n%v\n%v", before, after)
		}
	})
	for _, key := range []string{"ca.key", "front-proxy-ca.key", "sa.key"} {
		t.Run(key+" lost", func(t *testing.T) {
			genInto(t)
			removePKI(t, key)
			if err := RotateCAMaterial(true); err != nil {
				t.Fatal(err)
			}
			removePKI(t, caForcedMarker)
			before := pkiListing(t)
			err := RollbackCheck()
			pair := strings.TrimSuffix(key, ".key")
			if !errors.Is(err, ErrRollbackUnsafe) || !strings.Contains(err.Error(), pair) {
				t.Fatalf("RollbackCheck = %v, want ErrRollbackUnsafe naming %s", err, pair)
			}
			if pairs := RollbackUnsafePairs(); !slices.Equal(pairs, []string{pair}) {
				t.Errorf("RollbackUnsafePairs = %v, want [%s]", pairs, pair)
			}
			if after := pkiListing(t); !slices.Equal(before, after) {
				t.Errorf("the check changed the PKI directory:\n%v\n%v", before, after)
			}
		})
	}
	t.Run("a forced rotation", func(t *testing.T) {
		genInto(t)
		if err := RotateCAMaterial(true); err != nil {
			t.Fatal(err)
		}
		err := RollbackCheck()
		if !errors.Is(err, ErrRollbackUnsafe) || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("RollbackCheck = %v, want ErrRollbackUnsafe naming --force", err)
		}
	})
}

// A rollback target is the signers the rollback would leave live: the old ones
// from trust-new until finalize, and the new ones after it, when the backups
// are gone, so a finalized control plane's target differs from the others'.
func TestRollbackTargetIsTheOldSignersUntilFinalize(t *testing.T) {
	genInto(t)
	before := RollbackTarget()
	if !strings.HasPrefix(before, "ca=") || strings.Contains(before, "unreadable") {
		t.Fatalf("RollbackTarget = %q, want a fingerprint of each pair", before)
	}
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if got := RollbackTarget(); got != before {
		t.Errorf("after trust-new, RollbackTarget = %q, want the old signers %q", got, before)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if got := RollbackTarget(); got != before {
		t.Errorf("after sign-new, RollbackTarget = %q, want the old signers %q from the backups", got, before)
	}
	if err := FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	if err := RollbackCheck(); err != nil {
		t.Fatalf("a finalized node's own check = %v; it passes, which is why the cluster check compares targets", err)
	}
	if got := RollbackTarget(); got == before {
		t.Errorf("after finalize, RollbackTarget = %q, still the old signers", got)
	}
}
