package token

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// fakeSecrets is a hand-rolled secretAPI, for the same reason fakeConfigMaps
// exists instead of client-go's fake clientset: this package adds no new
// modules, test-only ones included.
type fakeSecrets struct {
	created   []string
	deleted   []string
	existing  bool  // Create reports AlreadyExists
	createErr error // Create fails with this
	deleteErr error // Delete fails with this
	// hangUntilCtxDone models a wedged apiserver: Delete blocks until the caller's
	// context is done.
	hangUntilCtxDone bool
	// stored is what a Get returns, modelling server-side state. An AMBIGUOUS
	// create — committed by the apiserver, surfaced to the client as an error —
	// is createErr set AND stored populated.
	stored   *corev1.Secret
	getErr   error
	getCalls int
	// uid, when set, is stamped on the Secret Create returns, modelling the UID the
	// apiserver assigns — so the ownerRef path can be exercised.
	uid string
	// lastCreated is the object handed to the most recent Create, so a test can
	// assert on the StringData (e.g. the expiration key) it minted.
	lastCreated *corev1.Secret
}

func (f *fakeSecrets) Create(_ context.Context, s *corev1.Secret, _ metav1.CreateOptions) (*corev1.Secret, error) {
	f.lastCreated = s
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.existing {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, s.Name)
	}
	f.created = append(f.created, s.Name)
	out := s.DeepCopy()
	out.UID = types.UID(f.uid)
	return out, nil
}

func (f *fakeSecrets) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Secret, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.stored == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return f.stored.DeepCopy(), nil
}

func (f *fakeSecrets) Delete(ctx context.Context, name string, _ metav1.DeleteOptions) error {
	if f.hangUntilCtxDone {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, name)
	return nil
}

func okSteps() tokenSteps {
	return tokenSteps{
		ensureRBAC:  func(context.Context) error { return nil },
		narrowCerts: func(context.Context) error { return nil },
		ensureInfo:  func(context.Context) error { return nil },
		waitForSig:  func(context.Context, string) error { return nil },
		caCertHash:  func() (string, error) { return "sha256:abc", nil },
	}
}

const (
	mintID     = "abcdef"
	mintSecret = "0123456789abcdef"
	mintName   = "bootstrap-token-abcdef"
)

// TestCreateTokenRevokesTheCredentialOnPostMintFailure is a regression test for the
// signed cluster-info work.
//
// Bootstrap tokens never expire and tokencleaner is not running on the clusters
// where the signing post-condition fires, so a failed `stratum token create` used
// to leave a live join credential in kube-system that the operator was never told
// about — the command had reported failure. The documented migration is "run,
// fail, restart the controller-manager, re-run", so they accumulate.
//
// Three of these four subtests would also fail on code that predates cluster-info
// signing: the RBAC, cluster-info and CA-hash paths could already fail after the
// mint. Signing added the fourth and made it routine rather than fault-only.
//
// Every post-mint step is covered, not just the one that was reported: the
// guarantee is "an error returns no credential", not "this particular step
// cleans up".
func TestCreateTokenRevokesTheCredentialOnPostMintFailure(t *testing.T) {
	boom := errors.New("boom")
	steps := map[string]func(tokenSteps) tokenSteps{
		"ensureRBAC": func(s tokenSteps) tokenSteps {
			s.ensureRBAC = func(context.Context) error { return boom }
			return s
		},
		"narrowCerts": func(s tokenSteps) tokenSteps {
			s.narrowCerts = func(context.Context) error { return boom }
			return s
		},
		"ensureClusterInfo": func(s tokenSteps) tokenSteps {
			s.ensureInfo = func(context.Context) error { return boom }
			return s
		},
		"waitForClusterInfoSignature": func(s tokenSteps) tokenSteps {
			s.waitForSig = func(context.Context, string) error { return boom }
			return s
		},
		"CACertHash": func(s tokenSteps) tokenSteps {
			s.caCertHash = func() (string, error) { return "", boom }
			return s
		},
	}
	for name, mut := range steps {
		t.Run(name+" fails", func(t *testing.T) {
			f := &fakeSecrets{}
			tok, hash, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, mut(okSteps()))
			if err == nil {
				t.Fatal("no error returned")
			}
			if !errors.Is(err, boom) {
				t.Errorf("original cause lost: %v", err)
			}
			if tok != "" || hash != "" {
				t.Errorf("returned token %q / hash %q alongside an error", tok, hash)
			}
			if len(f.created) != 1 {
				t.Fatalf("created = %v, want the secret to have been minted", f.created)
			}
			if got := f.deleted; len(got) != 1 || got[0] != mintName {
				t.Errorf("deleted = %v, want [%s]: a failed mint must leave no live credential", got, mintName)
			}
		})
	}
}

