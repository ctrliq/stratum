// In-cluster API clients and CA rotation. A client that loads the cluster CA
// once, at start, fails with x509 errors from sign-new until it restarts. It
// can restart safely only after kube-controller-manager has republished
// kube-root-ca.crt as old‖new, which it does once trust-new is on every node,
// so the restart goes between trust-new and sign-new. This file finds those
// clients, waits for the republish, rolls what Stratum owns, and gates on what
// it does not.
package certs

import (
	"bytes"
	"cmp"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/rollout"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	rootCAConfigMap = "kube-root-ca.crt"
	// serviceAccountsGroup is the group every ServiceAccount is in; the
	// per-namespace group is serviceAccountsGroup + ":" + namespace.
	serviceAccountsGroup = "system:serviceaccounts"
	saTokenSecretType    = "kubernetes.io/service-account-token"
	// serviceAccountUserPrefix starts the username of a ServiceAccount.
	serviceAccountUserPrefix = "system:serviceaccount:"

	// republishClockSkew is how far ahead of this host's clock a kube-root-ca.crt
	// write time may be before it is not believed. A namespace admin can set
	// managedFields on their own ConfigMap, and a time in the future would count
	// their pods as stale for good.
	republishClockSkew = 5 * time.Minute
	// projectedCAMargin is how long after the republish a container of a pod that
	// existed before it can still hold the old ca.crt: kubelet syncs a projected
	// volume about once a minute, so a container that started inside the window
	// can have mounted the content from before the write.
	projectedCAMargin = 2 * time.Minute
)

// Vars, so tests can shorten them.
var (
	// republishWait is how long to wait for kube-root-ca.crt to carry the new
	// CA in every namespace once trust-new is on every node.
	republishWait = 5 * time.Minute
	republishPoll = 2 * time.Second
	// clientRolloutWait is the total time the restarted workloads have to
	// finish rolling out.
	clientRolloutWait = 10 * time.Minute
	clientCallTimeout = 30 * time.Second
)

// adminKubectl is rollout.Roller's Kubectl against the cluster's admin.conf. A
// call that gets no answer in time is killed, never waited on.
func adminKubectl(ctx context.Context, timeout time.Duration, combined bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := k8sutil.KubectlContext(ctx, args...)
	cmd.WaitDelay = 5 * time.Second // a kubectl killed with ctx must not hold its pipes open
	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("no answer within %s: %w", timeout, err)
	}
	return out, err
}

func adminKubectlHint(args string) string { return "stratum kubectl " + args }

// stagedNewCA is the CA trust-new staged, the one kube-root-ca.crt must carry:
// ca-new.crt, or ca.crt on a node whose sign-new promoted it and stopped.
func stagedNewCA() (*x509.Certificate, error) {
	name := pki.StagedRootCAName + ".crt"
	if !pki.CARotationStaged() && pki.SignNewResumable() {
		name = pki.RootCAName + ".crt"
	}
	b, err := os.ReadFile(filepath.Join(version.PKIDir, name))
	if err != nil {
		return nil, fmt.Errorf("read the staged new CA: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("the staged new CA %s/%s holds no certificate", version.PKIDir, name)
	}
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the staged new CA: %w", err)
	}
	return ca, nil
}

// carries reports whether the PEM bundle holds ca.
func carries(bundle string, ca *x509.Certificate) bool {
	rest := []byte(bundle)
	for {
		var blk *pem.Block
		if blk, rest = pem.Decode(rest); blk == nil {
			return false
		}
		if blk.Type == "CERTIFICATE" && bytes.Equal(blk.Bytes, ca.Raw) {
			return true
		}
	}
}

// lastRepublish is the latest kube-controller-manager write in cm's
// managedFields, zero when there is none.
func lastRepublish(cm *corev1.ConfigMap) time.Time {
	var at time.Time
	for _, f := range cm.ManagedFields {
		if f.Manager == "kube-controller-manager" && f.Operation == metav1.ManagedFieldsOperationUpdate && f.Time != nil && f.Time.After(at) {
			at = f.Time.Time
		}
	}
	return at
}

// errForwardDated is an error when at is further ahead than republishClockSkew:
// the namespace's admin wrote it.
func errForwardDated(cm *corev1.ConfigMap, at time.Time) error {
	if at.After(time.Now().Add(republishClockSkew)) {
		return fmt.Errorf("%s/%s was written by kube-controller-manager at %s, more than %s ahead of this host's clock; its managedFields are not to be trusted (a namespace admin can set them), so pods cannot be told apart by start time",
			cm.Namespace, rootCAConfigMap, at.UTC().Format(time.RFC3339), republishClockSkew)
	}
	return nil
}

// republishedAt is when kube-controller-manager last wrote cm, which is when
// the namespace's kube-root-ca.crt took its current content.
func republishedAt(cm *corev1.ConfigMap) (time.Time, error) {
	at := lastRepublish(cm)
	if at.IsZero() {
		return at, fmt.Errorf("cannot tell when %s/%s took the new CA (no kube-controller-manager write in its managedFields)", cm.Namespace, rootCAConfigMap)
	}
	return at, errForwardDated(cm, at)
}

