package k8sutil

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// HeldAnnotation records who held a node behind the startup taint and when,
// as <who>@<RFC3339 with nanoseconds> like VerifiedAnnotation; HeldReasonAnnotation records
// why. The reason is free text, so it has an annotation of its own rather
// than a place in a value readers split.
const (
	HeldAnnotation       = "stratum.ciq.com/held"
	HeldReasonAnnotation = "stratum.ciq.com/held-reason"
)

// ErrNotHeld is ReleaseNode's refusal of a node that does not carry the
// startup taint.
var ErrNotHeld = errors.New("the node does not carry the " + UnverifiedTaint + " taint")

// ErrHeldMeanwhile is ReleaseNode's refusal of a node that someone held
// between its first read and a conflict retry: releasing it would undo a hold
// placed after the operator checked the node.
var ErrHeldMeanwhile = errors.New("the node was held while it was being released, and the release would undo that hold; nothing was changed, check the hold and run again")

// ErrNodeReplaced is the refusal of a release or hold whose node was deleted
// and registered again while the update was being retried; nothing was
// changed on the new Node.
var ErrNodeReplaced = errors.New("the node was deleted and registered again during the update, and nothing was changed on the new one; check it and run again")

// ValidateNodeGateActor reports whether who can name the actor in a verified
// or held annotation, which readers split at its last '@': it must be
// non-empty and free of whitespace.
func ValidateNodeGateActor(who string) error {
	if who == "" {
		return errors.New("the actor must not be empty")
	}
	if strings.ContainsFunc(who, unicode.IsSpace) {
		return fmt.Errorf("the actor %q must not contain whitespace", who)
	}
	return nil
}

// ValidateHoldReason reports whether reason can explain a hold: it must say
// something besides whitespace.
func ValidateHoldReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("a hold must give a reason")
	}
	return nil
}

// gateStamp is the <who>@<RFC3339 with nanoseconds> the gate annotations
// carry. The fraction keeps two holds by one actor within a second apart, so a
// release retry can tell a newer hold from the one it read.
func gateStamp(who string, now time.Time) string {
	return who + "@" + now.UTC().Format(time.RFC3339Nano)
}

// hasUnverifiedTaint reports whether n carries the startup taint, with any
// effect.
func hasUnverifiedTaint(n *corev1.Node) bool {
	return slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == UnverifiedTaint })
}

// ReleaseResult is what ReleaseNode did: Removed reports that the node
// carried the startup taint and it was removed, which a forced re-verify of an
// untainted node does not; Cordoned reports that the released node is still
// cordoned, as a drain leaves it.
type ReleaseResult struct {
	Removed  bool
	Cordoned bool
}

// ReleaseNode lets workloads onto a node held behind the startup taint: it
// removes every taint keyed stratum.ciq.com/unverified, records who released
// it and when in stratum.ciq.com/verified, and removes the hold record and
// its reason, in one update against the resourceVersion it read, retried on a
// conflict. It refuses a node without the taint with ErrNotHeld unless force,
// which re-verifies it. A retry refuses with ErrHeldMeanwhile when the hold
// record or its reason changed since the first read, since that is a newer
// hold; a record that was cleared is another release, which the taint check
// decides. Every other taint and annotation, and a cordon, is left as it was.
func ReleaseNode(ctx context.Context, cs kubernetes.Interface, name, who string, force bool, now time.Time) (ReleaseResult, error) {
	if err := ValidateNodeGateActor(who); err != nil {
		return ReleaseResult{}, err
	}
	var removed, read bool
	var held [2]string
	n, err := updateNodeOnConflict(ctx, cs, name, func(n *corev1.Node) error {
		rec := [2]string{n.Annotations[HeldAnnotation], n.Annotations[HeldReasonAnnotation]}
		if !read {
			held, read = rec, true
		} else if rec != held && rec[0] != "" {
			return fmt.Errorf("release %s: %w", name, ErrHeldMeanwhile)
		}
		removed = hasUnverifiedTaint(n)
		if !removed && !force {
			return fmt.Errorf("release %s: %w", name, ErrNotHeld)
		}
		n.Spec.Taints = slices.DeleteFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == UnverifiedTaint })
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[VerifiedAnnotation] = gateStamp(who, now)
		delete(n.Annotations, HeldAnnotation)
		delete(n.Annotations, HeldReasonAnnotation)
		return nil
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	return ReleaseResult{Removed: removed, Cordoned: n.Spec.Unschedulable}, nil
}

// HoldNode keeps new workloads off a node until it is released again: it
// adds stratum.ciq.com/unverified=true:NoSchedule, or sets the value of a
// NoSchedule taint the node already carries under that key to true, since a
// burn-in job tolerates the value the startup gate gives, removes the verified
// record, and
// records who held it and when in stratum.ciq.com/held and why in
// stratum.ciq.com/held-reason, in one update against the resourceVersion it
// read, retried on a conflict. NoSchedule leaves running pods where they are.
// Every other taint and annotation is left as it was.
func HoldNode(ctx context.Context, cs kubernetes.Interface, name, who, reason string, now time.Time) error {
	if err := ValidateNodeGateActor(who); err != nil {
		return err
	}
	if err := ValidateHoldReason(reason); err != nil {
		return err
	}
	_, err := updateNodeOnConflict(ctx, cs, name, func(n *corev1.Node) error {
		hold := corev1.Taint{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}
		if i := slices.IndexFunc(n.Spec.Taints, func(t corev1.Taint) bool { return hold.MatchTaint(&t) }); i >= 0 {
			n.Spec.Taints[i].Value = hold.Value
		} else {
			n.Spec.Taints = append(n.Spec.Taints, hold)
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		delete(n.Annotations, VerifiedAnnotation)
		n.Annotations[HeldAnnotation] = gateStamp(who, now)
		n.Annotations[HeldReasonAnnotation] = strings.TrimSpace(reason)
		return nil
	})
	return err
}

// updateNodeOnConflict reads the node, applies mutate, and updates it with the
// read's resourceVersion, starting over from a fresh read when another writer
// changed it in between. It returns the Node the update wrote. A retry that
// reads a Node with a different UID refuses with ErrNodeReplaced: the node
// was deleted and another registered under its name, and the operator acted
// on the first one.
func updateNodeOnConflict(ctx context.Context, cs kubernetes.Interface, name string, mutate func(*corev1.Node) error) (*corev1.Node, error) {
	var updated *corev1.Node
	var uid types.UID
	read := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !read {
			uid, read = n.UID, true
		} else if n.UID != uid {
			return fmt.Errorf("%s: %w", name, ErrNodeReplaced)
		}
		if err := mutate(n); err != nil {
			return err
		}
		updated, err = cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		return err
	})
	return updated, err
}