// TestCreateTokenNarrowsTheCertsReaderGrant: every mint, worker or control
// plane, drops a worker grant on stratum-certs left by an earlier release, so an
// upgraded cluster that only mints worker tokens still closes it.
func TestCreateTokenNarrowsTheCertsReaderGrant(t *testing.T) {
	for _, controlPlane := range []bool{false, true} {
		narrowed := false
		s := okSteps()
		s.narrowCerts = func(context.Context) error { narrowed = true; return nil }
		if _, _, _, err := createToken(context.Background(), &fakeSecrets{}, mintID, mintSecret, DefaultTokenTTL, controlPlane, s); err != nil {
			t.Fatal(err)
		}
		if !narrowed {
			t.Errorf("controlPlane=%v: the mint did not narrow the stratum-certs read grant", controlPlane)
		}
	}
}

// TestCreateTokenKeepsTheCredentialOnSuccess is the counterweight — cleanup that
// also fires on success would be a worse bug than the one it fixes.
func TestCreateTokenKeepsTheCredentialOnSuccess(t *testing.T) {
	f := &fakeSecrets{}
	tok, hash, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps())
	if err != nil {
		t.Fatal(err)
	}
	if tok != mintID+"."+mintSecret || hash != "sha256:abc" {
		t.Errorf("token = %q, hash = %q", tok, hash)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v on the success path", f.deleted)
	}
}

// TestCreateTokenReportsAnUnremovableCredential: if the delete itself fails the
// operator must be told, because nothing else will ever remove it — bootstrap
// tokens do not expire. Swallowing this would recreate the original defect with
// an extra step.
func TestCreateTokenReportsAnUnremovableCredential(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeSecrets{deleteErr: errors.New("apiserver said no")}
	s := okSteps()
	s.waitForSig = func(context.Context, string) error { return boom }

	_, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, s)
	if err == nil {
		t.Fatal("no error returned")
	}
	if !errors.Is(err, boom) {
		t.Errorf("original cause lost behind the cleanup failure: %v", err)
	}
	// Each of these must be independently falsifiable: bare mintID is a
	// substring of mintName, so that element could never fail on its own, and a
	// WARNING that dropped the token id entirely would still pass. Assert the
	// rendered fragment instead.
	for _, want := range []string{
		"WARNING",
		fmt.Sprintf("bootstrap token %q", mintID),
		"until its expiration",
		"kubectl -n kube-system delete secret " + mintName,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q\ngot: %v", want, err)
		}
	}
}

// TestCreateTokenRefusesARandomIDCollision: ids are 6 random characters drawn
// fresh per call, so AlreadyExists is an accident, not an idempotent re-run.
//
// Update-on-collision would take ownership of the colliding Secret's contents
// (and drop any expiration it carried) while reporting created=false, so the
// cleanup skips it — the stranded non-expiring credential createToken exists
// to prevent, reached by a different door. Refusing costs one re-run.
func TestCreateTokenRefusesARandomIDCollision(t *testing.T) {
	f := &fakeSecrets{existing: true}

	_, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps())
	if err == nil {
		t.Fatal("a colliding id was accepted; this run would own a credential it never cleans up")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("got %v; want the collision named so a re-run is the obvious remedy", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v, but this run did not create it", f.deleted)
	}
}

// TestCreateTokenPropagatesAMintFailure: a failure to mint has nothing to clean
// up, and must not be dressed up as a cleanup problem.
func TestCreateTokenPropagatesAMintFailure(t *testing.T) {
	f := &fakeSecrets{createErr: errors.New("quota exceeded")}
	_, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps())
	if err == nil {
		t.Fatal("no error returned")
	}
	if !strings.Contains(err.Error(), "bootstrap secret") || strings.Contains(err.Error(), "WARNING") {
		t.Errorf("got %v; want the plain mint failure", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v after a failed mint", f.deleted)
	}
}

// ourSecret is what the apiserver would hold after committing this run's write.
func ourSecret(id, secret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapSecretName(id), Namespace: "kube-system"},
		Type:       corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":     []byte(id),
			"token-secret": []byte(secret),
		},
	}
}

// TestCreateTokenCleansUpAnAmbiguousCreate is a regression test.
//
// A Create can be committed by the apiserver and still surface to the client as
// an error — a timeout after persistence, a dropped response. Reporting
// created=false there skipped the deferred revoke and left a valid non-expiring
// credential behind a command that reported failure: the same defect the revoke
// exists to prevent, reached through the write path instead of the failure path.
//
// The guarantee is "this command owns what it creates", so it has to hold when
// the command does not know whether it created.
func TestCreateTokenCleansUpAnAmbiguousCreate(t *testing.T) {
	cause := errors.New("etcdserver: request timed out")
	f := &fakeSecrets{createErr: cause, stored: ourSecret(mintID, mintSecret)}

	_, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps())
	if err == nil {
		t.Fatal("no error returned")
	}
	if !errors.Is(err, cause) {
		t.Errorf("original cause lost: %v", err)
	}
	if got := f.deleted; len(got) != 1 || got[0] != mintName {
		t.Errorf("deleted = %v, want [%s]: a committed-but-errored create must still be cleaned up", got, mintName)
	}
}