// rootCAConfigMaps is each namespace's kube-root-ca.crt, keyed by namespace,
// and the namespaces that are not being deleted (a terminating one is not
// published to).
func rootCAConfigMaps(ctx context.Context, cs kubernetes.Interface) (cms map[string]*corev1.ConfigMap, namespaces []string, err error) {
	nl, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list namespaces: %w", err)
	}
	cml, err := cs.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + rootCAConfigMap})
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", rootCAConfigMap, err)
	}
	cms = map[string]*corev1.ConfigMap{}
	for i := range cml.Items {
		if cml.Items[i].Name == rootCAConfigMap {
			cms[cml.Items[i].Namespace] = &cml.Items[i]
		}
	}
	for i := range nl.Items {
		if nl.Items[i].DeletionTimestamp == nil {
			namespaces = append(namespaces, nl.Items[i].Name)
		}
	}
	return cms, namespaces, nil
}

// waitRootCARepublished waits until kube-root-ca.crt carries ca in every
// namespace. A pod that starts before that gets a ca.crt without the new CA and
// fails after sign-new however often it restarts.
func waitRootCARepublished(ctx context.Context, cs kubernetes.Interface, ca *x509.Certificate) error {
	ctx, cancel := context.WithTimeout(ctx, republishWait)
	defer cancel()
	for {
		cms, namespaces, err := rootCAConfigMaps(ctx, cs)
		var lagging []string
		if err == nil {
			for _, ns := range namespaces {
				if cm := cms[ns]; cm == nil || !carries(cm.Data["ca.crt"], ca) {
					lagging = append(lagging, ns)
				}
			}
			if len(lagging) == 0 {
				return nil
			}
			err = fmt.Errorf("still without the new CA: %s", strings.Join(lagging, ", "))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not carry the new CA in every namespace within %s (is kube-controller-manager running on the new bundle?): %w", rootCAConfigMap, republishWait, err)
		case <-time.After(republishPoll):
		}
	}
}

// owner is the controller a pod belongs to.
type owner struct{ Kind, Namespace, Name string }

// plain is a name that needs no quoting to print or to paste into a shell.
var plain = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// quote is s as it is printed: as it is when plain, else Go-quoted, so no name
// can carry a newline or an escape sequence into the output.
func quote(s string) string {
	if plain.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// shellSafe is an argument a POSIX shell takes literally unquoted.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// shellQuote is s as one argument of a POSIX shell command.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (o owner) String() string { return quote(o.Kind) + " " + quote(o.Namespace) + "/" + quote(o.Name) }

// target is o as rollout.Workload names it, "kind/name", when Stratum rolls
// that kind. Jobs, bare Pods and anything else are never rolled.
func (o owner) target() (string, bool) {
	switch o.Kind {
	case "Deployment", "DaemonSet", "StatefulSet":
		return strings.ToLower(o.Kind) + "/" + o.Name, true
	}
	return "", false
}

// guidance is what an operator does about o by hand. The command is meant to
// be pasted into a shell, so every argument is quoted.
func (o owner) guidance() string {
	if t, ok := o.target(); ok {
		cmd := adminKubectlHint("-n " + shellQuote(o.Namespace) + " rollout restart " + shellQuote(t))
		if o.Kind == "StatefulSet" {
			cmd += " (with a rollingUpdate partition, also delete the pods below it)"
		}
		return cmd
	}
	return "recreate it (Stratum never restarts a " + quote(o.Kind) + ")"
}

// clientWorkload is the pods of one owner that use in-cluster API credentials.
type clientWorkload struct {
	Owner owner
	Pods  []*corev1.Pod
	// Settling is, for the stale pods none of whose containers ran before the
	// republish, when the wait can end: they have not started, and can still start
	// on a ca.crt kubelet has not synced. It is nil unless staleClients made it.
	Settling map[*corev1.Pod]time.Time
	// Held is why a template change does not recreate all of the owner's pods
	// (an update strategy that leaves some or all of them), or "" when it does.
	Held string
}

// saGrants is who the cluster's RoleBindings and ClusterRoleBindings give
// permissions to, among ServiceAccounts, the way the RBAC authorizer reads
// their subjects.
type saGrants struct {
	accounts map[string]bool // "namespace/name"
	groups   map[string]bool // namespaces with a system:serviceaccounts:<ns> subject
}

func (g *saGrants) add(bindingNamespace string, subjects []rbacv1.Subject) {
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.ServiceAccountKind:
			// A subject without a namespace is the binding's own.
			if ns := cmp.Or(s.Namespace, bindingNamespace); ns != "" {
				g.accounts[ns+"/"+s.Name] = true
			}
		case rbacv1.UserKind:
			// The authorizer also reads a service account as a user, by its username.
			if rest, ok := strings.CutPrefix(s.Name, serviceAccountUserPrefix); ok {
				if ns, name, ok := strings.Cut(rest, ":"); ok && ns != "" && name != "" {
					g.accounts[ns+"/"+name] = true
				}
			}
		case rbacv1.GroupKind:
			// Not system:serviceaccounts itself: the bootstrap policy binds it
			// (system:service-account-issuer-discovery), so it would match every pod.
			if ns, ok := strings.CutPrefix(s.Name, serviceAccountsGroup+":"); ok {
				g.groups[ns] = true
			}
		}
	}
}

