package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/backupevidence"
	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

func newUpCommand(cfg rootConfig) *cobra.Command {
	var o runUpOptions
	cmd := &cobra.Command{
		Use:         "up",
		Short:       "Run this node (control plane + workloads, or --worker for workload-only)",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{ownsNodeStateAnnotation: "1"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("config") && o.configPath == "" {
				return fmt.Errorf("stratum up: --config is empty; name the cluster config file, or leave --config out to read %s. "+
					"If the stratum unit runs this with an empty --config, rewrite it: %s. Nothing on this host was changed",
					cluster.DefaultConfigPath, unitConfigHint)
			}
			if err := refuseUnstampedBuild("stratum up"); err != nil {
				return err
			}
			if err := registerProfiles(cfg); err != nil {
				return err
			}
			cluster.ImporterStacks = importerStacks(cfg.stacks)
			return runUp(cmd.Context(), o)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&o.configPath, "config", cluster.DefaultConfigPath, "cluster config file (only the default path may be missing, meaning defaults)")
	fs.BoolVar(&o.worker, "worker", false, "run a workload-only node (no control plane)")
	fs.StringVar(&o.profile, "profile", "", "node profile to run, by name (the binary must register it; a worker gets it from its join line, and a worker restart refuses a different one)")
	fs.BoolVar(&o.cis, "cis", false, "the cis profile; the spelling every release reads, which the restart unit keeps (prefer --profile cis)")
	_ = fs.MarkHidden("cis")
	fs.StringVar(&o.join, "join", "", "HA join: existing control-plane URL (https://host:6443)")
	fs.StringVar(&o.token, "token", "", "join: bootstrap token (prefer --token-file or $"+tokenEnvVar+" to keep it out of ps/shell history)")
	fs.StringVar(&o.tokenFile, "token-file", "", "join: read the bootstrap token from this file instead of passing it on the command line")
	fs.StringVar(&o.caHash, "discovery-token-ca-cert-hash", "", "join: sha256:<hex> CA pin")
	fs.StringVar(&o.certKey, "certificate-key", "", "HA join: shared-cert decryption key (prefer --certificate-key-file to keep it out of ps/shell history)")
	fs.StringVar(&o.certKeyFile, "certificate-key-file", "", "HA join: read the shared-cert decryption key from this file (0600) instead of passing it on the command line")
	fs.StringVar(&o.server, "server", "", "--worker join: control-plane or VIP URL (https://host:6443)")
	fs.StringVar(&o.nodeIP, "node-ip", "", "this node's IP (\"v4\" or \"v4,v6\"); on a control plane it outranks spec.network.nodeIP and $NODE_IP, on a --worker join it outranks spec.network.nodeIP (which, with spec.selinux and spec.node, is all a worker reads from --config) and $NODE_IP, and on a --worker restart it outranks the saved address, moving the worker to it and saving it (an IPv4 address on a single-stack worker, the v4,v6 pair on a dual-stack one)")
	fs.BoolVar(&o.confirmEtcdQuorumLost, "confirm-etcd-quorum-lost", false, "control plane: start a node whose saved state records an etcd cluster but whose etcd data is gone; only after confirming no other control plane holds quorum (snapshot restore)")
	fs.BoolVar(&o.dual, "dual-stack", false, "--worker join: register a v4,v6 node IP pair")
	fs.StringVar(&o.serviceCIDR, "service-cidr", "", "--worker join: the cluster's service CIDR, when not the default (stratum token create prints it)")
	fs.StringVar(&o.clusterDomain, "cluster-domain", "", "--worker join: the cluster's DNS domain, when not cluster.local (stratum token create prints it)")
	fs.StringVar(&o.podCIDR, "pod-cidr", "", "--worker join or legacy-worker migration: the cluster's pod CIDR (stratum token create prints a non-default range)")
	fs.StringArrayVar(&o.nodeLabels, "node-label", nil, "--worker: a label for this node's Node object, key=value (repeatable); on a join it adds to spec.node.labels, on a restart the labels given replace the saved ones")
	fs.StringArrayVar(&o.nodeTaints, "node-taint", nil, "--worker: a taint this node registers with, key[=value]:Effect (repeatable); on a join it adds to spec.node.taints; a node registers its taints only at its join, so a restart refuses a different set")
	fs.StringArrayVar(&o.runtimeHandlers, "runtime-handler", nil, "--worker: an extra containerd runtime handler, name=/absolute/path/to/runtime (repeatable); on a join it adds to spec.node.runtimeHandlers, on a restart the handlers given replace the saved ones")
	return cmd
}

type runUpOptions struct {
	configPath            string
	worker                bool
	profile               string
	cis                   bool
	join                  string
	token                 string
	tokenFile             string
	caHash                string
	certKey               string
	certKeyFile           string
	server                string
	nodeIP                string
	serviceCIDR           string
	podCIDR               string
	clusterDomain         string
	dual                  bool
	nodeLabels            []string
	nodeTaints            []string
	runtimeHandlers       []string
	confirmEtcdQuorumLost bool
}

// resolveCertKey picks the key from the flag or the file; empty is not an error here.
func resolveCertKey(flag, file string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if file == "" {
		return "", nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read --certificate-key-file %s: %w", file, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Named once so the flag help, the resolver and `token create`'s guidance cannot drift.
const tokenEnvVar = "STRATUM_TOKEN"

// resolveToken: --token, then --token-file, then the environment; empty is not an error.
func resolveToken(flag, file string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --token-file %s: %w", file, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv(tokenEnvVar)), nil
}

// CIS/STIG umasks (027/077) lock out non-root containers; secrets carry explicit 0600/0700 modes.
const nodeUmask = 0o022

// Not applied root-wide: restore writes 0666 files and relies on the caller's umask.
const ownsNodeStateAnnotation = "stratum/owns-node-state"

func pinNodeUmask(cmd *cobra.Command, _ []string) {
	if cmd.Annotations[ownsNodeStateAnnotation] != "" {
		syscall.Umask(nodeUmask)
	}
}

// Only the run's own signal-asked stop exits clean; a host's cancellation keeps its exit code.
func cleanStopOr(err error, ctx context.Context) error {
	if interrupt.ExpectedStop(ctx, err) {
		fmt.Println("stratum: stopped (signal received)")
		return &cleanStopExit{stop: err}
	}
	return err
}

func runUp(parent context.Context, o runUpOptions) error {
	if err := upFlagConflict(o); err != nil {
		return err
	}
	node, err := o.nodeSpec()
	if err != nil {
		return err
	}
	configPath, err := upConfigPath(o.configPath, o.worker)
	if err != nil {
		return err
	}
	o.configPath = configPath
	joinCfg, err := preflightWorkerJoinNode(o, node)
	if err != nil {
		return err
	}
	// The run owns its signals from here, before the first preflight call.
	ctx, stop := interrupt.DerivedContext(parent)
	defer stop()
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}
	// Starting over a running node or its leftovers would crash-loop kubelet with no useful diagnostic.
	if err := preflightNoRunningNode(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}
	// The preflight is read-only, so two starts can race; the exclusive lock refuses the second.
	releaseStartLock, err := etcddata.AcquireStartLockChecked(startLockPath, preflightNoRunningNode)
	if err != nil {
		return err
	}
	defer releaseStartLock()
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}
	if err := refuseWorkerRestartProfileChange(o); err != nil {
		return err
	}
	if err := preStageProfile(o); err != nil {
		return err
	}
	// Refused before staging so a bad --node-ip changes nothing on the host.
	if o.worker && o.server != "" {
		if _, err := resolveJoinNodeIP(o.nodeIP, joinCfg.NodeIP, o.dual); err != nil {
			return fmt.Errorf("stratum up: %w. Nothing on this host was changed", err)
		}
	}
	// Uninstall takes the same data lock; keep staging outside its deletion window.
	releaseStageLock, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		releaseStageLock()
		return cleanStopOr(err, ctx)
	}
	if stageOptOut(o) {
		if err := forgetSELinuxLabels(); err != nil {
			releaseStageLock()
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		releaseStageLock()
		return cleanStopOr(err, ctx)
	}
	_, stageErr := stagePayload(version.DataDir)
	releaseStageLock()
	if stageErr != nil && !errors.Is(stageErr, assets.ErrNoPayload) {
		return fmt.Errorf("staging payload: %w", stageErr)
	}
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}

	if o.worker {
		// A restart reuses persisted state and needs no token, so this must not block it.
		var token string
		var wcfg cluster.WorkerConfig
		var selinuxOptOut *bool
		if o.server == "" {
			if v, set, err := cluster.WorkerSELinux(o.configPath); err != nil {
				fmt.Printf("  WARN: spec.selinux, spec.profile and spec.cis not read: %v; keeping the SELinux setting and the profile this worker saved\n", err)
			} else if set {
				selinuxOptOut = &v
			}
		}
		if o.server != "" {
			var err error
			if token, err = resolveToken(o.token, o.tokenFile); err != nil {
				return err
			}
			wcfg = joinCfg
			selinuxOptOut = &wcfg.SELinuxOptOut
		}
		profileName, err := workerProfileName(o)
		if err != nil {
			return err
		}
		st, err := prepareWorkerFn(ctx, cluster.WorkerOptions{
			Server: o.server, Token: token, CAHash: o.caHash, NodeIP: o.nodeIP, ConfigNodeIP: wcfg.NodeIP,
			ServiceCIDR: o.serviceCIDR, PodCIDR: o.podCIDR, ClusterDomain: o.clusterDomain,
			Profile: profileName, SELinuxOptOut: selinuxOptOut, DualStack: o.dual,
			Node: node, ConfigNode: wcfg.Node,
		})
		if err != nil {
			return cleanStopOr(err, ctx)
		}
		// A signal during the join stops here, before the persistence unit is installed.
		if err := ctx.Err(); err != nil {
			return cleanStopOr(err, ctx)
		}
		// The unit re-runs the persisted-state fast path, never the one-shot join flags.
		if err := installUnit(upExecArgv(true, "", "")); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: WARNING: could not install the reboot-persistence unit: %v\n", err)
		}
		fmt.Printf("stratum %s: starting worker (server=%s)\n", version.StratumVersion, st.Server)
		if err := runWorkerFn(ctx, st); err != nil &&
			!interrupt.ExpectedStop(ctx, err) {
			return err
		}
		fmt.Println("stratum: stopped")
		return nil
	}

	profileName, err := o.profileName()
	if err != nil {
		return err
	}
	cfg, err := controllerConfig(o.configPath, profileName)
	if err != nil {
		return err
	}
	if prev, loadErr := cluster.LoadState(); loadErr == nil {
		if err := refuseDroppedProfile(prev, cfg.ProfileName()); err != nil {
			return err
		}
	}

	var st *cluster.NodeState
	rejoinAsMember := false
	if o.join != "" {
		// PrepareControlPlaneJoin wipes the etcd data dir; a re-run join would destroy this member.
		prev, loadErr := cluster.LoadState()
		pending, err := ha.JoinIntentSurvives()
		if err != nil {
			return err
		}
		asMember, err := alreadyJoinedWithIntentDecision(prev, loadErr, localCAPins, o.caHash, pending, version.EtcdDataDir)
		if err != nil {
			return err
		}
		rejoinAsMember = asMember
	}
	if o.join != "" && !rejoinAsMember {
		// Join-only: a fresh bring-up never touches these, so a bad --token-file cannot block it.
		token, err := resolveToken(o.token, o.tokenFile)
		if err != nil {
			return err
		}
		var certKey string
		if certKey, err = resolveCertKey(o.certKey, o.certKeyFile); err != nil {
			return err
		}
		st, err = prepareControlPlaneJoinFn(ctx, ha.JoinOptions{
			Server: o.join, Token: token, CAHash: o.caHash, CertKey: certKey,
			NodeIP: o.nodeIP, ConfigNodeIP: cfg.Spec.Network.NodeIP,
			Profile: cfg.ProfileName(), CIS: cfg.Spec.CIS, SELinuxOptOut: cfg.SELinuxOptOut(), VIP: cfg.Spec.VIP,
			Operator: cfg.OperatorEnabled(), CertRenew: cfg.CertRenewEnabled(),
			PodCIDR: cfg.Spec.Network.PodCIDR, ServiceCIDR: cfg.Spec.Network.ServiceCIDR,
			ClusterDomain: cfg.Spec.Network.ClusterDomain,
			Cilium:        cfg.Spec.Network.Cilium.Settings(),
			NodeLabels:    cfg.Spec.Node.Labels, NodeTaints: cfg.Spec.Node.Taints, RuntimeHandlers: cfg.Spec.Node.RuntimeHandlers,
		})
		if err != nil {
			return cleanStopOr(err, ctx)
		}
		// The joined control plane's registration is waited out by the controller's node task.
	} else {
		if rejoinAsMember {
			fmt.Printf("stratum: %s is already a control-plane member of this cluster; "+
				"starting it instead of re-joining (drop --join to silence this)\n", o.join)
		}
		// PrepareController keeps the saved etcd membership; it never resets an HA member to state=new.
		releaseDataLock, err := etcddata.Lock(version.EtcdDataDir)
		if err != nil {
			return err
		}
		prev, loadErr := cluster.LoadState()
		// On an empty dir it would rejoin as a blank member, or bootstrap a separate cluster.
		if loadErr == nil && !o.confirmEtcdQuorumLost &&
			(rejoinAsMember || recordsLargerEtcdCluster(prev)) {
			if err := requireLocalEtcdMember(version.EtcdDataDir); err != nil {
				releaseDataLock()
				return err
			}
			if err := ha.ClearJoinIntent(); err != nil {
				fmt.Fprintf(os.Stderr, "stratum: WARNING: %v\n", err)
			}
		}
		if o.join == "" && errors.Is(loadErr, fs.ErrNotExist) {
			pending, intentErr := ha.JoinIntentSurvives()
			if intentErr != nil {
				releaseDataLock()
				return intentErr
			}
			if pending {
				releaseDataLock()
				return fmt.Errorf("stratum up: the join intent file %s survives from an unfinished HA "+
					"control-plane join, and this node has no saved state: bare `stratum up` would bootstrap a "+
					"separate single-member etcd cluster while the joined cluster may still list this node as its "+
					"member. Re-run `stratum up --join` to resume the join: it reconciles the intent against the "+
					"cluster's own member list and finishes the join without a second member-add. Remove the intent "+
					"file by hand only once the member it records is no longer in the cluster's member list "+
					"(`stratum etcdctl member list`)", ha.JoinIntentPath())
			}
		}
		// A held fence refuses the start; a take no apiserver answers is skipped, not failed.
		releaseKeyFence, ferr := takeCISKeyMintFence(cfg)
		if ferr != nil {
			releaseDataLock()
			return ferr
		}
		st, err = prepareControllerFn(ctx, cfg, o.nodeIP, releaseKeyFence, releaseDataLock)
		if releaseKeyFence != nil {
			releaseKeyFence.Release()
		}
		releaseDataLock()
		if err != nil {
			return cleanStopOr(err, ctx)
		}
	}

	// A signal during the preparation stops here, before the run phase's own writes and the supervisor.
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}

	// Resolved from cfg, not echoed from argv: a node joined with --profile keeps it on every restart.
	if err := installUnit(upExecArgv(false, o.configPath, cfg.ProfileName())); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: WARNING: could not install the reboot-persistence unit: %v\n", err)
	}

	fmt.Printf("stratum %s: starting control-plane node (vip=%q)\n", version.StratumVersion, st.VIP.Address)
	fmt.Printf("stratum: kubeconfig %s\n", version.AdminConf)
	if err := runControllerFn(ctx, cfg, st); err != nil &&
		!interrupt.ExpectedStop(ctx, err) {
		return err
	}
	fmt.Println("stratum: stopped")
	return nil
}

