package operator

// The reconciler runs long after admission, so identity must be captured then.

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
	// hostNetwork operator: this is a node port the Service routes to.
	webhookPort = 9448
	webhookPath = "/mutate-helmchart"
	// A TLS-authenticated liveness probe, no admission body.
	webhookHealthPath = "/healthz"
)

type patchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Annotation keys contain '~' and '/', so an unescaped pointer token mis-targets.
func jsonPatchEscape(token string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(token)
}

// add on an existing key replaces it, overwriting any client-supplied requester value.
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
		return []patchOp{{Op: "add", Path: "/metadata/annotations", Value: want}}, nil
	}
	// Deterministic op order keeps the emitted patch stable.
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

// Metadata-only updates preserve the old stamp, so the controller cannot become the requester.
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
	// An empty username would produce a stamp that impersonates nobody but looks present.
	if info.Username == "" {
		return admissionErrorf(uid, "%s %s/%s: admission request carried no user identity; refusing admission",
			req.Kind.Kind, req.Namespace, req.Name)
	}
	// An unsigned stamp looks present but fails verification cluster-wide.
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
			TypeMeta: review.TypeMeta,
			Response: resp,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Printf("admission webhook: encode response: %v", err)
		}
	}
}

// Fetches the serving certificate on every handshake so a rotation needs no restart.
func runWebhookServer(ctx context.Context, listener net.Listener, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), signers *requesterSignerHolder) error {
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, admissionHandler(signers))
	mux.HandleFunc(webhookHealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return serveWebhook(ctx, listener, webhookClientTLS(getCert, func() ([]byte, error) { return pki.ReadCABundle("front-proxy-ca") }), mux)
}

// Verifies the apiserver's front-proxy certificate, not a general cluster CA client cert.
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
