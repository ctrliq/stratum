package ha

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/bootstrap"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/version"
)

// --- the join fence admission policy ---
//
// The stratum:certs-reader Role gives every control-plane join token create,
// update and delete on the maintenance leases, and RBAC cannot restrict the
// create by name. The JoinFencePolicy admission policy is what keeps a token
// to its own fence, so the mint must never publish those rules without it. Its
// CEL is evaluated by a real apiserver in internal/operator's
// TestJoinTokenHoldsOnlyItsOwnMaintenanceFence; the fake clientset here
// emulates the policy's rules (joinFencePolicyDenial) the way
// leaseRequestAllowed emulates RBAC's, with the denial the real apiserver
// answers.

// TestUploadCertsPutsTheJoinFencePolicyInPlaceBeforeTheLeaseGrant: the mint
// writes the admission policy and a binding that denies on it before the Role
// that grants join tokens the leases, so there is no moment at which the grant
// exists without the policy.
func TestUploadCertsPutsTheJoinFencePolicyInPlaceBeforeTheLeaseGrant(t *testing.T) {
	cs := uploadFixture(t)
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatal(err)
	}
	first := map[string]int{}
	for i, a := range cs.Actions() {
		if a.GetVerb() != "create" && a.GetVerb() != "update" {
			continue
		}
		if _, seen := first[a.GetResource().Resource]; !seen {
			first[a.GetResource().Resource] = i
		}
	}
	policyAt, okP := first["validatingadmissionpolicies"]
	bindingAt, okB := first["validatingadmissionpolicybindings"]
	roleAt, okR := first["roles"]
	if !okP || !okB || !okR {
		t.Fatalf("the mint wrote policy=%v binding=%v role=%v, want all three", okP, okB, okR)
	}
	if policyAt > roleAt || bindingAt > roleAt {
		t.Errorf("the Role was written at action %d, before the policy (%d) or its binding (%d)", roleAt, policyAt, bindingAt)
	}
	joinFencePolicyInPlace(t, cs)
}

// TestUploadCertsGrantsNoLeasesWithoutTheJoinFencePolicy: a mint that cannot
// put the policy or its binding in place fails before the Role, so neither a
// fresh cluster nor one whose Role predates the lease grant gains lease rules,
// and no Secret is written under a grant the mint did not finish.
func TestUploadCertsGrantsNoLeasesWithoutTheJoinFencePolicy(t *testing.T) {
	for _, resource := range []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"} {
		for _, existingRole := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s fails, Role present=%v", resource, existingRole), func(t *testing.T) {
				cs := uploadFixture(t)
				if existingRole {
					old := &rbacv1.Role{
						ObjectMeta: metav1.ObjectMeta{Name: "stratum:certs-reader", Namespace: "kube-system"},
						Rules: []rbacv1.PolicyRule{{
							APIGroups: []string{""}, Resources: []string{"secrets"},
							ResourceNames: []string{certsSecret}, Verbs: []string{"get"},
						}},
					}
					if _, err := cs.RbacV1().Roles("kube-system").Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				boom := apierrors.NewServiceUnavailable("admission registration is down")
				cs.PrependReactor("create", resource, func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
				err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil)
				if !errors.Is(err, boom) || !strings.Contains(err.Error(), k8sutil.JoinFencePolicy) {
					t.Fatalf("upload = %v, want the failure to put %s in place", err, k8sutil.JoinFencePolicy)
				}
				role, gerr := cs.RbacV1().Roles("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{})
				switch {
				case !existingRole && !apierrors.IsNotFound(gerr):
					t.Errorf("get the Role after the failed mint: %v, want NotFound", gerr)
				case existingRole && gerr != nil:
					t.Fatal(gerr)
				case existingRole && len(role.Rules) != 1:
					t.Errorf("the Role carries %d rules after the failed mint, want only its secrets rule: %+v", len(role.Rules), role.Rules)
				}
				if _, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), certsSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Errorf("get %s after the failed mint: %v, want NotFound", certsSecret, err)
				}
			})
		}
	}
}

