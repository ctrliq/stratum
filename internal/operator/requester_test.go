package operator

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

func testSigner() *requesterSigner {
	return &requesterSigner{key: []byte("test-hmac-key-0123456789abcdef0123456789"), bindingVersion: stampBindingV2}
}

func signedHC(ns, name, uid string, info requesterInfo, signer *requesterSigner) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name, "uid": uid},
	}}
	groups, _ := json.Marshal(info.Groups)
	o.SetAnnotations(map[string]string{
		annRequester:       info.Username,
		annRequesterUID:    info.UID,
		annRequesterGroups: string(groups),
		annRequesterSig:    signer.sign(requesterBinding(o), info),
	})
	return o
}

func TestImpersonationRequired(t *testing.T) {
	priv := []string{"kube-system", "addons"}
	if impersonationRequired("kube-system", priv) {
		t.Error("privileged namespace must NOT require impersonation (installs as operator)")
	}
	if !impersonationRequired("team-a", priv) {
		t.Error("non-privileged namespace MUST require impersonation")
	}
	if !impersonationRequired("team-a", nil) {
		t.Error("with no privileged namespaces, every namespace requires impersonation")
	}
}

func hcWithAnnotations(ns string, ann map[string]string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata":   map[string]interface{}{"namespace": ns, "name": "app", "uid": "uid-1"},
	}}
	if ann != nil {
		o.SetAnnotations(ann)
	}
	return o
}

func TestRequesterFromCR(t *testing.T) {
	t.Run("present with groups + sig", func(t *testing.T) {
		hc := hcWithAnnotations("team-a", map[string]string{
			annRequester:       "alice",
			annRequesterUID:    "uid-1",
			annRequesterGroups: `["team-a","system:authenticated"]`,
			annRequesterSig:    "abc",
		})
		info, sig, ok, err := requesterFromCR(hc)
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if info.Username != "alice" || info.UID != "uid-1" || sig != "abc" {
			t.Errorf("info=%+v sig=%q", info, sig)
		}
		if !slices.Equal(info.Groups, []string{"team-a", "system:authenticated"}) {
			t.Errorf("groups = %v", info.Groups)
		}
	})
	t.Run("absent", func(t *testing.T) {
		_, _, ok, err := requesterFromCR(hcWithAnnotations("team-a", nil))
		if ok || err != nil {
			t.Errorf("no stamp must be ok=false, no error; got ok=%v err=%v", ok, err)
		}
	})
	t.Run("malformed groups is an error", func(t *testing.T) {
		_, _, ok, err := requesterFromCR(hcWithAnnotations("team-a", map[string]string{
			annRequester: "alice", annRequesterGroups: `not-json`,
		}))
		if err == nil || ok {
			t.Error("malformed groups annotation must error (dropping groups would under-authorize)")
		}
	})
}

func TestVerifiedRequester(t *testing.T) {
	s := testSigner()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}

	t.Run("authentic", func(t *testing.T) {
		got, ok, _ := verifiedRequester(signedHC("team-a", "app", "uid-1", info, s), s)
		if !ok || got.Username != "alice" {
			t.Fatalf("authentic stamp not accepted: ok=%v got=%+v", ok, got)
		}
	})
	t.Run("unstamped", func(t *testing.T) {
		if _, ok, reason := verifiedRequester(hcWithAnnotations("team-a", nil), s); ok || reason == "" {
			t.Errorf("unstamped must be inauthentic with a reason; ok=%v", ok)
		}
	})
	t.Run("forged (no valid signature)", func(t *testing.T) {
		hc := hcWithAnnotations("team-a", map[string]string{
			annRequester:    "system:masters",
			annRequesterSig: "not-a-real-sig",
		})
		if _, ok, _ := verifiedRequester(hc, s); ok {
			t.Error("a forged stamp with a bogus signature must be rejected")
		}
	})
	t.Run("tampered (valid sig for a different identity)", func(t *testing.T) {
		hc := signedHC("team-a", "app", "uid-1", info, s)
		ann := hc.GetAnnotations()
		ann[annRequester] = "system:masters"
		hc.SetAnnotations(ann)
		if _, ok, _ := verifiedRequester(hc, s); ok {
			t.Error("changing the identity without re-signing must fail verification")
		}
	})
	t.Run("signature bound to resource (cannot be copied to another object)", func(t *testing.T) {
		hc := signedHC("team-a", "app", "uid-1", info, s)
		hc.SetName("other")
		if _, ok, _ := verifiedRequester(hc, s); ok {
			t.Error("a stamp must not verify on another resource")
		}
	})
	t.Run("nil signer fails closed", func(t *testing.T) {
		if _, ok, _ := verifiedRequester(signedHC("team-a", "app", "uid-1", info, s), nil); ok {
			t.Error("a nil signer must verify nothing (fail closed)")
		}
	})
}

