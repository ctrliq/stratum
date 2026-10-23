package staticpod

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webhook "k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/version"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestWebhookAdmissionConfigPreservesCISPolicy(t *testing.T) {
	oldDir, oldAdmission := version.K8sConfDir, version.AdmissionConfigPath
	t.Cleanup(func() { version.K8sConfDir, version.AdmissionConfigPath = oldDir, oldAdmission })
	version.K8sConfDir = t.TempDir()
	version.AdmissionConfigPath = version.ProfileConfigFile(profiletest.AdmissionConfigFile)
	for _, cis := range []bool{false, true} {
		if err := os.WriteFile(version.AdmissionConfigPath, []byte("apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\nplugins:\n- name: PodSecurity\n  configuration:\n    defaults:\n      enforce: restricted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeWebhookAuth(profiletest.For(cis)); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(version.K8sConfDir, "admission-webhook.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]interface{}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		plugins := cfg["plugins"].([]interface{})
		want := 1
		if cis {
			want = 2
		}
		if len(plugins) != want {
			t.Fatalf("CIS=%v plugins=%v", cis, plugins)
		}
		if cis && !strings.Contains(string(raw), "enforce: restricted") {
			t.Fatal("lost PodSecurity enforcement")
		}
		wh := plugins[len(plugins)-1].(map[string]interface{})
		auth := wh["configuration"].(map[string]interface{})
		if wh["name"] != "MutatingAdmissionWebhook" || auth["kind"] != "WebhookAdmissionConfiguration" {
			t.Fatalf("invalid webhook admission config %v", wh)
		}
		client, err := clientcmd.LoadFromFile(auth["kubeConfigFile"].(string))
		if err != nil {
			t.Fatal(err)
		}
		resolver, err := webhook.NewDefaultAuthenticationInfoResolver(auth["kubeConfigFile"].(string))
		if err != nil {
			t.Fatal(err)
		}
		restConfig, err := resolver.ClientConfigForService("stratum-operator-webhook", "kube-system", 443)
		if err != nil {
			t.Fatal(err)
		}
		if restConfig.CertFile != filepath.Join(version.PKIDir, "front-proxy-client.crt") || restConfig.KeyFile != filepath.Join(version.PKIDir, "front-proxy-client.key") {
			t.Fatal("actual admission resolver did not select webhook credentials")
		}

		identity := client.AuthInfos["stratum-operator-webhook.kube-system.svc:443"]
		if identity == nil || identity.ClientCertificate != filepath.Join(version.PKIDir, "front-proxy-client.crt") || identity.ClientKey != filepath.Join(version.PKIDir, "front-proxy-client.key") {
			t.Fatal("apiserver missing webhook client credentials")
		}
	}
}

// webhookAuthFixture points the render's config directory at a fresh temp dir
// (writeWebhookAuth creates it) and restores the vars it redirects at cleanup.
func webhookAuthFixture(t *testing.T) string {
	t.Helper()
	oldDir, oldAdmission := version.K8sConfDir, version.AdmissionConfigPath
	t.Cleanup(func() { version.K8sConfDir, version.AdmissionConfigPath = oldDir, oldAdmission })
	version.K8sConfDir = t.TempDir()
	version.AdmissionConfigPath = filepath.Join(version.K8sConfDir, "admission-config.yaml")
	return version.K8sConfDir
}

// The webhook render's two files — admission-webhook.yaml and the
// webhook-client.kubeconfig its webhook plugin names — are read by the
// apiserver on start and on every webhook call, so the render writes them
// through the shared durable writer (internal/atomicfile): staged under a
// hidden unique temp name in the file's own directory, fsynced at its final
// mode, renamed into place, and the config directory fsynced so the rename
// survives a power loss. The writer's filesystem side effects are this
// package's own seams (syncFile and syncDir, declared beside the render in
// webhookauth.go and handed to the writer on every call — the pattern
// internal/host/systemd.go established for the same reason), so the tests
// below pin the durability contract through the delegation. The render's old
// raw CreateTemp-and-rename writer never fsynced and never calls the seams,
// so these tests fail on the missing syncs and errors.

// TestWriteWebhookAuthSyncsEachFileAtItsFinalModeThenTheDirectory: each of the
// two files is fsynced at its final mode before the rename — a mode changed
// after the fsync is not covered by it — and the config directory is fsynced
// once per written file, after the rename put that file in place: until that
// sync a power loss can bring the old file back under an apiserver that
// already started with the new one. The files keep the 0600 they carry today.
func TestWriteWebhookAuthSyncsEachFileAtItsFinalModeThenTheDirectory(t *testing.T) {
	confDir := webhookAuthFixture(t)
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })

	pair := []string{
		filepath.Join(confDir, "admission-webhook.yaml"),
		filepath.Join(confDir, "webhook-client.kubeconfig"),
	}
	var syncedModes []os.FileMode
	var syncedDirs []string
	var inPlace []int // how many of the two files each directory sync saw
	syncFile = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		syncedModes = append(syncedModes, fi.Mode().Perm())
		return oldSyncFile(f)
	}
	syncDir = func(d string) error {
		syncedDirs = append(syncedDirs, d)
		n := 0
		for _, p := range pair {
			if _, err := os.Stat(p); err == nil {
				n++
			}
		}
		inPlace = append(inPlace, n)
		return oldSyncDir(d)
	}

	if err := writeWebhookAuth(profile.Profile{}); err != nil {
		t.Fatal(err)
	}
	if len(syncedDirs) != 2 || syncedDirs[0] != confDir || syncedDirs[1] != confDir {
		t.Errorf("synced %v, want the config directory %q once per written file", syncedDirs, confDir)
	}
	if len(syncedModes) != 2 || syncedModes[0] != 0o600 || syncedModes[1] != 0o600 {
		t.Errorf("the render's files were fsynced at modes %v, want [0600 0600] — the final mode, set before the fsync", syncedModes)
	}
	if len(inPlace) == 0 || inPlace[len(inPlace)-1] != 2 {
		t.Errorf("the last directory sync saw %v file(s) in place, want both — the sync must follow the renames it makes durable", inPlace)
	}
	for i, n := range inPlace {
		if n == 0 {
			t.Errorf("directory sync %d ran before either file was renamed into place", i+1)
		}
	}
	for _, p := range pair {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600, the mode the render's files carry today", filepath.Base(p), fi.Mode().Perm())
		}
	}
	if entries, _ := os.ReadDir(confDir); len(entries) != 2 {
		t.Errorf("the config dir holds %d entries, want only the two written files (a stray staging copy?)", len(entries))
	}
}

