// Phase-major over node Jobs: all nodes finish a phase before the next starts, or quorum is lost.
package certs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// IP is the address the node's etcd client port listens on, which the survey dials.
type etcdNode struct {
	Name string
	IP   string
}

// Tests supply one backed by real per-node PKI directories, not a stub.
type etcdDriver interface {
	// Must return EtcdStateUnknown rather than guess; transient marks a re-probeable failure.
	Survey(n etcdNode) (state, detail string, transient bool)
	// force must reach the node, or every node refuses and the escape hatch is unreachable.
	RunPhase(n etcdNode, phase string, fetchShared, force bool) error
	// Returns a cleanup to run when the phase that needs it ends.
	PublishEtcdCA() (func(), error)
	// ReapStaleFetchKey removes a fetch key a previously killed run left behind.
	ReapStaleFetchKey()
	// Checked between nodes so a cancellation lands on a phase boundary.
	Interrupted() error
	// The non-etcd half of the drop-root gate: control planes Ready, local apiserver serving.
	ClusterHealthy(nodes []etcdNode) error
	// EtcdQuorumHealthy proves the members agree, not merely that each is up.
	EtcdQuorumHealthy(nodes []etcdNode) error
	// The process-derived proof that the etcd trust hole is shut on n.
	ConfirmClosed(n etcdNode) error
}

// The phase engine: no Kubernetes client and no I/O of its own.
type etcdMigration struct {
	nodes  []etcdNode // initiator first; the rest in a stable order
	drv    etcdDriver
	resume bool
	log    func(string)
	// w is the run's writer: the per-node phase lines write through it.
	w       io.Writer
	details map[string]string // node -> why its state could not be determined
	// Renews the shared maintenance Lease before each phase; a takeover stops the run.
	confirmLease func(phase string) error
	// Make the transient-unknown retry instant in tests.
	sleep        func(time.Duration)
	surveyBudget time.Duration
	// Members still unknown after the wait, only ever the transient not-serving-yet kind.
	stillStarting []string
}

func (m *etcdMigration) step(format string, a ...any) {
	if m.log != nil {
		m.log(fmt.Sprintf(format, a...))
	}
}

// Covers a just-rebooted control plane's WAL replay and rejoin, without silent staring.
const surveyRetryBudget = 90 * time.Second