// The mint runs inside PrepareController; fence only if a secret is missing and admin.conf exists.
func takeCISKeyMintFence(cfg *cluster.ClusterConfig) (*k8sutil.MaintenanceLease, error) {
	prof, err := profile.ForNode(cfg.ProfileName())
	if err != nil {
		return nil, err
	}
	if !missingSharedSecret(prof) {
		return nil, nil
	}
	if _, err := os.Stat(version.AdminConf); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	lease, err := cisKeyMintFence()
	if err == nil {
		return lease, nil
	}
	// Only a dial failure means nothing is serving; other errors may hide a live maintenance run.
	var st apierrors.APIStatus
	var op *net.OpError
	if errors.Is(err, maintlease.ErrTaken) || errors.As(err, &st) || !errors.As(err, &op) || op.Op != "dial" {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "stratum: WARNING: --profile cis: the maintenance fence around the at-rest key mint was skipped "+
		"because no apiserver answered through %s (%v); the key gate's etcd check still refuses a control plane "+
		"with other members\n", k8sutil.InternalAdminConf(), err)
	return nil, nil
}

func missingSharedSecret(prof profile.Profile) bool {
	for _, s := range prof.SharedSecrets {
		if _, err := os.Stat(version.ProfileSecretFile(s.Name)); err != nil {
			return true
		}
	}
	return false
}

