package version

import (
	"path/filepath"
	"strings"
	"testing"
)

// Pins that every node-file writer package owns at least one entry.
func TestNodeFileOwnersCoverTheWriterPackages(t *testing.T) {
	writerPkgs := []string{
		"internal/pki",
		"internal/kubeconfig",
		"internal/bootstrap",
		"internal/netcfg",
		"internal/profilefiles",
		"internal/certs",
		"internal/nodestate",
		"internal/runtime",
		"internal/staticpod",
		"internal/upgrade",
		"internal/host",
		"internal/cluster",
		"internal/etcdsnap",
	}
	owned := make(map[string]bool, len(NodeFileOwners))
	for _, pkg := range NodeFileOwners {
		owned[pkg] = true
	}
	for _, pkg := range writerPkgs {
		if !owned[pkg] {
			t.Errorf("NodeFileOwners names no node file owned by %s", pkg)
		}
	}
}

// Pins internal/runtime's start-critical node files in the record.
func TestNodeFileOwnersNameTheRuntimeConfigs(t *testing.T) {
	for _, path := range []string{
		filepath.Join(K8sConfDir, "kubelet.conf"),
		ContainerdConfig,
		ContainerdRuntimeConfD,
		filepath.Join(K8sConfDir, "selinux-confined-since"),
		filepath.Join(CoreDNSUpstreamDir, "resolv.conf"),
	} {
		if NodeFileOwners[path] != "internal/runtime" {
			t.Errorf("NodeFileOwners has no entry for %s owned by internal/runtime: the runtime's node files must sit in the owning-package record so the write guard scans the package", path)
		}
	}
}

// Pins internal/cluster's node files in the record.
func TestNodeFileOwnersNameTheClusterNodeFiles(t *testing.T) {
	for _, path := range []string{
		CiliumSettingsPath,
		filepath.Join(K8sConfDir, "components"),
	} {
		if NodeFileOwners[path] != "internal/cluster" {
			t.Errorf("NodeFileOwners has no entry for %s owned by internal/cluster: the Cilium settings record and the component .args files must sit in the owning-package record so the write guard scans internal/cluster", path)
		}
	}
}

// Pins the host sysctl and modules-load paths as static /etc paths.
func TestTheSysctlAndModulesLoadPathsLiveBesideSystemdUnitPath(t *testing.T) {
	for _, tt := range []struct {
		name, got, want string
	}{
		{"ModulesLoadFile", ModulesLoadFile, "/etc/modules-load.d/k8s.conf"},
		{"SysctlConf", SysctlConf, "/etc/sysctl.d/99-stratum.conf"},
		{"SysctlCISConf", SysctlCISConf, "/etc/sysctl.d/99-stratum-cis.conf"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q declared in version beside SystemdUnitPath — the one home host reads, Footprint returns and the record keys", tt.name, tt.got, tt.want)
		}
	}

	data, conf, run := t.TempDir(), t.TempDir(), t.TempDir()
	setRoots(t, data, conf, run)
	for name, got := range map[string]string{
		"ModulesLoadFile": ModulesLoadFile,
		"SysctlConf":      SysctlConf,
		"SysctlCISConf":   SysctlCISConf,
	} {
		if !strings.HasPrefix(got, "/etc/") {
			t.Errorf("%s = %q after SetRoots: a host path outside the three roots must not move with them", name, got)
		}
	}
	if NodeFileOwners[ModulesLoadFile] != "internal/host" ||
		NodeFileOwners[SysctlConf] != "internal/host" ||
		NodeFileOwners[SysctlCISConf] != "internal/host" {
		t.Errorf("the record is not keyed through the version path vars: ModulesLoadFile, SysctlConf and SysctlCISConf must each be owned by internal/host, got %q, %q and %q",
			NodeFileOwners[ModulesLoadFile], NodeFileOwners[SysctlConf], NodeFileOwners[SysctlCISConf])
	}
}

// Owning packages must be internal/ directory paths the write guard can match.
func TestNodeFileOwnersValuesArePackagePaths(t *testing.T) {
	for path, pkg := range NodeFileOwners {
		if !strings.HasPrefix(pkg, "internal/") || strings.HasSuffix(pkg, "/") || strings.Contains(pkg, "//") {
			t.Errorf("NodeFileOwners[%s] = %q: not an internal package directory", path, pkg)
		}
		if !strings.HasPrefix(path, "/") {
			t.Errorf("NodeFileOwners[%s]: key is not an absolute node path", path)
		}
	}
}

// Redirects the three roots for the test and restores them and every derived path on cleanup.
func setRoots(t *testing.T, dataDir, confDir, runDir string) {
	t.Helper()
	data, conf, run := DataDir, ConfDir, RunDir
	SetRoots(dataDir, confDir, runDir)
	t.Cleanup(func() { SetRoots(data, conf, run) })
}