func TestInstallIdentity(t *testing.T) {
	s := testSigner()
	priv := []string{"kube-system"}

	t.Run("privileged namespace installs as operator (nil, no refuse)", func(t *testing.T) {
		imp, refuse, _ := installIdentity(hcWithAnnotations("kube-system", nil), priv, s)
		if imp != nil || refuse {
			t.Errorf("privileged: imp=%v refuse=%v, want nil/false", imp, refuse)
		}
	})
	t.Run("non-privileged, authentic -> impersonate", func(t *testing.T) {
		hc := signedHC("team-a", "app", "uid-1", requesterInfo{Username: "alice", Groups: []string{"team-a"}}, s)
		imp, refuse, _ := installIdentity(hc, priv, s)
		if refuse || imp == nil || imp.UserName != "alice" || !slices.Equal(imp.Groups, []string{"team-a"}) {
			t.Errorf("authentic stamp: imp=%+v refuse=%v", imp, refuse)
		}
	})
	t.Run("non-privileged, unstamped -> fail closed", func(t *testing.T) {
		imp, refuse, msg := installIdentity(hcWithAnnotations("team-a", nil), priv, s)
		if !refuse || imp != nil || msg == "" {
			t.Fatalf("unstamped must fail closed: imp=%v refuse=%v", imp, refuse)
		}
	})
	t.Run("non-privileged, forged -> fail closed", func(t *testing.T) {
		hc := hcWithAnnotations("team-a", map[string]string{annRequester: "system:masters", annRequesterSig: "bogus"})
		_, refuse, _ := installIdentity(hc, priv, s)
		if !refuse {
			t.Fatal("a forged stamp must fail closed, never install as the forged identity")
		}
	})
	t.Run("nil signer -> fail closed even with a stamp", func(t *testing.T) {
		hc := signedHC("team-a", "app", "uid-1", requesterInfo{Username: "alice"}, s)
		_, refuse, _ := installIdentity(hc, priv, nil)
		if !refuse {
			t.Fatal("with no signer, verification is impossible and must fail closed")
		}
	})
}

func TestUninstallIdentityFailsClosed(t *testing.T) {
	s := testSigner()
	priv := []string{"kube-system"}
	if imp, refuse, _ := uninstallIdentity(hcWithAnnotations("kube-system", nil), priv, s); refuse || imp != nil {
		t.Fatal("privileged cleanup refused")
	}
	authentic := signedHC("team-a", "app", "uid-1", requesterInfo{Username: "alice"}, s)
	if imp, refuse, _ := uninstallIdentity(authentic, priv, s); refuse || imp == nil || imp.UserName != "alice" {
		t.Fatal("verified cleanup not impersonated")
	}
	for _, obj := range []*unstructured.Unstructured{hcWithAnnotations("team-a", nil), hcWithAnnotations("team-a", map[string]string{annRequester: "admin", annRequesterSig: "forged"})} {
		if _, refuse, _ := uninstallIdentity(obj, priv, s); !refuse {
			t.Fatal("unverified cleanup elevated to operator")
		}
	}
}

