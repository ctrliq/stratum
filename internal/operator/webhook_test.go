package operator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

func TestJSONPatchEscape(t *testing.T) {
	if got := jsonPatchEscape("stratum.ciq.com/requester"); got != "stratum.ciq.com~1requester" {
		t.Errorf("escape = %q", got)
	}
	if got := jsonPatchEscape("a~b/c"); got != "a~0b~1c" {
		t.Errorf("escape = %q, want a~0b~1c (~ before / per RFC 6901)", got)
	}
}

func TestRequesterPatchOps(t *testing.T) {
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}

	t.Run("no existing annotations: create the map in one op", func(t *testing.T) {
		ops, err := requesterPatchOps(nil, info, "SIG")
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != "/metadata/annotations" {
			t.Fatalf("ops = %+v; want a single add of /metadata/annotations", ops)
		}
		m := ops[0].Value.(map[string]string)
		if m[annRequester] != "alice" || m[annRequesterUID] != "u1" || m[annRequesterGroups] != `["team-a"]` || m[annRequesterSig] != "SIG" {
			t.Errorf("value = %v", m)
		}
	})

	t.Run("existing annotations: per-key add incl signature, escaped paths", func(t *testing.T) {
		ops, err := requesterPatchOps(map[string]string{"other": "keep"}, info, "SIG")
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 4 {
			t.Fatalf("want 4 per-key ops (incl sig), got %d: %+v", len(ops), ops)
		}
		want := map[string]bool{
			"/metadata/annotations/stratum.ciq.com~1requester":        false,
			"/metadata/annotations/stratum.ciq.com~1requester-uid":    false,
			"/metadata/annotations/stratum.ciq.com~1requester-groups": false,
			"/metadata/annotations/stratum.ciq.com~1requester-sig":    false,
		}
		for _, op := range ops {
			if op.Op != "add" {
				t.Errorf("op %q is not add", op.Path)
			}
			if _, known := want[op.Path]; !known {
				t.Errorf("unexpected path %q", op.Path)
			}
			want[op.Path] = true
		}
		for p, seen := range want {
			if !seen {
				t.Errorf("missing op for %q", p)
			}
		}
	})
}

