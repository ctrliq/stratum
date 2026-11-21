package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/pki"
)

// Resolves each pinned refusal invocation against the real cobra tree, flags included.
func TestRefusalCommandsResolveThroughTheCommandTree(t *testing.T) {
	// Every operator-facing invocation a refusal prints, plus each per-node Job's argv.
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
	// Verbs precede the first flag; a flag's value token is not a subcommand.
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
