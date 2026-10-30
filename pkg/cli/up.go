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
			// An explicit empty --config (an unset "$CFG") would read no file
			// at all — not even the default — and start on pure defaults,
			// recording the empty path in the unit (upConfigPath). The unit
			// an earlier build wrote may carry one already, so the refusal
			// also says how to rewrite it.
			if cmd.Flags().Changed("config") && o.configPath == "" {
				return fmt.Errorf("stratum up: --config is empty; name the cluster config file, or leave --config out to read %s. "+
					"If the stratum unit runs this with an empty --config, rewrite it: %s. Nothing on this host was changed",
					cluster.DefaultConfigPath, unitConfigHint)
			}
			// Before any host write: an unstamped build renders unpullable
			// :unknown images (refuseUnstampedBuild has the full why).
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
	fs.StringVar(&o.profile, "profile", "", "node profile to run, by name (the binary must register it; a worker gets it from its join line)")
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
	fs.StringArrayVar(&o.nodeTaints, "node-taint", nil, "--worker: a taint this node registers with, key[=value]:Effect (repeatable); on a join it adds to spec.node.taints, on a restart the taints given replace the saved ones")
	fs.StringArrayVar(&o.runtimeHandlers, "runtime-handler", nil, "--worker: an extra containerd runtime handler, name=/absolute/path/to/runtime (repeatable); on a join it adds to spec.node.runtimeHandlers, on a restart the handlers given replace the saved ones")
	return cmd
}

type runUpOptions struct {
	configPath    string
	worker        bool
	profile       string
	cis           bool
	join          string
	token         string
	tokenFile     string
	caHash        string
	certKey       string
	certKeyFile   string
	server        string
	nodeIP        string
	serviceCIDR   string
	podCIDR       string
	clusterDomain string
	dual          bool
	// nodeLabels, nodeTaints and runtimeHandlers are --node-label,
	// --node-taint and --runtime-handler, as given.
	nodeLabels      []string
	nodeTaints      []string
	runtimeHandlers []string
	// confirmEtcdQuorumLost lifts the refusal to start a recorded etcd member on
	// an empty data dir.
	confirmEtcdQuorumLost bool
}

// resolveCertKey picks the HA shared-cert decryption key from the two inputs:
// --certificate-key wins when given, otherwise --certificate-key-file is read
// (trimmed). The file form is what `stratum token create --control-plane`
// hands the operator, so the key never has to travel on a command line or
// through stdout/logs. Empty (no HA join material) is not an error here
// — the join path validates that downstream.
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

// tokenEnvVar is the environment variable `stratum token create` tells the
// operator to export. It is named once here so the flag help, the resolver and
// the guidance printed by `token create` cannot drift apart — if they did, an
// operator who followed the advice would get an empty token.
const tokenEnvVar = "STRATUM_TOKEN"

// resolveToken picks the bootstrap token from the three inputs, most explicit
// first: --token, then --token-file (trimmed), then $STRATUM_TOKEN. The flag is
// kept because existing runbooks and the printed join command use it, but both
// of the others keep the token out of `ps` output and shell history, which the
// flag cannot.
//
// Empty is not an error here — the join paths validate that downstream, and
// `stratum up` without join material is a legitimate fresh bring-up.
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

// nodeUmask is the umask a node runs under, whatever the caller's shell or
// unit had. CIS and STIG profiles set root's umask to 027 or 077, which turns
// every os.MkdirAll under /var/lib/stratum into 0750/0700 and locks out each
// container running as a non-root user (CoreDNS crash-loops on
// /etc/coredns/Corefile) while root pods stay healthy. Secrets are
// created with explicit 0600/0700 modes, which a 022 umask never loosens.
const nodeUmask = 0o022

// ownsNodeStateAnnotation marks a command that creates node state under the
// data dir (`stage`, `up`). The root PersistentPreRun pins the umask for
// exactly those, once, so `umask 077; stratum stage; stratum up` cannot leave
// the staging root 0700 for the later `up` to inherit. It is not applied
// root-wide: snapshot restore and similar commands write secret-bearing files
// at os.Create's 0666 and rely on the caller's umask to narrow them.
const ownsNodeStateAnnotation = "stratum/owns-node-state"