func TestRequesterVerifyFailureDistinguishesWrongKeyFromTampered(t *testing.T) {
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	spec := map[string]any{"chart": "app"}

	s := keyringFromSecret(t, fake.NewSimpleClientset(), "k1", map[string][]byte{"k1": testRotationKey(1)})
	wrongKey := stampedV2("team-a", "app", "uid-1", info, s, "k1", spec)
	foreign := keyringFromSecret(t, fake.NewSimpleClientset(), "k-gone", map[string][]byte{"k-gone": testRotationKey(9)})
	ann := wrongKey.GetAnnotations()
	ann[annRequesterSig] = foreign.sign(requesterBinding(wrongKey), info)
	ann[annRequesterSigV2] = foreign.signV2(requesterBinding(wrongKey), info)
	ann[annRequesterKeyID] = "k-gone"
	wrongKey.SetAnnotations(ann)
	_, okWrong, reasonWrong := verifiedRequester(wrongKey, s)
	if okWrong {
		t.Fatal("a stamp signed under a key this operator does not retain must be refused")
	}
	if !strings.Contains(reasonWrong, "k-gone") {
		t.Errorf("wrong-key refusal = %q, want it to name the stamp's key id k-gone", reasonWrong)
	}

	tampered := stampedV2("team-a", "app", "uid-1", info, s, "k1", spec)
	tampered.Object["spec"].(map[string]any)["chart"] = "other"
	_, okTamp, reasonTamp := verifiedRequester(tampered, s)
	if okTamp {
		t.Fatal("a tampered binding must be refused")
	}
	if !strings.Contains(reasonTamp, "tamper") {
		t.Errorf("tampered refusal = %q, want it to say the stamp is tampered", reasonTamp)
	}
	if reasonWrong == reasonTamp {
		t.Errorf("wrong-key and tampered refusals must be distinguishable; both said %q", reasonWrong)
	}
}

func TestRequesterStampV2FallbackForLegacyRestamp(t *testing.T) {
	s := testSigner()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	stamped := stampedV2("team-a", "app", "uid-1", info, s, derivedKeyID(s.key), map[string]any{"chart": "app", "valuesContent": "replicas: 1"})

	restamped := stamped.DeepCopy()
	restamped.Object["spec"].(map[string]any)["valuesContent"] = "replicas: 2"
	ann := restamped.GetAnnotations()
	ann[annRequesterSig] = s.sign(requesterBinding(restamped), info)
	restamped.SetAnnotations(ann)
	if _, ok, reason := verifiedRequester(restamped, s); !ok {
		t.Fatalf("a fresh legacy signature must authenticate the object when the stored v2 signature is stale: %s", reason)
	}

	foreignKeyID := restamped.DeepCopy()
	ann = foreignKeyID.GetAnnotations()
	ann[annRequesterKeyID] = "k-gone"
	ann[annRequesterSigV2] = "not-verifiable-under-k-gone"
	foreignKeyID.SetAnnotations(ann)
	if _, ok, reason := verifiedRequester(foreignKeyID, s); !ok {
		t.Fatalf("a retained-key legacy signature must authenticate a stamp naming an unretained key: %s", reason)
	}

	// genuine tampering fails both signatures: the refusal names the verification failure.
	tampered := restamped.DeepCopy()
	tampered.Object["spec"].(map[string]any)["chart"] = "other"
	if _, ok, reason := verifiedRequester(tampered, s); ok {
		t.Fatal("a stamp whose v2 and legacy signatures both fail must be refused")
	} else if !strings.Contains(reason, "failed signature verification") {
		t.Errorf("tampered refusal = %q, want a signature-verification failure", reason)
	}

	v2ok := stamped.DeepCopy()
	ann = v2ok.GetAnnotations()
	ann[annRequesterSig] = "stale-legacy-signature"
	v2ok.SetAnnotations(ann)
	if _, ok, reason := verifiedRequester(v2ok, s); !ok {
		t.Fatalf("a stamp whose v2 signature verifies must ignore a stale legacy signature: %s", reason)
	}
}

