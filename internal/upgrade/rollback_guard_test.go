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

// Kubelet parses every non-hidden file in the static-pod dir as a pod.
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
	// A visible staging leftover: it must never join a backup set.
	if err := os.WriteFile(filepath.Join(dir, "etcd.yaml.tmp"), []byte("half-written etcd manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bak, err := backupManifests(ts)
	if err != nil {
		t.Fatal(err)
	}

	// A hidden .etcd.yaml.tmp is a concurrent staticpod write in flight.
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

	stamp, items := mustNewestSet(t)
	if stamp != ts {
		t.Fatalf("newest backup set = %s, want %s", stamp, ts)
	}
	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("the restore failed for %q", failed)
	}

	for name, body := range live {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Errorf("%s after the rollback = %q, want the backed-up bytes %q restored byte-for-byte", name, got, body)
		}
	}

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

	if _, err := os.Stat(filepath.Join(dir, "kube-apiserver.yaml.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a visible staging file survived the rollback (%v); kubelet parses it as a pod", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".etcd.yaml.tmp")); err != nil {
		t.Errorf("the rollback deleted a hidden in-flight staging write: %v", err)
	} else if string(b) != "an in-flight render\n" {
		t.Errorf("the hidden in-flight staging write was changed: %q", b)
	}
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

func TestRollbackRestoresTheDerivedAdmissionWebhook(t *testing.T) {
	withCISConfigs(t)
	const ts = "20261014-000001"
	webhook := version.AdmissionWebhookConfig()

	// The node as the upgrade found it; no audit policy yet — the upgrade creates one.
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("old admission\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webhook, []byte("old admission-webhook\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
		t.Fatal(err)
	}

	stamp, items := mustNewestSet(t)
	if stamp != ts {
		t.Fatalf("newest backup set = %s, want %s", stamp, ts)
	}
	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("the restore failed for %q", failed)
	}

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

	if b, err := os.ReadFile(version.AdmissionConfigPath); err != nil || string(b) != "old admission\n" {
		t.Errorf("admission-config.yaml after the rollback = %q, %v; want the pre-upgrade bytes", b, err)
	}
	if _, err := os.Stat(version.AuditPolicyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the audit policy the upgrade created survived the rollback: %v", err)
	}
}