// pinNodeUmask is the root command's PersistentPreRun. The umask is set
// before anything is created, so every directory and every child process
// (containerd, kubelet) inherits it.
func pinNodeUmask(cmd *cobra.Command, _ []string) {
	if cmd.Annotations[ownsNodeStateAnnotation] != "" {
		syscall.Umask(nodeUmask)
	}
}

// cleanStopOr surfaces err unchanged unless it is the run's own signal-asked
// stop — identified by the cause interrupt.DerivedContext's handler records.
// Err remains context.Canceled. That stop must exit 0 the way a stop after start already
// does: an operator who interrupts a join or a preparation asked for exactly
// what an operator interrupting a running node asks for, and the node is not
// brought up past either. A host's cancellation never qualifies (its cause
// is not a signal stop) and keeps its own
// meaning and exit code, like the operator's does; an unrelated error keeps
// its own, signal or no signal — the signal arriving nearby does not make a
// real failure clean.
func cleanStopOr(err error, ctx context.Context) error {
	if interrupt.ExpectedStop(ctx, err) {
		fmt.Println("stratum: stopped (signal received)")
		return &cleanStopExit{stop: err}
	}
	return err
}

// runUp runs this node. Default: a control-plane node that also schedules
// workloads (the default, hyperconverged node) — fresh bring-up, restart, or
// HA join, decided by the flags and any persisted node state. --worker runs a
// workload-only node that joins an existing cluster.
func runUp(parent context.Context, o runUpOptions) error {
	if err := upFlagConflict(o); err != nil {
		return err
	}
	if _, err := o.nodeSpec(); err != nil {
		return err
	}
	configPath, err := upConfigPath(o.configPath, o.worker)
	if err != nil {
		return err
	}
	o.configPath = configPath
	// The run owns its signals from here, before the first preflight call:
	// one interrupt context — SIGINT, SIGTERM and SIGHUP, where the first
	// signal cancels and restores the default action so a second one kills
	// (internal/interrupt) — for the whole run, both roles, so a signal
	// during preparation stops at a step boundary rather than killing the
	// process by the default action — for an HA join potentially mid-join,
	// past the live etcd member-add. The context is threaded into
	// the preparation paths so they can stop at their step boundaries, and
	// the run phases observe it too. It is derived from the command's
	// parent context, like the operator's, mcp's and dash's, so a host that
	// runs the tree through ExecuteContext stops `stratum up` with its own
	// cancellation instead of only with signals.
	ctx, stop := interrupt.DerivedContext(parent)
	defer stop()
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}
	// Refuse before touching anything if this node already has a stratum node
	// running — a live supervisor, or leftovers an earlier `stratum up` left
	// behind when it exited without cleanup. Starting over them would
	// crash-loop kubelet with no useful diagnostic.
	if err := preflightNoRunningNode(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}
	// preflightNoRunningNode is read-only, so two near-simultaneous `stratum up`
	// invocations can both pass it and then both reach supervisor.Run — two
	// supervisors racing the same host processes, one stealing the control socket
	// from the other. Serialize node starts with an exclusive lock held
	// for the whole run: the second invocation is refused here instead.
	releaseStartLock, err := etcddata.AcquireStartLockChecked(startLockPath, preflightNoRunningNode)
	if err != nil {
		return err
	}
	defer releaseStartLock()
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}
	if err := preStageProfile(o); err != nil {
		return err
	}
	// Uninstall takes the same data lock before removing DataDir. Keep staging
	// outside its deletion window, including for worker starts.
	releaseStageLock, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		releaseStageLock()
		return cleanStopOr(err, ctx)
	}
	if stageOptOut(o) {
		if err := selinux.ForgetLabels(); err != nil {
			releaseStageLock()
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		releaseStageLock()
		return cleanStopOr(err, ctx)
	}
	_, stageErr := assets.Stage(version.DataDir)
	releaseStageLock()
	if stageErr != nil && !errors.Is(stageErr, assets.ErrNoPayload) {
		return fmt.Errorf("staging payload: %w", stageErr)
	}
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}

	if o.worker {
		// Resolve join credentials only for a NEW join (--server given). A restart
		// reuses persisted state via PrepareWorker's fast path, which requires no token
		// — so an absent or even unreadable --token-file must not block it, and the
		// join-only flags must not fail a restart.
		var token string
		var wcfg cluster.WorkerConfig
		var selinuxOptOut *bool
		if o.server == "" {
			if v, set, err := cluster.WorkerSELinux(o.configPath); err != nil {
				fmt.Printf("  WARN: spec.selinux not read: %v; keeping the setting this worker saved\n", err)
			} else if set {
				selinuxOptOut = &v
			}
		}
		if o.server != "" {
			var err error
			if token, err = resolveToken(o.token, o.tokenFile); err != nil {
				return err
			}
			// Only a join reads and validates the whole config. A restart
			// reads just spec.selinux, above, and passes --node-ip, which can
			// move the address saved in node.yaml; PrepareWorker's fast path
			// keeps the rest of the settings node.yaml saved.
			if wcfg, err = workerConfig(o.configPath); err != nil {
				return err
			}
			selinuxOptOut = &wcfg.SELinuxOptOut
		}
		profileName, err := o.profileName()
		if err != nil {
			return err
		}
		node, err := o.nodeSpec()
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
		// A signal that arrived while the join's steps ran (most of which
		// take no context) stops the run at this boundary, before the
		// reboot-persistence unit is installed and the worker started: a
		// start its operator interrupted must not go on running.
		if err := ctx.Err(); err != nil {
			return cleanStopOr(err, ctx)
		}
		// Bare "stratum up --worker" resumes from the persisted NodeState
		// (PrepareWorker's restart fast path above), so that — never the
		// one-shot join flags — is what the reboot-persistence unit re-runs.
		if err := installUnit(upExecArgv(true, "", "")); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: WARNING: could not install the reboot-persistence unit: %v\n", err)
		}
		fmt.Printf("stratum %s: starting worker (server=%s)\n", version.StratumVersion, st.Server)
		// Only the run's own signal-asked stop is a clean stop; a host's
		// cancellation keeps its own meaning and exit code, like the
		// operator's (cleanStopOr).
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
		// A node that already holds control-plane state must never re-run the join:
		// PrepareControlPlaneJoin wipes the etcd data dir unconditionally, so a
		// re-run install script or a unit that kept its --join flags would destroy
		// this member. Decided before the token is even read, so a stale
		// or expired token cannot change the answer.
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
		// Join-only: a fresh controller bring-up (no --join) never touches these, so
		// an invalid --token-file cannot block it.
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
		})
		if err != nil {
			return cleanStopOr(err, ctx)
		}
		// The joined control plane's registration — its control-plane label
		// and version stamp included — is waited out by the controller's
		// node-registered task under RunController, which runs with this node
		// state; nothing is fired beside the controller.
	} else {
		if rejoinAsMember {
			fmt.Printf("stratum: %s is already a control-plane member of this cluster; "+
				"starting it instead of re-joining (drop --join to silence this)\n", o.join)
		}
		// The same path a plain restart takes: PrepareController keeps the saved
		// etcd membership (never resets an HA member to state=new).
		releaseDataLock, err := etcddata.Lock(version.EtcdDataDir)
		if err != nil {
			return err
		}
		prev, loadErr := cluster.LoadState()
		// A node whose saved state records a larger etcd cluster must come back
		// with that cluster's data. Started on an empty dir it would rejoin as a
		// blank member, or, if the record is stale, bootstrap a separate cluster,
		// while its peers may still hold quorum.
		if loadErr == nil && !o.confirmEtcdQuorumLost &&
			(rejoinAsMember || recordsLargerEtcdCluster(prev)) {
			if err := requireLocalEtcdMember(version.EtcdDataDir); err != nil {
				releaseDataLock()
				return err
			}
			// The WAL gate confirms this node's local etcd data: the join's
			// recovery journal — kept across the state save for the crash
			// window between that save and etcd's first WAL — has done its
			// job and ends here. A leftover journal is harmless, so a failed
			// clear only warns; the next join reconciles the file anyway.
			if err := ha.ClearJoinIntent(); err != nil {
				fmt.Fprintf(os.Stderr, "stratum: WARNING: %v\n", err)
			}
		}
		// A failed `stratum up --join` can leave the join's intent journal on
		// disk with no node.yaml beside it: the member was added to the cluster,
		// but the state save never ran. Bare `stratum up` on that node looks
		// fresh — no saved state, an empty etcd data directory — and would
		// bootstrap a separate single-member cluster while the joined cluster
		// still lists this node as its member. Refuse, and name the resume path:
		// the join itself reconciles the journal against the cluster's own
		// member list before adding anything. A node that has its state back is
		// an ordinary restart, and a journal left over beside it is harmless.
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
		// A start that will mint the CIS at-rest key takes the cluster-wide
		// maintenance fence — the same one `token create --control-plane`,
		// the MCP mint and the join's member-add take — around the whole
		// preparation that contains the mint and its second look. A held
		// fence refuses the start before anything on the host changes, naming
		// the live holder; that keeps the key this node mints in step with
		// the bundles other control planes publish and the members they
		// admit. A take no apiserver answers is skipped instead
		// (takeCISKeyMintFence says why).
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

	// A signal that arrived while the join or the controller preparation ran
	// stops the run at this boundary, before the run phase's own writes and
	// the supervisor: the node is not brought up past a stop its operator
	// asked for.
	if err := ctx.Err(); err != nil {
		return cleanStopOr(err, ctx)
	}

	// The steady-state restart command, resolved from cfg rather than echoed
	// from argv, so a node originally joined with --profile (or --join, which
	// this never carries forward — see upExecArgv) keeps that on every
	// restart.
	if err := installUnit(upExecArgv(false, o.configPath, cfg.ProfileName())); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: WARNING: could not install the reboot-persistence unit: %v\n", err)
	}

	fmt.Printf("stratum %s: starting control-plane node (vip=%q)\n", version.StratumVersion, st.VIP.Address)
	fmt.Printf("stratum: kubeconfig %s\n", version.AdminConf)
	// Only the run's own signal-asked stop is a clean stop; a host's
	// cancellation keeps its own meaning and exit code, like the operator's
	// (cleanStopOr).
	if err := runControllerFn(ctx, cfg, st); err != nil &&
		!interrupt.ExpectedStop(ctx, err) {
		return err
	}
	fmt.Println("stratum: stopped")
	return nil
}

