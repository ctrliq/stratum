package certs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

// A control plane that finalized and one that lost an old backup key are each
// half of the one case no advice can mend: the finalized one holds only the
// new root, the other cannot return to the old one. Its target is readable
// although it refuses, so it must still count, or the check would see one
// target, take the finalized control plane for one that never ran trust-new,
// and tell the operator to restore the key and roll back every node, which
// cannot restore what finalize deleted.
func TestCheckRollbackClusterSeesAPartialFinalizeBesideALostKey(t *testing.T) {
	me, build := rollbackCheckClusterNodes(t)
	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: false, target: "ca=new"},
		"cp2": {trustsNew: true, target: "ca=old"},
		"w1":  {trustsNew: true},
	}, "cp2")
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "would not roll back to the same signers") ||
		!strings.Contains(err.Error(), "cp2 to ca=old") || !strings.Contains(err.Error(), me+" to ca=new") {
		t.Fatalf("CheckRollbackCluster = %v; want a refusal naming both targets", err)
	}
	for _, wrong := range []string{"Restore the missing key", "roll back every node", "Do not run sign-new"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("the refusal says %q, advice that cannot restore what finalize deleted: %v", wrong, err)
		}
	}
}

// A refusing control plane whose target has an unreadable half says nothing
// about where it would land, so it joins no group and the advice stays the
// lost-key one.
func TestCheckRollbackClusterIgnoresAnUnreadableTarget(t *testing.T) {
	me, build := rollbackCheckClusterNodes(t)
	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: true, target: "ca=old"},
		"cp2": {trustsNew: true, target: "ca=unreadable,sa=old"},
		"w1":  {trustsNew: true},
	}, "cp2")
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || strings.Contains(err.Error(), "would not roll back to the same signers") || !strings.Contains(err.Error(), "cp2 cannot roll back") {
		t.Fatalf("CheckRollbackCluster = %v; want the lost-key refusal, not a signer mismatch built on an unreadable target", err)
	}
}