func (g *saGrants) has(namespace, name string) bool {
	return g.groups[namespace] || g.accounts[namespace+"/"+name]
}

func listSAGrants(ctx context.Context, cs kubernetes.Interface) (*saGrants, error) {
	g := &saGrants{accounts: map[string]bool{}, groups: map[string]bool{}}
	rbl, err := cs.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list RoleBindings: %w", err)
	}
	for i := range rbl.Items {
		g.add(rbl.Items[i].Namespace, rbl.Items[i].Subjects)
	}
	crbl, err := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ClusterRoleBindings: %w", err)
	}
	for i := range crbl.Items {
		g.add("", crbl.Items[i].Subjects)
	}
	return g, nil
}

// legacyTokenSecrets is the ServiceAccount each kubernetes.io/service-account-token
// Secret holds a token for, by "namespace/name" of the Secret. That is not
// necessarily the account of a pod that mounts it.
func legacyTokenSecrets(ctx context.Context, cs kubernetes.Interface) (map[string]string, error) {
	sl, err := cs.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "type=" + saTokenSecretType})
	if err != nil {
		return nil, fmt.Errorf("list service-account token Secrets: %w", err)
	}
	out := map[string]string{}
	for i := range sl.Items {
		if sl.Items[i].Type == saTokenSecretType {
			out[sl.Items[i].Namespace+"/"+sl.Items[i].Name] = sl.Items[i].Annotations[corev1.ServiceAccountNameKey]
		}
	}
	return out, nil
}

// mountedAccounts is the ServiceAccounts whose tokens a container of the pod
// mounts: the pod's own for a projected volume with a service-account token
// source (whatever it is named), and the account each legacy token Secret was
// issued for, which can be another's. The container can be a regular, init or
// ephemeral one: a volume nothing mounts is no client.
func mountedAccounts(pod *corev1.Pod, legacy func() (map[string]string, error)) ([]string, error) {
	tokenVolumes := map[string]string{} // volume name -> the account its token is for
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(s corev1.VolumeProjection) bool { return s.ServiceAccountToken != nil }):
			tokenVolumes[v.Name] = cmp.Or(pod.Spec.ServiceAccountName, "default")
		case v.Secret != nil:
			secrets, err := legacy()
			if err != nil {
				return nil, err
			}
			if account, ok := secrets[pod.Namespace+"/"+v.Secret.SecretName]; ok {
				tokenVolumes[v.Name] = account
			}
		}
	}
	var accounts []string
	mounts := func(ms []corev1.VolumeMount) {
		for _, m := range ms {
			if account, ok := tokenVolumes[m.Name]; ok && !slices.Contains(accounts, account) {
				accounts = append(accounts, account)
			}
		}
	}
	for _, c := range pod.Spec.Containers {
		mounts(c.VolumeMounts)
	}
	for _, c := range pod.Spec.InitContainers {
		mounts(c.VolumeMounts)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		mounts(c.VolumeMounts)
	}
	return accounts, nil
}

// onDeleteHeld is the Held of a StatefulSet or DaemonSet with the OnDelete update
// strategy, which only replaces a pod when it is deleted.
const onDeleteHeld = "the OnDelete update strategy does not recreate pods on a restart"

// kindName is what a Kubernetes kind looks like.
var kindName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)

// ownerResolver names the workload a pod belongs to from its controller
// reference. The reference is the pod author's to write: the API server checks
// neither that the owner exists nor what its name looks like. So a Deployment,
// DaemonSet or StatefulSet owner counts only when it exists with the reference's
// UID and a DNS-1123 name; anything else is the pod itself, which Stratum lists
// and never restarts. Otherwise a tenant could name a workload that is not
// theirs, put text in front of the operator, or have the rotation halted by an
// owner that does not exist.
//
// The owners are listed once, on the first pod that needs one, and looked up in
// maps: a Get per owner is thousands of requests per scan on a big cluster.
type ownerResolver struct {
	cs          kubernetes.Interface
	loaded      bool
	replicaSets map[string]*appsv1.ReplicaSet // by namespace/name
	owners      map[string]ownerInfo          // Deployments, DaemonSets and StatefulSets, by kind/namespace/name
}

// ownerInfo is what is kept of a Deployment, DaemonSet or StatefulSet.
type ownerInfo struct {
	uid  types.UID
	held string // see clientWorkload.Held
}

func newOwnerResolver(cs kubernetes.Interface) *ownerResolver {
	return &ownerResolver{cs: cs}
}

func validName(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }

