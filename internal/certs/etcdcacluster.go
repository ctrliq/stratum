// Cross-node orchestration for `stratum certs migrate-etcd-ca --cluster`
// walks trust-both → issue-new → drop-root across every control
// plane PHASE-MAJOR — every node completes phase N before any node starts phase
// N+1 — which is what stops a member presenting a leaf its peers do not yet
// trust and losing quorum. internal/pki/etcdca.go states the requirement.
//
// Same mechanism as `stratum certs rotate-ca --cluster` and `stratum upgrade
// --cluster`: a privileged, node-pinned Job that nsenters PID 1 and runs the
// on-host `stratum` binary. It does NOT drain or cordon — no workload is
// disrupted — and it touches control planes only, because a worker holds no etcd
// leaf and no etcd CA. The etcd CA is minted once on the initiator and
// distributed to the other control planes over the existing encrypted
// stratum-certs Secret, keyed by the transient stratum-carotate Secret.
//
// A cluster-scoped custom resource carrying a command would grant root on
// every node, so the per-node Job is the house pattern; this reuses it rather
// than introducing a CR.
//
// # Where the migration state lives
//
// Nowhere. There is no journal, no CR, no ConfigMap and no phase counter. State
// is re-derived at the start of every run, and again before the gate, by asking
// each node's RUNNING etcd what it trusts (pki.ProbeEtcdTrust). A second store
// could disagree with what etcd actually enforces, and the disagreement that
// matters — a bundle written whose restart never took — is precisely the one a
// counter would record as success.
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

// etcdNode is one control plane in the migration. IP is the address its etcd
// client port listens on, which is what the survey dials.
type etcdNode struct {
	Name string
	IP   string
}

// etcdDriver is the seam between the phase engine and the cluster. The
// production implementation runs phases through node Jobs and surveys through
// TLS handshakes; tests supply one backed by real per-node PKI directories, so
// ordering and refusal are asserted against real trust bundles rather than
// against a stub that always agrees.
//
// Same reason restartControlPlaneGuarded takes its pod operations as parameters:
// the interesting logic here is sequencing and refusal, and none of it should
// need a cluster to exercise.
type etcdDriver interface {
	// Survey reports the migration state of the RUNNING etcd on n, plus the
	// reason when that state is unknown. It must return EtcdStateUnknown rather
	// than guess. The detail is not decoration: every refusal in this file can
	// fire on "unknown", and "cp3 is unreachable", "cp3 is TLS 1.3-only" and
	// "cp3 served a certificate from a CA I cannot place" need different
	// responses from the operator being told to fix it. transient marks the one
	// failure kind that a re-probe can resolve — etcd not serving yet, the
	// just-restarted member — and only that; a definite unknown stays definite.
	Survey(n etcdNode) (state, detail string, transient bool)
	// RunPhase applies one phase on n, including its restart.
	//
	// force reaches the node, not just this orchestrator: on the rollback phase
	// it selects pki.MigrateEtcdRollbackFromMigrated over the guarded
	// MigrateEtcdRollback. Passing it only to the cluster-level gate would let
	// --force past this layer and then be refused by every node — making the
	// escape hatch that three pki refusals name unreachable.
	RunPhase(n etcdNode, phase string, fetchShared, force bool) error
	// PublishEtcdCA makes the initiator's etcd CA fetchable by the followers,
	// returning a cleanup to run when the phase that needs it ends.
	PublishEtcdCA() (func(), error)
	// ReapStaleFetchKey removes a fetch key a previously killed run left behind.
	ReapStaleFetchKey()
	// Interrupted reports that the operator asked the run to stop. Checked
	// between nodes so a cancellation lands on a phase boundary, where halting
	// is safe, rather than mid-restart.
	Interrupted() error
	// ClusterHealthy is the non-etcd half of the drop-root gate: every control
	// plane Ready, the local apiserver serving.
	ClusterHealthy(nodes []etcdNode) error
	// EtcdQuorumHealthy proves the members agree, not merely that each is up.
	EtcdQuorumHealthy(nodes []etcdNode) error
	// ConfirmClosed is the process-derived proof the etcd trust hole (etcd
	// accepting any client certificate signed by the cluster root, which also
	// signs every kubelet's certificate) is shut on n.
	ConfirmClosed(n etcdNode) error
}