// Seams for runUp wiring tests: stub the blocking supervisor call and the host-mutating prepare.
var (
	installUnit         = host.InstallUnit
	runWorkerFn         = cluster.RunWorker
	runControllerFn     = runControllerWithEvidenceSampler
	prepareControllerFn = cluster.PrepareControllerWithLease
	prepareWorkerFn     = cluster.PrepareWorker
	cisKeyMintFence     = k8sutil.AdminMaintenanceFence

	prepareControlPlaneJoinFn = ha.PrepareControlPlaneJoin
	resolveJoinNodeIP         = cluster.ResolveJoinNodeIP
)

// Wires the engine's backup-evidence reader into the publisher; only this file can join them.
func runControllerWithEvidenceSampler(ctx context.Context, cfg *cluster.ClusterConfig, st *cluster.NodeState) error {
	return cluster.RunController(ctx, cfg, st, backupEvidenceSampler)
}

// The publisher is a heartbeat, not a full fetch: the read runs on the engine's built-in bounds.
var backupEvidenceSampler = func(ctx context.Context) (cluster.EvidenceSample, bool) {
	res := backupevidence.Evidence(ctx, backupevidence.EvidenceOptions{})
	return cluster.EvidenceSample{
		Node:           res.Node,
		NodeUID:        res.NodeUID,
		Complete:       res.Complete,
		LocalArtifacts: len(res.Local.Artifacts),
		RemoteCopies:   len(res.Remote.Copies),
		Backups:        len(res.Backups.Entries),
		Exact:          res.Backups.Complete,
		Warnings:       res.Warnings,
	}, true
}

