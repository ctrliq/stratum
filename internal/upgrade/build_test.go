package upgrade

import (
	"io"

	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// installedBuild makes the installed stratum binary hold installed and the
// executing one hold running, so a test decides whether they are one build.
func installedBuild(t *testing.T, installed, running string) {
	t.Helper()
	writeFile(t, stratumInstallPath, installed, 0o755)
	self := filepath.Join(t.TempDir(), "stratum-upgrade")
	writeFile(t, self, running, 0o755)
	old := selfExecutable
	selfExecutable = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfExecutable = old })
}

// sameContent is exact: the same file, or equal bytes. Size alone is not
// enough, and a different size needs no hashing.
func TestSameContent(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, copyOfA, sameSize, longer := w("a", "build-A"), w("a2", "build-A"), w("b", "build-B"), w("c", "build-AA")
	link := filepath.Join(dir, "link")
	if err := os.Link(a, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		x, y string
		want bool
	}{
		{"the same file", a, link, true},
		{"an identical copy", a, copyOfA, true},
		{"same size, other bytes", a, sameSize, false},
		{"another size", a, longer, false},
	} {
		if got, err := sameContent(c.x, c.y); err != nil || got != c.want {
			t.Errorf("%s: sameContent = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := sameContent(a, filepath.Join(dir, "missing")); err == nil {
		t.Errorf("a missing file compared without error")
	}
}

// A new build of the same Kubernetes release leaves the kubelet at
// target. That node is not "already at target"; this build is installed.
func TestUpgradeAtTargetInstallsAnotherBuild(t *testing.T) {
	// setRoots runs before resumeFixture: a SetRoots after it would recompute
	// every derived path (BinDir, K8sConfDir, StageDir) from the new roots and
	// step over the fixture's own per-var redirects, leaving the node state it
	// staged where the test no longer reads it.
	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.2", "rc.3")

	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{}) })
	if strings.Contains(out, "nothing to do") || !strings.Contains(out, "on another stratum build; installing this one") {
		t.Fatalf("a same-version build update was skipped:\n%s", out)
	}
	// On into the upgrade itself, which stops at the payload this test binary
	// does not carry.
	if err == nil || !strings.Contains(err.Error(), "payload") {
		t.Errorf("UpgradeNode = %v, want it to reach the payload", err)
	}
}

// --check answers the orchestrator's question and changes nothing: nil on this
// version and build, ErrUpgradeNeeded otherwise.
func TestUpgradeCheck(t *testing.T) {
	for _, c := range []struct {
		name        string
		kubelet     string
		installed   string
		interrupted bool
		want        error
	}{
		{"at target on this build", "v1.37.9", "rc.3", false, nil},
		{"at target on another build", "v1.37.9", "rc.2", false, ErrUpgradeNeeded},
		{"behind target", "v1.37.1", "rc.3", false, ErrUpgradeNeeded},
		{"at target, interrupted", "v1.37.9", "rc.3", true, ErrUpgradeNeeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			resumeFixture(t)
			redirect(t, &version.K8sVersion, "v1.37.9")
			fakeKubelet(t, c.kubelet)
			installedBuild(t, c.installed, "rc.3")
			if c.interrupted {
				interruptedAt(t, version.K8sVersion)
			}

			_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
			if !errors.Is(err, c.want) {
				t.Errorf("UpgradeNode(io.Discard, --check) = %v, want %v", err, c.want)
			}
			if b, rerr := os.ReadFile(stratumInstallPath); rerr != nil || string(b) != c.installed {
				t.Errorf("--check changed the installed binary: %q, %v", b, rerr)
			}
			if c.interrupted {
				if m, merr := readUpgradeMarker(); merr != nil || m == nil {
					t.Errorf("--check cleared the in-progress marker: %v, %v", m, merr)
				}
			}
		})
	}
}

