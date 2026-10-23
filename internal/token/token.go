// Package token implements `stratum token create`: it mints a bootstrap
// token, stores the bootstrap-token Secret, wires the node-bootstrap and
// node-renewal RBAC with upstream bootstrap auto-approval reserved and empty
// (the operator approves joins), publishes the cluster-info ConfigMap (so agents can
// discover the CA), and prints the join command. This replaces the pre-signed
// cert bundle (oss-mkbundle.sh) — the CA private key never leaves the server.
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

// DefaultTokenTTL is the lifetime stratum stamps on a freshly minted bootstrap
// token unless the operator overrides it with --ttl.
// tokencleaner — enabled in the rendered controller-manager argv
// (version.ControllerManagerControllersFlag) — reaps the token Secret once this
// elapses, so a token pasted into a runbook, CI secret or terraform variable
// stops being a working join credential (and a CA-injection key) on its own.
const DefaultTokenTTL = 24 * time.Hour

// bootstrapExpiration renders the RFC3339 `expiration` value for a token minted at
// now with lifetime ttl, and re-parses it with the SAME layout the bootstrap-token
// controller uses. A malformed value therefore fails LOUDLY here at mint time,
// rather than silently producing a token the controller treats as already expired —
// closed by construction (the value comes from a time.Time) and by this
// round-trip check.
//
// A ttl of 0 (or negative) means "never expires": the key is omitted entirely and
// this returns "". That is the opt-in escape hatch for the rare workflow that needs
// a permanent installer token; the CLI default is DefaultTokenTTL.
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

// CACertHash returns the "sha256:<hex>" of the CA's SubjectPublicKeyInfo.
// During a CA rotation pki.ReadCARotation names the staged root as the pin
// anchor (it is present in the published cluster-info bundle), so a token is
// pinned against it — that pin stays valid after finalize, when the staged
// root becomes the live ca.crt.
func CACertHash() (string, error) {
	return CACertHashFor(pki.ReadCARotation().Anchor)
}

// CACertHashFor returns the "sha256:<hex>" SubjectPublicKeyInfo pin of the named
// CA cert basename in version.PKIDir.
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
	// One implementation, shared with the verifier. This is the producer of a wire
	// value clusterinfo.checkPin consumes on the joining node; hashing cert.Raw
	// instead of RawSubjectPublicKeyInfo looks equally plausible and compiles, and
	// a divergence surfaces as "possible MITM" on every join — a security alarm
	// raised by a refactor, where an operator is least able to tell a bug from an
	// attack.
	return clusterinfo.PinFor(cert), nil
}

// RepinCA republishes the cluster-info ConfigMap with the current CA trust
// bundle (ca-bundle.crt), preserving the currently-advertised apiserver endpoint.
// Call after a CA-rotation phase changes the bundle so joiners fetch the right
// trust set (phase trust-new adds the new CA; finalize drops the old one).
func RepinCA() error {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return err
	}
	return RepinCAWith(context.Background(), cs)
}

// RepinCAWith is RepinCA on the caller's client and context, for a caller that
// decides the credential and bounds the requests itself.
func RepinCAWith(ctx context.Context, cs kubernetes.Interface) error {
	cm, err := cs.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cluster-info: %w", err)
	}
	server := ""
	if raw := cm.Data[clusterinfo.KubeConfigKey]; raw != "" {
		// clusterinfo.Parse owns the reading rule, shared with TrustBundle and
		// caFromClusterInfo: a second, differing entry is refused rather than
		// answered by map order, and an unparseable document is diagnosed as
		// such instead of surfacing as a missing server URL.
		s, _, err := clusterinfo.Parse([]byte(raw))
		if err != nil {
			return err
		}
		server = s
	}
	if server == "" {
		return fmt.Errorf("cluster-info has no server URL to preserve")
	}
	// The rotation republishes under its own fence and renews it through its
	// own run, so no per-write recheck here: the rotation engine's phases own
	// the fence, and its resumability machinery pins what each phase applied.
	return ensureClusterInfo(ctx, cs, server, nil)
}

