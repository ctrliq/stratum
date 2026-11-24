package staticpod

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// the render's write seams, handed to the atomicfile writer on every call.
var (
	// a seam, so a test can see the file when it is made durable.
	syncFile = func(f *os.File) error { return f.Sync() }
	// a seam, so a test can see which directory is synced.
	syncDir = atomicfile.SyncDir
)

// returns the render's write seams for the atomicfile writer.
func atomicfileSeams() atomicfile.Seams {
	return atomicfile.Seams{
		SyncFile: syncFile,
		SyncDir:  syncDir,
	}
}

// reuses the front-proxy identity: tenants cannot mint it and CA rotation renews it.
func writeWebhookAuth(p profile.Profile) error {
	config := map[string]interface{}{"apiVersion": "apiserver.config.k8s.io/v1", "kind": "AdmissionConfiguration"}
	if f, ok := p.AdmissionFile(); ok {
		raw, err := os.ReadFile(version.ProfileConfigFile(f.Name))
		if err != nil {
			return fmt.Errorf("read the %s profile's admission config: %w", p.Name, err)
		}
		if err := yaml.Unmarshal(raw, &config); err != nil {
			return err
		}
	}
	plugins, _ := config["plugins"].([]interface{})
	plugins = append(plugins, map[string]interface{}{"name": "MutatingAdmissionWebhook", "configuration": map[string]interface{}{
		"apiVersion": "apiserver.config.k8s.io/v1", "kind": "WebhookAdmissionConfiguration", "kubeConfigFile": version.WebhookClientKubeconfig(),
	}})
	config["plugins"] = plugins
	raw, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(version.K8sConfDir, 0o755); err != nil {
		return err
	}
	client := fmt.Sprintf(`apiVersion: v1
kind: Config
users:
- name: stratum-operator-webhook.kube-system.svc:443
  user:
    client-certificate: %s
    client-key: %s
`, filepath.Join(version.PKIDir, "front-proxy-client.crt"), filepath.Join(version.PKIDir, "front-proxy-client.key"))
	// the apiserver reads both on start and on every webhook call: durable writes.
	for dst, body := range map[string][]byte{version.WebhookClientKubeconfig(): []byte(client), version.AdmissionWebhookConfig(): raw} {
		if err := atomicfile.WriteFileWith(dst, body, 0o600, atomicfileSeams()); err != nil {
			return err
		}
	}
	return nil
}
