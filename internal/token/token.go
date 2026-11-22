// Package token implements `stratum token create`: minting, RBAC and cluster-info publication.
package token

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/retry"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func randStr(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		b[i] = alpha[x.Int64()]
	}
	return string(b)
}

// Lifetime of a fresh bootstrap token; tokencleaner reaps the Secret once it elapses.
const DefaultTokenTTL = 24 * time.Hour

// Renders and re-parses with the controller's layout; ttl<=0 omits the key: never expires.
func bootstrapExpiration(now time.Time, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", nil
	}
	s := now.Add(ttl).UTC().Format(time.RFC3339)
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return "", fmt.Errorf("computed bootstrap-token expiration %q is not valid RFC3339: %w", s, err)
	}
	return s, nil
}

// Pinned against the staged root during a CA rotation, so pins survive finalize.
func CACertHash() (string, error) {
	return CACertHashFor(pki.ReadCARotation().Anchor)
}

func CACertHashFor(name string) (string, error) {
	pemBytes, err := os.ReadFile(filepath.Join(version.PKIDir, name+".crt"))
	if err != nil {
		return "", err
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return "", fmt.Errorf("token: bad %s.crt PEM", name)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return "", err
	}
	// PinFor is the one producer of the wire value the joiner's verifier recomputes.
	return clusterinfo.PinFor(cert), nil
}

// Republishes cluster-info with the current CA bundle, preserving the apiserver endpoint.
func RepinCA() error {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return err
	}
	return RepinCAWith(context.Background(), cs)
}

func RepinCAWith(ctx context.Context, cs kubernetes.Interface) error {
	cm, err := cs.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cluster-info: %w", err)
	}
	server := ""
	if raw := cm.Data[clusterinfo.KubeConfigKey]; raw != "" {
		// clusterinfo.Parse owns the reading rule, shared with the join path.
		s, _, err := clusterinfo.Parse([]byte(raw))
		if err != nil {
			return err
		}
		server = s
	}
	if server == "" {
		return fmt.Errorf("cluster-info has no server URL to preserve")
	}
	// The rotation engine owns the fence here; no per-write recheck.
	return ensureClusterInfo(ctx, cs, server, nil)
}

// Mints a token and all join setup; confirm is the caller's lease recheck; ttl<=0 never expires.
func Create(w io.Writer, cs kubernetes.Interface, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
	ctx := context.Background()
	id, secret := randStr(6), randStr(16)
	return createToken(ctx, cs.CoreV1().Secrets("kube-system"), id, secret, ttl, controlPlane, tokenSteps{
		confirm: confirm,
		ensureRBAC: func(ctx context.Context) error {
			return ensureBootstrapRBAC(ctx, cs.RbacV1().ClusterRoleBindings())
		},
		narrowCerts: func(ctx context.Context) error { return ha.NarrowCertsReaderBinding(ctx, cs) },
		ensureInfo:  func(ctx context.Context) error { return ensureClusterInfo(ctx, cs, serverURL, confirm) },
		waitForSig: func(ctx context.Context, tokenID string) error {
			return waitForClusterInfoSignature(ctx, w, cs.CoreV1().ConfigMaps("kube-public"), tokenID)
		},
		caCertHash: CACertHash,
	})
}

// Nil for an empty uid: an ownerRef the GC cannot resolve deletes the dependent at once.
func bootstrapOwnerRef(id string, uid types.UID) *metav1.OwnerReference {
	if uid == "" {
		return nil
	}
	return &metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Secret",
		Name:       bootstrapSecretName(id),
		UID:        uid,
	}
}

// TokenInfo is one bootstrap token's identity and lifetime, for `stratum token list`.
type TokenInfo struct {
	ID      string
	Expires *time.Time // nil = never expires (unless Malformed)
	// Non-empty but unparseable expiration; the controller treats it as expired, not never.
	Malformed bool
}

// Returns every bootstrap-token Secret with its expiry, sorted by id for stable output.
func List() ([]TokenInfo, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	secs, err := cs.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{
		FieldSelector: "type=" + string(corev1.SecretTypeBootstrapToken),
	})
	if err != nil {
		return nil, err
	}
	return tokenInfos(secs.Items), nil
}

// Only token IDs and expiry metadata are returned; secret halves stay internal.
func ListPage(ctx context.Context, cs kubernetes.Interface, limit int64, cont string) ([]TokenInfo, string, error) {
	secs, err := cs.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{
		FieldSelector: "type=" + string(corev1.SecretTypeBootstrapToken), Limit: limit, Continue: cont,
	})
	if err != nil {
		return nil, "", err
	}
	return tokenInfos(secs.Items), secs.Continue, nil
}

