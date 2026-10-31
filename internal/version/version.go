// Package version carries the ldflags-stamped release versions, the ports and
// fixed component settings every node shares, and the filesystem layout every
// other package derives paths from. Paths are rooted at the /var/lib/stratum
// layout (one state directory to back up, snapshot, and uninstall). The
// cluster network's defaults are not here: a cluster may override them, so
// internal/netcfg owns them and resolves each cluster's actual values.
package version

import (
	"path/filepath"

	"github.com/ctrliq/stratum/internal/profile"
)

// Stamped at link time from build/versions.mk by the repo Makefile (the pin
// manifest is the only place versions may live — tools/lint-versions.sh).
var (
	StratumVersion = "v0.0.0-dev"
	// BuildID is the source this binary was built from (tools/build-id.sh):
	// the commit, plus a hash of any uncommitted change. Unlike StratumVersion,
	// which every untagged commit before a release shares, it tells two builds
	// apart -- and unlike a content hash it is the same on every architecture.
	BuildID     = "unknown"
	K8sVersion  = "unknown"
	EtcdVersion = "unknown"

	// The five payload component versions `version --components` reports. They
	// are stamped here, not as main-package vars handed to pkg/cli through
	// WithComponentVersions, so the report does not depend on main-package
	// wiring a downstream build does not have; WithComponentVersions remains
	// the (deprecated) per-field override. Unstamped builds report "unknown",
	// like every stamp above.
	ContainerdVersion = "unknown"
	RuncVersion       = "unknown"
	KubeVipVersion    = "unknown"
	CiliumVersion     = "unknown"
	CorednsVersion    = "unknown"

	// NsenterImage is the image for the node Jobs that nsenter into PID 1 to do
	// host-side work (CA rotation, cluster upgrade). It MUST be an image the
	// airgap payload carries — those Jobs run on clusters with no registry, so
	// anything not in build/images.txt is an ImagePullBackOff mid-rotation. It is
	// stamped rather than written here so a Renovate bump to the pin can't leave
	// a stale tag behind in Go. The name is the mirrored one (tools/mirror-images.sh),
	// which is what `make images` registers in the payload. Unstamped builds get
	// an unresolvable tag on purpose, matching addons.CorednsVersion.
	NsenterImage = "ghcr.io/ctrliq/stratum/busybox:unknown"

	// Control-plane static pod images. Same stamping contract as
	// NsenterImage: mirrored names from the payload, tags derived from the pin
	// manifest at link time, unresolvable on purpose in unstamped builds.
	EtcdImage                  = "ghcr.io/ctrliq/stratum/etcd:unknown"
	KubeAPIServerImage         = "ghcr.io/ctrliq/stratum/kube-apiserver:unknown"
	KubeControllerManagerImage = "ghcr.io/ctrliq/stratum/kube-controller-manager:unknown"
	KubeSchedulerImage         = "ghcr.io/ctrliq/stratum/kube-scheduler:unknown"
	KubeVipImage               = "ghcr.io/ctrliq/stratum/kube-vip:unknown"
)