// os.Args[0] is not reliably absolute; systemd runs the unit with a different cwd and PATH.
func selfExePath() string {
	exe, err := os.Executable()
	if err != nil {
		return version.StratumInstallPath
	}
	return exe
}

func upExecArgv(worker bool, configPath string, profileName string) []string {
	return cluster.UpArgv(selfExePath(), worker, configPath, profileName)
}

func (o runUpOptions) profileName() (string, error) {
	if o.cis && o.profile != "" && o.profile != nodestate.CISProfile {
		return "", fmt.Errorf("--cis and --profile %s name two profiles; use --profile alone", o.profile)
	}
	if o.profile != "" {
		return o.profile, nil
	}
	if o.cis {
		return nodestate.CISProfile, nil
	}
	return "", nil
}

// Refuses a start that names a different profile than the saved state records, or none at all.
func refuseDroppedProfile(prev *cluster.NodeState, want string) error {
	have := prev.ProfileName()
	switch {
	case have == "" || have == want:
		return nil
	case want == "":
		return fmt.Errorf("this node runs profile %q and this start names none; start it with --profile %s (or spec.profile: %s), as its unit does. A profile is not dropped by leaving its flag off: the apiserver reads config files only that profile writes",
			have, have, have)
	}
	return fmt.Errorf("this node runs profile %q; it cannot be changed to %q by restarting it", have, want)
}