// A node whose SELinux prerequisites fail refuses itself: --check answers
// ErrPrereqMissing, which the orchestrator collects without draining the
// node, when the node needs the upgrade. A node already on this build is
// never asked, and an interrupted upgrade is warned about, not refused: the
// node has already been changed.
func TestUpgradeCheckRefusesMissingSELinuxPrereq(t *testing.T) {
	// The settings and the policy answer; checkNodePrereqs itself decides,
	// so its refusal and its resume exception are what is tested.
	oldOpts, oldPrereq := nodeSELinuxOptions, nodePrereq
	nodeSELinuxOptions = func() (selinux.Options, error) { return selinux.Options{}, nil }
	nodePrereq = func(selinux.Options) error {
		return errors.New("SELinux is enforcing and the container-selinux package is not installed")
	}
	t.Cleanup(func() { nodeSELinuxOptions, nodePrereq = oldOpts, oldPrereq })
	for _, c := range []struct {
		name        string
		kubelet     string
		installed   string
		interrupted bool
		want        error
	}{
		{"needs the upgrade", "v1.37.1", "rc.3", false, ErrPrereqMissing},
		{"already on this build", "v1.37.9", "rc.3", false, nil},
		{"interrupted", "v1.37.9", "rc.3", true, ErrUpgradeNeeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			resumeFixture(t)
			redirect(t, &version.K8sVersion, "v1.37.9")
			fakeKubelet(t, c.kubelet)
			installedBuild(t, c.installed, "rc.3")
			if c.interrupted {
				interruptedAt(t, version.K8sVersion)
			}

			out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
			if !errors.Is(err, c.want) {
				t.Errorf("UpgradeNode(io.Discard, --check) = %v, want %v", err, c.want)
			}
			if errors.Is(err, ErrPrereqMissing) && errors.Is(err, ErrUpgradeNeeded) {
				t.Errorf("--check answered both a refusal and needs-the-upgrade: %v", err)
			}
			if c.interrupted && !strings.Contains(out, "WARN") {
				t.Errorf("an interrupted upgrade was refused without a warning:\n%s", out)
			}
			if b, rerr := os.ReadFile(stratumInstallPath); rerr != nil || string(b) != c.installed {
				t.Errorf("--check changed the installed binary: %q, %v", b, rerr)
			}
		})
	}
}

// A real upgrade of a node whose prerequisites fail stops before anything on
// the node is changed: nothing installed, nothing backed up, nothing marked.
func TestUpgradeNodeRefusesMissingSELinuxPrereqBeforeChanging(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, "v1.37.1")
	installedBuild(t, "rc.2", "rc.3")
	old := checkNodePrereqs
	checkNodePrereqs = func(io.Writer, bool) error {
		return fmt.Errorf("%w: SELinux is enforcing and the container-selinux package is not installed", ErrPrereqMissing)
	}
	t.Cleanup(func() { checkNodePrereqs = old })

	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{}) }); !errors.Is(err, ErrPrereqMissing) {
		t.Fatalf("UpgradeNode = %v, want ErrPrereqMissing", err)
	}
	if b, rerr := os.ReadFile(stratumInstallPath); rerr != nil || string(b) != "rc.2" {
		t.Errorf("the upgrade changed the installed binary: %q, %v", b, rerr)
	}
	if bak, _ := filepath.Glob(filepath.Join(filepath.Dir(stratumInstallPath), "*.bak-*")); len(bak) != 0 {
		t.Errorf("the upgrade backed up %v", bak)
	}
	if m, merr := readUpgradeMarker(); m != nil || merr != nil {
		t.Errorf("the upgrade wrote the in-progress marker: %v, %v", m, merr)
	}
}

// A dry-run previews the upgrade, but the prerequisite refusal comes first: a
// node whose policy would leave it unconfined is refused before the plan is
// printed or anything is previewed, dry-run or not.
func TestUpgradeNodeDryRunRefusesMissingSELinuxPrereq(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, "v1.37.1")
	installedBuild(t, "rc.2", "rc.3")
	old := checkNodePrereqs
	checkNodePrereqs = func(io.Writer, bool) error {
		return fmt.Errorf("%w: SELinux is enforcing and the container-selinux package is not installed", ErrPrereqMissing)
	}
	t.Cleanup(func() { checkNodePrereqs = old })

	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{DryRun: true}) }); !errors.Is(err, ErrPrereqMissing) {
		t.Fatalf("UpgradeNode --dry-run = %v, want ErrPrereqMissing", err)
	}
	if b, rerr := os.ReadFile(stratumInstallPath); rerr != nil || string(b) != "rc.2" {
		t.Errorf("the dry-run changed the installed binary: %q, %v", b, rerr)
	}
	if m, merr := readUpgradeMarker(); m != nil || merr != nil {
		t.Errorf("the dry-run wrote the in-progress marker: %v, %v", m, merr)
	}
}

