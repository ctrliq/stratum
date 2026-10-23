package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/staticpod"
)

func newCertsCommand() *cobra.Command {
	return newGroupCommand("certs", "Control-plane PKI lifecycle",
		newCertsCheckCommand(), newCertsRotateCommand(), newCertsRotateCACommand(),
		newCertsMigrateEtcdCACommand(), newCertsMigrateKubeletClientCACommand())
}

func newCertsCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Show certificate expiry status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			statuses, err := certs.Check(certs.Options{})
			if err != nil {
				return err
			}
			fmt.Printf("CA rotation: %s\n", certs.RotationPhase())
			for _, l := range certs.RotationDetail() {
				fmt.Printf("  %s\n", l)
			}
			fmt.Println()
			fmt.Printf("%-26s %-12s %-20s %s\n", "CERT", "KIND", "EXPIRES", "DAYS LEFT")
			for _, s := range statuses {
				kind, flag := "leaf", ""
				switch {
				case s.IsCA:
					kind = "CA"
				case s.Kubelet:
					kind = "kubelet CSR"
				}
				if s.Expiring {
					flag = "  <-- expiring"
				}
				if s.RenewalOverdue {
					flag = "  <-- renewal overdue"
				}
				fmt.Printf("%-26s %-12s %-20s %d%s\n", s.Name, kind, s.NotAfter.UTC().Format("2006-01-02"), s.DaysLeft, flag)
			}
			// The scheduled renewal loop's last attempt: the loop is
			// invisible otherwise — a dead loop looks the same as a
			// healthy one until the leaves expire. A local file read, so
			// a failure is reported, not fatal, like the table above.
			if rec, err := certs.LoadRenewalRecord(); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					fmt.Printf("\nlast renewal attempt: no renewal attempt recorded\n")
				} else {
					fmt.Printf("\nlast renewal attempt: could not read the renewal record: %v\n", err)
				}
			} else if rec.Result == "" {
				fmt.Printf("\nlast renewal attempt: %s (ok)\n", rec.AttemptedAt.UTC().Format("2006-01-02 15:04"))
			} else {
				fmt.Printf("\nlast renewal attempt: %s (failed: %s)\n", rec.AttemptedAt.UTC().Format("2006-01-02 15:04"), rec.Result)
			}
			// The HA shared certificate-key's age: it only re-keys as a
			// side effect of `token create --control-plane` or `certs rotate-ca`, so a
			// key that has not moved since install is worth surfacing. Cluster-facing,
			// so a read failure must not break the local, file-based table above.
			fmt.Print(clusterCertsReport(isWorkerNode(), sharedCertsKeySetAt, rotationPhase, carotateKeyStaged))
			return nil
		},
	}
}

// clusterCertsReport is the cluster-facing tail of `certs check`. A worker has
// no admin kubeconfig to read the cluster with, so it gets one line instead.
func clusterCertsReport(worker bool, keySetAt func() (time.Time, bool, error), phase func() string, staged func() (bool, error)) string {
	if worker {
		return "\nshared certs and CA-rotation key: checked on control planes only; run `stratum certs check` on one\n"
	}
	var b strings.Builder
	if setAt, present, err := keySetAt(); err != nil {
		fmt.Fprintf(&b, "\nshared certs (stratum-certs): could not read from the apiserver: %v\n", err)
	} else if !present {
		b.WriteString("\nshared certs (stratum-certs): not present\n")
	} else {
		days := int(time.Since(setAt).Hours() / 24)
		fmt.Fprintf(&b, "\nshared certs (stratum-certs): certificate-key last set %s (%d days ago); re-key with `stratum certs rotate --rekey-shared --yes`\n",
			setAt.UTC().Format("2006-01-02"), days)
	}
	b.WriteString(leftoverCarotateWarning(phase(), staged))
	return b.String()
}

// isWorkerNode is true only when the apiserver manifest is verifiably absent:
// an unreadable one is a broken controller, which keeps the cluster checks.
func isWorkerNode() bool {
	_, err := os.Stat(staticpod.Path("kube-apiserver"))
	return errors.Is(err, fs.ErrNotExist)
}

