package upgrade

import (
	"io"

	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profilefiles"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// withCISConfigs redirects the CIS config paths and DataDir at a temp dir.
func withCISConfigs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldAdm, oldAudit, oldData, oldConf, oldWrite := version.AdmissionConfigPath, version.AuditPolicyPath, version.DataDir, version.K8sConfDir, writeProfileConfigs
	t.Cleanup(func() {
		version.AdmissionConfigPath, version.AuditPolicyPath, version.DataDir, version.K8sConfDir, writeProfileConfigs = oldAdm, oldAudit, oldData, oldConf, oldWrite
	})
	// The derived webhook file has no path var of its own; it follows K8sConfDir.
	version.K8sConfDir = filepath.Join(dir, "config")
	version.AdmissionConfigPath = filepath.Join(dir, "config", "admission-config.yaml")
	version.AuditPolicyPath = filepath.Join(dir, "config", "audit-policy.yaml")
	version.DataDir = filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Named here, not from cisConfigPaths, so a file dropped from the backup set is still changed.
	writeProfileConfigs = func(profile.Profile) error {
		for _, p := range []string{version.AdmissionConfigPath, version.AuditPolicyPath, filepath.Join(version.K8sConfDir, "admission-webhook.yaml")} {
			if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestRefreshProfileConfigsBacksUpAndRollsBack(t *testing.T) {
	withCISConfigs(t)
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("old admission"), 0o644); err != nil {
		t.Fatal(err)
	}
	webhook := filepath.Join(version.K8sConfDir, "admission-webhook.yaml")
	if err := os.WriteFile(webhook, []byte("old webhook"), 0o600); err != nil {
		t.Fatal(err)
	}
	const ts = "20260927-000000"
	items, err := refreshProfileConfigs(io.Discard, ts, cisProfile())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(version.AdmissionConfigPath); string(b) != "new" {
		t.Errorf("admission config after the refresh = %q, want this release's", b)
	}

	stamp, set := mustNewestSet(t)
	var found bool
	for _, it := range set {
		found = found || (it.profile && it.bak == profileConfigBakDir(ts))
	}
	if stamp != ts || !found {
		t.Errorf("newest set = %s %+v; want the CIS config backup of %s", stamp, set, ts)
	}

	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("restore failed for %q", failed)
	}
	b, _ := os.ReadFile(version.AdmissionConfigPath)
	fi, _ := os.Stat(version.AdmissionConfigPath)
	if string(b) != "old admission" || fi.Mode().Perm() != 0o644 {
		t.Errorf("admission config after rollback = %q, %o; want the original, 0644", b, fi.Mode().Perm())
	}
	if _, err := os.Stat(version.AuditPolicyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the audit policy the upgrade created survived the rollback: %v", err)
	}
	// Restored manifests are never re-rendered, so the derived file is restored itself.
	b, _ = os.ReadFile(webhook)
	fi, _ = os.Stat(webhook)
	if string(b) != "old webhook" || fi.Mode().Perm() != 0o600 {
		t.Errorf("admission-webhook.yaml after rollback = %q, %o; want the original, 0600", b, fi.Mode().Perm())
	}

	writeProfileConfigs = func(profile.Profile) error { return errors.New("not stamped") }
	if items, err := refreshProfileConfigs(io.Discard, "20260927-000001", cisProfile()); err == nil || len(items) != 1 {
		t.Errorf("refreshProfileConfigs = %d backups, %v; want the backup and the error", len(items), err)
	}
}

func TestRestoreProfileConfigsKeepsTheKubeconfigAPreChangeBackupNeverHeld(t *testing.T) {
	withCISConfigs(t)
	kubeconfig := version.WebhookClientKubeconfig()
	webhookFile := version.AdmissionWebhookConfig()

	bak := profileConfigBakDir("20260927-000003")
	if err := os.MkdirAll(bak, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Base(version.AdmissionConfigPath): "old admission",
		filepath.Base(version.AuditPolicyPath):     "old audit",
	} {
		if err := os.WriteFile(filepath.Join(bak, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldWebhook := "apiVersion: apiserver.config.k8s.io/v1\n" +
		"kind: AdmissionConfiguration\n" +
		"plugins:\n" +
		"- name: MutatingAdmissionWebhook\n" +
		"  configuration:\n" +
		"    apiVersion: apiserver.config.k8s.io/v1\n" +
		"    kind: WebhookAdmissionConfiguration\n" +
		"    kubeConfigFile: " + kubeconfig + "\n"
	if err := os.WriteFile(filepath.Join(bak, filepath.Base(webhookFile)), []byte(oldWebhook), 0o600); err != nil {
		t.Fatal(err)
	}

	for p, body := range map[string]string{
		version.AdmissionConfigPath: "new admission",
		version.AuditPolicyPath:     "new audit",
		webhookFile:                 "new webhook",
		kubeconfig:                  "new client config",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := restoreProfileConfigs(io.Discard, bak); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		version.AdmissionConfigPath: "old admission",
		version.AuditPolicyPath:     "old audit",
	} {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s after rollback = %q, want the backup's %q", filepath.Base(p), b, want)
		}
	}

	raw, err := os.ReadFile(webhookFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Plugins []struct {
			Name          string `json:"name"`
			Configuration struct {
				KubeConfigFile string `json:"kubeConfigFile"`
			} `json:"configuration"`
		} `json:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the restored admission file (%s): %v", webhookFile, err)
	}
	var named string
	for _, p := range doc.Plugins {
		if p.Name == "MutatingAdmissionWebhook" && p.Configuration.KubeConfigFile != "" {
			named = p.Configuration.KubeConfigFile
		}
	}
	if named == "" {
		t.Fatalf("the restored admission file (%s) names no webhook kubeconfig, so the pin cannot see it", webhookFile)
	}
	if _, err := os.Stat(named); err != nil {
		t.Errorf("the kubeconfig the restored admission file names (%s) is gone: %v — the rollback deleted a live config the pre-change backup never held", named, err)
	}
	if b, _ := os.ReadFile(kubeconfig); string(b) != "new client config" {
		t.Errorf("the kubeconfig after rollback = %q, want the live one left untouched", b)
	}
}

func TestRestoreProfileConfigsPreservesLegacyAbsence(t *testing.T) {
	for _, missing := range []string{"admission-config.yaml", "audit-policy.yaml", "admission-webhook.yaml"} {
		t.Run(missing, func(t *testing.T) {
			withCISConfigs(t)
			legacyPaths := []string{version.AdmissionConfigPath, version.AuditPolicyPath, version.AdmissionWebhookConfig()}
			bak := profileConfigBakDir("20260927-000004")
			if err := os.MkdirAll(bak, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range legacyPaths {
				if filepath.Base(path) != missing {
					if err := os.WriteFile(filepath.Join(bak, filepath.Base(path)), []byte("old "+filepath.Base(path)), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			kubeconfig := version.WebhookClientKubeconfig()
			if err := os.WriteFile(kubeconfig, []byte("live client config"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := restoreProfileConfigs(io.Discard, bak); err != nil {
				t.Fatal(err)
			}
			for _, path := range legacyPaths {
				if filepath.Base(path) == missing {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("legacy absent file survived rollback: %s, stat=%v", path, err)
					}
					continue
				}
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if want := "old " + filepath.Base(path); string(body) != want {
					t.Errorf("restored %s = %q, want %q", path, body, want)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("restored %s mode = %o, want 600", path, info.Mode().Perm())
				}
			}
			body, err := os.ReadFile(kubeconfig)
			if err != nil || string(body) != "live client config" {
				t.Errorf("uncovered kubeconfig = %q, %v, want live file preserved", body, err)
			}
		})
	}
}

func TestRestoreProfileConfigsHonorsAnExplicitEmptyCoverageRecord(t *testing.T) {
	withCISConfigs(t)
	bak := profileConfigBakDir("20260927-000005")
	if err := os.MkdirAll(bak, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bak, profileConfigRecordName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{version.AdmissionConfigPath, version.AuditPolicyPath, version.AdmissionWebhookConfig(), version.WebhookClientKubeconfig()}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("uncovered live file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := restoreProfileConfigs(io.Discard, bak); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != "uncovered live file" {
			t.Errorf("explicitly uncovered %s = %q, %v", path, body, err)
		}
	}
}

func TestPruneKeepsOnlyRecentProfileConfigBackups(t *testing.T) {
	withCISConfigs(t)
	upgradeDir := filepath.Join(version.DataDir, "upgrade")
	for _, ts := range []string{"20260101-000000", "20260201-000000", "20260301-000000"} {
		if err := os.MkdirAll(profileConfigBakDir(ts), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneOldBackups(upgradeDir, keepBackupSets, upgradeDirBackups...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profileConfigBakDir("20260101-000000")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the oldest CIS config backup was not pruned: %v", err)
	}
	for _, ts := range []string{"20260201-000000", "20260301-000000"} {
		if _, err := os.Stat(profileConfigBakDir(ts)); err != nil {
			t.Errorf("a recent CIS config backup was pruned: %v", err)
		}
	}
}

func TestRefreshProfileConfigsLeavesNoPartialBackup(t *testing.T) {
	withCISConfigs(t)
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("old admission"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where the audit policy goes: the copy fails mid-set.
	if err := os.MkdirAll(version.AuditPolicyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	wrote := false
	writeProfileConfigs = func(profile.Profile) error { wrote = true; return nil }

	items, err := refreshProfileConfigs(io.Discard, "20260927-000002", cisProfile())
	if err == nil || len(items) != 0 || wrote {
		t.Fatalf("refreshProfileConfigs = %d items, %v, wrote=%v; want the error, no rollback item, nothing written", len(items), err, wrote)
	}
	if stamp, set := mustNewestSet(t); len(set) != 0 {
		t.Errorf("a partial backup is left for stratum rollback: %s %+v", stamp, set)
	}
	if b, _ := os.ReadFile(version.AdmissionConfigPath); string(b) != "old admission" {
		t.Errorf("admission config after a failed backup = %q, want it untouched", b)
	}
}

func TestRollbackRestoresProfileConfigsBeforeManifests(t *testing.T) {
	items := []backupItem{{name: "stratum"}, {name: "manifests", dir: true}, {name: "kubelet"}, {name: "profile configs", profile: true}}
	var got []string
	for _, it := range restoreOrder(items) {
		got = append(got, it.name)
	}
	if want := []string{"profile configs", "stratum", "manifests", "kubelet"}; !slices.Equal(got, want) {
		t.Errorf("restore order = %q, want %q", got, want)
	}
	if items[0].name != "stratum" {
		t.Error("restoreOrder reordered the caller's slice")
	}
}

func TestRestoreAllRestoresInThatOrder(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	failed := restoreAll(io.Discard, []backupItem{
		{name: "manifests", bak: missing, dst: t.TempDir(), dir: true},
		{name: "profile configs", bak: missing, profile: true},
	})
	if want := []string{"profile configs", "manifests"}; !slices.Equal(failed, want) {
		t.Errorf("restoreAll failed in the order %q, want %q", failed, want)
	}
}

func TestProfileConfigPathsCoverEveryFileTheCISAPIServerReads(t *testing.T) {
	withCISConfigs(t)
	// The render reads the admission config and stamps the audit policy's sum; both must exist.
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(version.AuditPolicyPath, []byte("rules:\n- level: Metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := staticpod.WriteAll(&nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://192.0.2.10:2380", EtcdState: "new",
		CIS: true,
	}); err != nil {
		t.Fatal(err)
	}
	spec, err := staticpod.ReadContainerSpec("kube-apiserver")
	if err != nil {
		t.Fatal(err)
	}
	// The admission config rides in through the derived file; the rest is walked off the argv.
	reads := []string{version.AdmissionConfigPath}
	for _, a := range spec.Argv {
		_, value, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(value, version.K8sConfDir+string(filepath.Separator)) {
			reads = append(reads, value)
		}
	}
	admission := filepath.Join(version.K8sConfDir, "admission-webhook.yaml")
	if !slices.Contains(reads, admission) {
		t.Fatalf("the rendered apiserver argv names no %s, so the pin cannot see what the apiserver reads", admission)
	}
	paths := profileConfigPaths(cisProfile())
	for _, p := range reads {
		if !slices.Contains(paths, p) {
			t.Errorf("the CIS apiserver reads %s, but cisConfigPaths does not back it up and restore it — a rollback would leave the new release's version in place", p)
		}
	}
}

func TestProfileConfigPathsComeFromTheProfile(t *testing.T) {
	withCISConfigs(t)
	// Pins call-time path resolution: a list built against the old directory misnames its paths.
	version.K8sConfDir = t.TempDir()

	prof := cisProfile()
	var want []string
	for _, f := range prof.ConfigFiles {
		want = append(want, version.ProfileConfigFile(f.Name))
	}
	want = append(want, version.AdmissionWebhookConfig(), version.WebhookClientKubeconfig())
	if got := profileConfigPaths(prof); !slices.Equal(got, want) {
		t.Errorf("profileConfigPaths(cis) = %q, want the profile's files and the derived admission pair %q", got, want)
	}

	plain := profile.Profile{Name: "plain", ConfigFiles: []profile.File{{Name: "audit-policy.yaml", Kind: profile.PlainFile}}}
	if got := profileConfigPaths(plain); !slices.Equal(got, []string{version.ProfileConfigFile("audit-policy.yaml")}) {
		t.Errorf("profileConfigPaths(plain) = %q, want its one file and no derived admission files", got)
	}
	if got := profileConfigPaths(profile.Profile{}); len(got) != 0 {
		t.Errorf("profileConfigPaths(no profile) = %q, want none", got)
	}
}

func TestProfileConfigPathsCoverTheWebhookClientKubeconfig(t *testing.T) {
	withCISConfigs(t)
	// Same constraint as above: the render needs both files to exist.
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(version.AuditPolicyPath, []byte("rules:\n- level: Metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := staticpod.WriteAll(&nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://192.0.2.10:2380", EtcdState: "new",
		CIS: true,
	}); err != nil {
		t.Fatal(err)
	}
	spec, err := staticpod.ReadContainerSpec("kube-apiserver")
	if err != nil {
		t.Fatal(err)
	}
	var admission string
	for _, a := range spec.Argv {
		if v, ok := strings.CutPrefix(a, "--admission-control-config-file="); ok {
			admission = v
		}
	}
	if admission == "" {
		t.Fatal("the rendered apiserver argv names no admission file, so the pin cannot see what the apiserver reads")
	}
	raw, err := os.ReadFile(admission)
	if err != nil {
		t.Fatalf("read the admission file the apiserver's argv names (%s): %v", admission, err)
	}
	var doc struct {
		Plugins []struct {
			Name          string `json:"name"`
			Configuration struct {
				KubeConfigFile string `json:"kubeConfigFile"`
			} `json:"configuration"`
		} `json:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the admission file the apiserver's argv names (%s): %v", admission, err)
	}
	var kubeconfig string
	for _, p := range doc.Plugins {
		if p.Name == "MutatingAdmissionWebhook" && p.Configuration.KubeConfigFile != "" {
			kubeconfig = p.Configuration.KubeConfigFile
		}
	}
	if kubeconfig == "" {
		t.Fatalf("the admission file the apiserver reads (%s) names no webhook kubeconfig, so the pin cannot see it", admission)
	}
	// The render writes the admission file and the kubeconfig it names together.
	if _, err := os.Stat(kubeconfig); err != nil {
		t.Fatalf("the render wrote no %s: %v", kubeconfig, err)
	}
	paths := profileConfigPaths(cisProfile())
	if !slices.Contains(paths, kubeconfig) {
		t.Errorf("the CIS apiserver reads %s — the kubeconfig its admission file's webhook plugin names and the render writes — but cisConfigPaths does not back it up and restore it, so a rollback would leave the new release's kubeconfig under the restored admission file", kubeconfig)
	}
}

func TestRefreshProfileConfigsCoversAnyProfile(t *testing.T) {
	withCISConfigs(t)
	prof := profile.Profile{Name: "plain", ConfigFiles: []profile.File{
		{Name: "extra-policy.yaml", Kind: profile.PlainFile, Render: func(profile.Context) ([]byte, error) { return []byte("new"), nil }},
	}}
	writeProfileConfigs = profilefiles.Write
	path := version.ProfileConfigFile("extra-policy.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	webhook := version.AdmissionWebhookConfig()
	if err := os.WriteFile(webhook, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := refreshProfileConfigs(io.Discard, "20261017-000000", prof)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("extra-policy.yaml after the refresh = %q, want this release's render", b)
	}
	if err := os.WriteFile(webhook, []byte("changed since"), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed := restoreAll(io.Discard, items); len(failed) != 0 {
		t.Fatalf("restore failed for %q", failed)
	}
	b, _ := os.ReadFile(path)
	fi, _ := os.Stat(path)
	if string(b) != "old" || fi.Mode().Perm() != 0o640 {
		t.Errorf("extra-policy.yaml after rollback = %q, %o; want the original, 0640", b, fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(webhook); string(b) != "changed since" {
		t.Errorf("a profile without an admission config restored admission-webhook.yaml (%q), which its backup never covered", b)
	}
}

func TestRestoreProfileConfigsNeedsNoProfile(t *testing.T) {
	withCISConfigs(t)
	const ts = "20261017-000001"
	if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
		t.Fatal(err)
	}
	prev := profile.All()
	if err := profile.SetRegistered(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
	if err := restoreProfileConfigs(io.Discard, profileConfigBakDir(ts)); err != nil {
		t.Fatalf("restore with no profile registered = %v", err)
	}
	if _, err := os.Stat(version.AdmissionConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the admission config the upgrade created survived a restore that ran with no profile registered: %v", err)
	}
}

func TestRestoreProfileConfigsRefusesARecordNamingAPathOutsideTheConfigDir(t *testing.T) {
	for _, bad := range []string{"../victim.yaml", "/etc/victim.yaml", "sub/victim.yaml", `..\victim.yaml`, "..", "."} {
		t.Run(bad, func(t *testing.T) {
			withCISConfigs(t)
			const ts = "20261017-000002"
			if _, err := refreshProfileConfigs(io.Discard, ts, cisProfile()); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(filepath.Dir(version.ProfileConfigFile("x")), "..", "victim.yaml")
			if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			bak := profileConfigBakDir(ts)
			record := filepath.Join(bak, profileConfigRecordName)
			raw, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(record, append(raw, []byte(bad+"\n")...), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := restoreProfileConfigs(io.Discard, bak); err == nil {
				t.Fatalf("restore accepted a record naming %q", bad)
			}
			if _, err := os.Stat(victim); err != nil {
				t.Errorf("the refused restore touched %s: %v", victim, err)
			}
			if _, err := os.Stat(version.AdmissionConfigPath); err != nil {
				t.Errorf("the refused restore removed a config before refusing: %v", err)
			}
		})
	}
}