// TestUploadCertsConvergesADriftedJoinFencePolicy: create-if-absent is not
// enough. A policy whose rules were edited, or a binding that only warns or
// audits, leaves the lease grant unconstrained, so the mint rewrites both to
// the shipped spec before the Role.
func TestUploadCertsConvergesADriftedJoinFencePolicy(t *testing.T) {
	cs := uploadFixture(t)
	adm := cs.AdmissionregistrationV1()
	drifted := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: k8sutil.JoinFencePolicy},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			MatchConditions: []admissionregistrationv1.MatchCondition{{Name: "never", Expression: "false"}},
			Validations:     []admissionregistrationv1.Validation{{Expression: "true"}},
		},
	}
	if _, err := adm.ValidatingAdmissionPolicies().Create(context.Background(), drifted, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	warnOnly := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: k8sutil.JoinFencePolicy},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        k8sutil.JoinFencePolicy,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn},
		},
	}
	if _, err := adm.ValidatingAdmissionPolicyBindings().Create(context.Background(), warnOnly, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatal(err)
	}
	p := joinFencePolicyInPlace(t, cs)
	for _, c := range p.Spec.MatchConditions {
		if c.Expression == "false" {
			t.Errorf("the drifted match condition %q survived the mint", c.Name)
		}
	}
}

// joinFencePolicyInPlace fails unless the policy and a binding that denies on
// it, with nothing narrowing what it matches, are stored, and returns the
// policy.
func joinFencePolicyInPlace(t *testing.T, cs kubernetes.Interface) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	adm := cs.AdmissionregistrationV1()
	p, err := adm.ValidatingAdmissionPolicies().Get(context.Background(), k8sutil.JoinFencePolicy, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the %s policy: %v", k8sutil.JoinFencePolicy, err)
	}
	if p.Spec.FailurePolicy == nil || *p.Spec.FailurePolicy != admissionregistrationv1.Fail || len(p.Spec.Validations) < 6 {
		t.Errorf("the %s policy fails %v with %d validations, want Fail with the six lease rules", k8sutil.JoinFencePolicy, p.Spec.FailurePolicy, len(p.Spec.Validations))
	}
	b, err := adm.ValidatingAdmissionPolicyBindings().Get(context.Background(), k8sutil.JoinFencePolicy, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the %s binding: %v", k8sutil.JoinFencePolicy, err)
	}
	if b.Spec.PolicyName != k8sutil.JoinFencePolicy || !slices.Equal(b.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) ||
		b.Spec.MatchResources != nil || b.Spec.ParamRef != nil {
		t.Errorf("the %s binding = %+v, want it to deny on its policy and nothing else", k8sutil.JoinFencePolicy, b.Spec)
	}
	return p
}