// admissionRequestFor builds a v1 AdmissionReview request for the given operation
// that stamps the given user over an object with the given uid + annotations.
func admissionRequestFor(t *testing.T, op admissionv1.Operation, uid string, user authenticationv1.UserInfo, annotations map[string]string) *admissionv1.AdmissionReview {
	t.Helper()
	meta := map[string]interface{}{"name": "app", "namespace": "team-a", "uid": uid}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	raw, err := json.Marshal(map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart", "metadata": meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "req-1",
			Kind:      metav1.GroupVersionKind{Group: k8sutil.HelmChartGroup, Version: k8sutil.HelmChartVersion, Kind: "HelmChart"},
			Namespace: "team-a",
			Name:      "app",
			Operation: op,
			UserInfo:  user,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func TestBuildAdmissionResponseStampsSignedOnCreate(t *testing.T) {
	s := testSigner()
	req := admissionRequestFor(t, admissionv1.Create, "uid-1", authenticationv1.UserInfo{
		Username: "alice", UID: "u1", Groups: []string{"team-a", "system:authenticated"},
	}, nil)
	resp := buildAdmissionResponse(req.Request, s)
	if !resp.Allowed || resp.PatchType == nil {
		t.Fatalf("must allow+patch: allowed=%v pt=%v", resp.Allowed, resp.PatchType)
	}
	var ops []patchOp
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("patch is not valid JSON: %v", err)
	}
	if len(ops) != 1 || ops[0].Path != "/metadata/annotations" {
		t.Fatalf("ops = %+v", ops)
	}
	m := ops[0].Value.(map[string]interface{})
	// The signature binds the resource and desired state to the requester.
	obj := &unstructured.Unstructured{}
	_ = json.Unmarshal(req.Request.Object.Raw, obj)
	wantSig := s.sign(requesterBinding(obj), requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a", "system:authenticated"}})
	if m[annRequesterSig] != wantSig {
		t.Errorf("signature = %v, want %v", m[annRequesterSig], wantSig)
	}
	if m[annRequester] != "alice" {
		t.Errorf("requester = %v", m[annRequester])
	}
}

func TestBuildAdmissionResponsePreservesIdentityOnMetadataUpdate(t *testing.T) {
	s := testSigner()
	old := signedHC("team-a", "app", "uid-1", requesterInfo{Username: "alice"}, s)
	changed := old.DeepCopy()
	changed.SetFinalizers([]string{helmFinalizer})
	changed.SetAnnotations(map[string]string{annRequester: "admin"})
	oldRaw, _ := json.Marshal(old)
	raw, _ := json.Marshal(changed)
	resp := buildAdmissionResponse(&admissionv1.AdmissionRequest{UID: "req", Operation: admissionv1.Update, OldObject: runtime.RawExtension{Raw: oldRaw}, Object: runtime.RawExtension{Raw: raw}, UserInfo: authenticationv1.UserInfo{Username: "admin", Groups: []string{"system:masters"}}}, s)
	var ops []patchOp
	_ = json.Unmarshal(resp.Patch, &ops)
	if !resp.Allowed || len(ops) != 1 || ops[0].Value.(map[string]interface{})[annRequester] != "alice" {
		t.Fatalf("metadata update replaced requester: %+v", resp)
	}
}

// TestBuildAdmissionResponseNoIdentity: a create with no user identity is denied.
func TestBuildAdmissionResponseNoIdentity(t *testing.T) {
	resp := buildAdmissionResponse(admissionRequestFor(t, admissionv1.Create, "uid-1", authenticationv1.UserInfo{}, nil).Request, testSigner())
	if resp.Allowed {
		t.Fatal("must reject an identity-less create")
	}
	if len(resp.Patch) != 0 {
		t.Errorf("must not stamp an empty identity; patch = %s", resp.Patch)
	}
	if len(resp.Warnings) == 0 {
		t.Error("should warn that the request carried no identity")
	}
}

// TestBuildAdmissionResponseNoSigner: without a signer, a create is denied.
func TestBuildAdmissionResponseNoSigner(t *testing.T) {
	resp := buildAdmissionResponse(admissionRequestFor(t, admissionv1.Create, "uid-1", authenticationv1.UserInfo{Username: "alice"}, nil).Request, nil)
	if resp.Allowed || len(resp.Patch) != 0 {
		t.Errorf("no signer: want deny with no patch; allowed=%v patch=%s", resp.Allowed, resp.Patch)
	}
}

// TestWebhookStampVerifiesInReconciler ties the two halves together: the stamp
// the webhook writes on CREATE must be one the reconciler's verifiedRequester
// accepts (same signer and desired-state binding). A drift between them would silently
// refuse every install.
func TestWebhookStampVerifiesInReconciler(t *testing.T) {
	s := testSigner()
	req := admissionRequestFor(t, admissionv1.Create, "",
		authenticationv1.UserInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}, nil)
	resp := buildAdmissionResponse(req.Request, s)
	var ops []patchOp
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatal(err)
	}
	ann := map[string]string{}
	for k, v := range ops[0].Value.(map[string]interface{}) {
		ann[k] = v.(string)
	}
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart",
		"metadata": map[string]interface{}{"namespace": "team-a", "name": "app", "uid": "uid-1"},
	}}
	hc.SetAnnotations(ann)
	info, authentic, reason := verifiedRequester(hc, s)
	if !authentic {
		t.Fatalf("reconciler rejected the webhook's own stamp: %s", reason)
	}
	if info.Username != "alice" {
		t.Errorf("verified requester = %q, want alice", info.Username)
	}
}

func TestHandleAdmissionRoundTrip(t *testing.T) {
	review := admissionRequestFor(t, admissionv1.Create, "uid-1", authenticationv1.UserInfo{Username: "alice", Groups: []string{"team-a"}}, nil)
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	admissionHandler(testSigner())(rr, httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not an AdmissionReview: %v", err)
	}
	if out.Response == nil || !out.Response.Allowed {
		t.Fatal("response must allow")
	}
	if out.Response.UID != "req-1" {
		t.Errorf("UID = %q, want req-1 echoed back", out.Response.UID)
	}
	if out.TypeMeta.APIVersion != "admission.k8s.io/v1" {
		t.Errorf("apiVersion not echoed: %q", out.TypeMeta.APIVersion)
	}
}