// Create mints a token + sets up everything a node needs to join (bootstrap
// Secret, RBAC, cluster-info), returning the token string "<id>.<secret>", the
// CA-cert hash, and an OwnerReference to the token's Secret. serverURL is the
// apiserver the joiner will reach; ttl is the token lifetime (0 = never expires,
// see bootstrapExpiration). controlPlane mints a control-plane join token, which
// alone carries k8sutil.ControlPlaneBootstrapGroup and so may read stratum-certs.
//
// cs is the client every call rides — the caller decides the credential, not
// this function: the CLI hands the admin client from this node's admin.conf,
// the MCP token_create handler the node connector's. A minting operation the
// front end cannot scope is one a narrowed server credential could be silently
// widened to serve.
//
// The writer leads, so the mint's one print — the control-plane signing wait —
// goes to the caller's stream: the CLI passes os.Stdout byte-identically, the
// MCP handler the buffer its result is built from.
//
// The returned OwnerReference lets the --control-plane path tie the stratum-certs
// Secret to this token, so tokencleaner deleting the expired token garbage-collects
// the shared-cert material too — it must never outlive the tokens that grant read
// access to it. It is nil when the token's UID could not be established.
//
// confirm is the caller's maintenance-lease recheck (nil when the caller holds
// no fence): it runs immediately before the bootstrap-Secret write and inside
// each publication's conflict retry, so a lease lost while an earlier call
// stalled refuses the next mutation instead of publishing this node's trust
// snapshot over the successor's.
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

// bootstrapOwnerRef ties a dependent object's lifetime to a bootstrap token's
// Secret, so tokencleaner deleting the expired token cascades garbage-collection
// to it. Returns nil when uid is empty — never emit an ownerRef the
// garbage collector cannot resolve, which would delete the dependent immediately.
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
	// Malformed is set when the Secret carries a non-empty but unparseable
	// expiration. The bootstrap-token controller treats such a value as
	// invalid/expired, NOT as non-expiring, so it must be reported distinctly from
	// a genuinely non-expiring token rather than shown as "never".
	Malformed bool
}

// List returns every bootstrap-token Secret in kube-system with its expiry, so an
// operator can see which join tokens are live and when they age out.
// Sorted by id for stable output.
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

// ListPage lists one bounded page using the caller's client and cancellation.
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

// tokenInfos is the pure core of List — the classification of raw Secrets into
// TokenInfo — split out so the never/expiring/malformed rules are testable without
// a live apiserver.
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
				// A non-empty value the controller cannot parse is treated by it as
				// invalid/expired — surface that, never as "never".
				info.Malformed = true
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RevokeOnFailure deletes the bootstrap-token Secret for tokenStr because the
// command that minted it is failing, and returns cause — folding in a warning if
// the credential could not be removed.
//
// createToken's own rollback disarms the moment it returns successfully, but the
// COMMAND is not finished then: `stratum token create --control-plane` still has to
// mint a certificate-key and upload the shared certs. Without this the advertised
// guarantee — this command owns what it creates until it succeeds — held for
// token.Create and was merely advertised for the command.
//
// The caller must not have printed the token yet. Revoking a credential already
// on the operator's terminal would trade a stranded credential for output that
// lies, which is worse; cmd/stratum therefore prints nothing until every step has
// succeeded.
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

// revokeTokenBounded runs the rollback under a deadline.
//
// The likely trigger for this rollback is a wedged apiserver — it is what made the
// caller fail — and the rollback then talks to that same wedged apiserver.
// Unbounded, `stratum token create` hangs with no output instead of exiting
// non-zero with the warning this path exists to emit; the operator interrupts, no
// defer completes, and the credential is stranded AND unreported. Exactly the
// shape waitForClusterInfoSignature documents a few functions down.
//
// revokeMintedSecret folds a kubectl-delete warning in on a failed delete, so a
// timeout degrades to the correct actionable message rather than to silence.
//
// Split out from RevokeOnFailure so the deadline is reachable from a test:
// RevokeOnFailure itself needs a live clientset and has no unit coverage.
func revokeTokenBounded(secrets secretAPI, tokenStr string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitForSigningTimeout)
	defer cancel()
	return revokeToken(ctx, secrets, tokenStr, cause)
}

