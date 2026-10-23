package version

import (
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestProfileLocationsResolveTheCISFilesToTheirPaths(t *testing.T) {
	pki := func(name string) string { return filepath.Join(PKIDir, name) }
	at := ProfileLocations(pki)
	for _, c := range []struct{ what, got, want string }{
		{"the audit policy", at.Config(profiletest.AuditPolicyFile), AuditPolicyPath},
		{"the admission config", at.Config(profiletest.AdmissionConfigFile), AdmissionConfigPath},
		{"the encryption config", at.Secret(profiletest.EncryptionConfigFile), EncryptionConfigPath},
		{"the audit log", at.Log(profiletest.AuditLogFile), AuditLogPath},
		{"the encryption config's secret path", ProfileSecretFile(profiletest.EncryptionConfigFile), EncryptionConfigPath},
	} {
		if c.got != c.want {
			t.Errorf("%s resolves to %q, want %q", c.what, c.got, c.want)
		}
	}
}

func TestTheEngineConfigFilesAreReservedFromProfiles(t *testing.T) {
	profileFiles := map[string]bool{AuditPolicyPath: true, AdmissionConfigPath: true}
	owned := []string{AdmissionWebhookConfig(), WebhookClientKubeconfig(), ContainerdConfig, filepath.Dir(ControllerManagerManifest)}
	for p := range NodeFileOwners {
		if filepath.Dir(p) == K8sConfDir && !profileFiles[p] {
			owned = append(owned, p)
		}
	}
	for _, p := range owned {
		if filepath.Dir(p) != K8sConfDir {
			t.Fatalf("%s is not in the config directory %s; the reserved names are that directory's", p, K8sConfDir)
		}
		if !profile.EngineOwnsConfigName(filepath.Base(p)) {
			t.Errorf("%s is the engine's, but a profile may declare a config file named %q and replace it", p, filepath.Base(p))
		}
	}
	for _, name := range []string{profiletest.AuditPolicyFile, profiletest.AdmissionConfigFile} {
		if profile.EngineOwnsConfigName(name) {
			t.Errorf("%q is the cis profile's config file, not the engine's", name)
		}
	}
	if !profile.EngineOwnsConfigName("stratum-images.tar.imported") {
		t.Error("a payload import marker is not reserved")
	}
}
