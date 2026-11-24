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

// points the render's config directory at a fresh temp dir and restores the vars.
func webhookAuthFixture(t *testing.T) string {
	t.Helper()
	oldDir, oldAdmission := version.K8sConfDir, version.AdmissionConfigPath
	t.Cleanup(func() { version.K8sConfDir, version.AdmissionConfigPath = oldDir, oldAdmission })
	version.K8sConfDir = t.TempDir()
	version.AdmissionConfigPath = filepath.Join(version.K8sConfDir, "admission-config.yaml")
	return version.K8sConfDir
}

// the render's durability contract is pinned through its syncFile and syncDir seams.
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
	var inPlace []int
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

// a file that cannot be made durable must fail the render and leave the old pair untouched.
func TestWriteWebhookAuthKeepsTheOldPairWhenTheSyncFails(t *testing.T) {
	confDir := webhookAuthFixture(t)
	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })
	boom := errors.New("fsync: input/output error")
	syncFile = func(*os.File) error { return boom }
	syncDir = oldSyncDir

	// the pair as the running apiserver already reads it.
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

// the rename is durable only once the directory holding it is synced.
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
	// the first write's rename landed before its directory sync failed.
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