// An unregistered name reports false; the start refuses it on its own.
func profileRequiresSELinux(name string) bool {
	p, ok := profile.Lookup(name)
	return ok && p.RequiresSELinux
}

// Refuses before the payload is staged or the SELinux labels are forgotten.
func preStageProfile(o runUpOptions) error {
	names := map[string]bool{}
	if name, err := o.profileName(); err == nil {
		names[name] = true
	}
	if !o.worker {
		if cfg, err := cluster.LoadConfig(o.configPath); err == nil {
			names[cfg.ProfileName()] = true
		}
	}
	if st, err := cluster.LoadState(); err == nil {
		names[st.ProfileName()] = true
	}
	for name := range names {
		if _, err := profile.Select(name); err != nil {
			return fmt.Errorf("%w; nothing on this host was changed", err)
		}
	}
	return nil
}

// On a restart spec.profile/spec.cis from the config file must agree with the flags.
func workerProfileName(o runUpOptions) (string, error) {
	name, err := o.profileName()
	if err != nil || o.server != "" {
		return name, err
	}
	if _, _, err := cluster.WorkerSELinux(o.configPath); err != nil {
		return name, nil
	}
	fromConfig, err := cluster.WorkerProfile(o.configPath)
	if err != nil || fromConfig == "" {
		return name, err
	}
	if name != "" && name != fromConfig {
		return "", fmt.Errorf("--profile %s and the config's spec.profile %s name two profiles", name, fromConfig)
	}
	return fromConfig, nil
}

func refuseWorkerRestartProfileChange(o runUpOptions) error {
	if !o.worker || o.server != "" {
		return nil
	}
	st, err := cluster.LoadState()
	if err != nil || st.Role != "worker" {
		return nil
	}
	name, err := workerProfileName(o)
	if err != nil {
		return err
	}
	return cluster.RefuseWorkerProfileChange(st, name)
}

// Absolute (the unit's cwd is /); a missing non-default config is refused: missing means defaults.
func upConfigPath(configPath string, worker bool) (string, error) {
	if configPath == "" {
		return "", nil
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve --config %q: %w", configPath, err)
	}
	if abs == cluster.DefaultConfigPath {
		return abs, nil
	}
	if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
		// A worker's unit never carries --config, so only an operator's command line reaches this.
		hint := "Restore the file, or name the intended one. If the stratum unit runs this node, point it at the " +
			"intended one: " + unitConfigHint
		if worker {
			hint = "Restore the file, or name the intended one"
		}
		return "", fmt.Errorf("stratum up: --config %s does not exist; only the default %s may be absent. "+
			"%s. Nothing on this host was changed", abs, cluster.DefaultConfigPath, hint)
	}
	return abs, nil
}

var unitConfigHint = "`sudo systemctl stop stratum`, then `sudo " + version.StratumInstallPath + " up --config <the intended file>` with the " +
	"unit's other flags (`systemctl cat stratum`); it runs in the foreground, so once it is up stop it with Ctrl-C " +
	"and run `sudo systemctl start stratum`"

