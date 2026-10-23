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

// The render's durable single-file writer's own test seams. The writer lives
// in internal/atomicfile, and the loop below hands these to it on every call,
// so the tests that pin the render's write behavior keep working through the
// delegation (the pattern internal/host/systemd.go established for the same
// reason).
var (
	// syncFile is (*os.File).Sync behind a seam, so a test can see what the
	// file looks like at the moment it is made durable.
	syncFile = func(f *os.File) error { return f.Sync() }
	// syncDir fsyncs a directory behind a seam, so a test can see it is
	// called, and with which directory.
	syncDir = atomicfile.SyncDir
)

// atomicfileSeams returns the render's write seams, for handing to the
// atomicfile writer.
func atomicfileSeams() atomicfile.Seams {
	return atomicfile.Seams{
		SyncFile: syncFile,
		SyncDir:  syncDir,
	}
}

// Write the admission client configuration before rendering kube-apiserver.
// Reuse its dedicated front-proxy identity: tenants cannot mint this certificate,
// and existing CA rotation already renews it. No private key is copied.
//
// A profile that declares an AdmissionConfiguration file is the base the
// engine's own admission plugins are merged into.
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
	// Each file is written at the location internal/version names it by —
	// the admission file the apiserver's argv carries, the kubeconfig that
	// file's plugin names — so the render writes what every reader names.
	// The apiserver reads both on start and on every webhook call, so each
	// goes through the shared durable writer: staged under a hidden unique
	// temp name in this directory, fsynced at its final 0600, renamed into
	// place, and the directory fsynced so the rename survives a power loss —
	// a failed render leaves the file the running apiserver reads untouched,
	// never a torn one where a working one was.
	for dst, body := range map[string][]byte{version.WebhookClientKubeconfig(): []byte(client), version.AdmissionWebhookConfig(): raw} {
		if err := atomicfile.WriteFileWith(dst, body, 0o600, atomicfileSeams()); err != nil {
			return err
		}
	}
	return nil
}
