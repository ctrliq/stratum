package operator

// joinfence_envtest_test.go — the admission policy that holds a control-plane
// join token to its own maintenance fence, evaluated by a real apiserver.
//
// The mint's stratum:certs-reader Role lets the control-plane bootstrap group
// create, renew and release the two maintenance leases, so a joining control
// plane can fence its etcd member-add with its bootstrap token. RBAC cannot
// restrict a create by name, and a join token is printed on stdout, so the
// Role alone would let anyone holding one squat any Lease in kube-system or
// overwrite the holder of a running rotation's or upgrade's lease. The policy
// ha.GrantCertsReader installs is what forbids that; its CEL is a security
// control, so it is exercised here through real bootstrap-token
// authentication, the real Role, and real admission rather than a fake. The
// case lives with this package's envtest cases because those are the ones
// `make test-envtest` runs.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
)

const (
	fenceNS          = "kube-system"
	fenceCommon      = "stratum-cluster-maintenance"
	fenceLegacy      = "stratum-cluster-upgrade"
	fenceTokenA      = "abc123"
	fenceTokenB      = "def456"
	fenceTokenSecret = "0123456789abcdef"
)

// TestJoinTokenHoldsOnlyItsOwnMaintenanceFence: a control-plane join token,
// authenticated as the apiserver authenticates it, may create, renew and
// release only the two maintenance leases, only for at most stratum's lease
// duration, and only under a holder that names the token; it may not
// overwrite or delete a lease another run holds, live or expired. Every other
// identity writes leases exactly as before.
func TestJoinTokenHoldsOnlyItsOwnMaintenanceFence(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make test-envtest`")
	}
	// Kubernetes rejects loopback addresses in Service endpoints. Bind envtest
	// to a local unicast address so production can probe the advertised peer.
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var listenIP string
	for _, address := range addresses {
		if ipnet, ok := address.(*net.IPNet); ok && ipnet.IP.To4() != nil && ipnet.IP.IsGlobalUnicast() {
			listenIP = ipnet.IP.String()
			break
		}
	}
	if listenIP == "" {
		t.Fatal("admission envtest needs a local unicast address for its Service endpoint")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(listenIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	_, listenPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().SecureServing.Address = listenIP
	env.ControlPlane.GetAPIServer().SecureServing.Port = listenPort
	env.ControlPlane.GetAPIServer().Configure().Set("enable-bootstrap-token-auth", "true")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := ha.GrantCertsReader(ctx, admin); err != nil {
		t.Fatalf("grant the control-plane bootstrap group its Role: %v", err)
	}
	joinerA := fenceJoinClient(t, cfg, admin, fenceTokenA, true)
	joinerB := fenceJoinClient(t, cfg, admin, fenceTokenB, true)

	t.Run("legacy worker lease rights remain fenced when binding migration fails", func(t *testing.T) {
		worker := fenceJoinClient(t, cfg, admin, "fed321", false)
		binding, err := admin.RbacV1().RoleBindings(fenceNS).Get(ctx, "stratum:certs-reader", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		binding.Subjects = []rbacv1.Subject{{Kind: "Group", Name: k8sutil.BootstrapGroup, APIGroup: rbacv1.GroupName}}
		if _, err := admin.RbacV1().RoleBindings(fenceNS).Update(ctx, binding, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		role, err := admin.RbacV1().Roles(fenceNS).Get(ctx, "stratum:certs-reader", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Earlier releases granted only the shared Secret. The re-upload adds
		// lease rights before it can narrow the legacy worker binding.
		role.Rules = role.Rules[:1]
		if _, err := admin.RbacV1().Roles(fenceNS).Update(ctx, role, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		wcfg := rest.CopyConfig(cfg)
		wcfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/rolebindings/stratum:certs-reader") {
					return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","code":403,"message":"injected binding update refusal"}`))}, nil
				}
				return rt.RoundTrip(r)
			})
		})
		failing, err := kubernetes.NewForConfig(wcfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := ha.GrantCertsReader(ctx, failing); !apierrors.IsForbidden(err) {
			t.Fatalf("migration = %v, want binding refusal", err)
		}
		defer func() {
			fenceClear(t, admin)
			_ = admin.CoordinationV1().Leases(fenceNS).Delete(ctx, "legacy-worker-squat", metav1.DeleteOptions{})
			if err := ha.GrantCertsReader(ctx, admin); err != nil {
				t.Errorf("finish binding migration: %v", err)
			}
		}()
		binding, err = admin.RbacV1().RoleBindings(fenceNS).Get(ctx, binding.Name, metav1.GetOptions{})
		if err != nil || len(binding.Subjects) != 1 || binding.Subjects[0].Name != k8sutil.BootstrapGroup {
			t.Fatalf("failed migration did not leave legacy binding: %+v, %v", binding, err)
		}
		secs := int32(maintlease.Duration / time.Second)
		fenceSeed(t, admin, fenceLease(fenceCommon, "controller/active/1", &secs, time.Now()))
		// Wait for the Role cache to grant GET, proving the worker really has
		// the expanded lease rules rather than relying on an RBAC refusal.
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err = worker.CoordinationV1().Leases(fenceNS).Get(ctx, fenceCommon, metav1.GetOptions{})
			if err == nil {
				break
			}
			if !apierrors.IsForbidden(err) || time.Now().After(deadline) {
				t.Fatalf("worker never inherited lease rights: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		_, err = worker.CoordinationV1().Leases(fenceNS).Create(ctx, fenceLease("legacy-worker-squat", "system:bootstrap:fed321/h/1/1", &secs, time.Now()), metav1.CreateOptions{})
		fenceWantDenied(t, err, "legacy worker create arbitrary lease")
		fenceWantAbsent(t, admin, "legacy-worker-squat")
		cur, err := worker.CoordinationV1().Leases(fenceNS).Get(ctx, fenceCommon, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		hijack := "system:bootstrap:fed321/h/1/1"
		cur.Spec.HolderIdentity = &hijack
		_, err = worker.CoordinationV1().Leases(fenceNS).Update(ctx, cur, metav1.UpdateOptions{})
		fenceWantDenied(t, err, "legacy worker overwrite active fence")
		err = worker.CoordinationV1().Leases(fenceNS).Delete(ctx, fenceCommon, metav1.DeleteOptions{})
		fenceWantDenied(t, err, "legacy worker delete active fence")
		if got := fenceHolder(t, admin, fenceCommon); got != "controller/active/1" {
			t.Errorf("active holder changed to %q", got)
		}
	})

	ownA := "system:bootstrap:" + fenceTokenA + "/"
	ownB := "system:bootstrap:" + fenceTokenB + "/"
	sixty := int32(maintlease.Duration / time.Second)
	now := time.Now()
	stale := now.Add(-10 * maintlease.Duration)

	t.Run("a second grant leaves the policy and its binding as they are", func(t *testing.T) {
		// The mint compares the whole stored spec with the shipped one, so a
		// field the apiserver defaults and the YAML leaves out would turn
		// every mint into a policy rewrite.
		var rewrites []string
		wcfg := rest.CopyConfig(cfg)
		wcfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/validatingadmissionpolic") {
					rewrites = append(rewrites, r.URL.Path)
				}
				return rt.RoundTrip(r)
			})
		})
		watched, err := kubernetes.NewForConfig(wcfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := ha.GrantCertsReader(ctx, watched); err != nil {
			t.Fatalf("grant again: %v", err)
		}
		if len(rewrites) != 0 {
			t.Errorf("a second grant rewrote %v: the apiserver defaults a field the shipped spec leaves out", rewrites)
		}
	})

	t.Run("a join token may not create any other lease", func(t *testing.T) {
		fenceClear(t, admin)
		for _, name := range []string{"squat-anything", "kube-controller-manager", k8sutil.OperatorLeaseName} {
			_, err := joinerA.CoordinationV1().Leases(fenceNS).Create(ctx, fenceLease(name, ownA+"h/1/1", &sixty, now), metav1.CreateOptions{})
			fenceWantDenied(t, err, "create lease "+name)
			fenceWantAbsent(t, admin, name)
		}
	})

	t.Run("a join token may not record a duration longer than stratum's", func(t *testing.T) {
		fenceClear(t, admin)
		long := sixty + 1
		for _, secs := range []*int32{&long, nil} {
			_, err := joinerA.CoordinationV1().Leases(fenceNS).Create(ctx, fenceLease(fenceCommon, ownA+"h/1/1", secs, now), metav1.CreateOptions{})
			fenceWantDenied(t, err, fmt.Sprintf("create %s with duration %v", fenceCommon, fenceSecs(secs)))
			fenceWantAbsent(t, admin, fenceCommon)
		}
	})

	t.Run("a join token may name only itself as the holder", func(t *testing.T) {
		fenceClear(t, admin)
		for _, holder := range []string{"cp1/1/1", "system:bootstrap:" + fenceTokenA, "system:bootstrap:" + fenceTokenA + "4/h/1/1", ownB + "h/1/1", ""} {
			_, err := joinerA.CoordinationV1().Leases(fenceNS).Create(ctx, fenceLease(fenceCommon, holder, &sixty, now), metav1.CreateOptions{})
			fenceWantDenied(t, err, fmt.Sprintf("create %s held by %q", fenceCommon, holder))
			fenceWantAbsent(t, admin, fenceCommon)
		}
	})

	t.Run("a join token may not attach lease lifecycle metadata", func(t *testing.T) {
		for _, field := range []string{"ownerReferences", "finalizers"} {
			for _, verb := range []string{"create", "update"} {
				t.Run(field+"/"+verb, func(t *testing.T) {
					fenceClear(t, admin)
					leases := joinerA.CoordinationV1().Leases(fenceNS)
					l := fenceLease(fenceCommon, ownA+"h/1/1", &sixty, now)
					if verb == "update" {
						var err error
						l, err = leases.Create(ctx, l, metav1.CreateOptions{})
						if err != nil {
							t.Fatal(err)
						}
					}
					if field == "ownerReferences" {
						owner, err := admin.CoreV1().Secrets(fenceNS).Get(ctx, "bootstrap-token-"+fenceTokenA, metav1.GetOptions{})
						if err != nil {
							t.Fatal(err)
						}
						l.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: owner.Name, UID: owner.UID}}
					} else {
						l.Finalizers = []string{"example.com/keep-fence"}
					}
					var err error
					if verb == "create" {
						_, err = leases.Create(ctx, l, metav1.CreateOptions{})
					} else {
						_, err = leases.Update(ctx, l, metav1.UpdateOptions{})
					}
					// Clean admitted finalizers even on the red regression run.
					if stored, getErr := admin.CoordinationV1().Leases(fenceNS).Get(ctx, fenceCommon, metav1.GetOptions{}); getErr == nil {
						stored.Finalizers = nil
						if _, clearErr := admin.CoordinationV1().Leases(fenceNS).Update(ctx, stored, metav1.UpdateOptions{}); clearErr != nil {
							t.Fatal(clearErr)
						}
					}
					fenceWantDenied(t, err, verb+" lease with "+field)
				})
			}
		}
	})

	t.Run("a join token takes, renews and releases its own fence", func(t *testing.T) {
		fenceClear(t, admin)
		lease, err := k8sutil.MaintenanceFenceAs(joinerA, k8sutil.JoinMaintenanceHolder(fenceTokenA))
		if err != nil {
			t.Fatalf("the join token could not take its fence: %v", err)
		}
		if err := lease.Confirm(ctx); err != nil {
			t.Errorf("the join token could not renew its fence: %v", err)
		}
		// Another join token neither overwrites nor deletes it.
		for _, name := range []string{fenceCommon, fenceLegacy} {
			cur, err := joinerB.CoordinationV1().Leases(fenceNS).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("the second join token cannot read %s: %v", name, err)
			}
			hijack := ownB + "h/1/1"
			cur.Spec.HolderIdentity = &hijack
			_, err = joinerB.CoordinationV1().Leases(fenceNS).Update(ctx, cur, metav1.UpdateOptions{})
			fenceWantDenied(t, err, "overwrite the holder of the first join token's "+name)
			err = joinerB.CoordinationV1().Leases(fenceNS).Delete(ctx, name, metav1.DeleteOptions{})
			fenceWantDenied(t, err, "delete the first join token's "+name)
		}
		if err := lease.Confirm(ctx); err != nil {
			t.Errorf("the join token lost its fence to another join token: %v", err)
		}
		lease.Release()
		fenceWantAbsent(t, admin, fenceCommon)
		fenceWantAbsent(t, admin, fenceLegacy)
	})

	t.Run("a join token may not take over or delete a live lease another run holds", func(t *testing.T) {
		fenceClear(t, admin)
		lease, err := k8sutil.MaintenanceFence(admin)
		if err != nil {
			t.Fatalf("the admin run could not take the fence: %v", err)
		}
		defer lease.Release()
		for _, name := range []string{fenceCommon, fenceLegacy} {
			cur, err := joinerA.CoordinationV1().Leases(fenceNS).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("the join token cannot read %s: %v", name, err)
			}
			hijack := ownA + "h/1/1"
			cur.Spec.HolderIdentity = &hijack
			_, err = joinerA.CoordinationV1().Leases(fenceNS).Update(ctx, cur, metav1.UpdateOptions{})
			fenceWantDenied(t, err, "overwrite the holder of the admin run's "+name)
			err = joinerA.CoordinationV1().Leases(fenceNS).Delete(ctx, name, metav1.DeleteOptions{})
			fenceWantDenied(t, err, "delete the admin run's "+name)
		}
		if _, _, err := maintlease.AcquireCompatible(ctx, joinerA, fenceNS, fenceCommon, ownA+k8sutil.MaintenanceHolder()); !errors.Is(err, maintlease.ErrTaken) {
			t.Errorf("the join token's fence take beside a live admin run = %v, want maintlease's held refusal", err)
		}
		if err := lease.Confirm(ctx); err != nil {
			t.Errorf("the admin run lost its fence to a join token: %v", err)
		}
	})

	t.Run("a join token may not take over an expired lease another run left", func(t *testing.T) {
		fenceClear(t, admin)
		for _, left := range []string{"cp1/4242/1", ownB + "h/1/1"} {
			fenceSeed(t, admin, fenceLease(fenceLegacy, left, &sixty, stale))
			_, _, err := maintlease.AcquireCompatible(ctx, joinerA, fenceNS, fenceCommon, ownA+k8sutil.MaintenanceHolder())
			fenceWantDenied(t, err, "take over "+fenceLegacy+" left by "+left)
			if got := fenceHolder(t, admin, fenceLegacy); got != left {
				t.Errorf("%s holder = %q after the refused takeover, want %q left in place", fenceLegacy, got, left)
			}
			fenceWantAbsent(t, admin, fenceCommon)
			err = joinerA.CoordinationV1().Leases(fenceNS).Delete(ctx, fenceLegacy, metav1.DeleteOptions{})
			fenceWantDenied(t, err, "delete "+fenceLegacy+" left by "+left)
			fenceClear(t, admin)
		}
	})

	t.Run("a join token takes over its own expired lease and an empty one", func(t *testing.T) {
		fenceClear(t, admin)
		fenceSeed(t, admin, fenceLease(fenceLegacy, ownA+"h/1/1", &sixty, stale))
		fenceSeed(t, admin, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: fenceCommon, Namespace: fenceNS}})
		_, release, err := maintlease.AcquireCompatible(ctx, joinerA, fenceNS, fenceCommon, ownA+k8sutil.MaintenanceHolder())
		if err != nil {
			t.Fatalf("the join token could not take over its own expired lease and an empty one: %v", err)
		}
		release()
		fenceWantAbsent(t, admin, fenceCommon)
		fenceWantAbsent(t, admin, fenceLegacy)
	})

	t.Run("every other identity writes leases as before", func(t *testing.T) {
		fenceClear(t, admin)
		leases := admin.CoordinationV1().Leases(fenceNS)
		hour := int32(3600)
		for _, name := range []string{"squat-anything", fenceCommon} {
			if _, err := leases.Create(ctx, fenceLease(name, "anything", &hour, now), metav1.CreateOptions{}); err != nil {
				t.Errorf("admin create of %s: %v", name, err)
			}
		}
		// An admin run takes over a lease a join token holds live.
		fenceSeed(t, admin, fenceLease(fenceLegacy, ownA+"h/1/1", &sixty, now))
		cur, err := leases.Get(ctx, fenceLegacy, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		holder := "cp1/1/1"
		cur.Spec.HolderIdentity = &holder
		if _, err := leases.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Errorf("admin takeover of a join token's lease: %v", err)
		}
		for _, name := range []string{"squat-anything", fenceCommon, fenceLegacy} {
			if err := leases.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
				t.Errorf("admin delete of %s: %v", name, err)
			}
		}
	})
}