// Refuses flags the role being started would silently drop.
func upFlagConflict(o runUpOptions) error {
	if !o.worker {
		var bad []string
		for _, f := range []struct{ flag, value, field string }{
			{"--pod-cidr", o.podCIDR, "spec.network.podCIDR"},
			{"--service-cidr", o.serviceCIDR, "spec.network.serviceCIDR"},
			{"--cluster-domain", o.clusterDomain, "spec.network.clusterDomain"},
			{"--node-label", strings.Join(o.nodeLabels, ","), "spec.node.labels"},
			{"--node-taint", strings.Join(o.nodeTaints, ","), "spec.node.taints"},
			{"--runtime-handler", strings.Join(o.runtimeHandlers, ","), "spec.node.runtimeHandlers"},
		} {
			if f.value != "" {
				bad = append(bad, fmt.Sprintf("%s %q (%s)", f.flag, f.value, f.field))
			}
		}
		if len(bad) == 0 {
			return nil
		}
		advice := "A control plane reads these from its --config file instead: move each value into the field named " +
			"beside it (spec.network is read at the cluster's first boot only, spec.node at every start), or " +
			"add --worker and --server to join a workload-only node"
		if o.join != "" {
			network := o.podCIDR != "" || o.serviceCIDR != "" || o.clusterDomain != ""
			node := len(o.nodeLabels)+len(o.nodeTaints)+len(o.runtimeHandlers) > 0
			const takesNetwork = "A control plane joining with --join takes its network from the cluster it joins, so remove "
			const toSpecNode = "each node flag into the spec.node field named beside it in this node's --config file, " +
				"which a control plane reads at its join and every start"
			switch {
			case network && node:
				advice = takesNetwork + "the network flags, and move " + toSpecNode
			case network:
				advice = takesNetwork + "these flags"
			default:
				advice = "Move " + toSpecNode
			}
			advice += "; to join a workload-only node instead, run the worker join line `stratum token create` prints " +
				"(without --control-plane), not this line: its token can read the shared cluster CA material"
		}
		return fmt.Errorf("stratum up: %s without --worker: only a --worker node reads these flags. %s. Nothing on this host was changed",
			strings.Join(bad, ", "), advice)
	}
	var bad []string
	if o.join != "" {
		bad = append(bad, "--join")
	}
	if o.certKey != "" {
		bad = append(bad, "--certificate-key")
	}
	if o.certKeyFile != "" {
		bad = append(bad, "--certificate-key-file")
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("stratum up: --worker cannot be combined with %s: those are for joining a control plane, "+
		"and --worker joins a workload-only node with --server. Use one or the other", strings.Join(bad, ", "))
}

// A var for tests; beside RunDir, which uninstall removes — never delete the lock file itself.
var startLockPath = version.StartLockPath

var forgetSELinuxLabels = selinux.ForgetLabels

// Read-only: it dials the control and containerd sockets before anything is staged or changed.
func preflightNoRunningNode() error {
	if comps.SupervisorRunning() {
		return runningNodeError(true, false)
	}
	// A clean stop takes containerd down, so a live socket means an earlier start left it behind.
	return runningNodeError(false, containerdOrphaned())
}

// asMember=true starts the node as a member; an error whenever re-joining could destroy it.
func alreadyJoinedDecision(st *cluster.NodeState, loadErr error, localPins func() []string, want string) (asMember bool, err error) {
	const remedy = "run `stratum uninstall --yes` first if you really mean to join this node to a cluster"
	if loadErr != nil {
		if errors.Is(loadErr, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stratum up --join: cannot read this node's stratum state (%w), so it cannot be "+
			"confirmed to be a fresh node, and the join would delete its etcd data — fix or remove %s, or %s",
			loadErr, cluster.StatePath(), remedy)
	}
	if st == nil {
		return false, nil
	}
	if err := clusterinfo.ValidatePin(want); err != nil {
		return false, fmt.Errorf("stratum up --join: %w", err)
	}
	if st.Role != "controller" {
		return false, fmt.Errorf("stratum up --join: this node already has stratum state as a %q node; "+
			"--join would turn it into a control plane and wipe its state — %s", st.Role, remedy)
	}
	pins := localPins()
	if len(pins) == 0 {
		return false, fmt.Errorf("stratum up --join: this node already has control-plane state but no readable "+
			"cluster CA certificate/key pair in %s, so it cannot be confirmed to belong to the cluster being joined, and the join "+
			"would delete its etcd data — restore the CA material from backup and start it without --join, or %s", version.PKIDir, remedy)
	}
	for _, p := range pins {
		if p == want {
			if problem := etcdMembershipProblem(st); problem != "" {
				return false, fmt.Errorf("stratum up --join: this node is a control plane of this cluster, but its "+
					"state in %s %s, so starting it would not keep its etcd membership: PrepareController "+
					"re-creates a single-member cluster (state=new) unless the saved membership is complete — "+
					"repair the file, or %s", cluster.StatePath(), problem, remedy)
			}
			return true, nil
		}
	}
	return false, fmt.Errorf("stratum up --join: this node is already a control-plane member of a different "+
		"cluster (its CA is %s; --discovery-token-ca-cert-hash names %s) — %s", pins[0], want, remedy)
}

// With a surviving journal and no local etcd data, resume the join: a member restart needs a WAL.
func alreadyJoinedWithIntentDecision(st *cluster.NodeState, loadErr error, localPins func() []string, want string, pending bool, dataDir string) (bool, error) {
	asMember, err := alreadyJoinedDecision(st, loadErr, localPins, want)
	if err != nil || !asMember || !pending {
		return asMember, err
	}
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stratum up --join: cannot inspect retained etcd data before resuming the join: %w", err)
	}
	return len(entries) != 0, nil
}

