// Package nodejob runs a privileged, node-pinned Job that nsenters PID 1 and
// execs the on-host stratum binary, then gates on the Job result. It is the one
// implementation of a mechanism that internal/upgrade (rolling upgrades) and
// internal/certs (cluster CA rotation) both drive, so the two drivers cannot
// diverge. Every change to the privileged-Job runner — stale-Job
// guard, image pin, security-context tightening — lives here once.
//
// The Job pod is deliberately hostPID/hostNetwork + privileged and runs a
// shell-free argv (nsenter execs the binary directly; no /bin/bash -c), so no
// interpreter ever re-parses a caller value.
package nodejob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	batchtyped "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/utils/ptr"

	"github.com/ctrliq/stratum/internal/version"
)

// pollInterval is how often Run/WaitReady re-check. A var, not a const, so tests
// can zero it; production never reassigns it.
var pollInterval = 5 * time.Second

// getCallTimeout bounds each poll read, so a request the API server accepts
// and never answers (its restart) costs one poll, not the run.
var getCallTimeout = 10 * time.Second

// readTimeout is a poll read's bound: getCallTimeout, but never longer than
// pollInterval. A poll sleeps only what is left of its interval after the
// read, so a read held to the interval keeps every poll at pollInterval and the
// whole loop at PollAttempts * pollInterval, the Job's own deadline.
func readTimeout() time.Duration {
	if pollInterval > 0 && pollInterval < getCallTimeout {
		return pollInterval
	}
	return getCallTimeout
}

// maxNameLen is the Kubernetes object-name limit; TruncName fits Job names to it.
const maxNameLen = 63

// Spec describes one privileged node Job. Namespace/Name/Node/LabelKey/Container/
// Argv define the pod; ActiveDeadline and PollAttempts bound it; Kind names it
// in the error messages so each caller keeps its own wording.
type Spec struct {
	// Output receives cleanup warnings. Nil preserves stdout for standalone
	// callers; operations serving MCP supply their own progress writer.
	Output         io.Writer
	Namespace      string
	Name           string   // Job name; use TruncName to respect the 63-char limit
	Node           string   // pod NodeName, and the value of the LabelKey label
	LabelKey       string   // label whose value is Node (e.g. stratum.ciq.com/upgrade)
	Container      string   // container name
	Argv           []string // full shell-free command: nsenter … -- <binary> …
	ActiveDeadline time.Duration
	PollAttempts   int    // completion polls, one every pollInterval
	Kind           string // noun for error messages, e.g. "per-node upgrade" / "carotate"
	// Critical admits the pod as system-node-critical, as the static pods are:
	// under node pressure the kubelet refuses every other new pod. It is also
	// allowed to evict workload pods to make room, past every PDB, so only a
	// Job on a node that has already been drained should set it.
	Critical bool
}

func (s Spec) output() io.Writer {
	if s.Output != nil {
		return s.Output
	}
	return os.Stdout
}

// NodeAnnotation carries the target node's name verbatim.
//
// The label cannot: a node name is a DNS subdomain and may be 253 characters,
// while a label value may be 63. Build therefore puts a hash in the label when
// the name does not fit, and the full name here, where there is no limit.
const NodeAnnotation = "stratum.ciq.com/node"

// NodeLabelValue returns node when it is a usable label value, or a stable
// hash of it when it is not. Selecting on LabelKey=NodeLabelValue(node) finds
// every Job targeting that node, whatever scheme named them.
func NodeLabelValue(node string) string {
	if len(validation.IsValidLabelValue(node)) == 0 {
		return node
	}
	sum := sha256.Sum256([]byte(node))
	return fmt.Sprintf("node-%x", sum[:16])
}

// JobNode returns the node a Job targets. Jobs written before NodeAnnotation
// existed carry the name in the label, which was possible only because a node
// whose name did not fit could not have a Job at all.
func JobNode(j *batchv1.Job, labelKey string) string {
	if v, ok := j.Annotations[NodeAnnotation]; ok {
		return v
	}
	return j.Labels[labelKey]
}

// TruncName returns name when it fits the 63-character object-name limit.
// A longer name keeps its first 46 characters, less any trailing '-' or '.',
// and gains a hash of the whole name, so the result is a valid name and two
// long names sharing a prefix still differ.
func TruncName(name string) string {
	if len(name) <= maxNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:46], ".-") + fmt.Sprintf("-%x", sum[:8])
}

