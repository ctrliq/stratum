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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// HeldAnnotation records who held a node behind the startup taint, when, and
// why: a taint's value must be a label value, which a reason is not.
const HeldAnnotation = "stratum.ciq.com/held"

// ErrNotHeld is ReleaseNode's refusal of a node that does not carry the
// startup taint.
var ErrNotHeld = errors.New("the node does not carry the " + UnverifiedTaint + " taint")

// ValidateNodeGateActor reports whether who can name the actor in a verified
// or held annotation, which readers split at its last '@': it must be
// non-empty and free of whitespace.
func ValidateNodeGateActor(who string) error {
	if who == "" {
		return errors.New("--by must name who is acting")
	}
	if strings.ContainsFunc(who, unicode.IsSpace) {
		return fmt.Errorf("--by %q must not contain whitespace", who)
	}
	return nil
}

// gateStamp is the <who>@<RFC3339> the gate annotations carry.
func gateStamp(who string, now time.Time) string {
	return who + "@" + now.UTC().Format(time.RFC3339)
}

// hasUnverifiedTaint reports whether n carries the startup taint, with any
// effect.
func hasUnverifiedTaint(n *corev1.Node) bool {
	return slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == UnverifiedTaint })
}

// ReleaseNode lets workloads onto a node held behind the startup taint: it
// removes every taint keyed stratum.ciq.com/unverified, records who released
// it and when in stratum.ciq.com/verified, and clears the hold record, in one
// update against the resourceVersion it read, retried on a conflict. It
// refuses a node without the taint with ErrNotHeld unless force, which
// re-verifies it. Every other taint and annotation is left as it was.
func ReleaseNode(ctx context.Context, cs kubernetes.Interface, name, who string, force bool, now time.Time) error {
	if err := ValidateNodeGateActor(who); err != nil {
		return err
	}
	return updateNodeOnConflict(ctx, cs, name, func(n *corev1.Node) error {
		if !hasUnverifiedTaint(n) && !force {
			return fmt.Errorf("release %s: %w; pass --force to record it as verified anyway", name, ErrNotHeld)
		}
		n.Spec.Taints = slices.DeleteFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == UnverifiedTaint })
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[VerifiedAnnotation] = gateStamp(who, now)
		delete(n.Annotations, HeldAnnotation)
		return nil
	})
}

// HoldNode keeps new workloads off a node until it is released again: it
// adds stratum.ciq.com/unverified=true:NoSchedule unless the node already
// carries the taint, removes the verified record, and records who held it,
// when and why in stratum.ciq.com/held, in one update against the
// resourceVersion it read, retried on a conflict. NoSchedule leaves running
// pods where they are. Every other taint and annotation is left as it was.
func HoldNode(ctx context.Context, cs kubernetes.Interface, name, who, reason string, now time.Time) error {
	if err := ValidateNodeGateActor(who); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("--reason must say why the node is held")
	}
	return updateNodeOnConflict(ctx, cs, name, func(n *corev1.Node) error {
		if !hasUnverifiedTaint(n) {
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule})
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		delete(n.Annotations, VerifiedAnnotation)
		n.Annotations[HeldAnnotation] = gateStamp(who, now) + ": " + strings.TrimSpace(reason)
		return nil
	})
}

// updateNodeOnConflict reads the node, applies mutate, and updates it with the
// read's resourceVersion, starting over from a fresh read when another writer
// changed it in between.
func updateNodeOnConflict(ctx context.Context, cs kubernetes.Interface, name string, mutate func(*corev1.Node) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := mutate(n); err != nil {
			return err
		}
		_, err = cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		return err
	})
}