// leftoverCarotateWarning flags a stratum-carotate Secret that outlived its
// rotation: while this node reports idle nothing should be staging
// shared material, yet the Secret holds the key that decrypts ca.key, sa.key and
// front-proxy-ca.key. An etcd-CA migration in flight also stages it, so the
// message says to check first. A failed lookup is reported, not hidden.
func leftoverCarotateWarning(phase string, staged func() (bool, error)) string {
	if phase != "idle" {
		return ""
	}
	present, err := staged()
	if err != nil {
		return fmt.Sprintf("\ntransient CA-rotation key (stratum-carotate): could not read from the apiserver: %v\n", err)
	}
	if !present {
		return ""
	}
	return "\nWARN: kube-system/stratum-carotate exists while no CA rotation is in progress on this node.\n" +
		"      It holds the AES key that decrypts the shared cluster CA material (ca.key, sa.key,\n" +
		"      front-proxy-ca.key). If no rotate-ca or migrate-etcd-ca run is active, remove it:\n\n" +
		"        kubectl -n kube-system delete secret stratum-carotate\n"
}

// rotationPhase (node-local) and carotateKeyStaged (cluster-wide) back
// `certs check`'s leftover-rotation warning; package vars so both are testable
// without a cluster or on-disk rotation state. The --rekey-shared guard sits
// next to the write it protects: rekeyGuard is the one re-key entry
// point, certs.GuardRekey, which the token paths consult too.
var (
	rotationPhase         = certs.RotationPhase
	carotateKeyStaged     = certs.CARotateKeyStaged
	sharedCertsKeySetAt   = ha.SharedCertsKeySetAt
	rekeyGuard            = certs.GuardRekey
	requireNodeProfile    = certs.RequireNodeProfile
	rekeyMaintenanceFence = k8sutil.AdminMaintenanceFence
	rekeyNewCertKey       = ha.NewCertKey
	rekeySharedCerts      = ha.RekeySharedCerts
	rekeyWriteKeyFile     = writeHACertKeyFile
)

func newCertsRotateCommand() *cobra.Command {
	var ifExp int
	var yes bool
	var rekeyShared bool
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate leaf certificates (or --rekey-shared to re-key the HA shared cert material)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rekeyShared {
				// A distinct operation from leaf rotation: it mints a new certificate-key
				// and re-encrypts stratum-certs, invalidating the old key. It does not
				// touch leaves, so --if-expiring (leaf-only) does not apply.
				if cmd.Flags().Changed("if-expiring") {
					return fmt.Errorf("stratum certs rotate --rekey-shared re-keys the shared cert material; it is not a leaf rotation, so --if-expiring does not apply")
				}
				if !yes {
					return fmt.Errorf("stratum certs rotate --rekey-shared mints a new certificate-key and re-encrypts the shared cert material, invalidating the previous key (any pending HA-join command still holding the old key stops working); re-run with --yes")
				}
				if err := requireNodeProfile("rotate --rekey-shared"); err != nil {
					return err
				}
				// Hold the same fence as the rotation from before the staged-key
				// check through publication and key handover: a check alone leaves
				// a window for a rotation to stage a different decryption key.
				release, err := rekeyMaintenanceFence()
				if err != nil {
					return fmt.Errorf("stratum certs rotate --rekey-shared: %w", err)
				}
				defer release.Release()
				// One re-key entry point: the guard lives in internal/certs,
				// next to the staged key it reads, and `token create
				// --control-plane` and the MCP token_create tool refuse by it
				// too. Its two legs — the node-local mid-rotation phase read
				// (cheap, catches a rotation staged on THIS node) and the
				// cluster-wide stratum-carotate Secret (which a follower the
				// initiator has not reached yet needs) — are pinned in
				// internal/certs/rekey_test.go.
				if err := rekeyGuard(); err != nil {
					return fmt.Errorf("stratum certs rotate --rekey-shared: %w", err)
				}
				return runRekeyShared(cmd.OutOrStdout(), rekeyNewCertKey, func(key string) error {
					if err := release.Confirm(cmd.Context()); err != nil {
						return err
					}
					// The rekey makes several API calls between the take and
					// the Secret write; the fence's recheck rides with it, so
					// a lease lost while one of them stalled refuses the
					// publication instead of replacing the successor's bundle.
					if err := rekeySharedCerts(key, release.Confirm); err != nil {
						return err
					}
					return release.Confirm(cmd.Context())
				}, rekeyWriteKeyFile)
			}
			if ifExp == 0 && !yes {
				return fmt.Errorf("stratum certs rotate restarts the control-plane units; re-run with --yes (or --if-expiring=N for the conditional/timer path)")
			}
			// The rotation owns the shared interrupt context: cmd.Context() is
			// Background for every stratum invocation (main calls Execute, not
			// ExecuteContext), so the step boundaries below it could never fire
			// on the raw command context and a first Ctrl-C kills the rotation
			// mid-step by the default action. On this context the first signal
			// stops the run at its next step boundary and a second kills, the
			// same contract the cluster cert commands, the renewal loop and the
			// MCP tools run under — and a signal-asked stop exits 0, with the
			// boundary it halted on named.
			ctx, stop := interrupt.DerivedContext(cmd.Context())
			defer stop()
			// The rotation prints its own outcome ("nothing to rotate" /
			// "certs: rotated"), so the whether-it-rotated verdict needs no
			// CLI-side text of its own.
			_, err := certs.RotateContext(ctx, os.Stdout, certs.Options{IfExpiringDays: ifExp})
			if interrupt.ExpectedStop(ctx, err) {
				fmt.Fprintf(cmd.OutOrStdout(), "stratum: %v\nrotation stopped (signal received); re-run `stratum certs rotate` to finish it\n", err)
				return &cleanStopExit{stop: err}
			}
			return err
		},
	}
	cmd.Flags().IntVar(&ifExp, "if-expiring", 0, "only rotate if a leaf is within N days of expiry (0 = always)")
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed (rotation restarts control-plane units)")
	cmd.Flags().BoolVar(&rekeyShared, "rekey-shared", false,
		"re-key the HA shared cert material (new certificate-key, re-encrypt stratum-certs) without a token or CA rotation; requires --yes")
	return cmd
}