// revokeToken is RevokeOnFailure with the client injected, so the credential
// lifecycle is testable without a cluster.
func revokeToken(ctx context.Context, secrets secretAPI, tokenStr string, cause error) error {
	tok, err := clusterinfo.ParseToken(tokenStr)
	if err != nil {
		// Should be unreachable — the string came from Create. Warn rather than
		// silently skip: a credential nobody can name is still a credential.
		return fmt.Errorf("%w\n\nWARNING: this run created a bootstrap token and could not parse it back "+
			"(%v) to remove it. It stays valid until its expiration (or forever, if minted with --ttl 0); "+
			"find and delete it with\n\n"+
			"  kubectl -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token\n", cause, err)
	}
	return revokeMintedSecret(ctx, secrets, tok.ID(), cause)
}

// tokenSteps are the cluster effects Create performs after minting the
// credential. They are injected so the credential-lifecycle guarantee below can
// be tested without a live apiserver. confirm is the caller's maintenance-lease
// recheck: createToken runs it immediately before the bootstrap-Secret write,
// so a lease lost after the caller took the fence refuses the mint itself.
type tokenSteps struct {
	confirm     func(context.Context) error
	ensureRBAC  func(context.Context) error
	narrowCerts func(context.Context) error
	ensureInfo  func(context.Context) error
	waitForSig  func(ctx context.Context, tokenID string) error
	caCertHash  func() (string, error)
}