func (r *ownerResolver) load(ctx context.Context) error {
	if r.loaded {
		return nil
	}
	apps := r.cs.AppsV1()
	rsl, err := apps.ReplicaSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list ReplicaSets: %w", err)
	}
	dl, err := apps.Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Deployments: %w", err)
	}
	dsl, err := apps.DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list DaemonSets: %w", err)
	}
	ssl, err := apps.StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list StatefulSets: %w", err)
	}
	r.replicaSets = make(map[string]*appsv1.ReplicaSet, len(rsl.Items))
	r.owners = make(map[string]ownerInfo, len(dl.Items)+len(dsl.Items)+len(ssl.Items))
	for i := range rsl.Items {
		r.replicaSets[rsl.Items[i].Namespace+"/"+rsl.Items[i].Name] = &rsl.Items[i]
	}
	for i := range dl.Items {
		r.owners["Deployment/"+dl.Items[i].Namespace+"/"+dl.Items[i].Name] = ownerInfo{uid: dl.Items[i].UID}
	}
	for i := range dsl.Items {
		held := ""
		if dsl.Items[i].Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
			held = onDeleteHeld
		}
		r.owners["DaemonSet/"+dsl.Items[i].Namespace+"/"+dsl.Items[i].Name] = ownerInfo{uid: dsl.Items[i].UID, held: held}
	}
	for i := range ssl.Items {
		held, st := "", ssl.Items[i].Spec.UpdateStrategy
		if st.Type == appsv1.OnDeleteStatefulSetStrategyType {
			held = onDeleteHeld
		} else if st.RollingUpdate != nil && st.RollingUpdate.Partition != nil && *st.RollingUpdate.Partition > 0 {
			held = "a rollingUpdate partition leaves the pods below it"
		}
		r.owners["StatefulSet/"+ssl.Items[i].Namespace+"/"+ssl.Items[i].Name] = ownerInfo{uid: ssl.Items[i].UID, held: held}
	}
	r.loaded = true
	return nil
}

// exists reports whether the Deployment, DaemonSet or StatefulSet ref names
// exists in namespace with ref's UID.
func (r *ownerResolver) exists(ctx context.Context, namespace string, ref metav1.OwnerReference) (bool, error) {
	switch ref.Kind {
	case "Deployment", "DaemonSet", "StatefulSet":
	default:
		return false, nil
	}
	if !validName(ref.Name) {
		return false, nil
	}
	if err := r.load(ctx); err != nil {
		return false, err
	}
	info, ok := r.owners[ref.Kind+"/"+namespace+"/"+ref.Name]
	return ok && ref.UID != "" && info.uid == ref.UID, nil
}

// held is why a restart of o does not recreate all of its pods, from the lists
// load made.
func (r *ownerResolver) held(o owner) string {
	return r.owners[o.Kind+"/"+o.Namespace+"/"+o.Name].held
}

// replicaSet is the ReplicaSet ref names, or nil when there is none with ref's UID.
func (r *ownerResolver) replicaSet(ctx context.Context, namespace string, ref metav1.OwnerReference) (*appsv1.ReplicaSet, error) {
	if !validName(ref.Name) {
		return nil, nil
	}
	if err := r.load(ctx); err != nil {
		return nil, err
	}
	if rs := r.replicaSets[namespace+"/"+ref.Name]; rs != nil && ref.UID != "" && rs.UID == ref.UID {
		return rs, nil
	}
	return nil, nil
}

func (r *ownerResolver) resolve(ctx context.Context, pod *corev1.Pod) (owner, error) {
	bare := owner{"Pod", pod.Namespace, pod.Name}
	c := metav1.GetControllerOf(pod)
	if c == nil {
		return bare, nil
	}
	switch c.Kind {
	case "ReplicaSet":
		rs, err := r.replicaSet(ctx, pod.Namespace, *c)
		if err != nil || rs == nil {
			return bare, err
		}
		if rc := metav1.GetControllerOf(rs); rc != nil && rc.Kind == "Deployment" {
			ok, err := r.exists(ctx, pod.Namespace, *rc)
			if err != nil {
				return owner{}, err
			}
			if ok {
				return owner{"Deployment", pod.Namespace, rc.Name}, nil
			}
		}
		return owner{"ReplicaSet", pod.Namespace, rs.Name}, nil
	case "Deployment", "DaemonSet", "StatefulSet":
		ok, err := r.exists(ctx, pod.Namespace, *c)
		if err != nil || !ok {
			return bare, err
		}
		return owner{c.Kind, pod.Namespace, c.Name}, nil
	}
	// Jobs and whatever else a CRD controller owns: listed, never restarted, so
	// the reference is not looked up, but it must still look like a kind and a name.
	if kindName.MatchString(c.Kind) && validName(c.Name) {
		return owner{c.Kind, pod.Namespace, c.Name}, nil
	}
	return bare, nil
}