func TestUninstallRefusalNamesTheUninstall(t *testing.T) {
	s := testSigner()
	priv := []string{"kube-system"}
	cases := []struct {
		name string
		hc   *unstructured.Unstructured
	}{
		{"unstamped", hcWithAnnotations("team-a", nil)},
		{"forged stamp", hcWithAnnotations("team-a", map[string]string{annRequester: "admin", annRequesterSig: "forged"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, reason := verifiedRequester(tc.hc, s)
			if reason == "" {
				t.Fatal("fixture must be unverified")
			}
			_, refuse, msg := uninstallIdentity(tc.hc, priv, s)
			if !refuse {
				t.Fatal("unverified cleanup must be refused")
			}
			if !strings.HasPrefix(msg, "refusing to uninstall: "+reason+";") {
				t.Errorf("uninstall refusal must name the uninstall and carry the reason %q: %q", reason, msg)
			}
			if !strings.Contains(msg, "remove the finalizer") {
				t.Errorf("uninstall refusal must name removing the held finalizer: %q", msg)
			}
			if strings.Contains(msg, "refusing to install") {
				t.Errorf("uninstall refusal is worded as an install: %q", msg)
			}

			_, _, imsg := installIdentity(tc.hc, priv, s)
			if !strings.HasPrefix(imsg, "refusing to install: "+reason+";") || !strings.Contains(imsg, "submit a spec change through admission") {
				t.Errorf("install refusal wording changed: %q", imsg)
			}
		})
	}
}

func TestRequesterKeyedStampInstallsAndUninstalls(t *testing.T) {
	s := keyringFromSecret(t, fake.NewSimpleClientset(), "k1", map[string][]byte{"k1": testRotationKey(1), "k0": testRotationKey(0)})
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	hc := stampedV2("team-a", "app", "uid-1", info, s, "k1", map[string]any{"chart": "app"})
	priv := []string{"kube-system"}

	imp, refuse, msg := installIdentity(hc, priv, s)
	if refuse || imp == nil || imp.UserName != "alice" || !slices.Equal(imp.Groups, []string{"team-a"}) {
		t.Fatalf("a v2-stamped CR must install impersonated: imp=%+v refuse=%v msg=%s", imp, refuse, msg)
	}
	uimp, urefuse, umsg := uninstallIdentity(hc, priv, s)
	if urefuse || uimp == nil || uimp.UserName != "alice" {
		t.Fatalf("a v2-stamped CR must uninstall impersonated: imp=%+v refuse=%v msg=%s", uimp, urefuse, umsg)
	}

	// a stamp signed entirely under an unretained key fails closed.
	foreign := keyringFromSecret(t, fake.NewSimpleClientset(), "k-gone", map[string][]byte{"k-gone": testRotationKey(9)})
	gone := stampedV2("team-a", "app", "uid-1", info, foreign, "k-gone", map[string]any{"chart": "app"})
	if _, refuse, _ := installIdentity(gone, priv, s); !refuse {
		t.Fatal("a stamp signed under an unretained key must fail closed")
	}
}

func TestHccInstallAllowed(t *testing.T) {
	s := testSigner()
	priv := []string{"kube-system"}
	hc := signedHC("team-a", "app", "uid-1", requesterInfo{Username: "alice"}, s)

	t.Run("no HCC values: allowed", func(t *testing.T) {
		if refuse, _ := hccInstallAllowed(hc, "", nil, requesterInfo{Username: "alice"}, priv, s); refuse {
			t.Error("an absent/empty HCC must not block install")
		}
	})
	t.Run("HCC by same requester: allowed", func(t *testing.T) {
		hcc := signedHC("team-a", "app", "uid-2", requesterInfo{Username: "alice"}, s)
		if refuse, msg := hccInstallAllowed(hc, "replicas: 2", hcc, requesterInfo{Username: "alice"}, priv, s); refuse {
			t.Errorf("same-requester HCC must be allowed: %s", msg)
		}
	})
	t.Run("HCC by a different requester: refused", func(t *testing.T) {
		hcc := signedHC("team-a", "app", "uid-2", requesterInfo{Username: "bob"}, s)
		if refuse, _ := hccInstallAllowed(hc, "replicas: 2", hcc, requesterInfo{Username: "alice"}, priv, s); !refuse {
			t.Error("an HCC authored by a different principal must be refused (escalation via overlay)")
		}
	})
	t.Run("forged HCC stamp: refused", func(t *testing.T) {
		hcc := hcWithAnnotations("team-a", map[string]string{annRequester: "alice", annRequesterSig: "bogus"})
		if refuse, _ := hccInstallAllowed(hc, "replicas: 2", hcc, requesterInfo{Username: "alice"}, priv, s); !refuse {
			t.Error("an HCC with a forged stamp must be refused")
		}
	})
	t.Run("privileged namespace: exempt", func(t *testing.T) {
		phc := hcWithAnnotations("kube-system", nil)
		hcc := signedHC("kube-system", "app", "uid-2", requesterInfo{Username: "bob"}, s)
		if refuse, _ := hccInstallAllowed(phc, "replicas: 2", hcc, requesterInfo{}, priv, s); refuse {
			t.Error("privileged-namespace HCC overlays are exempt (operator identity applies the merge)")
		}
	})
}

func TestReconcileBackstopRefusesUnstampedTenantCR(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata":   map[string]interface{}{"name": "app", "namespace": "team-a", "finalizers": []interface{}{helmFinalizer}},
		"spec":       map[string]interface{}{"chart": "app", "repo": "https://example.invalid"},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"},
		hc.DeepCopy(),
	)
	reconcileHelmChart(context.Background(), dyn, t.TempDir()+"/missing-kubeconfig", hc, "", nil,
		helmPolicy{privilegedNamespaces: []string{"kube-system"}, signer: testSigner()})

	got, err := dyn.Resource(helmChartGVR).Namespace("team-a").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase != hcPhaseFailed {
		t.Fatalf("phase = %q, want %q (fail closed)", phase, hcPhaseFailed)
	}
	msg, _, _ := unstructured.NestedString(got.Object, "status", "message")
	if !strings.Contains(msg, "no requester identity") {
		t.Errorf("message = %q, want the requester backstop message", msg)
	}
}