// createToken mints the bootstrap Secret and performs the remaining setup,
// guaranteeing that a run which returns an error leaves no credential behind.
//
// That guarantee is not decoration. A minted token is a live join credential
// (and a CA-injection key: the join path trusts a signed cluster-info) the
// moment it is written; it carries a TTL by default, but the window
// before tokencleaner reaps it is still real, and a --ttl 0 token never ages
// out at all. A run that fails after the mint but reports failure must
// therefore not leave one behind in kube-system that the operator was never
// told about. Fumble the edit twice and there are three.
//
// The cleanup is a single deferred block rather than four call sites on purpose.
//
// Four post-mint failure paths can each strand a live credential —
// ensureBootstrapRBAC, ensureClusterInfo, CACertHash and the signing
// post-condition — and the signing one fires on a healthy cluster, not only on
// an apiserver fault. A defer, which covers all four and any future one, is
// the right shape rather than a check at whichever site fails first.
func createToken(ctx context.Context, secrets secretAPI, id, secret string, ttl time.Duration, controlPlane bool, steps tokenSteps) (tokenStr, caHash string, owner *metav1.OwnerReference, err error) {
	// The fence recheck sits immediately before the Secret write: the mint is
	// the publication the fence exists to serialize, and a lease lost between
	// the caller's take and here must refuse the write, not mint under
	// another run's fence.
	if steps.confirm != nil {
		if err := steps.confirm(ctx); err != nil {
			return "", "", nil, fmt.Errorf("the maintenance lease was lost before the mint: %w", err)
		}
	}
	created, uid, mintErr := ensureBootstrapSecret(ctx, secrets, id, secret, ttl, controlPlane)
	// Registered BEFORE the mint error is inspected, because an AMBIGUOUS create —
	// the apiserver committed the write and the client saw an error anyway — is
	// reported as created=true WITH an error. "This command owns what it creates"
	// has to hold when the command does not know whether it created.
	defer func() {
		if err != nil && created {
			// WithoutCancel: if the failure WAS a cancelled context, cleanup on the
			// same ctx would be unreachable exactly when it matters most.
			err = revokeMintedSecret(context.WithoutCancel(ctx), secrets, id, err)
			// Defence in depth for a future return that forgets to zero these; every
			// current failure path already returns empty values explicitly.
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
	// Post-condition: a token whose cluster-info is never signed cannot
	// be used to join, because the joiner hard-fails on a missing signature. Fail
	// here, on the control plane where the remedy is, rather than handing out a
	// token that only fails later on a worker.
	if err := steps.waitForSig(ctx, id); err != nil {
		return "", "", nil, err
	}
	hash, err := steps.caCertHash()
	if err != nil {
		return "", "", nil, err
	}
	return fmt.Sprintf("%s.%s", id, secret), hash, bootstrapOwnerRef(id, uid), nil
}

// revokeMintedSecret deletes the bootstrap Secret this run created and returns
// cause unchanged on success.
//
// If the delete fails it folds a warning into the returned error rather than
// swallowing it: a credential that could not be cleaned up is worse left silent
// than reported, because nothing else will ever remove it.
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

// secretAPI is the slice of the typed client the bootstrap-token lifecycle
// needs. Narrow for the same reason as configMapAPI — see its doc comment.
type secretAPI interface {
	Create(ctx context.Context, s *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// ensureBootstrapSecret mints the bootstrap-token Secret, reporting whether THIS
// call created it so the caller deletes only its own.
//
// AlreadyExists is a hard error rather than an update. The id is 6 random
// characters drawn fresh on every call and never reused, so a collision is a
// ~1-in-2.2e9 accident, not the idempotent re-run that "ensure" usually implies.
// Updating the colliding object with our token-secret would be the worst of
// both: it takes ownership of the credential's contents (and drops any
// expiration the other object carried) while still reporting created=false,
// so the cleanup would skip it — exactly the stranded credential this
// function exists to prevent. Refusing costs one re-run and draws a new id.
//
// ttl sets the token's lifetime: >0 stamps an RFC3339 `expiration` (see
// bootstrapExpiration), 0 omits the key for a non-expiring token. controlPlane
// adds k8sutil.ControlPlaneBootstrapGroup to auth-extra-groups (see
// bootstrapGroups). The created Secret's UID is returned so the caller can tie
// dependent objects to it via an ownerRef.
func ensureBootstrapSecret(ctx context.Context, secrets secretAPI, id, secret string, ttl time.Duration, controlPlane bool) (created bool, uid types.UID, err error) {
	name := bootstrapSecretName(id)
	data := map[string]string{
		"token-id":                       id,
		"token-secret":                   secret,
		"usage-bootstrap-authentication": "true",
		"usage-bootstrap-signing":        "true",
		"auth-extra-groups":              bootstrapGroups(controlPlane),
	}
	// expiration must be valid RFC3339 or the bootstrap-token controller treats the
	// token as already expired; bootstrapExpiration guarantees that or fails here.
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

	// Ambiguous: a Create can be committed by the apiserver and still surface as a
	// client error — a timeout after persistence, a dropped connection on the
	// response. Reporting created=false there would skip the cleanup and leave a
	// valid non-expiring credential behind a command that reported failure, which
	// is the exact defect the deferred revoke exists to prevent, reached through
	// the write path instead of the failure path.
	//
	// Read back and claim it only if it is provably ours: the id is six random
	// characters drawn this call and the secret sixteen more, so a Secret carrying
	// both can have been written by nothing else. A failed or non-matching read
	// leaves created=false — never delete on a guess.
	cur, gerr := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case gerr == nil && string(cur.Data["token-id"]) == id && string(cur.Data["token-secret"]) == secret:
		return true, cur.UID, err
	case gerr == nil || apierrors.IsNotFound(gerr):
		// Proven not ours, or proven absent. Nothing to clean up and nothing to say.
		return false, "", err
	default:
		// The read itself failed, so the create may well have committed and this run
		// cannot tell. Declining to delete is right — never delete on a guess — but
		// staying silent is not: this is the same uncertainty revokeMintedSecret
		// goes out of its way to report, and nothing else will ever remove it.
		return false, "", fmt.Errorf("%w\n\nWARNING: the create may have been committed and this run could not "+
			"read it back (%v), so bootstrap token %q may exist as a valid join credential.\n"+
			"Check and remove it by hand:\n\n  kubectl -n kube-system get secret %s\n", err, gerr, id, name)
	}
}

// bootstrapGroups is a token's auth-extra-groups. Every token carries
// k8sutil.BootstrapGroup, which the CSR bindings name, so a control-plane join
// still bootstraps its kubelet; only a control-plane token adds
// k8sutil.ControlPlaneBootstrapGroup, the one group allowed to read
// stratum-certs.
func bootstrapGroups(controlPlane bool) string {
	if controlPlane {
		return k8sutil.BootstrapGroup + "," + k8sutil.ControlPlaneBootstrapGroup
	}
	return k8sutil.BootstrapGroup
}

// ensureBootstrapRBAC reserves upstream node-client auto-approval's binding,
// with no subjects, before publishing a join token, whether or not this
// controller runs the operator, and grants the bootstrap and renewal bindings.
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

// configMapAPI is the slice of the typed client the cluster-info writers need.
//
// It is deliberately this narrow rather than kubernetes.Interface: the fake
// clientset would pull gopkg.in/evanphx/json-patch.v4 into the module graph, and
// even test-only modules move govulncheck's require-set count. A three-method interface is also
// a more honest statement of what these functions touch.
// roleAPI is the slice of the typed Role client ensureClusterInfoRole needs.
type roleAPI interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*rbacv1.Role, error)
	Create(ctx context.Context, r *rbacv1.Role, opts metav1.CreateOptions) (*rbacv1.Role, error)
	Update(ctx context.Context, r *rbacv1.Role, opts metav1.UpdateOptions) (*rbacv1.Role, error)
}

type configMapAPI interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error)
	Create(ctx context.Context, cm *corev1.ConfigMap, opts metav1.CreateOptions) (*corev1.ConfigMap, error)
	Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

