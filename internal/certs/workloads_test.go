package certs

import (
	"io"

	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/rollout"
	"github.com/ctrliq/stratum/internal/version"
)

func genCA(t *testing.T, cn string) (*x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

var republished = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// rootCA is a namespace's kube-root-ca.crt, written by kube-controller-manager at at.
func rootCA(ns, bundle string, at time.Time) *corev1.ConfigMap {
	t := metav1.NewTime(at)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: rootCAConfigMap, Namespace: ns, ManagedFields: []metav1.ManagedFieldsEntry{
			{Manager: "kube-controller-manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t},
		}},
		Data: map[string]string{"ca.crt": bundle},
	}
}

// pod is a running pod that mounts a ServiceAccount token as every pod does,
// owned by owner (a ReplicaSet, a DaemonSet, a Job...), or bare if kind is "".
func pod(ns, name, sa, kind, ownerName string, started time.Time) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{ServiceAccountName: sa, Volumes: []corev1.Volume{
			{Name: "kube-api-access-x", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}},
			}}},
		}, Containers: []corev1.Container{{Name: "c", VolumeMounts: []corev1.VolumeMount{{Name: "kube-api-access-x"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}},
		}},
	}
	if kind != "" {
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: ownerName, UID: uidOf(kind, ns, ownerName), Controller: &yes}}
	}
	return p
}

// tokenSecret is a kubernetes.io/service-account-token Secret for the account.
func tokenSecret(ns, name, account string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		Annotations: map[string]string{corev1.ServiceAccountNameKey: account}}, Type: saTokenSecretType}
}

// secretOnly is a pod that mounts the token Secret and no projected token.
func secretOnly(ns, name, podAccount, secret string, started time.Time) *corev1.Pod {
	p := pod(ns, name, podAccount, "StatefulSet", name[:len(name)-2], started)
	p.Spec.Volumes = []corev1.Volume{{Name: "tok", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}}}
	p.Spec.Containers[0].VolumeMounts[0].Name = "tok"
	return p
}

// terminating is p, deleted and gone in grace.
func terminating(p *corev1.Pod, grace time.Duration) *corev1.Pod {
	p.DeletionTimestamp = new(metav1.NewTime(time.Now().Add(grace)))
	return p
}

func uidOf(kind, ns, name string) types.UID { return types.UID(kind + "/" + ns + "/" + name) }

// client is a fake clientset holding objs and, for every Deployment, DaemonSet
// or StatefulSet that a pod or a ReplicaSet names as its controller and that is
// not among objs, that owner, with the UID the reference carries.
func client(objs ...runtime.Object) *fake.Clientset {
	have := map[string]bool{}
	var refs []metav1.OwnerReference
	var namespaces []string
	for _, o := range objs {
		m, ok := o.(metav1.Object)
		if !ok {
			continue
		}
		switch o.(type) {
		case *appsv1.Deployment:
			have["Deployment/"+m.GetNamespace()+"/"+m.GetName()] = true
		case *appsv1.DaemonSet:
			have["DaemonSet/"+m.GetNamespace()+"/"+m.GetName()] = true
		case *appsv1.StatefulSet:
			have["StatefulSet/"+m.GetNamespace()+"/"+m.GetName()] = true
		case *corev1.Pod, *appsv1.ReplicaSet:
			if c := metav1.GetControllerOf(m); c != nil {
				refs = append(refs, *c)
				namespaces = append(namespaces, m.GetNamespace())
			}
		}
	}
	for i, c := range refs {
		key := c.Kind + "/" + namespaces[i] + "/" + c.Name
		if have[key] {
			continue
		}
		meta := metav1.ObjectMeta{Namespace: namespaces[i], Name: c.Name, UID: c.UID}
		switch c.Kind {
		case "Deployment":
			objs = append(objs, &appsv1.Deployment{ObjectMeta: meta})
		case "DaemonSet":
			objs = append(objs, &appsv1.DaemonSet{ObjectMeta: meta})
		case "StatefulSet":
			objs = append(objs, &appsv1.StatefulSet{ObjectMeta: meta})
		default:
			continue
		}
		have[key] = true
	}
	return fake.NewClientset(objs...)
}

func replicaSet(ns, name, deployment string) *appsv1.ReplicaSet {
	yes := true
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: uidOf("ReplicaSet", ns, name),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deployment, UID: uidOf("Deployment", ns, deployment), Controller: &yes}}}}
}

func roleBinding(ns, name string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Subjects: subjects}
}

func sa(ns, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: name}
}

func owners(ws []clientWorkload) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Owner.String())
	}
	return out
}