func surveyBackoff(attempt int) time.Duration {
	d := 2 * time.Second << attempt
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

// Only not-serving-yet unknowns are re-probed; every other unknown is definite.
func (m *etcdMigration) survey() (map[string]string, error) {
	states := make(map[string]string, len(m.nodes))
	m.details = make(map[string]string, len(m.nodes))

	var pending []etcdNode
	for _, n := range m.nodes {
		state, detail, transient := m.drv.Survey(n)
		states[n.Name] = state
		if detail != "" {
			m.details[n.Name] = detail
		}
		if state == pki.EtcdStateUnknown && transient {
			pending = append(pending, n)
		}
	}

	sleep := m.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	budget := m.surveyBudget
	if budget == 0 {
		budget = surveyRetryBudget
	}
	// Report the interrupt, naming the members waited on; nothing was dispatched yet.
	interrupted := func(pending []etcdNode) error {
		err := m.drv.Interrupted()
		if err == nil {
			return nil
		}
		names := make([]string, 0, len(pending))
		for _, n := range pending {
			names = append(names, n.Name)
		}
		if len(names) == 0 {
			return fmt.Errorf("interrupted while waiting for etcd to start serving: %w", err)
		}
		return fmt.Errorf("interrupted while waiting for etcd on %s to start serving: %w", strings.Join(names, ", "), err)
	}
	var waited time.Duration
	for attempt := 0; len(pending) > 0; attempt++ {
		wait := surveyBackoff(attempt)
		if waited+wait > budget {
			break
		}
		if err := interrupted(pending); err != nil {
			return states, err
		}
		if m.confirmLease != nil {
			// The budget exceeds the lease's renewal window, so prove no successor took it mid-wait.
			if err := m.confirmLease("survey retry"); err != nil {
				return states, err
			}
		}
		names := make([]string, 0, len(pending))
		for _, n := range pending {
			names = append(names, fmt.Sprintf("%s (%s)", n.Name, m.details[n.Name]))
		}
		m.step("waiting for etcd to start serving (attempt %d) on %s",
			attempt+1, strings.Join(names, ", "))
		sleep(wait)
		waited += wait
		if err := interrupted(pending); err != nil {
			return states, err
		}

		var still []etcdNode
		for _, n := range pending {
			state, detail, transient := m.drv.Survey(n)
			states[n.Name] = state
			delete(m.details, n.Name) // a recovered member must not keep its stale reason
			if detail != "" {
				m.details[n.Name] = detail
			}
			if state == pki.EtcdStateUnknown && transient {
				still = append(still, n)
			}
		}
		pending = still
	}
	// No check inside the loop follows the last wait or probe, so check both once more.
	if waited > 0 {
		if err := interrupted(pending); err != nil {
			return states, err
		}
		if m.confirmLease != nil {
			if err := m.confirmLease("survey retry"); err != nil {
				return states, err
			}
		}
	}
	// What is left is only ever the not-serving-yet kind; plan() can name what that means.
	m.stillStarting = nil
	for _, n := range pending {
		m.stillStarting = append(m.stillStarting, n.Name)
	}
	return states, nil
}

// Appends each node's reason; "unknown" alone is not what the operator needs.
func (m *etcdMigration) describeSurvey(states map[string]string) string {
	rows := make([]string, 0, len(m.nodes))
	for _, n := range m.nodes {
		row := fmt.Sprintf("%s=%s", n.Name, states[n.Name])
		if d := m.details[n.Name]; d != "" {
			row += fmt.Sprintf(" (%s)", d)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, " ")
}

func distinctStates(states map[string]string) []string {
	seen := map[string]bool{}
	for _, s := range states {
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func nodesInState(nodes []etcdNode, states map[string]string, want string) []string {
	var out []string
	for _, n := range nodes {
		if states[n.Name] == want {
			out = append(out, n.Name)
		}
	}
	return out
}

// Ranks forward states so "adjacent" is defined; unknown is refused before this is used.
var etcdStateOrder = map[string]int{
	pki.EtcdStateNotMigrated: 0,
	pki.EtcdStateTrustBoth:   1,
	pki.EtcdStateIssueNew:    2,
	pki.EtcdStateMigrated:    3,
}

// The rank at or past which a forward phase is a no-op on a node.
var phaseAppliedAt = map[string]int{
	pki.EtcdPhaseTrustBoth: 1,
	pki.EtcdPhaseIssueNew:  2,
	pki.EtcdPhaseDropRoot:  3,
}

// Sentinel with errors.Is, so a later %w-wrapped error keeps matching.
var errNothingToDo = errors.New("already migrated")

// Refuses rather than guessing; --resume never covers unknown.
func (m *etcdMigration) plan(states map[string]string) error {
	if unknown := nodesInState(m.nodes, states, pki.EtcdStateUnknown); len(unknown) > 0 {
		refusal := fmt.Errorf("refusing to migrate: the etcd migration state of %s could not be determined (%s). "+
			"An indeterminate node is not a node that is fine — its etcd may be unreachable, may be serving a certificate this node cannot place, "+
			"or may have a trust anchor that cannot be named. --resume does NOT override this. "+
			"Fix or remove the node, then re-run",
			strings.Join(unknown, ", "), m.describeSurvey(states))
		return m.withStartupRemedy(refusal)
	}
	if len(nodesInState(m.nodes, states, pki.EtcdStateMigrated)) == len(m.nodes) {
		return errNothingToDo
	}
	if d := distinctStates(states); len(d) > 1 {
		if !m.resume {
			return fmt.Errorf("refusing to migrate a partially-migrated cluster: control planes are at different phases (%s). "+
				"This is either a run that halted cleanly or a node that is broken, and they need opposite responses. "+
				"Re-run with --resume to continue from here, or `--rollback` to return the cluster to the cluster root",
				m.describeSurvey(states))
		}
		// --resume may continue only a halted run; a wider spread is an already-split cluster.
		lo, hi := len(etcdStateOrder), 0
		for _, s := range d {
			o := etcdStateOrder[s] // unknown already refused above; all d are known
			if o < lo {
				lo = o
			}
			if o > hi {
				hi = o
			}
		}
		if hi-lo > 1 {
			return fmt.Errorf("refusing to resume: the control planes are at non-adjacent phases (%s), which a cleanly-halted run cannot produce — "+
				"this cluster is already split rather than paused. Use `--rollback` to return it to the cluster root, or fix the lagging node before resuming",
				m.describeSurvey(states))
		}
	}
	return nil
}

// Adds start-up steps for members still unknown after the wait; a definite unknown has none.
func (m *etcdMigration) withStartupRemedy(refusal error) error {
	if len(m.stillStarting) == 0 {
		return refusal
	}
	return fmt.Errorf("%w\n%s", refusal, m.startupRemedy(m.stillStarting))
}

// An accepted-but-unanswered handshake means etcd is starting; a refusal means not listening.
func (m *etcdMigration) startupRemedy(unknown []string) string {
	var stalled, notListening []string
	for _, name := range unknown {
		if strings.Contains(m.details[name], pki.EtcdStalled) {
			stalled = append(stalled, name)
		} else {
			notListening = append(notListening, name)
		}
	}
	var steps []string
	if len(stalled) > 0 {
		steps = append(steps, fmt.Sprintf(
			"%s's etcd accepted a connection but did not answer TLS within the wait, which is what a member that was just power-cycled looks like before it rejoins raft. "+
				"On %s: read etcd's log, under /var/log/pods/kube-system_etcd-*; re-run with --resume once it is serving. "+
				"Removing the node is only needed if its etcd will not start.",
			strings.Join(stalled, ", "), strings.Join(stalled, ", ")))
	}
	if len(notListening) > 0 {
		steps = append(steps, fmt.Sprintf(
			"%s's etcd is not listening on its client port: read etcd's log on that node, and if it shows a certificate/key mismatch, "+
				"repair it first with `sudo %s certs migrate-etcd-ca --phase=issue-new --yes` on that node, which rewrites the pair and restarts etcd without needing it to serve; "+
				"then re-run with --resume once etcd is serving.",
			strings.Join(notListening, ", "), version.StratumInstallPath))
	}
	return strings.Join(steps, " ")
}

// run drives the whole migration.
func (m *etcdMigration) run() error {
	m.step("etcd-CA migration across %d control plane(s): initiator=%s", len(m.nodes), m.nodes[0].Name)
	states, err := m.survey()
	if err != nil {
		return err
	}
	m.step("Survey (from each running etcd): %s", m.describeSurvey(states))
	switch err := m.plan(states); {
	case errors.Is(err, errNothingToDo):
		m.step("Every control plane has already completed drop-root — verifying rather than re-running")
		return m.confirmClosed("this cluster reports as already migrated, but")
	case err != nil:
		return err
	}

	for i, phase := range EtcdCAPhases {
		// A run that lost the maintenance lease must dispatch nothing more.
		if m.confirmLease != nil {
			if err := m.confirmLease(phase); err != nil {
				return err
			}
		}
		if phase == pki.EtcdPhaseDropRoot {
			m.step("Phase %d/%d: %s preflight (cluster-wide)", i+1, len(EtcdCAPhases), phase)
			if err := m.gateDropRoot(); err != nil {
				return fmt.Errorf("refusing %s (the cluster is left in the safe %s state): %w",
					pki.EtcdPhaseDropRoot, pki.EtcdPhaseIssueNew, err)
			}
		}
		m.step("Phase %d/%d: %s across %d control plane(s)", i+1, len(EtcdCAPhases), phase, len(m.nodes))
		if err := m.runPhaseWithSharedMaterial(phase, states); err != nil {
			return err
		}
	}
	return m.confirmClosed("the migration wrote every phase but")
}

// Publishes the etcd CA only while a follower might fetch it, then removes it.
func (m *etcdMigration) runPhaseWithSharedMaterial(phase string, states map[string]string) error {
	// A single control plane has no follower to distribute to.
	if phase != pki.EtcdPhaseTrustBoth || len(m.nodes) < 2 {
		return m.runPhase(phase, states)
	}
	m.drv.ReapStaleFetchKey()
	cleanup, err := m.drv.PublishEtcdCA()
	if err != nil {
		return fmt.Errorf("publish the etcd CA for the other control planes: %w", err)
	}
	defer cleanup()
	return m.runPhase(phase, states)
}

// Serial, initiator first: two members restarting at once loses quorum on a three-node cluster.
func (m *etcdMigration) runPhase(phase string, states map[string]string) error {
	for i, n := range m.nodes {
		if err := m.drv.Interrupted(); err != nil {
			return fmt.Errorf("%s halted before %s: %w (the cluster is left in the last state every node completed, which is a working one)", phase, n.Name, err)
		}
		// fetchShared marks a follower, which must fetch the CA; only the initiator mints.
		fetchShared := i > 0 && phase == pki.EtcdPhaseTrustBoth
		// A resumed run would otherwise print identically to a fresh one.
		if o, ok := etcdStateOrder[states[n.Name]]; ok && o >= phaseAppliedAt[phase] {
			fmt.Fprintf(m.w, "  %s: %s (already applied — skipping)\n", n.Name, phase)
		} else {
			fmt.Fprintf(m.w, "  %s: %s\n", n.Name, phase)
		}
		if m.confirmLease != nil {
			if err := m.confirmLease(phase + " on " + n.Name); err != nil {
				return err
			}
		}
		if err := m.drv.RunPhase(n, phase, fetchShared, false); err != nil {
			return fmt.Errorf("control plane %s: %s: %w (halting — the cluster is left in the last state every node completed, which is a working one; "+
				"re-run with --resume once %s is reachable, or `--rollback` to return to the cluster root)",
				n.Name, phase, err, n.Name)
		}
		// A TCP dial does not prove raft quorum; prove convergence before touching another node.
		if err := m.drv.EtcdQuorumHealthy(m.nodes); err != nil {
			return fmt.Errorf("control plane %s completed %s but the cluster did not re-converge: %w "+
				"(halting before restarting another member; the cluster is left in a working state)", n.Name, phase, err)
		}
	}
	return nil
}

// Safe only when every member presents an etcd-CA-signed leaf; the per-node guard cannot see it.
func (m *etcdMigration) gateDropRoot() error {
	states, err := m.survey()
	if err != nil {
		return err
	}
	if unknown := nodesInState(m.nodes, states, pki.EtcdStateUnknown); len(unknown) > 0 {
		return m.withStartupRemedy(fmt.Errorf("the state of %s could not be determined (%s) — an indeterminate node is never treated as ready",
			strings.Join(unknown, ", "), m.describeSurvey(states)))
	}
	var lagging []string
	for _, n := range m.nodes {
		switch states[n.Name] {
		case pki.EtcdStateIssueNew, pki.EtcdStateMigrated:
		default:
			lagging = append(lagging, fmt.Sprintf("%s=%s", n.Name, states[n.Name]))
		}
	}
	if len(lagging) > 0 {
		return fmt.Errorf("%s has not completed %s on every control plane (%s); "+
			"dropping the root now would lock those members out of raft",
			strings.Join(lagging, ", "), pki.EtcdPhaseIssueNew, m.describeSurvey(states))
	}
	if err := m.drv.ClusterHealthy(m.nodes); err != nil {
		return err
	}
	return m.drv.EtcdQuorumHealthy(m.nodes)
}

// A cluster-root-signed client certificate must be rejected by every running etcd.
func (m *etcdMigration) confirmClosed(lead string) error {
	var bad []string
	for _, n := range m.nodes {
		if err := m.drv.ConfirmClosed(n); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", n.Name, err))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s could not verify it landed on the running etcd of: %s. "+
			"Re-run `%s --cluster --resume --yes` once they are reachable; a node whose drop-root restart did not take still accepts every kubelet certificate in the cluster",
			lead, strings.Join(bad, " | "), pki.EtcdMigrateCommand)
	}
	m.step("Verified against every running etcd: a cluster-root-signed client certificate is rejected (the etcd trust hole closed)")
	return nil
}

// --force widens trust everywhere first, then re-derives leaves node by node; both halt-safe.
func (m *etcdMigration) rollback(force bool) error {
	states, err := m.survey()
	if err != nil {
		return err
	}
	m.step("Survey (from each running etcd): %s", m.describeSurvey(states))
	if unknown := nodesInState(m.nodes, states, pki.EtcdStateUnknown); len(unknown) > 0 {
		return m.withStartupRemedy(fmt.Errorf("refusing to roll back: the state of %s could not be determined (%s). "+
			"Rolling back re-adds the cluster root to etcd's trust, and doing that blind is how a node that was already fixed is re-opened",
			strings.Join(unknown, ", "), m.describeSurvey(states)))
	}
	migrated := nodesInState(m.nodes, states, pki.EtcdStateMigrated)
	if len(migrated) > 0 && !force {
		return fmt.Errorf("refusing to roll back: %s already completed %s (%s). "+
			"This phase undoes a FAILED %s; deliberately un-migrating a cluster RE-OPENS the etcd trust hole on it and needs --force",
			strings.Join(migrated, ", "), pki.EtcdPhaseDropRoot, m.describeSurvey(states), pki.EtcdPhaseIssueNew)
	}

	// Pass 1, only when a node dropped the root; intermediate-state bundles already carry it.
	if len(migrated) > 0 {
		m.step("Un-migration 1/2: re-add the cluster root to etcd's trust on every control plane (no certificate moves)")
		// A rollback that lost the maintenance lease must dispatch nothing more.
		if m.confirmLease != nil {
			if err := m.confirmLease(EtcdPhaseReAddRoot); err != nil {
				return err
			}
		}
		if err := m.rollbackPass(states, EtcdPhaseReAddRoot, force, func(state string) bool {
			// A node that never dropped the root already trusts it.
			return state == pki.EtcdStateMigrated
		}); err != nil {
			return err
		}
		// Re-survey: pass 2's safety rests on every member now trusting the root.
		if states, err = m.survey(); err != nil {
			return err
		}
		m.step("Survey after widening: %s", m.describeSurvey(states))
		// A node that went unknown during pass 1 must not be dispatched a leaf move on unproven trust.
		if unknown := nodesInState(m.nodes, states, pki.EtcdStateUnknown); len(unknown) > 0 {
			return m.withStartupRemedy(fmt.Errorf("refusing to re-derive any certificate: the state of %s could not be determined after widening (%s) — "+
				"an indeterminate node is never treated as having adopted the wider trust, and moving another node's certificate now could evict it from raft",
				strings.Join(unknown, ", "), m.describeSurvey(states)))
		}
		if still := nodesInState(m.nodes, states, pki.EtcdStateMigrated); len(still) > 0 {
			return fmt.Errorf("refusing to re-derive any certificate: %s still enforces %s alone (%s) — "+
				"its etcd did not adopt the widened trust, and moving another node's certificate now would evict it from raft",
				strings.Join(still, ", "), pki.EtcdCAName, m.describeSurvey(states))
		}
	}

	m.step("Un-migration 2/2: re-derive etcd certificates under the cluster root")
	if m.confirmLease != nil {
		if err := m.confirmLease(pki.EtcdPhaseRollback); err != nil {
			return err
		}
	}
	if err := m.rollbackPass(states, pki.EtcdPhaseRollback, force, func(state string) bool {
		// A never-migrated node is pki's refused case and a halted run's leftover shape; skip it.
		return state != pki.EtcdStateNotMigrated
	}); err != nil {
		return err
	}

	// A bundle on disk proves intent, not that the restart took.
	after, err := m.survey()
	if err != nil {
		return err
	}
	m.step("Rollback complete: %s", m.describeSurvey(after))
	var bad []string
	for _, n := range m.nodes {
		switch after[n.Name] {
		case pki.EtcdStateTrustBoth, pki.EtcdStateNotMigrated:
		default:
			bad = append(bad, fmt.Sprintf("%s=%s", n.Name, after[n.Name]))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("rollback ran on every control plane but %s did not return to a root-trusting state (%s) — "+
			"check whether etcd restarted on those nodes", strings.Join(bad, ", "), m.describeSurvey(after))
	}
	return nil
}

// Applies one un-migration pass to every node that needs it, serially.
func (m *etcdMigration) rollbackPass(states map[string]string, phase string, force bool, needs func(string) bool) error {
	for _, n := range m.nodes {
		if err := m.drv.Interrupted(); err != nil {
			return fmt.Errorf("%s halted before %s: %w", phase, n.Name, err)
		}
		if !needs(states[n.Name]) {
			fmt.Fprintf(m.w, "  %s: skipped %s — nothing to do here\n", n.Name, phase)
			continue
		}
		fmt.Fprintf(m.w, "  %s: %s\n", n.Name, phase)
		if m.confirmLease != nil {
			if err := m.confirmLease(phase + " on " + n.Name); err != nil {
				return err
			}
		}
		if err := m.drv.RunPhase(n, phase, false, force); err != nil {
			return fmt.Errorf("control plane %s: %s: %w (halting; every node that completed this pass still verifies every other, so the cluster keeps working)",
				n.Name, phase, err)
		}
		// No per-node quorum probe: pass 2 re-signed the certificate the probe would authenticate with.
	}
	return nil
}

// ─── production driver ───────────────────────────────────────────────────────

const etcdMigrateJobPrefix = "stratum-etcdca"

// Runs phases through node Jobs; surveys through TLS handshakes.
type clusterDriver struct {
	ctx       context.Context
	cs        kubernetes.Interface
	initiator string
	w         io.Writer
}

// A var so a test can point the real driver at a listener of its own.
var probeEtcdTrust = pki.ProbeEtcdTrust

// The failure to catch is a file that says one thing while the process does another.
func (d *clusterDriver) Survey(n etcdNode) (string, string, bool) {
	t := probeEtcdTrust(n.IP)
	if t.Err != nil {
		return t.State(), t.Err.Error(), t.Transient()
	}
	return t.State(), "", false
}

func (d *clusterDriver) RunPhase(n etcdNode, phase string, fetchShared, force bool) error {
	if n.Name == d.initiator {
		return MigrateEtcdCA(d.w, EtcdCAOptions{Phase: phase, Force: force, Quiet: true})
	}
	if err := nodejob.Run(d.ctx, d.cs, nodejob.Spec{
		Output:         d.w,
		Namespace:      caNamespace,
		Name:           nodejob.TruncName(fmt.Sprintf("%s-%s-%s", etcdMigrateJobPrefix, phase, n.Name)),
		Node:           n.Name,
		LabelKey:       "stratum.ciq.com/etcdca",
		Container:      "etcdca",
		Argv:           etcdNodeArgv(phase, fetchShared, force),
		ActiveDeadline: 600 * time.Second,
		PollAttempts:   120,
		Kind:           "etcd-CA migration",
	}); err != nil {
		return err
	}
	if err := nodejob.WaitReady(d.ctx, d.cs, n.Name, 60); err != nil {
		return fmt.Errorf("after etcd-CA migration phase %s: %w", phase, err)
	}
	return nil
}

// The staged key decrypts all cluster private keys; a hard kill leaves it for the next run to reap.
func (d *clusterDriver) PublishEtcdCA() (func(), error) {
	if err := checkStagedKeyOwner(d.ctx, d.cs, carotateOpMigrateEtcdCA); err != nil {
		return nil, err
	}
	certKey, err := ha.NewCertKey()
	if err != nil {
		return nil, err
	}
	// The upload runs pki.EnsureEtcdTrustBundle first, so the etcd CA exists on an older control plane.
	if err := uploadStagedMaterial(certKey); err != nil {
		return nil, fmt.Errorf("publish shared material: %w", err)
	}
	if err := putStagedKey(d.ctx, d.cs, certKey, migrationStagedKeyOwner(certKey)); err != nil {
		// The Secret can exist even though this failed; leave no key with no cleanup registered.
		ctx, cancel := cleanupContext()
		cleanupOwnCarotateKey(ctx, d.w, d.cs, "", certKey)
		cancel()
		return nil, fmt.Errorf("stage the fetch key: %w", err)
	}
	return func() {
		ctx, cancel := cleanupContext()
		defer cancel()
		cleanupOwnCarotateKey(ctx, d.w, d.cs, "", certKey)
	}, nil
}

// Removal must happen even while the run is being cancelled; a cancelled context fails deletes.
func cleanupContext() (context.Context, context.CancelFunc) {
	// The caller defers the cancel; dropping it would leak the timer past the retry schedule.
	return context.WithTimeout(context.Background(), cleanupBudget)
}

// Covers the apiserver health gate, then five attempts five seconds apart.
const cleanupBudget = 3 * time.Minute

func (d *clusterDriver) Interrupted() error { return d.ctx.Err() }

// Reaps a killed run's fetch key before publishing; a delete that fails only warns.
func (d *clusterDriver) ReapStaleFetchKey() {
	sec, err := d.cs.CoreV1().Secrets(caNamespace).Get(d.ctx, caRotateKeySecret, metav1.GetOptions{})
	if err != nil {
		return // absent (or unreadable, which publishing will surface anyway)
	}
	// Only our operation's leftover, or an unannotated legacy key, is ours to reap.
	if foreign, conflict := stagedKeyForeign(stagedKeyOwner{operation: carotateOpMigrateEtcdCA}, sec); conflict {
		fmt.Fprintf(d.w, "  WARN: a transient fetch-key Secret %s/%s belongs to %s. Leaving it; that operation's followers may still need it. Resume or roll back that operation before migrating etcd. If no such run is live, remove it by hand:\n\n          kubectl -n %s delete secret %s\n", caNamespace, caRotateKeySecret, foreign, caNamespace, caRotateKeySecret)
		return
	}
	fmt.Fprintf(d.w, "  WARN: a transient fetch-key Secret %s/%s was already present — a previous run was killed before it could\n"+
		"        remove it. Removing it now; it held the key to this cluster's CA material until this moment.\n",
		caNamespace, caRotateKeySecret)
	ctx, cancel := cleanupContext()
	defer cancel()
	_ = cleanupCarotateKeySnapshot(ctx, d.w, d.cs, sec)
}

func (d *clusterDriver) ClusterHealthy(nodes []etcdNode) error {
	// Gating on the whole cluster would let one NotReady worker strand drop-root.
	for _, n := range nodes {
		node, err := d.cs.CoreV1().Nodes().Get(d.ctx, n.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("cannot verify control plane %s is healthy before dropping the root: %w", n.Name, err)
		}
		if !nodejob.Ready(node) {
			return fmt.Errorf("control plane %s is not Ready", n.Name)
		}
	}
	if err := k8sutil.WaitHealthz(6); err != nil {
		return fmt.Errorf("local apiserver /healthz: %w", err)
	}
	return nil
}

func (d *clusterDriver) EtcdQuorumHealthy(nodes []etcdNode) error {
	ips := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ips = append(ips, n.IP)
	}
	return ha.EtcdClusterHealthy(ips)
}

func (d *clusterDriver) ConfirmClosed(n etcdNode) error {
	rejected, err := pki.EtcdRejectsRootSignedClientAt(etcdClientAddr(n.IP))
	if err != nil {
		return fmt.Errorf("could not probe etcd, so this is UNVERIFIED (not a pass): %w", err)
	}
	if !rejected {
		return fmt.Errorf("the running etcd still ACCEPTS a cluster-root-signed client certificate — " +
			"the bundle was written but the restart did not take")
	}
	return nil
}

// net.JoinHostPort, not Sprintf: a bare IPv6 address needs bracketing or the proof mis-dials.
func etcdClientAddr(ip string) string {
	return net.JoinHostPort(ip, strconv.Itoa(version.EtcdClientPort))
}

// Deliberately shell-free: nsenter execs the binary directly, nothing re-parses the argv.
func etcdNodeArgv(phase string, fetchShared, force bool) []string {
	argv := []string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		version.StratumInstallPath, "certs", "migrate-etcd-ca",
		"--phase", phase,
		"--yes",
	}
	if fetchShared {
		argv = append(argv, "--fetch-shared")
	}
	if force {
		argv = append(argv, "--force")
	}
	return argv
}

// ─── entry point ─────────────────────────────────────────────────────────────

// A CA rotation rebuilds its admin client twice mid-run, so entry cannot hand over a client.
type clientBuilder = func() (kubernetes.Interface, error)

// Production front ends inject one; bare wrappers and tests use the default.
func resolveClientBuilder(injected []clientBuilder) clientBuilder {
	if len(injected) > 0 && injected[0] != nil {
		return injected[0]
	}
	return adminClientset
}

// A var so a test can drive an orchestrator on a fake clientset.
var adminClientset = func() (kubernetes.Interface, error) {
	return k8sutil.AdminClientset()
}

// MigrateEtcdCACluster drives the etcd-CA migration across every control plane from one of them.
func MigrateEtcdCACluster(o EtcdCAOptions) error {
	// Dying between the publish and the deferred cleanup would leave the fetch key in the cluster.
	ctx, stop := interrupt.Context()
	defer stop()
	return MigrateEtcdCAClusterContext(ctx, os.Stdout, o)
}

// Runs the migration with the caller's context, without a signal handler.
func MigrateEtcdCAClusterContext(ctx context.Context, w io.Writer, o EtcdCAOptions, buildClient ...clientBuilder) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: etcd CA migration not started: %w", err)
	}
	if err := RequireNodeProfile("migrate-etcd-ca --cluster"); err != nil {
		return err
	}
	cs, err := resolveClientBuilder(buildClient)()
	if err != nil {
		return err
	}
	me, err := host.Hostname()
	if err != nil {
		return err
	}
	// Each is safe by design interleaved, but neither locks etcd's trust bundle; refuse.
	if phase := RotationPhase(); phase != "idle" {
		return fmt.Errorf("certs: refusing to migrate the etcd CA while a cluster CA rotation is in flight (%s). "+
			"Finish it (`stratum certs rotate-ca --cluster --yes`) or roll it back first", phase)
	}
	// Held from the first write through the closing verification, so nothing starts beside it.
	holder := maintenanceHolder()
	_, maintGuard, releaseMaintenance, err := acquireMaintenanceLease(ctx, cs, holder)
	if err != nil {
		return err
	}
	defer releaseMaintenance()
	// Local finalize can finish first; publishing its narrowed bundle would bypass their gates.
	active, err := unfinishedCARotation(ctx, cs)
	if err != nil {
		return fmt.Errorf("read CA rotation record before etcd-CA migration: %w", err)
	}
	if active {
		return fmt.Errorf("certs: refusing to migrate the etcd CA while a cluster CA rotation is unfinished (its record kube-system/stratum-ca-rotation is not complete). %s", unfinishedRotationExits)
	}
	if err := checkStagedKeyOwner(ctx, cs, carotateOpMigrateEtcdCA); err != nil {
		return err
	}
	// Two concurrent migrations would delete each other's fetch key and race etcd restarts.
	releaseLock, err := acquireMigrationLock(w, ctx, cs, me)
	if err != nil {
		return err
	}
	defer releaseLock()
	nodes, err := etcdMigrationNodes(ctx, cs, me)
	if err != nil {
		return err
	}
	if err := etcdMembersAreMigrationNodes(ctx, cs, nodes); err != nil {
		return err
	}
	m := &etcdMigration{
		nodes:  nodes,
		drv:    &clusterDriver{ctx: ctx, cs: cs, initiator: me, w: w},
		resume: o.Resume,
		w:      w,
		log: func(s string) {
			if !o.Quiet {
				k8sutil.Step(w, s)
			}
		},
		// A successor that took the lease mid-run cannot inherit this run's node work.
		confirmLease: func(phase string) error {
			return confirmMaintenanceLease(ctx, maintGuard, "phase "+phase)
		},
	}
	if o.Phase == pki.EtcdPhaseRollback {
		return m.rollback(o.Force)
	}
	return m.run()
}

