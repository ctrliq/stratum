package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/etcdsnap"
)

// restoreOutcome runs `snapshot restore` with the restore itself replaced by
// restoreErr, and returns what it printed and what it returned.
// restartCommands are the commands the guidance gives for restarting a control
// plane's watchers by hand once every control plane has rejoined.
var restartCommands = []string{
	`sudo pkill -x kubelet`,
	`sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-controller-manager([[:space:]]|$)'`,
	`sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-scheduler([[:space:]]|$)'`,
	"sudo pkill -TERM -f '^[^[:space:]]+ operator( [^[:space:]]+)* --kubeconfig (/var/lib/stratum/kubeconfig/super-admin\\.conf|/var/lib/stratum/kubeconfig/admin\\.conf)",
	// the in-cluster workloads, one command per namespace
	"stratum kubectl -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns",
	"stratum kubectl -n local-path-storage rollout restart deploy/local-path-provisioner",
	// the pattern for third-party controllers, which the restore does not restart
	"stratum kubectl -n <namespace> rollout restart deploy/<name>",
}

func restoreOutcome(t *testing.T, restoreErr error) (string, error) {
	t.Helper()
	old := restoreLocalSeed
	restoreLocalSeed = func(_ io.Writer, _ string) error { return restoreErr }
	t.Cleanup(func() { restoreLocalSeed = old })
	cmd := newSnapshotRestoreCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"/var/backups/etcd/x.db", "--yes"})
	err := cmd.Execute()
	return out.String(), err
}

// TestRestoreOutcomeSuccess: a restore that restarted everything says so and
// lists what is left.
func TestRestoreOutcomeSuccess(t *testing.T) {
	out, err := restoreOutcome(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"seed restored.", "were restarted", "Still to do:", "snapshot rejoin",
		"If the cluster has other control planes", "repeat", "EVERY one has rejoined",
		"--yes --force", "Without --force a rejoin only proceeds",
		"rollout restart ds/cilium deploy/cilium-operator deploy/coredns", "CoreDNS", "local-path provisioner"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, want := range restartCommands {
		if !strings.Contains(out, want) {
			t.Errorf("output missing the restart command %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT every") {
		t.Errorf("a complete restore claims an incomplete one:\n%s", out)
	}
}

// TestRestoreOutcomePartial: the data is restored but a watcher was not
// restarted. The recovery steps are printed, the failure is still returned, and
// nothing claims every restart worked. The manual commands name the seed.
func TestRestoreOutcomePartial(t *testing.T) {
	cause := fmt.Errorf("%w: kubelet", etcdsnap.ErrWatchersNotRestarted)
	out, err := restoreOutcome(t, cause)
	if !errors.Is(err, etcdsnap.ErrWatchersNotRestarted) {
		t.Fatalf("err = %v; the failure was swallowed", err)
	}
	for _, want := range []string{"NOT every component", "Do not run the restore again", "Still to do:",
		"--server https://127.0.0.1:6443", "this seed node", "repeat", "EVERY one has rejoined",
		"--server https://127.0.0.1:6443 -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns",
		"--server https://127.0.0.1:6443 -n local-path-storage rollout restart deploy/local-path-provisioner"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, want := range restartCommands {
		if !strings.Contains(out, want) {
			t.Errorf("output missing the restart command %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "were restarted: they held") {
		t.Errorf("claims every restart worked:\n%s", out)
	}
}

// TestRestoreOutcomeOrdinaryFailure: a restore that did not restore says
// nothing about the snapshot being in place, and prints no next steps.
func TestRestoreOutcomeOrdinaryFailure(t *testing.T) {
	out, err := restoreOutcome(t, errors.New("etcdutl exploded"))
	if err == nil || !strings.Contains(err.Error(), "etcdutl exploded") {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{"seed restored", "Still to do", "Do not run the restore again"} {
		if strings.Contains(out, bad) {
			t.Errorf("a failed restore printed %q:\n%s", bad, out)
		}
	}
}

// TestDocsGiveEveryPrintedCommand: snapshot-restore.md carries each command the
// restore prints for restarting components by hand, so the docs and the output
// cannot drift. It reads the page from the repository.
func TestDocsGiveEveryPrintedCommand(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "docs", "cluster-admin", "snapshot-restore.md"))
	if err != nil {
		t.Skipf("docs not available from this checkout: %v", err)
	}
	doc := string(b)
	for _, c := range append(etcdsnap.RepeatRestartCommands(), etcdsnap.RolloutRestartCommands("stratum kubectl")...) {
		if !strings.Contains(doc, asRootPrompt(c)) {
			t.Errorf("snapshot-restore.md does not give the command the restore prints:\n%s", c)
		}
	}
	for _, c := range etcdsnap.SeedRolloutRestartCommands() {
		if !strings.Contains(doc, asRootPrompt(c)) {
			t.Errorf("snapshot-restore.md does not give the seed command the restore prints:\n%s", c)
		}
	}
}

// asRootPrompt is how the docs write a command the restore prints: a sudo
// command appears as a root prompt, "# cmd", in place of "sudo cmd".
func asRootPrompt(c string) string {
	if rest, ok := strings.CutPrefix(c, "sudo "); ok {
		return "# " + rest
	}
	return c
}
