package upgrade

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// The pre-change rollback guards the refactor of the manifest
// backup/restore path has to keep: a rollback puts every backed-up manifest
// back byte-for-byte through a rename, puts the derived admission-webhook.yaml
// back itself (restored manifests are never re-rendered, so nothing else
// restores it), and leaves no visible staging file in the directory kubelet
// watches — kubelet parses every non-hidden file there whatever the
// extension, so a visible leftover is a pod.

// TestRollbackRestoresEveryManifestByteForByte: the manifests the upgrade
// re-rendered come back as the backup holds them, a visible staging file never
// joins the backup set, the rollback evicts the visible staging files the live
// directory accumulated, and a hidden in-flight write is left alone.
func TestRollbackRestoresEveryManifestByteForByte(t *testing.T) {
	redirect(t, &version.DataDir, t.TempDir())
	redirect(t, &version.K8sConfDir, t.TempDir())
	const ts = "20261014-000000"
	dir := staticpod.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	live := map[string]string{
		"etcd.yaml":           "old etcd manifest\n",
		"kube-apiserver.yaml": "old kube-apiserver manifest\n",
		"kube-scheduler.yaml": "old kube-scheduler manifest\n",
	}
	for name, body := range live {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A visible staging leftover of the pre-atomic writer already sits in the
	// live directory: it must never join a backup set, or every later rollback
	// would restore a file kubelet parses as a pod.
	if err := os.WriteFile(filepath.Join(dir, "etcd.yaml.tmp"), []byte("half-written etcd manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bak, err := backupManifests(ts)
	if err != nil {
		t.Fatal(err)
	}

	// The upgrade re-renders every manifest, and its own writes leave a visible
	// staging file in the live directory — the shape a non-atomic write
	// leaves between its WriteFile and its Rename. A hidden .etcd.yaml.tmp is a
	// concurrent staticpod write in flight, never a leftover of ours.
	for name, body := range live {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Replace(body, "old", "new", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "kube-apiserver.yaml.tmp"), []byte("half-written apiserver manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".etcd.yaml.tmp"), []byte("an in-flight render\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// `stratum rollback` restores exactly the set the scan finds.
	stamp, items := mustNewestSet(t)
	if stamp != ts {
		t.Fatalf("newest backup set = %s, want %s", stamp, ts)
	}
	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("the restore failed for %q", failed)
	}

	// Every manifest is back byte-for-byte.
	for name, body := range live {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Errorf("%s after the rollback = %q, want the backed-up bytes %q restored byte-for-byte", name, got, body)
		}
	}

	// The backup set holds the manifests and nothing else: the visible staging
	// file that sat in the live directory never joined it.
	entries, err := os.ReadDir(bak)
	if err != nil {
		t.Fatal(err)
	}
	var backed []string
	for _, e := range entries {
		backed = append(backed, e.Name())
	}
	wantBacked := make([]string, 0, len(live))
	for name := range live {
		wantBacked = append(wantBacked, name)
	}
	slices.Sort(backed)
	slices.Sort(wantBacked)
	if !slices.Equal(backed, wantBacked) {
		t.Errorf("the manifest backup holds %q, want only the manifests %q — a staging file joined the backup set", backed, wantBacked)
	}

	// The visible staging files are gone from the directory kubelet watches.
	if _, err := os.Stat(filepath.Join(dir, "kube-apiserver.yaml.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a visible staging file survived the rollback (%v); kubelet parses it as a pod", err)
	}
	// A hidden in-flight write is not ours to remove.
	if b, err := os.ReadFile(filepath.Join(dir, ".etcd.yaml.tmp")); err != nil {
		t.Errorf("the rollback deleted a hidden in-flight staging write: %v", err)
	} else if string(b) != "an in-flight render\n" {
		t.Errorf("the hidden in-flight staging write was changed: %q", b)
	}
	// Nothing visible that is not a manifest is left behind, whatever its name.
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".yaml.tmp") {
			t.Errorf("the rollback left visible staging file %s in the static-pod dir; kubelet parses it as a pod", e.Name())
		}
	}
}

// TestRollbackRestoresTheDerivedAdmissionWebhook: the apiserver render derives
// admission-webhook.yaml from the admission config whenever it writes the
// manifest, and a rollback's restored manifests are never re-rendered — so the
// rollback restores the derived file itself, with the CIS configs, or the
// rolled-back apiserver keeps the new release's admission chain.
func TestRollbackRestoresTheDerivedAdmissionWebhook(t *testing.T) {
	withCISConfigs(t)
	const ts = "20261014-000001"
	webhook := version.AdmissionWebhookConfig()

	// The node as the upgrade found it: an admission config and the derived
	// file the old release's render wrote beside it. No audit policy yet — the
	// upgrade creates one.
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("old admission\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webhook, []byte("old admission-webhook\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The upgrade backs up the CIS set and writes this release's configs, the
	// derived file among them.
	if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
		t.Fatal(err)
	}

	// `stratum rollback` restores the set the scan finds, CIS configs first.
	stamp, items := mustNewestSet(t)
	if stamp != ts {
		t.Fatalf("newest backup set = %s, want %s", stamp, ts)
	}
	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("the restore failed for %q", failed)
	}

	// The derived file is put back itself, byte-for-byte, with its own mode.
	b, err := os.ReadFile(webhook)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old admission-webhook\n" {
		t.Errorf("admission-webhook.yaml after the rollback = %q, want the pre-upgrade bytes — the restored manifests are not re-rendered, so nothing else puts this file back", b)
	}
	fi, err := os.Stat(webhook)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("admission-webhook.yaml mode after the rollback = %o, want 0600", fi.Mode().Perm())
	}

	// The configs it rides with: the admission config restored, and the audit
	// policy the upgrade created (none existed before it) removed again.
	if b, err := os.ReadFile(version.AdmissionConfigPath); err != nil || string(b) != "old admission\n" {
		t.Errorf("admission-config.yaml after the rollback = %q, %v; want the pre-upgrade bytes", b, err)
	}
	if _, err := os.Stat(version.AuditPolicyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the audit policy the upgrade created survived the rollback: %v", err)
	}
}
