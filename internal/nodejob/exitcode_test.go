package nodejob

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// jobPod is the Pod the Job controller would have made for the Job with uid,
// its container terminated with code.
func jobPod(s Spec, uid, container string, code int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name + "-" + uid, Namespace: s.Namespace,
			Labels: map[string]string{controllerUIDLabel: uid, "batch.kubernetes.io/job-name": s.Name},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  container,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
		}}},
	}
}

// failedJob answers every Get with a failed Job whose UID is uid.
func failedJob(uid string) clienttesting.ReactionFunc {
	return func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga := a.(clienttesting.GetAction)
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace(), UID: types.UID(uid)},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	}
}

// A check Job answers with its exit status: success is 0, and a
// failure is the code its container exited with, not just "failed".
func TestRunExitCode(t *testing.T) {
	restore := pollInterval
	pollInterval = 0
	defer func() { pollInterval = restore }()
	ctx := context.Background()
	s := testSpec()

	t.Run("succeeded", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{Succeeded: 1}))
		if code, err := RunExitCode(ctx, cs, s); err != nil || code != 0 {
			t.Errorf("RunExitCode = %d, %v; want 0, nil", code, err)
		}
	})
	t.Run("failed with a code", func(t *testing.T) {
		cs := fake.NewSimpleClientset(jobPod(s, "new", s.Container, 10))
		cs.PrependReactor("get", "jobs", failedJob("new"))
		if code, err := RunExitCode(ctx, cs, s); err != nil || code != 10 {
			t.Errorf("RunExitCode = %d, %v; want 10, nil", code, err)
		}
	})
	// The previous check's Job was deleted in the background and its Pod,
	// exit 0, is still there beside this one's. Its answer is not this one's.
	t.Run("an earlier Job's Pod does not answer", func(t *testing.T) {
		// Named so the stale Pod lists first: a selector by Job name alone
		// would read its 0.
		cs := fake.NewSimpleClientset(jobPod(s, "aaa-old", s.Container, 0), jobPod(s, "zzz-new", s.Container, 10))
		cs.PrependReactor("get", "jobs", failedJob("zzz-new"))
		if code, err := RunExitCode(ctx, cs, s); err != nil || code != 10 {
			t.Errorf("RunExitCode = %d, %v; want this Job's 10, not the old Pod's 0", code, err)
		}
	})
	t.Run("another container's code is not the answer", func(t *testing.T) {
		cs := fake.NewSimpleClientset(jobPod(s, "new", "sidecar", 10))
		cs.PrependReactor("get", "jobs", failedJob("new"))
		if _, err := RunExitCode(ctx, cs, s); err == nil || !strings.Contains(err.Error(), "exit status could not be read") {
			t.Errorf("RunExitCode = %v, want an error saying the status is unknown", err)
		}
	})
	// Evicted at admission under node pressure: the container never ran, and
	// the error says why rather than only that no status exists.
	t.Run("evicted before it ran", func(t *testing.T) {
		pod := jobPod(s, "new", s.Container, 0)
		pod.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "The node had condition: [DiskPressure]."}
		cs := fake.NewSimpleClientset(pod)
		cs.PrependReactor("get", "jobs", failedJob("new"))
		if _, err := RunExitCode(ctx, cs, s); err == nil || !strings.Contains(err.Error(), "Evicted") || !strings.Contains(err.Error(), "DiskPressure") {
			t.Errorf("RunExitCode = %v, want the Pod's eviction reason", err)
		}
	})
	t.Run("failed with no pod", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", failedJob("new"))
		if _, err := RunExitCode(ctx, cs, s); err == nil {
			t.Errorf("RunExitCode reported a code with no terminated container")
		}
	})
	t.Run("never completes", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{}))
		if _, err := RunExitCode(ctx, cs, s); err == nil || !strings.Contains(err.Error(), "did not complete in time") {
			t.Errorf("RunExitCode = %v, want a timeout", err)
		}
	})
}

// Logs is the log of this Job's Pod, found by the Job's UID as RunExitCode's
// code is: an earlier Job's Pod, left behind under the same name, does not answer.
func TestLogs(t *testing.T) {
	ctx := context.Background()
	s := testSpec()

	t.Run("this Job's Pod", func(t *testing.T) {
		cs := fake.NewSimpleClientset(jobPod(s, "new", s.Container, 10))
		cs.PrependReactor("get", "jobs", failedJob("new"))
		got, err := Logs(ctx, cs, s)
		if err != nil || got != "fake logs" {
			t.Errorf("Logs = %q, %v; want the Pod's log", got, err)
		}
	})
	t.Run("an earlier Job's Pod does not answer", func(t *testing.T) {
		cs := fake.NewSimpleClientset(jobPod(s, "old", s.Container, 0))
		cs.PrependReactor("get", "jobs", failedJob("new"))
		if got, err := Logs(ctx, cs, s); err == nil {
			t.Errorf("Logs = %q, nil; want an error: the only Pod is another Job's", got)
		}
	})
	t.Run("no Job", func(t *testing.T) {
		if _, err := Logs(ctx, fake.NewSimpleClientset(), s); err == nil {
			t.Error("Logs of a Job that does not exist returned no error")
		}
	})
}