// fenceJoinClient mints a worker or control-plane token the way token.Create
// does and waits for real bootstrap authentication with the expected groups.
func fenceJoinClient(t *testing.T, cfg *rest.Config, admin kubernetes.Interface, id string, controlPlane bool) kubernetes.Interface {
	t.Helper()
	groups := k8sutil.BootstrapGroup
	if controlPlane {
		groups += "," + k8sutil.ControlPlaneBootstrapGroup
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + id, Namespace: fenceNS},
		Type:       corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id":                       id,
			"token-secret":                   fenceTokenSecret,
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              groups,
		},
	}
	if _, err := admin.CoreV1().Secrets(fenceNS).Create(t.Context(), sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	jcfg := rest.AnonymousClientConfig(cfg)
	jcfg.BearerToken = id + "." + fenceTokenSecret
	cs, err := kubernetes.NewForConfig(jcfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		review, err := cs.AuthenticationV1().SelfSubjectReviews().Create(t.Context(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err == nil {
			u := review.Status.UserInfo
			if u.Username != "system:bootstrap:"+id || !slices.Contains(u.Groups, k8sutil.BootstrapGroup) || slices.Contains(u.Groups, k8sutil.ControlPlaneBootstrapGroup) != controlPlane {
				t.Fatalf("the join token authenticates as %q in %v, want system:bootstrap:%s with controlPlane=%v", u.Username, u.Groups, id, controlPlane)
			}
			return cs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the bootstrap token authenticator never accepted token %s within 30s: %v", id, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// fenceWantDenied fails unless err is the join fence policy's denial as
// k8sutil.JoinFenceDenied, which the join keys its refusal on, recognises it.
// The denial is never Forbidden: that status is what the join reads as a Role
// that predates the lease grant.
func fenceWantDenied(t *testing.T, err error, what string) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: admitted, want the %s policy to deny it", what, k8sutil.JoinFencePolicy)
	case !k8sutil.JoinFenceDenied(err):
		t.Errorf("%s: %v, want the %s policy's denial", what, err, k8sutil.JoinFencePolicy)
	case apierrors.IsForbidden(err) || !apierrors.IsInvalid(err):
		t.Errorf("%s: the denial is %v, want an Invalid status, never Forbidden", what, err)
	}
}

func fenceWantAbsent(t *testing.T, admin kubernetes.Interface, name string) {
	t.Helper()
	if _, err := admin.CoordinationV1().Leases(fenceNS).Get(t.Context(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("lease %s after the case: %v, want NotFound", name, err)
		if err == nil {
			_ = admin.CoordinationV1().Leases(fenceNS).Delete(t.Context(), name, metav1.DeleteOptions{})
		}
	}
}

func fenceHolder(t *testing.T, admin kubernetes.Interface, name string) string {
	t.Helper()
	l, err := admin.CoordinationV1().Leases(fenceNS).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read lease %s: %v", name, err)
	}
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

// fenceSeed writes l as the admin, which the policy leaves alone.
func fenceSeed(t *testing.T, admin kubernetes.Interface, l *coordinationv1.Lease) {
	t.Helper()
	if _, err := admin.CoordinationV1().Leases(fenceNS).Create(t.Context(), l, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed lease %s: %v", l.Name, err)
	}
}

// fenceClear deletes both maintenance leases as the admin.
func fenceClear(t *testing.T, admin kubernetes.Interface) {
	t.Helper()
	for _, name := range []string{fenceCommon, fenceLegacy} {
		if err := admin.CoordinationV1().Leases(fenceNS).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("clear lease %s: %v", name, err)
		}
	}
}

func fenceLease(name, holder string, secs *int32, renewed time.Time) *coordinationv1.Lease {
	at := metav1.NewMicroTime(renewed)
	l := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fenceNS},
		Spec:       coordinationv1.LeaseSpec{LeaseDurationSeconds: secs, AcquireTime: &at, RenewTime: &at},
	}
	if holder != "" {
		l.Spec.HolderIdentity = &holder
	}
	return l
}

func fenceSecs(secs *int32) any {
	if secs == nil {
		return "unset"
	}
	return *secs
}