// The node's own check cannot ask the question when the SELinux tools fail
// rather than go missing: it warns, naming the failure, and lets the node
// through, as it does for settings it cannot read.
func TestCheckNodePrereqsWarnsWhenItCannotTell(t *testing.T) {
	resumeFixture(t)
	old := nodePrereq
	nodePrereq = func(selinux.Options) error {
		return fmt.Errorf("%w: list the loaded SELinux policy modules: semodule -l: exit status 1: ValueError: policy store locked", selinux.ErrCannotTell)
	}
	t.Cleanup(func() { nodePrereq = old })

	out, err := runWriter(t, func(w io.Writer) error { return checkNodePrereqs(w, false) })
	if err != nil {
		t.Fatalf("checkNodePrereqs refused a node it cannot tell about: %v", err)
	}
	for _, want := range []string{"WARN", "could not tell", "policy store locked"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not say %q:\n%s", want, out)
		}
	}
}

// Settings it cannot read are a question the check cannot ask: it warns,
// naming the failure, and lets the node through without asking the policy.
func TestCheckNodePrereqsWarnsWhenItCannotReadTheSettings(t *testing.T) {
	resumeFixture(t)
	oldOpts, oldPrereq := nodeSELinuxOptions, nodePrereq
	nodeSELinuxOptions = func() (selinux.Options, error) {
		return selinux.Options{}, errors.New(`unknown profile "does-not-exist"`)
	}
	nodePrereq = func(selinux.Options) error {
		t.Error("checkNodePrereqs asked the policy with settings it could not read")
		return nil
	}
	t.Cleanup(func() { nodeSELinuxOptions, nodePrereq = oldOpts, oldPrereq })

	out, err := runWriter(t, func(w io.Writer) error { return checkNodePrereqs(w, false) })
	if err != nil {
		t.Fatalf("checkNodePrereqs refused a node whose settings it cannot read: %v", err)
	}
	for _, want := range []string{"WARN", "could not read this node's SELinux settings", "does-not-exist", "not checking the SELinux prerequisites"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not say %q:\n%s", want, out)
		}
	}
}

// --check answers only while nothing else is changing the node. With the
// node lock held by an upgrade, "already on this build" could be read mid-
// upgrade and turn false on a rollback after the orchestrator had skipped the
// node, so the check fails instead -- and that is neither a yes nor
// ErrUpgradeNeeded.
func TestUpgradeCheckRefusesWhileTheNodeIsLocked(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3", "rc.3")
	release, err := lockNode("upgrade")
	if err != nil {
		t.Fatal(err)
	}

	_, err = runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
	if err == nil || errors.Is(err, ErrUpgradeNeeded) {
		t.Errorf("--check with the node lock held = %v, want a refusal", err)
	}
	release()
	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) }); err != nil {
		t.Errorf("--check once the lock is free = %v, want nil", err)
	}
}

// A marker for this target whose run recorded nothing is discarded by a real
// run, but it cannot vouch for "already on this build": installing stratum
// over no binary backs nothing up, so a run killed right after that looks
// exactly like this, with the host binaries never swapped.
// --check answers "needs the upgrade", and leaves the marker where it is.
func TestUpgradeCheckDoesNotTrustAnAbandonedSameTargetUpgrade(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3", "rc.3")
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: version.K8sVersion}); err != nil {
		t.Fatal(err)
	}

	_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
	if !errors.Is(err, ErrUpgradeNeeded) {
		t.Errorf("--check with an abandoned same-target upgrade = %v, want ErrUpgradeNeeded", err)
	}
	if m, err := readUpgradeMarker(); err != nil || m == nil {
		t.Errorf("--check discarded the marker: %v, %v", m, err)
	}
}

// An abandoned upgrade to ANOTHER version that changed nothing says nothing
// about this build, so the build match still counts.
func TestUpgradeCheckIgnoresAnAbandonedUpgradeToAnotherVersion(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3", "rc.3")
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.8"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) }); err != nil {
		t.Errorf("--check = %v, want nil", err)
	}
}

// No stratum installed is an answer: not this build. It needs the upgrade,
// which installs it -- not a check that "cannot tell" and halts the rollout.
func TestUpgradeCheckWithNothingInstalledNeedsTheUpgrade(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3", "rc.3")
	if err := os.Remove(stratumInstallPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) }); !errors.Is(err, ErrUpgradeNeeded) {
		t.Errorf("--check with nothing installed = %v, want ErrUpgradeNeeded", err)
	}
}

// --check changes nothing, and its log says what it found, not what an
// upgrade would do: the check Job's log is where an operator is sent.
func TestUpgradeCheckSaysItFoundAnotherBuild(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.2", "rc.3")
	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
	if !errors.Is(err, ErrUpgradeNeeded) || strings.Contains(out, "installing") || !strings.Contains(out, "it needs the upgrade") {
		t.Errorf("--check on another build: err=%v output:\n%s", err, out)
	}
}

