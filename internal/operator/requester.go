package operator

// requester.go — requester-scoped HelmChart installs (the second tier of HelmChart
// containment).
//
// The first tier confined a HelmChart's release NAMESPACE to the CR's own
// namespace unless the CR lives in a privileged namespace. That bounds the blast
// radius but cannot stop a chart's own templates from rendering a cluster-scoped
// object (a ClusterRoleBinding) or an in-namespace RBAC object that escalates the
// creator's own access — because the operator applies every rendered object with
// its cluster-admin / system:masters identity, so the apiserver authorizer never
// sees the actual requester.
//
// This closes that: a mutating admission webhook (webhook.go) stamps the creating
// user onto the CR at admission, and the reconciler installs through an
// IMPERSONATING client (impersonationConfig, wired into newActionConfig) so the
// apiserver evaluates every applied object's RBAC as the requester. Impersonation
// REPLACES the effective identity for authorization, so the operator's
// system:masters no longer short-circuits the check.
//
// Reconciliation also fails closed on an absent or invalid stamp. Privileged
// namespaces retain operator authority; all other installs and cleanup require
// an authenticated requester. Admission itself uses failurePolicy: Fail.

import (
	"encoding/json"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const (
	// Admission records the writer of the desired spec; metadata-only writes
	// preserve the previous stamp rather than adopting the controller identity.
	annRequester       = group + "/requester"        // username
	annRequesterUID    = group + "/requester-uid"    // uid (may be empty for some authenticators)
	annRequesterGroups = group + "/requester-groups" // JSON array of group names
)

// requesterInfo is the identity captured at admission, decoded from the CR's
// stamped annotations.
type requesterInfo struct {
	Username string
	UID      string
	Groups   []string
}

// impersonationRequired reports whether a HelmChart in crNS must install with the
// requester's authority (true) or may install as the operator (false, for CRs in
// an operator-configured privileged namespace — the bundled add-ons). It is the
// exact complement of the cross-namespace privilege: a namespace trusted
// to target other namespaces is trusted to install as the operator.
func impersonationRequired(crNS string, privileged []string) bool {
	return !slices.Contains(privileged, crNS)
}

// requesterFromCR decodes the stamped requester identity + signature from a CR's
// annotations (a pure decode; authenticity is checked by verifiedRequester). ok is
// false when no requester username is recorded. A malformed groups annotation is
// an error rather than a silent empty group set, because dropping the requester's
// groups would evaluate RBAC against fewer grants than the requester actually
// holds — a subtle, wrong, and hard-to-notice authorization result.
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

// verifiedRequester decodes the requester stamp AND verifies its webhook-authored
// HMAC signature, bound to resource identity and desired spec. authentic is true only for a present,
// well-formed, correctly-signed stamp — so an unstamped, malformed, forged, or
// tampered stamp all come back authentic=false with a reason. A nil signer (the
// key could not be loaded) verifies nothing and so fails closed. This is what
// makes the stamp's PRESENCE meaningless on its own: only a signature the webhook
// could have produced (with the cluster HMAC key tenants cannot read) is trusted.
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
	// A migrated stamp names the key that produced its v2 signature. When the
	// v2 signature verifies it authenticates the admitted field policy and every
	// admitted desired-state value under a named retained key. Only later fields
	// equal to their schema defaults are outside that bound state.
	if sigV2 := ann[annRequesterSigV2]; sigV2 != "" && ann[annRequesterKeyID] != "" {
		keyID := ann[annRequesterKeyID]
		key, known := signer.keyByID(keyID)
		if known && signer.verifyV2(bindingV2(obj), decoded, sigV2, key) {
			return decoded, true, ""
		}
		// Fallback for the mixed-version window: each control-plane node serves
		// admission from its own webhook endpoint, and a pre-migration node's
		// re-stamp refreshes only the legacy annotations, leaving the v2
		// signature over an older binding beside it. The legacy MAC over the
		// CURRENT binding is unforgeable under a retained key and proves the
		// same identity and desired state the v2 stamp would have, so it
		// authenticates the object; a genuinely tampered stamp fails both
		// signatures and is refused below.
		if signer.verifyCompatibleLegacy(obj, decoded, sig) {
			return decoded, true, ""
		}
		if !known {
			return requesterInfo{}, false, fmt.Sprintf(
				"requester stamp names signing key %q, which this operator does not hold (key not retained after rotation), and the legacy signature verifies under no retained key — the stamp cannot be verified", keyID)
		}
		return requesterInfo{}, false, "requester stamp failed signature verification: neither the v2 signature under its named key nor the legacy signature matches the object's identity and desired state (forged or tampered)"
	}
	// Legacy (pre-migration) stamp: it names no key id, so try the whole
	// retained key ring.
	if !signer.verifyCompatibleLegacy(obj, decoded, sig) {
		return requesterInfo{}, false, "requester stamp failed signature verification (forged or tampered)"
	}
	return decoded, true, ""
}

// impersonationConfig builds the rest.Config impersonation settings for a
// requester. The group set MUST carry the requester's real groups: RBAC grants
// bound to a group (a RoleBinding to system:authenticated, or a team group) only
// apply when the impersonated identity claims that group, so replaying the
// stamped groups is what makes group-scoped authorization behave as it would for
// the requester making the request directly.
func (r requesterInfo) impersonationConfig() rest.ImpersonationConfig {
	return rest.ImpersonationConfig{
		UserName: r.Username,
		UID:      r.UID,
		Groups:   r.Groups,
	}
}

// installIdentity decides the identity a HelmChart's install/upgrade must run as.
// A privileged-namespace CR installs as the operator (imp == nil), exactly as
// before requester-scoped installs — that is how the bundled add-ons keep working. Every other CR
// must carry an AUTHENTIC (webhook-signed) requester stamp and installs
// impersonated; a missing, malformed, or forged stamp is a FAIL-CLOSED refusal
// (refuse == true). Existing unsigned resources must acquire a new stamp through
// a spec-changing admission before they can be installed.
func installIdentity(hc *unstructured.Unstructured, privileged []string, signer *requesterSigner) (imp *rest.ImpersonationConfig, refuse bool, msg string) {
	if !impersonationRequired(hc.GetNamespace(), privileged) {
		return nil, false, ""
	}
	info, authentic, reason := verifiedRequester(hc, signer)
	if !authentic {
		return nil, true, "refusing to install: " + reason + "; submit a spec change through admission to record the requester"
	}
	cfg := info.impersonationConfig()
	return &cfg, false, ""
}

// uninstallIdentity never elevates an unverified tenant to operator authority.
// Keep the finalizer until an administrator repairs ownership or cleans up.
func uninstallIdentity(hc *unstructured.Unstructured, privileged []string, signer *requesterSigner) (*rest.ImpersonationConfig, bool, string) {
	return installIdentity(hc, privileged, signer)
}

// hccInstallAllowed guards the HelmChartConfig overlay path. A same-name HCC's
// valuesContent is merged into the release, which installs under the HelmChart's
// requester identity — so an HCC authored by a DIFFERENT principal would let that
// principal apply values under the chart owner's authority, defeating
// requester-scoped authorization. When an HCC contributes values to a
// non-privileged chart, its stamp must therefore be authentic AND name the SAME
// requester as the HelmChart. An HCC that contributes nothing (empty
// valuesContent) is not checked. Privileged namespaces are exempt (operator
// identity applies the whole merge anyway).
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