func TestSelectClients(t *testing.T) {
	before, after := republished.Add(-time.Hour), republished.Add(time.Hour)
	legacy := pod("legacy", "old-0", "worker", "StatefulSet", "old", before)
	legacy.Spec.Volumes = []corev1.Volume{{Name: "tok", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "worker-token"}}}}
	legacy.Spec.Containers[0].VolumeMounts[0].Name = "tok"
	// A token projection under another name counts; a projected volume that
	// carries the kube-api-access- name but no token does not.
	renamed := pod("custom", "proj-0", "proj", "StatefulSet", "proj", before)
	renamed.Spec.Volumes[0].Name = "creds"
	renamed.Spec.Containers[0].VolumeMounts[0].Name = "creds"
	notToken := pod("custom", "cfg-0", "proj", "ReplicaSet", "cfg-1", before)
	notToken.Spec.Volumes = []corev1.Volume{{Name: "kube-api-access-cfg", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{}}},
	}}}}
	notToken.Spec.Containers[0].VolumeMounts[0].Name = "kube-api-access-cfg"
	// A token volume only counts when a container mounts it: a regular, init or
	// ephemeral one, and not when nothing does.
	unused := pod("mounts", "unused-0", "m", "StatefulSet", "unused", before)
	unused.Spec.Containers[0].VolumeMounts = nil
	initOnly := pod("mounts", "init-0", "m", "StatefulSet", "init", before)
	initOnly.Spec.InitContainers = initOnly.Spec.Containers
	initOnly.Spec.Containers = nil
	ephemeralOnly := pod("mounts", "eph-0", "m", "StatefulSet", "eph", before)
	ephemeralOnly.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: "debug", VolumeMounts: ephemeralOnly.Spec.Containers[0].VolumeMounts}}}
	ephemeralOnly.Spec.Containers = nil
	noToken := pod("apps", "quiet-0", "bound", "ReplicaSet", "quiet-1", before)
	noToken.Spec.Volumes = nil
	done := pod("apps", "job-done", "bound", "Job", "migrate", before)
	done.Status.Phase = corev1.PodSucceeded

	cs := client(
		// Bound by name, two replicas of one Deployment.
		pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", before),
		pod("kong", "ingress-b", "kic", "ReplicaSet", "ingress-1", after),
		replicaSet("kong", "ingress-1", "ingress"),
		roleBinding("kong", "kic", sa("kong", "kic")),
		// Bound only through system:serviceaccounts:<ns>.
		pod("ops", "agent-x", "agent", "DaemonSet", "agent", before),
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ops"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "system:serviceaccounts:ops"}}},
		// A RoleBinding subject without a namespace is the binding's own.
		pod("tools", "t-0", "runner", "", "", before),
		roleBinding("tools", "runner", rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "runner"}),
		// The bootstrap policy binds system:serviceaccounts itself; that must not
		// select every pod that mounts a token.
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "system:service-account-issuer-discovery",
			Labels: map[string]string{"kubernetes.io/bootstrapping": "rbac-defaults"}},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: serviceAccountsGroup}}},
		pod("unbound", "app-0", "default", "ReplicaSet", "app-1", before),
		replicaSet("unbound", "app-1", "app"),
		// The same name in another namespace is not bound.
		pod("other", "runner-0", "runner", "ReplicaSet", "runner-1", before),
		replicaSet("other", "runner-1", "runner"),
		// A Job's pod is listed, and a finished one is not.
		pod("apps", "job-run", "bound", "Job", "migrate", before),
		done,
		roleBinding("apps", "bound", sa("apps", "bound")),
		noToken,
		renamed, notToken, roleBinding("custom", "proj", sa("custom", "proj")),
		unused, initOnly, ephemeralOnly, roleBinding("mounts", "m", sa("mounts", "m")),
		// A legacy token Secret is for the account it names, not the pod's: the
		// pod's own account is unbound here and the Secret's is bound, and the
		// other way round.
		secretOnly("xsa", "borrows-0", "unbound", "bound-token", before),
		secretOnly("xsa", "lends-0", "bound", "unbound-token", before),
		tokenSecret("xsa", "bound-token", "bound"), tokenSecret("xsa", "unbound-token", "unbound"),
		roleBinding("xsa", "b", sa("xsa", "bound")),
		// The authorizer reads a ServiceAccount as a user, too.
		pod("users", "u-0", "usa", "StatefulSet", "u", before),
		roleBinding("users", "u", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "system:serviceaccount:users:usa"}),
		pod("users", "v-0", "other", "StatefulSet", "v", before),
		roleBinding("users", "v", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "system:serviceaccount:elsewhere:other"}),
		// A legacy token Secret volume counts.
		legacy,
		tokenSecret("legacy", "worker-token", "worker"),
		roleBinding("legacy", "worker", sa("legacy", "worker")),
		// Stratum's own add-on, bound.
		pod("kube-system", "coredns-a", "coredns", "ReplicaSet", "coredns-1", before),
		replicaSet("kube-system", "coredns-1", "coredns"),
		roleBinding("kube-system", "coredns", sa("kube-system", "coredns")),
	)

	got, err := selectClients(context.Background(), cs, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DaemonSet ops/agent",
		"Deployment kong/ingress",
		"Job apps/migrate",
		"Pod tools/t-0",
		"StatefulSet custom/proj",
		"StatefulSet legacy/old",
		"StatefulSet mounts/eph",
		"StatefulSet mounts/init",
		"StatefulSet users/u",
		"StatefulSet xsa/borrows",
	}
	if !slices.Equal(owners(got), want) {
		t.Errorf("clients = %v, want %v", owners(got), want)
	}
	for _, w := range got {
		if w.Owner.Name == "ingress" && len(w.Pods) != 2 {
			t.Errorf("the two replicas of kong/ingress are %d pod(s) of one workload, want 2", len(w.Pods))
		}
	}

	all, err := selectClients(context.Background(), cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(owners(all), "Deployment kube-system/coredns") || len(all) != len(want)+1 {
		t.Errorf("with includeOwned: %v, want %v and the coredns Deployment", owners(all), want)
	}
}

func TestPreflightWorkloads(t *testing.T) {
	user := []runtime.Object{
		pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", republished),
		replicaSet("kong", "ingress-1", "ingress"),
		roleBinding("kong", "kic", sa("kong", "kic")),
		pod("apps", "job-run", "bound", "Job", "migrate", republished),
		roleBinding("apps", "bound", sa("apps", "bound")),
	}
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		o       CARotateOptions
		wantErr []string // all must appear; nil means it passes
	}{
		{"no user workloads", nil, CARotateOptions{}, nil},
		{"refuses", user, CARotateOptions{}, []string{
			"refusing to start", "Deployment kong/ingress", "stratum kubectl -n kong rollout restart deployment/ingress",
			"Job apps/migrate", "never restarts a Job", "--restart-workloads", "--skip-workload-check",
		}},
		{"restart-workloads", user, CARotateOptions{RestartWorkloads: true}, nil},
		{"skip-workload-check", user, CARotateOptions{SkipWorkloadCheck: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := client(tc.objects...)
			err := preflightWorkloads(context.Background(), io.Discard, cs, tc.o)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("passed; want a refusal")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestCheckClientsRestarted(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	_, oldPEM := genCA(t, "old")
	both := newPEM + oldPEM
	before, after := republished.Add(-time.Hour), republished.Add(time.Hour)
	deployment := func(ns, name string, started time.Time) []runtime.Object {
		return []runtime.Object{
			pod(ns, name+"-pod", "kic", "ReplicaSet", name+"-1", started),
			replicaSet(ns, name+"-1", name),
			roleBinding(ns, "kic", sa(ns, "kic")),
		}
	}
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		wantErr []string // nil means the gate passes
		notErr  []string
	}{
		{"no clients", []runtime.Object{rootCA("kong", both, republished)}, nil, nil},
		{"restarted since the republish", append(deployment("kong", "ingress", after), rootCA("kong", both, republished)), nil, nil},
		{"started before the republish", append(deployment("kong", "ingress", before), rootCA("kong", both, republished)),
			[]string{"1 workload(s)", "Deployment kong/ingress", "stratum kubectl -n kong rollout restart deployment/ingress", "--skip-workload-check"}, nil},
		{"only the stale one is listed",
			append(append(deployment("kong", "ingress", before), deployment("apps", "api", after)...), rootCA("kong", both, republished), rootCA("apps", both, republished)),
			[]string{"Deployment kong/ingress"}, []string{"apps/api"}},
		{"the old pod is still terminating, on the old CA", append(deployment("kong", "ingress", after),
			terminating(pod("kong", "old-pod", "kic", "ReplicaSet", "ingress-1", before), 30*time.Second), rootCA("kong", both, republished)),
			[]string{"Deployment kong/ingress", "still terminating on the old CA", "old-pod", "of its grace period left"}, []string{"rollout restart"}},
		{"a stale pod that is not terminating is told to be restarted", append(deployment("kong", "ingress", after),
			pod("kong", "old-pod", "kic", "ReplicaSet", "ingress-1", before), rootCA("kong", both, republished)),
			[]string{"rollout restart deployment/ingress"}, []string{"terminating"}},
		{"a Job is told to be recreated", []runtime.Object{
			pod("apps", "job-run", "bound", "Job", "migrate", before), roleBinding("apps", "bound", sa("apps", "bound")), rootCA("apps", both, republished)},
			[]string{"Job apps/migrate", "recreate it"}, nil},
		{"stratum's own add-on", []runtime.Object{
			pod("kube-system", "coredns-a", "coredns", "ReplicaSet", "coredns-1", before), replicaSet("kube-system", "coredns-1", "coredns"),
			roleBinding("kube-system", "coredns", sa("kube-system", "coredns")), rootCA("kube-system", both, republished)},
			[]string{"Deployment kube-system/coredns", "rollout restart deployment/coredns"}, nil},
		{"new CA not published yet", append(deployment("kong", "ingress", after), rootCA("kong", oldPEM, republished)),
			[]string{"does not carry the new CA yet", "kong"}, nil},
		{"no ConfigMap", deployment("kong", "ingress", after),
			[]string{"does not carry the new CA yet", "kong"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := client(tc.objects...)
			err := checkClientsRestarted(context.Background(), cs, newCA, nil)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("passed; want a refusal")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal lacks %q:\n%v", w, err)
				}
			}
			for _, w := range tc.notErr {
				if strings.Contains(err.Error(), w) {
					t.Errorf("refusal names %q, which restarted:\n%v", w, err)
				}
			}
		})
	}
}