// runRekeyShared mints a fresh certificate-key, re-encrypts the shared cert
// material under it (so the previous key no longer decrypts stratum-certs),
// and hands the new key to the operator via a
// 0600 file, never printing it.
// The effects are injected so the handover ordering is testable without a cluster.
func runRekeyShared(out io.Writer,
	newCertKey func() (string, error),
	reUpload func(string) error,
	writeKeyFile func(string) (string, error),
) error {
	key, err := newCertKey()
	if err != nil {
		return fmt.Errorf("generate a new certificate-key: %w", err)
	}
	if err := reUpload(key); err != nil {
		return fmt.Errorf("re-encrypt the shared cert material: %w", err)
	}
	// The re-key has landed; the Secret now decrypts only under the new key. Hand
	// it over by path, never stdout — if the file write fails, say so rather than
	// print the key, and the operator re-runs to mint a fresh one.
	path, err := writeKeyFile(key)
	if err != nil {
		return fmt.Errorf("the shared cert material was re-keyed, but writing the new certificate-key to a file failed: %w — "+
			"re-run `stratum certs rotate --rekey-shared --yes` to mint and capture a fresh key", err)
	}
	if _, err := fmt.Fprintf(out, "Re-keyed the shared cert material — the previous certificate-key no longer decrypts stratum-certs.\n"+
		"New certificate-key written to %s (0600); it was not printed.\n"+
		"Existing control planes need no action. The new key is only needed to add a control plane before the next "+
		"`stratum token create --control-plane` (which mints its own key).\n", path); err != nil {
		return fmt.Errorf("the shared cert material was re-keyed and the new certificate-key is saved at %s (0600), but reporting success failed: %w", path, err)
	}
	return nil
}

// The restart warnings are internal/certs': the CLI prints them before a
// rotating (not rollback) rotate-ca run and the MCP tool returns them in its
// result, so the two front ends cannot drift. The local names keep the call
// sites short.
const (
	caRotateClusterWarning = certs.CARotateClusterWarning
	caRotatePhaseWarning   = certs.CARotatePhaseWarning
)

// adminK8sClientset builds the internal admin API client (super-admin.conf
// when the node carries one, admin.conf otherwise), as the client builder the
// --cluster orchestrators take.
func adminK8sClientset() (kubernetes.Interface, error) { return k8sutil.AdminClientset() }

