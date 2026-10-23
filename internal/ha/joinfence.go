package ha

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/k8sutil"
)

// joinFencePolicy is the k8sutil.JoinFencePolicy admission policy and its
// binding as addons.RenderJoinFencePolicy ships them.
func joinFencePolicy() (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	docs := bytes.Split(addons.RenderJoinFencePolicy(), []byte("\n---\n"))
	if len(docs) != 2 {
		return nil, nil, fmt.Errorf("the %s stack has %d documents, want the policy and its binding", k8sutil.JoinFencePolicy, len(docs))
	}
	var p admissionregistrationv1.ValidatingAdmissionPolicy
	var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
	if err := yaml.UnmarshalStrict(docs[0], &p); err != nil {
		return nil, nil, fmt.Errorf("decode the %s policy: %w", k8sutil.JoinFencePolicy, err)
	}
	if err := yaml.UnmarshalStrict(docs[1], &b); err != nil {
		return nil, nil, fmt.Errorf("decode the %s binding: %w", k8sutil.JoinFencePolicy, err)
	}
	return &p, &b, nil
}

// ensureJoinFencePolicy creates the join fence admission policy and its
// binding, or rewrites existing ones whose spec is not exactly the shipped
// one. Create-if-absent is not enough: an edited policy, or a binding that
// only warns or audits, would leave the lease rules of the stratum:certs-reader
// Role unconstrained. The comparison is the whole spec, so the shipped YAML
// spells out every field the apiserver defaults; a re-run against a policy
// already in place writes nothing.
func ensureJoinFencePolicy(ctx context.Context, cs kubernetes.Interface) error {
	want, wantBinding, err := joinFencePolicy()
	if err != nil {
		return err
	}
	policies := cs.AdmissionregistrationV1().ValidatingAdmissionPolicies()
	if _, err := policies.Create(ctx, want, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := policies.Get(ctx, want.Name, metav1.GetOptions{})
			if err != nil || reflect.DeepEqual(cur.Spec, want.Spec) {
				return err
			}
			cur.Spec = want.Spec
			_, err = policies.Update(ctx, cur, metav1.UpdateOptions{})
			return err
		}); err != nil {
			return err
		}
	}
	bindings := cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings()
	if _, err := bindings.Create(ctx, wantBinding, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := bindings.Get(ctx, wantBinding.Name, metav1.GetOptions{})
			if err != nil || reflect.DeepEqual(cur.Spec, wantBinding.Spec) {
				return err
			}
			cur.Spec = wantBinding.Spec
			_, err = bindings.Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
	}
	return nil
}

// awaitJoinFenceEnforcement is indirected for fake-client upload tests, which
// cannot model independent apiserver admission caches. The real admission
// boundary is also exercised with envtest.
var awaitJoinFenceEnforcement = waitJoinFenceEnforcement

// waitJoinFenceEnforcement probes each apiserver directly, without a VIP or
// service load balancer. RBAC and admission have separate informer caches:
// persisting a policy and binding does not make the new lease rights safe.
func waitJoinFenceEnforcement(ctx context.Context, cs kubernetes.Interface) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport, ok := cs.CoreV1().RESTClient().(*rest.RESTClient)
	if !ok || transport == nil {
		return fmt.Errorf("cannot verify join fence admission without an API transport")
	}
	want, _, err := joinFencePolicy()
	if err != nil {
		return err
	}
	// This validation was added with the metadata protections. Its exact
	// message proves that the current policy, not the older policy that only
	// checked holder/name/duration, is enforcing the binding's Deny action.
	message := want.Spec.Validations[1].Message
	probe := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "stratum-join-fence-probe-", Namespace: "kube-system", Finalizers: []string{"stratum.ciq.com/admission-probe"}},
	}
	var last error
	for {
		targets, err := joinFenceAPIServers(ctx, cs)
		if err != nil {
			last = err
		} else {
			last = nil
			for _, target := range targets {
				peer, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: target}, transport.Client)
				if err != nil {
					return err
				}
				// Probe each group separately: a combined identity would match the
				// control-plane selector even while workers remain unprotected
				// through a legacy RoleBinding or a stale RBAC cache.
				for _, group := range []string{k8sutil.ControlPlaneBootstrapGroup, k8sutil.BootstrapGroup} {
					attempt, stop := context.WithTimeout(ctx, 5*time.Second)
					err = peer.CoordinationV1().RESTClient().Post().Namespace("kube-system").Resource("leases").
						Param("dryRun", metav1.DryRunAll).
						SetHeader("Impersonate-User", "system:bootstrap:stratum-fence-probe").
						SetHeader("Impersonate-Group", "system:masters", group).
						Body(probe).Do(attempt).Error()
					stop()
					// The masters group authorizes the probe before the token Role exists;
					// admission still matches the bootstrap group. A plain RBAC denial,
					// compile failure, or denial by another policy is not enforcement proof.
					if !k8sutil.JoinFenceDenied(err) || !strings.Contains(err.Error(), message) {
						last = fmt.Errorf("apiserver %s has not enforced the join fence metadata policy for %s (probe result: %v)", target, group, err)
						break
					}
				}
				if last != nil {
					break
				}
			}
			if last == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for join fence admission: %v: %w", last, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func joinFenceAPIServers(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	endpoints, err := cs.CoreV1().Endpoints("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, subset := range endpoints.Subsets {
		for _, port := range subset.Ports {
			if port.Name != "https" || port.Protocol != corev1.ProtocolTCP {
				continue
			}
			for _, address := range append(subset.Addresses, subset.NotReadyAddresses...) {
				if net.ParseIP(address.IP) == nil || port.Port <= 0 || port.Port > 65535 {
					return nil, fmt.Errorf("invalid apiserver endpoint %q:%d", address.IP, port.Port)
				}
				targets = append(targets, "https://"+net.JoinHostPort(address.IP, strconv.Itoa(int(port.Port))))
			}
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("the kubernetes Service publishes no API servers; cannot verify join fence admission")
	}
	return targets, nil
}