// The holder Secret serializing concurrent migrations, beside the transient fetch key.
const etcdMigrationLockSecret = "stratum-etcdca-migration-lock"

// A holder Secret created atomically; a hard kill leaves it and the message says to remove it.
func acquireMigrationLock(w io.Writer, ctx context.Context, cs kubernetes.Interface, holder string) (func(), error) {
	// ctx can expire after the apiserver persisted the Create; the token identifies our orphan.
	token := rand.Text()
	created, err := cs.CoreV1().Secrets(caNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: etcdMigrationLockSecret, Namespace: caNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"holder": []byte(holder),
			"since":  []byte(time.Now().UTC().Format(time.RFC3339)),
			"token":  []byte(token),
		},
	}, metav1.CreateOptions{})
	switch {
	case err == nil:
		return lockReleaser(w, cs, created.UID), nil
	case apierrors.IsAlreadyExists(err):
		existing, gerr := cs.CoreV1().Secrets(caNamespace).Get(ctx, etcdMigrationLockSecret, metav1.GetOptions{})
		if gerr != nil {
			// Our Create may have committed before its response was lost; reconcile detached.
			removeOwnLock(w, cs, token)
			return nil, fmt.Errorf("acquire the etcd-CA migration lock: it already exists and its owner could not be read "+
				"(%s/%s; if no `migrate-etcd-ca --cluster` is running, remove it): %w", caNamespace, etcdMigrationLockSecret, gerr)
		}
		if string(existing.Data["token"]) == token {
			return lockReleaser(w, cs, existing.UID), nil // our own earlier attempt landed after all
		}
		return nil, fmt.Errorf("certs: another `migrate-etcd-ca --cluster` is already in progress (holder=%s since=%s). "+
			"Two concurrent etcd-CA migrations can restart different members at once and lose quorum. Wait for it to finish; "+
			"if it was killed, remove the lock Secret %s/%s and retry",
			existing.Data["holder"], existing.Data["since"], caNamespace, etcdMigrationLockSecret)
	default:
		removeOwnLock(w, cs, token)
		return nil, fmt.Errorf("acquire the etcd-CA migration lock: %w", err)
	}
}

