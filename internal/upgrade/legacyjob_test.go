package upgrade

import (
	"io"

	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/nodejob"
)

// nodejob.Run replaces a stale Job only at its own name; a legacy-named one is invisible to it.
func TestClearStaleUpgradeJobs(t *testing.T) {
	// Two names that truncate to the same legacy name: the collision jobName exists to prevent.
	shared := strings.Repeat("a", 63)
	nodeA := shared + ".worker-1"
	nodeB := shared + ".worker-2"

	legacyJob := finishedLegacyJob

	t.Run("a legacy Job for this node is removed", func(t *testing.T) {
		legacy := legacyJobName(nodeA)
		cs := fake.NewSimpleClientset(legacyJob(legacy, nodeA))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, nodeA); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), legacy, metav1.GetOptions{}); err == nil {
			t.Error("the legacy Job survived")
		}
	})

	// Both nodes truncate to the same legacy name; deleting by name would take the other's Job.
	t.Run("a legacy Job belonging to another node is left alone", func(t *testing.T) {
		legacy := legacyJobName(nodeA)
		if legacy != legacyJobName(nodeB) {
			t.Fatalf("fixture no longer collides: %q vs %q", legacy, legacyJobName(nodeB))
		}
		cs := fake.NewSimpleClientset(legacyJob(legacy, nodeB))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, nodeA); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), legacy, metav1.GetOptions{}); err != nil {
			t.Errorf("another node's Job was deleted: %v", err)
		}
	})

	// sweepUpgradeJobs lists once; a legacy Job created after it would be interrupted mid-host-change.
	t.Run("a legacy Job still running is refused, not removed", func(t *testing.T) {
		legacy := legacyJobName(nodeA)
		cs := fake.NewSimpleClientset(unfinishedLegacyJob(legacy, nodeA))

		err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, nodeA)
		if err == nil {
			t.Fatal("a running legacy Job was removed")
		}
		if !strings.Contains(err.Error(), "still running") {
			t.Errorf("error %q does not say the Job is still running", err)
		}
		if _, gerr := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), legacy, metav1.GetOptions{}); gerr != nil {
			t.Errorf("the running Job was deleted: %v", gerr)
		}
	})

	t.Run("nothing to remove is not an error", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, nodeA); err != nil {
			t.Errorf("removeLegacyUpgradeJob: %v", err)
		}
	})

	// A short node's Job is the one nodejob.Run replaces itself; deleting it here would race it.
	t.Run("a short node name is left to nodejob.Run", func(t *testing.T) {
		const short = "worker-1"
		if legacyJobName(short) != jobName(short) {
			t.Fatalf("fixture is not a short name: %q", short)
		}
		cs := fake.NewSimpleClientset(legacyJob(jobName(short), short))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, short); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), jobName(short), metav1.GetOptions{}); err != nil {
			t.Errorf("the current Job was deleted: %v", err)
		}
	})
}

// A long node's legacy name can equal a short node's current name; ownership resolves it.
func TestLegacySweepResolvesACrossNameCollision(t *testing.T) {
	long := strings.Repeat("a", 80)
	short := strings.TrimPrefix(legacyJobName(long), "stratum-upgrade-")
	if jobName(short) != legacyJobName(long) {
		t.Fatalf("fixture does not collide: %q vs %q", jobName(short), legacyJobName(long))
	}
	shared := jobName(short)

	t.Run("the long node's legacy Job is removed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(finishedLegacyJob(shared, long))
		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, long); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), shared, metav1.GetOptions{}); err == nil {
			t.Error("the legacy Job survived")
		}
	})

	t.Run("the short node's own Job at that name is left alone", func(t *testing.T) {
		cs := fake.NewSimpleClientset(finishedLegacyJob(shared, short))
		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, long); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), shared, metav1.GetOptions{}); err != nil {
			t.Errorf("the short node's Job was deleted: %v", err)
		}
	})

	t.Run("the short node sweeps nothing", func(t *testing.T) {
		cs := fake.NewSimpleClientset(finishedLegacyJob(shared, short))
		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, short); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), shared, metav1.GetOptions{}); err != nil {
			t.Errorf("the short node deleted its own Job: %v", err)
		}
	})
}

func unfinishedLegacyJob(name, labelNode string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   cuNamespace,
		Labels:      map[string]string{upgradeJobLabel: nodejob.NodeLabelValue(labelNode)},
		Annotations: map[string]string{nodejob.NodeAnnotation: labelNode},
	}}
}

// The only state the sweep may delete.
func finishedLegacyJob(name, labelNode string) *batchv1.Job {
	j := unfinishedLegacyJob(name, labelNode)
	j.Status.Succeeded = 1
	return j
}

// Selecting on the node label finds Jobs under any naming, not just the one scheme known.
func TestClearStaleUpgradeJobsFindsAnyName(t *testing.T) {
	const node = "cp1"

	t.Run("a Job under an unrecognised old name is still removed", func(t *testing.T) {
		stale := "stratum-upgrade-cp1-from-some-older-scheme"
		if stale == jobName(node) || stale == legacyJobName(node) {
			t.Fatalf("fixture is not an unrecognised name: %q", stale)
		}
		cs := fake.NewSimpleClientset(finishedLegacyJob(stale, node))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, node); err != nil {
			t.Fatalf("clearStaleUpgradeJobs: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), stale, metav1.GetOptions{}); err == nil {
			t.Error("a stale Job under an unrecognised name survived")
		}
	})

	// nodejob.Run replaces this Job itself; deleting it here would race that.
	t.Run("the Job this run will create is left to nodejob.Run", func(t *testing.T) {
		cs := fake.NewSimpleClientset(finishedLegacyJob(jobName(node), node))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, node); err != nil {
			t.Fatalf("clearStaleUpgradeJobs: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), jobName(node), metav1.GetOptions{}); err != nil {
			t.Errorf("the current Job was deleted: %v", err)
		}
	})

	t.Run("a running Job under an unrecognised name refuses the run", func(t *testing.T) {
		stale := "stratum-upgrade-cp1-older"
		cs := fake.NewSimpleClientset(unfinishedLegacyJob(stale, node))

		err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, node)
		if err == nil {
			t.Fatal("a running Job under an unrecognised name was ignored")
		}
		if !strings.Contains(err.Error(), stale) {
			t.Errorf("error %q does not name the Job", err)
		}
	})
}

// The label may be a hash and the annotation is the node name verbatim.
func TestClearStaleUpgradeJobsTrustsTheAnnotationOverTheLabel(t *testing.T) {
	const mine, theirs = "cp1", "cp2"

	mislabelled := func(name string) *batchv1.Job {
		j := finishedLegacyJob(name, theirs)
		j.Labels[upgradeJobLabel] = nodejob.NodeLabelValue(mine)
		return j
	}

	t.Run("a finished Job owned by another node is not deleted", func(t *testing.T) {
		const name = "stratum-upgrade-shared"
		cs := fake.NewSimpleClientset(mislabelled(name))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, mine); err != nil {
			t.Fatalf("clearStaleUpgradeJobs: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), name, metav1.GetOptions{}); err != nil {
			t.Errorf("another node's Job was deleted: %v", err)
		}
	})

	t.Run("a running Job owned by another node does not block this one", func(t *testing.T) {
		const name = "stratum-upgrade-shared-running"
		j := unfinishedLegacyJob(name, theirs)
		j.Labels[upgradeJobLabel] = nodejob.NodeLabelValue(mine)
		cs := fake.NewSimpleClientset(j)

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, mine); err != nil {
			t.Errorf("another node's running Job blocked this node: %v", err)
		}
	})
}