// takeCISKeyMintFence takes the cluster-wide maintenance fence when this
// controller start is about to mint the selected profile's shared secrets —
// the cis profile's is the CIS at-rest encryption key — and returns nil when
// it is not.
//
// The mint and its second look run inside PrepareController (its CIS-config
// step), which the CLI cannot reach, so the fence is taken here, around the
// whole preparation, under the gate's own precondition spelled the same way:
// a shared secret of the selected profile missing at its
// version.ProfileSecretFile path (missingSharedSecret) — a secret on disk is
// used as it is, and nothing is minted. That condition keeps the fence off every
// ordinary restart: a rebooted control plane holds its key, so it never
// refuses while a maintenance run does, and crash recovery is never blocked
// by the fence.
//
// A first boot has no admin.conf yet, and needs no fence: a cluster that does
// not exist yet cannot have a maintenance run to exclude. With admin.conf
// present, an apiserver that answers decides: a fence another run holds, or
// any API answer refusing the lease call, refuses the start, because minting
// a key no other control plane has is the defect this prevents.
//
// A take whose connection to the apiserver fails — nothing serves the API
// through admin.conf — goes on unfenced, with a notice on stderr; any other
// failure, credentials that do not load or a TLS failure included, refuses
// as an answer does. The connection fails in the common way CIS is turned on
// for a cluster's only control plane: a restart after a reboot, or after a
// first --cis start that wrote admin.conf and
// failed before the key, where the apiserver is a static pod only this start
// brings up. Refusing there would crash-loop the start, since every retry
// finds the apiserver down again. With no apiserver answering, no mint,
// bundle download or member-add can run through it, and the key gate's own
// etcd read still refuses a control plane whose etcd has other members.
//
// The release must run as soon as the preparation returns, before the
// supervisor starts: the run phase lasts as long as the node does, and holding
// the fence for it would refuse every maintenance run for the node's whole
// life.
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
	// Only a failure to connect means nothing is serving. Credentials that do
	// not load, or a TLS failure against an apiserver that is there, may hide
	// a live maintenance run, so they refuse like any answer would.
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