// selectClients finds the workloads whose pods use in-cluster API credentials.
// Stratum cannot see which clients do, so a pod counts when it mounts a
// ServiceAccount token and that ServiceAccount is a subject of a RoleBinding or
// ClusterRoleBinding, by name or through system:serviceaccounts:<namespace>.
// A client that has no grant but its group's is missed. Pods that have
// finished are left out, and so are the workloads Stratum rolls itself unless
// includeOwned is set.
func selectClients(ctx context.Context, cs kubernetes.Interface, includeOwned bool) ([]clientWorkload, error) {
	pl, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	grants, err := listSAGrants(ctx, cs)
	if err != nil {
		return nil, err
	}
	var legacy map[string]string
	legacyOnce := func() (map[string]string, error) {
		var err error
		if legacy == nil {
			legacy, err = legacyTokenSecrets(ctx, cs)
		}
		return legacy, err
	}
	owners := newOwnerResolver(cs)
	byOwner := map[owner]*clientWorkload{}
	for i := range pl.Items {
		pod := &pl.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// A client is a pod that mounts a token of a ServiceAccount that has a grant.
		accounts, err := mountedAccounts(pod, legacyOnce)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(accounts, func(account string) bool { return grants.has(pod.Namespace, account) }) {
			continue
		}
		o, err := owners.resolve(ctx, pod)
		if err != nil {
			return nil, err
		}
		if !includeOwned && rollout.Owned(o.Namespace, o.Kind, o.Name) {
			continue
		}
		if byOwner[o] == nil {
			byOwner[o] = &clientWorkload{Owner: o, Held: owners.held(o)}
		}
		byOwner[o].Pods = append(byOwner[o].Pods, pod)
	}
	out := make([]clientWorkload, 0, len(byOwner))
	for _, w := range byOwner {
		out = append(out, *w)
	}
	slices.SortFunc(out, func(a, b clientWorkload) int { return cmp.Compare(a.Owner.String(), b.Owner.String()) })
	return out, nil
}

// listWorkloads is one line per workload, with what to do about each.
func listWorkloads(ws []clientWorkload) string {
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "\n  %s: %s", w.Owner, w.Owner.guidance())
	}
	return b.String()
}

// refuseHeld refuses a StatefulSet or DaemonSet among ws, Stratum's own add-ons
// included, that a restart does not reach whole: a rollingUpdate partition leaves
// the ordinals below it, and OnDelete leaves every pod. They stay on the old CA,
// so the run would halt after trust-new, to be finished by hand. Held comes
// from the lists selectClients made. --skip-workload-check is offered for the
// user workloads only: restartClients rolls the add-ons whatever the flags say,
// and its rollout status never completes on an OnDelete one.
func refuseHeld(ws []clientWorkload) error {
	var held []clientWorkload
	var user, addOns bool
	for _, w := range ws {
		if w.Held == "" {
			continue
		}
		held = append(held, w)
		if rollout.Owned(w.Owner.Namespace, w.Owner.Kind, w.Owner.Name) {
			addOns = true
		} else {
			user = true
		}
	}
	if len(held) == 0 {
		return nil
	}
	var b strings.Builder
	for _, w := range held {
		fmt.Fprintf(&b, "\n  %s: %s", w.Owner, w.Held)
	}
	if addOns {
		b.WriteString("\n  Switch the update strategy of Stratum's own add-ons to RollingUpdate and run this again: Stratum restarts them whatever the flags say, and waits for a rollout that OnDelete never completes")
	}
	if user {
		b.WriteString("\n  Set the partition to 0 or the update strategy to RollingUpdate for your own workloads, or pass --skip-workload-check to rotate without this check and restart them yourself")
	}
	return fmt.Errorf("refusing to start: %d workload(s) are not restarted whole by Stratum, so their pods stay on the old CA and the run would halt after trust-new, to be finished by hand:%s", len(held), b.String())
}

// refuseForwardDated refuses a cluster where the kube-root-ca.crt of a
// namespace with a client, Stratum's own add-ons included, carries a
// kube-controller-manager write time in the future. Such a time makes every pod
// of the namespace count as stale however often it restarts, so the run would halt
// after trust-new, to be finished by hand. A time in the past that is wrong cannot be seen from here; staleClients
// still checks after the rollouts.
func refuseForwardDated(ctx context.Context, cs kubernetes.Interface, ws []clientWorkload) error {
	cms, _, err := rootCAConfigMaps(ctx, cs)
	if err != nil {
		return fmt.Errorf("check %s: %w (pass --skip-workload-check to rotate without this check)", rootCAConfigMap, err)
	}
	seen := map[string]bool{}
	for _, w := range ws {
		ns := w.Owner.Namespace
		if cm := cms[ns]; cm != nil && !seen[ns] {
			seen[ns] = true
			if err := errForwardDated(cm, lastRepublish(cm)); err != nil {
				return fmt.Errorf("refusing to start: %w\n  Delete that ConfigMap so kube-controller-manager recreates it, or pass --skip-workload-check to rotate without this check", err)
			}
		}
	}
	return nil
}

