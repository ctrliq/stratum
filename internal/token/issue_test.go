package token

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// issueRecorder records the calls the Issue fakes make, so the tests can pin
// the sequence and the arguments the owner passes between the steps.
type issueRecorder struct {
	calls     []string
	revoked   []string
	gotURL    string
	gotTTL    time.Duration
	gotCP     bool
	owner     *metav1.OwnerReference
	uploadKey string
}

// okIssueDeps returns IssueDeps whose every step succeeds, recording as it
// goes. The fakes mirror the seams the two front ends inject today — tokenCmd
// (pkg/cli/token.go) and tokenDeps (internal/mcp/stratumwrite.go) — because
// Issue replaces the sequence both of them re-implement by hand.
func okIssueDeps(rec *issueRecorder) IssueDeps {
	owner := &metav1.OwnerReference{Kind: "Secret", Name: "bootstrap-token-abcdef"}
	return IssueDeps{
		Create: func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			rec.calls = append(rec.calls, "create")
			rec.gotURL, rec.gotTTL, rec.gotCP = serverURL, ttl, controlPlane
			return "abcdef.0123456789abcdef", "sha256:aa", owner, nil
		},
		NewCertKey: func() (string, error) {
			rec.calls = append(rec.calls, "newCertKey")
			return "deadbeef", nil
		},
		UploadCerts: func(certKey string, o *metav1.OwnerReference, confirm func(context.Context) error) error {
			rec.calls = append(rec.calls, "uploadCerts")
			rec.uploadKey, rec.owner = certKey, o
			return nil
		},
		Revoke: func(tok string, cause error) error {
			rec.calls = append(rec.calls, "revoke")
			rec.revoked = append(rec.revoked, tok)
			// Behaves like RevokeOnFailure: a failed cleanup folds a WARNING
			// into the error, and that warning is the only thing that ever
			// tells an operator a live credential is stranded.
			return fmt.Errorf("%w\n\nWARNING: could not remove bootstrap token %s", cause, tok)
		},
	}
}

// handover returns a handover callback that records the grant it received and
// answers with the given delivered/err pair.
func handover(rec *issueRecorder, delivered bool, err error, got *Grant) func(Grant) (bool, error) {
	return func(g Grant) (bool, error) {
		rec.calls = append(rec.calls, "handover")
		if got != nil {
			*got = g
		}
		return delivered, err
	}
}

// TestIssue_HandsOverTheGrant pins the sequence and the grant: Issue mints via
// deps.Create with exactly the server URL, ttl and control-plane shape it was
// given, then — for a control-plane token only — generates the certificate key
// and uploads the shared certificates under the mint's owner reference (so the
// uploaded material is garbage-collected with the token), then hands the
// assembled grant to the caller's handover callback exactly once and returns
// it. The join flags travel as data inside the grant, so a front end cannot
// print or return a grant that omits them — the omission that once kept all
// four flags off token_create.
func TestIssue_HandsOverTheGrant(t *testing.T) {
	flags := JoinFlags{ServiceCIDR: "10.43.0.0/16", Profile: "cis"}
	for _, tc := range []struct {
		name         string
		controlPlane bool
		wantCalls    []string
		wantGrant    Grant
	}{
		{
			name:         "a worker token skips the certificate phase",
			controlPlane: false,
			wantCalls:    []string{"create", "handover"},
			wantGrant: Grant{
				ServerURL: "https://cp:6443", Token: "abcdef.0123456789abcdef",
				CACertHash: "sha256:aa", ExpiresIn: 24 * time.Hour, JoinFlags: flags,
			},
		},
		{
			name:         "a control-plane token uploads the shared certs before the handover",
			controlPlane: true,
			wantCalls:    []string{"create", "newCertKey", "uploadCerts", "handover"},
			wantGrant: Grant{
				ServerURL: "https://cp:6443", Token: "abcdef.0123456789abcdef",
				CACertHash: "sha256:aa", ExpiresIn: 24 * time.Hour,
				CertKey: "deadbeef", JoinFlags: flags,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec issueRecorder
			var handedOver Grant
			grant, err := Issue(io.Discard, okIssueDeps(&rec), "https://cp:6443", 24*time.Hour, tc.controlPlane, flags,
				handover(&rec, true, nil, &handedOver))
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if !slices.Equal(rec.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", rec.calls, tc.wantCalls)
			}
			if rec.gotURL != "https://cp:6443" || rec.gotTTL != 24*time.Hour || rec.gotCP != tc.controlPlane {
				t.Errorf("create was asked for (%q, %s, controlPlane=%v), want the request Issue was given",
					rec.gotURL, rec.gotTTL, rec.gotCP)
			}
			if handedOver != tc.wantGrant {
				t.Errorf("the handover received %+v, want the assembled grant %+v", handedOver, tc.wantGrant)
			}
			if grant != tc.wantGrant {
				t.Errorf("Issue returned %+v, want the same grant it handed over", grant)
			}
			if tc.controlPlane {
				if rec.uploadKey != "deadbeef" {
					t.Errorf("UploadCerts was called with key %q, want the generated one", rec.uploadKey)
				}
				if rec.owner == nil || rec.owner.Name != "bootstrap-token-abcdef" {
					t.Errorf("UploadCerts was called with owner %v, want the mint token's owner reference", rec.owner)
				}
			}
		})
	}
}

