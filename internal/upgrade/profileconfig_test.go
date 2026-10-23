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

// withCISConfigs points the CIS config paths and DataDir at a temp dir, and
// writeProfileConfigs at a stub that writes "new" to both.
func withCISConfigs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldAdm, oldAudit, oldData, oldConf, oldWrite := version.AdmissionConfigPath, version.AuditPolicyPath, version.DataDir, version.K8sConfDir, writeProfileConfigs
	t.Cleanup(func() {
		version.AdmissionConfigPath, version.AuditPolicyPath, version.DataDir, version.K8sConfDir, writeProfileConfigs = oldAdm, oldAudit, oldData, oldConf, oldWrite
	})
	// The derived admission file has no path var of its own: it follows the
	// config directory through AdmissionWebhookConfig, which is why redirecting
	// K8sConfDir here is enough — and enough for production too, since the
	// backup list, the render and the argv all resolve that one call.
	version.K8sConfDir = filepath.Join(dir, "config")
	version.AdmissionConfigPath = filepath.Join(dir, "config", "admission-config.yaml")
	version.AuditPolicyPath = filepath.Join(dir, "config", "audit-policy.yaml")
	version.DataDir = filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// This release's configs, and the derived file the apiserver re-render
	// writes from them: named here, not taken from cisConfigPaths, so a file
	// dropped from the backup set is still changed and its loss is seen.
	writeProfileConfigs = func(profile.Profile) error {
		for _, p := range []string{version.AdmissionConfigPath, version.AuditPolicyPath, filepath.Join(version.K8sConfDir, "admission-webhook.yaml")} {
			if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestRefreshProfileConfigsBacksUpAndRollsBack: an
// upgrade re-renders the apiserver with its new image, and the pod that
// recreates reads the admission config on disk, so the upgrade writes this
// release's first. What it replaced is backed up where `stratum rollback`
// finds it, and restoring puts exactly that back: the old content as a config
// file, and a config that did not exist before removed again.
func TestRefreshProfileConfigsBacksUpAndRollsBack(t *testing.T) {
	withCISConfigs(t)
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("old admission"), 0o644); err != nil {
		t.Fatal(err)
	}
	// What the apiserver actually reads, derived from the admission config when
	// its manifest is rendered, and written 0600.
	webhook := filepath.Join(version.K8sConfDir, "admission-webhook.yaml")
	if err := os.WriteFile(webhook, []byte("old webhook"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No audit policy yet: the upgrade creates it.

	const ts = "20260927-000000"
	items, err := refreshProfileConfigs(io.Discard, ts, cisProfile())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(version.AdmissionConfigPath); string(b) != "new" {
		t.Errorf("admission config after the refresh = %q, want this release's", b)
	}

	// `stratum rollback` finds the same set the in-process rollback holds.
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
	// A rollback's restored manifests are not re-rendered, so the derived file
	// is restored itself, with its own mode.
	b, _ = os.ReadFile(webhook)
	fi, _ = os.Stat(webhook)
	if string(b) != "old webhook" || fi.Mode().Perm() != 0o600 {
		t.Errorf("admission-webhook.yaml after rollback = %q, %o; want the original, 0600", b, fi.Mode().Perm())
	}

	// A write that fails still returns the backup, so the caller's rollback
	// puts it back.
	writeProfileConfigs = func(profile.Profile) error { return errors.New("not stamped") }
	if items, err := refreshProfileConfigs(io.Discard, "20260927-000001", cisProfile()); err == nil || len(items) != 1 {
		t.Errorf("refreshProfileConfigs = %d backups, %v; want the backup and the error", len(items), err)
	}
}

// TestRestoreProfileConfigsKeepsTheKubeconfigAPreChangeBackupNeverHeld: a backup
// written before webhook-client.kubeconfig joined the profile's list holds
// only the three files that predate the kubeconfig — no record, never the kubeconfig,
// though the node had one: the render writes it on every apiserver write, CIS
// or not. Restoring such a set must not read that absence as "did not exist
// before the upgrade": the restored admission-webhook.yaml still names the
// kubeconfig, and deleting it leaves the rolled-back apiserver an admission
// chain it cannot load.
func TestRestoreProfileConfigsKeepsTheKubeconfigAPreChangeBackupNeverHeld(t *testing.T) {
	withCISConfigs(t)
	kubeconfig := version.WebhookClientKubeconfig()
	webhookFile := version.AdmissionWebhookConfig()

	// The backup a pre-change binary wrote: the three paths its list held,
	// and no record of any fourth.
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
	// The admission file the old release's render derived, its webhook
	// plugin naming the kubeconfig as every release's does.
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

	// The node as the upgrade left it: this release's configs, and the
	// kubeconfig the pre-change backup never copied.
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

	// The restored admission file must still name a kubeconfig that exists.
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

// A pre-record backup covered the legacy three files. An omitted file means
// it did not exist before the upgrade and must be removed during rollback;
// the webhook client kubeconfig was never covered by that backup format.
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

// TestPruneKeepsOnlyRecentProfileConfigBackups: every upgrade
// writes a CIS config backup set, and the success path prunes old ones with
// the manifest sets, so they cannot accumulate.
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

// TestRefreshProfileConfigsLeavesNoPartialBackup: a backup that
// fails part-way is not handed to rollback, and none is left for `stratum
// rollback` to find, since every file missing from it would read as one that
// did not exist and be deleted. The live configs are left as they were, and
// nothing is written.
func TestRefreshProfileConfigsLeavesNoPartialBackup(t *testing.T) {
	withCISConfigs(t)
	if err := os.WriteFile(version.AdmissionConfigPath, []byte("old admission"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where the audit policy should be: stat succeeds, and the
	// copy of it fails, after the admission config's copy succeeded.
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

// TestRollbackRestoresProfileConfigsBeforeManifests: kubelet acts
// on restored manifests at once, so an older apiserver started from them must
// already find its own admission configs rather than the newer pin.
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

// TestRestoreAllRestoresInThatOrder: restoreAll itself follows restoreOrder,
// seen in the order its failures are reported when every restore fails.
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

// TestProfileConfigPathsCoverEveryFileTheCISAPIServerReads closes the gap a
// hand-maintained backup list leaves: it renders a CIS control plane, reads the
// apiserver's argv back off the manifest kubelet runs, and requires that every
// flag value naming a file under the config directory — plus the admission
// config admission-webhook.yaml is derived from — is in profileConfigPaths(cisProfile()). A
// file a CIS apiserver reads that the upgrade does not back up and restore is
// one a rolled-back apiserver keeps reading from the new release, which is how
// a rollback once kept the new Pod Security pin.
func TestProfileConfigPathsCoverEveryFileTheCISAPIServerReads(t *testing.T) {
	withCISConfigs(t)
	// writeWebhookAuth under CIS reads the admission config the profile is
	// seeded from; without it the render fails before the argv exists. The
	// render also stamps the audit policy's sum into the manifest, so the
	// policy file must exist too.
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
	// The admission config is read through the derived file; the upgrade backs
	// it up in its own right. Everything else the apiserver reads comes off its
	// argv: each flag value naming a file under the config directory.
	reads := []string{version.AdmissionConfigPath}
	for _, a := range spec.Argv {
		_, value, ok := strings.Cut(a, "=")
		if !ok {
			continue // a flag with no value carries no file path
		}
		if strings.HasPrefix(value, version.K8sConfDir+string(filepath.Separator)) {
			reads = append(reads, value)
		}
	}
	// The fixture must actually exercise the collection: the derived admission
	// file is the one this pin exists for, so an argv that stops naming it is
	// a broken pin, not a green one.
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

// TestProfileConfigPathsComeFromTheProfile: the backup set follows the
// profile's ConfigFiles, each where the renderers read it, and a profile with
// an AdmissionConfiguration adds the two files the render derives from it,
// resolved through the same version calls the render writes them with:
// writer, argv and backup list resolve one location once. A profile without
// an admission config backs up its own files alone.
func TestProfileConfigPathsComeFromTheProfile(t *testing.T) {
	withCISConfigs(t)
	// Move the config directory out from under the fixture's first layout:
	// a list that rebuilt a path against the old directory, instead of
	// resolving it at call time, drops or misnames it right here.
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

// TestProfileConfigPathsCoverTheWebhookClientKubeconfig: the admission file a
// CIS apiserver reads points its MutatingAdmissionWebhook plugin at a
// second file, webhook-client.kubeconfig, and the render that writes the
// admission file writes that kubeconfig beside it. The apiserver reads the
// kubeconfig on every webhook call, so the upgrade must back it up and
// restore it with the rest: a rollback that puts the admission file back
// but not the kubeconfig it names leaves the new release's client config
// under the old admission chain. No argv flag names the kubeconfig, so the
// argv-walking pin above cannot see it; this test reads it the way the
// apiserver does, out of the admission file.
func TestProfileConfigPathsCoverTheWebhookClientKubeconfig(t *testing.T) {
	withCISConfigs(t)
	// writeWebhookAuth under CIS reads the admission config the render
	// derives the webhook file from; without it the render fails before
	// the kubeconfig exists. The render also stamps the audit policy's
	// sum into the manifest, so the policy file must exist too.
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
	// The same render that writes the admission file writes the kubeconfig
	// it names; one without the other is a broken render, not a green pin.
	if _, err := os.Stat(kubeconfig); err != nil {
		t.Fatalf("the render wrote no %s: %v", kubeconfig, err)
	}
	paths := profileConfigPaths(cisProfile())
	if !slices.Contains(paths, kubeconfig) {
		t.Errorf("the CIS apiserver reads %s — the kubeconfig its admission file's webhook plugin names and the render writes — but cisConfigPaths does not back it up and restore it, so a rollback would leave the new release's kubeconfig under the restored admission file", kubeconfig)
	}
}

// A profile other than cis gets the same backup and rollback over its own
// ConfigFiles: written by this release's render, backed up first, and put
// back exactly, with no admission pair when it declares no admission config.
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

// The restore reads what to put back from the backup itself, so a rollback
// restores the configs the upgrade backed up whatever profile is registered
// or selected now: no profile is resolved on the way back.
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