const (
	// Ports.
	APIServerPort  = 6443
	EtcdClientPort = 2379
	EtcdPeerPort   = 2380
	// EtcdMetricsPort is etcd's plain-HTTP localhost /health listener, added
	// for the static pod's liveness probe.
	EtcdMetricsPort = 2381
	KubeletPort     = 10250

	// On-disk layout (shared with the bash installer for interop).
	EtcdName    = "stratum"
	ClusterName = "stratum"

	// EtcdClusterToken is the etcd --initial-cluster-token used at bootstrap and
	// member-join, so both read it from one place. NOTE: snapshot restore does the
	// opposite on purpose — it derives a UNIQUE per-restore token (see
	// etcdsnap.Restore) so the restored cluster gets a distinct cluster-id and any
	// still-running old peer can't crash the fresh seed's raft.
	EtcdClusterToken = "stratum-etcd"

	// ControllerManagerControllersFlag is the kube-controller-manager --controllers
	// value.
	//
	// bootstrapsigner publishes the detached JWS over the cluster-info ConfigMap
	// that a joining node verifies. It is DISABLED by default, so without
	// this flag no signature is ever produced and every join that requires one
	// fails. "*" is "all on-by-default controllers", making this a strict superset
	// of the default set rather than a replacement for it.
	//
	// No regex or shell check of hand-edited unit files is needed: processes.go
	// renders argv on every start, so the flag arrives with the next restart
	// and there is nothing for an operator to edit or verify.
	ControllerManagerControllersFlag = "--controllers=*,bootstrapsigner,tokencleaner"

	// CertRenewWindowDays is how many days before a leaf cert's expiry the
	// auto-renew timer rotates it (the `--if-expiring` window). Leaf certs are
	// ~1yr, so this fires roughly once a year per node.
	CertRenewWindowDays = 45

	// SnapshotInterval is how often the controller's built-in snapshot loop
	// saves (the supervisor owns it, not a systemd timer);
	// SnapshotRetainDefault is how many of THIS node's snapshots to keep
	// (local + S3).
	SnapshotIntervalHours = 12
	SnapshotRetainDefault = 5

	// TLSCipherSuites is the CIS-approved strong TLS 1.2+ suite set, shared by the
	// apiserver (--tls-cipher-suites, comma list) and the kubelet (tlsCipherSuites,
	// YAML flow sequence — the bare names are valid plain scalars). Applied as a
	// hardened default (no downside).
	TLSCipherSuites = "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256," +
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384," +
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305"
)

