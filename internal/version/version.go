// Package version holds the ldflags-stamped build versions and the shared on-disk layout.
package version

import (
	"path/filepath"

	"github.com/ctrliq/stratum/internal/profile"
)

// Stamped at link time from the pin manifest by the repo Makefile.
var (
	StratumVersion = "v0.0.0-dev"
	BuildID        = "unknown"
	K8sVersion     = "unknown"
	EtcdVersion    = "unknown"

	// Stamped here so the report does not depend on the deprecated WithComponentVersions wiring.
	ContainerdVersion = "unknown"
	RuncVersion       = "unknown"
	KubeVipVersion    = "unknown"
	CiliumVersion     = "unknown"
	CorednsVersion    = "unknown"

	// The airgap payload must carry it: those node Jobs run with no registry.
	NsenterImage = "ghcr.io/ctrliq/stratum/busybox:unknown"

	// Same stamping contract as NsenterImage.
	EtcdImage                  = "ghcr.io/ctrliq/stratum/etcd:unknown"
	KubeAPIServerImage         = "ghcr.io/ctrliq/stratum/kube-apiserver:unknown"
	KubeControllerManagerImage = "ghcr.io/ctrliq/stratum/kube-controller-manager:unknown"
	KubeSchedulerImage         = "ghcr.io/ctrliq/stratum/kube-scheduler:unknown"
	KubeVipImage               = "ghcr.io/ctrliq/stratum/kube-vip:unknown"
)

const (
	APIServerPort   = 6443
	EtcdClientPort  = 2379
	EtcdPeerPort    = 2380
	EtcdMetricsPort = 2381
	KubeletPort     = 10250

	// Shared with the bash installer.
	EtcdName    = "stratum"
	ClusterName = "stratum"

	// Snapshot restore derives its own unique token so an old peer cannot crash the fresh cluster.
	EtcdClusterToken = "stratum-etcd"

	// bootstrapsigner is disabled by default; "*" keeps every default controller on as well.
	ControllerManagerControllersFlag = "--controllers=*,bootstrapsigner,tokencleaner"

	CertRenewWindowDays = 45

	SnapshotIntervalHours = 12
	SnapshotRetainDefault = 5

	TLSCipherSuites = "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256," +
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384," +
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305"
)

// Vars, not consts, so tests can redirect the layout; SetRoots recomputes the derived paths.
var (
	ConfDir             = "/etc/stratum"
	DataDir             = "/var/lib/stratum"
	RunDir              = "/run/stratum"
	StartLockPath       string
	NodeUpgradeLockPath string
	PKIDir              string
	KubeconfigDir       string
	// A cluster upgrade started through MCP runs only a binary staged here.
	UpgradeStageDir     string
	DefaultRoot         string
	StageDir            string
	BootstrapKubeconfig string
	K8sConfDir          string
	CoreDNSUpstreamDir  string
	AdminConf           string
	BinDir              string
	// Outside the three roots: BinDir holds payload binaries, never stratum.
	StratumInstallPath = "/usr/local/bin/stratum"
	LogDir             string
	ControlSocket      string
	EtcdDataDir        string
	ManifestsDir       string
	HelmCacheDir       string
	LegacyHelmCacheDir = "/var/lib/stratum-helm"

	// Stratum-owned: writing the host /etc/containerd path clobbered distro configs.
	ContainerdConfig string
	// Stratum-owned: a distro containerd (<=v2.2) importing /etc/containerd/conf.d would refuse it.
	ContainerdRuntimeConfD string
	// The single source for what ensureConfig writes and isStratumConfig matches on.
	ContainerdRootDir  string
	ContainerdStateDir string

	// The audit paths sit outside the roots: /var/log is host territory, not stratum data.
	AuditPolicyPath     string
	AuditLogDir         = "/var/log/stratum"
	AuditLogPath        = "/var/log/stratum/audit.log"
	AdmissionConfigPath string
	// Holds the at-rest key; under PKIDir it rides the stratum-certs channel to every HA control plane.
	EncryptionConfigPath string

	RegistriesConfig string
	CertsDir         = "/etc/containerd/certs.d"
	ContainerdConfD  = "/etc/containerd/conf.d/stratum-registry.toml"

	// Imported beside the payload at every start, so nodes without registry access get them.
	ImagesDropInDir string

	// Under PKIDir so it rides the stratum-certs channel to every HA control plane.
	NetConfigPath string

	// Under PKIDir so it rides the stratum-certs channel to a joining control plane.
	CiliumSettingsPath string

	// Host state outside the roots: SetRoots never moves them; uninstall removes each.
	ModulesLoadFile = "/etc/modules-load.d/k8s.conf"
	SysctlConf      = "/etc/sysctl.d/99-stratum.conf"
	SysctlCISConf   = "/etc/sysctl.d/99-stratum-cis.conf"

	// Outside the three roots: host state, not stratum data.
	SystemdUnitPath = "/etc/systemd/system/stratum.service"
)