// missingSharedSecret reports whether a shared secret prof declares is not on
// disk, which is when the controller's preparation mints.
func missingSharedSecret(prof profile.Profile) bool {
	for _, s := range prof.SharedSecrets {
		if _, err := os.Stat(version.ProfileSecretFile(s.Name)); err != nil {
			return true
		}
	}
	return false
}

// installUnit/runWorkerFn/runControllerFn/prepareControllerFn are
// host.InstallUnit/cluster.RunWorker/cluster.RunController/
// cluster.PrepareControllerWithLease behind seams: a wiring test can stub the
// blocking supervisor call and the host-mutating prepare step (real PKI
// generation, host.Prereqs' sysctls/modprobe/swapoff — not something a unit
// test may run) and observe the exact argv InstallUnit was given, without
// starting a real supervisor or faking a systemd host.
var (
	installUnit         = host.InstallUnit
	runWorkerFn         = cluster.RunWorker
	runControllerFn     = cluster.RunController
	prepareControllerFn = cluster.PrepareControllerWithLease
	// prepareWorkerFn is the worker setup seam for runUp wiring tests.
	prepareWorkerFn = cluster.PrepareWorker
	cisKeyMintFence = k8sutil.AdminMaintenanceFence
	// prepareControlPlaneJoinFn is the HA join seam, so a wiring test sees the
	// JoinOptions runUp builds from the flags and the config without a join.
	prepareControlPlaneJoinFn = ha.PrepareControlPlaneJoin
)