// An incomplete record is refused: the start would bootstrap a new cluster over its data.
func etcdMembershipProblem(st *cluster.NodeState) string {
	switch {
	case st.EtcdName == "":
		return "records no etcd member name"
	case st.EtcdInitialCluster == "":
		return "records no etcd cluster membership"
	case !slices.ContainsFunc(strings.Split(st.EtcdInitialCluster, ","), func(m string) bool {
		return strings.HasPrefix(m, st.EtcdName+"=")
	}):
		return fmt.Sprintf("records an etcd membership (%s) that does not include this member (%s)", st.EtcdInitialCluster, st.EtcdName)
	case st.EtcdState == "new" && len(strings.Split(st.EtcdInitialCluster, ",")) != 1:
		return "records state=new with multiple etcd members; only a single-member bootstrap record may use state=new"
	case st.EtcdState != "new" && st.EtcdState != "existing":
		return fmt.Sprintf("records etcd state %q, not new or existing", st.EtcdState)
	}
	return ""
}

// The original seed keeps its state=new record and notes growth in EtcdHasPeers.
func recordsLargerEtcdCluster(st *cluster.NodeState) bool {
	return st.Role == "controller" &&
		(st.EtcdState == "existing" || st.EtcdHasPeers || strings.Contains(st.EtcdInitialCluster, ","))
}

// etcd ignores initial-cluster flags on member data; require a nonempty WAL or it may start fresh.
func requireLocalEtcdMember(dir string) error {
	walDir := filepath.Join(dir, "member", "wal")
	entries, err := os.ReadDir(walDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wal") {
				continue
			}
			info, err := entry.Info()
			if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				return nil
			}
		}
	}
	return fmt.Errorf("stratum up: saved control-plane state records an etcd cluster but %s has no readable etcd WAL; "+
		"refusing to start an empty member, which could overwrite a cluster whose other members still hold quorum "+
		"(inspect and recover the local etcd data before restarting). Only if every other control plane is destroyed "+
		"or confirmed powered off, re-run with --confirm-etcd-quorum-lost", walDir)
}

// Never select the restart path using a lone certificate whose signing key cannot be used.
func localCAPins() []string {
	if _, err := tls.LoadX509KeyPair(filepath.Join(version.PKIDir, pki.RootCAName+".crt"),
		filepath.Join(version.PKIDir, pki.RootCAName+".key")); err != nil {
		return nil
	}
	var pins []string
	stagedRoot := strings.TrimSuffix(pki.StagedTrustName(pki.RootCAName+".crt"), ".crt")
	for _, name := range []string{pki.RootCAName, stagedRoot} {
		if _, err := tls.LoadX509KeyPair(filepath.Join(version.PKIDir, name+".crt"),
			filepath.Join(version.PKIDir, name+".key")); err != nil {
			continue
		}
		if p, err := token.CACertHashFor(name); err == nil {
			pins = append(pins, p)
		}
	}
	return pins
}

// A graceful teardown looks like an orphan for a few seconds, so re-probe past the shutdown grace.
func containerdOrphaned() bool {
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	deadline := time.Now().Add(containerdDrainGrace)
	for {
		if !sockLive(sock) {
			return false
		}
		if !time.Now().Before(deadline) {
			return true
		}
		time.Sleep(containerdDrainPoll)
	}
}

// Sits above the supervisor's 10s containerd stop grace plus reap slack.
const (
	containerdDrainGrace = 12 * time.Second
	containerdDrainPoll  = 500 * time.Millisecond
)

// Keys on containerd, not etcd/apiserver ports: control-plane containers survive supervisor stops.
func runningNodeError(supervisorRunning, containerdOrphaned bool) error {
	switch {
	case supervisorRunning:
		return fmt.Errorf("a stratum supervisor is already running on this node (%s is live). Stop it before running `stratum up` again", version.ControlSocket)
	case containerdOrphaned:
		return fmt.Errorf("an earlier `stratum up` left containerd serving %s with no supervisor — a kill -9, a crash, or a dropped session. Its kubelet still owns the node, so starting again would crash-loop with an exit status that points nowhere near the cause. Stop the leftover processes or run `stratum uninstall`, then retry",
			strings.TrimPrefix(version.ContainerdSock, "unix://"))
	default:
		return nil
	}
}