// TestIssue_RevokesWhenNothingWasDelivered pins the rollback half of the
// ownership rule: while nothing has been handed over, a failing step takes the
// minted token back — exactly once, with the failure as the cause and the
// cleanup warning carried to the caller, because nothing else will ever remove
// a credential the rollback could not delete. This is the guarantee both front
// ends re-implemented by hand: pkg/cli's createToken with its deferred revoke,
// and the MCP handler with a per-step deps.revoke.
//
// It also pins the degenerate report — a handover that claims it delivered
// nothing and reports no error. Not reporting success means the caller gave up
// the grant, so the owner revokes it with a cause of its own rather than
// leaving a live credential behind a call that handed over nothing.
func TestIssue_RevokesWhenNothingWasDelivered(t *testing.T) {
	boom := errors.New("apiserver said no")
	for _, tc := range []struct {
		name               string
		breakStep          func(IssueDeps) IssueDeps // nil = leave the steps intact
		handoverDelivered  bool
		handoverErr        error
		wantHandoverCalled bool
	}{
		{
			name: "certificate-key generation fails",
			breakStep: func(d IssueDeps) IssueDeps {
				d.NewCertKey = func() (string, error) { return "", boom }
				return d
			},
		},
		{
			name: "the shared-certificate upload fails",
			breakStep: func(d IssueDeps) IssueDeps {
				d.UploadCerts = func(string, *metav1.OwnerReference, func(context.Context) error) error { return boom }
				return d
			},
		},
		{
			name:               "the handover reports a failure and no delivery",
			handoverDelivered:  false,
			handoverErr:        boom,
			wantHandoverCalled: true,
		},
		{
			name:               "the handover reports neither delivery nor error",
			handoverDelivered:  false,
			handoverErr:        nil,
			wantHandoverCalled: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec issueRecorder
			deps := okIssueDeps(&rec)
			if tc.breakStep != nil {
				deps = tc.breakStep(deps)
			}
			grant, err := Issue(io.Discard, deps, "https://cp:6443", 24*time.Hour, true, JoinFlags{},
				handover(&rec, tc.handoverDelivered, tc.handoverErr, nil))
			if err == nil {
				t.Fatal("the failure was not reported")
			}
			if tc.handoverErr != nil && !errors.Is(err, tc.handoverErr) {
				t.Errorf("error %v does not carry the cause", err)
			}
			if !strings.Contains(err.Error(), "WARNING") {
				t.Errorf("the revoke's cleanup warning did not reach the caller: %v", err)
			}
			if !slices.Equal(rec.revoked, []string{"abcdef.0123456789abcdef"}) {
				t.Errorf("revoked = %v, want the minted token revoked exactly once", rec.revoked)
			}
			if grant != (Grant{}) {
				t.Errorf("an error from Issue returned a grant: %+v", grant)
			}
			if got := slices.Contains(rec.calls, "handover"); got != tc.wantHandoverCalled {
				t.Errorf("handover called = %v, want %v", got, tc.wantHandoverCalled)
			}
		})
	}
}

// TestIssue_NeverRevokesAfterDelivery pins the shared owner's partial-delivery
// rule: once the handover reports that credential bytes were delivered, Issue
// must not revoke the token as if nothing was handed over — even when the same
// handover also reports a failure, which is a join command only partially
// written or a broken pipe after the token reached the terminal. Revoking a
// credential the caller may already hold would trade a stranded credential for
// output that lies. The owner instead returns the error and leaves the
// judgement — and any cleanup advice about what was exposed — to the caller,
// who knows what was delivered.
func TestIssue_NeverRevokesAfterDelivery(t *testing.T) {
	boom := errors.New("output failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"delivered cleanly", nil},
		{"delivered, then the output failed", boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec issueRecorder
			grant, err := Issue(io.Discard, okIssueDeps(&rec), "https://cp:6443", 24*time.Hour, true, JoinFlags{},
				handover(&rec, true, tc.err, nil))
			if len(rec.revoked) != 0 {
				t.Errorf("a delivered grant was revoked: %v", rec.revoked)
			}
			if tc.err == nil {
				if err != nil {
					t.Fatalf("a delivered grant failed the call: %v", err)
				}
				if grant.Token != "abcdef.0123456789abcdef" {
					t.Errorf("Issue returned %+v, want the delivered grant", grant)
				}
				return
			}
			if !errors.Is(err, boom) {
				t.Errorf("the delivery failure was not reported: %v", err)
			}
			if grant != (Grant{}) {
				t.Errorf("an error from Issue returned a grant: %+v", grant)
			}
		})
	}
}

