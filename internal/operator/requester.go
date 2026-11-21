package operator

// Installs run impersonated so the apiserver evaluates RBAC as the requester, not system:masters.

import (
	"encoding/json"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const (
	// metadata-only writes preserve the previous stamp rather than adopting the controller identity.
	annRequester       = group + "/requester"
	annRequesterUID    = group + "/requester-uid"
	annRequesterGroups = group + "/requester-groups"
)

type requesterInfo struct {
	Username string
	UID      string
	Groups   []string
}

// the complement of cross-namespace privilege: trusted to target others means trusted as operator.
func impersonationRequired(crNS string, privileged []string) bool {
	return !slices.Contains(privileged, crNS)
}

// a malformed groups annotation errors: dropping groups would evaluate RBAC against fewer grants.
func requesterFromCR(hc *unstructured.Unstructured) (info requesterInfo, sig string, ok bool, err error) {
	ann := hc.GetAnnotations()
	user := ann[annRequester]
	if user == "" {
		return requesterInfo{}, "", false, nil
	}
	info = requesterInfo{Username: user, UID: ann[annRequesterUID]}
	if g := ann[annRequesterGroups]; g != "" {
		if e := json.Unmarshal([]byte(g), &info.Groups); e != nil {
			return requesterInfo{}, "", false, fmt.Errorf("malformed %s annotation %q: %w", annRequesterGroups, g, e)
		}
	}
	return info, ann[annRequesterSig], true, nil
}

// only a signature the webhook could have produced is trusted; a nil signer fails closed.
func verifiedRequester(obj *unstructured.Unstructured, signer *requesterSigner) (info requesterInfo, authentic bool, reason string) {
	decoded, sig, ok, err := requesterFromCR(obj)
	if err != nil {
		return requesterInfo{}, false, err.Error()
	}
	if !ok {
		return requesterInfo{}, false, "no requester identity recorded on this object — " +
			"the admission webhook did not stamp it (it may have been created or modified while the webhook was unavailable)"
	}
	if signer == nil {
		return requesterInfo{}, false, "requester-signing key unavailable, cannot verify the stamp"
	}
	ann := obj.GetAnnotations()
	if sigV2 := ann[annRequesterSigV2]; sigV2 != "" && ann[annRequesterKeyID] != "" {
		keyID := ann[annRequesterKeyID]
		key, known := signer.keyByID(keyID)
		if known && signer.verifyV2(bindingV2(obj), decoded, sigV2, key) {
			return decoded, true, ""
		}
		// a pre-migration node's re-stamp refreshes only the legacy signature; it proves the same state.
		if signer.verifyCompatibleLegacy(obj, decoded, sig) {
			return decoded, true, ""
		}
		if !known {
			return requesterInfo{}, false, fmt.Sprintf(
				"requester stamp names signing key %q, which this operator does not hold (key not retained after rotation), and the legacy signature verifies under no retained key — the stamp cannot be verified", keyID)
		}
		return requesterInfo{}, false, "requester stamp failed signature verification: neither the v2 signature under its named key nor the legacy signature matches the object's identity and desired state (forged or tampered)"
	}
	// a pre-migration stamp names no key id: try the whole retained key ring.
	if !signer.verifyCompatibleLegacy(obj, decoded, sig) {
		return requesterInfo{}, false, "requester stamp failed signature verification (forged or tampered)"
	}
	return decoded, true, ""
}

// the real groups must be replayed, or group-scoped RBAC grants would not apply.
func (r requesterInfo) impersonationConfig() rest.ImpersonationConfig {
	return rest.ImpersonationConfig{
		UserName: r.Username,
		UID:      r.UID,
		Groups:   r.Groups,
	}
}

func installIdentity(hc *unstructured.Unstructured, privileged []string, signer *requesterSigner) (imp *rest.ImpersonationConfig, refuse bool, msg string) {
	imp, authentic, reason := requesterIdentity(hc, privileged, signer)
	if !authentic {
		return nil, true, "refusing to install: " + reason + "; submit a spec change through admission to record the requester"
	}
	return imp, false, ""
}

// same decision as installIdentity; the wording offers the finalizer as the way out.
func uninstallIdentity(hc *unstructured.Unstructured, privileged []string, signer *requesterSigner) (*rest.ImpersonationConfig, bool, string) {
	imp, authentic, reason := requesterIdentity(hc, privileged, signer)
	if !authentic {
		return nil, true, "refusing to uninstall: " + reason + "; submit a spec change through admission to re-stamp the requester, or remove the finalizer by hand, which leaves the release installed"
	}
	return imp, false, ""
}

func requesterIdentity(hc *unstructured.Unstructured, privileged []string, signer *requesterSigner) (imp *rest.ImpersonationConfig, authentic bool, reason string) {
	if !impersonationRequired(hc.GetNamespace(), privileged) {
		return nil, true, ""
	}
	info, authentic, reason := verifiedRequester(hc, signer)
	if !authentic {
		return nil, false, reason
	}
	cfg := info.impersonationConfig()
	return &cfg, true, ""
}

// an overlay by another principal would apply values under the chart owner's authority.
func hccInstallAllowed(hc *unstructured.Unstructured, hccValues string, hcc *unstructured.Unstructured, hcRequester requesterInfo, privileged []string, signer *requesterSigner) (refuse bool, msg string) {
	if hccValues == "" || hcc == nil {
		return false, ""
	}
	if !impersonationRequired(hc.GetNamespace(), privileged) {
		return false, ""
	}
	hccInfo, authentic, reason := verifiedRequester(hcc, signer)
	if !authentic {
		return true, "refusing to install: the HelmChartConfig overlay carries no authentic requester stamp (" + reason + ") — submit a spec change through admission"
	}
	if hccInfo.Username != hcRequester.Username || hccInfo.UID != hcRequester.UID || !slices.Equal(canonicalGroups(hccInfo.Groups), canonicalGroups(hcRequester.Groups)) {
		return true, fmt.Sprintf("refusing to install: the HelmChartConfig overlay was authored by %q but the HelmChart's requester is %q — an overlay applied under another principal's identity would defeat requester-scoped authorization; both must be the same requester", hccInfo.Username, hcRequester.Username)
	}
	return false, ""
}
