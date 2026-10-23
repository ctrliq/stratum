package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/upgrade"
	"github.com/ctrliq/stratum/internal/version"
)

func TestUnknownTopLevelCommandPrintsUsageAndExitsOne(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"bogus"})
	err := root.Execute()
	if err == nil || errors.Is(err, ErrNoCommand) {
		t.Fatalf("Execute([bogus]) = %v, want an unknown-command error", err)
	}
	if !strings.Contains(err.Error(), `unknown command "bogus"`) {
		t.Fatalf("error does not identify the invalid command: %v", err)
	}
	if !strings.Contains(out.String(), "Available Commands:") {
		t.Fatalf("no usage printed before the unknown-command error:\n%s", &out)
	}
}

func TestBareInvocationReturnsErrNoCommand(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(nil)
	err := root.Execute()
	if !errors.Is(err, ErrNoCommand) {
		t.Fatalf("Execute(nil) = %v, want ErrNoCommand", err)
	}
	if !strings.Contains(out.String(), "Available Commands:") {
		t.Fatalf("no help printed for a bare invocation:\n%s", &out)
	}
}

func TestBadFlagReturnsErrUsage(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"up", "--not-a-real-flag"})
	err := root.Execute()
	var usageErr *ErrUsage
	if !errors.As(err, &usageErr) {
		t.Fatalf("Execute([up --not-a-real-flag]) = %v (%T), want *ErrUsage", err, err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("no usage printed for a bad flag:\n%s", &out)
	}
}

// TestNewGroupCommand exercises newGroupCommand directly, rather than one of
// the five group names hardcoded elsewhere in this file — so this coverage
// doesn't silently stop applying to whichever group a future author adds,
// the way five hand-copied `Args`/`RunE` assignments could.
func TestNewGroupCommand(t *testing.T) {
	child := &cobra.Command{Use: "child", RunE: func(*cobra.Command, []string) error { return nil }}
	group := newGroupCommand("group", "a test group", child)
	// Cobra only suppresses its OWN automatic usage-on-error printing when
	// SilenceUsage is true on both the resolved command and whatever
	// Execute() was actually called on. In production, group is always
	// reached via root.Execute(), and NewRootCommand sets SilenceUsage on
	// root — so unknownCommand's manual cmd.Help() call is the only thing
	// producing output. Calling group.Execute() directly, as this test
	// does, makes group itself the Execute() target; without this, cobra's
	// own default printing would mask a broken unknownCommand entirely.
	group.SilenceUsage = true

	t.Run("bare group requires a subcommand", func(t *testing.T) {
		var out bytes.Buffer
		group.SetOut(&out)
		group.SetErr(&out)
		group.SetArgs(nil)
		err := group.Execute()
		if err == nil || !strings.Contains(err.Error(), "requires a subcommand") {
			t.Fatalf("Execute(nil) = %v, want a requires-a-subcommand error", err)
		}
		if !strings.Contains(out.String(), "Available Commands:") {
			t.Fatalf("no usage printed for a bare group:\n%s", &out)
		}
	})

	t.Run("unmatched verb prints usage before the error", func(t *testing.T) {
		var out bytes.Buffer
		group.SetOut(&out)
		group.SetErr(&out)
		group.SetArgs([]string{"typo"})
		err := group.Execute()
		if err == nil || !strings.Contains(err.Error(), `unknown command "typo"`) {
			t.Fatalf("Execute([typo]) = %v, want an unknown-command error", err)
		}
		if !strings.Contains(out.String(), "Available Commands:") {
			t.Fatalf("no usage printed for an unmatched subcommand:\n%s", &out)
		}
	})

	t.Run("real subcommand still dispatches", func(t *testing.T) {
		group.SetArgs([]string{"child"})
		if err := group.Execute(); err != nil {
			t.Fatalf("Execute([child]) = %v, want nil", err)
		}
	})
}

func TestCommandGroupsRequireSubcommand(t *testing.T) {
	for _, group := range []string{"certs", "snapshot", "token", "registry"} {
		for _, verb := range []string{"", "typo"} {
			t.Run(group+"/"+verb, func(t *testing.T) {
				root := NewRootCommand()
				var out bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&out)
				args := []string{group}
				if verb != "" {
					args = append(args, verb)
				}
				root.SetArgs(args)
				err := root.Execute()
				if err == nil || errors.Is(err, ErrNoCommand) {
					t.Fatalf("Execute(%v) = %v, want an invalid-subcommand error", args, err)
				}
				if !strings.Contains(err.Error(), group) || (verb != "" && !strings.Contains(err.Error(), verb)) {
					t.Fatalf("error does not identify the invalid invocation: %v", err)
				}
				if !strings.Contains(out.String(), "Available Commands:") {
					t.Fatalf("no usage printed for %v:\n%s", args, &out)
				}
			})
		}
		t.Run(group+"/help", func(t *testing.T) {
			root := NewRootCommand()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs([]string{group, "--help"})
			if err := root.Execute(); err != nil {
				t.Fatalf("explicit help failed: %v", err)
			}
			if !strings.Contains(out.String(), "Available Commands:") {
				t.Fatalf("help does not list subcommands: %s", &out)
			}
		})
	}
}

