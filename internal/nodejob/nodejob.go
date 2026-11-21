// Package nodejob runs a privileged Job that nsenters PID 1 and execs the on-host stratum binary.
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

// A var so tests can zero it; production never reassigns it.
var pollInterval = 5 * time.Second

// Bounds each poll read: an unanswered read costs one poll, not the run.
var getCallTimeout = 10 * time.Second

// Kept at pollInterval so a poll sleeps the remainder and the loop keeps its deadline.
func readTimeout() time.Duration {
	if pollInterval > 0 && pollInterval < getCallTimeout {
		return pollInterval
	}
	return getCallTimeout
}

const maxNameLen = 63

// Spec describes one privileged node Job.
type Spec struct {
	// Nil keeps stdout: standalone callers print there; MCP callers supply their own.
	Output         io.Writer
	Namespace      string
	Name           string
	Node           string
	LabelKey       string
	Container      string
	Argv           []string
	ActiveDeadline time.Duration
	PollAttempts   int
	Kind           string // noun for error messages
	// system-node-critical is admitted under node pressure, but may evict workloads past PDBs.
	Critical bool
}

func (s Spec) output() io.Writer {
	if s.Output != nil {
		return s.Output
	}
	return os.Stdout
}

// The label cannot hold a node name over 63 characters; the annotation is unlimited.
const NodeAnnotation = "stratum.ciq.com/node"

// NodeLabelValue returns node when it fits a label value, else a stable hash of it.
func NodeLabelValue(node string) string {
	if len(validation.IsValidLabelValue(node)) == 0 {
		return node
	}
	sum := sha256.Sum256([]byte(node))
	return fmt.Sprintf("node-%x", sum[:16])
}

func JobNode(j *batchv1.Job, labelKey string) string {
	if v, ok := j.Annotations[NodeAnnotation]; ok {
		return v
	}
	return j.Labels[labelKey]
}

func TruncName(name string) string {
	if len(name) <= maxNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:46], ".-") + fmt.Sprintf("-%x", sum[:8])
}

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
						// The image ships in the airgap payload; a node mid-change may have no registry reachable.
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
		// Replace only a finished Job: a running one belongs to a run still changing the host.
		existing, err := jobs.Get(ctx, s.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("inspect existing %s Job %s: %w", s.Kind, s.Name, err)
		}
		if err == nil {
			// Two nodes can reach one Job name; replacing another node's is the deletion naming prevents.
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

func RunExitCode(ctx context.Context, cs kubernetes.Interface, s Spec) (int, error) {
	code, _, err := runExitCode(ctx, cs, s, false)
	return code, err
}

func RunExitCodeLogs(ctx context.Context, cs kubernetes.Interface, s Spec) (int, string, error) {
	return runExitCode(ctx, cs, s, true)
}

// The Job's status is valid; the apiserver reads the log from a kubelet that may not answer.
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

// The label the Job controller puts on every Pod it creates.
const controllerUIDLabel = "batch.kubernetes.io/controller-uid"

// By UID: a previous Job's Pod can outlive its background deletion and answer for this one.
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
		// An evicted Pod never ran its container; its own reason is the answer.
		if p.Status.Reason != "" || p.Status.Message != "" {
			why = fmt.Sprintf(" (Pod %s %s: %s %s)", p.Name, p.Status.Phase, p.Status.Reason, p.Status.Message)
		}
	}
	return 0, fmt.Errorf("no terminated %s container found%s", s.Container, why)
}

// By the Job's UID, so an earlier Job's Pod cannot answer.
func Logs(ctx context.Context, cs kubernetes.Interface, s Spec) (string, error) {
	j, err := getJob(ctx, cs.BatchV1().Jobs(s.Namespace), s.Name)
	if err != nil {
		return "", err
	}
	return podLog(ctx, cs, s, string(j.UID))
}

// A stalled kubelet log read must not hold the caller past the Job's end.
var logReadTimeout = 30 * time.Second

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
			// Left in place for inspection; a re-run replaces it.
			return fmt.Errorf("%s Job %s failed (logs: kubectl -n %s logs job/%s)", s.Kind, s.Name, s.Namespace, s.Name)
		}
		if err := wait(ctx, pollInterval-time.Since(started)); err != nil {
			return err
		}
	}
	return timedOut(s, lastErr)
}

// Not-NotFound errors are an unreachable API server mid-upgrade; the poll rides them out.
func getJob(ctx context.Context, jobs batchtyped.JobInterface, name string) (*batchv1.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout())
	defer cancel()
	return jobs.Get(ctx, name, metav1.GetOptions{})
}

func timedOut(s Spec, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("%s Job %s did not complete in time (last error reading it: %w)", s.Kind, s.Name, lastErr)
	}
	return fmt.Errorf("%s Job %s did not complete in time", s.Kind, s.Name)
}

// A deletion timestamp alone says nothing about the host.
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

// Preconditions keep a Job recreated or changed since it was inspected from being removed.
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

func Ready(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

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
