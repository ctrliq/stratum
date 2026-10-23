// Package clusterinfo authenticates the kube-public cluster-info ConfigMap and
// turns the authenticated payload into a CA trust bundle.
//
// It exists as its own package for one reason: the trust-bundle helper must be
// reachable ONLY from the authenticated path. Inside a single
// package that is a convention a future caller can quietly break — an unexported
// marker type is still constructible by any code in the same package. Across a
// package boundary the compiler enforces it: Authenticated carries only
// unexported fields, so no outside package can build a populated one, and the
// only thing that returns a populated one is Authenticate.
//
// The wire format is the Kubernetes cluster-info format: a detached JWS (RFC 7515 Appendix F) with an
// HMAC-SHA256 MAC keyed by the bootstrap token secret, published by
// kube-controller-manager's bootstrapsigner as data["jws-kubeconfig-<token-id>"]
// alongside data["kubeconfig"]. See the golden vector in the tests.
package clusterinfo

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// KubeConfigKey and JWSSignatureKeyPrefix mirror k8s.io/cluster-bootstrap's
	// token/api constants. They are restated rather than imported: taking that
	// module for two strings and ~15 lines of signer would add 19 modules to the
	// require set and a permanently-unfixed advisory (GO-2024-2631) via
	// gopkg.in/square/go-jose.v2.
	KubeConfigKey         = "kubeconfig"
	JWSSignatureKeyPrefix = "jws-kubeconfig-"
)

// KubeletClientCAConfigMap is the kube-public ConfigMap holding a split
// cluster's kubelet client-CA bundle under KubeletClientCAKey.
const (
	KubeletClientCAConfigMap = "stratum-kubelet-client-ca"
	KubeletClientCAKey       = "ca.crt"
	// KubeletClientCAModeExtension is covered by cluster-info's detached JWS.
	// An absent extension identifies cluster-info from a release before the split.
	KubeletClientCAModeExtension = "stratum.ciq.com/kubelet-client-ca"
	KubeletClientCAModeLegacy    = "legacy"
	KubeletClientCAModeSplit     = "split"
)

// KubeletClientCASplit reports the mode carried in the token-authenticated
// kubeconfig. A malformed or unknown mode is never treated as legacy.
func (a Authenticated) KubeletClientCASplit() (bool, error) {
	if a.kubeconfig == "" {
		return false, fmt.Errorf("clusterinfo: kubelet client-CA mode requested from an unauthenticated value")
	}
	cfg, err := clientcmd.Load([]byte(a.kubeconfig))
	if err != nil {
		return false, fmt.Errorf("parse cluster-info kubeconfig: %w", err)
	}
	obj, ok := cfg.Extensions[KubeletClientCAModeExtension]
	if !ok {
		return false, nil // cluster-info published before this mode existed
	}
	u, ok := obj.(*runtime.Unknown)
	if !ok {
		return false, fmt.Errorf("cluster-info has an invalid kubelet client-CA mode extension")
	}
	var marker struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(u.Raw, &marker); err != nil {
		return false, fmt.Errorf("parse cluster-info kubelet client-CA mode: %w", err)
	}
	switch marker.Mode {
	case KubeletClientCAModeSplit:
		return true, nil
	case KubeletClientCAModeLegacy:
		return false, nil
	default:
		return false, fmt.Errorf("cluster-info has unknown kubelet client-CA mode %q", marker.Mode)
	}
}

// tokenPattern is the Kubernetes bootstrap-token format: a 6-character ID and a
// 16-character secret, lowercase alphanumeric. Parsing against it is what
// makes the protected header below safe to build by concatenation.
var tokenPattern = regexp.MustCompile(`\A([a-z0-9]{6})\.([a-z0-9]{16})\z`)

// Token is a bootstrap token that has been validated against tokenPattern.
//
// Its fields are unexported and ParseToken is the only constructor, so — like
// Authenticated on the output side — no outside package can hand Authenticate an
// unvalidated token. That matters twice: computeDetachedSignature builds a JSON
// header by concatenating ID, and an empty secret would key the MAC with the
// empty string, which any attacker can compute. The zero value is rejected by
// Authenticate.
type Token struct{ id, secret string }

// ID returns the token id. It is not secret — it is the cluster-info key suffix
// and the bootstrap-token Secret's name suffix, which is what callers need it for.
func (t Token) ID() string { return t.id }

// ParseToken validates a bootstrap token "<id>.<secret>" against
// tokenPattern. Call it BEFORE any
// network I/O, so a malformed token never causes a request to an endpoint that
// cannot yet be verified.
func ParseToken(token string) (Token, error) {
	m := tokenPattern.FindStringSubmatch(token)
	if m == nil {
		return Token{}, fmt.Errorf("bootstrap token must be <6 lowercase alphanumerics>.<16 lowercase alphanumerics>")
	}
	return Token{id: m[1], secret: m[2]}, nil
}