// Pins every derived path's recomputation across a SetRoots redirect.
func TestSetRootsRecomputesEveryDerivedPath(t *testing.T) {
	data, conf, run := t.TempDir(), t.TempDir(), t.TempDir()
	setRoots(t, data, conf, run)

	for _, tt := range []struct {
		name string
		got  string
		want string
	}{
		{"DataDir", DataDir, data},
		{"ConfDir", ConfDir, conf},
		{"RunDir", RunDir, run},
		{"StartLockPath", StartLockPath, run + "-start.lock"},
		{"NodeUpgradeLockPath", NodeUpgradeLockPath, run + "-upgrade.lock"},
		{"ControllerManagerManifest", ControllerManagerManifest,
			filepath.Join(data, "config", "manifests", "kube-controller-manager.yaml")},
		{"PKIDir", PKIDir, filepath.Join(data, "pki")},
		{"KubeconfigDir", KubeconfigDir, filepath.Join(data, "kubeconfig")},
		{"UpgradeStageDir", UpgradeStageDir, filepath.Join(data, "upgrade")},
		{"BootstrapKubeconfig", BootstrapKubeconfig, filepath.Join(data, "kubeconfig", "bootstrap.conf")},
		{"K8sConfDir", K8sConfDir, filepath.Join(data, "config")},
		{"CoreDNSUpstreamDir", CoreDNSUpstreamDir, filepath.Join(data, "dns")},
		{"AdminConf", AdminConf, filepath.Join(data, "kubeconfig", "admin.conf")},
		{"BinDir", BinDir, filepath.Join(data, "bin")},
		{"LogDir", LogDir, filepath.Join(data, "log")},
		{"ControlSocket", ControlSocket, filepath.Join(run, "stratum.sock")},
		{"EtcdDataDir", EtcdDataDir, filepath.Join(data, "etcd")},
		{"ManifestsDir", ManifestsDir, filepath.Join(data, "manifests")},
		{"HelmCacheDir", HelmCacheDir, filepath.Join(data, "helm")},
		{"ContainerdConfig", ContainerdConfig, filepath.Join(data, "config", "containerd.toml")},
		{"ContainerdRootDir", ContainerdRootDir, filepath.Join(data, "containerd")},
		{"ContainerdStateDir", ContainerdStateDir, filepath.Join(run, "containerd")},
		{"AuditPolicyPath", AuditPolicyPath, filepath.Join(data, "config", "audit-policy.yaml")},
		{"AdmissionConfigPath", AdmissionConfigPath, filepath.Join(data, "config", "admission-config.yaml")},
		{"EncryptionConfigPath", EncryptionConfigPath, filepath.Join(data, "pki", "encryption-config.yaml")},
		{"RegistriesConfig", RegistriesConfig, filepath.Join(conf, "registries.yaml")},
		{"ImagesDropInDir", ImagesDropInDir, filepath.Join(conf, "images.d")},
		{"NetConfigPath", NetConfigPath, filepath.Join(data, "pki", "netconfig")},
		{"CiliumSettingsPath", CiliumSettingsPath, filepath.Join(data, "pki", "cilium.json")},
		{"DefaultRoot", DefaultRoot, data},
		{"StageDir", StageDir, filepath.Join(data, "upgrade", "staged")},
		{"AdmissionWebhookConfig()", AdmissionWebhookConfig(), filepath.Join(data, "config", "admission-webhook.yaml")},
		{"WebhookClientKubeconfig()", WebhookClientKubeconfig(), filepath.Join(data, "config", "webhook-client.kubeconfig")},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}

	for _, tt := range []struct {
		name, got, want string
	}{
		{"StratumInstallPath", StratumInstallPath, "/usr/local/bin/stratum"},
		{"CertsDir", CertsDir, "/etc/containerd/certs.d"},
		{"ContainerdConfD", ContainerdConfD, "/etc/containerd/conf.d/stratum-registry.toml"},
		{"SystemdUnitPath", SystemdUnitPath, "/etc/systemd/system/stratum.service"},
		{"AuditLogDir", AuditLogDir, "/var/log/stratum"},
		{"AuditLogPath", AuditLogPath, "/var/log/stratum/audit.log"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q after SetRoots; the host paths outside the three roots must not move", tt.name, tt.got)
		}
	}
}

// The setRoots helper must restore the derived paths along with the roots.
func TestSetRootsRedirectRestoresThePreviousRoots(t *testing.T) {
	data, conf, run := DataDir, ConfDir, RunDir

	t.Run("redirected", func(t *testing.T) {
		setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
		if DataDir == data {
			t.Error("SetRoots left DataDir at its default; nothing was redirected")
		}
		if PKIDir != filepath.Join(DataDir, "pki") {
			t.Errorf("after SetRoots, PKIDir = %q, want the redirected data root's %q", PKIDir, filepath.Join(DataDir, "pki"))
		}
	})

	if DataDir != data || ConfDir != conf || RunDir != run {
		t.Errorf("roots not restored: DataDir=%q ConfDir=%q RunDir=%q; the helper must restore all three", DataDir, ConfDir, RunDir)
	}
	if PKIDir != filepath.Join(data, "pki") {
		t.Errorf("after the restore, PKIDir = %q, want the restored data root's %q — the derived paths must come back with the roots",
			PKIDir, filepath.Join(data, "pki"))
	}
	if StartLockPath != run+"-start.lock" || NodeUpgradeLockPath != run+"-upgrade.lock" {
		t.Errorf("after the restore, StartLockPath = %q and NodeUpgradeLockPath = %q; the init-time derivations must follow the restored RunDir %q",
			StartLockPath, NodeUpgradeLockPath, run)
	}
	if ControllerManagerManifest != filepath.Join(data, "config", "manifests", "kube-controller-manager.yaml") {
		t.Errorf("after the restore, ControllerManagerManifest = %q; it must follow the restored config directory %q",
			ControllerManagerManifest, data)
	}
}