// SetRoots reassigns the three roots (data, conf, run) and recomputes every derived path.
func SetRoots(dataDir, confDir, runDir string) {
	DataDir, ConfDir, RunDir = dataDir, confDir, runDir

	StartLockPath = RunDir + "-start.lock"
	NodeUpgradeLockPath = RunDir + "-upgrade.lock"

	PKIDir = filepath.Join(DataDir, "pki")
	KubeconfigDir = filepath.Join(DataDir, "kubeconfig")
	UpgradeStageDir = filepath.Join(DataDir, "upgrade")
	DefaultRoot = DataDir
	StageDir = filepath.Join(UpgradeStageDir, "staged")
	BootstrapKubeconfig = filepath.Join(KubeconfigDir, "bootstrap.conf")
	K8sConfDir = filepath.Join(DataDir, "config")
	CoreDNSUpstreamDir = filepath.Join(DataDir, "dns")
	AdminConf = filepath.Join(KubeconfigDir, "admin.conf")
	BinDir = filepath.Join(DataDir, "bin")
	LogDir = filepath.Join(DataDir, "log")
	ControlSocket = filepath.Join(RunDir, "stratum.sock")
	EtcdDataDir = filepath.Join(DataDir, "etcd")
	ManifestsDir = filepath.Join(DataDir, "manifests")
	HelmCacheDir = filepath.Join(DataDir, "helm")
	ContainerdConfig = filepath.Join(K8sConfDir, "containerd.toml")
	ContainerdRuntimeConfD = filepath.Join(K8sConfDir, "containerd.d", "stratum-runtime.toml")
	ContainerdRootDir = filepath.Join(DataDir, "containerd")
	ContainerdStateDir = filepath.Join(RunDir, "containerd")
	AuditPolicyPath = filepath.Join(K8sConfDir, "audit-policy.yaml")
	AdmissionConfigPath = filepath.Join(K8sConfDir, "admission-config.yaml")
	EncryptionConfigPath = filepath.Join(PKIDir, "encryption-config.yaml")
	RegistriesConfig = filepath.Join(ConfDir, "registries.yaml")
	ImagesDropInDir = filepath.Join(ConfDir, "images.d")
	NetConfigPath = filepath.Join(PKIDir, "netconfig")
	CiliumSettingsPath = filepath.Join(PKIDir, "cilium.json")
	ControllerManagerManifest = filepath.Join(K8sConfDir, "manifests", "kube-controller-manager.yaml")

	NodeFileOwners = nodeFileOwners()
}

func init() { SetRoots(DataDir, ConfDir, RunDir) }

// Each node file's owner writes it through internal/atomicfile; an owner absent here is unguarded.
var NodeFileOwners map[string]string

