package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/pki"
)

// TestRefusalCommandsResolveThroughTheCommandTree is the other half of
// internal/pki's TestRollbackRefusesOnAMigratedNode.
//
// That test pins the escape hatch a refusal prints as a whole command string.
// On its own that only proves the MESSAGE is self-consistent — it cannot tell
// whether the binary has such a command, and a refusal naming a command that
// does not exist is worse than one naming none: it lands in front of an
// operator mid-incident who then runs it and gets "unknown command".
//
// Pinning the literal alone cannot catch that drift. This test resolves the
// literal instead: it walks
// the real cobra tree, verb by verb, and requires every flag in the pinned
// invocation to be registered on the command it lands on. Renaming the command,
// dropping a flag, or moving it under a different parent fails here.
func TestRefusalCommandsResolveThroughTheCommandTree(t *testing.T) {
	// Every operator-facing invocation any refusal in internal/pki prints,
	// plus every invocation the cluster orchestrator builds for a per-node Job
	// (internal/certs, etcdNodeArgv). Adding either means adding it here.
	//
	// The orchestrator's own invocations matter as much as the refusals: the
	// Job nsenters into the host and runs this command, so renaming or dropping
	// a flag here breaks every FOLLOWER while the initiator — which the driver
	// runs in-process, never parsing flags — succeeds, splitting the cluster's
	// trust. internal/certs checks those argv against the validation RULES;
	// this checks them against the flags that actually exist.
	for _, invocation := range []string{
		pki.EtcdMigrateCommand + " --cluster --rollback --force --yes",
		pki.EtcdMigrateCommand + " --cluster --resume --yes",
		pki.EtcdMigrateCommand + " --cluster --yes",
		pki.EtcdMigrateCommand + " --phase trust-both --yes",
		pki.EtcdMigrateCommand + " --phase trust-both --fetch-shared --yes",
		pki.EtcdMigrateCommand + " --phase issue-new --yes",
		pki.EtcdMigrateCommand + " --phase drop-root --yes",
		pki.EtcdMigrateCommand + " --phase rollback --yes",
		pki.EtcdMigrateCommand + " --phase rollback --force --yes",
	} {
		t.Run(invocation, func(t *testing.T) {
			assertInvocationResolves(t, invocation)
		})
	}
}

func assertInvocationResolves(t *testing.T, invocation string) {
	t.Helper()
	fields := strings.Fields(invocation)
	if len(fields) == 0 || fields[0] != "stratum" {
		t.Fatalf("invocation %q does not start with the binary name", invocation)
	}

	cmd := NewRootCommand()
	// Verbs precede the first flag; everything after it is flags and their
	// values. A flag may take the next token as its value (--phase trust-both),
	// which must not be mistaken for a subcommand.
	var verbs, flags []string
	rest := fields[1:]
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		if !strings.HasPrefix(f, "-") {
			if len(flags) == 0 {
				verbs = append(verbs, f)
			}
			continue
		}
		name, inlineValue, hasInline := strings.Cut(strings.TrimLeft(f, "-"), "=")
		flags = append(flags, name)
		_ = inlineValue
		// Consume a separate value token, if the flag has one.
		if !hasInline && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
			i++
		}
	}

	for _, verb := range verbs {
		next := findSubcommand(cmd, verb)
		if next == nil {
			t.Fatalf("`%s`: no subcommand %q under %q — a refusal names a command the binary does not have",
				invocation, verb, cmd.Name())
		}
		cmd = next
	}
	for _, flag := range flags {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("`%s`: command %q has no --%s flag", invocation, cmd.Name(), flag)
		}
	}
}

func findSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