// Seams for the cluster calls, so the warning can be tested without a cluster.
// The cluster orchestrators acquire their API client through a client builder;
// these commands hand them the internal admin builder.
var (
	rotateCACluster = func(ctx context.Context, o certs.CARotateOptions) error {
		return certs.RotateCAClusterContext(ctx, os.Stdout, o, adminK8sClientset)
	}
	rollbackCheck        = certs.CheckRollback
	rollbackCheckCluster = func(ctx context.Context, w io.Writer) error {
		return certs.CheckRollbackCluster(ctx, w, adminK8sClientset)
	}
	migrateEtcdCACluster = func(ctx context.Context, o certs.EtcdCAOptions) error {
		return certs.MigrateEtcdCAClusterContext(ctx, os.Stdout, o, adminK8sClientset)
	}
	migrateKubeletCACluster = func(ctx context.Context, o certs.KubeletCAOptions) error {
		return certs.MigrateKubeletClientCAClusterContext(ctx, os.Stdout, o, adminK8sClientset)
	}
	rotateCA       = certs.RotateCA
	scanNodeTokens = certs.ScanTokens
)

func newCertsRotateCACommand() *cobra.Command {
	var (
		clusterWide bool
		phase       string
		rotationRun string
		force       bool
		rollback    bool
		check       bool
		fetchShared bool
		yes         bool

		restartWorkloads, skipWorkloadCheck bool
		skipTokenCheck, tokensChecked       bool

		// The node scan Job's own mode: see certs.ScanTokens.
		scanTokens, restartKubelet bool
		oldKid                     string
	)
	cmd := &cobra.Command{
		Use:   "rotate-ca",
		Short: "Rotate the cluster CA",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rotationRun != "" && (phase == "" || clusterWide || rollback || scanTokens) {
				return fmt.Errorf("stratum certs rotate-ca: --rotation-run goes with a single --phase")
			}
			if check {
				// Read-only, so it needs no --yes. The answer is the exit
				// status, as for --scan-tokens: the cluster-wide check reads
				// it from each node's Job.
				f := certs.CARotateFlags{Cluster: clusterWide, Phase: phase, Rollback: rollback, Check: true}
				if err := f.Validate(); err != nil {
					return err
				}
				var err error
				if clusterWide {
					err = rollbackCheckCluster(cmd.Context(), os.Stdout)
				} else {
					err = rollbackCheck(os.Stdout)
				}
				if certs.IsRollbackUnsafe(err) {
					fmt.Println("stratum certs rotate-ca:", err)
					return &ErrExitCode{Code: certs.ExitRollbackUnsafe}
				}
				return err
			}
			if !yes {
				return fmt.Errorf("stratum certs rotate-ca restarts control-plane units; re-run with --yes")
			}
			// Only --cluster's node Jobs pass it, and they run one phase: on its own
			// or with --cluster it would skip the token gate without --force.
			if tokensChecked && (phase == "" || clusterWide) {
				return fmt.Errorf("stratum certs rotate-ca: --tokens-checked is set by --cluster on its node Jobs and goes with --phase, not --cluster")
			}
			if scanTokens {
				// --scan-tokens is the node scan Job's own mode: it stands alone.
				if rollback || clusterWide || phase != "" {
					return fmt.Errorf("stratum certs rotate-ca: --scan-tokens stands alone")
				}
				// The answer is the exit status, as for `upgrade --check`.
				err := scanNodeTokens(os.Stdout, oldKid, restartKubelet)
				if errors.Is(err, certs.ErrStaleTokens) {
					fmt.Println("stratum certs rotate-ca:", err)
					return &ErrExitCode{Code: certs.ExitStaleTokens}
				}
				return err
			}
			f := certs.CARotateFlags{
				Cluster: clusterWide, Phase: phase, Force: force, Rollback: rollback, FetchShared: fetchShared,
				RestartWorkloads: restartWorkloads, SkipWorkloadCheck: skipWorkloadCheck,
				SkipTokenCheck: skipTokenCheck, Check: check,
			}
			// The shared argument rules: --cluster with --phase, a missing phase,
			// an unknown phase. The MCP tool runs the same validator.
			if err := f.Validate(); err != nil {
				return err
			}
			o := f.Options()
			o.RunID = rotationRun
			if err := o.CheckWorkloadFlags(); err != nil {
				return fmt.Errorf("stratum certs rotate-ca: %w", err)
			}
			// What --cluster's node Jobs pass: the orchestrator checked, so no --force.
			o.SkipTokenCheck = o.SkipTokenCheck || tokensChecked
			switch {
			case rollback:
				return certs.RollbackCA(os.Stdout, o)
			case clusterWide:
				fmt.Fprint(cmd.OutOrStdout(), caRotateClusterWarning)
				ctx, stop := interrupt.DerivedContext(cmd.Context())
				defer stop()
				err := rotateCACluster(ctx, o)
				if interrupt.ExpectedStop(ctx, err) {
					fmt.Fprintf(cmd.OutOrStdout(), "stratum: %v\n", err)
					return &cleanStopExit{stop: err}
				}
				return err
			default:
				fmt.Fprint(cmd.OutOrStdout(), caRotatePhaseWarning)
				return rotateCA(os.Stdout, o)
			}
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&clusterWide, "cluster", false, "orchestrate all phases across every node (recommended)")
	fs.StringVar(&phase, "phase", "", "single-node phase: trust-new | sign-new | finalize")
	fs.StringVar(&rotationRun, "rotation-run", "", "rotation identity set by cluster orchestration")
	_ = fs.MarkHidden("rotation-run")
	fs.BoolVar(&force, "force", false, "rotate even though an old CA or service-account key is gone (compromise recovery); the old CA certificates (including kubelet-client-ca.crt where the kubelets have their own client CA) and the service-account public key (or its key) are still required")
	fs.BoolVar(&rollback, "rollback", false, "revert a partial rotation on this node (restore the pre-rotation CA files)")
	fs.BoolVar(&check, "check", false, "with --rollback: report whether this node can roll back, or with --cluster whether every node can, and change nothing; exits "+strconv.Itoa(certs.ExitRollbackUnsafe)+" when one cannot")
	fs.BoolVar(&fetchShared, "fetch-shared", false, "(follower CP, trust-new) pull the initiator's staged CA material first — set by --cluster orchestration")
	fs.BoolVar(&yes, "yes", false, "proceed (CA rotation restarts control-plane units)")
	fs.BoolVar(&restartWorkloads, "restart-workloads", false, "(--cluster) restart the Deployments, DaemonSets and StatefulSets that use in-cluster API credentials once the new CA is published, before sign-new; without it, --cluster refuses to start when there are any")
	fs.BoolVar(&skipWorkloadCheck, "skip-workload-check", false, "do not check the workloads that use in-cluster API credentials: --cluster does not refuse on them and --phase=sign-new does not wait for them to restart (they fail with x509 errors from sign-new until restarted)")
	fs.BoolVar(&skipTokenCheck, "skip-token-check", false, "finalize without checking that no mounted ServiceAccount token is still signed by the old key (those tokens get 401 once the old key is dropped); for compromise recovery, so it needs --force")
	fs.BoolVar(&scanTokens, "scan-tokens", false, "read-only scan of this node for mounted ServiceAccount tokens signed by --old-kid; exits 10 if there are any. Run by --cluster as a node Job")
	fs.StringVar(&oldKid, "old-kid", "", "(--scan-tokens) the id of the old ServiceAccount key")
	fs.BoolVar(&restartKubelet, "restart-kubelet", false, "(--scan-tokens) restart the kubelet once if it has such tokens, then scan again")
	fs.BoolVar(&tokensChecked, "tokens-checked", false, "(set by --cluster on its node Jobs) the mounted tokens were checked on every node already")
	for _, f := range []string{"scan-tokens", "old-kid", "restart-kubelet", "tokens-checked"} {
		_ = fs.MarkHidden(f)
	}
	return cmd
}