// The gate fails closed when it cannot tell when the namespace took the new CA.
func TestCheckClientsRestartedWithoutAPublishTime(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	cm := rootCA("kong", newPEM, republished)
	cm.ManagedFields = nil
	cs := client(cm,
		pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", republished.Add(time.Hour)),
		replicaSet("kong", "ingress-1", "ingress"),
		roleBinding("kong", "kic", sa("kong", "kic")))
	err := checkClientsRestarted(context.Background(), cs, newCA, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot tell when kong/kube-root-ca.crt took the new CA") {
		t.Fatalf("error = %v, want a refusal that it cannot tell when the CA was published", err)
	}
}

func stageNewCA(t *testing.T, pemData string) {
	t.Helper()
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new.crt"), []byte(pemData), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeKubectl records the kubectl calls and answers every probe with its target.
type fakeKubectl struct {
	mu     sync.Mutex
	calls  []string
	failed string // a rollout status that fails
}

func (f *fakeKubectl) run(_ context.Context, _ time.Duration, _ bool, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if i := slices.Index(args, "get"); i >= 0 {
		return []byte(args[i+1]), nil // present
	}
	if f.failed != "" && strings.Contains(line, "rollout status "+f.failed) {
		return []byte("timed out"), errors.New("exit status 1")
	}
	return nil, nil
}

func TestRestartClients(t *testing.T) {
	_, newPEM := genCA(t, "new")
	_, oldPEM := genCA(t, "old")
	stageNewCA(t, newPEM)
	before, after := republished.Add(-time.Hour), republished.Add(time.Hour)

	// kong's Deployment has one pod, started at started: before the roll, or
	// after it, as a rollout leaves it. The Job apps/migrate never restarts.
	deployment := func(started time.Time) []runtime.Object {
		return []runtime.Object{
			pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", started),
			replicaSet("kong", "ingress-1", "ingress"),
			roleBinding("kong", "kic", sa("kong", "kic")),
		}
	}
	job := []runtime.Object{pod("apps", "job-run", "bound", "Job", "migrate", before), roleBinding("apps", "bound", sa("apps", "bound"))}
	run := func(t *testing.T, o CARotateOptions, k *fakeKubectl, objs ...[]runtime.Object) error {
		all := []runtime.Object{rootCA("kong", newPEM+oldPEM, republished), rootCA("apps", newPEM+oldPEM, republished)}
		for _, os := range objs {
			all = append(all, os...)
		}
		roller := rollout.Roller{Kubectl: k.run, Hint: adminKubectlHint, CallTimeout: time.Second, Wait: time.Second}
		return restartClients(context.Background(), io.Discard, client(all...), o, roller)
	}
	restarted := func(k *fakeKubectl) []string {
		var out []string
		for _, c := range k.calls {
			if strings.Contains(c, "rollout restart") {
				out = append(out, c)
			}
		}
		return out
	}

	t.Run("add-ons only", func(t *testing.T) {
		k := &fakeKubectl{}
		if err := run(t, CARotateOptions{}, k); err != nil {
			t.Fatal(err)
		}
		if got := restarted(k); len(got) != 5 || !slices.Contains(got, "-n kube-system rollout restart ds/cilium") {
			t.Errorf("restarted %q, want the five add-on targets", got)
		}
	})
	t.Run("restart-workloads", func(t *testing.T) {
		k := &fakeKubectl{}
		// The Job started before the republish and is not counted: it was listed, and is not Stratum's to restart.
		if err := run(t, CARotateOptions{RestartWorkloads: true}, k, deployment(after), job); err != nil {
			t.Fatal(err)
		}
		got := restarted(k)
		if !slices.Contains(got, "-n kong rollout restart deployment/ingress") {
			t.Errorf("restarted %q, want the kong Deployment too", got)
		}
		if slices.ContainsFunc(got, func(c string) bool { return strings.Contains(c, "migrate") || strings.Contains(c, "apps") }) {
			t.Errorf("restarted %q: a Job is never restarted", got)
		}
	})
	t.Run("skip-workload-check rolls the add-ons only and does not check", func(t *testing.T) {
		k := &fakeKubectl{}
		if err := run(t, CARotateOptions{RestartWorkloads: true, SkipWorkloadCheck: true}, k, deployment(before), job); err != nil {
			t.Fatal(err)
		}
		if got := restarted(k); len(got) != 5 {
			t.Errorf("restarted %q, want the five add-on targets", got)
		}
	})
	t.Run("a rollout that does not finish is an error", func(t *testing.T) {
		k := &fakeKubectl{failed: "deployment/ingress"}
		err := run(t, CARotateOptions{RestartWorkloads: true}, k, deployment(after))
		if err == nil || !strings.Contains(err.Error(), "deployment/ingress did not finish rolling out") {
			t.Fatalf("error = %v, want the unfinished rollout", err)
		}
	})
	t.Run("a client that appeared after the preflight", func(t *testing.T) {
		err := run(t, CARotateOptions{}, &fakeKubectl{}, deployment(before))
		if err == nil || !strings.Contains(err.Error(), "Deployment kong/ingress") {
			t.Fatalf("error = %v, want the stale Deployment refused before sign-new", err)
		}
	})
	t.Run("a forged owner is neither rolled nor halting", func(t *testing.T) {
		k := &fakeKubectl{}
		forged := pod("kong", "forged-0", "kic", "Deployment", "x;id", before)
		if err := run(t, CARotateOptions{RestartWorkloads: true}, k, []runtime.Object{forged, roleBinding("kong", "kic", sa("kong", "kic"))}); err != nil {
			t.Fatalf("a forged owner halted the run: %v", err)
		}
		if slices.ContainsFunc(restarted(k), func(c string) bool { return strings.Contains(c, "x;id") || strings.Contains(c, "kong") }) {
			t.Errorf("restarted %q: a forged owner was rolled", restarted(k))
		}
	})
	t.Run("a partitioned StatefulSet leaves pods on the old CA", func(t *testing.T) {
		// rollout status succeeds with a partition; the ordinal below it never restarted.
		sts := []runtime.Object{
			pod("kong", "db-0", "kic", "StatefulSet", "db", before),
			pod("kong", "db-1", "kic", "StatefulSet", "db", after),
			roleBinding("kong", "kic", sa("kong", "kic")),
		}
		err := run(t, CARotateOptions{RestartWorkloads: true}, &fakeKubectl{}, sts)
		if err == nil || !strings.Contains(err.Error(), "StatefulSet kong/db") || !strings.Contains(err.Error(), "rollingUpdate partition") {
			t.Fatalf("error = %v, want the StatefulSet refused with the partition hint", err)
		}
	})
}

func TestWaitRootCARepublished(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	_, oldPEM := genCA(t, "old")
	ns := func(name string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	old, oldPoll := republishWait, republishPoll
	republishWait, republishPoll = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { republishWait, republishPoll = old, oldPoll })

	t.Run("every namespace carries it", func(t *testing.T) {
		cs := client(ns("a"), ns("b"), rootCA("a", newPEM+oldPEM, republished), rootCA("b", newPEM+oldPEM, republished))
		if err := waitRootCARepublished(context.Background(), cs, newCA); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("one lags", func(t *testing.T) {
		cs := client(ns("a"), ns("b"), rootCA("a", newPEM+oldPEM, republished), rootCA("b", oldPEM, republished))
		err := waitRootCARepublished(context.Background(), cs, newCA)
		if err == nil || !strings.Contains(err.Error(), "still without the new CA: b") {
			t.Fatalf("error = %v, want namespace b named", err)
		}
	})
	t.Run("a namespace being deleted is not waited on", func(t *testing.T) {
		gone := ns("gone")
		now := metav1.Now()
		gone.DeletionTimestamp = &now
		cs := client(ns("a"), gone, rootCA("a", newPEM, republished))
		if err := waitRootCARepublished(context.Background(), cs, newCA); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCANodeArgvSkipsTheWorkloadCheckOnlyAtSignNew(t *testing.T) {
	for _, phase := range []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize} {
		got := slices.Contains(caNodeArgv(phase, false, false), "--skip-workload-check")
		if want := phase == PhaseSignNew; got != want {
			t.Errorf("phase %s: --skip-workload-check = %v, want %v", phase, got, want)
		}
	}
}

func TestSignNewClientGateSkipped(t *testing.T) {
	// No staged CA and no admin.conf: only the skip returns before needing either.
	if err := signNewClientGate(context.Background(), CARotateOptions{SkipWorkloadCheck: true}); err != nil {
		t.Fatalf("skipped gate: %v", err)
	}
	if err := signNewClientGate(context.Background(), CARotateOptions{}); err == nil {
		t.Fatal("gate passed with no staged CA")
	}
}

// TestStagedNewCAAfterAStoppedPromotion: a sign-new resumed after it promoted
// the new root gates the clients on that root, now ca.crt, rather than failing
// to read the consumed ca-new.crt.
func TestStagedNewCAAfterAStoppedPromotion(t *testing.T) {
	prep(t)
	if err := pki.RotateCAMaterial(true); err != nil {
		t.Fatal(err)
	}
	staged, err := stagedNewCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if !pki.SignNewResumable() {
		t.Fatal("precondition: a promotion with no marker is resumable")
	}
	got, err := stagedNewCA()
	if err != nil {
		t.Fatalf("stagedNewCA on a resumable node: %v", err)
	}
	if !got.Equal(staged) {
		t.Error("stagedNewCA on a resumable node is not the promoted new root")
	}
}

// A refused sign-new changes nothing: the gate runs before the new CA is promoted.
func TestCPSignNewRefusesBeforePromoting(t *testing.T) {
	prep(t)
	oldConf := version.AdminConf
	version.AdminConf = filepath.Join(t.TempDir(), "absent.conf") // the gate cannot reach a cluster
	t.Cleanup(func() { version.AdminConf = oldConf })
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	err := cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true})
	if err == nil || !strings.Contains(err.Error(), "sign-new refused") {
		t.Fatalf("error = %v, want sign-new refused", err)
	}
	if !pki.CARotationStaged() {
		t.Error("the new CA was promoted although sign-new was refused")
	}
}

// Native sidecars, long-running init containers and ephemeral containers hold
// the CA they started with, as the regular containers do.
func TestStartedBefore(t *testing.T) {
	running := func(started time.Time) corev1.ContainerStatus {
		return corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}}
	}
	fresh, stale := republished.Add(time.Hour), republished.Add(-time.Hour)
	done := corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(stale)}}}
	for _, tc := range []struct {
		name    string
		created time.Time // zero: before the republish, as a pod with no creation time is
		status  corev1.PodStatus
		want    bool
	}{
		{"all restarted", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(fresh)}}, false},
		{"stale container", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(stale)}}, true},
		{"stale native sidecar", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(fresh)}, InitContainerStatuses: []corev1.ContainerStatus{running(stale)}}, true},
		{"stale ephemeral container", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(fresh)}, EphemeralContainerStatuses: []corev1.ContainerStatus{running(stale)}}, true},
		{"started in the second of the republish", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(republished)}}, true},
		{"started in the second after the republish", republished.Add(time.Second), corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(republished.Add(time.Second))}}, false},
		{"finished init container", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(fresh)}, InitContainerStatuses: []corev1.ContainerStatus{done}}, false},
		{"created before, started inside the margin", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(republished.Add(30 * time.Second))}}, true},
		{"created before, started at the end of the margin", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(republished.Add(projectedCAMargin))}}, true},
		{"created before, started after the margin", time.Time{}, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(republished.Add(projectedCAMargin + time.Second))}}, false},
		{"created after, started inside the margin", republished.Add(time.Second), corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{running(republished.Add(30 * time.Second))}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := startedBefore([]*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(tc.created)}, Status: tc.status}}, republished); got != tc.want {
				t.Errorf("startedBefore = %v, want %v", got, tc.want)
			}
		})
	}
}

