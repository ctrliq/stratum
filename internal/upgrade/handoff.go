package upgrade

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// handoffEnv marks a process that an installed binary exec'd into the staged
// upgrade binary. A second hand-off would mean the staged binary did not
// recognise itself as --from, and exec'ing again could loop; refuse instead.
const handoffEnv = "STRATUM_UPGRADE_HANDOFF"

// Seams for tests: exec'ing for real would replace the test binary.
var (
	execBinary     = syscall.Exec
	selfExecutable = os.Executable
	selinuxMode    = selinux.CurrentMode
)

// requireRunningFrom protects callers that invoke UpgradeNode without the CLI
// handoff (notably MCP). A different --from carries a different payload, but
// this process would still render manifests from its own compiled-in pins.
func requireRunningFrom(from string) error {
	if from == "" {
		return nil
	}
	staged, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("upgrade binary %s: %w", from, err)
	}
	self, err := selfExecutable()
	if err != nil {
		return fmt.Errorf("stratum upgrade: cannot identify the executing binary: %w", err)
	}
	running, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("stratum upgrade: executing binary %s: %w", self, err)
	}
	if !os.SameFile(staged, running) {
		return fmt.Errorf("stratum upgrade: --from %s is not the executing binary %s; run %s upgrade directly so its manifests and payload match", from, self, from)
	}
	return nil
}

// HandOff makes `stratum upgrade --from <new binary>` work when run by the
// installed, older binary.
//
// UpgradeNode upgrades a node to the EXECUTING binary's version: the static
// pod manifests, the image pins and the target check all come from its
// compiled-in constants, not from the payload it installs. An old binary that
// merely read the new payload would install new host binaries under old
// control-plane manifests. So when --from names a different file than the one
// running, the right thing is to become it: exec the staged binary with the
// same arguments, exactly as the cluster orchestrator's Job already does on
// every node (nodeUpgradeArgv). The staged binary then sees --from naming
// itself and proceeds.
//
// Returns nil without doing anything when from is empty or is this binary. On
// a successful hand-off it does not return. args is the command line after
// argv[0], passed through unchanged, so a defaulted --target defaults to the
// staged binary's version rather than this one's.
//
// Before the exec it gives the upgrade binary the installed binary's SELinux
// label, so a systemd unit can execute it wherever it was staged and whatever
// it is called. readOnly (--dry-run, --check) changes nothing on the node: it
// skips the relabel and says so on a node where SELinux is not disabled.
func HandOff(w io.Writer, from string, args []string, readOnly bool) error {
	if from == "" {
		return nil
	}
	fi, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("upgrade binary %s: %w", from, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file — --from takes the upgrade binary itself (the new stratum binary)", from)
	}
	self, err := selfExecutable()
	if err != nil {
		return fmt.Errorf("stratum upgrade: cannot tell whether --from %s is this binary: %w", from, err)
	}
	if selfFi, err := os.Stat(self); err == nil && os.SameFile(selfFi, fi) {
		return nil
	}
	if os.Getenv(handoffEnv) != "" {
		return fmt.Errorf("stratum upgrade: already handed off once, but %s is not the binary running (%s); refusing to hand off again", from, self)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("upgrade binary %s is not executable (chmod 0755 it)", from)
	}
	if readOnly {
		if selinuxMode() != selinux.Disabled {
			fmt.Fprintf(w, "stratum upgrade: not relabelling %s for SELinux on a read-only run; a systemd unit cannot execute it until it is labelled like %s\n", from, version.StratumInstallPath)
		}
	} else if err := labelLike(w, from, version.StratumInstallPath); err != nil {
		return fmt.Errorf("stratum upgrade: label the upgrade binary %s like %s for SELinux: %w", from, version.StratumInstallPath, err)
	}
	fmt.Fprintf(w, "stratum upgrade: handing off to the upgrade binary %s, which carries the version being installed\n", from)
	argv := append([]string{from}, args...)
	parentEnv := os.Environ()
	env := make([]string, 0, len(parentEnv)+1)
	for _, entry := range parentEnv {
		if !strings.HasPrefix(entry, handoffEnv+"=") {
			env = append(env, entry)
		}
	}
	env = append(env, handoffEnv+"=1")
	if err := execBinary(from, argv, env); err != nil {
		return fmt.Errorf("stratum upgrade: exec %s: %w", from, err)
	}
	return nil
}