// Filesystem paths (the on-disk layout this binary and the tooling around
// it — upgrade payloads, the operator — share). ConfDir, DataDir and
// RunDir are the three roots: every var in this block that carries no literal
// is derived from them, and SetRoots recomputes those derivations whenever a
// root moves. These are vars rather than consts only so tests can redirect
// them to a temp dir; production never reassigns them.
var (
	ConfDir             = "/etc/stratum"
	DataDir             = "/var/lib/stratum"
	RunDir              = "/run/stratum"
	StartLockPath       string
	NodeUpgradeLockPath string
	PKIDir              string
	KubeconfigDir       string
	// UpgradeStageDir is where an operator stages upgrade binaries. A cluster
	// upgrade started through MCP runs only a binary staged here.
	UpgradeStageDir string
	// DefaultRoot is where the payload is staged: binaries land in
	// DefaultRoot/bin, images in DefaultRoot/images. It is the data root
	// itself; internal/assets.DefaultRoot is the same path under its earlier
	// name, kept for its existing callers.
	DefaultRoot string
	// StageDir is where a node upgrade stages the incoming payload before the
	// swap, under the upgrade staging dir. It is the one home of that path:
	// internal/upgrade resolves it at each extract, and a test redirects it
	// through SetRoots like any other derived path.
	StageDir            string
	BootstrapKubeconfig string
	K8sConfDir          string
	// CoreDNSUpstreamDir holds the resolv.conf CoreDNS forwards through,
	// mounted into its pods.
	CoreDNSUpstreamDir string
	AdminConf          string
	BinDir             string
	// StratumInstallPath is where the stratum binary itself is installed: the
	// documented install location, where an upgrade installs it, and the one
	// path every node is known to have. BinDir holds the payload's binaries
	// (kubelet, containerd, ...), never stratum, so a node Job must not exec
	// stratum from there. It sits outside the three roots — it is not
	// stratum's data — so uninstall removes it as its own special target and
	// SetRoots never moves it.
	StratumInstallPath = "/usr/local/bin/stratum"
	LogDir             string
	ControlSocket      string
	EtcdDataDir        string
	// ManifestsDir holds cluster manifests the stratum operator watches (HelmChart
	// CRs etc.); HelmCacheDir is the operator's Helm repo/chart cache.
	ManifestsDir string
	HelmCacheDir string
	// LegacyHelmCacheDir is where releases before the data-dir layout kept the
	// operator's helm cache: the administrator-owned repositories.yaml (named
	// repositories, credentials, TLS file paths) and the cached repo indexes a
	// named-repository fetch consults. Nothing writes it anymore: the operator
	// migrates it into HelmCacheDir on first use, and uninstall removes the
	// leftover on a node brought up by such a release.
	LegacyHelmCacheDir = "/var/lib/stratum-helm"

	// ContainerdConfig is stratum's own containerd config. It lives under the
	// stratum data dir (K8sConfDir) rather than the host's
	// /etc/containerd/config.toml: writing the host path clobbered a distro
	// containerd's config with a version=4 file its older (<=v2.2) containerd
	// could not parse, crash-looping it — and Docker with it — after a reboot,
	// and uninstall never restored it. A stratum-owned path leaves
	// /etc/containerd alone and is removed automatically with DataDir.
	ContainerdConfig string
	// ContainerdRuntimeConfD declares the node's extra containerd runtime
	// handlers (spec.node.runtimeHandlers). It is a config version 4 file, so
	// it lives in a stratum-owned directory that only stratum's config
	// imports, after /etc/containerd/conf.d: a distro containerd (<=v2.2)
	// that also imports the shared directory would refuse it, and the later
	// import wins over a handler nvidia-ctk declared there.
	ContainerdRuntimeConfD string
	// ContainerdRootDir / ContainerdStateDir are where stratum points containerd's
	// root and state (away from the distro defaults, onto the stratum layout so a
	// distro containerd on the same host does not collide). These are the SINGLE
	// source for the two values that both ensureConfig writes and isStratumConfig
	// matches on: the legacy-config heal deletes /etc/containerd/config.toml only when a
	// file carries both, so producer and matcher must never drift apart.
	ContainerdRootDir  string
	ContainerdStateDir string

	// A hardened node profile's config files (written by internal/profilefiles,
	// the shared secret by internal/cluster).
	// EncryptionConfig is 0600 (holds the at-rest key) and is shared across CPs
	// via the stratum-certs channel. The audit log paths are the CIS-adjacent
	// paths OUTSIDE the roots — /var/log/stratum is host log territory — so
	// uninstall removes them as its own special targets and SetRoots never
	// moves them.
	AuditPolicyPath     string
	AuditLogDir         = "/var/log/stratum"
	AuditLogPath        = "/var/log/stratum/audit.log"
	AdmissionConfigPath string
	// The admission file every apiserver reads — which the render derives
	// from AdmissionConfigPath on every write — has no var here on purpose,
	// unlike its authoring config above. Its location is K8sConfDir's, so it
	// resolves at each call through AdmissionWebhookConfig below, exactly as
	// the render resolves the file it writes; an init-frozen copy beside the
	// CIS paths would name a file the render never wrote whenever the
	// directory differs — and it is used (the upgrade's profile config
	// backup, upgrade.profileConfigPaths, names this location),
	// so one resolution must serve writer, argv and rollback set. The config
	// directory redirects here already carry the derived files with it.
	// EncryptionConfigPath lives UNDER PKIDir (it holds the at-rest key) so it
	// rides the existing PKIDir-relative shared-cert channel to every HA CP.
	EncryptionConfigPath string

	// Registry mirroring (internal/registry): a per-node RKE2-compatible
	// registries.yaml is translated into containerd's certs.d. ContainerdConfD
	// sets config_path via containerd's default conf.d import; CertsDir holds the
	// per-registry hosts.toml (some 0600 when they embed a credential header).
	RegistriesConfig string
	CertsDir         = "/etc/containerd/certs.d"
	ContainerdConfD  = "/etc/containerd/conf.d/stratum-registry.toml"

	// NetConfigPath persists the cluster's effective pod/service CIDRs (chosen at
	// cluster-init) so HA joins and cert rotation inherit the exact same values
	// (incl. IPv6 families under dual-stack). Lives UNDER PKIDir so it rides the
	// existing PKIDir-relative stratum-certs channel to every HA CP (as
	// encryption-config.yaml does); it is not a secret.
	NetConfigPath string

	// CiliumSettingsPath holds the cluster's spec.network.cilium settings when
	// they differ from the shipped ones. Like NetConfigPath it lives under
	// PKIDir so it rides the stratum-certs channel to a joining control plane,
	// which refuses to join with different settings; it is not a secret.
	CiliumSettingsPath string

	// The host files bring-up writes outside the three roots:
	// ModulesLoadFile persists the kernel module list (overlay, br_netfilter)
	// across reboots, SysctlConf is the pod-networking and inotify sysctl
	// file written on every node, and SysctlCISConf is the CIS node-sysctl
	// file written under --cis. Like SystemdUnitPath they are host state, not
	// stratum's data: they sit outside the three roots, SetRoots never moves
	// them, and uninstall removes them as its own special targets. This is
	// the one home — internal/host writes and loads them through these
	// variables, host.Footprint returns them, and the owning-package record
	// keys internal/host's entries through them — so the writer, the sysctl
	// loader and uninstall's delete list cannot drift apart. They live here,
	// not in internal/host, because the owning-package record lives here and
	// internal/host imports this package, never the reverse.
	ModulesLoadFile = "/etc/modules-load.d/k8s.conf"
	SysctlConf      = "/etc/sysctl.d/99-stratum.conf"
	SysctlCISConf   = "/etc/sysctl.d/99-stratum-cis.conf"

	// SystemdUnitPath is the reboot-persistence unit `stratum up` installs on
	// every bring-up and `stratum uninstall` removes: the layer the
	// in-process supervisor never covered, because nothing else restarted the
	// supervisor's OWN process after a reboot. Like the other literals above
	// it sits outside the three roots and never moves with them.
	SystemdUnitPath = "/etc/systemd/system/stratum.service"
)