// The owner reference is the pod author's to write. A reference that names a
// workload that does not exist, or is not that pod's, or whose name is not a
// name, is not trusted: the pod is listed as itself, never restarted, and no
// text of the author's reaches the operator.
func TestSelectClientsDistrustsForgedOwners(t *testing.T) {
	before := republished.Add(-time.Hour)
	forged := func(name, kind, ownerName string) *corev1.Pod {
		p := pod("forge", name, "f", kind, ownerName, before)
		return p
	}
	wrongUID := forged("wrong-uid-0", "Deployment", "real")
	wrongUID.OwnerReferences[0].UID = "someone-elses"
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
	}{
		{"name with a shell metacharacter", forged("meta-0", "Deployment", "x;id")},
		{"name with a newline", forged("newline-0", "StatefulSet", "a\nb")},
		{"name with a command substitution", forged("subst-0", "DaemonSet", "x$(id)")},
		{"owner that does not exist", forged("missing-0", "Deployment", "nonexistent")},
		{"owner with another UID", wrongUID},
		{"kind that is not a kind", forged("kind-0", "Deployment$(id)", "ok")},
		{"unrolled kind with a bad name", forged("job-0", "Job", "x;id")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(tc.pod, roleBinding("forge", "f", sa("forge", "f")),
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "forge", Name: "real", UID: "the-real-one"}})
			got, err := selectClients(context.Background(), cs, false)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"Pod forge/" + tc.pod.Name}; !slices.Equal(owners(got), want) {
				t.Fatalf("clients = %v, want %v", owners(got), want)
			}
			out := listWorkloads(got)
			if strings.Contains(out, "rollout restart") || strings.ContainsAny(out, ";$") || strings.Count(out, "\n") != 1 {
				t.Errorf("a forged owner reached the listing:\n%s", out)
			}
		})
	}
}

