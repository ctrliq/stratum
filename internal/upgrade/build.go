package upgrade

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// A new stratum build can carry the same Kubernetes release as the one it
// replaces: two builds of one release can move containerd to a new minor
// and fix bugs while the kubelet version stays identical on both sides. The
// kubelet version, which is the node's upgrade marker, cannot tell those builds
// apart, so a node counted as done by it alone is drained, left on the old
// build, and reported upgraded.
//
// The build a node runs is its installed stratum binary. Comparing that with
// the upgrade binary byte for byte is exact where it matters -- on the node,
// where both are the same architecture -- and needs nothing recorded anywhere,
// so it also works for nodes brought up before any build marker was recorded.
//
// It is not a completion check on its own. The upgrade installs stratum
// FIRST, so a run killed right after that leaves the binaries matching on a
// half-upgraded node. What makes "done" safe is the pair: the binaries match
// AND no upgrade is in progress, the marker being written before the first
// change and cleared only when the upgrade completes.

// ErrUpgradeNeeded is what `stratum upgrade --check` returns when the node is
// not at this upgrade binary's version and build: the orchestrator reads it as
// exit status ExitUpgradeNeeded, before it decides whether to drain the node.
var ErrUpgradeNeeded = errors.New("upgrade needed")

// ExitUpgradeNeeded is --check's exit status for ErrUpgradeNeeded. Zero means
// the node already runs this build; anything else is a failure to tell.
const ExitUpgradeNeeded = 10

// ErrPrereqMissing is what an upgrade returns when this node refuses the
// upgrade itself, not the version check: SELinux is enforcing or the CIS
// profile is on and container-selinux is missing. --check reports it as exit
// status ExitPrereqMissing, which the orchestrator collects without draining
// the node.
var ErrPrereqMissing = errors.New("prerequisite missing")

// ExitPrereqMissing is --check's exit status for ErrPrereqMissing.
const ExitPrereqMissing = 11

// installedIsThisBuild reports whether the installed stratum binary is the
// executing one, byte for byte. Only meaningful with no upgrade in progress:
// see above.
func installedIsThisBuild() (bool, error) {
	self, err := selfExecutable()
	if err != nil {
		return false, fmt.Errorf("identify the executing binary: %w", err)
	}
	// Nothing installed is an answer, not a failure to tell: it is not this
	// build, and the upgrade installs it. `stratum up` never installs itself,
	// so a node brought up from another path has no binary here.
	if _, err := os.Stat(stratumInstallPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return sameContent(self, stratumInstallPath)
}

// sameContent compares two files: the same file, or equal size and SHA-256.
// Stat first, so two different builds of different sizes cost nothing to tell
// apart; the hash runs only when the sizes agree.
func sameContent(a, b string) (bool, error) {
	f, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return sameContentFile(f, b)
}

// sameContentFile is sameContent for a file the caller already opened, judged
// by the open descriptor rather than its path: a caller that goes on to read
// the descriptor must decide from the bytes it will read, not from whatever the
// path names by then. It leaves in at offset 0.
func sameContentFile(in *os.File, b string) (bool, error) {
	ai, err := in.Stat()
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if os.SameFile(ai, bi) {
		return true, nil
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	h := sha256.New()
	_, err = io.Copy(h, in)
	if _, serr := in.Seek(0, io.SeekStart); err == nil {
		err = serr
	}
	if err != nil {
		return false, err
	}
	bh, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(h.Sum(nil), bh), nil
}

func fileSHA256(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