// Build renders the Job object for s. Pure — split out so the pod spec is
// unit-testable without a cluster.
func Build(s Spec) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        s.Name,
			Namespace:   s.Namespace,
			Labels:      map[string]string{s.LabelKey: NodeLabelValue(s.Node)},
			Annotations: map[string]string{NodeAnnotation: s.Node},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(int32(0)),
			ActiveDeadlineSeconds: ptr.To(int64(s.ActiveDeadline.Seconds())),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					PriorityClassName:             priorityClass(s.Critical),
					NodeName:                      s.Node,
					HostPID:                       true,
					HostNetwork:                   true,
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
					Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name: s.Container,
						// Never pull: the image ships in the airgap payload, and an
						// upgrade or CA rotation is exactly when a node may have no
						// registry reachable.
						Image:           version.NsenterImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
						Command:         s.Argv,
					}},
				},
			},
		},
	}
}

// create creates the Job, replacing a finished one this node owns from a
// previous run so its stale status cannot answer for this one.
func create(ctx context.Context, cs kubernetes.Interface, s Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	jobs := cs.BatchV1().Jobs(s.Namespace)
	job := Build(s)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s job: %w", s.Kind, err)
		}
		// A leftover Job from a previous run would short-circuit the poll below with
		// its STALE status (a stale Succeeded would falsely pass this node). Replace
		// it, but only if it has finished. A Job that is still
		// running, or being deleted with its Pod still up, belongs to a run that is
		// either alive or crashed mid-change on this node, and deleting it would
		// interrupt that change on the host. No lease or preflight check can make
		// "check, then replace" atomic against a stale process resuming, so the
		// replace itself refuses: two runs racing for one node leave one Job
		// running and the other run failing loudly. An abandoned Job still ends at
		// its ActiveDeadline and becomes replaceable.
		existing, err := jobs.Get(ctx, s.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("inspect existing %s Job %s: %w", s.Kind, s.Name, err)
		}
		if err == nil {
			// Whose Job this is, before deciding anything about it. Two
			// nodes can arrive at one Job name -- through truncation, or
			// because a departed node's Job was named for a name a current
			// node now hashes to -- and replacing a Job this node does not
			// own is the deletion the naming exists to prevent. An owner
			// that cannot be established at all counts as someone else's.
			if owner := JobNode(existing, s.LabelKey); owner != s.Node {
				return fmt.Errorf("the existing %s Job %s belongs to node %q, not %q; refusing to replace it. "+
					"If that node is gone, remove the Job (kubectl -n %s delete job %s) and retry",
					s.Kind, s.Name, owner, s.Node, s.Namespace, s.Name)
			}
			if !Finished(existing) {
				return fmt.Errorf("a %s Job %s is already running on %s (from another or an earlier run); refusing to "+
					"replace it mid-change. Wait for it to finish or hit its deadline, then retry", s.Kind, s.Name, s.Node)
			}
			if err := DeleteExactAndWait(ctx, cs, existing); err != nil {
				return fmt.Errorf("stale %s Job %s could not be replaced: %w", s.Kind, s.Name, err)
			}
		}
		if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("recreate %s job: %w", s.Kind, err)
		}
	}
	return nil
}

// RunExitCode runs a Job whose answer is its exit status rather than its
// success, and returns the container's exit code: 0 when the Job succeeded,
// the code it failed with otherwise. A failed Job is left in place for
// inspection, and the next Run or RunExitCode on it replaces it. An error
// means no exit status could be established (the Job timed out, or failed
// without its container terminating).
func RunExitCode(ctx context.Context, cs kubernetes.Interface, s Spec) (int, error) {
	code, _, err := runExitCode(ctx, cs, s, false)
	return code, err
}

// RunExitCodeLogs is RunExitCode that also returns the log of the Job's Pod,
// for a Job that answers in its output as well as its exit status. The log of
// a succeeded Job is read before the Job is deleted, since Logs finds the Pod
// through the Job. An error also means the log could not be read; one that
// wraps ErrLogUnread had an exit status all the same, and returns it.
func RunExitCodeLogs(ctx context.Context, cs kubernetes.Interface, s Spec) (int, string, error) {
	return runExitCode(ctx, cs, s, true)
}

// ErrLogUnread marks a RunExitCodeLogs error whose Job did end with an exit
// status, returned beside it, but whose Pod's log could not be read: the
// apiserver reads it from the node's kubelet, which can be unreachable or
// untrusted while the Job's status still says how it ended.
var ErrLogUnread = errors.New("its log could not be read")