// Split from List so the never/expiring/malformed rules are testable without an apiserver.
func tokenInfos(items []corev1.Secret) []TokenInfo {
	out := make([]TokenInfo, 0, len(items))
	for i := range items {
		s := &items[i]
		id := string(s.Data["token-id"])
		if id == "" {
			continue
		}
		info := TokenInfo{ID: id}
		if raw := string(s.Data["expiration"]); raw != "" {
			if t, perr := time.Parse(time.RFC3339, raw); perr == nil {
				info.Expires = &t
			} else {
				// The controller treats an unparseable value as expired; surface that, never "never".
				info.Malformed = true
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Deletes the token Secret on a failing command; the caller must not have printed it yet.
func RevokeOnFailure(tokenStr string, cause error) error {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return fmt.Errorf("%w\n\nWARNING: could not reach the apiserver to remove the bootstrap token this "+
			"run created (%v). It stays valid until its expiration (or forever, if minted with --ttl 0); "+
			"find and delete it with\n\n"+
			"  kubectl -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token\n", cause, err)
	}
	return revokeTokenBounded(cs.CoreV1().Secrets("kube-system"), tokenStr, cause)
}

// Bounded: the likely trigger is a wedged apiserver, and the rollback talks to it again.
func revokeTokenBounded(secrets secretAPI, tokenStr string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitForSigningTimeout)
	defer cancel()
	return revokeToken(ctx, secrets, tokenStr, cause)
}

// RevokeOnFailure with the client injected, testable without a cluster.
func revokeToken(ctx context.Context, secrets secretAPI, tokenStr string, cause error) error {
	tok, err := clusterinfo.ParseToken(tokenStr)
	if err != nil {
		// Should be unreachable; a credential nobody can name is still a credential.
		return fmt.Errorf("%w\n\nWARNING: this run created a bootstrap token and could not parse it back "+
			"(%v) to remove it. It stays valid until its expiration (or forever, if minted with --ttl 0); "+
			"find and delete it with\n\n"+
			"  kubectl -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token\n", cause, err)
	}
	return revokeMintedSecret(ctx, secrets, tok.ID(), cause)
}

// Injected so the credential-lifecycle guarantee is testable without a live apiserver.
type tokenSteps struct {
	confirm     func(context.Context) error
	ensureRBAC  func(context.Context) error
	narrowCerts func(context.Context) error
	ensureInfo  func(context.Context) error
	waitForSig  func(ctx context.Context, tokenID string) error
	caCertHash  func() (string, error)
}

// Guarantees that a run returning an error leaves no credential behind.
func createToken(ctx context.Context, secrets secretAPI, id, secret string, ttl time.Duration, controlPlane bool, steps tokenSteps) (tokenStr, caHash string, owner *metav1.OwnerReference, err error) {
	// A lease lost since the caller's take must refuse the mint, not publish under another fence.
	if steps.confirm != nil {
		if err := steps.confirm(ctx); err != nil {
			return "", "", nil, fmt.Errorf("the maintenance lease was lost before the mint: %w", err)
		}
	}
	created, uid, mintErr := ensureBootstrapSecret(ctx, secrets, id, secret, ttl, controlPlane)
	// An ambiguous create reports created=true with an error; the defer must cover it.
	defer func() {
		if err != nil && created {
			// WithoutCancel: cleanup on a cancelled ctx would be unreachable exactly when it matters.
			err = revokeMintedSecret(context.WithoutCancel(ctx), secrets, id, err)
			// Defence in depth for a future return that forgets to zero these.
			tokenStr, caHash, owner = "", "", nil
		}
	}()
	if mintErr != nil {
		return "", "", nil, fmt.Errorf("bootstrap secret: %w", mintErr)
	}

	if err := steps.ensureRBAC(ctx); err != nil {
		return "", "", nil, fmt.Errorf("bootstrap RBAC: %w", err)
	}
	if err := steps.narrowCerts(ctx); err != nil {
		return "", "", nil, fmt.Errorf("narrow the stratum-certs read grant: %w", err)
	}
	if err := steps.ensureInfo(ctx); err != nil {
		return "", "", nil, fmt.Errorf("cluster-info: %w", err)
	}
	// Fail here, on the control plane, rather than handing out a token that only fails on a worker.
	if err := steps.waitForSig(ctx, id); err != nil {
		return "", "", nil, err
	}
	hash, err := steps.caCertHash()
	if err != nil {
		return "", "", nil, err
	}
	return fmt.Sprintf("%s.%s", id, secret), hash, bootstrapOwnerRef(id, uid), nil
}

// A failed delete folds a warning into the error; nothing else will ever remove the credential.
func revokeMintedSecret(ctx context.Context, secrets secretAPI, id string, cause error) error {
	name := bootstrapSecretName(id)
	derr := secrets.Delete(ctx, name, metav1.DeleteOptions{})
	if derr == nil || apierrors.IsNotFound(derr) {
		return cause
	}
	return fmt.Errorf("%w\n\nWARNING: this run created bootstrap token %q and could not remove it again (%v).\n"+
		"It remains a valid credential for joining this cluster until its expiration (or forever, if minted\n"+
		"with --ttl 0) unless you delete it by hand:\n\n  kubectl -n kube-system delete secret %s\n", cause, id, derr, name)
}

func bootstrapSecretName(id string) string { return "bootstrap-token-" + id }

// Narrow for the same reason as configMapAPI.
type secretAPI interface {
	Create(ctx context.Context, s *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// AlreadyExists is a hard error: a random-id collision is not an idempotent re-run.
func ensureBootstrapSecret(ctx context.Context, secrets secretAPI, id, secret string, ttl time.Duration, controlPlane bool) (created bool, uid types.UID, err error) {
	name := bootstrapSecretName(id)
	data := map[string]string{
		"token-id":                       id,
		"token-secret":                   secret,
		"usage-bootstrap-authentication": "true",
		"usage-bootstrap-signing":        "true",
		"auth-extra-groups":              bootstrapGroups(controlPlane),
	}
	// The controller treats a non-RFC3339 expiration as already expired.
	exp, err := bootstrapExpiration(time.Now(), ttl)
	if err != nil {
		return false, "", err
	}
	if exp != "" {
		data["expiration"] = exp
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Type:       corev1.SecretTypeBootstrapToken,
		StringData: data,
	}
	out, err := secrets.Create(ctx, sec, metav1.CreateOptions{})
	switch {
	case err == nil:
		return true, out.UID, nil
	case apierrors.IsAlreadyExists(err):
		return false, "", fmt.Errorf("%s already exists — a random-id collision; re-run to draw a fresh id", name)
	}

	// Read back and claim it only if both halves match; never delete on a guess.
	cur, gerr := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case gerr == nil && string(cur.Data["token-id"]) == id && string(cur.Data["token-secret"]) == secret:
		return true, cur.UID, err
	case gerr == nil || apierrors.IsNotFound(gerr):
		// Proven not ours, or proven absent. Nothing to clean up and nothing to say.
		return false, "", err
	default:
		// The create may have committed; silence would strand an unreported credential.
		return false, "", fmt.Errorf("%w\n\nWARNING: the create may have been committed and this run could not "+
			"read it back (%v), so bootstrap token %q may exist as a valid join credential.\n"+
			"Check and remove it by hand:\n\n  kubectl -n kube-system get secret %s\n", err, gerr, id, name)
	}
}

// Only a control-plane token adds the group allowed to read stratum-certs.
func bootstrapGroups(controlPlane bool) string {
	if controlPlane {
		return k8sutil.BootstrapGroup + "," + k8sutil.ControlPlaneBootstrapGroup
	}
	return k8sutil.BootstrapGroup
}

// Reserves upstream auto-approval's binding and grants the bootstrap and renewal bindings.
func ensureBootstrapRBAC(ctx context.Context, crbs k8sutil.NodeClientBindingAPI) error {
	if err := k8sutil.DisableNodeClientAutoApprove(ctx, crbs); err != nil {
		return err
	}
	want := []*rbacv1.ClusterRoleBinding{
		// bootstrappers may create CSRs
		crb("stratum-node-bootstrapper", "system:node-bootstrapper",
			rbacv1.Subject{Kind: "Group", Name: k8sutil.BootstrapGroup, APIGroup: rbacv1.GroupName}),
		// auto-renew node-client certs for already-joined nodes
		crb("stratum-csr-renewal", "system:certificates.k8s.io:certificatesigningrequests:selfnodeclient",
			rbacv1.Subject{Kind: "Group", Name: "system:nodes", APIGroup: rbacv1.GroupName}),
	}
	for _, b := range want {
		_, err := crbs.Create(ctx, b, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func crb(name, clusterRole string, subj rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
		Subjects:   []rbacv1.Subject{subj},
	}
}

// roleAPI is the slice of the typed Role client ensureClusterInfoRole needs.
type roleAPI interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*rbacv1.Role, error)
	Create(ctx context.Context, r *rbacv1.Role, opts metav1.CreateOptions) (*rbacv1.Role, error)
	Update(ctx context.Context, r *rbacv1.Role, opts metav1.UpdateOptions) (*rbacv1.Role, error)
}

// Deliberately narrow: the fake clientset would add a module to the require set.
type configMapAPI interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error)
	Create(ctx context.Context, cm *corev1.ConfigMap, opts metav1.CreateOptions) (*corev1.ConfigMap, error)
	Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

// Read-modify-write because the signer stores signatures in this same Data map.
func putClusterInfoKubeconfig(ctx context.Context, cms configMapAPI, kubeconfig string, confirm func(context.Context) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := cms.Get(ctx, "cluster-info", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if confirm != nil {
				if err := confirm(ctx); err != nil {
					return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
				}
			}
			_, err = cms.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
				Data:       map[string]string{clusterinfo.KubeConfigKey: kubeconfig},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				// RetryOnConflict only retries Conflict, so convert the create race into one.
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "configmaps"}, "cluster-info", err)
			}
			return err
		}
		if err != nil {
			return fmt.Errorf("read cluster-info: %w", err)
		}
		if cur.Data == nil {
			cur.Data = map[string]string{}
		}
		if cur.Data[clusterinfo.KubeConfigKey] == kubeconfig {
			return nil // unchanged: do not churn the object and invalidate signatures
		}
		cur.Data[clusterinfo.KubeConfigKey] = kubeconfig
		// Stale signatures read as MITM; dropping them degrades to a retriable no-signature error.
		for k := range cur.Data {
			if strings.HasPrefix(k, clusterinfo.JWSSignatureKeyPrefix) {
				delete(cur.Data, k)
			}
		}
		// A stalled read can return the successor's version; recheck after it and on retries.
		if confirm != nil {
			if err := confirm(ctx); err != nil {
				return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
			}
		}
		_, err = cms.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// Vars so tests can shorten them; the budget covers a control plane still coming up.
