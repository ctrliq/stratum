package certs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

// its target is readable although it refuses; it must count, or the check would see one target.
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

// an unreadable target says nothing about where the node would land, so it joins no group.
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