// SetRoots reassigns the three roots — DataDir, ConfDir, RunDir, in that
// argument order — and recomputes every path derived from them, the init-time
// derivations (StartLockPath, NodeUpgradeLockPath, ControllerManagerManifest)
// included: those are the ones a direct root-var reassignment leaves stale.
// Production never calls it; it exists so a test can point the whole layout at
// a temp dir with one call and put it back with t.Cleanup. A test that
// redirects a single root var by hand keeps working too — the readers read the
// vars at call time — it just leaves the derived paths where they were.
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
	NetConfigPath = filepath.Join(PKIDir, "netconfig")
	CiliumSettingsPath = filepath.Join(PKIDir, "cilium.json")
	ControllerManagerManifest = filepath.Join(K8sConfDir, "manifests", "kube-controller-manager.yaml")

	// The owning-package record is keyed by the derived paths, so it is
	// rebuilt with them; every key derives from the roots and follows them.
	NodeFileOwners = nodeFileOwners()
}

// init builds the shipped layout through the same derivations SetRoots uses on
// a reassignment, so the compiled-in paths and a redirected layout cannot
// disagree.
func init() { SetRoots(DataDir, ConfDir, RunDir) }

// NodeFileOwners is the owning-package record: beside each node file's path,
// the one package that owns its writes. It is the rule the node-file write
// guard (tools/nodefilewrites, run by make lint) enforces: a node file is
// written by its owning package through the shared atomic writer
// (internal/atomicfile), never by a raw os.WriteFile or clientcmd.WriteToFile,
// so a torn write can never leave a half-written file where a working one was.
// The guard scans every package named here as an owner, and a value names the
// package answerable for the file's behavior even where another package hands
// it the bytes — internal/pki owns the trust material even though a join's
// worker path (pkg/cli) and an HA control plane (internal/ha) write the shared
// CA files through the same atomic writer during a join. Deriving the guard's
// package set from these values, not from a second list in the tool, is what
// keeps the rule and its guard from drifting apart. A node file that no entry
// names has no owner and no guard: new node files belong here.
//
// A key names a file, or the directory whose members the package writes
// (every kubeconfig under KubeconfigDir, every rendered manifest under the
// manifest directory).
var NodeFileOwners map[string]string