// preflightWorkloads is the check before trust-new (--cluster): a halt after
// trust-new is recovered by hand, node by node, or rolled back, so a cluster that
// would need a restart it was not told to do is refused while nothing has changed.
func preflightWorkloads(ctx context.Context, w io.Writer, cs kubernetes.Interface, o CARotateOptions) error {
	if o.SkipWorkloadCheck {
		return nil
	}
	all, err := selectClients(ctx, cs, true)
	if err != nil {
		return fmt.Errorf("find the workloads that use in-cluster API credentials: %w (pass --skip-workload-check to rotate without this check)", err)
	}
	// What the post-rollout gate counts, Stratum's own add-ons included, is
	// refused here when it would halt the run after trust-new: a forward-dated
	// kube-root-ca.crt, and a workload a restart does not reach whole. Stratum
	// restarts its add-ons whatever the flags say, so these do not wait for
	// --restart-workloads.
	if err := refuseForwardDated(ctx, cs, all); err != nil {
		return err
	}
	if err := refuseHeld(all); err != nil {
		return err
	}
	ws := slices.DeleteFunc(slices.Clone(all), func(w clientWorkload) bool { return rollout.Owned(w.Owner.Namespace, w.Owner.Kind, w.Owner.Name) })
	if len(ws) == 0 {
		return nil
	}
	if o.RestartWorkloads {
		fmt.Fprintf(w, "  %d workload(s) use in-cluster API credentials; Stratum restarts the Deployments, DaemonSets and StatefulSets among them once the new CA is republished:%s\n", len(ws), listWorkloads(ws))
		return nil
	}
	return fmt.Errorf("refusing to start: %d workload(s) use in-cluster API credentials, and unless they restart after the new CA is published (trust-new) and before sign-new they fail with x509 errors:%s\n"+
		"  Pass --restart-workloads to have Stratum restart the Deployments, DaemonSets and StatefulSets among them (Jobs and bare Pods are never restarted), "+
		"or --skip-workload-check to rotate without this check and restart them yourself", len(ws), listWorkloads(ws))
}

// restartClients runs between trust-new and sign-new: it waits for the new CA
// to be republished, then rolls the add-ons Stratum ships and, with
// RestartWorkloads, the user workloads that use in-cluster API credentials.
func restartClients(ctx context.Context, w io.Writer, cs kubernetes.Interface, o CARotateOptions, roller rollout.Roller) error {
	ca, err := stagedNewCA()
	if err != nil {
		return err
	}
	if err := waitRootCARepublished(ctx, cs, ca); err != nil {
		return err
	}
	workloads := slices.Clone(rollout.Workloads)
	unrolled := map[owner]bool{} // listed, never rolled: Jobs and bare Pods
	if o.RestartWorkloads && !o.SkipWorkloadCheck {
		ws, err := selectClients(ctx, cs, false)
		if err != nil {
			return fmt.Errorf("find the workloads that use in-cluster API credentials: %w", err)
		}
		for _, cw := range ws {
			if t, ok := cw.Owner.target(); ok {
				workloads = append(workloads, rollout.Workload{Name: cw.Owner.String(), Namespace: cw.Owner.Namespace, Probe: t, Targets: []string{t}})
			} else {
				unrolled[cw.Owner] = true
				fmt.Fprintf(w, "  %s is not restarted (Stratum never restarts a %s); it fails with x509 errors from sign-new until you recreate it\n", cw.Owner, quote(cw.Owner.Kind))
			}
		}
	}
	roll := func() error {
		var failed []error
		for _, r := range roller.RestartOf(workloads) {
			switch {
			case !r.Present:
				fmt.Fprintf(w, "  %s: not on this cluster, skipped\n", r.Name)
			case r.Err != nil || r.Warn != nil:
				fmt.Fprintf(w, "  %s: NOT restarted cleanly\n", r.Name)
				failed = append(failed, r.Err, r.Warn)
			default:
				fmt.Fprintf(w, "  restarted %s\n", r.Name)
			}
		}
		// A rollout that has not finished leaves old pods holding the old CA, and
		// sign-new would cut them off.
		return errors.Join(failed...)
	}
	rolledAt := time.Now()
	if err := roll(); err != nil {
		return err
	}
	if o.SkipWorkloadCheck {
		return nil
	}
	// A rollout can succeed and leave containers on the old CA (a StatefulSet
	// with a rollingUpdate partition does not touch the ordinals below it), and
	// a client can have appeared since the preflight. The check --phase makes,
	// once, before sign-new, apart from the Jobs and bare Pods that were listed
	// and are not Stratum's to restart: counting those would halt a run over
	// workloads the operator was already told about.
	err = waitClientsRestarted(ctx, cs, ca, unrolled)
	// kube-controller-manager can write kube-root-ca.crt again during the
	// restarts, dating every rolled pod before the republish. Roll once more.
	if err != nil && republishedSince(ctx, cs, rolledAt) {
		fmt.Fprintf(w, "  %s was republished after the restarts; restarting them again\n", rootCAConfigMap)
		if err := roll(); err != nil {
			return err
		}
		err = waitClientsRestarted(ctx, cs, ca, unrolled)
	}
	return err
}

// republishedSince reports whether kube-controller-manager wrote any
// namespace's kube-root-ca.crt at or after t, to the second its managedFields
// record.
func republishedSince(ctx context.Context, cs kubernetes.Interface, t time.Time) bool {
	cms, _, err := rootCAConfigMaps(ctx, cs)
	if err != nil {
		return false
	}
	for _, cm := range cms {
		if !lastRepublish(cm).Before(t.Truncate(time.Second)) {
			return true
		}
	}
	return false
}

