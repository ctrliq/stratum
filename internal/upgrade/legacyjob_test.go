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

// TestClearStaleUpgradeJobs covers the rename's migration path.
//
// nodejob.Run replaces a stale Job only when it already carries the name it is
// about to create, so a Job left under the old truncated name is invisible to
// it: an interrupted upgrade could keep one privileged Job running while a
// retry starts a second on the same node.
func TestClearStaleUpgradeJobs(t *testing.T) {
	// Two node names long enough to be truncated, sharing the prefix the
	// truncation keeps. This is the collision jobName exists to prevent.
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

	// The case that makes the label check load-bearing: both nodes truncate
	// to the same legacy name, so deleting by name alone would take the
	// other node's Job with it.
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

	// The gap sweepUpgradeJobs cannot close: it lists once, and an
	// older process can create its legacy-named Job afterwards. Deleting that
	// would interrupt a privileged upgrade mid-change on the host.
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

	// A short node name never changed, so its Job is the same object
	// nodejob.Run would replace itself. Deleting it here would remove a Job
	// a concurrent run may be polling.
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

// TestLegacySweepResolvesACrossNameCollision covers the case a preflight used
// to reject outright: a long node's legacy truncated name can equal a short
// node's current name, which is never truncated.
//
// Refusing the pair made such a cluster permanently unupgradeable, and no
// manual cleanup could help, because the check never looked at Jobs. The sweep
// resolves it by ownership instead.
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

	// The half that makes the pair usable rather than merely diagnosable.
	t.Run("the short node's own Job at that name is left alone", func(t *testing.T) {
		cs := fake.NewSimpleClientset(finishedLegacyJob(shared, short))
		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, long); err != nil {
			t.Fatalf("removeLegacyUpgradeJob: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), shared, metav1.GetOptions{}); err != nil {
			t.Errorf("the short node's Job was deleted: %v", err)
		}
	})

	// And the short node never sweeps: its legacy name is its current name.
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

// unfinishedLegacyJob is a Job under the pre-hash name that has neither
// succeeded nor failed.
func unfinishedLegacyJob(name, labelNode string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   cuNamespace,
		Labels:      map[string]string{upgradeJobLabel: nodejob.NodeLabelValue(labelNode)},
		Annotations: map[string]string{nodejob.NodeAnnotation: labelNode},
	}}
}

// finishedLegacyJob is a legacy-named Job that has run to completion, which is
// the only state removeLegacyUpgradeJob may delete.
func finishedLegacyJob(name, labelNode string) *batchv1.Job {
	j := unfinishedLegacyJob(name, labelNode)
	j.Status.Succeeded = 1
	return j
}

// TestClearStaleUpgradeJobsFindsAnyName is the property the label selector
// buys over guessing one name.
//
// The previous version computed legacyJobName(node) and looked that up, so it
// only ever found Jobs named by the one scheme it knew about. Selecting on the
// node's label value finds every Job targeting the node, whatever named it —
// which matters because the naming this migrates from is not the only naming
// that has ever existed.
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

	// The Job this run is about to create must survive: nodejob.Run replaces
	// it itself, and deleting it here would race that.
	t.Run("the Job this run will create is left to nodejob.Run", func(t *testing.T) {
		cs := fake.NewSimpleClientset(finishedLegacyJob(jobName(node), node))

		if err := clearStaleUpgradeJobs(io.Discard, t.Context(), cs, node); err != nil {
			t.Fatalf("clearStaleUpgradeJobs: %v", err)
		}
		if _, err := cs.BatchV1().Jobs(cuNamespace).Get(t.Context(), jobName(node), metav1.GetOptions{}); err != nil {
			t.Errorf("the current Job was deleted: %v", err)
		}
	})

	// A running Job under any name stops the run, not just one we can name.
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

// TestClearStaleUpgradeJobsTrustsTheAnnotationOverTheLabel covers the one case
// the label selector cannot filter: a Job whose label selects for this node but
// whose annotation says it targets another.
//
// The label may be a hash, so it is a selector, not proof. The annotation is
// the node name verbatim, so it is the answer. Without this the run would
// delete — or be blocked by — a Job belonging to a different node.
func TestClearStaleUpgradeJobsTrustsTheAnnotationOverTheLabel(t *testing.T) {
	const mine, theirs = "cp1", "cp2"

	mislabelled := func(name string) *batchv1.Job {
		j := finishedLegacyJob(name, theirs)
		// Selected for cp1, owned by cp2.
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
