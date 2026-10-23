package operator

// webhook.go — the mutating admission webhook that stamps the requester onto
// HelmChart / HelmChartConfig CRs at admission.
//
// The reconciler runs asynchronously, long after the CR was admitted, so the
// requester's identity has to be captured AT admission and persisted on the
// object — the API object records no creator of its own. This webhook reads the
// authenticated identity from AdmissionRequest.userInfo and overwrites the
// requester annotations with it on every CREATE and UPDATE, so a client cannot
// forge a different requester (a client-supplied value is always replaced by the
// real one) and the annotation always reflects who last wrote the desired state.
//
// Admission fails closed to prevent replaying an old valid stamp on a recreated
// resource while stamping is unavailable. The configured privileged namespaces
// (Options.PrivilegedNamespaces, see webhook_policy.go) are exempt.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/ctrliq/stratum/internal/pki"
	"io"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// webhookPort is the HTTPS port the operator serves admission on. The operator
	// runs hostNetwork, so this is a node port; the Service/EndpointSlice
	// (webhookdeploy.go) route the apiserver here.
	webhookPort = 9448
	// webhookPath is the single admission endpoint. Both resources post here.
	webhookPath = "/mutate-helmchart"
	// webhookHealthPath is a TLS-authenticated liveness probe (no admission body).
	webhookHealthPath = "/healthz"
)

// patchOp is one RFC 6902 JSON Patch operation. Emitted as the AdmissionResponse
// patch so the apiserver applies the requester annotations to the incoming object.
type patchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// jsonPatchEscape escapes a JSON Pointer reference token per RFC 6901: "~" -> "~0"
// and "/" -> "~1". The annotation keys contain both ("stratum.ciq.com/requester"),
// so an unescaped path would point at the wrong (nonexistent) location and the
// patch would either no-op or fail.
func jsonPatchEscape(token string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(token)
}

// requesterPatchOps builds the JSON Patch that stamps info + its signature onto an
// object whose current annotations are `existing`. When the object has no
// annotations yet, the map must be created first (a lone `add
// /metadata/annotations/<key>` fails: its parent object does not exist); when it
// does, each key is added individually so the other annotations are preserved.
// `add` on an existing key replaces it, so the same op both creates and overwrites
// — which is exactly the anti-forgery behavior (a client-supplied requester value,
// signature included, is overwritten with the real one).
func requesterPatchOps(existing map[string]string, info requesterInfo, sig string) ([]patchOp, error) {
	groupsJSON, err := json.Marshal(info.Groups)
	if err != nil {
		return nil, fmt.Errorf("marshal requester groups: %w", err)
	}
	want := map[string]string{
		annRequester:       info.Username,
		annRequesterUID:    info.UID,
		annRequesterGroups: string(groupsJSON),
		annRequesterSig:    sig,
	}
	if len(existing) == 0 {
		// Create the whole annotations map in one op with all keys set.
		return []patchOp{{Op: "add", Path: "/metadata/annotations", Value: want}}, nil
	}
	// Deterministic op order so the emitted patch (and its tests) are stable.
	keys := []string{annRequester, annRequesterUID, annRequesterGroups, annRequesterSig}
	ops := make([]patchOp, 0, len(keys))
	for _, k := range keys {
		ops = append(ops, patchOp{
			Op:    "add",
			Path:  "/metadata/annotations/" + jsonPatchEscape(k),
			Value: want[k],
		})
	}
	return ops, nil
}

// admissionErrorf denies a request whose identity cannot be safely stamped.
func admissionErrorf(uid string, format string, args ...interface{}) *admissionv1.AdmissionResponse {
	msg := fmt.Sprintf(format, args...)
	log.Printf("admission webhook: %s", msg)
	return &admissionv1.AdmissionResponse{
		UID:      types.UID(uid),
		Allowed:  false,
		Result:   &metav1.Status{Message: msg, Code: http.StatusForbidden},
		Warnings: []string{msg},
	}
}