// Whatever reaches the output is quoted: names are printed so that none carries
// a newline or an escape, and the command is one a shell takes literally.
func TestOwnerFormatting(t *testing.T) {
	for _, tc := range []struct {
		o                string
		owner            owner
		wantString, want string
	}{
		{"plain", owner{"Deployment", "kong", "ingress"}, "Deployment kong/ingress", "stratum kubectl -n kong rollout restart deployment/ingress"},
		{"metacharacter", owner{"Deployment", "ns", "x;id"}, "Deployment ns/\"x;id\"", "stratum kubectl -n ns rollout restart 'deployment/x;id'"},
		{"newline", owner{"StatefulSet", "ns", "a\nb"}, "StatefulSet ns/\"a\\nb\"", "stratum kubectl -n ns rollout restart 'statefulset/a\nb' (with a rollingUpdate partition, also delete the pods below it)"},
		{"quote", owner{"DaemonSet", "ns", "it's"}, "DaemonSet ns/\"it's\"", `stratum kubectl -n ns rollout restart 'daemonset/it'\''s'`},
		{"kind with an escape", owner{"Job\x1b[2J", "ns", "j"}, "\"Job\\x1b[2J\" ns/j", `recreate it (Stratum never restarts a "Job\x1b[2J")`},
	} {
		t.Run(tc.o, func(t *testing.T) {
			if got := tc.owner.String(); got != tc.wantString {
				t.Errorf("String() = %q, want %q", got, tc.wantString)
			}
			if got := tc.owner.guidance(); got != tc.want {
				t.Errorf("guidance() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The old pods of a rolled workload are still running, on the old CA, until their
// grace period ends. The run waits for them, up to the time the rollouts were
// allowed, and halts, listing what is left, when they do not go.
func TestRestartClientsWaitsForTerminatingPods(t *testing.T) {
	_, newPEM := genCA(t, "new")
	_, oldPEM := genCA(t, "old")
	stageNewCA(t, newPEM)
	before, after := republished.Add(-time.Hour), republished.Add(time.Hour)
	oldWait, oldPoll := clientRolloutWait, republishPoll
	republishPoll = 5 * time.Millisecond
	t.Cleanup(func() { clientRolloutWait, republishPoll = oldWait, oldPoll })

	objects := func(extra ...runtime.Object) *fake.Clientset {
		return client(append([]runtime.Object{
			rootCA("kong", newPEM+oldPEM, republished),
			pod("kong", "ingress-new", "kic", "ReplicaSet", "ingress-1", after),
			terminating(pod("kong", "ingress-old", "kic", "ReplicaSet", "ingress-1", before), 20*time.Second),
			replicaSet("kong", "ingress-1", "ingress"),
			roleBinding("kong", "kic", sa("kong", "kic")),
		}, extra...)...)
	}
	roller := func() rollout.Roller {
		return rollout.Roller{Kubectl: (&fakeKubectl{}).run, Hint: adminKubectlHint, CallTimeout: time.Second, Wait: time.Second}
	}

	t.Run("they go within the wait", func(t *testing.T) {
		clientRolloutWait = 5 * time.Second
		cs := objects()
		go func() {
			time.Sleep(60 * time.Millisecond)
			_ = cs.CoreV1().Pods("kong").Delete(context.Background(), "ingress-old", metav1.DeleteOptions{})
		}()
		start := time.Now()
		if err := restartClients(context.Background(), io.Discard, cs, CARotateOptions{RestartWorkloads: true}, roller()); err != nil {
			t.Fatalf("waited for a pod that went: %v", err)
		}
		if time.Since(start) < 50*time.Millisecond {
			t.Errorf("returned after %s, before the old pod was gone", time.Since(start))
		}
	})
	t.Run("they do not go", func(t *testing.T) {
		clientRolloutWait = 80 * time.Millisecond
		start := time.Now()
		err := restartClients(context.Background(), io.Discard, objects(), CARotateOptions{RestartWorkloads: true}, roller())
		if err == nil {
			t.Fatal("went on to sign-new with an old pod still running on the old CA")
		}
		for _, want := range []string{"Deployment kong/ingress", "still terminating on the old CA", "ingress-old", "of its grace period left", "1 workload(s)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q:\n%v", want, err)
			}
		}
		if time.Since(start) < 80*time.Millisecond {
			t.Errorf("gave up after %s, before the wait was over", time.Since(start))
		}
	})
	t.Run("a pod that is not terminating ends the wait", func(t *testing.T) {
		clientRolloutWait = 5 * time.Second
		start := time.Now()
		err := restartClients(context.Background(), io.Discard, objects(pod("kong", "ingress-stuck", "kic", "ReplicaSet", "ingress-1", before)), CARotateOptions{RestartWorkloads: true}, roller())
		if err == nil || !strings.Contains(err.Error(), "rollout restart deployment/ingress") {
			t.Fatalf("error = %v, want the stuck pod's Deployment to be restarted", err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("waited %s for a pod that was never going to go", time.Since(start))
		}
	})
}

// The wait budget bounds the API calls too: the admin client has no request
// timeout, so a response that never comes would otherwise hold the wait open.
func TestWaitClientsRestartedCancelsAStalledAPICall(t *testing.T) {
	newCA, _ := genCA(t, "new")
	cancelled := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers; ends when the client gives up
		once.Do(func() { close(cancelled) })
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := clientRolloutWait
	clientRolloutWait = 80 * time.Millisecond
	t.Cleanup(func() { clientRolloutWait = old })

	start := time.Now()
	err = waitClientsRestarted(context.Background(), cs, newCA, nil)
	if err == nil || !strings.Contains(err.Error(), "could not check that no client is left on the old CA within 80ms") {
		t.Fatalf("error = %v, want the check to have timed out", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("a stalled call held the wait for %s, over its 80ms budget", time.Since(start))
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Error("the request was not cancelled when the budget ran out")
	}
}

// stallAfter lets the first n namespace lists through and holds the rest until
// their context ends.
type stallAfter struct {
	kubernetes.Interface
	n     int32
	calls *atomic.Int32
}

func (s stallAfter) CoreV1() corev1client.CoreV1Interface {
	return stallCoreV1{s.Interface.CoreV1(), s}
}

type stallCoreV1 struct {
	corev1client.CoreV1Interface
	s stallAfter
}

func (c stallCoreV1) Namespaces() corev1client.NamespaceInterface {
	return stallNamespaces{c.CoreV1Interface.Namespaces(), c.s}
}

type stallNamespaces struct {
	corev1client.NamespaceInterface
	s stallAfter
}

func (n stallNamespaces) List(ctx context.Context, opts metav1.ListOptions) (*corev1.NamespaceList, error) {
	if n.s.calls.Add(1) > n.s.n {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return n.NamespaceInterface.List(ctx, opts)
}

// When a call stalls after the old pods have been seen, the halt still lists them.
func TestWaitClientsRestartedReportsThePodsSeenBeforeAStall(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	_, oldPEM := genCA(t, "old")
	before, after := republished.Add(-time.Hour), republished.Add(time.Hour)
	cs := stallAfter{n: 1, calls: new(atomic.Int32), Interface: client(
		rootCA("kong", newPEM+oldPEM, republished),
		pod("kong", "ingress-new", "kic", "ReplicaSet", "ingress-1", after),
		terminating(pod("kong", "ingress-old", "kic", "ReplicaSet", "ingress-1", before), 20*time.Second),
		replicaSet("kong", "ingress-1", "ingress"),
		roleBinding("kong", "kic", sa("kong", "kic")),
	)}
	oldWait, oldPoll := clientRolloutWait, republishPoll
	clientRolloutWait, republishPoll = 80*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { clientRolloutWait, republishPoll = oldWait, oldPoll })

	err := waitClientsRestarted(context.Background(), cs, newCA, nil)
	if err == nil {
		t.Fatal("went on to sign-new with an old pod still running on the old CA")
	}
	for _, want := range []string{"Deployment kong/ingress", "ingress-old", "of its grace period left", "sign-new can run once they have"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "rotate again once they are gone") {
		t.Errorf("the halt tells the operator to retry, which the staged CA refuses:\n%v", err)
	}
}

// A namespace admin can write managedFields on their own kube-root-ca.crt. A
// time further ahead than the clock-skew allowance is refused, not believed.
func TestCheckClientsRestartedRefusesAForwardDatedRepublish(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	for _, tc := range []struct {
		name    string
		at      time.Time
		wantErr string // empty means the gate passes
	}{
		{"an hour ahead", time.Now().Add(time.Hour), "ahead of this host's clock"},
		{"just past the allowance", time.Now().Add(republishClockSkew + time.Minute), "ahead of this host's clock"},
		{"within the allowance", time.Now().Add(republishClockSkew / 2), ""},
		{"in the past", time.Now().Add(-time.Hour), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := tc.at.Add(time.Hour)
			p := pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", started)
			p.CreationTimestamp = metav1.NewTime(started)
			cs := client(rootCA("kong", newPEM, tc.at), p, replicaSet("kong", "ingress-1", "ingress"), roleBinding("kong", "kic", sa("kong", "kic")))
			err := checkClientsRestarted(context.Background(), cs, newCA, nil)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one with %q", err, tc.wantErr)
			}
		})
	}
}

// A StatefulSet with a rollingUpdate partition is not restarted past it, so the
// preflight refuses it while nothing has changed.
func TestPreflightWorkloadsRefusesAPartitionedStatefulSet(t *testing.T) {
	statefulSet := func(partition *int32) *appsv1.StatefulSet {
		ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kong", Name: "db", UID: uidOf("StatefulSet", "kong", "db")}}
		if partition != nil {
			ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: partition}
		}
		return ss
	}
	objs := func(ss *appsv1.StatefulSet) []runtime.Object {
		return []runtime.Object{ss, pod("kong", "db-0", "kic", "StatefulSet", "db", republished), roleBinding("kong", "kic", sa("kong", "kic"))}
	}
	for _, tc := range []struct {
		name    string
		ss      *appsv1.StatefulSet
		o       CARotateOptions
		wantErr bool
	}{
		{"partition above 0", statefulSet(new(int32(1))), CARotateOptions{RestartWorkloads: true}, true},
		{"partition 0", statefulSet(new(int32(0))), CARotateOptions{RestartWorkloads: true}, false},
		{"no partition", statefulSet(nil), CARotateOptions{RestartWorkloads: true}, false},
		{"partition above 0, check skipped", statefulSet(new(int32(1))), CARotateOptions{RestartWorkloads: true, SkipWorkloadCheck: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightWorkloads(context.Background(), io.Discard, client(objs(tc.ss)...), tc.o)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("passed; want a refusal")
			}
			for _, w := range []string{"refusing to start", "rollingUpdate partition", "StatefulSet kong/db", "Set the partition to 0", "--skip-workload-check"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

// Owners are listed once per scan, whatever the number of pods and controllers,
// not fetched one by one.
func TestSelectClientsListsOwnersOnce(t *testing.T) {
	var objs []runtime.Object
	for i := range 20 {
		name := "app" + strconv.Itoa(i)
		objs = append(objs,
			pod("apps", name+"-pod", "kic", "ReplicaSet", name+"-1", republished), replicaSet("apps", name+"-1", name),
			pod("apps", name+"-ds", "kic", "DaemonSet", name+"-d", republished),
			pod("apps", name+"-ss-0", "kic", "StatefulSet", name+"-s", republished))
	}
	objs = append(objs, roleBinding("apps", "kic", sa("apps", "kic")))
	cs := client(objs...)
	got, err := selectClients(context.Background(), cs, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 60 {
		t.Fatalf("clients = %d (%v), want 60", len(got), owners(got))
	}
	lists := map[string]int{}
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "get":
			t.Errorf("a get of %s: owners are looked up in the lists", a.GetResource().Resource)
		case "list":
			lists[a.GetResource().Resource]++
		}
	}
	for _, r := range []string{"replicasets", "deployments", "daemonsets", "statefulsets"} {
		if lists[r] != 1 {
			t.Errorf("%s listed %d times, want 1", r, lists[r])
		}
	}
}

// The forward-dated time is caught in the preflight, which runs before trust-new,
// so the run is refused while nothing has changed, not halted after the rollouts.
func TestPreflightWorkloadsRefusesAForwardDatedRepublish(t *testing.T) {
	_, newPEM := genCA(t, "new")
	for _, tc := range []struct {
		name    string
		ns      string // namespace of the forward-dated kube-root-ca.crt
		at      time.Time
		o       CARotateOptions
		wantErr bool
	}{
		{"a client's namespace, an hour ahead", "kong", time.Now().Add(time.Hour), CARotateOptions{RestartWorkloads: true}, true},
		{"within the allowance", "kong", time.Now().Add(republishClockSkew / 2), CARotateOptions{RestartWorkloads: true}, false},
		{"a namespace without a client", "empty", time.Now().Add(time.Hour), CARotateOptions{RestartWorkloads: true}, false},
		{"check skipped", "kong", time.Now().Add(time.Hour), CARotateOptions{RestartWorkloads: true, SkipWorkloadCheck: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", republished), replicaSet("kong", "ingress-1", "ingress"),
				roleBinding("kong", "kic", sa("kong", "kic")), rootCA(tc.ns, newPEM, tc.at)}
			if tc.ns != "kong" {
				objs = append(objs, rootCA("kong", newPEM, republished))
			}
			cs := client(objs...)
			err := preflightWorkloads(context.Background(), io.Discard, cs, tc.o)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "ahead of this host's clock") {
				t.Fatalf("error = %v, want a refusal for the forward-dated time", err)
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() != "list" && a.GetVerb() != "get" {
					t.Errorf("the preflight did a %s of %s", a.GetVerb(), a.GetResource().Resource)
				}
			}
		})
	}
}

// The post-rollout gate counts the add-ons Stratum rolls, so a forward-dated time
// in a namespace that holds only those is refused in the preflight too, with or
// without --restart-workloads, and nothing is changed.
func TestPreflightWorkloadsRefusesAForwardDatedRepublishInAnOwnedOnlyNamespace(t *testing.T) {
	_, newPEM := genCA(t, "new")
	for _, tc := range []struct {
		name string
		o    CARotateOptions
	}{
		{"without --restart-workloads", CARotateOptions{}},
		{"with --restart-workloads", CARotateOptions{RestartWorkloads: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := client(
				pod("kube-system", "coredns-a", "coredns", "ReplicaSet", "coredns-1", republished), replicaSet("kube-system", "coredns-1", "coredns"),
				roleBinding("kube-system", "coredns", sa("kube-system", "coredns")), rootCA("kube-system", newPEM, time.Now().Add(time.Hour)))
			err := preflightWorkloads(context.Background(), io.Discard, cs, tc.o)
			if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "kube-system/kube-root-ca.crt") || !strings.Contains(err.Error(), "ahead of this host's clock") {
				t.Fatalf("error = %v, want a refusal for the forward-dated time", err)
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() != "list" && a.GetVerb() != "get" {
					t.Errorf("the preflight did a %s of %s", a.GetVerb(), a.GetResource().Resource)
				}
			}
		})
	}
}

// The preflight reads the update strategies from the lists the owner lookup
// already made: no Get per workload, and each of StatefulSets and DaemonSets is
// listed once whatever their number.
func TestPreflightWorkloadsReadsUpdateStrategiesFromTheOwnerLists(t *testing.T) {
	objs := []runtime.Object{roleBinding("apps", "kic", sa("apps", "kic"))}
	for i := range 20 {
		name := "app" + strconv.Itoa(i)
		ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name + "-s", UID: uidOf("StatefulSet", "apps", name+"-s")}}
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name + "-d", UID: uidOf("DaemonSet", "apps", name+"-d")}}
		objs = append(objs, ss, ds,
			pod("apps", name+"-s-0", "kic", "StatefulSet", name+"-s", republished), pod("apps", name+"-d-x", "kic", "DaemonSet", name+"-d", republished))
	}
	cs := client(objs...)
	if err := preflightWorkloads(context.Background(), io.Discard, cs, CARotateOptions{RestartWorkloads: true}); err != nil {
		t.Fatalf("refused: %v", err)
	}
	lists := map[string]int{}
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "get":
			t.Errorf("a get of %s: update strategies are read from the lists", a.GetResource().Resource)
		case "list":
			lists[a.GetResource().Resource]++
		}
	}
	for _, r := range []string{"statefulsets", "daemonsets"} {
		if lists[r] != 1 {
			t.Errorf("%s listed %d times, want 1", r, lists[r])
		}
	}
}