// waitClientsRestarted makes the final check before sign-new, after the rollouts.
// A rollout is done when the new pods are ready, while the old ones are still
// terminating, and a terminating pod's containers keep running on the old CA
// until its grace period ends, so those are waited for, for as long as the
// rollouts were allowed to take. A stale pod that is not terminating will not go
// by itself (a StatefulSet partition, a client that appeared since the
// preflight), and ends the wait. The budget bounds the API calls as well as the
// polling: the admin client has no request timeout of its own.
func waitClientsRestarted(ctx context.Context, cs kubernetes.Interface, ca *x509.Certificate, exempt map[owner]bool) error {
	wctx, cancel := context.WithTimeout(ctx, clientRolloutWait)
	defer cancel()
	var last []clientWorkload // the stale pods seen at the last look
	// outOfTime is what ends the wait when the budget does, and not the caller.
	outOfTime := func(err error) error {
		if ctx.Err() != nil {
			return err // interrupted, not out of time
		}
		if len(last) == 0 {
			return fmt.Errorf("could not check that no client is left on the old CA within %s of the rollouts: %w", clientRolloutWait, err)
		}
		return fmt.Errorf("%d workload(s) still have pods running on the old CA while they terminate, %s after the rollouts; "+
			"from sign-new they fail with x509 errors:%s\n  They go when their grace period ends; sign-new can run once they have",
			len(last), clientRolloutWait, listStale(last))
	}
	for {
		stale, err := staleClients(wctx, cs, ca, exempt)
		if err != nil {
			if wctx.Err() != nil {
				return outOfTime(err)
			}
			return err
		}
		if len(stale) == 0 {
			return nil
		}
		if !allSettle(stale) {
			return staleError(stale)
		}
		last = stale
		select {
		case <-wctx.Done():
			return outOfTime(wctx.Err())
		case <-time.After(republishPoll):
		}
	}
}

// clientRoller is the roller restartClients uses in a cluster.
func clientRoller() rollout.Roller {
	return rollout.Roller{Kubectl: adminKubectl, Hint: adminKubectlHint, CallTimeout: clientCallTimeout, Wait: clientRolloutWait}
}

// clientRollerWithFence checks ownership at each restart dispatch, after the
// republish wait and between targets. A failed check ends mutation for this run,
// even though Roller continues collecting results for the remaining targets.
func clientRollerWithFence(ctx context.Context, roller rollout.Roller, confirm func(string) error) rollout.Roller {
	kubectl := roller.Kubectl
	var stopped error
	roller.Kubectl = func(callctx context.Context, timeout time.Duration, combined bool, args ...string) ([]byte, error) {
		if len(args) >= 4 && args[2] == "rollout" && args[3] == "restart" {
			if stopped == nil {
				stopped = ctx.Err()
				if stopped == nil {
					stopped = confirm("restarting " + strings.Join(args[4:], " "))
				}
			}
			if stopped != nil {
				return nil, stopped
			}
			callctx = ctx
		}
		return kubectl(callctx, timeout, combined, args...)
	}
	return roller
}

// checkClientsRestarted is the gate before sign-new (--phase): it refuses while
// any container of a workload that uses in-cluster API credentials started
// before its namespace's kube-root-ca.crt carried ca. A container that started
// after reads the new CA with it (a restart in place re-reads ca.crt, which
// kubelet has already written), so one that started before still holds only the
// old CA. It runs on the Stratum-owned workloads too, since a --phase run does
// not roll them. exempt is the workloads not to count (nil for --phase).
func checkClientsRestarted(ctx context.Context, cs kubernetes.Interface, ca *x509.Certificate, exempt map[owner]bool) error {
	stale, err := staleClients(ctx, cs, ca, exempt)
	if err != nil || len(stale) == 0 {
		return err
	}
	return staleError(stale)
}

func staleError(stale []clientWorkload) error {
	return fmt.Errorf("%d workload(s) use in-cluster API credentials and have not restarted since the new CA was published; from sign-new they fail with x509 errors:%s\n"+
		"  Restart them before sign-new, or pass --skip-workload-check to sign-new to proceed anyway", len(stale), listStale(stale))
}

// staleClients is the workloads with a pod that has not restarted since its
// namespace's kube-root-ca.crt carried ca, each with only those pods. Pods that
// are terminating count: their containers are running.
func staleClients(ctx context.Context, cs kubernetes.Interface, ca *x509.Certificate, exempt map[owner]bool) ([]clientWorkload, error) {
	cms, _, err := rootCAConfigMaps(ctx, cs)
	if err != nil {
		return nil, fmt.Errorf("check the new CA is published: %w", err)
	}
	ws, err := selectClients(ctx, cs, true)
	if err != nil {
		return nil, fmt.Errorf("find the workloads that use in-cluster API credentials: %w", err)
	}
	var stale []clientWorkload
	var unpublished []string
	for _, w := range ws {
		if exempt[w.Owner] {
			continue
		}
		cm := cms[w.Owner.Namespace]
		if cm == nil || !carries(cm.Data["ca.crt"], ca) {
			if !slices.Contains(unpublished, w.Owner.Namespace) {
				unpublished = append(unpublished, w.Owner.Namespace)
			}
			continue
		}
		at, err := republishedAt(cm)
		if err != nil {
			return nil, err
		}
		var pods []*corev1.Pod
		var settling map[*corev1.Pod]time.Time
		for _, p := range w.Pods {
			switch {
			case podStartedBefore(p, at):
				pods = append(pods, p)
			case podMayStillStart(p, at, time.Now()):
				pods = append(pods, p)
				if settling == nil {
					settling = map[*corev1.Pod]time.Time{}
				}
				settling[p] = at.Add(projectedCAMargin)
			}
		}
		if len(pods) > 0 {
			stale = append(stale, clientWorkload{Owner: w.Owner, Pods: pods, Settling: settling})
		}
	}
	if len(unpublished) > 0 {
		return nil, fmt.Errorf("%s does not carry the new CA yet in namespace(s) %s: trust-new must be done on every node and kube-controller-manager must have republished it", rootCAConfigMap, strings.Join(unpublished, ", "))
	}
	return stale, nil
}

