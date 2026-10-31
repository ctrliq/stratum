package version

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNodeFileOwnersCoverTheWriterPackages pins the record's coverage: the
// packages that write node files each own at least one. The write guard
// (tools/nodefilewrites) derives its package set from these values, so a
// package missing here writes outside the rule — and its raw writes pass
// make lint unseen. internal/host and internal/cluster are in the set for
// the same reason the rest are: the host's sysctl and modules-load files,
// the Cilium settings record and the component .args files are node files,
// so their raw writes must fail make lint like every other owner's.
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

// TestNodeFileOwnersNameTheRuntimeConfigs pins the runtime half of the
// record: kubelet.conf and containerd.toml are start-critical — a torn one
// stops the kubelet or every pod sandbox — and the confinement record and
// the CoreDNS upstream file sit beside them under the roots, so all four
// belong to the one package that writes them, internal/runtime, and that
// package sits in the write guard's scanned set.
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

// TestNodeFileOwnersNameTheClusterNodeFiles pins internal/cluster's node
// files in the record. The Cilium settings record rides the stratum-certs
// channel to a joining control plane, which refuses to join against settings
// it never ran — a torn one is a refused join, not a recovered one — and the
// component .args files under the config directory are the on-disk truth
// of each component's rendered argv. Both are written by internal/cluster, so
// the entries name it, and the write guard scans the package through them.
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

// TestTheSysctlAndModulesLoadPathsLiveBesideSystemdUnitPath pins the one home
// of the three host-side paths bring-up writes outside the roots:
// version declares them beside SystemdUnitPath (a host path outside the three
// roots never moves with them), and the owning-package record is keyed
// through these variables — a second spelling of the paths, in host or in the
// record, would let the writer, the sysctl loader and uninstall's delete list
// drift apart.
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

// TestNodeFileOwnersValuesArePackagePaths keeps the record honest: an owning
// package is a package directory under internal/, the form the write guard
// matches a file's directory against, so a value in any other spelling owns
// nothing.
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

// setRoots redirects the three roots through SetRoots for the test and
// restores the previous roots — and with them every derived path — with
// t.Cleanup. This is the one redirection helper new tests should reach for: a
// direct assignment of a root var leaves the derived paths (the lock paths,
// ControllerManagerManifest, ...) pointing at the old root.
func setRoots(t *testing.T, dataDir, confDir, runDir string) {
	t.Helper()
	data, conf, run := DataDir, ConfDir, RunDir
	SetRoots(dataDir, confDir, runDir)
	t.Cleanup(func() { SetRoots(data, conf, run) })
}

// TestSetRootsRecomputesEveryDerivedPath: one SetRoots(DataDir, ConfDir,
// RunDir) call reassigns the three roots and recomputes every path derived
// from them. The table names each derived var and restates its derivation, so
// a var that stops following its root — above all the ones the var block
// derives once at init (StartLockPath, NodeUpgradeLockPath,
// ControllerManagerManifest), which a direct root reassignment leaves stale —
// fails here. The wants are the shipped layout under the new roots.
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
		// The init-time derivations the brief names: derived from RunDir and
		// K8sConfDir once at package init before this task.
		{"StartLockPath", StartLockPath, run + "-start.lock"},
		{"NodeUpgradeLockPath", NodeUpgradeLockPath, run + "-upgrade.lock"},
		{"ControllerManagerManifest", ControllerManagerManifest,
			filepath.Join(data, "config", "manifests", "kube-controller-manager.yaml")},
		// Everything else derived from the three roots.
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
		{"NetConfigPath", NetConfigPath, filepath.Join(data, "pki", "netconfig")},
		{"CiliumSettingsPath", CiliumSettingsPath, filepath.Join(data, "pki", "cilium.json")},
		// Moved here from internal/assets and internal/upgrade; the payload
		// stage root IS the data root, and the upgrade payload stages under it.
		{"DefaultRoot", DefaultRoot, data},
		{"StageDir", StageDir, filepath.Join(data, "upgrade", "staged")},
		// The call-time resolvers follow the config directory on every call;
		// they must answer the redirected one too.
		{"AdmissionWebhookConfig()", AdmissionWebhookConfig(), filepath.Join(data, "config", "admission-webhook.yaml")},
		{"WebhookClientKubeconfig()", WebhookClientKubeconfig(), filepath.Join(data, "config", "webhook-client.kubeconfig")},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}

	// The host-side paths outside the three roots must NOT move with them:
	// they are not stratum's data, and uninstall removes them as its own
	// special targets rather than with the data dir.
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

// TestSetRootsRedirectRestoresThePreviousRoots: the helper's t.Cleanup puts
// the three roots back AND, with them, every derived path — the init-time
// derivations included. A restore that moved only the roots would leave the
// derived paths pointing into the temp dir for every test that runs after this
// one in the same binary.
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