// joinFencePolicyDenial emulates the JoinFencePolicy admission policy for a
// lease request by username, a member of the control-plane bootstrap group:
// nil when the policy admits it, otherwise the denial in the shape the
// apiserver's admission dispatcher answers with (an Invalid status naming the
// policy and its binding, which the envtest case holds the real one to).
// stored is the lease as the apiserver has it, nil when absent.
func joinFencePolicyDenial(username string, action k8stesting.Action, stored *coordinationv1.Lease) error {
	if action.GetNamespace() != "kube-system" {
		return nil
	}
	var obj *coordinationv1.Lease
	switch a := action.(type) {
	case k8stesting.CreateAction:
		obj, _ = a.GetObject().(*coordinationv1.Lease)
	case k8stesting.UpdateAction:
		obj, _ = a.GetObject().(*coordinationv1.Lease)
	case k8stesting.DeleteAction:
		if stored == nil {
			return nil
		}
	default:
		return nil
	}
	own := username + "/"
	holderOf := func(l *coordinationv1.Lease) string {
		if l == nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}
	var name string
	if obj != nil {
		name = obj.Name
	} else {
		name = stored.Name
	}
	deny := func(msg string) error {
		m := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", k8sutil.JoinFencePolicy, k8sutil.JoinFencePolicy, msg)
		err := apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, name, errors.New(m))
		err.ErrStatus.Reason, err.ErrStatus.Code = metav1.StatusReasonInvalid, http.StatusUnprocessableEntity
		err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{Message: m})
		return err
	}
	if name != "stratum-cluster-maintenance" && name != "stratum-cluster-upgrade" {
		return deny("a control-plane join token may write only the maintenance leases")
	}
	verb := action.GetVerb()
	if verb != "delete" {
		if len(obj.OwnerReferences) != 0 || len(obj.Finalizers) != 0 {
			return deny("a control-plane join token may not set owner references or finalizers on a maintenance lease")
		}
		if obj.Spec.LeaseDurationSeconds == nil || *obj.Spec.LeaseDurationSeconds > int32(maintlease.Duration/time.Second) {
			return deny("a control-plane join token may record a maintenance lease duration of at most 60 seconds")
		}
		if !strings.HasPrefix(holderOf(obj), own) {
			return deny("a control-plane join token may name only itself as a maintenance lease's holder")
		}
	}
	if old := holderOf(stored); verb != "create" && !strings.HasPrefix(old, own) && (verb != "update" || old != "") {
		return deny("a control-plane join token may not take over or delete a maintenance lease another run holds")
	}
	return nil
}

// joinerClientset is a fake apiserver as a control-plane join token sees it:
// lease requests pass RBAC under role (leaseRequestAllowed) and then the join
// fence policy for user system:bootstrap:<tokenID>. Leases the test seeds
// through the tracker come from some other identity, which neither applies to.
func joinerClientset(role *rbacv1.Role, tokenID string) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
	cs.PrependReactor("*", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := ""
		switch a := action.(type) {
		case k8stesting.GetAction:
			name = a.GetName()
		case k8stesting.UpdateAction:
			name = a.GetObject().(metav1.Object).GetName()
		case k8stesting.DeleteAction:
			name = a.GetName()
		}
		if !leaseRequestAllowed(role, action.GetVerb(), name) {
			return true, nil, apierrors.NewForbidden(gvr.GroupResource(), name,
				fmt.Errorf("User %q cannot %s resource \"leases\"", "system:bootstrap:"+tokenID, action.GetVerb()))
		}
		if name == "" {
			if c, ok := action.(k8stesting.CreateAction); ok {
				name = c.GetObject().(metav1.Object).GetName()
			}
		}
		var stored *coordinationv1.Lease
		if o, err := cs.Tracker().Get(gvr, action.GetNamespace(), name); err == nil {
			stored = o.(*coordinationv1.Lease)
		}
		if err := joinFencePolicyDenial("system:bootstrap:"+tokenID, action, stored); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	return cs
}

// publishedCertsReaderRole is the stratum:certs-reader Role a mint publishes.
func publishedCertsReaderRole(t *testing.T) *rbacv1.Role {
	t.Helper()
	cs := uploadFixture(t)
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatal(err)
	}
	role, err := cs.RbacV1().Roles("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return role
}

// joinFenceFixture writes the bootstrap kubeconfig a join carries for token
// <tokenID>.<secret> and points the join fence's client at cs.
func joinFenceFixture(t *testing.T, cs kubernetes.Interface, tokenID string) {
	t.Helper()
	origConf, origBoot := version.ConfDir, version.BootstrapKubeconfig
	version.ConfDir = t.TempDir()
	version.BootstrapKubeconfig = filepath.Join(version.ConfDir, "bootstrap.conf")
	origClient := joinFenceClientset
	t.Cleanup(func() {
		version.ConfDir, version.BootstrapKubeconfig, joinFenceClientset = origConf, origBoot, origClient
	})
	if err := bootstrap.WriteBootstrapKubeconfig("https://10.0.0.10:6443", tokenID+".0123456789abcdef", []byte("ca")); err != nil {
		t.Fatal(err)
	}
	joinFenceClientset = func(path string) (kubernetes.Interface, error) {
		if path != version.BootstrapKubeconfig {
			return nil, fmt.Errorf("the fence client was built from %s, want the bootstrap kubeconfig %s", path, version.BootstrapKubeconfig)
		}
		return cs, nil
	}
}