// computeDetachedSignature reproduces
// k8s.io/cluster-bootstrap/token/jws.ComputeDetachedSignature — the function
// bootstrapsigner itself calls — on the standard library.
//
// The result is "<b64url(protected)>..<b64url(mac)>": a compact JWS with the
// payload elided. go-jose serialises the protected header from a map, and
// encoding/json sorts map keys, so the header bytes are exactly
// {"alg":"HS256","kid":"<id>"} and the whole output is deterministic. Building
// that header by concatenation is safe only because tokenID has already been
// validated against tokenPattern, which admits no JSON metacharacter — do not
// call this with an unvalidated id.
func computeDetachedSignature(content, tokenID, tokenSecret string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"` + tokenID + `"}`))
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString([]byte(content))
	mac := hmac.New(sha256.New, []byte(tokenSecret))
	mac.Write([]byte(signingInput))
	return header + ".." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// detachedTokenIsValid recomputes the expected detached JWS and compares it to
// the received one in constant time.
//
// Recompute-and-compare, deliberately: we never parse the received protected
// header, so "alg" confusion is structurally impossible rather than defended
// against — a tampered alg changes the bytes and the comparison simply fails.
// Upstream's DetachedTokenIsValid does the same recomputation but finishes with
// a plain string ==; the constant-time compare here is the one intentional
// divergence from it.
func detachedTokenIsValid(detached, content, tokenID, tokenSecret string) bool {
	want := computeDetachedSignature(content, tokenID, tokenSecret)
	return subtle.ConstantTimeCompare([]byte(detached), []byte(want)) == 1
}

// NoSignatureError reports that cluster-info carries no JWS for this token id.
//
// Callers distinguish it because it is the one absent-signature case worth
// retrying: the bootstrapsigner patches the signature in a moment after the
// ConfigMap itself is written. A signature that is present and does NOT verify
// is never retried — that is the MITM signal, and it fails immediately.
type NoSignatureError struct{ TokenID string }

func (e *NoSignatureError) Error() string {
	return fmt.Sprintf("cluster-info carries no %s%s signature", JWSSignatureKeyPrefix, e.TokenID)
}

// Authenticated is a cluster-info kubeconfig whose detached JWS has been
// verified against the bootstrap token. Only Authenticate returns a populated
// value, and the field is unexported, so no other package can manufacture one.
// The zero value is inert and TrustBundle rejects it.
type Authenticated struct{ kubeconfig string }

// Authenticate verifies the detached JWS over data["kubeconfig"] using the
// bootstrap token, and returns the authenticated payload.
//
// This must run before the kubeconfig is parsed. The bytes arrive from a
// transport with InsecureSkipVerify, so until this returns nil they are entirely
// attacker-controlled and must not reach a YAML parser.
func Authenticate(data map[string]string, t Token) (Authenticated, error) {
	if t.id == "" || t.secret == "" {
		return Authenticated{}, fmt.Errorf("clusterinfo: Authenticate requires a token from ParseToken")
	}
	kubeconfig := data[KubeConfigKey]
	if kubeconfig == "" {
		return Authenticated{}, fmt.Errorf("cluster-info ConfigMap missing data.%s", KubeConfigKey)
	}
	sig := data[JWSSignatureKeyPrefix+t.id]
	if sig == "" {
		return Authenticated{}, &NoSignatureError{TokenID: t.id}
	}
	if !detachedTokenIsValid(sig, kubeconfig, t.id, t.secret) {
		return Authenticated{}, fmt.Errorf(
			"cluster-info %s%s signature does not verify — the response was not produced by a holder of this "+
				"bootstrap token (possible MITM). If a CA rotation is in progress on the control plane, wait for it "+
				"to finish republishing cluster-info and retry",
			JWSSignatureKeyPrefix, t.id)
	}
	return Authenticated{kubeconfig: kubeconfig}, nil
}

// Parse is the one reader of the cluster-info kubeconfig document: it returns
// the apiserver URL the document advertises and the CA data it carries.
//
// token.ensureClusterInfo writes the document with exactly one cluster entry,
// but three consumers read it back — TrustBundle on the joining node, the
// verified re-fetch in bootstrap, and RepinCA on the control plane — and a
// second entry (a VIP beside a node address, say) must not make them answer by
// Go map order, each under its own rule. So Parse never chooses between entries:
// every entry that states a server must state the same one, every entry that
// carries CA data must carry the same data, and an entry that states no opinion
// on a field is skipped. A document whose entries disagree is refused.
//
// Parse does not require either field to be present. Each consumer checks the
// field it needs, so the refusal names what that consumer is missing.
func Parse(kubeconfig []byte) (server string, caData []byte, err error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return "", nil, fmt.Errorf("parse cluster-info kubeconfig: %w", err)
	}
	for _, c := range cfg.Clusters {
		if c.Server != "" {
			if server != "" && server != c.Server {
				return "", nil, fmt.Errorf(
					"cluster-info publishes more than one server URL (%q, %q); refusing to guess which is authoritative",
					server, c.Server)
			}
			server = c.Server
		}
		if len(c.CertificateAuthorityData) > 0 {
			if len(caData) > 0 && string(caData) != string(c.CertificateAuthorityData) {
				return "", nil, fmt.Errorf("cluster-info publishes more than one differing CA bundle; refusing to guess which is authoritative")
			}
			caData = c.CertificateAuthorityData
		}
	}
	return server, caData, nil
}

// TrustBundle parses the authenticated kubeconfig, checks the pinned CA hash
// against the certificates it carries, and returns those certificates
// canonically re-encoded.
//
// This is the single trust-bundle helper, and it is reachable only from a
// populated Authenticated. What it returns is every certificate the JWS covered
// — NOT only the pinned one. The MAC authenticates the whole document, so every
// block in it is equally authenticated, and narrowing to the pinned certificate
// would break a staged CA rotation, where a joiner legitimately needs a CA it
// holds no pin for.
func (a Authenticated) TrustBundle(pin string) ([]byte, error) {
	if a.kubeconfig == "" {
		return nil, fmt.Errorf("clusterinfo: TrustBundle called on an unauthenticated value")
	}
	_, caPEM, err := Parse([]byte(a.kubeconfig))
	if err != nil {
		return nil, err
	}
	if len(caPEM) == 0 {
		return nil, fmt.Errorf("cluster-info has no CA")
	}

	certs, err := parseCertificates(caPEM)
	if err != nil {
		return nil, err
	}
	if err := checkPin(certs, pin); err != nil {
		return nil, err
	}
	// Canonical re-encoding: exactly the certificates above, nothing else. Any
	// non-CERTIFICATE PEM block and any trailing bytes in the authenticated field
	// are dropped here rather than travelling on into ca-bundle.crt.
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out, nil
}

// parseCertificates decodes every CERTIFICATE block in caPEM.
//
// A CERTIFICATE-typed block that does not parse as X.509 is a hard error, not a
// skipped block: past the JWS check a malformed block is a control-plane bug,
// and failing closed is correct. This is the same rule as client-go's cert.ParseCertsPEM.
// A bundle with no parseable certificate at all is likewise an error — an empty
// trust bundle must never reach a caller.
func parseCertificates(caPEM []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := caPEM
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("cluster-info CA bundle has an unparseable CERTIFICATE block: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("cluster-info CA bundle carries no parseable certificate")
	}
	return certs, nil
}

// checkPin accepts if ANY certificate matches the pinned hash. The cluster-info
// CA is a trust bundle: one cert at rest, old‖new mid-CA-rotation while the join
// token pins only one of them — so a first-block-only check would wrongly reject
// a valid bundle. Accepting any match is safe because the JWS authenticates the
// whole bundle.
func checkPin(certs []*x509.Certificate, want string) error {
	if err := validPin(want); err != nil {
		return err
	}
	got := make([]string, 0, len(certs))
	for _, c := range certs {
		h := PinFor(c)
		if h == want {
			return nil
		}
		got = append(got, h)
	}
	return fmt.Errorf("CA hash mismatch: server presents %v, token expects %s (possible MITM)", got, want)
}

// pinPattern is the exact shape PinFor emits: lowercase hex, sha256 only.
var pinPattern = regexp.MustCompile(`\Asha256:[0-9a-f]{64}\z`)

// validPin rejects a malformed --discovery-token-ca-cert-hash BEFORE it is
// compared against anything.
//
// Without this a pin pasted with uppercase hex, without the "sha256:" prefix, or
// with stray whitespace can never match any certificate, and the operator is told
// "possible MITM" — sending them to hunt an attacker over a copy-paste error, in
// the one message whose whole purpose is diagnosis. The failure was always closed;
// it was the diagnosis that was wrong.
func validPin(want string) error {
	if want == "" {
		return fmt.Errorf("no CA pin supplied: --discovery-token-ca-cert-hash is required")
	}
	if !pinPattern.MatchString(want) {
		return fmt.Errorf("malformed --discovery-token-ca-cert-hash %q: "+
			"expected sha256:<64 lowercase hex chars> as printed by `stratum token create` "+
			"(this is a malformed pin, NOT a certificate mismatch)", want)
	}
	return nil
}

// ValidatePin is validPin for callers outside this package: `stratum up --join`
// checks the pin before comparing it with the node's own CA, so a missing or
// mistyped pin gets this diagnosis there too, rather than "a different cluster".
func ValidatePin(want string) error { return validPin(want) }

// PinFor returns the "sha256:<hex>" pin of a certificate's
// SubjectPublicKeyInfo — the same value token.CACertHash computes on the
// control plane, so the two ends of the handshake agree.
func PinFor(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}
