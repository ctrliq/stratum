package upgrade

import (
	"io"

	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

type execCall struct {
	path string
	argv []string
	env  []string
}

// stubHandOff points the running binary at self and records every exec
// instead of replacing the test process.
func stubHandOff(t *testing.T, self string, execErr error) *[]execCall {
	t.Helper()
	var calls []execCall
	oldExec, oldSelf := execBinary, selfExecutable
	execBinary = func(path string, argv, env []string) error {
		calls = append(calls, execCall{path, argv, env})
		return execErr
	}
	selfExecutable = func() (string, error) { return self, nil }
	oldLabel := labelLike
	labelLike = func(io.Writer, string, string) error { return nil }
	t.Cleanup(func() { execBinary, selfExecutable, labelLike = oldExec, oldSelf, oldLabel })
	return &calls
}

func writeBinary(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// The documented one-node command: the installed binary, given a
// different --from, becomes the upgrade binary with the same arguments, and
// marks the process so the upgrade binary cannot hand off again.
func TestHandOffExecsTheUpgradeBinary(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "stratum-next"), 0o755)
	calls := stubHandOff(t, self, nil)
	t.Setenv(handoffEnv, "")

	args := []string{"upgrade", "--target", "v1.37.1", "--from", next}
	if err := HandOff(io.Discard, next, args, false); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("exec'd %d times, want once", len(*calls))
	}
	c := (*calls)[0]
	if c.path != next {
		t.Errorf("exec'd %s, want the upgrade binary %s", c.path, next)
	}
	if want := append([]string{next}, args...); !slices.Equal(c.argv, want) {
		t.Errorf("argv = %q, want %q (the arguments passed through unchanged)", c.argv, want)
	}
	var markers []string
	for _, entry := range c.env {
		if strings.HasPrefix(entry, handoffEnv+"=") {
			markers = append(markers, entry)
		}
	}
	if !slices.Equal(markers, []string{handoffEnv + "=1"}) {
		t.Errorf("handoff markers = %q, want exactly one nonempty marker", markers)
	}
}

// The upgrade binary sees --from naming itself, which is the cluster Job's
// argv too (nodeUpgradeArgv): it must proceed, not exec itself forever. Also
// through a hard link, where the path differs but the file does not.
func TestHandOffIsANoOpForThisBinary(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	link := filepath.Join(dir, "stratum-link")
	if err := os.Link(self, link); err != nil {
		t.Fatal(err)
	}
	calls := stubHandOff(t, self, nil)
	for _, from := range []string{"", self, link} {
		if err := HandOff(io.Discard, from, []string{"upgrade"}, false); err != nil {
			t.Errorf("HandOff(io.Discard, %q) = %v, want nil", from, err)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("exec'd %d times for this binary's own path, want none", len(*calls))
	}
}

func TestHandOffRefuses(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "stratum-next"), 0o755)
	plain := writeBinary(t, filepath.Join(dir, "stratum-noexec"), 0o644)

	for name, tc := range map[string]struct {
		from, env, want string
	}{
		"a second hand-off": {next, "1", "refusing to hand off again"},
		"a missing file":    {filepath.Join(dir, "absent"), "", "no such file"},
		"a directory":       {dir, "", "not a regular file"},
		"a non-executable":  {plain, "", "not executable"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := stubHandOff(t, self, nil)
			t.Setenv(handoffEnv, tc.env)
			err := HandOff(io.Discard, tc.from, []string{"upgrade"}, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("HandOff = %v, want an error containing %q", err, tc.want)
			}
			if len(*calls) != 0 {
				t.Errorf("exec'd despite refusing")
			}
		})
	}
}

func TestHandOffReportsAFailedExec(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "stratum-next"), 0o755)
	stubHandOff(t, self, errors.New("exec format error"))
	t.Setenv(handoffEnv, "")
	if err := HandOff(io.Discard, next, []string{"upgrade"}, false); err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Errorf("HandOff = %v, want the exec failure", err)
	}
}