func seedLease(t *testing.T, cs *fake.Clientset, name, holder string, renewed time.Time) {
	t.Helper()
	at := metav1.NewMicroTime(renewed)
	secs := int32(maintlease.Duration / time.Second)
	l := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, AcquireTime: &at, RenewTime: &at},
	}
	if err := cs.Tracker().Add(l); err != nil {
		t.Fatal(err)
	}
}

// TestJoinFenceHolderNamesTheBootstrapToken: the join takes its fence under
// a holder that starts with its bootstrap token's username, so the policy
// admits it, and renews and releases it as its own; the rest of the identity
// is MaintenanceHolder's host/pid/nanoseconds, distinct per take.
func TestJoinFenceHolderNamesTheBootstrapToken(t *testing.T) {
	cs := joinerClientset(publishedCertsReaderRole(t), "abc123")
	joinFenceFixture(t, cs, "abc123")
	lease, err := takeJoinMaintenanceFence()
	if err != nil {
		t.Fatalf("take the join fence: %v", err)
	}
	for _, name := range []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"} {
		l, err := cs.CoordinationV1().Leases("kube-system").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		holder := *l.Spec.HolderIdentity
		rest, ok := strings.CutPrefix(holder, "system:bootstrap:abc123/")
		if !ok || strings.Count(rest, "/") != 2 {
			t.Errorf("%s holder = %q, want system:bootstrap:abc123/<host>/<pid>/<nanoseconds>", name, holder)
		}
	}
	if err := lease.Confirm(context.Background()); err != nil {
		t.Errorf("renew the join fence: %v", err)
	}
	lease.Release()
	for _, name := range []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"} {
		if _, err := cs.Tracker().Get(coordinationv1.SchemeGroupVersion.WithResource("leases"), "kube-system", name); !apierrors.IsNotFound(err) {
			t.Errorf("%s after the release: %v, want NotFound", name, err)
		}
	}
}

// TestControlPlaneJoinRefusesAMaintenanceLeaseAnUnfinishedRunLeft: a run that
// died holding the fence leaves an expired lease naming its own holder, and
// the policy never lets a join token take over a lease another run holds. The
// join refuses before the member-add, naming the lease and its holder and the
// two ways out — any control-plane mint's fence takes the lease over and
// releases it, or deleting it by hand — and says nothing was added. It is not
// the refusal for a Role that predates the lease grant: this token's Role
// grants the leases.
func TestControlPlaneJoinRefusesAMaintenanceLeaseAnUnfinishedRunLeft(t *testing.T) {
	joinFixture(t)
	cs := joinerClientset(publishedCertsReaderRole(t), "abc123")
	seedLease(t, cs, "stratum-cluster-upgrade", "cp1/4242/1", time.Now().Add(-10*maintlease.Duration))
	joinFenceFixture(t, cs, "abc123")
	r := &recordedJoin{}
	fx := fakeJoinEffects(r)
	fx.takeMaintenanceFence = takeJoinMaintenanceFence
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx)
	if err == nil {
		t.Fatal("the join took over a maintenance lease another run left")
	}
	for _, want := range []string{
		"kube-system/stratum-cluster-upgrade",
		"cp1/4242/1",
		"did not finish",
		"`stratum token create --control-plane`",
		"`kubectl -n kube-system delete lease stratum-cluster-upgrade`",
		"nothing was added to etcd",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "predates") || apierrors.IsForbidden(err) {
		t.Errorf("the refusal reads as a Role that predates the lease grant: %v", err)
	}
	if r.addCalls != 0 || r.saved != nil {
		t.Fatalf("the refused join touched membership: adds=%d saved=%+v", r.addCalls, r.saved)
	}
	if _, statErr := os.Stat(joinIntentPath); statErr != nil {
		t.Fatalf("the intent journal did not survive the refused join: %v", statErr)
	}
	if l, err := cs.CoordinationV1().Leases("kube-system").Get(context.Background(), "stratum-cluster-upgrade", metav1.GetOptions{}); err != nil || *l.Spec.HolderIdentity != "cp1/4242/1" {
		t.Errorf("the left lease after the refused join = %v (%v), want it untouched", l, err)
	}
}