// buildAdmissionResponse computes the stamping patch for one AdmissionRequest.
// Split from the HTTP handler so the stamping logic is unit-testable without a
// server. Stamping failures deny the request (see admissionErrorf).
//
// Spec changes are stamped with the authenticated writer. Metadata-only
// updates preserve the old stamp, so the controller cannot become the requester
// when it updates finalizers. Client-supplied annotation changes are overwritten.
func buildAdmissionResponse(req *admissionv1.AdmissionRequest, signer *requesterSigner) *admissionv1.AdmissionResponse {
	if req == nil {
		return &admissionv1.AdmissionResponse{Allowed: false}
	}
	uid := string(req.UID)
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, obj); err != nil {
		return admissionErrorf(uid, "decode object: %v", err)
	}
	if req.Operation == admissionv1.Update {
		old := &unstructured.Unstructured{}
		if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
			return admissionErrorf(uid, "decode old object: %v", err)
		}
		if reflect.DeepEqual(old.Object["spec"], obj.Object["spec"]) {
			// A finalizer-only or metadata update cannot replace the captured principal.
			ann := obj.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			for _, key := range []string{annRequester, annRequesterUID, annRequesterGroups, annRequesterSig, annRequesterSigV2, annRequesterKeyID, annRequesterFieldsV2} {
				if value, ok := old.GetAnnotations()[key]; ok {
					ann[key] = value
				} else {
					delete(ann, key)
				}
			}
			patch, _ := json.Marshal([]patchOp{{Op: "add", Path: "/metadata/annotations", Value: ann}})
			pt := admissionv1.PatchTypeJSONPatch
			return &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true, Patch: patch, PatchType: &pt}
		}
	}
	if obj.GetName() == "" {
		return admissionErrorf(uid, "metadata.name is required for requester binding")
	}

	info := requesterInfo{
		Username: req.UserInfo.Username,
		UID:      req.UserInfo.UID,
		Groups:   req.UserInfo.Groups,
	}
	// An unauthenticated or identity-less request should not be stamped with an
	// empty username — that would produce a stamp the backstop treats as present
	// but which impersonates nobody. Leave it unstamped; the backstop refuses it.
	if info.Username == "" {
		return admissionErrorf(uid, "%s %s/%s: admission request carried no user identity; refusing admission",
			req.Kind.Kind, req.Namespace, req.Name)
	}
	// Without a signer the stamp cannot be made authentic, and an unsigned stamp is
	// worse than none (it looks present but fails verification cluster-wide). Leave
	// it unstamped and warn; the backstop refuses the install until the key loads.
	if signer == nil {
		return admissionErrorf(uid, "%s %s/%s: requester-signing key unavailable; refusing admission",
			req.Kind.Kind, req.Namespace, req.Name)
	}

	binding := requesterBinding(obj)
	sig := signer.sign(binding, info)
	ops, err := requesterPatchOps(obj.GetAnnotations(), info, sig)
	if err != nil {
		return admissionErrorf(uid, "%s %s/%s: build patch: %v", req.Kind.Kind, req.Namespace, req.Name, err)
	}
	// Readers are deployed before the administrator enables new signing through
	// the shared Secret. Legacy signatures remain alongside v2 after activation.
	if signer.bindingVersion == stampBindingV2 {
		policy := requesterFieldPolicy(obj)
		signingObject := obj.DeepCopy()
		signingAnnotations := signingObject.GetAnnotations()
		if signingAnnotations == nil {
			signingAnnotations = make(map[string]string)
		}
		signingAnnotations[annRequesterFieldsV2] = policy
		signingObject.SetAnnotations(signingAnnotations)
		sigV2, keyID := signer.signV2(bindingV2(signingObject), info), signer.currentID()
		if len(obj.GetAnnotations()) == 0 {
			if m, ok := ops[0].Value.(map[string]string); ok {
				m[annRequesterSigV2], m[annRequesterKeyID], m[annRequesterFieldsV2] = sigV2, keyID, policy
			}
		} else {
			ops = append(ops,
				patchOp{Op: "add", Path: "/metadata/annotations/" + jsonPatchEscape(annRequesterSigV2), Value: sigV2},
				patchOp{Op: "add", Path: "/metadata/annotations/" + jsonPatchEscape(annRequesterKeyID), Value: keyID},
				patchOp{Op: "add", Path: "/metadata/annotations/" + jsonPatchEscape(annRequesterFieldsV2), Value: policy},
			)
		}
	} else {
		for _, key := range []string{annRequesterSigV2, annRequesterKeyID, annRequesterFieldsV2} {
			if _, present := obj.GetAnnotations()[key]; present {
				ops = append(ops, patchOp{Op: "remove", Path: "/metadata/annotations/" + jsonPatchEscape(key)})
			}
		}
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return admissionErrorf(uid, "%s %s/%s: marshal patch: %v", req.Kind.Kind, req.Namespace, req.Name, err)
	}
	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		UID:       req.UID,
		Allowed:   true,
		Patch:     patch,
		PatchType: &pt,
	}
}