// The cluster upgrade pins each node to the build ID it read from its own
// --from. A node whose copy at that path is another build refuses -- before
// the lock, before anything -- rather than answer, or upgrade, as itself.
func TestUpgradeRefusesAnotherBuildThanExpected(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	redirect(t, &version.BuildID, "0123abcd")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.2", "rc.2")
	for _, check := range []bool{true, false} {
		_, err := runWriter(t, func(w io.Writer) error {
			return UpgradeNode(w, NodeOptions{Check: check, ExpectBuild: "feedbeef"})
		})
		if err == nil || errors.Is(err, ErrUpgradeNeeded) || !strings.Contains(err.Error(), "the rollout's is feedbeef") {
			t.Errorf("check=%v: UpgradeNode = %v, want a refusal naming the rollout's build", check, err)
		}
	}
	if _, err := runWriter(t, func(w io.Writer) error {
		return UpgradeNode(w, NodeOptions{Check: true, ExpectBuild: "0123abcd"})
	}); err != nil {
		t.Errorf("--check on the expected build = %v, want nil", err)
	}
}

// An EMBEDDED_BINS=none build of the same commit has the same build ID, and a
// node of another architecture has no sha256 pin. The check refuses it before
// the drain instead of the upgrade finding no payload after it.
func TestUpgradeRefusesAPayloadlessBinaryOfTheExpectedBuild(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	redirect(t, &version.BuildID, "0123abcd")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3", "rc.3")
	stagedPayload = func(string) error { return assets.ErrNoPayload }
	for _, check := range []bool{true, false} {
		_, err := runWriter(t, func(w io.Writer) error {
			return UpgradeNode(w, NodeOptions{Check: check, ExpectBuild: "0123abcd"})
		})
		if err == nil || errors.Is(err, ErrUpgradeNeeded) || !strings.Contains(err.Error(), "no embedded payload") {
			t.Errorf("check=%v: UpgradeNode = %v, want a refusal naming the missing payload", check, err)
		}
	}
}

// One build ID, two binaries: the same source, a byte-different copy staged on
// this node. The build ID cannot tell them apart; the sha256 pin, set for nodes
// of the orchestrator's architecture, does.
func TestUpgradeRefusesAByteDifferentCopyOfTheSameBuild(t *testing.T) {
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	redirect(t, &version.BuildID, "0123abcd")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3-patched", "rc.3-patched")
	self, _ := selfExecutable()
	sum, err := fileSHA256(self)
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(sum)
	other := strings.Repeat("0", 64)

	_, err = runWriter(t, func(w io.Writer) error {
		return UpgradeNode(w, NodeOptions{Check: true, ExpectBuild: "0123abcd", ExpectSHA256: other})
	})
	if err == nil || errors.Is(err, ErrUpgradeNeeded) || !strings.Contains(err.Error(), "sha256 is "+want) {
		t.Errorf("--check with the same build ID but other bytes = %v, want a refusal naming both hashes", err)
	}
	if _, err := runWriter(t, func(w io.Writer) error {
		return UpgradeNode(w, NodeOptions{Check: true, ExpectBuild: "0123abcd", ExpectSHA256: want})
	}); err != nil {
		t.Errorf("--check on the exact binary = %v, want nil", err)
	}
}

// A --check that cannot tell does not say "needs the upgrade": that answer is
// what makes the orchestrator drain the node. It fails, which the orchestrator
// refuses without draining.
func TestUpgradeCheckFailsWhenItCannotTell(t *testing.T) {
	t.Run("no readable kubelet version", func(t *testing.T) {
		resumeFixture(t)
		redirect(t, &version.K8sVersion, "v1.37.9")
		installedBuild(t, "rc.3", "rc.3") // and no kubelet at all
		_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
		if err == nil || errors.Is(err, ErrUpgradeNeeded) {
			t.Errorf("UpgradeNode(io.Discard, --check) = %v, want an error that is not ErrUpgradeNeeded", err)
		}
	})
	t.Run("builds cannot be compared", func(t *testing.T) {
		resumeFixture(t)
		redirect(t, &version.K8sVersion, "v1.37.9")
		fakeKubelet(t, version.K8sVersion)
		installedBuild(t, "rc.3", "rc.3")
		selfExecutable = func() (string, error) { return "", errors.New("no /proc/self/exe") }
		_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
		if err == nil || errors.Is(err, ErrUpgradeNeeded) || !strings.Contains(err.Error(), "executing binary") {
			t.Errorf("UpgradeNode(io.Discard, --check) = %v, want a comparison error that is not ErrUpgradeNeeded", err)
		}
	})
}