// A template change does not recreate the pods of a StatefulSet or DaemonSet that
// updates only on delete, so they stay on the old CA: refused in the preflight.
func TestPreflightWorkloadsRefusesOnDelete(t *testing.T) {
	statefulSet := func(t appsv1.StatefulSetUpdateStrategyType) *appsv1.StatefulSet {
		ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "db", UID: uidOf("StatefulSet", "apps", "db")}}
		ss.Spec.UpdateStrategy.Type = t
		return ss
	}
	daemonSet := func(t appsv1.DaemonSetUpdateStrategyType) *appsv1.DaemonSet {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "agent", UID: uidOf("DaemonSet", "apps", "agent")}}
		ds.Spec.UpdateStrategy.Type = t
		return ds
	}
	for _, tc := range []struct {
		name    string
		obj     runtime.Object
		pod     *corev1.Pod
		o       CARotateOptions
		wantErr string // empty means the preflight passes
	}{
		{"StatefulSet OnDelete", statefulSet(appsv1.OnDeleteStatefulSetStrategyType), pod("apps", "db-0", "kic", "StatefulSet", "db", republished), CARotateOptions{RestartWorkloads: true}, "StatefulSet apps/db"},
		{"DaemonSet OnDelete", daemonSet(appsv1.OnDeleteDaemonSetStrategyType), pod("apps", "agent-x", "kic", "DaemonSet", "agent", republished), CARotateOptions{RestartWorkloads: true}, "DaemonSet apps/agent"},
		{"StatefulSet RollingUpdate", statefulSet(appsv1.RollingUpdateStatefulSetStrategyType), pod("apps", "db-0", "kic", "StatefulSet", "db", republished), CARotateOptions{RestartWorkloads: true}, ""},
		{"DaemonSet RollingUpdate", daemonSet(appsv1.RollingUpdateDaemonSetStrategyType), pod("apps", "agent-x", "kic", "DaemonSet", "agent", republished), CARotateOptions{RestartWorkloads: true}, ""},
		{"StatefulSet OnDelete, check skipped", statefulSet(appsv1.OnDeleteStatefulSetStrategyType), pod("apps", "db-0", "kic", "StatefulSet", "db", republished), CARotateOptions{RestartWorkloads: true, SkipWorkloadCheck: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := client(tc.obj, tc.pod, roleBinding("apps", "kic", sa("apps", "kic")))
			err := preflightWorkloads(context.Background(), io.Discard, cs, tc.o)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("passed; want a refusal")
			}
			for _, w := range []string{"refusing to start", tc.wantErr, "OnDelete", "RollingUpdate", "--skip-workload-check"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

// Stratum restarts its own add-ons whatever the flags say, so one that a restart
// does not reach whole (here Cilium with OnDelete) is refused in the preflight
// with or without --restart-workloads, and nothing is changed.
func TestPreflightWorkloadsRefusesAHeldOwnedAddOn(t *testing.T) {
	cilium := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium", UID: uidOf("DaemonSet", "kube-system", "cilium")}}
	cilium.Spec.UpdateStrategy.Type = appsv1.OnDeleteDaemonSetStrategyType
	for _, tc := range []struct {
		name string
		o    CARotateOptions
	}{
		{"without --restart-workloads", CARotateOptions{}},
		{"with --restart-workloads", CARotateOptions{RestartWorkloads: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := client(cilium, pod("kube-system", "cilium-x", "cilium", "DaemonSet", "cilium", republished), roleBinding("kube-system", "cilium", sa("kube-system", "cilium")))
			err := preflightWorkloads(context.Background(), io.Discard, cs, tc.o)
			if err == nil {
				t.Fatal("passed; want a refusal")
			}
			for _, w := range []string{"refusing to start", "DaemonSet kube-system/cilium", "OnDelete", "Switch the update strategy of Stratum's own add-ons to RollingUpdate"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal lacks %q:\n%v", w, err)
				}
			}
			// The flag does not bypass the rollout of an add-on, so it is not offered.
			if strings.Contains(err.Error(), "--skip-workload-check") {
				t.Errorf("refusal offers --skip-workload-check for a Stratum add-on:\n%v", err)
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() != "list" && a.GetVerb() != "get" {
					t.Errorf("the preflight did a %s of %s", a.GetVerb(), a.GetResource().Resource)
				}
			}
		})
	}
}

// A user workload keeps the skip advice, and says so next to an add-on's refusal.
func TestRefuseHeldAdvice(t *testing.T) {
	held := func(kind, ns, name string) clientWorkload {
		return clientWorkload{Owner: owner{kind, ns, name}, Held: onDeleteHeld}
	}
	user, addOn := held("StatefulSet", "apps", "db"), held("DaemonSet", "kube-system", "cilium")
	for _, tc := range []struct {
		name     string
		ws       []clientWorkload
		wantSkip bool
		wantSwap bool
	}{
		{"user workload", []clientWorkload{user}, true, false},
		{"add-on", []clientWorkload{addOn}, false, true},
		{"both", []clientWorkload{user, addOn}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseHeld(tc.ws)
			if err == nil {
				t.Fatal("passed; want a refusal")
			}
			if got := strings.Contains(err.Error(), "--skip-workload-check"); got != tc.wantSkip {
				t.Errorf("offers --skip-workload-check = %v, want %v:\n%v", got, tc.wantSkip, err)
			}
			if got := strings.Contains(err.Error(), "Stratum's own add-ons to RollingUpdate"); got != tc.wantSwap {
				t.Errorf("tells to switch the add-ons = %v, want %v:\n%v", got, tc.wantSwap, err)
			}
		})
	}
}

// waiting is a pod of the kong/ingress Deployment, created at created, whose
// container has not started: state is its status (Waiting, or Terminated between
// restarts), or nil for a pod kubelet has not reported on yet.
func waiting(created time.Time, state *corev1.ContainerState) *corev1.Pod {
	p := pod("kong", "ingress-w", "kic", "ReplicaSet", "ingress-1", created)
	p.CreationTimestamp = metav1.NewTime(created)
	p.Status.ContainerStatuses = nil
	if state != nil {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: *state}}
	}
	return p
}