// allTerminating reports whether every stale pod is being deleted.
func allTerminating(stale []clientWorkload) bool {
	return !slices.ContainsFunc(stale, func(w clientWorkload) bool {
		return slices.ContainsFunc(w.Pods, func(p *corev1.Pod) bool { return p.DeletionTimestamp == nil })
	})
}

// allSettle reports whether every stale pod either is being deleted or has not
// started and is waiting out projectedCAMargin: time ends those, a restart does
// not.
func allSettle(stale []clientWorkload) bool {
	return !slices.ContainsFunc(stale, func(w clientWorkload) bool {
		return slices.ContainsFunc(w.Pods, func(p *corev1.Pod) bool {
			_, settling := w.Settling[p]
			return p.DeletionTimestamp == nil && !settling
		})
	})
}

// listStale is one line per stale workload: what to restart, or, when all of its
// stale pods are terminating, which they are and how long their grace period has
// left, since restarting does not hurry them.
func listStale(stale []clientWorkload) string {
	var b strings.Builder
	for _, w := range stale {
		if !allSettle([]clientWorkload{w}) {
			fmt.Fprintf(&b, "\n  %s: %s", w.Owner, w.Owner.guidance())
			continue
		}
		var pods []string
		for _, p := range w.Pods {
			if until, ok := w.Settling[p]; ok {
				pods = append(pods, fmt.Sprintf("%s (not running yet; it can still start on the old CA until %s)", quote(p.Name), until.UTC().Format(time.RFC3339)))
			} else {
				pods = append(pods, fmt.Sprintf("%s (%s of its grace period left)", quote(p.Name), max(0, time.Until(p.DeletionTimestamp.Time)).Round(time.Second)))
			}
		}
		state := "not settled on the new CA"
		if allTerminating([]clientWorkload{w}) {
			state = "still terminating on the old CA"
		}
		fmt.Fprintf(&b, "\n  %s: %s: %s", w.Owner, state, strings.Join(pods, ", "))
	}
	return b.String()
}

// startedBefore reports whether a container of any of the pods that is running
// may hold a ca.crt from before at. Native sidecars and long-running init
// containers, and ephemeral containers, are not in ContainerStatuses, and hold
// the CA they started with as well.
func startedBefore(pods []*corev1.Pod, at time.Time) bool {
	return slices.ContainsFunc(pods, func(p *corev1.Pod) bool { return podStartedBefore(p, at) })
}

// podMayStillStart is true for a pod that was created at or before at, is not
// being deleted and has a container that is not running (Pending, waiting out a
// CrashLoopBackOff, between restarts), while now is within projectedCAMargin of
// at: kubelet may not have synced the projected ca.crt yet, so a container that
// starts now can read the old one. After the margin it reads the synced file. A
// container that is running is judged by when it started (podStartedBefore).
func podMayStillStart(p *corev1.Pod, at, now time.Time) bool {
	if p.CreationTimestamp.After(at) || now.After(at.Add(projectedCAMargin)) || p.DeletionTimestamp != nil {
		return false
	}
	return slices.ContainsFunc(p.Spec.Containers, func(c corev1.Container) bool {
		return !slices.ContainsFunc(p.Status.ContainerStatuses, func(st corev1.ContainerStatus) bool { return st.Name == c.Name && st.State.Running != nil })
	})
}

// podStartedBefore is true for a container that started before at, or at it
// (StartedAt and the write time of the ConfigMap have one-second precision, so a
// container that started in the second the CA was republished may have read the
// old one). A pod that was created before at also counts for projectedCAMargin
// after it: its ca.crt was projected before the republish, and kubelet may not
// have synced it yet. A pod created after at had the new CA to project.
func podStartedBefore(p *corev1.Pod, at time.Time) bool {
	if !p.CreationTimestamp.After(at) {
		at = at.Add(projectedCAMargin)
	}
	for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses, p.Status.EphemeralContainerStatuses) {
		if r := c.State.Running; r != nil && !r.StartedAt.Time.After(at) {
			return true
		}
	}
	return false
}

// signNewClientGate is checkClientsRestarted from a control plane's own admin
// client and staged CA, unless the caller has already done the check
// (SkipWorkloadCheck: --cluster rolls them, and a node's Job is not gated again).
func signNewClientGate(ctx context.Context, o CARotateOptions) error {
	if o.SkipWorkloadCheck {
		return nil
	}
	ca, err := stagedNewCA()
	if err != nil {
		return err
	}
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return err
	}
	return checkClientsRestarted(ctx, cs, ca, nil)
}