// nodeFileOwners rebuilds the owning-package record from the path vars as they
// stand at the call, so SetRoots carries the record's keys with the roots it
// reassigns instead of leaving it naming files under the old layout.
func nodeFileOwners() map[string]string {
	return map[string]string{
		// node.yaml, the node's own state record.
		K8sConfDir + "/node.yaml": "internal/nodestate",

		// The kubelet leaf-renewal record, the incomplete leaf-rotation
		// marker and the CA-rotation run id.
		K8sConfDir + "/renewal.yaml":        "internal/certs",
		K8sConfDir + "/rotation-incomplete": "internal/certs",
		PKIDir + "/carotate-run-id":         "internal/certs",

		// The component kubeconfigs (admin, kubelet, controller-manager, ...),
		// the kubectl admin.conf, and the bootstrap kubeconfig — which
		// internal/bootstrap writes (kubelet's TLS bootstrap), not the
		// kubeconfig package, whose record entry covers only the files it
		// renders.
		KubeconfigDir:       "internal/kubeconfig",
		AdminConf:           "internal/kubeconfig",
		BootstrapKubeconfig: "internal/bootstrap",

		// The persisted network config the first start records and every HA
		// join and renewal inherits (internal/netcfg's Save; the file is
		// authoritative once it exists).
		NetConfigPath: "internal/netcfg",

		// The rendered static pod manifests, plus the two files the apiserver
		// render derives: the admission configuration its argv names and the
		// webhook client kubeconfig that configuration names.
		K8sConfDir + "/manifests":                 "internal/staticpod",
		K8sConfDir + "/admission-webhook.yaml":    "internal/staticpod",
		K8sConfDir + "/webhook-client.kubeconfig": "internal/staticpod",

		// A node profile's files: the config files it renders (the audit
		// policy and the admission authoring config of a hardened profile),
		// and the shared secret the first control plane mints (the at-rest
		// encryption config, 0600; it holds the key).
		AuditPolicyPath:      "internal/profilefiles",
		AdmissionConfigPath:  "internal/profilefiles",
		EncryptionConfigPath: "internal/cluster",

		// The runtime configs the kubelet and containerd read at start — a
		// torn kubelet.conf stops the kubelet, a torn containerd.toml stops
		// every pod sandbox — plus the record of when containerd began
		// confining containers and the resolv.conf CoreDNS forwards through.
		K8sConfDir + "/kubelet.conf":           "internal/runtime",
		ContainerdConfig:                       "internal/runtime",
		K8sConfDir + "/selinux-confined-since": "internal/runtime",
		CoreDNSUpstreamDir + "/resolv.conf":    "internal/runtime",

		// The PKI material: the roots, the front-proxy CA, the service-account
		// key, and the trust bundles that carry them to joins and renewals.
		PKIDir + "/ca.crt":                    "internal/pki",
		PKIDir + "/ca.key":                    "internal/pki",
		PKIDir + "/front-proxy-ca.crt":        "internal/pki",
		PKIDir + "/front-proxy-ca.key":        "internal/pki",
		PKIDir + "/sa.key":                    "internal/pki",
		PKIDir + "/sa.pub":                    "internal/pki",
		PKIDir + "/ca-bundle.crt":             "internal/pki",
		PKIDir + "/front-proxy-ca-bundle.crt": "internal/pki",
		PKIDir + "/sa-bundle.pub":             "internal/pki",

		// The upgrade stage: the operator-staged binaries and the backup sets an
		// upgrade takes before it touches a live file.
		UpgradeStageDir: "internal/upgrade",

		// The host files bring-up writes outside the roots, keyed through the
		// path vars beside SystemdUnitPath: a torn modules-load file leaves
		// pod networking broken after a reboot, and a torn sysctl file breaks
		// the kubelet's protectKernelDefaults boot gate under --cis.
		ModulesLoadFile: "internal/host",
		SysctlConf:      "internal/host",
		SysctlCISConf:   "internal/host",

		// The Cilium settings record rides the stratum-certs channel to a
		// joining control plane, which refuses to join against settings it
		// never ran — a torn one is a refused join, not a recovered one — and
		// the component .args files under the config directory are the
		// on-disk truth of each component's rendered argv. Both are
		// written by internal/cluster.
		CiliumSettingsPath:         "internal/cluster",
		K8sConfDir + "/components": "internal/cluster",
	}
}

