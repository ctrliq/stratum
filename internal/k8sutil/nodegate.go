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

// The reason is free text, so it lives beside the <who>@<stamp> value readers split at '@'.
const (
	HeldAnnotation       = "stratum.ciq.com/held"
	HeldReasonAnnotation = "stratum.ciq.com/held-reason"
)

var ErrNotHeld = errors.New("the node does not carry the " + UnverifiedTaint + " taint")

var ErrHeldMeanwhile = errors.New("the node was held while it was being released, and the release would undo that hold; nothing was changed, check the hold and run again")

var ErrNodeReplaced = errors.New("the node was deleted and registered again during the update, and nothing was changed on the new one; check it and run again")

// Readers split the stamp at its last '@', so the actor must not contain whitespace.
func ValidateNodeGateActor(who string) error {
	if who == "" {
		return errors.New("the actor must not be empty")
	}
	if strings.ContainsFunc(who, unicode.IsSpace) {
		return fmt.Errorf("the actor %q must not contain whitespace", who)
	}
	return nil
}

func ValidateHoldReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("a hold must give a reason")
	}
	return nil
}

// Nanoseconds keep two holds by one actor within a second tellable apart.
func gateStamp(who string, now time.Time) string {
	return who + "@" + now.UTC().Format(time.RFC3339Nano)
}

func hasUnverifiedTaint(n *corev1.Node) bool {
	return slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == UnverifiedTaint })
}

// Removed is false for a forced re-verify of an untainted node; Cordoned is the node's cordon.
type ReleaseResult struct {
	Removed  bool
	Cordoned bool
}

// Refuses an untainted node with ErrNotHeld unless force; retries refuse a newer hold.
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

// Sets the startup taint's value to true, which a burn-in job tolerates, and records the hold.
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

// Retries on conflict; a different UID on retry refuses with ErrNodeReplaced.
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