// A pod created before the republish whose container has not started can still
// start on a ca.crt kubelet has not synced, until projectedCAMargin is over.
func TestPodMayStillStart(t *testing.T) {
	now := time.Now()
	at := now.Add(-30 * time.Second) // inside the margin
	late := now.Add(-projectedCAMargin - time.Minute)
	created := at.Add(-time.Hour)
	crashLoop := &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	pending := &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	restarting := &corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		at   time.Time
		want bool
	}{
		{"pending, inside the margin", waiting(created, pending), at, true},
		{"crash loop, inside the margin", waiting(created, crashLoop), at, true},
		{"between restarts, inside the margin", waiting(created, restarting), at, true},
		{"not reported yet, inside the margin", waiting(created, nil), at, true},
		{"crash loop, after the margin", waiting(created, crashLoop), late, false},
		{"created after the republish", waiting(at.Add(time.Second), crashLoop), at, false},
		{"running", pod("kong", "ingress-r", "kic", "ReplicaSet", "ingress-1", at.Add(time.Hour)), at, false},
		{"terminating", terminating(waiting(created, crashLoop), time.Minute), at, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podMayStillStart(tc.pod, tc.at, now); got != tc.want {
				t.Errorf("podMayStillStart = %v, want %v", got, tc.want)
			}
		})
	}
}

