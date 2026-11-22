// Separate package so only Authenticate can yield the trust bundle; the compiler enforces it.
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
	// Mirrors k8s.io/cluster-bootstrap constants; importing that module adds 19 modules and a CVE.
	KubeConfigKey         = "kubeconfig"
	JWSSignatureKeyPrefix = "jws-kubeconfig-"
)

// KubeletClientCAConfigMap holds a split cluster's kubelet client-CA bundle.
const (
	KubeletClientCAConfigMap = "stratum-kubelet-client-ca"
	KubeletClientCAKey       = "ca.crt"
	// Covered by cluster-info's detached JWS; absence marks cluster-info from before the split.
	KubeletClientCAModeExtension = "stratum.ciq.com/kubelet-client-ca"
	KubeletClientCAModeLegacy    = "legacy"
	KubeletClientCAModeSplit     = "split"
)

// KubeletClientCASplit reports the mode from the authenticated kubeconfig; unknown modes error.
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

// Bootstrap-token shape; validating it is what makes header concatenation safe below.
var tokenPattern = regexp.MustCompile(`\A([a-z0-9]{6})\.([a-z0-9]{16})\z`)

// Token is a bootstrap token validated by ParseToken; the zero value is rejected by Authenticate.
type Token struct{ id, secret string }

// ID returns the token id; it is not secret.
func (t Token) ID() string { return t.id }

// ParseToken validates a bootstrap token; call it before any network I/O.
func ParseToken(token string) (Token, error) {
	m := tokenPattern.FindStringSubmatch(token)
	if m == nil {
		return Token{}, fmt.Errorf("bootstrap token must be <6 lowercase alphanumerics>.<16 lowercase alphanumerics>")
	}
	return Token{id: m[1], secret: m[2]}, nil
}

// Reproduces the upstream bootstrapsigner's detached signature; tokenID must already be validated.
func computeDetachedSignature(content, tokenID, tokenSecret string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"` + tokenID + `"}`))
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString([]byte(content))
	mac := hmac.New(sha256.New, []byte(tokenSecret))
	mac.Write([]byte(signingInput))
	return header + ".." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Recompute-and-compare: the received header is never parsed, so alg confusion cannot happen.
func detachedTokenIsValid(detached, content, tokenID, tokenSecret string) bool {
	want := computeDetachedSignature(content, tokenID, tokenSecret)
	return subtle.ConstantTimeCompare([]byte(detached), []byte(want)) == 1
}

// The signer writes the ConfigMap first, its signature moments later; absence alone is retried.
type NoSignatureError struct{ TokenID string }

func (e *NoSignatureError) Error() string {
	return fmt.Sprintf("cluster-info carries no %s%s signature", JWSSignatureKeyPrefix, e.TokenID)
}

// Authenticated is a cluster-info kubeconfig with a verified JWS; only Authenticate populates one.
type Authenticated struct{ kubeconfig string }

// Authenticate verifies the detached JWS; the bytes are untrusted until it succeeds.
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

// Parse reads the cluster-info kubeconfig, refusing a document whose cluster entries disagree.
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

// TrustBundle returns every certificate the JWS covered, not only the pinned one.
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
	// Non-CERTIFICATE blocks and trailing bytes are dropped, not carried into ca-bundle.crt.
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out, nil
}

// A malformed CERTIFICATE block is a hard error: past the JWS check it is a control-plane bug.
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

// Any match suffices: mid-rotation the token pins only one cert of the bundle.
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

// A malformed pin must read as malformed, not as a mismatch sending operators after an attacker.
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

// ValidatePin gives callers outside this package the same malformed-pin diagnosis.
func ValidatePin(want string) error { return validPin(want) }

// PinFor returns the pin token.CACertHash computes, so both ends of the handshake agree.
func PinFor(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}