// TestWriteWebhookAuthKeepsTheOldPairWhenTheSyncFails: the failure injection
// the sync seam makes possible, and the whole point of the durable writer: a
// render whose file cannot be made durable must fail the render and leave the
// file the running apiserver reads byte for byte, with no staging copy left
// behind — a torn admission webhook where a working one was is the incident
// this writer exists to prevent. The render's old raw writer never fsynced,
// never calls the seam, and fails this test on the missing error.
func TestWriteWebhookAuthKeepsTheOldPairWhenTheSyncFails(t *testing.T) {
	confDir := webhookAuthFixture(t)
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })
	boom := errors.New("fsync: input/output error")
	syncFile = func(*os.File) error { return boom }
	syncDir = oldSyncDir

	// The pair as the running apiserver already reads it.
	old := map[string]string{
		"admission-webhook.yaml":    "old admission-webhook",
		"webhook-client.kubeconfig": "old webhook-client",
	}
	for name, body := range old {
		if err := os.WriteFile(filepath.Join(confDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := writeWebhookAuth(profile.Profile{}); !errors.Is(err, boom) {
		t.Fatalf("writeWebhookAuth = %v, want the fsync's error — the replacement could not be made durable", err)
	}
	for name, want := range old {
		if b, _ := os.ReadFile(filepath.Join(confDir, name)); string(b) != want {
			t.Errorf("%s = %q after the failed render, want the old bytes kept", name, b)
		}
	}
	if entries, _ := os.ReadDir(confDir); len(entries) != len(old) {
		t.Errorf("the config dir holds %d entries after the failed render, want only the old pair (a stray staging copy?)", len(entries))
	}
}

// TestWriteWebhookAuthReportsAnUnsyncedRename: the rename is durable only once
// the directory holding it is synced, so a failed directory sync is a failed
// render reported as the writer's ErrNotDurable — the renamed file is already
// in place, and the caller must learn that a power loss could still bring the
// old one back.
func TestWriteWebhookAuthReportsAnUnsyncedRename(t *testing.T) {
	confDir := webhookAuthFixture(t)
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })
	boom := errors.New("fsync: input/output error")
	syncFile = oldSyncFile
	syncDir = func(string) error { return boom }

	if err := writeWebhookAuth(profile.Profile{}); !errors.Is(err, boom) || !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("writeWebhookAuth = %v, want the directory sync's error marked atomicfile.ErrNotDurable", err)
	}
	// The first write's rename landed before its directory sync failed; the
	// render stops there, so exactly one of the pair is new and no staging
	// copy is left behind.
	entries, _ := os.ReadDir(confDir)
	if len(entries) != 1 {
		t.Errorf("the config dir holds %d entries after the failed sync, want the one renamed file", len(entries))
	}
	for _, e := range entries {
		if e.Name() != "admission-webhook.yaml" && e.Name() != "webhook-client.kubeconfig" {
			t.Errorf("the config dir holds %q after the failed sync — a stray staging copy", e.Name())
		}
	}
}