func runExitCode(ctx context.Context, cs kubernetes.Interface, s Spec, withLog bool) (int, string, error) {
	if err := create(ctx, cs, s); err != nil {
		return 0, "", err
	}
	jobs := cs.BatchV1().Jobs(s.Namespace)
	var lastErr error
	for i := 0; i < s.PollAttempts; i++ {
		started := time.Now()
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		j, err := getJob(ctx, jobs, s.Name)
		if err != nil {
			if apierrors.IsNotFound(err) || ctx.Err() != nil {
				return 0, "", err
			}
			lastErr = err
			if err := wait(ctx, pollInterval-time.Since(started)); err != nil {
				return 0, "", err
			}
			continue
		}
		if j.Status.Succeeded >= 1 {
			var log string
			var logErr error
			if withLog {
				log, logErr = podLog(ctx, cs, s, string(j.UID))
			}
			pp := metav1.DeletePropagationBackground
			if derr := jobs.Delete(ctx, s.Name, metav1.DeleteOptions{PropagationPolicy: &pp}); derr != nil {
				fmt.Fprintf(s.output(), "  WARN: could not delete completed Job %s: %v (a later re-run will replace it)\n", s.Name, derr)
			}
			if logErr != nil {
				return 0, "", fmt.Errorf("%s Job %s succeeded, and %w: %w", s.Kind, s.Name, ErrLogUnread, logErr)
			}
			return 0, log, nil
		}
		if j.Status.Failed >= 1 {
			code, err := containerExitCode(ctx, cs, s, string(j.UID))
			if err != nil {
				return 0, "", fmt.Errorf("%s Job %s failed, and its exit status could not be read: %w (logs: kubectl -n %s logs job/%s)",
					s.Kind, s.Name, err, s.Namespace, s.Name)
			}
			if !withLog {
				return code, "", nil
			}
			log, err := podLog(ctx, cs, s, string(j.UID))
			if err != nil {
				return code, "", fmt.Errorf("%s Job %s exited %d, and %w: %w (logs: kubectl -n %s logs job/%s)",
					s.Kind, s.Name, code, ErrLogUnread, err, s.Namespace, s.Name)
			}
			return code, log, nil
		}
		if err := wait(ctx, pollInterval-time.Since(started)); err != nil {
			return 0, "", err
		}
	}
	return 0, "", timedOut(s, lastErr)
}

// controllerUIDLabel is the label the Job controller puts on every Pod it
// creates, naming that one Job by UID.
const controllerUIDLabel = "batch.kubernetes.io/controller-uid"

// containerExitCode reads the exit code of s.Container in the Pod of the Job
// with jobUID. By UID, not by name: every check on a node reuses one Job name,
// and a finished check's Job is deleted in the background, so its Pod can
// outlive it and answer for the next one -- an old exit 0 read for a new 10
// would skip an upgrade the node needs.
func containerExitCode(ctx context.Context, cs kubernetes.Interface, s Spec, jobUID string) (int, error) {
	if jobUID == "" {
		return 0, fmt.Errorf("the Job has no UID to find its Pod by")
	}
	pods, err := cs.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: controllerUIDLabel + "=" + jobUID})
	if err != nil {
		return 0, err
	}
	why := ""
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == s.Container && c.State.Terminated != nil {
				return int(c.State.Terminated.ExitCode), nil
			}
		}
		// A Pod the kubelet refused -- evicted at admission under node
		// pressure -- never ran its container; its own reason is the answer.
		if p.Status.Reason != "" || p.Status.Message != "" {
			why = fmt.Sprintf(" (Pod %s %s: %s %s)", p.Name, p.Status.Phase, p.Status.Reason, p.Status.Message)
		}
	}
	return 0, fmt.Errorf("no terminated %s container found%s", s.Container, why)
}

// Logs returns the log of s.Container in the Pod of the Job named s.Name, as
// RunExitCode reads its exit code: by the Job's UID, so an earlier Job's Pod
// cannot answer.
func Logs(ctx context.Context, cs kubernetes.Interface, s Spec) (string, error) {
	j, err := getJob(ctx, cs.BatchV1().Jobs(s.Namespace), s.Name)
	if err != nil {
		return "", err
	}
	return podLog(ctx, cs, s, string(j.UID))
}

// logReadTimeout bounds podLog. The apiserver relays a Pod's log from the
// node's kubelet, and one that accepts the request and never answers would
// otherwise hold the caller after the Job has already ended: ActiveDeadline
// and PollAttempts bound the Job, not this read.
var logReadTimeout = 30 * time.Second

// podLog is the log of s.Container in the Pod of the Job with jobUID.
func podLog(ctx context.Context, cs kubernetes.Interface, s Spec, jobUID string) (string, error) {
	if jobUID == "" {
		return "", fmt.Errorf("the Job has no UID to find its Pod by")
	}
	ctx, cancel := context.WithTimeout(ctx, logReadTimeout)
	defer cancel()
	pods, err := cs.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: controllerUIDLabel + "=" + jobUID})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("the Job has no Pod")
	}
	b, err := cs.CoreV1().Pods(s.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: s.Container}).DoRaw(ctx)
	return string(b), err
}