// ControllerManagerManifest is kube-controller-manager's rendered static pod
// manifest (internal/staticpod) — the on-disk truth for its argv. It has ONE
// home because two operator-facing remediation strings quote it —
// internal/bootstrap (the joiner side) and internal/token (the control-plane
// side) — and a path that drifted between them would send an operator to a
// file that does not exist. It is derived from K8sConfDir and recomputed by
// SetRoots.
var ControllerManagerManifest string

// ContainerdSock is the CRI endpoint used in the kubelet config.
const ContainerdSock = "unix:///run/stratum/containerd/containerd.sock"

// GeneratedMarker opens every file stratum writes into a directory it shares
// with the host (containerd's conf.d and certs.d), so prune removes only
// stratum's own files. Each writer appends its source; prune matches the
// writer's full first line.
const GeneratedMarker = "# generated by stratum — do not edit"

// ProfileConfigFile is where the engine writes a node profile's config file
// of that name: under K8sConfDir, which the apiserver pod mounts. The cis
// profile's audit policy and admission config resolve here to
// AuditPolicyPath and AdmissionConfigPath.
func ProfileConfigFile(name string) string { return filepath.Join(K8sConfDir, name) }

// ProfileSecretFile is where a node profile's shared secret of that name
// lives: under PKIDir, 0600. The cis profile's encryption config resolves
// here to EncryptionConfigPath.
func ProfileSecretFile(name string) string { return filepath.Join(PKIDir, name) }

// ProfileLogFile is where a node profile's log file of that name lives: under
// AuditLogDir, which the apiserver pod mounts writable. The cis profile's
// audit log resolves here to AuditLogPath.
func ProfileLogFile(name string) string { return filepath.Join(AuditLogDir, name) }

// ProfileLocations resolves the files a node profile names, for a render
// whose PKI files resolve through pki: config files under ProfileConfigFile,
// shared secrets beside the render's other PKI files, log files under
// ProfileLogFile.
func ProfileLocations(pki func(string) string) profile.Locations {
	return profile.Locations{Config: ProfileConfigFile, Secret: pki, Log: ProfileLogFile, PKI: pki}
}

// AdmissionWebhookConfig is the admission file's location under the config
// directory as it stands at each call. The apiserver render writes the file
// and the argv it renders names it (--admission-control-config-file,
// internal/staticpod), so both resolve it through the directory they render
// for and follow it wherever it points; a location fixed at init would name
// a file the render never wrote whenever the directory differs. The upgrade's
// backup list (cisConfigPaths in internal/upgrade) names this entry of the
// profile's file list through the same call, so the rollback set and the
// render target cannot hold two answers to where the file is.
func AdmissionWebhookConfig() string {
	return filepath.Join(K8sConfDir, "admission-webhook.yaml")
}

// SuperAdminConf is the system:masters kubeconfig Stratum's own clients use;
// AdminConf is the people's, revocable through RBAC.
func SuperAdminConf() string {
	return filepath.Join(KubeconfigDir, "super-admin.conf")
}

// KubeVipKubeconfig is kube-vip's kubeconfig, alone in its own directory so
// kube-vip's pod mounts it without the other kubeconfigs beside it.
func KubeVipKubeconfig() string {
	return filepath.Join(KubeconfigDir, "kube-vip", "kube-vip.kubeconfig")
}

// SchedulerKubeconfig is kube-scheduler's kubeconfig, alone in its own
// directory so the scheduler's pod mounts it without the other kubeconfigs
// beside it.
func SchedulerKubeconfig() string {
	return filepath.Join(KubeconfigDir, "scheduler", "scheduler.kubeconfig")
}

// WebhookClientKubeconfig is the kubeconfig the admission file's
// MutatingAdmissionWebhook plugin names and the render writes beside it:
// the apiserver reads it on every webhook call, so the upgrade's profile
// config backup (upgrade.profileConfigPaths) backs it up with the admission
// file. Like the
// admission file, it follows the config directory at each call.
func WebhookClientKubeconfig() string {
	return filepath.Join(K8sConfDir, "webhook-client.kubeconfig")
}
