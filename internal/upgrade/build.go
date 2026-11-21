package upgrade

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// The kubelet version cannot tell two builds of one release apart; binaries are compared.

// Returned by `stratum upgrade --check`; the orchestrator maps it to exit status ExitUpgradeNeeded.
var ErrUpgradeNeeded = errors.New("upgrade needed")

// ExitUpgradeNeeded is --check's exit status for ErrUpgradeNeeded.
const ExitUpgradeNeeded = 10

// The node refuses the upgrade itself: --check reports it as exit status ExitPrereqMissing.
var ErrPrereqMissing = errors.New("prerequisite missing")

// ExitPrereqMissing is --check's exit status for ErrPrereqMissing.
const ExitPrereqMissing = 11

// Only meaningful with no upgrade in progress: the install happens before the upgrade finishes.
func installedIsThisBuild() (bool, error) {
	self, err := selfExecutable()
	if err != nil {
		return false, fmt.Errorf("identify the executing binary: %w", err)
	}
	// Nothing installed is an answer, not a failure: the upgrade installs stratum itself.
	if _, err := os.Stat(stratumInstallPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return sameContent(self, stratumInstallPath)
}

// Stat first: the hash runs only when the sizes agree.
func sameContent(a, b string) (bool, error) {
	f, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return sameContentFile(f, b)
}

// Takes the open descriptor: the caller goes on to read those same bytes. Leaves in at offset 0.
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