// etcdMigration is the phase engine. It holds no Kubernetes client and performs
// no I/O of its own.
type etcdMigration struct {
	nodes  []etcdNode // initiator first; the rest in a stable order
	drv    etcdDriver
	resume bool
	log    func(string)
	// w is the run's writer: the per-node phase lines write through it.
	w       io.Writer
	details map[string]string // node -> why its state could not be determined
	// confirmLease, when set, renews the shared maintenance Lease before each
	// phase of the run and reports a takeover: the fence the migration must
	// keep checking mid-run, the way the upgrade and a CA rotation do. A run
	// whose lease was taken by a successor stops before dispatching that
	// phase's node work. Nil in tests that drive the phases directly.
	confirmLease func(phase string) error
	// sleep and surveyBudget make the transient-unknown retry instant in
	// tests. sleep nil means time.Sleep; surveyBudget 0 means
	// surveyRetryBudget.
	sleep        func(time.Duration)
	surveyBudget time.Duration
	// stillStarting names, from the most recent survey, the members still
	// unknown after the wait and only ever the transient not-serving-yet kind
	// — the only unknowns plan() may attach a start-up remedy to.
	stillStarting []string
}

func (m *etcdMigration) step(format string, a ...any) {
	if m.log != nil {
		m.log(fmt.Sprintf(format, a...))
	}
}

// surveyRetryBudget bounds how long survey() keeps re-probing a member whose
// etcd has not started serving: long enough to cover a just-rebooted control
// plane's WAL replay and rejoin, short enough that an operator is not left
// staring at a silent command.
const surveyRetryBudget = 90 * time.Second