// selfExePath is the absolute path the reboot-persistence unit's ExecStart
// re-execs — os.Args[0] is not reliably absolute (a relative "./stratum" or a
// PATH-resolved bare name would break once systemd runs it with a different
// cwd/PATH).
func selfExePath() string {
	exe, err := os.Executable()
	if err != nil {
		return version.StratumInstallPath
	}
	return exe
}

// upExecArgv is the reboot-persistence unit's ExecStart: cluster.UpArgv run
// from this binary.
func upExecArgv(worker bool, configPath string, profileName string) []string {
	return cluster.UpArgv(selfExePath(), worker, configPath, profileName)
}

// profileName is the profile the flags select: --profile, or the deprecated
// --cis as "cis". Both, naming different profiles, is an error.
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

// refuseDroppedProfile refuses a control-plane start that names a different
// profile from the one the node's saved state records, or none at all.
//
// A profile is not something a restart can leave off: the node's apiserver
// reads config files and an at-rest encryption key only that profile writes,
// so a start without it would rewrite the unit, stop writing those files, and
// read every Secret as corrupt. A node with no saved profile may start with
// one: that is how a profile is turned on for a running cluster.
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

// profileRequiresSELinux reports whether the named profile, when registered,
// refuses an SELinux opt-out. An unregistered name reports false; the start
// refuses it on its own.
func profileRequiresSELinux(name string) bool {
	p, ok := profile.Lookup(name)
	return ok && p.RequiresSELinux
}

// preStageProfile refuses, before the payload is staged or the SELinux
// labels are forgotten, a start whose profile this binary does not register.
// It checks every name the start could end up running: the flag's, the
// config's on a control plane, and the saved state's. The later selection
// repeats the check with the resolved name and reports any conflict between
// them; this one only keeps an unregistered profile from writing to the host.
// A config or a state that cannot be read is left to the start proper, which
// refuses or warns about it with the right message.
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

// upConfigPath resolves --config to an absolute path and refuses one that
// names a file which does not exist.
//
// Absolute before it is loaded OR persisted: a relative --config reads
// correctly from the caller's cwd right now, but the unit ExecStart carries
// this same string, and a system service starts with / as its cwd by default
// — after a reboot the same relative path would name a different file, or
// none.
//
// cluster.LoadConfig treats a missing file as pure defaults, which is right
// for DefaultConfigPath — `stratum up` with no config brings up a sane single
// node — but for any other path it is a typo, and a typo dropped every setting
// the intended file held (spec.vip, the cis profile, snapshots, registries)
// with no error. The node kept its saved IP and looked healthy, and the unit
// recorded the bad path, so every later start ran without the config too.
// Refused here, before anything is staged or the unit is written, the way
// strict parsing refuses a misspelled key. Only absence is refused: a file
// that exists but cannot be read is left to LoadConfig, which reports it.
//
// The path refused may be one the operator never typed: the unit's ExecStart
// and a supervisor's upgrade re-exec both carry the path the node was started
// with, so the refusal says how to record a different one. An empty path is
// not given, and is returned as is; newUpCommand refuses an explicit empty
// --config before it gets here.
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
		// A worker's unit never carries --config, so only an operator's own
		// command line reaches this on a worker, and it records nothing.
		// A first start has no unit yet, so the unit's steps are conditional.
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

