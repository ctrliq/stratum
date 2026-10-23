package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/pki"
)

// `rotate-ca --rollback --check` is read-only: it needs no --yes, runs the
// node check or the cluster-wide one, and turns a refusal into the exit
// status the cluster-wide check reads from each node's Job. A rollback with
// --cluster and no --check is refused, because the rollback itself stays per
// node.
func TestRotateCARollbackCheck(t *testing.T) {
	oldNode, oldCluster := rollbackCheck, rollbackCheckCluster
	t.Cleanup(func() { rollbackCheck, rollbackCheckCluster = oldNode, oldCluster })
	var ran []string
	var verdict error
	rollbackCheck = func(io.Writer) error { ran = append(ran, "node"); return verdict }
	rollbackCheckCluster = func(context.Context, io.Writer) error { ran = append(ran, "cluster"); return verdict }

	run := func(args ...string) error {
		ran = nil
		cmd := newCertsRotateCACommand()
		cmd.SetOut(io.Discard)
		cmd.SetArgs(args)
		return cmd.Execute()
	}

	if err := run("--rollback", "--check"); err != nil || !strings.Contains(strings.Join(ran, ","), "node") {
		t.Errorf("--rollback --check = %v, ran %v; want the node check without --yes", err, ran)
	}
	if err := run("--rollback", "--cluster", "--check"); err != nil || strings.Join(ran, ",") != "cluster" {
		t.Errorf("--rollback --cluster --check = %v, ran %v; want the cluster-wide check", err, ran)
	}
	if err := run("--check"); err == nil || !strings.Contains(err.Error(), "--check goes with --rollback") || len(ran) != 0 {
		t.Errorf("--check alone = %v, ran %v; want a refusal", err, ran)
	}
	if err := run("--rollback", "--cluster", "--yes"); err == nil || !strings.Contains(err.Error(), "--rollback --cluster --check") || len(ran) != 0 {
		t.Errorf("--rollback --cluster --yes = %v, ran %v; want a refusal pointing at the check", err, ran)
	}

	verdict = fmt.Errorf("pki: refusing to roll back: %w", pki.ErrRollbackUnsafe)
	var exit *ErrExitCode
	if err := run("--rollback", "--check"); !errors.As(err, &exit) || exit.Code != certs.ExitRollbackUnsafe {
		t.Errorf("a node that cannot roll back = %v, want exit %d", err, certs.ExitRollbackUnsafe)
	}
	if err := run("--rollback", "--cluster", "--check"); !errors.As(err, &exit) || exit.Code != certs.ExitRollbackUnsafe {
		t.Errorf("a cluster with a node that cannot roll back = %v, want exit %d", err, certs.ExitRollbackUnsafe)
	}
	verdict = errors.New("the Job did not run")
	if err := run("--rollback", "--cluster", "--check"); err == nil || errors.As(err, &exit) {
		t.Errorf("a check that did not run = %v, want its own error, not a verdict exit", err)
	}
}