// A stale socket file left by a crash refuses the connection; a successful dial proves a listener.
func sockLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// spec.selinux: false in the config this start reads; staging runs before the config is validated.
func stageOptOut(o runUpOptions) bool {
	name, _ := o.profileName()
	if o.worker {
		st, stErr := cluster.LoadState()
		if profileRequiresSELinux(name) || (stErr == nil && profileRequiresSELinux(st.ProfileName())) {
			return false
		}
		if o.server != "" {
			c, err := cluster.LoadWorkerConfig(o.configPath)
			return err == nil && c.SELinuxOptOut
		}
		optOut, set, err := cluster.WorkerSELinux(o.configPath)
		if err == nil && set {
			return optOut
		}
		return stErr == nil && st.SELinuxOptOut
	}
	if st, err := cluster.LoadState(); err == nil && profileRequiresSELinux(st.ProfileName()) {
		return false
	}
	cfg, err := cluster.LoadConfig(o.configPath)
	return err == nil && cfg.SELinuxOptOut() && !profileRequiresSELinux(name) && !profileRequiresSELinux(cfg.ProfileName())
}

func workerConfig(configPath string) (cluster.WorkerConfig, error) {
	return cluster.LoadWorkerConfig(configPath)
}

// Separated from runUp so the validation is reachable from a test.
func controllerConfig(path string, profileName string) (*cluster.ClusterConfig, error) {
	cfg, err := cluster.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if profileName != "" {
		if have := cfg.ProfileName(); have != "" && have != profileName {
			return nil, fmt.Errorf("--profile %s and the config's spec.profile %s name two profiles", profileName, have)
		}
		cfg.SetProfile(profileName)
	}
	p, err := profile.Select(cfg.ProfileName())
	if err != nil {
		return nil, fmt.Errorf("%w; a node runs only a profile the binary it runs registers", err)
	}
	if err := checkProfileOperator(cfg, p); err != nil {
		return nil, err
	}
	if p.RequiresSELinux && cfg.SELinuxOptOut() {
		return nil, fmt.Errorf("the %s profile cannot be combined with spec.selinux: false: on an SELinux host the profile requires the container runtime and its containers to be confined. Remove spec.selinux: false, or drop the profile", p.Name)
	}
	return cfg, nil
}

// Kubernetes never auto-approves kubelet-serving CSRs; only the operator's approver signs them.
func checkProfileOperator(cfg *cluster.ClusterConfig, p profile.Profile) error {
	if !p.RequiresOperator || cfg.OperatorEnabled() {
		return nil
	}
	return fmt.Errorf("the %s profile requires the operator: it runs the kubelet serving-certificate "+
		"approver, and the profile makes kubelet serving certs bootstrap-issued. With spec.operator: false "+
		"no kubelet would get a serving certificate, losing kubectl "+
		"logs/exec/top cluster-wide. Remove spec.operator: false, or drop the profile", p.Name)
}

// spec.node from the worker flags, validated as the API server would, before the host is touched.
func (o runUpOptions) nodeSpec() (cluster.NodeSpec, error) {
	labels, err := cluster.ParseKeyValues("--node-label", o.nodeLabels)
	if err != nil {
		return cluster.NodeSpec{}, fmt.Errorf("stratum up: %w. Nothing on this host was changed", err)
	}
	handlers, err := cluster.ParseKeyValues("--runtime-handler", o.runtimeHandlers)
	if err != nil {
		return cluster.NodeSpec{}, fmt.Errorf("stratum up: %w. Nothing on this host was changed", err)
	}
	node := cluster.NodeSpec{Labels: labels, Taints: o.nodeTaints, RuntimeHandlers: handlers}
	if err := node.Validate(); err != nil {
		return cluster.NodeSpec{}, fmt.Errorf("stratum up: --node-label, --node-taint or --runtime-handler: %w. Nothing on this host was changed", err)
	}
	return node, nil
}

// Refuses a join-refusing spec.node before staging or relabelling touch the host.
func preflightWorkerJoinNode(o runUpOptions, flags cluster.NodeSpec) (cluster.WorkerConfig, error) {
	if !o.worker || o.server == "" {
		return cluster.WorkerConfig{}, nil
	}
	wcfg, err := workerConfig(o.configPath)
	if err != nil {
		return cluster.WorkerConfig{}, fmt.Errorf("stratum up: %w. Nothing on this host was changed", err)
	}
	if _, err := cluster.JoinNodeSpec(wcfg.Node, flags); err != nil {
		return cluster.WorkerConfig{}, fmt.Errorf("stratum up: worker join: %w. Nothing on this host was changed", err)
	}
	return wcfg, nil
}