// Deletes by UID: a name-only retry after client-go's retry could remove another migration's lock.
func lockReleaser(w io.Writer, cs kubernetes.Interface, uid types.UID) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := deleteLockOwnedBy(ctx, cs, uid); err != nil {
			fmt.Fprintf(w, "  WARN: could not release the etcd-CA migration lock %s/%s: %v — remove it before the next run\n",
				caNamespace, etcdMigrationLockSecret, err)
		}
	}
}

// An empty uid (a fake client) sends no precondition; a real API server assigns one.
func deleteSecretOwnedBy(ctx context.Context, cs kubernetes.Interface, name string, uid types.UID) error {
	opts := metav1.DeleteOptions{}
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err := cs.CoreV1().Secrets(caNamespace).Delete(ctx, name, opts)
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// deleteLockOwnedBy is deleteSecretOwnedBy for the migration lock.
func deleteLockOwnedBy(ctx context.Context, cs kubernetes.Interface, uid types.UID) error {
	return deleteSecretOwnedBy(ctx, cs, etcdMigrationLockSecret, uid)
}

// Deletes only a lock carrying this attempt's token; anything else belongs to someone else.
func removeOwnLock(w io.Writer, cs kubernetes.Interface, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, etcdMigrationLockSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return // nothing was stored, so nothing is left behind
	case err != nil:
		// The Create may have stored the lock; say so, since it would block every later migration.
		fmt.Fprintf(w, "  WARN: could not check whether a failed attempt to take the etcd-CA migration lock left %s/%s behind (%v); "+
			"if no `migrate-etcd-ca --cluster` is running, remove it with `kubectl -n %s delete secret %s`\n",
			caNamespace, etcdMigrationLockSecret, err, caNamespace, etcdMigrationLockSecret)
		return
	case string(sec.Data["token"]) != token:
		return
	}
	if err := deleteLockOwnedBy(ctx, cs, sec.UID); err != nil {
		fmt.Fprintf(w, "  WARN: a failed attempt to take the etcd-CA migration lock left %s/%s behind and it could not be removed: %v — remove it before the next run\n",
			caNamespace, etcdMigrationLockSecret, err)
	}
}