var (
	waitForSigningInterval = time.Second
	waitForSigningTimeout  = 30 * time.Second
)

// Mint-time only: a re-used valid token never sees it; the joiner-side check controls.
var unsignedControlPlaneRemedy = `
This control plane is not signing the cluster-info ConfigMap, so any node using
this token would refuse to join.

kube-controller-manager runs the bootstrapsigner only when it is asked to; it is
disabled by default. There is nothing to hand-edit: argv is rendered on every
start, so a node running a build that renders the controllers flag picks the
flag up the next time the component starts. On EVERY control-plane node:

  1. confirm the node is running a stratum build that includes the flag
  2. restart the controller-manager, so its argv is re-rendered
  3. verify - this must print the flag:

       grep -o -- '--controllers=[^ ]*' ` + version.ControllerManagerManifest + `

     expected:

       ` + version.ControllerManagerControllersFlag + `

Every control plane, not just this one: kube-controller-manager leader-elects, so
the signer runs on whichever node holds the lease. Then re-run this command.`

// Progress goes through the writer it is handed, so front ends control the stream.
func waitForClusterInfoSignature(ctx context.Context, w io.Writer, cms configMapAPI, tokenID string) error {
	// The context bounds each Get too; the extra interval makes the wall-clock remediation win.
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, waitForSigningTimeout+waitForSigningInterval)
	defer cancel()

	key := clusterinfo.JWSSignatureKeyPrefix + tokenID
	deadline := time.Now().Add(waitForSigningTimeout)
	var lastErr error
	for attempt := 1; ; attempt++ {
		cm, err := cms.Get(ctx, "cluster-info", metav1.GetOptions{})
		switch {
		case err != nil:
			lastErr = err
		case cm.Data[key] != "":
			return nil
		default:
			// One clean read clears lastErr, so a transient blip cannot suppress the remediation.
			lastErr = nil
		}
		// ctx carries the same budget, so a hung Get returns at the deadline and this fires.
		if time.Now().After(deadline.Add(-waitForSigningInterval)) {
			return signingDeadlineError(tokenID, lastErr)
		}
		fmt.Fprintf(w, "  waiting for the control plane to sign cluster-info (attempt %d)...\n", attempt)
		select {
		case <-ctx.Done():
			if err := caller.Err(); err != nil {
				return err
			}
			return signingDeadlineError(tokenID, lastErr)
		case <-time.After(waitForSigningInterval):
		}
	}
}