// newCertsMigrateEtcdCACommand exposes the etcd-CA migration: the
// command that moves an ALREADY-RUNNING cluster from the shared cluster root
// onto the dedicated etcd CA. Clusters built with the etcd trust hole already
// closed (a dedicated etcd CA from the start) never need it.
//
// --cluster is the supported form. The bare --phase form exists because the
// cluster orchestration itself drives it on each node, and because an operator
// recovering a halted run needs the same lever by hand; it is not the way to
// migrate a cluster, and running it on one node alone will cost that node
// quorum at issue-new.
func newCertsMigrateEtcdCACommand() *cobra.Command {
	var (
		clusterWide bool
		phase       string
		resume      bool
		rollback    bool
		force       bool
		fetchShared bool
		yes         bool
	)
	cmd := &cobra.Command{
		Use:   "migrate-etcd-ca",
		Short: "Migrate a running cluster's etcd onto its own CA",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("stratum certs migrate-etcd-ca restarts etcd on every control plane; re-run with --yes")
			}
			f := certs.EtcdCAFlags{
				Cluster: clusterWide, Phase: phase, Resume: resume,
				Rollback: rollback, Force: force, FetchShared: fetchShared,
			}
			if err := f.Validate(); err != nil {
				return err
			}
			o := f.Options()
			switch {
			case clusterWide:
				ctx, stop := interrupt.DerivedContext(cmd.Context())
				defer stop()
				err := migrateEtcdCACluster(ctx, o)
				if interrupt.ExpectedStop(ctx, err) {
					fmt.Fprintf(cmd.OutOrStdout(), "stratum: %v\n", err)
					return &cleanStopExit{stop: err}
				}
				return err
			case o.Phase != "":
				return certs.MigrateEtcdCA(os.Stdout, o)
			default:
				return fmt.Errorf("stratum certs migrate-etcd-ca: pass --cluster (drive all phases across every control plane) "+
					"or --phase=<%s> (single node)", strings.Join(certs.EtcdCAPhases, "|"))
			}
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&clusterWide, "cluster", false, "orchestrate all phases across every control plane (recommended)")
	fs.StringVar(&phase, "phase", "", "single-node phase: trust-both | issue-new | drop-root | rollback (re-add-root is set by --cluster orchestration)")
	fs.BoolVar(&resume, "resume", false, "(--cluster) continue on a cluster whose control planes are at different phases")
	fs.BoolVar(&rollback, "rollback", false, "undo a failed issue-new, re-deriving the etcd leaves under the cluster root")
	fs.BoolVar(&force, "force", false, "(--rollback) un-migrate a node/cluster that already completed drop-root — this RE-OPENS the etcd trust hole")
	fs.BoolVar(&fetchShared, "fetch-shared", false, "(follower CP, trust-both) pull the initiator's etcd CA first — set by --cluster orchestration")
	fs.BoolVar(&yes, "yes", false, "proceed (the migration restarts etcd)")
	return cmd
}