// Workers are excluded by construction: they hold no etcd leaf and receive no etcd CA.
func etcdMigrationNodes(ctx context.Context, cs kubernetes.Interface, me string) ([]etcdNode, error) {
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var initiator *etcdNode
	var others []etcdNode
	for i := range nl.Items {
		n := &nl.Items[i]
		if n.Labels[k8sutil.ControlPlaneLabel] != "true" {
			continue
		}
		ip := nodeInternalIP(n)
		if ip == "" {
			return nil, fmt.Errorf("control plane %s has no InternalIP, so its etcd cannot be surveyed", n.Name)
		}
		if n.Name == me {
			initiator = &etcdNode{Name: n.Name, IP: ip}
			continue
		}
		others = append(others, etcdNode{Name: n.Name, IP: ip})
	}
	if initiator == nil {
		return nil, fmt.Errorf("certs: run `migrate-etcd-ca --cluster` from a control-plane node (this node %q is not labelled %s)",
			me, k8sutil.ControlPlaneLabel)
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	return append([]etcdNode{*initiator}, others...), nil
}

// A var so a test can stand in a membership.
var etcdMemberIPs = ha.MemberHostsVia

// A controller whose label never landed is still a voting member; refusing leaves nothing behind.
func etcdMembersAreMigrationNodes(ctx context.Context, cs kubernetes.Interface, nodes []etcdNode) error {
	ips := make([]string, 0, len(nodes))
	listed := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		ips = append(ips, n.IP)
		listed[n.IP] = true
	}
	members, err := etcdMemberIPs(ips)
	if err != nil {
		return fmt.Errorf("certs: read etcd's member list to confirm every member is a control plane this run will migrate: %w", err)
	}
	var missing []string
	for _, ip := range members {
		if !listed[ip] {
			missing = append(missing, ip)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := map[string]string{}
	if nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for i := range nl.Items {
			if ip := nodeInternalIP(&nl.Items[i]); ip != "" {
				names[ip] = nl.Items[i].Name
			}
		}
	}
	var described, fixes []string
	for _, ip := range missing {
		name, ok := names[ip]
		if !ok {
			described = append(described, ip+" (no Kubernetes node has this address)")
			continue
		}
		described = append(described, fmt.Sprintf("%s (node %s)", ip, name))
		fixes = append(fixes, fmt.Sprintf("stratum kubectl label node %s %s=true --overwrite", name, k8sutil.ControlPlaneLabel))
	}
	msg := fmt.Sprintf("certs: refusing to migrate the etcd CA: etcd has a member at %s that is not labelled %s, "+
		"so this run would change every other member's etcd trust and leave it behind, rejected by its peers and out of raft",
		strings.Join(described, ", "), k8sutil.ControlPlaneLabel)
	if len(fixes) > 0 {
		msg += ". Label it and run this again: " + strings.Join(fixes, "; ")
	}
	return errors.New(msg)
}

func nodeInternalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}