// Run creates the Job (replacing a stale one from a prior run so its stale status
// cannot short-circuit the poll), waits for it to complete, deletes it on success,
// and returns an error on failure or timeout.
func Run(ctx context.Context, cs kubernetes.Interface, s Spec) error {
	if err := create(ctx, cs, s); err != nil {
		return err
	}
	jobs := cs.BatchV1().Jobs(s.Namespace)
	var lastErr error
	for i := 0; i < s.PollAttempts; i++ {
		started := time.Now()
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := getJob(ctx, jobs, s.Name)
		if err != nil {
			if apierrors.IsNotFound(err) || ctx.Err() != nil {
				return err
			}
			lastErr = err
			if err := wait(ctx, pollInterval-time.Since(started)); err != nil {
				return err
			}
			continue
		}
		if j.Status.Succeeded >= 1 {
			pp := metav1.DeletePropagationBackground
			if derr := jobs.Delete(ctx, s.Name, metav1.DeleteOptions{PropagationPolicy: &pp}); derr != nil {
				fmt.Fprintf(s.output(), "  WARN: could not delete completed Job %s: %v (a later re-run will replace it)\n", s.Name, derr)
			}
			return nil
		}
		if j.Status.Failed >= 1 {
			// The failed Job is left in place for inspection; a re-run replaces it.
			return fmt.Errorf("%s Job %s failed (logs: kubectl -n %s logs job/%s)", s.Kind, s.Name, s.Namespace, s.Name)
		}
		if err := wait(ctx, pollInterval-time.Since(started)); err != nil {
			return err
		}
	}
	return timedOut(s, lastErr)
}

// getJob is one poll read. An error from it that is not NotFound is the API
// server being unreachable, as while it restarts under a single-node cluster's
// own upgrade: the callers keep polling through it rather than give up on a
// Job that is still changing the host.
func getJob(ctx context.Context, jobs batchtyped.JobInterface, name string) (*batchv1.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout())
	defer cancel()
	return jobs.Get(ctx, name, metav1.GetOptions{})
}

// timedOut is the poll's give-up error, naming the last read that failed.
func timedOut(s Spec, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("%s Job %s did not complete in time (last error reading it: %w)", s.Kind, s.Name, lastErr)
	}
	return fmt.Errorf("%s Job %s did not complete in time", s.Kind, s.Name)
}

// Finished reports whether a Job has run to an end: it succeeded or failed, by
// its counters or its conditions. A Job being deleted is not finished until its
// Pod is: a deletion timestamp alone says nothing about the host.
func Finished(j *batchv1.Job) bool {
	if j.Status.Succeeded >= 1 || j.Status.Failed >= 1 {
		return true
	}
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// DeleteExactAndWait deletes exactly the Job object it was given, with its UID
// and resourceVersion as preconditions, so a Job recreated or changed since it
// was inspected (another run's, now running) is never the one removed. Then it
// waits for it to be gone.
func DeleteExactAndWait(ctx context.Context, cs kubernetes.Interface, j *batchv1.Job) error {
	jobs := cs.BatchV1().Jobs(j.Namespace)
	pp := metav1.DeletePropagationForeground
	uid, rv := j.UID, j.ResourceVersion
	if err := jobs.Delete(ctx, j.Name, metav1.DeleteOptions{
		PropagationPolicy: &pp,
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
	}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for i := 0; i < 30; i++ { // up to ~60s
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := jobs.Get(ctx, j.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		if err := wait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("job did not finish deleting")
}

// DeleteAndWait foreground-deletes a Job and waits until it is gone, so a fresh
// Create cannot race the old object.
func DeleteAndWait(ctx context.Context, cs kubernetes.Interface, namespace, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	jobs := cs.BatchV1().Jobs(namespace)
	pp := metav1.DeletePropagationForeground
	if err := jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &pp}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for i := 0; i < 30; i++ { // up to ~60s
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := jobs.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		if err := wait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("job did not finish deleting")
}

// Ready reports whether a node's Ready condition is True.
func Ready(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// WaitReady polls until the named node is Ready or attempts run out (one every
// pollInterval).
func WaitReady(ctx context.Context, cs kubernetes.Interface, name string, attempts int) error {
	for i := 0; i < attempts; i++ {
		started := time.Now()
		if err := ctx.Err(); err != nil {
			return err
		}
		gctx, cancel := context.WithTimeout(ctx, readTimeout())
		n, err := cs.CoreV1().Nodes().Get(gctx, name, metav1.GetOptions{})
		cancel()
		if err == nil && Ready(n) {
			return nil
		}
		if err := wait(ctx, pollInterval-time.Since(started)); err != nil {
			return err
		}
	}
	return fmt.Errorf("node %s did not become Ready in time", name)
}

// wait interrupts the polling delay when the caller cancels the operation.
func wait(ctx context.Context, interval time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func priorityClass(critical bool) string {
	if critical {
		return "system-node-critical"
	}
	return ""
}