// API failures and an unsigned cluster get different messages; blaming the signer misleads.
func signingDeadlineError(tokenID string, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("could not read cluster-info to confirm it was signed for token id %q within %s: %w",
			tokenID, waitForSigningTimeout, lastErr)
	}
	return fmt.Errorf("cluster-info was not signed for token id %q within %s\n%s",
		tokenID, waitForSigningTimeout, unsignedControlPlaneRemedy)
}

// Publishes the trust a joiner reads before it holds any credential.
func ensureClusterInfo(ctx context.Context, cs kubernetes.Interface, serverURL string, confirm func(context.Context) error) error {
	// Fail before the local PKI read; a refusal before it is cheaper.
	if confirm != nil {
		if err := confirm(ctx); err != nil {
			return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
		}
	}
	// The bundle covers every CA the cluster currently accepts during a rotation.
	ca, _, err := pki.ReadRootTrust()
	if err != nil {
		return err
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters[""] = &clientcmdapi.Cluster{Server: serverURL, CertificateAuthorityData: ca}
	split, err := pki.KubeletClientSplit()
	if err != nil {
		return fmt.Errorf("tell whether kubelet client trust is split: %w", err)
	}
	mode := clusterinfo.KubeletClientCAModeLegacy
	if split {
		mode = clusterinfo.KubeletClientCAModeSplit
	}
	kc.Extensions[clusterinfo.KubeletClientCAModeExtension] = &runtime.Unknown{Raw: []byte(`{"mode":"` + mode + `"}`)}
	raw, err := clientcmd.Write(*kc)
	if err != nil {
		return err
	}
	if err := publishKubeletClientCA(ctx, cs.CoreV1().ConfigMaps("kube-public"), confirm); err != nil {
		return err
	}
	if err := putClusterInfoKubeconfig(ctx, cs.CoreV1().ConfigMaps("kube-public"), string(raw), confirm); err != nil {
		return err
	}
	// Anonymous read lets a joiner fetch the CA before it holds a credential.
	if err := ensureClusterInfoRole(ctx, cs.RbacV1().Roles("kube-public")); err != nil {
		return err
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum:cluster-info", Namespace: "kube-public"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "stratum:cluster-info"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "system:anonymous", APIGroup: rbacv1.GroupName}},
	}
	_, err = cs.RbacV1().RoleBindings("kube-public").Create(ctx, rb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

var clusterInfoRoleRules = []rbacv1.PolicyRule{{
	APIGroups: []string{""}, Resources: []string{"configmaps"},
	ResourceNames: []string{"cluster-info", clusterinfo.KubeletClientCAConfigMap}, Verbs: []string{"get"},
}}

func ensureClusterInfoRole(ctx context.Context, roles roleAPI) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := roles.Get(ctx, "stratum:cluster-info", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = roles.Create(ctx, &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: "stratum:cluster-info", Namespace: "kube-public"},
				Rules:      clusterInfoRoleRules,
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, "stratum:cluster-info", err)
			}
			return err
		}
		if err != nil {
			return err
		}
		if reflect.DeepEqual(cur.Rules, clusterInfoRoleRules) {
			return nil
		}
		cur = cur.DeepCopy()
		cur.Rules = clusterInfoRoleRules
		_, err = roles.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// Removing the published bundle makes a joining worker's kubelet follow the cluster root.
func UnpublishKubeletClientCA() error {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return err
	}
	err = cs.CoreV1().ConfigMaps("kube-public").Delete(context.Background(), clusterinfo.KubeletClientCAConfigMap, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Publishes the kubelet client-CA bundle in kube-public when kubelets trust one.
func publishKubeletClientCA(ctx context.Context, cms configMapAPI, confirm func(context.Context) error) error {
	inUse, err := pki.KubeletClientCAInUse()
	if err != nil {
		return fmt.Errorf("tell whether the kubelets trust a kubelet-client CA: %w", err)
	}
	if !inUse {
		return nil
	}
	bundle, err := os.ReadFile(pki.KubeletClientBundlePath())
	if err != nil {
		return err
	}
	want := map[string]string{clusterinfo.KubeletClientCAKey: string(bundle)}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := cms.Get(ctx, clusterinfo.KubeletClientCAConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if confirm != nil {
				if err := confirm(ctx); err != nil {
					return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
				}
			}
			_, err = cms.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: clusterinfo.KubeletClientCAConfigMap, Namespace: "kube-public"},
				Data:       want,
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, clusterinfo.KubeletClientCAConfigMap, err)
			}
			return err
		}
		if err != nil {
			return err
		}
		if reflect.DeepEqual(cur.Data, want) {
			return nil
		}
		cur = cur.DeepCopy()
		cur.Data = want
		if confirm != nil {
			if err := confirm(ctx); err != nil {
				return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
			}
		}
		_, err = cms.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}