// putClusterInfoKubeconfig writes data.kubeconfig into the cluster-info
// ConfigMap, PRESERVING every other key.
//
// It is a read-modify-write rather than an object replace because the object is
// shared: the bootstrapsigner stores its detached JWS signatures in this same
// Data map as jws-kubeconfig-<token-id>. Replacing the map wholesale
// silently discards anything else living in it.
//
// Signatures are the deliberate exception. They cover the kubeconfig CONTENT, so
// on a real change they must be dropped rather than preserved: a joiner that
// fetches a stale signature gets a MAC mismatch, which is the MITM signal and is
// never retried, so the join aborts outright. Dropping them degrades to
// NoSignatureError, which the joiner retries until the signer catches up. That
// distinction matters because RepinCA routes through here and runs twice per CA
// rotation (--phase=trust-new and --phase=finalize), each time just after
// restartControlPlane() has bounced the controller-manager with no readiness
// gate.
//
// An unchanged kubeconfig is left completely alone — no Update, so no needless
// signature churn on the common `stratum token create` path.
//
// Conflicts are expected: the signer updates this object concurrently. Retrying
// on conflict is the documented client-go pattern for a shared object.
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
				// Raced with another writer; RetryOnConflict only retries on
				// Conflict, so convert this into one.
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
		// The existing signatures cover the OLD kubeconfig. Carrying them across a
		// content change would hand a joiner a signature that does not verify —
		// which DiscoverCA treats as the MITM signal and refuses to retry, so the
		// join aborts. Deleting them yields NoSignatureError instead, which the
		// joiner waits out until the signer republishes. Fail soft, not loud, for
		// a condition we caused ourselves. Every other key is still preserved.
		for k := range cur.Data {
			if strings.HasPrefix(k, clusterinfo.JWSSignatureKeyPrefix) {
				delete(cur.Data, k)
			}
		}
		// A successful read can return the successor's current version after
		// a long stall. Confirm after that read and on every conflict retry.
		if confirm != nil {
			if err := confirm(ctx); err != nil {
				return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
			}
		}
		_, err = cms.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// waitForSigningInterval and waitForSigningTimeout bound the post-condition
// check below. The bootstrapsigner is informer-driven and normally signs
// sub-second; the budget covers an apiserver or controller-manager still coming
// up, not a cluster that will never sign.
// Vars rather than consts only so tests can shorten them; production never
// reassigns them. Unexported, matching the identical pair in internal/bootstrap.
var (
	waitForSigningInterval = time.Second
	waitForSigningTimeout  = 30 * time.Second
)

// unsignedControlPlaneRemedy is printed when this control plane does not sign
// cluster-info. This is the courtesy half of the hard-fail: it catches the
// operator on the control plane, where the remedy is, instead of on a worker.
//
// It does NOT catch every case, and that is stated rather than papered over:
// Create runs this only at mint time, so an operator re-using a still-valid token
// (within its TTL, or a --ttl 0 token) minted before the upgrade never runs this
// code. The joiner-side message in internal/bootstrap is the actual control.
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

