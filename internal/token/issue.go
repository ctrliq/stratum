// The token-issuing owner: one function for the mint → shared-certificate
// upload → handover → rollback sequence that the CLI command and the MCP
// token_create tool each re-implemented by hand, with the ownership rule the
// handover defines: the credential belongs to the issuer until it has been
// delivered, and never comes back afterwards.

package token

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Grant is a minted join credential together with everything a joining node
// needs alongside it. It travels to the caller's handover callback as data,
// so a front end cannot hand over a grant that omits a piece — the omission
// that once kept all four join flags off token_create.
type Grant struct {
	ServerURL  string // the apiserver the joiner sends the credential to
	Token      string // "<id>.<secret>" bootstrap token
	CACertHash string // the "sha256:<hex>" pin the joiner verifies the control plane against
	ExpiresIn  time.Duration
	// CertKey is the shared-certificate decryption key, present only for a
	// control-plane token. It is a second, more sensitive credential; how it
	// travels is each front end's handover decision (the CLI writes it to a
	// 0600 file and hands over the path; the MCP tool returns it in its
	// result).
	CertKey   string
	JoinFlags JoinFlags
}

// IssueDeps are the cluster effects Issue performs, injectable so the
// credential lifecycle is testable without a live apiserver. They mirror the
// seams the two front ends inject today — pkg/cli's tokenCmd and the MCP
// handler's tokenDeps — because Issue replaces the sequence both of them
// re-implemented by hand.
type IssueDeps struct {
	// Confirm rechecks an acquired maintenance lease before the next mutation.
	// RekeyGuard refuses publication while another rotation remains unfinished.
	Confirm    func() error
	RekeyGuard func() error

	// Create mints the bootstrap token: the Secret, its RBAC and the
	// cluster-info publication. It takes the writer Issue was handed, so the
	// mint's signing-wait progress reaches the caller's stream, and returns
	// the token string, the CA pin and the token Secret's owner reference,
	// which the shared-certificate upload uses so the uploaded material is
	// garbage-collected with the token. It takes the same maintenance-lease
	// recheck Confirm carries, and runs it immediately before each write: a
	// lease lost while an earlier call stalled refuses the next publication
	// instead of overwriting the successor's trust.
	Create func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (token, caHash string, owner *metav1.OwnerReference, err error)
	// NewCertKey generates the shared-certificate decryption key, and
	// UploadCerts publishes the shared certificates under the mint's owner
	// reference, rechecking the lease immediately before the Secret write.
	// Both run for a control-plane token only.
	NewCertKey  func() (string, error)
	UploadCerts func(certKey string, owner *metav1.OwnerReference, confirm func(context.Context) error) error
	// Revoke takes back a minted token because the issuance is failing, and
	// returns cause — folding in a warning when the credential could not be
	// removed, because that warning is the only thing that ever tells an
	// operator a live credential is stranded.
	Revoke func(tok string, cause error) error
}

// Issue mints a join token and owns it until it has been handed over. The
// writer leads: it travels into deps.Create, so the mint's progress reaches
// the caller's stream (the CLI's os.Stdout, the MCP handler's buffer) instead
// of printing to one the caller does not control.
//
// The sequence: deps.Create mints the credential; for a control-plane token
// the certificate key is generated and the shared certificates are uploaded
// under the mint's owner reference (so the uploaded material is
// garbage-collected with the token); then the assembled grant goes to the
// caller's handover callback exactly once, which delivers it to whoever asked
// for it and reports whether the credential bytes actually arrived.
//
// The ownership rule the handover defines:
//
//   - While nothing has been delivered, a failing step takes the minted token
//     back — the caller never saw it, so a live token behind a reported
//     failure would be a stranded credential nobody can name to revoke.
//   - Once the handover reports delivery, the token is the recipient's, and
//     Issue never revokes it — not even when the same handover also reports a
//     failure, which is a join command only partially written or a broken
//     pipe after the token reached the terminal. Revoking a credential the
//     caller may already hold would trade a stranded credential for output
//     that lies; the error is returned instead, and the judgement about what
//     was exposed stays with the front end, which knows what was delivered.
//   - A handover that reports neither delivery nor an error gave up the
//     grant, so the token is revoked with a cause of its own rather than left
//     behind a call that handed over nothing.
//   - A failed mint revokes nothing: no token exists, and token.Create
//     already rolls its own Secret back — a revoke here would run that
//     rollback twice and call back a credential that was never created.
//
// On success it returns the same grant it handed over. On failure it returns
// a zero grant and the cause, carried through the revoke's warning when one
// ran.
func Issue(w io.Writer, deps IssueDeps, serverURL string, ttl time.Duration, controlPlane bool, flags JoinFlags, handover func(Grant) (bool, error)) (Grant, error) {
	confirm := func() error {
		if deps.Confirm != nil {
			return deps.Confirm()
		}
		return nil
	}
	if err := confirm(); err != nil {
		return Grant{}, err
	}
	// Create republishes cluster-info and the kubelet client CA from local
	// PKI as well as minting a token — for a worker token too. Refuse before
	// either mutation: revocation cannot restore trust overwritten by a
	// controller that has not joined the active rotation, and a worker mint
	// from one would replace the published old‖new bundle with old-only
	// trust, stranding every joiner pinned to the staged CA.
	if deps.RekeyGuard != nil {
		if err := deps.RekeyGuard(); err != nil {
			return Grant{}, err
		}
		if err := confirm(); err != nil {
			return Grant{}, err
		}
	}
	// The lease recheck the publications consult: Confirm is the acquired
	// fence's owner check, and the helpers run it immediately before each
	// ConfigMap or Secret write, so a lease lost while an earlier call
	// stalled refuses the next publication.
	confirmCtx := func(_ context.Context) error { return confirm() }
	tok, hash, owner, err := deps.Create(w, serverURL, ttl, controlPlane, confirmCtx)
	if err != nil {
		return Grant{}, fmt.Errorf("mint join token: %w", err)
	}
	grant := Grant{
		ServerURL:  serverURL,
		Token:      tok,
		CACertHash: hash,
		ExpiresIn:  ttl,
		JoinFlags:  flags,
	}
	if controlPlane {
		if err := confirm(); err != nil {
			return Grant{}, deps.Revoke(tok, err)
		}
		certKey, err := deps.NewCertKey()
		if err != nil {
			return Grant{}, deps.Revoke(tok, fmt.Errorf("generate the control-plane certificate key: %w", err))
		}
		if err := confirm(); err != nil {
			return Grant{}, deps.Revoke(tok, err)
		}
		if err := deps.UploadCerts(certKey, owner, confirmCtx); err != nil {
			return Grant{}, deps.Revoke(tok, fmt.Errorf("upload the shared certificates: %w", err))
		}
		grant.CertKey = certKey
	}
	if err := confirm(); err != nil {
		return Grant{}, deps.Revoke(tok, err)
	}
	delivered, herr := handover(grant)
	switch {
	case herr != nil && !delivered:
		// Nothing was handed over, so the rollback is unambiguously right; its
		// warning is what reaches the caller when the credential could not be
		// removed.
		return Grant{}, deps.Revoke(tok, herr)
	case herr != nil:
		// Delivered, then failed: report the failure and keep the credential.
		return Grant{}, herr
	case !delivered:
		// Not reporting success means the caller gave up the grant.
		return Grant{}, deps.Revoke(tok, errors.New("the handover reported no credential was delivered"))
	}
	return grant, nil
}