func TestReconcileBackstopRefusesForgedStamp(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{"name": "app", "namespace": "team-a", "uid": "uid-1",
			"finalizers":  []interface{}{helmFinalizer},
			"annotations": map[string]interface{}{annRequester: "system:masters", annRequesterSig: "forged"}},
		"spec": map[string]interface{}{"chart": "app", "repo": "https://example.invalid"},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"},
		hc.DeepCopy(),
	)
	reconcileHelmChart(context.Background(), dyn, t.TempDir()+"/missing-kubeconfig", hc, "", nil,
		helmPolicy{privilegedNamespaces: []string{"kube-system"}, signer: testSigner()})

	got, _ := dyn.Resource(helmChartGVR).Namespace("team-a").Get(context.Background(), "app", metav1.GetOptions{})
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase != hcPhaseFailed {
		t.Fatalf("phase = %q, want Failed — a forged stamp must be refused", phase)
	}
	msg, _, _ := unstructured.NestedString(got.Object, "status", "message")
	if !strings.Contains(msg, "signature verification") {
		t.Errorf("message = %q, want a signature-verification refusal", msg)
	}
}

func TestReconcileBackstopExemptsPrivilegedNamespace(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata":   map[string]interface{}{"name": "coredns", "namespace": "kube-system"},
		"spec":       map[string]interface{}{"chart": "coredns"},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"},
		hc.DeepCopy(),
	)
	reconcileHelmChart(context.Background(), dyn, t.TempDir()+"/missing-kubeconfig", hc, "", nil,
		helmPolicy{privilegedNamespaces: []string{"kube-system"}, signer: testSigner()})

	got, err := dyn.Resource(helmChartGVR).Namespace("kube-system").Get(context.Background(), "coredns", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinalizer(got, helmFinalizer) {
		t.Error("a privileged-namespace CR must be adopted without a requester stamp (bundled add-on path)")
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase == hcPhaseFailed {
		t.Error("privileged-namespace CR was refused by the backstop; it must be exempt")
	}
}
