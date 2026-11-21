package token

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Travels to the handover callback as data, so a grant cannot omit a piece.
type Grant struct {
	ServerURL  string
	Token      string // "<id>.<secret>" bootstrap token
	CACertHash string // the "sha256:<hex>" pin the joiner verifies the control plane against
	ExpiresIn  time.Duration
	// Control-plane only; how it travels is each front end's handover decision.
	CertKey   string
	JoinFlags JoinFlags
}

// Injectable so the credential lifecycle is testable without a live apiserver.
type IssueDeps struct {
	// Rechecks an acquired maintenance lease before the next mutation.
	Confirm func() error
	// Refuses publication while a rotation remains unfinished.
	RekeyGuard func() error

	// Mints the token; the confirm argument runs immediately before each write.
	Create func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (token, caHash string, owner *metav1.OwnerReference, err error)
	// Control-plane only; UploadCerts publishes under the mint's owner reference.
	NewCertKey  func() (string, error)
	UploadCerts func(certKey string, owner *metav1.OwnerReference, confirm func(context.Context) error) error
	// Folds a warning into cause; that warning is all an operator ever sees of a stranded credential.
	Revoke func(tok string, cause error) error
}

// Owns the credential until the handover reports delivery; never revokes a delivered token.
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
	// Revocation cannot restore trust a stale controller would overwrite; refuse before either write.
	if deps.RekeyGuard != nil {
		if err := deps.RekeyGuard(); err != nil {
			return Grant{}, err
		}
		if err := confirm(); err != nil {
			return Grant{}, err
		}
	}
	// Run before each write, so a stalled call cannot publish stale trust.
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
		// Nothing was handed over; the revoke's warning reaches the caller.
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