// TestControlPlaneJoinStillRefusesARoleThatPredatesTheLeaseGrant: through the
// production fence take, a token whose Role carries only the secrets rule is
// refused its first lease read, and the join says the command was minted by a
// stratum that predates the grant.
func TestControlPlaneJoinStillRefusesARoleThatPredatesTheLeaseGrant(t *testing.T) {
	joinFixture(t)
	released := &rbacv1.Role{Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{certsSecret}, Verbs: []string{"get"},
	}}}
	cs := joinerClientset(released, "abc123")
	joinFenceFixture(t, cs, "abc123")
	r := &recordedJoin{}
	fx := fakeJoinEffects(r)
	fx.takeMaintenanceFence = takeJoinMaintenanceFence
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "predates the member-add fence grant") {
		t.Fatalf("err = %v, want the refusal for a Role that predates the lease grant", err)
	}
	if r.addCalls != 0 || r.saved != nil {
		t.Fatalf("the refused join touched membership: adds=%d saved=%+v", r.addCalls, r.saved)
	}
}

// TestJoinFenceRefusalNamesEveryLeaseLeft: when an unfinished run left both
// maintenance leases, the refusal names both, with their holders, and the
// delete covers both, so one way out clears the whole fence.
func TestJoinFenceRefusalNamesEveryLeaseLeft(t *testing.T) {
	cs := joinerClientset(publishedCertsReaderRole(t), "abc123")
	stale := time.Now().Add(-10 * maintlease.Duration)
	seedLease(t, cs, "stratum-cluster-maintenance", "cp1/4242/1", stale)
	seedLease(t, cs, "stratum-cluster-upgrade", "system:bootstrap:def456/cp2/7/1", stale)
	joinFenceFixture(t, cs, "abc123")
	_, err := takeJoinMaintenanceFence()
	if !k8sutil.JoinFenceDenied(err) {
		t.Fatalf("take = %v, want the join fence policy's denial, wrapped", err)
	}
	for _, want := range []string{
		"kube-system/stratum-cluster-maintenance (holder cp1/4242/1",
		"kube-system/stratum-cluster-upgrade (holder system:bootstrap:def456/cp2/7/1",
		"delete them with `kubectl -n kube-system delete lease stratum-cluster-maintenance stratum-cluster-upgrade`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// TestJoinFenceRefusesAKubeconfigWithoutABootstrapToken: a holder without the
// token's username is one the policy refuses, so a bootstrap kubeconfig that
// carries no <id>.<secret> token refuses the take before any lease is
// written, rather than taking it under a holder the token cannot release.
func TestJoinFenceRefusesAKubeconfigWithoutABootstrapToken(t *testing.T) {
	cs := fake.NewSimpleClientset()
	joinFenceFixture(t, cs, "abc123")
	if err := bootstrap.WriteBootstrapKubeconfig("https://10.0.0.10:6443", "not-a-bootstrap-token", []byte("ca")); err != nil {
		t.Fatal(err)
	}
	if _, err := takeJoinMaintenanceFence(); err == nil || !strings.Contains(err.Error(), "carries no bootstrap token") {
		t.Fatalf("take = %v, want the refusal for a kubeconfig without a bootstrap token", err)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("the refused take made %d API calls, want none: %v", n, cs.Actions())
	}
}