// surveyBackoff is the wait before re-probe attempt N (counting from 0): 2s,
// 4s, 8s, then 15s capped.
func surveyBackoff(attempt int) time.Duration {
	d := 2 * time.Second << attempt
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

// survey asks every node's running etcd where it is, recording why for any
// node that could not answer.
//
// A member whose etcd did not answer because it is NOT SERVING YET — the
// listener up, WAL replay still running, not back in raft — is the one
// unknown a run may wait out: it re-probes only those, backing off within
// surveyRetryBudget, and only their recovery lets the run see a real state.
// Every other unknown (impostor listener, foreign issuer, unnameable anchor)
// is definite, is never retried, and still refuses exactly as before. The
// error is a lost maintenance lease or an interrupt, found during the wait;
// it is returned rather than left to read as an etcd that never started
// serving.
func (m *etcdMigration) survey() (map[string]string, error) {
	states := make(map[string]string, len(m.nodes))
	m.details = make(map[string]string, len(m.nodes))

	// pending holds only the unknown-AND-transient members; everything else
	// is settled on the first pass.
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
	// An interrupted run stops re-probing, and starts no more network work, at
	// either end of the wait. It reports the interrupt, naming the members it
	// was waiting on, rather than a refusal whose remedy points at their etcd:
	// nothing has been dispatched yet, so this is a phase boundary.
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
			// The budget exceeds the maintenance lease's renewal window, so
			// the run proves mid-wait that no successor has taken it. A failed
			// confirm stops the run with that reason: the operator must look
			// at who holds the lease, not at the member that was starting.
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
	// The lease may also have been lost, or an interrupt arrived, during the
	// last wait or probe, which no check inside the loop follows. A run that
	// waited checks both once more, so a member still unknown is not reported
	// in place of either.
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
	// Whatever is left after the budget was spent is still unknown AND was
	// only ever the not-serving-yet kind: plan() can name what that means.
	m.stillStarting = nil
	for _, n := range pending {
		m.stillStarting = append(m.stillStarting, n.Name)
	}
	return states, nil
}

// describeSurvey renders a survey as a stable, operator-readable table, with
// the reason appended for any node that could not be classified. Every refusal
// carries one: "refuses to proceed rather than guessing" is only useful if the
// operator is handed what it saw, and "unknown" on its own is not that.
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

// etcdStateOrder ranks the forward migration states so "adjacent" is well
// defined. A cleanly-halted phase-major run leaves the cluster spanning at most
// two ADJACENT ranks (the phase that was completing); anything wider is a cluster
// that is already split, not one that is paused. unknown is deliberately absent —
// it is refused before this map is ever consulted.
var etcdStateOrder = map[string]int{
	pki.EtcdStateNotMigrated: 0,
	pki.EtcdStateTrustBoth:   1,
	pki.EtcdStateIssueNew:    2,
	pki.EtcdStateMigrated:    3,
}

// phaseAppliedAt is the surveyed-state rank at (or past) which a forward phase is
// already a no-op on a node, so a resumed run can label an already-applied node
// on the console instead of printing it identically to one it actually reworked.
var phaseAppliedAt = map[string]int{
	pki.EtcdPhaseTrustBoth: 1,
	pki.EtcdPhaseIssueNew:  2,
	pki.EtcdPhaseDropRoot:  3,
}

// errNothingToDo reports a cluster that is already fully migrated. errors.New
// and errors.Is rather than fmt.Errorf and ==, the repo's sentinel-error
// convention: a later `fmt.Errorf("...: %w", errNothingToDo)` in plan would
// otherwise silently stop matching, turning "already migrated, verify it" into
// a hard failure of the command.
var errNothingToDo = errors.New("already migrated")

// plan decides whether to proceed, and refuses rather than guessing.
//
// A cluster whose control planes are at DIFFERENT phases is not resumable
// without a decision only an operator can make: it may be a run that halted
// cleanly, or it may be a node that is broken. Those need opposite responses and
// nothing here can tell them apart, so the default is to refuse and print what
// was seen. --resume is the operator saying they have read the table.
//
// --resume does NOT extend to unknown. pki's EtcdStateUnknown "is never a
// resting state and no guard may treat it as safe" (internal/pki/etcdca.go,
// where the constant is declared), and a flag that converted unknown into a
// proceed would be a guard relaxation wearing an ergonomics costume — the node
// it waves through is the one whose etcd nobody could reach.
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
		// --resume is the operator saying they read the table — but it may only
		// continue a HALTED run, which spans at most two adjacent phases. A wider
		// spread (e.g. one member migrated while another never started) is an
		// already-split cluster, not a paused one: walking phases forward across it
		// restarts etcd on a broken cluster instead of failing loud. Refuse it and
		// point at --rollback.
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

// withStartupRemedy is refusal, a refusal over unknown members, with the
// start-up steps for those the last survey left still starting: a member
// still unknown after the wait, and never anything but the not-serving-yet
// kind, is a power-cycled node whose etcd has not come back, which has
// concrete steps, given for it even when another member's unknown is
// definite. A definite unknown has none.
func (m *etcdMigration) withStartupRemedy(refusal error) error {
	if len(m.stillStarting) == 0 {
		return refusal
	}
	return fmt.Errorf("%w\n%s", refusal, m.startupRemedy(m.stillStarting))
}

// startupRemedy turns a still-unknown-but-transient member into the steps an
// operator can actually take, split by how the probe failed: a handshake that
// was accepted but never answered means etcd is starting (or stuck); a refused
// connection means it never bound the port at all.
func (m *etcdMigration) startupRemedy(unknown []string) string {
	var stalled, notListening []string
	for _, name := range unknown {
		// Matched against pki's own sub-cause wording: the two failures have
		// opposite remedies, and the detail is what the probe reported.
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
		// The lease first, before the preflight and the phase's node work: a
		// run that has lost the maintenance lease must dispatch nothing more.
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

// runPhaseWithSharedMaterial publishes the etcd CA for exactly as long as a
// follower might fetch it, then removes it.
//
// The staged key decrypts the stratum-certs blob, which carries ca.key, sa.key,
// front-proxy-ca.key and etcd-ca.key — every private key the cluster has. Only
// trust-both fetches it, and only on followers, so the earlier shape (publish
// before the loop, remove on a defer at the end of the run) left it readable in
// kube-system across every phase, every etcd restart and every node-Ready wait
// on the way. That is a long window on a command whose whole purpose is to
// narrow who can read cluster state, and the operator reflex that ends it —
// Ctrl-C during an etcd restart — is the one that strands it.
func (m *etcdMigration) runPhaseWithSharedMaterial(phase string, states map[string]string) error {
	// A single control plane has no follower to distribute to, so publishing
	// would put every cluster private key into an un-owned Secret for nothing.
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

// runPhase applies one phase to every control plane, initiator first, serially.
//
// Serial, never parallel: two control planes restarting etcd at once is a lost
// quorum on a three-node cluster. Halting on the first failure is deliberate and
// leaves a WORKING cluster in every case, because the phase order guarantees it
// — mid-trust-both every leaf is still root-signed and every node still trusts
// the root; mid-issue-new every node completed trust-both so every node trusts
// both anchors; mid-drop-root every leaf is already etcd-CA-signed. The only
// unsafe interleaving is issue-new starting somewhere before trust-both finished
// everywhere, which is what phase-major ordering exists to prevent.
func (m *etcdMigration) runPhase(phase string, states map[string]string) error {
	for i, n := range m.nodes {
		if err := m.drv.Interrupted(); err != nil {
			return fmt.Errorf("%s halted before %s: %w (the cluster is left in the last state every node completed, which is a working one)", phase, n.Name, err)
		}
		// Only the initiator may mint the etcd CA; every follower must receive
		// it. fetchShared is what tells the node which it is.
		fetchShared := i > 0 && phase == pki.EtcdPhaseTrustBoth
		// Distinguish a real apply from an already-applied no-op on the console.
		// The per-node skip decision lives inside RunPhase (EtcdPhaseApplied) and is
		// otherwise invisible in a --cluster run — the initiator runs Quiet and a
		// follower's Job log is discarded — so a resumed run would print identically
		// to a fresh one, matching the rollback path which already labels its skips.
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
		// Every phase restarts etcd, and serialising the restarts is not the
		// same as waiting for one to rejoin raft. What the phase waited on is a
		// TCP dial to the client port (staticpod.Active) and, for a follower,
		// the node's kubelet Ready condition — neither of which moves when a
		// member is listening but not yet in quorum. Restarting the next member
		// in that window is the quorum loss this whole file is arranged to
		// prevent, so prove convergence before touching another node.
		if err := m.drv.EtcdQuorumHealthy(m.nodes); err != nil {
			return fmt.Errorf("control plane %s completed %s but the cluster did not re-converge: %w "+
				"(halting before restarting another member; the cluster is left in a working state)", n.Name, phase, err)
		}
	}
	return nil
}

// gateDropRoot is the cluster-wide precondition for the only phase that closes
// the etcd trust hole and the only one that cannot be walked back node-by-node.
//
// Dropping the root is safe exactly when every member already presents an
// etcd-CA-signed leaf. A node still at trust-both would be locked out of raft
// the moment its peers stopped trusting the root, and the local guard inside
// MigrateEtcdDropRoot cannot see it — it only knows about the node it runs on.
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

// confirmClosed is the run's closing assertion, and the only process-derived
// proof that the vulnerability is shut: a cluster-root-signed client
// certificate must be REJECTED by each member's running etcd. A bundle on disk
// proves intent; this proves the restart took.
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

// rollback returns every control plane to the cluster root.
//
// # Why this is two passes when --force is set, and one when it is not
//
// The forward phases are safe to halt because trust always widens EVERYWHERE
// before any leaf moves, so at every intermediate point every member still
// verifies every other. Un-migrating has to reproduce that property, and the
// naive shape does not.
//
// From an intermediate state (trust-both / issue-new) every node's bundle
// already carries etcd-ca ‖ root, so re-deriving one node's leaves under the
// root produces a certificate its peers already accept. One pass, safe to halt.
//
// From MIGRATED it is the opposite: every peer enforces the etcd CA alone, so
// the first node whose leaf moves is rejected by all of them and leaves raft
// immediately — before anything has told the others to follow. Halting there
// splits the cluster, and on a two-member cluster it loses quorum outright,
// which also strands the run: the initiator dispatches the remaining nodes
// through the Kubernetes API, and its own apiserver talks to its own
// now-evicted etcd.
//
// So --force widens first, everywhere (pass 1, no leaf moves, every member
// still presenting the etcd-CA-signed certificate everyone still trusts), and
// only then re-derives leaves node by node (pass 2, every member now trusting
// the root). Both passes are halt-safe for the same reason the forward phases
// are, and TestForcedRollbackIsHaltSafeAtEveryStep measures it the same way.
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

	// Pass 1, only when a node has actually dropped the root. Skipped entirely
	// for an intermediate-state rollback, where every bundle already carries it.
	if len(migrated) > 0 {
		m.step("Un-migration 1/2: re-add the cluster root to etcd's trust on every control plane (no certificate moves)")
		// The lease first: a rollback that has lost the maintenance lease must
		// dispatch nothing more, the same as the forward run.
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
		// An indeterminate node is never treated as having adopted the wider trust.
		// A node that went unknown DURING pass 1 (its etcd flapped on the re-add-root
		// restart) is not "still migrated", so the check below would miss it — and
		// pass 2's `needs` predicate (state != not-migrated) would then dispatch a
		// leaf move to it while its trust is unproven, the exact raft eviction pass 1
		// exists to prevent. Refuse it here, symmetric with the initial survey.
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
		// A node that never received the etcd CA has nothing to undo, and pki
		// refuses on it ("nothing to roll back"). That is exactly the shape a
		// halted forward run leaves — and the halt message tells the operator to
		// run this command — so dispatching to it would make the recovery path
		// fail on the wreckage it exists to clear.
		return state != pki.EtcdStateNotMigrated
	}); err != nil {
		return err
	}

	// Verify, for the same reason the forward path does: this wrote bundles and
	// restarted etcd on every control plane, and a bundle on disk proves intent,
	// not that the restart took.
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

// rollbackPass applies one un-migration pass across every control plane that
// needs it, serially.
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
		// Deliberately NO per-node quorum probe, unlike the forward path. Once
		// pass 2 has moved a leaf, ha's etcd client authenticates with this
		// node's apiserver-etcd-client certificate — which pass 2 has just
		// re-signed under the root — so the probe would be testing its own
		// credentials rather than the cluster. Pass 1 is safe by construction
		// (nothing moves), and the survey between the passes is what proves the
		// widening actually took on every member.
	}
	return nil
}

// ─── production driver ───────────────────────────────────────────────────────

const etcdMigrateJobPrefix = "stratum-etcdca"

// clusterDriver runs phases through node Jobs and surveys through TLS
// handshakes against each member's etcd.
type clusterDriver struct {
	ctx       context.Context
	cs        kubernetes.Interface
	initiator string
	// w is where the driver's recovery warnings go: the writer the run was
	// handed, never the process's own stdout.
	w io.Writer
}

// probeEtcdTrust is pki.ProbeEtcdTrust; a var so a test can point the real
// driver at a listener of its own.
var probeEtcdTrust = pki.ProbeEtcdTrust

// Survey asks the RUNNING etcd, not the node's disk, and there is deliberately
// no second opinion: the failure this gate exists to catch is a file that says
// one thing while the process does another, so a disk-derived fallback would be
// the very reading that must not be trusted. A member that cannot answer is
// unknown, and unknown refuses — including under --resume.
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

// PublishEtcdCA stages the key that lets followers decrypt the shared material.
//
// That key decrypts ca.key, sa.key, front-proxy-ca.key and etcd-ca.key — every
// private key the cluster has — so its lifetime is the sharpest edge in this
// command. Three things bound it, and one thing cannot be:
//
//   - The window is one PHASE, not the run: runPhaseWithSharedMaterial brackets
//     trust-both alone, and skips publishing entirely when there is no follower.
//   - The cleanup runs on a context that is NOT the run's, so cancelling the run
//     (Ctrl-C) is what triggers removal rather than what prevents it — deleting
//     through an already-cancelled context fails on the first call.
//   - A failure to stage after the Secret exists removes it before returning,
//     rather than leaving a key nothing will ever come back for.
//
// What is NOT covered: SIGKILL, a panic in the runtime, or power loss. Nothing
// in-process can be, and Kubernetes has no TTL for a Secret. `rotate-ca` shares
// this helper and this exposure. A run that starts while a stale key Secret is
// present reaps it (reapStaleFetchKey), so the practical bound is "until the
// next invocation" rather than "indefinitely" — but an operator who is never
// coming back is why this is also stated in the docs.
func (d *clusterDriver) PublishEtcdCA() (func(), error) {
	if err := checkStagedKeyOwner(d.ctx, d.cs, carotateOpMigrateEtcdCA); err != nil {
		return nil, err
	}
	certKey, err := ha.NewCertKey()
	if err != nil {
		return nil, err
	}
	// ReUploadCerts calls pki.EnsureEtcdTrustBundle first, so the etcd CA exists
	// to publish even on a control plane installed before it did.
	if err := uploadStagedMaterial(certKey); err != nil {
		return nil, fmt.Errorf("publish shared material: %w", err)
	}
	if err := putStagedKey(d.ctx, d.cs, certKey, migrationStagedKeyOwner(certKey)); err != nil {
		// The Create may well have reached the apiserver and had its response
		// lost, so the Secret can exist even though this failed. Do not return
		// leaving a key behind that no deferred cleanup was ever registered for.
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

// cleanupContext is deliberately independent of the run's context.
//
// Removing the key is the one action that MUST still happen when the run is
// being cancelled — that is precisely the case the operator's Ctrl-C creates.
// Passing the cancelled run context here would make every delete attempt fail
// with "context canceled" and burn all five retries doing it.
func cleanupContext() (context.Context, context.CancelFunc) {
	// The caller is synchronous and must defer the cancel: the budget outlives
	// cleanupCarotateKey's own retry schedule, so dropping the cancel would leak
	// the timer until the full budget elapses (and would suppress go vet's
	// lostcancel check on the way).
	return context.WithTimeout(context.Background(), cleanupBudget)
}

// cleanupBudget bounds the detached cleanup: the apiserver health gate
// (waitAPIServer, at most half of it), then five attempts five seconds apart
// plus room for the calls themselves.
const cleanupBudget = 3 * time.Minute

// reapStaleFetchKey removes a fetch-key Secret left behind by a previous run
// that was killed outright. Called before publishing, so the window a hard kill
// can open is bounded by the next invocation rather than left open forever.
//
// Deliberately not fatal: a cluster that will not let us delete it is a cluster
// we should still be able to migrate, and the warning says what is left behind.
func (d *clusterDriver) Interrupted() error { return d.ctx.Err() }

func (d *clusterDriver) ReapStaleFetchKey() {
	sec, err := d.cs.CoreV1().Secrets(caNamespace).Get(d.ctx, caRotateKeySecret, metav1.GetOptions{})
	if err != nil {
		return // absent (or unreadable, which publishing will surface anyway)
	}
	// Followers of another operation may still need its retained key. Only our
	// operation's leftover (or an unannotated legacy key) is ours to reap.
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
	// The CONTROL PLANES, not every node. Only they hold an etcd leaf, and
	// gating on the whole cluster would let one NotReady worker — which owns no
	// material this migration touches — block drop-root and strand the cluster
	// at issue-new, where the vulnerability is still open.
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

// etcdClientAddr is the "host:port" the final verification dials.
//
// net.JoinHostPort, not fmt.Sprintf("%s:%d"): a bare IPv6 control-plane address
// needs bracketing, and without it the closing proof that the etcd trust hole is shut would
// fail to connect on every dual-stack cluster — reporting the migration
// unverified on a cluster it had just correctly migrated.
func etcdClientAddr(ip string) string {
	return net.JoinHostPort(ip, strconv.Itoa(version.EtcdClientPort))
}

// etcdNodeArgv builds the exact argv the per-node Job runs.
//
// Deliberately shell-free, for the same reason caNodeArgv and nodeUpgradeArgv
// are: nsenter execs the stratum binary directly, so every value below
// is a discrete argv element that no interpreter ever re-parses. An earlier
// version built one string and handed it to `/bin/bash -c`, which made the
// phase executable text on every node in the cluster, as root, in the host
// namespaces.
//
// Keep it that way. This payload has no pipes, redirections, globs or variable
// expansions, so it has never needed a shell.
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

// clientBuilder builds the Kubernetes client a --cluster orchestrator runs on.
// The orchestrators take a builder rather than a client because a CA rotation
// rebuilds its admin client twice mid-run — the admin certificate is reissued
// after trust-new and again after sign-new, and the client the run began with
// stops authenticating once backends promote and finalize drops the old CA —
// so a client handed at entry cannot cover the run. Every acquisition point,
// the seam at entry and both rebuilds, serves from the one builder the caller
// injected: a rebuild must not silently fall back to admin.conf behind the
// credential decision the caller made.
type clientBuilder = func() (kubernetes.Interface, error)

// resolveClientBuilder is the injected builder, or the package's admin.conf
// builder when a caller passed none. Production front ends — the CLI's certs
// commands and the MCP tools — always inject one; the bare wrappers
// (RotateCACluster, MigrateEtcdCACluster) and the tests driving them use the
// default.
func resolveClientBuilder(injected []clientBuilder) clientBuilder {
	if len(injected) > 0 && injected[0] != nil {
		return injected[0]
	}
	return adminClientset
}

// adminClientset is the default client builder: exactly k8sutil.AdminClientset,
// the admin.conf client the --cluster orchestrators build themselves.
// It serves the callers that inject no builder of their own; a test replaces it
// (fakeAdminClientset) to drive an orchestrator on a fake clientset instead of
// admin.conf.
var adminClientset = func() (kubernetes.Interface, error) {
	return k8sutil.AdminClientset()
}

// MigrateEtcdCACluster drives the etcd-CA migration across every control plane
// from one of them.
func MigrateEtcdCACluster(o EtcdCAOptions) error {
	// Cancel on interrupt rather than dying on it. This run stages a Secret
	// holding the key to every cluster private key, and the operator reflex
	// during a long etcd restart is Ctrl-C — which, on a plain Background
	// context, kills the process between the publish and the deferred cleanup
	// and leaves that key in the cluster. Cancelling instead lets the phase
	// return, the defer fire, and the detached cleanup context do the delete.
	//
	// The second signal is left to the default handler: an operator who insists
	// gets the immediate kill, and the next run reaps what it leaves.
	ctx, stop := interrupt.Context()
	defer stop()
	return MigrateEtcdCAClusterContext(ctx, os.Stdout, o)
}

// MigrateEtcdCAClusterContext runs the migration with the caller's context,
// without installing a signal handler or detaching from parent cancellation.
//
// buildClient is the client builder the run acquires its one API client
// through (see clientBuilder); callers that pass none get the package's
// admin.conf builder. The front ends inject their own, so the credential
// decision stays with the caller.
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
	// A CA rotation and an etcd-CA migration both rewrite etcd's trust bundle.
	// Interleaved they are individually safe by design — syncEtcdBundleToRoot
	// preserves the bundle's WIDTH and refreshes its CONTENTS precisely so a root
	// rotation can happen mid-migration — but neither takes a lock, and two
	// orchestrators racing the same file is not something either was designed
	// for. Refuse rather than find out.
	if phase := RotationPhase(); phase != "idle" {
		return fmt.Errorf("certs: refusing to migrate the etcd CA while a cluster CA rotation is in flight (%s). "+
			"Finish it (`stratum certs rotate-ca --cluster --yes`) or roll it back first", phase)
	}
	// One maintenance operation at a time, cluster-wide: take the shared
	// maintenance Lease before the run's first act on the cluster — the
	// migration lock Secret below is its first write and the per-node Jobs
	// follow the survey — and hold it through the final readiness waits and
	// the closing verification, so an upgrade or a rotate-ca --cluster cannot
	// start beside this migration. Released on every exit, an interrupt
	// included: the run's context cancels, the step returns, the defers fire.
	// This run never rebuilds its admin client, so the lease rides one client
	// for its whole life and the rider needs no swap.
	holder := maintenanceHolder()
	_, maintGuard, releaseMaintenance, err := acquireMaintenanceLease(ctx, cs, holder)
	if err != nil {
		return err
	}
	defer releaseMaintenance()
	// Local finalize can finish before the other control planes. Publishing
	// its narrowed root bundle now would bypass their remaining finalize gates.
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
	// Serialize against a SECOND concurrent migrate-etcd-ca run. The RotationPhase
	// guard above only covers rotate-ca; nothing else stops two operators (or an
	// automation retry plus a human) both surveying and dispatching per-node Jobs.
	// Two migrations restarting etcd on different members at once is the quorum loss
	// the serial phase-major design prevents within a single run, and run B's
	// ReapStaleFetchKey would delete run A's live fetch key. An atomic create-if-
	// absent lock Secret makes the second run refuse.
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
		// The guard's Confirm before each phase renews the lease against this
		// run's client (the rider needs no swap: the migration never rebuilds
		// its admin client) and stops the run on a takeover, so a successor
		// that took the lease mid-run cannot inherit this run's node work.
		confirmLease: func(phase string) error {
			return confirmMaintenanceLease(ctx, maintGuard, "phase "+phase)
		},
	}
	if o.Phase == pki.EtcdPhaseRollback {
		return m.rollback(o.Force)
	}
	return m.run()
}

// etcdMigrationLockSecret is the holder Secret that serializes concurrent
// migrations. It lives beside the transient fetch key in caNamespace.
const etcdMigrationLockSecret = "stratum-etcdca-migration-lock"

// acquireMigrationLock takes a cluster-wide advisory lock so two concurrent
// `migrate-etcd-ca --cluster` runs cannot both dispatch overlapping node Jobs.
// It is a holder Secret created with an atomic create-if-absent (Create returns
// AlreadyExists if one is already present), carrying the holder + start time for
// diagnostics. The returned closer removes it on exit using a detached context,
// so an operator's Ctrl-C still releases it; a hard kill leaves it behind, and
// the message tells the operator to remove it — the same "bounded by the next
// run" contract the fetch key has (Kubernetes has no Secret TTL).
func acquireMigrationLock(w io.Writer, ctx context.Context, cs kubernetes.Interface, holder string) (func(), error) {
	// The token identifies THIS attempt. ctx can expire after the API server has
	// persisted the Secret but before the response arrives; Create then returns an
	// error with no release closure, and the lock would block every later
	// migration. The token lets us tell our own orphan from someone else's lock.
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
			// Whose lock this is cannot be read, and ctx may be the reason. A
			// Create of ours that committed before its response was lost shows up
			// as exactly this, so reconcile detached and return the failure
			// rather than proceed as the holder.
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

// lockReleaser returns the closer that removes the lock this run owns. It deletes
// by UID, never by name alone: client-go retries a DELETE whose response was a
// 429 or 5xx, and if the first attempt already freed the name and another
// migration took it, a name-only retry would remove THAT migration's lock.
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

// deleteSecretOwnedBy deletes the named Secret in caNamespace with a UID
// precondition. Gone, or replaced by someone else's (a precondition conflict),
// both mean there is nothing of ours left to delete. An empty uid (a fake client)
// sends no precondition; a real API server always assigns one.
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

// removeOwnLock removes the lock Secret if, and only if, it carries token: a
// lock this attempt stored whose Create then failed. It runs detached from the
// caller's context, which has typically just expired, under its own short
// deadline, and deletes by the UID of the object it verified. A lock with any
// other token, or none, belongs to someone else and is left alone.
func removeOwnLock(w io.Writer, cs kubernetes.Interface, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, etcdMigrationLockSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return // nothing was stored, so nothing is left behind
	case err != nil:
		// The Create may have stored the lock, and we cannot tell. Say so: it would
		// block every later migration until someone removes it.
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

// etcdMigrationNodes lists the control planes, initiator first. Workers are
// excluded by construction: they hold no etcd leaf, receive no etcd CA
// (internal/ha/certs.go keeps it in the control-plane-only shared set), and have
// nothing to migrate.
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

// etcdMemberIPs lists one host per etcd member, read through the control
// planes' own etcd, a member not yet started included; a var so a test can
// stand in a membership.
var etcdMemberIPs = ha.MemberHostsVia

// etcdMembersAreMigrationNodes refuses a run that would leave an etcd member
// out. The node list comes from the control-plane label, and a controller
// whose label never landed is still a voting etcd member: the run would
// migrate or roll back every other member, survey only them, report success,
// and leave that one enforcing the old trust, rejected by its peers and out
// of raft. So every member etcd reports must be a node in the list, read
// before any phase runs.
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