// TestStampedVersionsReturnsTheEngineStampsReadOnly is the stamp guard's
// second case: an importer cannot see internal/version, so pkg/cli exports
// the engine's stamped component versions read-only — a value copy, so
// mutating what comes back cannot reach the package state behind it. FAILS
// WITHOUT THE FIX: StampedVersions does not exist.
func TestStampedVersionsReturnsTheEngineStampsReadOnly(t *testing.T) {
	for _, sw := range []struct {
		p    *string
		want string
	}{
		{&version.ContainerdVersion, "v1.37.1"},
		{&version.RuncVersion, "v1.3.0"},
		{&version.KubeVipVersion, "v0.8.4"},
		{&version.CiliumVersion, "v1.15.3"},
		{&version.CorednsVersion, "v1.11.1"},
	} {
		old := *sw.p
		*sw.p = sw.want
		t.Cleanup(func() { *sw.p = old })
	}

	got := StampedVersions()
	want := ComponentVersions{
		Containerd: "v1.37.1", Runc: "v1.3.0", KubeVip: "v0.8.4",
		Cilium: "v1.15.3", CoreDNS: "v1.11.1",
	}
	if got != want {
		t.Fatalf("StampedVersions() = %+v, want the internal/version stamps %+v", got, want)
	}

	// Read-only: the caller gets a copy. A pointer or map handed out would
	// let an importer rewrite the engine's stamps.
	got.Containerd = "mutated"
	if version.ContainerdVersion != "v1.37.1" {
		t.Fatalf("mutating the returned ComponentVersions rewrote version.ContainerdVersion to %q", version.ContainerdVersion)
	}
}

// TestCommandsReturnTheirSentinels pins which sentinel each command returns
// for the outcomes an embedding binary (and any hand copy of the exit-code
// mapping) depends on: `upgrade --check` answers ErrUpgradeNeeded
// as *ErrExitCode 10, and the kubectl/etcdctl passthroughs propagate the
// child's exact exit code. upgrade calls its operation through the
// upgradeNode seam, which is the only way a test can reach that path without
// a live node.
func TestCommandsReturnTheirSentinels(t *testing.T) {
	t.Run("upgrade --check answers ErrUpgradeNeeded as exit code 10", func(t *testing.T) {
		old := upgradeNode
		upgradeNode = func(io.Writer, upgrade.NodeOptions) error { return upgrade.ErrUpgradeNeeded }
		t.Cleanup(func() { upgradeNode = old })

		root := NewRootCommand()
		root.SetArgs([]string{"upgrade", "--check"})
		var err error
		out := captureStdout(t, func() { err = root.Execute() })
		var ec *ErrExitCode
		if !errors.As(err, &ec) {
			t.Fatalf("upgrade --check = %v (%T), want *ErrExitCode", err, err)
		}
		// 10 is upgrade.ExitUpgradeNeeded, the status the cluster orchestrator
		// reads from the Job's pod before deciding to drain the node.
		if ec.Code != 10 {
			t.Errorf("exit code = %d, want 10", ec.Code)
		}
		// ErrExitCode's contract is that whoever returns it has already told
		// the operator what it needed to.
		if !strings.Contains(out, "needs the upgrade") {
			t.Errorf("nothing told the operator before the exit status; stdout:\n%s", out)
		}
	})

	t.Run("upgrade --check answers ErrPrereqMissing as exit code 11", func(t *testing.T) {
		old := upgradeNode
		upgradeNode = func(io.Writer, upgrade.NodeOptions) error {
			return fmt.Errorf("%w: SELinux is enforcing and the container-selinux package is not installed", upgrade.ErrPrereqMissing)
		}
		t.Cleanup(func() { upgradeNode = old })

		root := NewRootCommand()
		root.SetArgs([]string{"upgrade", "--check"})
		var err error
		errOut := captureStderr(t, func() { err = root.Execute() })
		var ec *ErrExitCode
		if !errors.As(err, &ec) {
			t.Fatalf("upgrade --check = %v (%T), want *ErrExitCode", err, err)
		}
		// 11 is upgrade.ExitPrereqMissing, the status that says the node
		// refused rather than that it can be drained.
		if ec.Code != upgrade.ExitPrereqMissing {
			t.Errorf("exit code = %d, want %d", ec.Code, upgrade.ExitPrereqMissing)
		}
		if !strings.Contains(errOut, "container-selinux") {
			t.Errorf("the refusal did not print before the exit status; stderr:\n%s", errOut)
		}
	})

	for _, bin := range []string{"kubectl", "etcdctl"} {
		t.Run(bin+" passthrough propagates the child's exit code", func(t *testing.T) {
			writeFakeBin(t, bin, 3)
			root := NewRootCommand()
			root.SetArgs([]string{bin})
			err := root.Execute()
			var ec *ErrExitCode
			if !errors.As(err, &ec) {
				t.Fatalf("%s passthrough = %v (%T), want *ErrExitCode", bin, err, err)
			}
			if ec.Code != 3 {
				t.Errorf("%s passthrough exit code = %d, want the child's 3", bin, ec.Code)
			}
		})
	}
}