func nodeFileOwners() map[string]string {
	return map[string]string{
		K8sConfDir + "/node.yaml": "internal/nodestate",

		K8sConfDir + "/renewal.yaml":        "internal/certs",
		K8sConfDir + "/rotation-incomplete": "internal/certs",
		PKIDir + "/carotate-run-id":         "internal/certs",

		K8sConfDir + "/snapshot-evidence.yaml": "internal/etcdsnap",

		K8sConfDir + "/backup-evidence.yaml": "internal/cluster",

		KubeconfigDir:       "internal/kubeconfig",
		AdminConf:           "internal/kubeconfig",
		BootstrapKubeconfig: "internal/bootstrap",

		NetConfigPath: "internal/netcfg",

		K8sConfDir + "/manifests":                 "internal/staticpod",
		K8sConfDir + "/admission-webhook.yaml":    "internal/staticpod",
		K8sConfDir + "/webhook-client.kubeconfig": "internal/staticpod",

		AuditPolicyPath:      "internal/profilefiles",
		AdmissionConfigPath:  "internal/profilefiles",
		EncryptionConfigPath: "internal/cluster",

		K8sConfDir + "/kubelet.conf":           "internal/runtime",
		ContainerdConfig:                       "internal/runtime",
		ContainerdRuntimeConfD:                 "internal/runtime",
		K8sConfDir + "/selinux-confined-since": "internal/runtime",
		CoreDNSUpstreamDir + "/resolv.conf":    "internal/runtime",

		PKIDir + "/ca.crt":                    "internal/pki",
		PKIDir + "/ca.key":                    "internal/pki",
		PKIDir + "/front-proxy-ca.crt":        "internal/pki",
		PKIDir + "/front-proxy-ca.key":        "internal/pki",
		PKIDir + "/sa.key":                    "internal/pki",
		PKIDir + "/sa.pub":                    "internal/pki",
		PKIDir + "/ca-bundle.crt":             "internal/pki",
		PKIDir + "/front-proxy-ca-bundle.crt": "internal/pki",
		PKIDir + "/sa-bundle.pub":             "internal/pki",

		UpgradeStageDir: "internal/upgrade",

		ModulesLoadFile: "internal/host",
		SysctlConf:      "internal/host",
		SysctlCISConf:   "internal/host",

		CiliumSettingsPath:         "internal/cluster",
		K8sConfDir + "/components": "internal/cluster",
	}
}

// One home: two operator-facing remediation strings quote this exact path.
var ControllerManagerManifest string

// ContainerdSock is the CRI endpoint used in the kubelet config.
const ContainerdSock = "unix:///run/stratum/containerd/containerd.sock"

// Writers append their source after it; prune matches the writer's full first line.
const GeneratedMarker = "# generated by stratum — do not edit"

// Under K8sConfDir, which the apiserver pod mounts.
func ProfileConfigFile(name string) string { return filepath.Join(K8sConfDir, name) }

// Under PKIDir, 0600.
func ProfileSecretFile(name string) string { return filepath.Join(PKIDir, name) }

// Under AuditLogDir, which the apiserver pod mounts writable.
func ProfileLogFile(name string) string { return filepath.Join(AuditLogDir, name) }

// Resolves a profile's config, secret and log files; PKI files resolve through pki.
func ProfileLocations(pki func(string) string) profile.Locations {
	return profile.Locations{Config: ProfileConfigFile, Secret: pki, Log: ProfileLogFile, PKI: pki}
}

// Follows K8sConfDir at each call, so the render and its argv agree on the location.
func AdmissionWebhookConfig() string {
	return filepath.Join(K8sConfDir, "admission-webhook.yaml")
}

// The system:masters kubeconfig for Stratum's own clients; AdminConf is RBAC-revocable.
func SuperAdminConf() string {
	return filepath.Join(KubeconfigDir, "super-admin.conf")
}

// Alone in its own directory so the kube-vip pod mounts it without the others.
func KubeVipKubeconfig() string {
	return filepath.Join(KubeconfigDir, "kube-vip", "kube-vip.kubeconfig")
}

// Alone in its own directory so the scheduler pod mounts it without the others.
func SchedulerKubeconfig() string {
	return filepath.Join(KubeconfigDir, "scheduler", "scheduler.kubeconfig")
}

// The apiserver reads it on every webhook call; the upgrade's backup list includes it.
func WebhookClientKubeconfig() string {
	return filepath.Join(K8sConfDir, "webhook-client.kubeconfig")
}
