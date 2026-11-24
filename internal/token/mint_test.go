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

// Hand-rolled for the same reason as fakeConfigMaps: no new modules in this package.
type fakeSecrets struct {
	created   []string
	deleted   []string
	existing  bool  // Create reports AlreadyExists
	createErr error // Create fails with this
	deleteErr error // Delete fails with this
	// Models a wedged apiserver: Delete blocks until the caller's context is done.
	hangUntilCtxDone bool
	// An ambiguous create (committed, but an error surfaced) is createErr set AND stored set.
	stored   *corev1.Secret
	getErr   error
	getCalls int
	// Stamped on the Secret Create returns, so the ownerRef path can be exercised.
	uid string
	// The most recent Create's object, so a test can assert on the StringData.
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

// Every post-mint step is covered: the guarantee is an error returns no credential.
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

// Every mint drops a worker grant on stratum-certs left by an earlier release.
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

// Cleanup that also fires on success would be a worse bug than the one it prevents.
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

// Nothing else will ever remove the credential, so a failed delete must be told.
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
	// bare mintID is a substring of mintName; assert the rendered fragment.
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

// Update-on-collision would own a credential it reports created=false for; refuse instead.
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

// A failure to mint has nothing to clean up.
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

// What the apiserver would hold after committing this run's write.
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

// A committed-but-errored create must still be cleaned up.
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

// The read-back is what makes the claim safe to delete on.
func TestCreateTokenNeverDeletesOnAGuess(t *testing.T) {
	cause := errors.New("connection refused")
	other := ourSecret(mintID, mintSecret)
	other.Data["token-secret"] = []byte("ffffffffffffffff") // same name, not our secret

	// Both conjuncts need a case that falsifies them independently, or one is dead weight.
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
			// Only an unproven outcome should warn.
			warned := strings.Contains(err.Error(), "may exist as a valid join credential")
			if unknown := f.getErr != nil; warned != unknown {
				t.Errorf("warned = %v, want %v (only an UNPROVEN outcome should warn)\ngot: %v",
					warned, unknown, err)
			}
		})
	}
}

// The extra Get is for the ambiguous path only.
func TestCreateTokenDoesNotReadBackOnACleanCreate(t *testing.T) {
	f := &fakeSecrets{}
	if _, _, _, err := createToken(context.Background(), f, mintID, mintSecret, DefaultTokenTTL, false, okSteps()); err != nil {
		t.Fatal(err)
	}
	if f.getCalls != 0 {
		t.Errorf("made %d read-back call(s) on the clean path", f.getCalls)
	}
}

// The seam cmd/stratum uses to extend the ownership window past createToken.
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

// The rollback's trigger is usually the apiserver it now has to talk to.
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