// admissionHandler returns the HTTP handler for the admission endpoint, closing
// over the signer source: decode the AdmissionReview, compute the response, echo
// it back wrapped in a review of the same apiVersion/kind. The snapshot is read
// per admission, so a signer the Secret owner publishes is used by the next
// request without a restart.
func admissionHandler(signers signerSnapshot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 3<<20)) // 3 MiB cap: a CR is small
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var review admissionv1.AdmissionReview
		if err := json.Unmarshal(body, &review); err != nil {
			http.Error(w, "decode AdmissionReview", http.StatusBadRequest)
			return
		}
		resp := buildAdmissionResponse(review.Request, signers.current())
		out := admissionv1.AdmissionReview{
			TypeMeta: review.TypeMeta, // echo the request's apiVersion/kind
			Response: resp,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Printf("admission webhook: encode response: %v", err)
		}
	}
}

// runWebhookServer serves the admission endpoint over HTTPS until ctx is
// cancelled, using getCert to fetch the current serving certificate on every
// handshake (so a CA/cert rotation is picked up without a restart) and signers
// for the per-admission requester-signing snapshot. It returns when the server
// stops; a non-context error is returned to the caller.
func runWebhookServer(ctx context.Context, listener net.Listener, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), signers *requesterSignerHolder) error {
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, admissionHandler(signers))
	mux.HandleFunc(webhookHealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return serveWebhook(ctx, listener, webhookClientTLS(getCert, func() ([]byte, error) { return pki.ReadCABundle("front-proxy-ca") }), mux)
}

// Verify the apiserver's existing front-proxy certificate, not a tenant client
// certificate issued by the general cluster CA. Reload trust on each handshake.
func webhookClientTLS(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), bundle func() ([]byte, error)) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: getCert, ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("apiserver client certificate required")
			}
			pem, err := bundle()
			if err != nil {
				return err
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return fmt.Errorf("invalid front-proxy CA bundle")
			}
			leaf := state.PeerCertificates[0]
			if leaf.Subject.CommonName != "front-proxy-client" {
				return fmt.Errorf("unexpected webhook client identity")
			}
			intermediates := x509.NewCertPool()
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			return err
		}}
}

func serveWebhook(ctx context.Context, listener net.Listener, tlsConfig *tls.Config, handler http.Handler) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", webhookPort),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig,
	}
	errCh := make(chan error, 1)
	go func() {
		// Certs come from TLSConfig.GetCertificate, so the file args are empty.
		errCh <- srv.ServeTLS(listener, "", "")
	}()
	log.Printf("stratum-operator: admission webhook serving on :%d%s", webhookPort, webhookPath)
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