// UpgradeNode is also called directly by MCP, which cannot replace its server
// process with the staged binary. Refuse before a default target or payload
// extraction can pair this process's old manifests with newer host binaries.
func TestUpgradeNodeRejectsDifferentFromWithoutHandoff(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "stratum-next"), 0o755)
	oldSelf := selfExecutable
	selfExecutable = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfExecutable = oldSelf })

	if err := requireRunningFrom(self); err != nil {
		t.Fatalf("the executing binary was refused: %v", err)
	}
	err := UpgradeNode(io.Discard, NodeOptions{From: next, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "run "+next+" upgrade directly") {
		t.Errorf("UpgradeNode with a foreign --from = %v, want a refusal naming the staged binary", err)
	}
}

// Each refusal says what to do next, because a single message cannot be
// right for all three cases.
func TestTargetMismatchSaysWhatToDo(t *testing.T) {
	k := version.K8sVersion
	for name, tc := range map[string]struct {
		target, from, want string
	}{
		"a build suffix":            {k + "-stratum.0-rc.2", "", "use --target " + k + ", not \"" + k + "-stratum.0-rc.2\""},
		"the installed binary":      {"v9.9.9", "", "pass it with --from"},
		"the staged upgrade binary": {"v9.9.9", "/var/lib/stratum/upgrade/next", "does not match the upgrade binary /var/lib/stratum/upgrade/next"},
	} {
		if err := targetMismatch(tc.target, tc.from); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestHandOffLabelsTheUpgradeBinaryBeforeExecingIt(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "next"), 0o755)
	calls := stubHandOff(t, self, nil)
	t.Setenv(handoffEnv, "")
	var labelled [][2]string
	labelLike = func(_ io.Writer, path, dst string) error {
		if len(*calls) != 0 {
			t.Error("labelled after the exec")
		}
		labelled = append(labelled, [2]string{path, dst})
		return nil
	}
	if err := HandOff(io.Discard, next, []string{"upgrade", "--from", next}, false); err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{next, version.StratumInstallPath}}; !slices.Equal(labelled, want) {
		t.Errorf("labelled %q, want the upgrade binary labelled like the installed binary %q", labelled, want)
	}
	if len(*calls) != 1 {
		t.Fatalf("exec'd %d times, want once", len(*calls))
	}
}

func TestHandOffDoesNotExecAnUpgradeBinaryItCannotLabel(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "stratum-next"), 0o755)
	calls := stubHandOff(t, self, nil)
	t.Setenv(handoffEnv, "")
	labelLike = func(io.Writer, string, string) error { return errors.New("chcon: permission denied") }
	err := HandOff(io.Discard, next, []string{"upgrade", "--from", next}, false)
	if err == nil || !strings.Contains(err.Error(), "chcon: permission denied") || !strings.Contains(err.Error(), next) {
		t.Errorf("HandOff = %v, want the labelling failure naming %s", err, next)
	}
	if len(*calls) != 0 {
		t.Errorf("exec'd %d times after a labelling failure, want none", len(*calls))
	}
}

func TestHandOffDoesNotLabelThisBinary(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	stubHandOff(t, self, nil)
	labelLike = func(io.Writer, string, string) error {
		t.Error("labelled the running binary, which needs no hand-off")
		return nil
	}
	if err := HandOff(io.Discard, self, []string{"upgrade"}, false); err != nil {
		t.Fatal(err)
	}
}

// A read-only run (--dry-run, --check) still hands off, so the preview comes
// from the version being installed, but changes no label on the node.
func TestHandOffDoesNotRelabelOnAReadOnlyRun(t *testing.T) {
	dir := t.TempDir()
	self := writeBinary(t, filepath.Join(dir, "stratum"), 0o755)
	next := writeBinary(t, filepath.Join(dir, "next"), 0o755)
	for _, flag := range []string{"--dry-run", "--check"} {
		t.Run(flag, func(t *testing.T) {
			calls := stubHandOff(t, self, nil)
			t.Setenv(handoffEnv, "")
			oldMode := selinuxMode
			selinuxMode = func() selinux.Mode { return selinux.Enforcing }
			t.Cleanup(func() { selinuxMode = oldMode })
			labelLike = func(io.Writer, string, string) error {
				t.Error("relabelled the upgrade binary on a read-only run")
				return nil
			}
			var out strings.Builder
			if err := HandOff(&out, next, []string{"upgrade", "--from", next, flag}, true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "not relabelling "+next) {
				t.Errorf("output %q does not say the binary was left unlabelled", out.String())
			}
			if len(*calls) != 1 {
				t.Fatalf("exec'd %d times, want the hand-off once", len(*calls))
			}
		})
	}
}