// unitConfigHint is how an operator points the reboot-persistence unit at a
// different --config. `stratum up` refuses over a running node, runs in the
// foreground, and must be given the unit's other flags: a profile is not
// dropped by leaving its flag off. Its unit install records the path for every
// later start.
var unitConfigHint = "`sudo systemctl stop stratum`, then `sudo " + version.StratumInstallPath + " up --config <the intended file>` with the " +
	"unit's other flags (`systemctl cat stratum`); it runs in the foreground, so once it is up stop it with Ctrl-C " +
	"and run `sudo systemctl start stratum`"

// upFlagConflict refuses flags the role being started would drop. Beside
// --worker, the control-plane join flags name two roles at once: the worker
// branch returns before the control-plane join is ever considered, so
// unrefused, `up --worker --join <url>` would silently drop --join, and on an
// existing controller it would rewrite node.yaml as a worker and leave its
// etcd member unmanaged, bypassing the already-joined refusal entirely. The
// certificate key decrypts the shared control-plane certs and means nothing to
// a worker, so it is refused the same way rather than ignored.
//
// --pod-cidr, --service-cidr and --cluster-domain are the reverse: they tell a
// worker its cluster's network, and runUp reads them only in the worker
// branch. A control plane takes its network from spec.network, so without
// --worker each flag would be silently dropped, at first boot and at --join
// alike, and the node would come up on a network its operator did not ask for.
// --node-label, --node-taint and --runtime-handler are worker flags for the
// same reason: a control plane reads spec.node from its config file.
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
		// A joining control plane takes the cluster's network, so spec.network
		// can only repeat it, and --worker beside --join is refused below. Nor
		// is a workload-only node told to swap --worker in for --join: the
		// line's token can read the shared cluster CA material, and a worker
		// join keeps its token on disk, so a worker needs its own token.
		advice := "A control plane reads these from its --config file instead: move each value into the field named " +
			"beside it (spec.network is read at the cluster's first boot only, spec.node at every start), or " +
			"add --worker and --server to join a workload-only node"
		if o.join != "" {
			advice = "A control plane joining with --join takes its network from the cluster it joins, so remove these " +
				"flags; to join a workload-only node instead, run the worker join line `stratum token create` prints " +
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

// startLockPath is the flock file that serializes node starts on this host. A var
// so tests can point it at a temp dir. It lives beside RunDir, not inside it:
// uninstall may remove RunDir while the old start still holds its descriptor.
// Never remove the coordination inode; closing the descriptor releases the lock.
var startLockPath = version.StartLockPath

// preflightNoRunningNode refuses to start when this node already has a stratum
// node running. Two cases: a live supervisor already serving the control socket
// (a plain double-start), or containerd/etcd/apiserver
// still running from an earlier `stratum up` that exited without stopping them
// (a kill -9, a crash, or a dropped session before SIGHUP shutdown was handled).
// The leftovers hold /run/stratum and the cluster ports, so a fresh start would
// crash-loop kubelet with an exit status that points nowhere near the cause.
// Read-only: it dials the control socket and the containerd socket, and refuses
// before anything is staged or changed.
func preflightNoRunningNode() error {
	if comps.SupervisorRunning() {
		return runningNodeError(true, false)
	}
	// No supervisor. The stratum containerd serving its socket with no supervisor
	// is the orphan signal: a clean supervisor stop takes containerd down (so an
	// ordinary restart is unaffected), so a live socket here means an earlier
	// `stratum up` exited without cleanup. The socket is stratum-specific, so a
	// distro containerd on /run/containerd does not trip it.
	return runningNodeError(false, containerdOrphaned())
}

// alreadyJoinedDecision decides what `stratum up --join` does on a node that
// already has node state. It returns asMember=true when the node is
// already a control plane of the cluster the pin names — start it as a member
// rather than re-joining — and an error whenever re-running the join could
// destroy something:
//
//   - the node is a worker: --join would turn it into a control plane;
//   - there is no readable cluster CA/key pair to compare against: we cannot tell whose
//     member this is, and the join would wipe its etcd data dir either way;
//   - the pin names a different cluster.
//
// localPins are the "sha256:<hex>" pins of usable CA cert/key pairs on disk:
// ca and, during a rotation, ca-new (a mid-rotation token is pinned to ca-new). Pure,
// so the whole table is unit-tested without a node.
//
// loadErr is LoadState's error. Only a missing state file means a fresh node:
// an unreadable or malformed node.yaml is a node whose state cannot be read,
// not one without state, and treating it as fresh would let the join wipe it.
// The pin is validated before it is compared, with the validator the join
// itself uses, so a missing or mistyped pin is reported as that and not as a
// different cluster. localPins is called only once there is state to check.
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

// A state save can publish node.yaml before its durability is confirmed. With
// a surviving join journal and no local etcd data, finish journal recovery
// rather than the normal member restart, which requires a retained WAL.
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

// etcdMembershipProblem describes what is missing from a controller's saved etcd
// membership, or returns "" when it is complete. The member path hands the node
// to PrepareController, which keeps the saved membership only when
// EtcdInitialCluster is set; otherwise it writes a fresh single-member
// state=new. So an incomplete record is refused rather than trusted: that is
// the difference between restarting this member and starting a new cluster
// over its data.
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

// recordsLargerEtcdCluster reports whether the saved state says this controller
// belongs to, or joined, an etcd cluster beyond a first single-member bootstrap.
// The original seed keeps its state=new record, which is etcd's argv, and notes
// the growth in EtcdHasPeers once it sees it (cluster.recordEtcdGrowth); a
// single-member state=new record without it is also what a sole-controller
// replacement legitimately starts from.
func recordsLargerEtcdCluster(st *cluster.NodeState) bool {
	return st.Role == "controller" &&
		(st.EtcdState == "existing" || st.EtcdHasPeers || strings.Contains(st.EtcdInitialCluster, ","))
}

// A saved bootstrap record may still say state=new after more members join.
// etcd ignores initial-cluster flags when restarting from member data. A mere
// member/ directory is not enough: require a nonempty WAL file, without which
// etcd can treat this as a fresh bootstrap.
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

// localCAPins returns pins only when the live CA certificate and its private
// key are readable and match. Never select the restart path using a lone
// certificate whose signing key cannot be used. A staged ca-new pair can
// contribute a second pin — named by pki's manifest, which records the staged
// replacement a rotation writes for the live root.
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

// containerdOrphaned reports whether the stratum containerd socket is a genuine
// leftover — still serving with no supervisor — rather than a graceful shutdown
// still draining. The distinction matters because a stop tears the control
// socket down the instant it begins (comps.SupervisorRunning goes false at once)
// while containerd keeps serving through the supervisor's SIGTERM→SIGKILL grace,
// so for several seconds a normal `stratum up` teardown looks identical to an
// orphan. Re-probe across a window past that grace: a draining containerd goes
// quiet and we proceed; a socket still live at the deadline is a real leftover
// from a kill -9 / crash / dropped session, and we refuse.
//
// A clear node returns immediately — the first probe fails and there is no wait.
// The window is only paid when a containerd socket is actually live here, which
// is exactly the ambiguous case worth resolving before refusing.
func containerdOrphaned() bool {
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	deadline := time.Now().Add(containerdDrainGrace)
	for {
		if !sockLive(sock) {
			return false // never up, or a graceful stop has finished draining
		}
		if !time.Now().Before(deadline) {
			return true // still serving past the shutdown grace: a genuine orphan
		}
		time.Sleep(containerdDrainPoll)
	}
}

// containerdDrainGrace bounds how long a live containerd socket is given to go
// quiet before it is judged an orphan. It sits above the supervisor's 10s
// containerd stop grace (SIGTERM→SIGKILL, internal/supervisor.stop) plus reap
// slack, so an in-flight graceful teardown is never mistaken for a leftover.
const (
	containerdDrainGrace = 12 * time.Second
	containerdDrainPoll  = 500 * time.Millisecond
)

// runningNodeError builds preflightNoRunningNode's message from the two probes.
// Pure, so the decision and its operator-facing wording are testable without a
// live node.
//
// It deliberately keys on containerd, NOT on the etcd/apiserver ports: the
// control-plane CONTAINERS survive a supervisor stop by design (their shims
// re-attach to the next containerd), so a bound :2379/:6443 is normal after an
// ordinary restart and must not be mistaken for an orphan.
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

// sockLive reports whether a unix socket has a listener right now — a stale
// socket file left by a crash refuses the connection, so this is "serving", not
// merely "the file exists".
func sockLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// stageOptOut reports spec.selinux: false in the config this start will read,
// for payload staging, which runs before it is read and validated. A file
// that cannot be read reports false here; the start then refuses or warns
// about it as usual. So does an opt-out under CIS, which the start refuses,
// and, on a join, a config the join refuses for another field: a refused
// start leaves the node labelled.
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

// workerConfig is what a worker join reads from its config file:
// spec.network.nodeIP, spec.selinux and spec.node. The node IP is passed to
// cluster.PrepareWorker apart from --node-ip, which outranks it there, so the
// join names the source it took. The config is parsed even when --node-ip is
// given, so a field a worker cannot honour is refused rather than dropped.
func workerConfig(configPath string) (cluster.WorkerConfig, error) {
	return cluster.LoadWorkerConfig(configPath)
}

// controllerConfig loads the cluster config, folds in the flags that override
// it, and validates the result. Separated from runUp so the validation is
// reachable from a test: a guard nothing calls is worth nothing, and asserting
// the call site by reading the source would be a test of the source text
// rather than of the behaviour.
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

// checkProfileOperator refuses a controller whose profile requires the
// operator while the config turns it off.
//
// A profile that bootstraps every node's kubelet serving certificate, control
// planes included, where the default pins a static leaf on control planes,
// gets serverTLSBootstrap: true in the kubelet config. The apiserver verifies
// kubelet serving certificates against --kubelet-certificate-authority in
// every profile. Kubernetes deliberately does NOT auto-approve
// kubernetes.io/kubelet-serving CSRs, so something has to sign them, and that
// something is the CSR approver inside `stratum operator`
// (internal/operator/csrapprove.go), which processes.go only supervises when
// OperatorEnabled() is true.
//
// With spec.operator: false the cluster therefore comes up looking healthy while
// no kubelet has a serving certificate to complete a TLS handshake with — a
// cluster-wide loss of kubectl logs/exec/top and metrics, on a profile chosen
// specifically to be more secure, with nothing in the startup path saying so.
//
// Checked against the RESOLVED config rather than the flag: the profile can
// equally arrive from spec.profile in the config file, and a guard that only
// watched the flag would wave that case through.
func checkProfileOperator(cfg *cluster.ClusterConfig, p profile.Profile) error {
	if !p.RequiresOperator || cfg.OperatorEnabled() {
		return nil
	}
	return fmt.Errorf("the %s profile requires the operator: it runs the kubelet serving-certificate "+
		"approver, and the profile makes kubelet serving certs bootstrap-issued. With spec.operator: false "+
		"no kubelet would get a serving certificate, losing kubectl "+
		"logs/exec/top cluster-wide. Remove spec.operator: false, or drop the profile", p.Name)
}

// nodeSpec is the spec.node the worker flags give. A flag not given leaves its
// field nil, which a restart reads as "keep what the worker saved".
func (o runUpOptions) nodeSpec() (cluster.NodeSpec, error) {
	labels, err := cluster.ParseKeyValues("--node-label", o.nodeLabels)
	if err != nil {
		return cluster.NodeSpec{}, fmt.Errorf("stratum up: %w. Nothing on this host was changed", err)
	}
	handlers, err := cluster.ParseKeyValues("--runtime-handler", o.runtimeHandlers)
	if err != nil {
		return cluster.NodeSpec{}, fmt.Errorf("stratum up: %w. Nothing on this host was changed", err)
	}
	var taints []string
	if len(o.nodeTaints) > 0 {
		taints = o.nodeTaints
	}
	return cluster.NodeSpec{Labels: labels, Taints: taints, RuntimeHandlers: handlers}, nil
}