// TestCreateTokenNeverDeletesOnAGuess: the read-back is what makes the claim
// safe. Without it the command would be deleting a name, not a credential it
// owns.
func TestCreateTokenNeverDeletesOnAGuess(t *testing.T) {
	cause := errors.New("connection refused")
	other := ourSecret(mintID, mintSecret)
	other.Data["token-secret"] = []byte("ffffffffffffffff") // same name, not our secret

	// The claim is token-id AND token-secret. Both conjuncts need a case that
	// falsifies them independently, or one is dead weight: the idMismatch row
	// is what makes the token-id comparison falsifiable.
	idMismatch := ourSecret(mintID, mintSecret)
	idMismatch.Data["token-id"] = []byte("zzzzzz") // right name, wrong id field

	cases := map[string]*fakeSecrets{
		"nothing was committed":                     {createErr: cause},
		"read-back itself fails":                    {createErr: cause, getErr: errors.New("apiserver unreachable")},
		"object at our name has a different secret": {createErr: cause, stored: other},
		"object at our name has a different id":     {createErr: cause, stored: idMismatch},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps())
			if err == nil {
				t.Fatal("no error returned")
			}
			if len(f.deleted) != 0 {
				t.Errorf("deleted %v without establishing ownership", f.deleted)
			}
			// Declining to delete is right. Staying silent is not, when the create
			// may have committed and this run cannot tell — that is the same
			// uncertainty revokeMintedSecret goes out of its way to report.
			warned := strings.Contains(err.Error(), "may exist as a valid join credential")
			if unknown := f.getErr != nil; warned != unknown {
				t.Errorf("warned = %v, want %v (only an UNPROVEN outcome should warn)\ngot: %v",
					warned, unknown, err)
			}
		})
	}
}

// TestCreateTokenDoesNotReadBackOnACleanCreate: the extra Get is for the
// ambiguous path only; a successful create already knows what it made.
func TestCreateTokenDoesNotReadBackOnACleanCreate(t *testing.T) {
	f := &fakeSecrets{}
	if _, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps()); err != nil {
		t.Fatal(err)
	}
	if f.getCalls != 0 {
		t.Errorf("made %d read-back call(s) on the clean path", f.getCalls)
	}
}

// TestRevokeTokenRemovesTheCredentialByTokenString covers the seam cmd/stratum uses
// to extend the ownership window past createToken: `stratum token create
// --control-plane` still has work to do after the mint returns.
func TestRevokeTokenRemovesTheCredentialByTokenString(t *testing.T) {
	cause := errors.New("uploading shared certs failed")

	t.Run("deletes the named secret and preserves the cause", func(t *testing.T) {
		f := &fakeSecrets{}
		err := revokeToken(context.Background(), f, mintID+"."+mintSecret, cause)
		if !errors.Is(err, cause) {
			t.Errorf("cause lost: %v", err)
		}
		if got := f.deleted; len(got) != 1 || got[0] != mintName {
			t.Errorf("deleted = %v, want [%s]", got, mintName)
		}
	})

	t.Run("an unparseable token warns instead of silently skipping", func(t *testing.T) {
		f := &fakeSecrets{}
		err := revokeToken(context.Background(), f, "not-a-token", cause)
		if !errors.Is(err, cause) {
			t.Errorf("cause lost: %v", err)
		}
		if len(f.deleted) != 0 {
			t.Errorf("deleted %v for a token it could not parse", f.deleted)
		}
		// A credential nobody can name is still a credential.
		for _, want := range []string{"WARNING", "until its expiration", "field-selector type=bootstrap.kubernetes.io/token"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message missing %q\ngot: %v", want, err)
			}
		}
	})

	t.Run("a failed delete reports the credential it could not remove", func(t *testing.T) {
		f := &fakeSecrets{deleteErr: errors.New("apiserver said no")}
		err := revokeToken(context.Background(), f, mintID+"."+mintSecret, cause)
		if !errors.Is(err, cause) {
			t.Errorf("cause lost: %v", err)
		}
		if !strings.Contains(err.Error(), "kubectl -n kube-system delete secret "+mintName) {
			t.Errorf("message does not name the credential to remove\ngot: %v", err)
		}
	})
}

// TestRevokeTokenBoundedDoesNotHangOnAWedgedApiserver: the rollback's likely
// trigger IS a wedged apiserver, so an unbounded Delete turns a reported stranded
// credential into a hung command with no output at all.
func TestRevokeTokenBoundedDoesNotHangOnAWedgedApiserver(t *testing.T) {
	restore := waitForSigningTimeout
	waitForSigningTimeout = 30 * time.Millisecond
	defer func() { waitForSigningTimeout = restore }()

	cause := errors.New("uploading shared certs failed")
	f := &fakeSecrets{hangUntilCtxDone: true}
	done := make(chan error, 1)
	go func() { done <- revokeTokenBounded(f, mintID+"."+mintSecret, cause) }()

	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Errorf("cause lost: %v", err)
		}
		// A timeout must degrade to the actionable message, not to silence.
		if !strings.Contains(err.Error(), "kubectl -n kube-system delete secret "+mintName) {
			t.Errorf("a timed-out rollback did not name the credential to remove\ngot: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revokeTokenBounded hung: the rollback must be bounded, because the thing that " +
			"made the caller fail is usually the apiserver it now has to talk to")
	}
}