// succeededJob answers every Get with a succeeded Job whose UID is uid, until
// the Job is deleted.
func succeededJob(uid string, deleted *bool) clienttesting.ReactionFunc {
	return func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga := a.(clienttesting.GetAction)
		if *deleted {
			return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), ga.GetName())
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace(), UID: types.UID(uid)},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	}
}

// A succeeded Job is deleted as soon as it is seen, so its log must be read
// first: Logs after RunExitCode finds no Job, and RunExitCodeLogs still answers.
func TestRunExitCodeLogs(t *testing.T) {
	restore := pollInterval
	pollInterval = 0
	defer func() { pollInterval = restore }()
	ctx := context.Background()
	s := testSpec()

	t.Run("succeeded", func(t *testing.T) {
		deleted := false
		cs := fake.NewSimpleClientset(jobPod(s, "new", s.Container, 0))
		cs.PrependReactor("get", "jobs", succeededJob("new", &deleted))
		cs.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
			deleted = true
			return true, nil, nil
		})
		code, log, err := RunExitCodeLogs(ctx, cs, s)
		if err != nil || code != 0 || log != "fake logs" {
			t.Fatalf("RunExitCodeLogs = %d, %q, %v; want 0 and the Pod's log", code, log, err)
		}
		if !deleted {
			t.Error("the succeeded Job was not deleted")
		}
		if _, err := Logs(ctx, cs, s); err == nil {
			t.Error("Logs after the Job was deleted returned no error; the test does not model the deletion")
		}
	})
	t.Run("failed with a code", func(t *testing.T) {
		cs := fake.NewSimpleClientset(jobPod(s, "new", s.Container, 11))
		cs.PrependReactor("get", "jobs", failedJob("new"))
		if code, log, err := RunExitCodeLogs(ctx, cs, s); err != nil || code != 11 || log != "fake logs" {
			t.Errorf("RunExitCodeLogs = %d, %q, %v; want 11 and the Pod's log", code, log, err)
		}
	})
	t.Run("succeeded with no pod", func(t *testing.T) {
		deleted := false
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", succeededJob("new", &deleted))
		code, _, err := RunExitCodeLogs(ctx, cs, s)
		if err == nil || !strings.Contains(err.Error(), "log could not be read") {
			t.Errorf("RunExitCodeLogs = %v, want an error saying the log is unknown", err)
		}
		// The Job succeeded, so the caller may still decide on its status.
		if !errors.Is(err, ErrLogUnread) || code != 0 {
			t.Errorf("RunExitCodeLogs = %d, %v; want 0 and an error wrapping ErrLogUnread", code, err)
		}
	})
	t.Run("failed with log unread", func(t *testing.T) {
		cs := fake.NewSimpleClientset(jobPod(s, "new", s.Container, 11))
		cs.PrependReactor("get", "jobs", failedJob("new"))
		cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() == "log" {
				return true, nil, errors.New("log unavailable")
			}
			return false, nil, nil
		})
		code, _, err := RunExitCodeLogs(ctx, cs, s)
		if err == nil || !errors.Is(err, ErrLogUnread) {
			t.Fatalf("RunExitCodeLogs = %d, %v; want an error wrapping ErrLogUnread", code, err)
		}
		if code != 11 {
			t.Errorf("RunExitCodeLogs = %d, %v; want 11 and ErrLogUnread", code, err)
		}
	})
}

// A kubelet that accepts the log request and never answers costs the read its
// timeout, not the run: the Job's exit status still comes back, with
// ErrLogUnread. The fake clientset ignores contexts, so this one is served.
func TestRunExitCodeLogsBoundsAStalledLog(t *testing.T) {
	restorePoll, restoreLog := pollInterval, logReadTimeout
	pollInterval, logReadTimeout = 0, 50*time.Millisecond
	defer func() { pollInterval, logReadTimeout = restorePoll, restoreLog }()
	s := testSpec()
	job := &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace, UID: "new"},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	pods := &corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    []corev1.Pod{*jobPod(s, "new", s.Container, 11)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case strings.Contains(r.URL.Path, "/jobs"):
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(job)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			_ = json.NewEncoder(w).Encode(pods)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	code, _, err := RunExitCodeLogs(context.Background(), cs, s)
	if !errors.Is(err, ErrLogUnread) || code != 11 {
		t.Errorf("RunExitCodeLogs = %d, %v; want 11 and an error wrapping ErrLogUnread", code, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("RunExitCodeLogs waited %v on a log that never answered, want about logReadTimeout", elapsed)
	}
}