// newCertsMigrateKubeletClientCACommand moves a cluster installed before the
// kubelet-client CA onto it, or back to the cluster root.
func newCertsMigrateKubeletClientCACommand() *cobra.Command {
	var (
		clusterWide bool
		phase       string
		rollback    bool
		force       bool
		fetchShared bool
		yes         bool
	)
	cmd := &cobra.Command{
		Use:   "migrate-kubelet-client-ca",
		Short: "Move the kubelets' client trust off the cluster root onto its own CA",
		Long: "Move a running cluster's kubelets from trusting every client certificate the cluster root signs to trusting\n" +
			"only a dedicated kubelet-client CA, which signs the API servers' kubelet client certificate alone.\n" +
			"Clusters installed with it never need this. --cluster runs trust-both, issue-new and drop-root on every\n" +
			"node, control planes first; a halted run is resumed by running it again, and --rollback undoes it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("%s restarts the kubelet on every node and the API server on every control plane; re-run with --yes", "stratum certs migrate-kubelet-client-ca")
			}
			f := certs.KubeletCAFlags{Cluster: clusterWide, Phase: phase, Rollback: rollback, Force: force, FetchShared: fetchShared}
			if err := f.Validate(); err != nil {
				return err
			}
			o := f.Options()
			if !clusterWide {
				return certs.MigrateKubeletClientCA(os.Stdout, o)
			}
			ctx, stop := interrupt.DerivedContext(cmd.Context())
			defer stop()
			err := migrateKubeletCACluster(ctx, o)
			if interrupt.ExpectedStop(ctx, err) {
				fmt.Fprintf(cmd.OutOrStdout(), "stratum: %v\n", err)
				return &cleanStopExit{stop: err}
			}
			return err
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&clusterWide, "cluster", false, "run every phase on every node, from a control plane (recommended)")
	fs.StringVar(&phase, "phase", "", "run one phase on this node: "+strings.Join(append(append([]string{}, certs.KubeletCAPhases...), certs.KubeletCARollbackPhases...), " | "))
	fs.BoolVar(&rollback, "rollback", false, "(--cluster) move every kubelet back to trusting the cluster root")
	fs.BoolVar(&force, "force", false, "(rollback) re-add the cluster root to kubelets that completed drop-root, which lets root-signed client certificates reach them again")
	fs.BoolVar(&fetchShared, "fetch-shared", false, "(control plane, trust-both) fetch the initiator's kubelet-client CA first; set by --cluster")
	fs.BoolVar(&yes, "yes", false, "proceed (restarts the kubelets and API servers)")
	return cmd
}
