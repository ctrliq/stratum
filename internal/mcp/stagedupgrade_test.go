package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// stageDir points version.UpgradeStageDir at a fresh directory owned by the
// test's user, which stands in for root.
func stageDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "upgrade")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir, oldOwner := version.UpgradeStageDir, upgradeStageOwner
	t.Cleanup(func() { version.UpgradeStageDir, upgradeStageOwner = oldDir, oldOwner })
	version.UpgradeStageDir, upgradeStageOwner = dir, uint32(os.Getuid())
	return dir
}

func stage(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/true\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// A binary staged in the stage directory is accepted by name or by its path.
func TestStagedUpgradeBinaryAcceptsAStagedBinary(t *testing.T) {
	dir := stageDir(t)
	want := stage(t, dir, "stratum-v1.37.2", 0o755)
	for _, from := range []string{"stratum-v1.37.2", want, dir + "//stratum-v1.37.2"} {
		got, err := stagedUpgradeBinary(from)
		if err != nil || got != want {
			t.Errorf("stagedUpgradeBinary(%q) = %q, %v; want %q", from, got, err, want)
		}
	}
}

// Anything the operator did not stage in the directory, or could have been
// changed by someone other than root, is refused.
func TestStagedUpgradeBinaryRefusesWhatWasNotStaged(t *testing.T) {
	dir := stageDir(t)
	stage(t, dir, "stratum", 0o755)
	outside := stage(t, filepath.Dir(dir), "stratum-outside", 0o755)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	stage(t, filepath.Join(dir, "sub"), "stratum", 0o755)
	stage(t, dir, "group-writable", 0o775)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{
		outside,
		"/usr/bin/stratum",
		dir + "/../stratum-outside",
		dir + "/sub/stratum",
		"sub/stratum",
		"..",
		dir,
		"link",
		"group-writable",
		"missing",
	} {
		if got, err := stagedUpgradeBinary(from); err == nil {
			t.Errorf("stagedUpgradeBinary(%q) = %q, want a refusal", from, got)
		}
	}

	upgradeStageOwner = uint32(os.Getuid()) + 1
	if _, err := stagedUpgradeBinary("stratum"); err == nil || !strings.Contains(err.Error(), "not owned by root") {
		t.Errorf("a binary owned by another user: %v, want a refusal naming the owner", err)
	}
	upgradeStageOwner = uint32(os.Getuid())

	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := stagedUpgradeBinary("stratum"); err == nil {
		t.Error("a binary in a world-writable stage directory was accepted")
	}
}

// upgrade_apply refuses a cluster upgrade from an unstaged binary before it
// runs anything.
func TestUpgradeApplyClusterRefusesAnUnstagedBinary(t *testing.T) {
	stageDir(t)
	_, _, err := UpgradeApplyHandler(nil)(context.Background(), nil, UpgradeApplyIn{Scope: "cluster", From: "/tmp/anything"})
	if err == nil || !strings.Contains(err.Error(), "is not a binary in") {
		t.Fatalf("upgrade_apply scope=cluster from=/tmp/anything: %v, want a refusal naming the stage directory", err)
	}
}