// TestIssue_MintFailureRevokesNothing mirrors the MCP pin
// TestTokenCreate_MintFailureRevokesNothing at the owner. When the mint itself
// fails, no token exists: token.Create already rolls its own Secret back, so a
// revoke here would run that rollback twice — and would call back a credential
// the mint never created.
func TestIssue_MintFailureRevokesNothing(t *testing.T) {
	boom := errors.New("mint failed")
	var rec issueRecorder
	deps := okIssueDeps(&rec)
	deps.Create = func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
		rec.calls = append(rec.calls, "create")
		return "", "", nil, boom
	}

	grant, err := Issue(io.Discard, deps, "https://cp:6443", 24*time.Hour, true, JoinFlags{},
		handover(&rec, true, nil, nil))
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not carry the cause", err)
	}
	if len(rec.revoked) != 0 {
		t.Errorf("revoked %v after a mint that created nothing", rec.revoked)
	}
	if !slices.Equal(rec.calls, []string{"create"}) {
		t.Errorf("calls = %v, want only the failed mint (no certificate phase, no handover, no revoke)", rec.calls)
	}
	if grant != (Grant{}) {
		t.Errorf("an error from Issue returned a grant: %+v", grant)
	}
}

func TestIssueGuardRefusesBeforeMintOrPublication(t *testing.T) {
	var rec issueRecorder
	deps := okIssueDeps(&rec)
	refusal := errors.New("rotation unfinished")
	deps.Confirm = func() error { rec.calls = append(rec.calls, "confirm"); return nil }
	deps.RekeyGuard = func() error { rec.calls = append(rec.calls, "guard"); return refusal }
	grant, err := Issue(io.Discard, deps, "https://cp:6443", time.Hour, true, JoinFlags{}, handover(&rec, true, nil, nil))
	if !errors.Is(err, refusal) || grant != (Grant{}) {
		t.Fatalf("grant=%+v err=%v; want the guard refusal without credentials", grant, err)
	}
	// Create also republishes cluster-info. Revoking afterwards cannot undo
	// replacing a staged controller's dual-root bundle with local old trust.
	if !slices.Equal(rec.calls, []string{"confirm", "guard"}) {
		t.Fatalf("effects=%v; refusal must precede mint, publication and rollback", rec.calls)
	}
}

// TestIssueWorkerGuardRefusesBeforeMintOrPublication is the worker-side twin
// of the pin above, and the regression for worker issuance from an untouched
// controller during an unfinished rotation: token.Create republishes
// cluster-info and the kubelet client CA from local PKI for a worker token
// too, so a mint from a controller the rotation has not reached would replace
// the published old‖new bundle with old-only trust and strand every joiner
// pinned to the staged CA. The guard must refuse before the mint, and a
// refusal revokes nothing — no token exists.
func TestIssueWorkerGuardRefusesBeforeMintOrPublication(t *testing.T) {
	var rec issueRecorder
	deps := okIssueDeps(&rec)
	refusal := errors.New("rotation unfinished")
	deps.Confirm = func() error { rec.calls = append(rec.calls, "confirm"); return nil }
	deps.RekeyGuard = func() error { rec.calls = append(rec.calls, "guard"); return refusal }
	grant, err := Issue(io.Discard, deps, "https://cp:6443", time.Hour, false, JoinFlags{}, handover(&rec, true, nil, nil))
	if !errors.Is(err, refusal) || grant != (Grant{}) {
		t.Fatalf("grant=%+v err=%v; want the guard refusal without credentials", grant, err)
	}
	if !slices.Equal(rec.calls, []string{"confirm", "guard"}) {
		t.Fatalf("effects=%v; the refusal must precede the worker mint and its cluster-info publication", rec.calls)
	}
}

// TestIssueWorkerMintRunsTheGuardFirst pins the worker path's ordering on
// success: the guard's cluster-wide read and the fence re-check after it
// precede the mint, so a rotation staged between the fence and the guard
// still refuses, and one staged while the guard reads cannot be overtaken by
// the publication.
func TestIssueWorkerMintRunsTheGuardFirst(t *testing.T) {
	var rec issueRecorder
	deps := okIssueDeps(&rec)
	deps.Confirm = func() error { rec.calls = append(rec.calls, "confirm"); return nil }
	deps.RekeyGuard = func() error { rec.calls = append(rec.calls, "guard"); return nil }
	if _, err := Issue(io.Discard, deps, "https://cp:6443", time.Hour, false, JoinFlags{},
		handover(&rec, true, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.calls, []string{"confirm", "guard", "confirm", "create", "confirm", "handover"}) {
		t.Errorf("calls=%v; the guard must precede the worker mint, with the fence re-checked after it", rec.calls)
	}
}