// waitForClusterInfoSignature blocks until the bootstrapsigner has published a
// signature for this token id, or the budget runs out. Its waiting progress
// goes through the writer it is handed (after its context), so the CLI passes
// os.Stdout and the MCP front end a buffer.
func waitForClusterInfoSignature(ctx context.Context, w io.Writer, cms configMapAPI, tokenID string) error {
	// Bound the whole operation, INCLUDING each API call. This context is the
	// entire fix for the hang: the wall-clock deadline below is only ever tested
	// AFTER Get returns, so on its own it bounds the LOOP, not the call, and a
	// stalled apiserver blocks inside Get forever with `stratum token create`
	// hanging and the 30s hard-fail never firing. The joiner side has no
	// equivalent gap — its http.Client carries its own Timeout.
	//
	// A caller-supplied cancellation is honoured too: Get returns early and the
	// select at the bottom of the loop surfaces ctx.Err().
	// One budget, two deadlines — so they must be ORDERED. Giving the context the
	// same duration as the wall clock lets it expire a hair first, and then the
	// loop's final select can take ctx.Done() and return a bare
	// context.DeadlineExceeded, throwing away the remediation this function exists
	// to print. The extra interval makes the wall-clock hard-fail win in the
	// ordinary case, while still bounding a hung Get (which is the whole point of
	// the context). It is not a guarantee: a goroutine stalled for longer than an
	// interval between the deadline check and the wait finds the context already
	// done. So the wait tells the two apart: the budget running out ends in the
	// same error as the wall clock, and only the caller's own cancellation is
	// returned as such.
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
			// A clean read that is merely unsigned clears any earlier API failure.
			// Without this, one transient blip inside the budget permanently
			// suppresses the remediation below — and a single apiserver hiccup is
			// unremarkable on exactly the freshly-upgraded cluster that needs it.
			lastErr = nil
		}
		// The wall-clock check catches a hung Get too, because ctx carries the
		// SAME budget: a Get that blocks until the context expires returns at the
		// deadline, and this test fires immediately after.
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

// signingDeadlineError is what waitForClusterInfoSignature returns when its
// budget runs out. Blaming the signer for what was actually an API failure
// would send the operator to restart components that were never the problem,
// so the two causes get different messages.
func signingDeadlineError(tokenID string, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("could not read cluster-info to confirm it was signed for token id %q within %s: %w",
			tokenID, waitForSigningTimeout, lastErr)
	}
	return fmt.Errorf("cluster-info was not signed for token id %q within %s\n%s",
		tokenID, waitForSigningTimeout, unsignedControlPlaneRemedy)
}

// ensureClusterInfo publishes the cluster-info ConfigMap in
// kube-public (CA + server, no creds) and the RBAC to read it anonymously.
// ensureClusterInfo publishes the trust a joiner reads before it holds any
// credential. confirm is the holder's maintenance-lease recheck (nil when the
// caller holds no fence); it runs immediately before each ConfigMap write,
// inside the conflict retry, so a lease lost while an earlier call stalled
// refuses the next publication instead of overwriting the successor's trust
// with this node's snapshot.
func ensureClusterInfo(ctx context.Context, cs kubernetes.Interface, serverURL string, confirm func(context.Context) error) error {
	// Fail fast when the fence is already gone: the publication reads local
	// PKI first, and a refusal before that read is cheaper than one after it.
	if confirm != nil {
		if err := confirm(ctx); err != nil {
			return fmt.Errorf("the maintenance lease was lost before this publication: %w", err)
		}
	}
	// Publish the trust bundle (old‖new during a rotation) so joiners trust every
	// CA the cluster currently accepts; pki's reader falls back to ca.crt
	// pre-bundle, the same rule kubeconfig.Write embeds.
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
	// allow anonymous read of cluster-info so a joining node can read the CA before it holds any credential
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

// clusterInfoRoleRules grant anonymous get on the ConfigMaps a joining node reads.
var clusterInfoRoleRules = []rbacv1.PolicyRule{{
	APIGroups: []string{""}, Resources: []string{"configmaps"},
	ResourceNames: []string{"cluster-info", clusterinfo.KubeletClientCAConfigMap}, Verbs: []string{"get"},
}}

// ensureClusterInfoRole creates or updates stratum:cluster-info to clusterInfoRoleRules.
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

// UnpublishKubeletClientCA removes the published kubelet client-CA bundle, so
// a joining worker's kubelet follows the cluster root again.
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

// publishKubeletClientCA publishes this node's kubelet client-CA bundle in
// kube-public when its kubelet trusts a kubelet-client CA.
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