// The gate counts such a pod as stale inside the margin and not after it.
func TestCheckClientsRestartedCountsAPodThatMayStillStart(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	crashLoop := &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	for _, tc := range []struct {
		name    string
		at      time.Time
		wantErr bool
	}{
		{"inside the margin", time.Now().Add(-30 * time.Second), true},
		{"after the margin", time.Now().Add(-projectedCAMargin - time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := client(rootCA("kong", newPEM, tc.at), waiting(tc.at.Add(-time.Hour), crashLoop), replicaSet("kong", "ingress-1", "ingress"),
				roleBinding("kong", "kic", sa("kong", "kic")))
			err := checkClientsRestarted(context.Background(), cs, newCA, nil)
			switch {
			case !tc.wantErr && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr && (err == nil || !strings.Contains(err.Error(), "ingress-w") && !strings.Contains(err.Error(), "Deployment kong/ingress")):
				t.Fatalf("error = %v, want the Deployment listed as stale", err)
			}
		})
	}
}

// The wait before sign-new waits such a pod out with its re-scan and goes on once
// the margin is over, where a stale pod that will not go ends it at once.
func TestWaitClientsRestartedWaitsOutTheMargin(t *testing.T) {
	newCA, newPEM := genCA(t, "new")
	oldPoll, oldWait := republishPoll, clientRolloutWait
	republishPoll, clientRolloutWait = 20*time.Millisecond, 10*time.Second
	t.Cleanup(func() { republishPoll, clientRolloutWait = oldPoll, oldWait })
	crashLoop := &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	at := time.Now().Add(-projectedCAMargin + 400*time.Millisecond) // the margin ends in 400ms
	cs := client(rootCA("kong", newPEM, at), waiting(at.Add(-time.Hour), crashLoop), replicaSet("kong", "ingress-1", "ingress"),
		roleBinding("kong", "kic", sa("kong", "kic")))
	start := time.Now()
	if err := waitClientsRestarted(context.Background(), cs, newCA, nil); err != nil {
		t.Fatalf("halted on a pod that was only waiting out the margin: %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("went on after %s, before the margin was over", time.Since(start))
	}
}

// republishedSince is what tells restartClients that kube-controller-manager
// wrote kube-root-ca.crt after the clients were rolled, so they roll again.
func TestRepublishedSince(t *testing.T) {
	rolled := time.Date(2026, 10, 12, 18, 0, 0, 500_000_000, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before the roll", rolled.Add(-time.Minute), false},
		{"in the same second", rolled.Truncate(time.Second), true},
		{"after the roll", rolled.Add(time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
				rootCA("default", "bundle", tc.at),
			)
			if got := republishedSince(context.Background(), cs, rolled); got != tc.want {
				t.Errorf("republishedSince = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRestartClientsRollsAgainAfterALateRepublish: kube-controller-manager
// writes kube-root-ca.crt during the restarts, which dates the rolled pod
// before the republish. restartClients rolls once more and goes on. The write
// lands in the same second the roll starts and the roll ends a second later, so
// a rolledAt taken after the roll would miss it.
func TestRestartClientsRollsAgainAfterALateRepublish(t *testing.T) {
	_, newPEM := genCA(t, "new")
	_, oldPEM := genCA(t, "old")
	stageNewCA(t, newPEM)
	cs := client(
		rootCA("kong", newPEM+oldPEM, republished),
		pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", time.Now().Add(-time.Minute)),
		replicaSet("kong", "ingress-1", "ingress"),
		roleBinding("kong", "kic", sa("kong", "kic")),
	)
	ctx := context.Background()
	var mu sync.Mutex
	cmAt := republished
	cs.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		return true, &corev1.ConfigMapList{Items: []corev1.ConfigMap{*rootCA("kong", newPEM+oldPEM, cmAt)}}, nil
	})
	rolls := 0
	k := &fakeKubectl{}
	kubectl := func(ctx context.Context, d time.Duration, b bool, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "rollout restart deployment/ingress") {
			rolls++
			switch rolls {
			case 1:
				now := time.Now().Truncate(time.Second)
				mu.Lock()
				cmAt = now
				mu.Unlock()
				time.Sleep(time.Until(now.Add(1100 * time.Millisecond)))
			case 2:
				rolled := pod("kong", "ingress-a", "kic", "ReplicaSet", "ingress-1", time.Now().Add(time.Second))
				rolled.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Second))
				if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), rolled, "kong"); err != nil {
					t.Error(err)
				}
			}
		}
		return k.run(ctx, d, b, args...)
	}
	roller := rollout.Roller{Kubectl: kubectl, Hint: adminKubectlHint, CallTimeout: 5 * time.Second, Wait: 5 * time.Second}
	if err := restartClients(ctx, io.Discard, cs, CARotateOptions{RestartWorkloads: true}, roller); err != nil {
		t.Fatalf("restartClients: %v", err)
	}
	if rolls != 2 {
		t.Errorf("rolled the Deployment %d time(s), want 2", rolls)
	}
}
