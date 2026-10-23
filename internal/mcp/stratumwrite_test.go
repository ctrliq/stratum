package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// captureRealStdout captures what reaches the process's stdout while fn
// runs, installing a pipe as os.Stdout for the duration. It returns what was
// written and the *os.File that os.Stdout held while fn ran, so a pin can
// tell a plain capture from a redirect underneath it: code that swaps
// os.Stdout again while fn runs (the stdout guard's shape) leaves
// the code under test observing the redirector's own pipe, not this one.
func captureRealStdout(t *testing.T, fn func()) (string, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	var buf strings.Builder
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()

	fn()

	os.Stdout = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String(), w
}

// captureStderrDiscard silences stderr without needing a *testing.T, for
// use inside a stdout capture.
func captureStderrDiscard(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		fn()
		return ""
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan struct{})
	var buf strings.Builder
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()
	fn()
	os.Stderr = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

// The minted token must never appear in the log stream.
func TestTokenCreateDoesNotLogTheToken(t *testing.T) {
	fakeStratumHost(t)

	const (
		secret  = "abcdef.0123456789abcdef"
		certKey = "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	)

	deps := tokenDeps{
		create: func(w io.Writer, serverURL string, ttl time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			// Real token.Create prints progress while it waits for cluster-info to be
			// signed; this stub writes it through the writer it is handed.
			fmt.Fprintln(w, "  waiting for the control plane to sign cluster-info (attempt 1)...")
			return secret, "sha256:" + strings.Repeat("a", 64), nil, nil
		},
		newCertKey:  func() (string, error) { return certKey, nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		rekeyGuard:  func() error { return nil },
		nodeIP:      func() (string, error) { return "10.0.0.1", nil },
	}

	var out TokenCreateOut
	var err error
	handler := tokenCreateWith(deps)

	onStdout, _ := captureRealStdout(t, func() {
		logged := captureStderrDiscard(func() {
			_, out, err = handler(context.Background(), nil, TokenCreateIn{ControlPlane: true})
		})
		if strings.Contains(logged, secret) {
			t.Errorf("THE TOKEN WAS WRITTEN TO THE LOG STREAM:\n%s", logged)
		}
		if strings.Contains(logged, certKey) {
			t.Errorf("THE CONTROL-PLANE KEY WAS WRITTEN TO THE LOG STREAM:\n%s", logged)
		}
	})
	if err != nil {
		t.Fatalf("token_create: %v", err)
	}

	// Nor to stdout, which on stdio is the protocol stream.
	if strings.Contains(onStdout, secret) || strings.Contains(onStdout, certKey) {
		t.Errorf("a credential reached the real stdout: %q", onStdout)
	}

	// It must still be returned — once, in the result.
	if out.Token != secret {
		t.Errorf("token = %q, want it returned in the result", out.Token)
	}
	if out.CertKey != certKey {
		t.Errorf("certKey = %q, want it returned when control_plane is set", out.CertKey)
	}
	if !strings.Contains(out.Note, "credential") {
		t.Errorf("note does not say the token is a credential: %q", out.Note)
	}
	if !strings.Contains(out.Note, "mint a separate token for a worker") {
		t.Errorf("note does not keep the control-plane token off workers: %q", out.Note)
	}
}

// An omitted ttl must not mean "never expires". The CLI spells that
// --ttl=0, but an absent JSON field and a zero one are indistinguishable,
// so a model that simply did not think about lifetime would otherwise mint
// a permanent cluster credential.
func TestTokenCreateTTLDefaults(t *testing.T) {
	fakeStratumHost(t)

	var gotTTL time.Duration
	deps := tokenDeps{
		create: func(w io.Writer, _ string, ttl time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			gotTTL = ttl
			return "tok", "hash", nil, nil
		},
		newCertKey:  func() (string, error) { return "", nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		nodeIP:      func() (string, error) { return "10.0.0.1", nil },
	}
	handler := tokenCreateWith(deps)

	t.Run("omitted means the default, not forever", func(t *testing.T) {
		captureStderrDiscard(func() {
			_, _, _ = handler(context.Background(), nil, TokenCreateIn{})
		})
		if gotTTL == 0 {
			t.Fatal("an omitted ttl_hours minted a NON-EXPIRING token")
		}
		if gotTTL != 24*time.Hour {
			t.Errorf("ttl = %s, want the 24h default", gotTTL)
		}
	})

	t.Run("explicit hours are honoured", func(t *testing.T) {
		captureStderrDiscard(func() {
			_, _, _ = handler(context.Background(), nil, TokenCreateIn{TTLHours: 2})
		})
		if gotTTL != 2*time.Hour {
			t.Errorf("ttl = %s, want 2h", gotTTL)
		}
	})

	t.Run("negative is rejected", func(t *testing.T) {
		var err error
		captureStderrDiscard(func() {
			_, _, err = handler(context.Background(), nil, TokenCreateIn{TTLHours: -1})
		})
		if err == nil {
			t.Error("a negative ttl was accepted")
		}
	})

	// An hours value that overflows time.Duration wraps NEGATIVE, and a
	// non-positive ttl means "never expires" -- so without a bound the
	// caller asking for the longest possible life gets a permanent
	// credential instead. The exact opposite of the request.
	t.Run("an overflowing ttl is rejected, not silently made permanent", func(t *testing.T) {
		gotTTL = 0
		var err error
		captureStderrDiscard(func() {
			_, _, err = handler(context.Background(), nil, TokenCreateIn{TTLHours: 3_000_000})
		})
		if err == nil {
			t.Fatalf("ttl_hours=3000000 was accepted and minted a token with ttl %s", gotTTL)
		}
		if gotTTL != 0 {
			t.Errorf("the token was minted anyway, with ttl %s", gotTTL)
		}
		if !strings.Contains(err.Error(), "too large") {
			t.Errorf("error %q does not say the value is out of range", err)
		}
	})

	t.Run("the largest value that does fit is still honoured", func(t *testing.T) {
		maxHours := int(math.MaxInt64 / int64(time.Hour))
		gotTTL = 0
		var err error
		captureStderrDiscard(func() {
			_, _, err = handler(context.Background(), nil, TokenCreateIn{TTLHours: maxHours})
		})
		if err != nil {
			t.Fatalf("the maximum in-range ttl was rejected: %v", err)
		}
		if gotTTL <= 0 {
			t.Errorf("ttl = %s, want a positive duration", gotTTL)
		}
	})
}

// The default server URL is derived from this node's IP, not taken from a
// caller-supplied string, when none is given.
func TestTokenCreateDerivesServerURL(t *testing.T) {
	fakeStratumHost(t)
	// A test host whose real node.yaml records a VIP would answer the VIP. The
	// state saved here has none, and with it the join flags recover the
	// network from this state rather than from the host's own PKI.
	joinFlagDirs(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
		t.Fatal(err)
	}

	var gotURL string
	handler := tokenCreateWith(tokenDeps{
		create: func(w io.Writer, serverURL string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			gotURL = serverURL
			return "tok", "hash", nil, nil
		},
		newCertKey:  func() (string, error) { return "", nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		nodeIP:      func() (string, error) { return "192.0.2.10", nil },
	})

	var err error
	captureStderrDiscard(func() {
		_, _, err = handler(context.Background(), nil, TokenCreateIn{})
	})
	if err != nil {
		t.Fatalf("token_create: %v", err)
	}
	if !strings.HasPrefix(gotURL, "https://192.0.2.10:") {
		t.Errorf("server URL = %q, want it derived from the node IP", gotURL)
	}
}

// control_plane must reach the mint: only a control-plane token is in the
// group allowed to read the shared certs the handler uploads.
func TestTokenCreateMintsTheShapeItWasAskedFor(t *testing.T) {
	fakeStratumHost(t)
	for _, cp := range []bool{false, true} {
		var got, called bool
		handler := tokenCreateWith(tokenDeps{
			create: func(w io.Writer, _ string, _ time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
				got, called = controlPlane, true
				return "tok", "hash", nil, nil
			},
			newCertKey:  func() (string, error) { return "key", nil },
			uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
			rekeyGuard:  func() error { return nil },
			nodeIP:      func() (string, error) { return "192.0.2.10", nil },
		})
		var err error
		captureStderrDiscard(func() {
			_, _, err = handler(context.Background(), nil, TokenCreateIn{ControlPlane: cp})
		})
		if err != nil {
			t.Fatalf("control_plane=%v: %v", cp, err)
		}
		if !called || got != cp {
			t.Errorf("control_plane=%v: create was asked for controlPlane=%v", cp, got)
		}
	}
}

func TestDefaultTokenDepsUsesRecordedNodeIP(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	t.Setenv("NODE_IP", "10.9.9.9")
	if err := nodestate.SaveState(&nodestate.NodeState{NodeIP: "192.168.99.12"}); err != nil {
		t.Fatal(err)
	}

	ip, err := defaultTokenDeps().nodeIP()
	if err != nil || ip != "192.168.99.12" {
		t.Fatalf("MCP token_create node IP = %q, %v; want the saved address", ip, err)
	}
}

// keep=0 would delete every snapshot the node has. Rejecting it is what
// lets this tool sit at the write tier instead of the destructive one.
func TestSnapshotPruneRejectsKeepZero(t *testing.T) {
	fakeStratumHost(t)
	handler := SnapshotPruneHandler()

	for _, keep := range []int{0, -1} {
		_, _, err := handler(context.Background(), nil, SnapshotPruneIn{Keep: keep})
		if err == nil {
			t.Fatalf("keep=%d was accepted; it would delete every snapshot on the node", keep)
		}
		if !strings.Contains(err.Error(), "at least 1") {
			t.Errorf("keep=%d error does not say what is required: %v", keep, err)
		}
	}
}

func TestStratumWriteToolsRequireAStratumHost(t *testing.T) {
	notStratumHost(t)
	ctx := context.Background()

	checks := map[string]func() error{
		"snapshot_save": func() error {
			_, _, err := SnapshotSaveHandler()(ctx, nil, SnapshotSaveIn{})
			return err
		},
		"snapshot_schedule": func() error {
			_, _, err := SnapshotScheduleHandler()(ctx, nil, SnapshotScheduleIn{})
			return err
		},
		"snapshot_prune": func() error {
			_, _, err := SnapshotPruneHandler()(ctx, nil, SnapshotPruneIn{Keep: 3})
			return err
		},
		"token_create": func() error {
			_, _, err := TokenCreateHandler()(ctx, nil, TokenCreateIn{})
			return err
		},
		"registry_apply": func() error {
			_, _, err := RegistryApplyHandler()(ctx, nil, RegistryApplyIn{})
			return err
		},
	}

	for name, call := range checks {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s succeeded off a Stratum node", name)
			}
			if !strings.Contains(err.Error(), "not a Stratum node") {
				t.Errorf("%s failed with an unhelpful message: %v", name, err)
			}
		})
	}
}

// TestApplyScheduleCannotRepointTheStore.
//
// An etcd snapshot is the whole cluster, so where it is sent is a total
// exfiltration primitive -- and the endpoint carries the operator's object
// store credentials to whatever host it names. The tool sets retention and
// nothing else; the destination stays where the operator put it.
func TestApplyScheduleCannotRepointTheStore(t *testing.T) {
	existing := etcdsnap.Config{
		Retain: 7,
		S3: objstore.Target{
			Bucket:    "operator-bucket",
			Endpoint:  "https://minio.internal:9000",
			Region:    "us-east-1",
			Prefix:    "clusters/alpha",
			PathStyle: true,
		},
	}

	got := applySchedule(existing, SnapshotScheduleIn{Retain: 3})
	if got.Retain != 3 {
		t.Errorf("retain = %d, want 3", got.Retain)
	}
	if got.S3 != existing.S3 {
		t.Errorf("the store was changed:\n got %+v\nwant %+v", got.S3, existing.S3)
	}

	t.Run("omitted retain leaves everything alone", func(t *testing.T) {
		if got := applySchedule(existing, SnapshotScheduleIn{}); got != existing {
			t.Errorf("an empty request changed the config: %+v", got)
		}
	})
}

// TestSnapshotNameMustBeABareFileName. etcdsnap.Save does
// filepath.Join(dir, name), which cleans but does not confine, and this
// server usually runs as root on a node — so an unchecked name turns a
// write-tier tool into an arbitrary-file-write primitive.
func TestSnapshotNameMustBeABareFileName(t *testing.T) {
	for _, bad := range []string{
		"../../etc/cron.d/pwn",
		"..",
		".",
		"/etc/passwd",
		"sub/dir.db",
		"a\x00b",
		"cluster.yaml", // a custom snapshot dir may hold other files
		"snap.db.part",
		"db",
	} {
		if err := checkSnapshotName(bad, "/srv/snaps"); err == nil {
			t.Errorf("name %q was accepted", bad)
		}
	}
	for _, ok := range []string{"", "snap.db", "pre-upgrade-2026.db", "a.b.db"} {
		if err := checkSnapshotName(ok, "/srv/snaps"); err != nil {
			t.Errorf("name %q was rejected: %v", ok, err)
		}
	}

	// And the handler has to actually call it. Testing the predicate alone
	// leaves the call site uncovered, which is the half that matters.
	t.Run("the handler refuses a name that is not a .db file", func(t *testing.T) {
		fakeStratumHost(t)
		dir := stubSnapshotDir(t)
		var err error
		captureStderrDiscard(func() {
			_, _, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Name: "cluster.yaml"})
		})
		if err == nil || !strings.Contains(err.Error(), "ending in .db") {
			t.Fatalf("snapshot_save accepted %q: %v", "cluster.yaml", err)
		}
		if _, serr := os.Stat(dir); serr == nil {
			t.Errorf("the name was refused after %s was created", dir)
		}
	})

	t.Run("the handler refuses a traversing name", func(t *testing.T) {
		fakeStratumHost(t)
		var err error
		captureStderrDiscard(func() {
			_, _, err = SnapshotSaveHandler()(context.Background(), nil,
				SnapshotSaveIn{Name: "../../etc/cron.d/pwn"})
		})
		if err == nil {
			t.Fatal("snapshot_save accepted a traversing name")
		}
		if !strings.Contains(err.Error(), "bare file name") {
			t.Errorf("error %q is not the name check; it got past it", err)
		}
	})
}

// TestTokenCreateRevokesTheTokenWhenALaterStepFails.
//
// With control_plane set, the token is minted before the cert key is
// generated and the shared certs uploaded. A failure in either returns an
// error to the caller and hands back no token — so without a rollback a live
// join credential is left in kube-system that nobody saw and nobody can name
// to revoke. token.RevokeOnFailure exists for this and `stratum token create`
// already uses it; this pins that the MCP handler does too.
func TestTokenCreateRevokesTheTokenWhenALaterStepFails(t *testing.T) {
	fakeStratumHost(t)

	boom := errors.New("apiserver said no")

	for _, tc := range []struct {
		name string
		deps func(revoked *string) tokenDeps
	}{
		{"cert key generation fails", func(revoked *string) tokenDeps {
			return tokenDeps{
				create: func(_ io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					return "abcdef.0123456789abcdef", "hash", nil, nil
				},
				newCertKey: func() (string, error) { return "", boom },
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
					t.Error("uploadCerts ran after cert-key generation failed")
					return nil
				},
				rekeyGuard: func() error { return nil },
				nodeIP:     func() (string, error) { return "10.0.0.1", nil },
				revoke:     func(tok string, cause error) error { *revoked = tok; return cause },
			}
		}},
		{"certificate upload fails", func(revoked *string) tokenDeps {
			return tokenDeps{
				create: func(_ io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					return "abcdef.0123456789abcdef", "hash", nil, nil
				},
				newCertKey:  func() (string, error) { return "key", nil },
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return boom },
				rekeyGuard:  func() error { return nil },
				nodeIP:      func() (string, error) { return "10.0.0.1", nil },
				revoke:      func(tok string, cause error) error { *revoked = tok; return cause },
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revoked string
			var err error
			var out TokenCreateOut
			captureStderrDiscard(func() {
				_, out, err = tokenCreateWith(tc.deps(&revoked))(context.Background(), nil,
					TokenCreateIn{ControlPlane: true})
			})
			if err == nil {
				t.Fatal("the failure was not reported")
			}
			if !errors.Is(err, boom) {
				t.Errorf("error %v does not carry the cause", err)
			}
			if revoked != "abcdef.0123456789abcdef" {
				t.Errorf("the minted token was not revoked (revoke saw %q)", revoked)
			}
			if out.Token != "" {
				t.Errorf("a token was returned alongside the error: %q", out.Token)
			}
		})
	}

	t.Run("a successful control-plane mint is not revoked", func(t *testing.T) {
		var revoked string
		var out TokenCreateOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = tokenCreateWith(tokenDeps{
				create: func(_ io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					return "abcdef.0123456789abcdef", "hash", nil, nil
				},
				newCertKey:  func() (string, error) { return "key", nil },
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
				rekeyGuard:  func() error { return nil },
				nodeIP:      func() (string, error) { return "10.0.0.1", nil },
				revoke:      func(tok string, cause error) error { revoked = tok; return cause },
			})(context.Background(), nil, TokenCreateIn{ControlPlane: true})
		})
		if err != nil {
			t.Fatalf("a clean run failed: %v", err)
		}
		if revoked != "" {
			t.Errorf("a token handed to the caller was revoked: %q", revoked)
		}
		if out.Token == "" || out.CertKey == "" {
			t.Errorf("out = %+v, want both credentials", out)
		}
	})
}

// TestTokenCreate_MintFailureRevokesNothing mirrors the CLI's
// TestCreateTokenMintFailureHasNothingToRevoke (pkg/cli/token_test.go) for the
// MCP front end. When the mint itself fails, no token exists, so revoking would
// call back a credential that was never created — and now that both front ends
// share one token.Issue, a single deferred revoke that also fires on a failed
// mint would run token.Create's own rollback twice.
func TestTokenCreate_MintFailureRevokesNothing(t *testing.T) {
	fakeStratumHost(t)

	boom := errors.New("mint failed")
	var revoked []string
	handler := tokenCreateWith(tokenDeps{
		create: func(_ io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			return "", "", nil, boom
		},
		newCertKey: func() (string, error) {
			t.Error("cert-key generation ran after the mint failed")
			return "", nil
		},
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
			t.Error("certificate upload ran after the mint failed")
			return nil
		},
		nodeIP: func() (string, error) { return "10.0.0.1", nil },
		revoke: func(tok string, _ error) error {
			revoked = append(revoked, tok)
			return nil
		},
	})

	var out TokenCreateOut
	var err error
	captureStderrDiscard(func() {
		_, out, err = handler(context.Background(), nil, TokenCreateIn{ControlPlane: true})
	})
	if err == nil {
		t.Fatal("the failed mint was not reported")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not carry the cause", err)
	}
	if len(revoked) != 0 {
		t.Errorf("revoked %v after a mint that created nothing", revoked)
	}
	if out.Token != "" {
		t.Errorf("a token was returned alongside the error: %q", out.Token)
	}
}

// TestTokenCreateControlPlaneRefusesMidRotation is the MCP twin of
// the CLI's TestTokenCreateControlPlaneRefusesMidRotation
// (pkg/cli/certs_test.go): token_create with control_plane set mints a new
// certificate-key and re-encrypts stratum-certs, so it consults the same
// re-key entry point and refuses while a CA rotation is staged or mid-phase —
// the two front ends must answer the request the same way. The refusal
// precedes the mint and its cluster-info publication, so no credential or
// trust publication is changed. A worker token re-keys nothing, but its mint
// still republishes cluster-info and the kubelet client CA from local PKI,
// so it consults the guard too: a worker mint from a controller the rotation
// has not reached would replace the published old‖new trust with old-only.
func TestTokenCreateControlPlaneRefusesMidRotation(t *testing.T) {
	fakeStratumHost(t)
	refusal := errors.New("a cluster-wide CA rotation or etcd-CA migration is staging shared material (the stratum-carotate Secret exists)")

	t.Run("control-plane refuses before minting", func(t *testing.T) {
		var revoked string
		var out TokenCreateOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = tokenCreateWith(tokenDeps{
				create: func(_ io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					t.Error("mint/publication ran although the guard refused")
					return "", "", nil, errors.New("unreachable")
				},
				newCertKey: func() (string, error) {
					t.Error("newCertKey ran although the re-key guard refused")
					return "", errors.New("unreachable")
				},
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
					t.Error("uploadCerts ran although the re-key guard refused")
					return errors.New("unreachable")
				},
				rekeyGuard: func() error { return refusal },
				nodeIP:     func() (string, error) { return "10.0.0.1", nil },
				revoke:     func(tok string, cause error) error { revoked = tok; return cause },
			})(context.Background(), nil, TokenCreateIn{ControlPlane: true})
		})
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v, want the re-key guard's refusal", err)
		}
		if revoked != "" {
			t.Errorf("revoke ran before any mint (saw %q)", revoked)
		}
		if out.Token != "" || out.CertKey != "" {
			t.Errorf("credentials were returned alongside the refusal: %+v", out)
		}
	})

	// Replaces the landed pin "a worker token never consults the guard",
	// whose premise — a worker mint publishes no trust — is false:
	// token.Create republishes cluster-info and the kubelet client CA from
	// local PKI for a worker token too, so a worker mint from a controller
	// the rotation has not reached would roll the published old‖new trust
	// back to old-only. The guard refuses before the mint and before any
	// credential exists, so nothing is revoked.
	t.Run("a worker token consults the guard too", func(t *testing.T) {
		var revoked string
		var out TokenCreateOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = tokenCreateWith(tokenDeps{
				create: func(_ io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					t.Error("mint/publication ran although the guard refused")
					return "", "", nil, errors.New("unreachable")
				},
				newCertKey: func() (string, error) {
					t.Error("newCertKey ran for a worker token")
					return "", errors.New("unreachable")
				},
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
					t.Error("uploadCerts ran for a worker token")
					return errors.New("unreachable")
				},
				rekeyGuard: func() error { return refusal },
				nodeIP:     func() (string, error) { return "10.0.0.1", nil },
				revoke:     func(tok string, cause error) error { revoked = tok; return cause },
			})(context.Background(), nil, TokenCreateIn{})
		})
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v, want the re-key guard's refusal for a worker mint too", err)
		}
		if revoked != "" {
			t.Errorf("revoke ran before any mint (saw %q)", revoked)
		}
		if out.Token != "" || out.CertKey != "" {
			t.Errorf("credentials were returned alongside the refusal: %+v", out)
		}
	})

	// A worker mint whose guard passes still mints: the guard is a refusal
	// gate, not a worker prohibition.
	t.Run("a worker token mints when the guard passes", func(t *testing.T) {
		var out TokenCreateOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = tokenCreateWith(tokenDeps{
				create: func(_ io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					return "abcdef.0123456789abcdef", "hash", nil, nil
				},
				newCertKey: func() (string, error) {
					t.Error("newCertKey ran for a worker token")
					return "", errors.New("unreachable")
				},
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
					t.Error("uploadCerts ran for a worker token")
					return errors.New("unreachable")
				},
				rekeyGuard: func() error { return nil },
				nodeIP:     func() (string, error) { return "10.0.0.1", nil },
				revoke: func(tok string, cause error) error {
					t.Error("a successful worker mint was revoked")
					return cause
				},
			})(context.Background(), nil, TokenCreateIn{})
		})
		if err != nil {
			t.Fatalf("worker token_create: %v", err)
		}
		if out.Token == "" {
			t.Error("the worker token was not returned")
		}
	})
}

// TestTokenCreateDerivesTheJoinURL. The URL is published in cluster-info, so
// it is where a joiner sends the bootstrap credential. A caller-supplied one
// lets a model point the next node at a host it controls, and the victim is
// whoever follows the join instructions — so the tool does not accept it.
func TestTokenCreateDerivesTheJoinURL(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"10.0.0.7", "https://10.0.0.7:6443"},
		{"fd00::7", "https://[fd00::7]:6443"},
		{"10.0.0.7,fd00::7", "https://10.0.0.7:6443"},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			fakeStratumHost(t)
			// The join URL reads node state, so a test host whose real
			// node.yaml records a VIP would answer the VIP in every case; and
			// the join flags derived before it read the netconfig, or with
			// no netconfig the host's PKI, which a non-root run cannot open
			// on an installed host. The state saved here has no VIP, and the
			// flags recover the network from it.
			joinFlagDirs(t)
			if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
				t.Fatal(err)
			}

			var gotServer string
			handler := tokenCreateWith(tokenDeps{
				create: func(w io.Writer, server string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					gotServer = server
					return "abcdef.0123456789abcdef", "hash", nil, nil
				},
				newCertKey:  func() (string, error) { return "k", nil },
				uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
				nodeIP:      func() (string, error) { return tc.ip, nil },
				revoke:      func(_ string, cause error) error { return cause },
			})

			var out TokenCreateOut
			var err error
			captureStderrDiscard(func() {
				_, out, err = handler(context.Background(), nil, TokenCreateIn{})
			})
			if err != nil {
				t.Fatalf("token_create: %v", err)
			}
			if want := tc.want; gotServer != want {
				t.Errorf("join URL = %q, want %q derived from the node IP", gotServer, want)
			}
			if out.Server != gotServer {
				t.Errorf("out.Server = %q, want the URL actually published (%q)", out.Server, gotServer)
			}
		})
	}
}

// TestTokenCreateNamesTheVIP is the MCP twin of the CLI's pin
// (pkg/cli TestTokenCreateNamesTheVIP): on a cluster with a VIP, token_create's
// server names the VIP, the same line `stratum token create` prints, because
// the URL is what a joiner dials and the VIP is the one address that survives
// the controller that minted the token going away.
//
// The refusal of a caller-supplied URL is the input's shape and stays that
// way: TokenCreateIn carries no server field, so the URL can only come from
// this node's recorded facts — which is what this pin checks the mint saw.
func TestTokenCreateNamesTheVIP(t *testing.T) {
	fakeStratumHost(t)
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	if err := nodestate.SaveState(&nodestate.NodeState{
		NodeIP: "192.168.99.12",
		VIP:    nodestate.VIP{Address: "10.0.0.10"},
	}); err != nil {
		t.Fatal(err)
	}

	var gotServer string
	handler := tokenCreateWith(tokenDeps{
		create: func(_ io.Writer, server string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			gotServer = server
			return "abcdef.0123456789abcdef", "hash", nil, nil
		},
		newCertKey:  func() (string, error) { return "", nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		nodeIP:      func() (string, error) { return "192.168.99.12", nil },
	})

	var out TokenCreateOut
	captureStderrDiscard(func() {
		_, out, _ = handler(context.Background(), nil, TokenCreateIn{})
	})
	const want = "https://10.0.0.10:6443"
	if gotServer != want {
		t.Errorf("token_create minted for %q, want the VIP %q: a join line naming the minting control plane strands the joiner when that controller fails", gotServer, want)
	}
	if out.Server != want {
		t.Errorf("out.Server = %q, want the VIP %q", out.Server, want)
	}
}

// TestCertsRotateCAListsOnlyRemainingPhases. Re-running a completed CA phase
// is not a no-op, so guidance that names the whole sequence after the first
// phase is an instruction to repeat a destructive step.
func TestCertsRotateCAListsOnlyRemainingPhases(t *testing.T) {
	for _, tc := range []struct {
		current string
		want    []string
	}{
		{"trust-new", []string{"sign-new", "finalize"}},
		{"sign-new", []string{"finalize"}},
		{"finalize", nil},
		{"nonexistent", nil},
	} {
		got := phasesAfter(caPhases, tc.current)
		if len(got) != len(tc.want) {
			t.Errorf("after %q: got %v, want %v", tc.current, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("after %q: got %v, want %v", tc.current, got, tc.want)
				break
			}
		}
	}
}

// The guidance an operator actually reads must never name the phase that
// just completed.
func TestCertsRotateCAGuidanceNeverRepeatsThePhase(t *testing.T) {
	for _, phase := range caPhases {
		msg := caRotationNext(phase)
		if phase == caPhases[len(caPhases)-1] {
			if msg != "" {
				t.Errorf("the last phase still produced guidance: %q", msg)
			}
			continue
		}
		if msg == "" {
			t.Errorf("phase %q produced no guidance", phase)
			continue
		}
		after := msg[strings.Index(msg, "run "):]
		if strings.Contains(after, phase) {
			t.Errorf("guidance after %q tells the operator to run it again: %q", phase, msg)
		}
		for _, earlier := range caPhases {
			if earlier == phase {
				break
			}
			if strings.Contains(after, earlier) {
				t.Errorf("guidance after %q names the earlier phase %q: %q", phase, earlier, msg)
			}
		}
	}
}

func TestApplyScheduleKeepsSeededS3Target(t *testing.T) {
	seed := etcdsnap.Config{Retain: 10, S3: objstore.Target{Bucket: "b", PathStyle: true}}
	got := applySchedule(seed, SnapshotScheduleIn{Retain: 3})
	if got.Retain != 3 || got.S3.Bucket != "b" || !got.S3.PathStyle {
		t.Errorf("applySchedule dropped the seeded S3 target: %+v", got)
	}
}

// TestSnapshotSaveHonoursSnapshotDir pins that snapshot_save's name error and
// its write target both follow spec.snapshots.dir.
func TestSnapshotSaveHonoursSnapshotDir(t *testing.T) {
	fakeStratumHost(t)
	dir := stubSnapshotDir(t)

	t.Run("the bad-name error names the dir", func(t *testing.T) {
		var err error
		captureStderrDiscard(func() {
			_, _, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Name: "../x"})
		})
		if err == nil || !strings.Contains(err.Error(), dir) {
			t.Fatalf("error %v does not name %s", err, dir)
		}
	})

	// etcdsnap.Snapshot creates the target's directory before it runs etcdctl,
	// which is absent here (BinDir is an empty temp dir): the directory appearing is the evidence of where
	// the write was aimed.
	t.Run("the write targets the dir", func(t *testing.T) {
		// Keep the host's real etcdctl out of the test.
		origBin := version.BinDir
		version.BinDir = t.TempDir()
		t.Cleanup(func() { version.BinDir = origBin })

		captureStderrDiscard(func() {
			_, _, _ = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Name: "t.db"})
		})
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("snapshot_save did not write under %s: %v", dir, err)
		}
	})
}

func TestSnapshotPruneHonoursSnapshotDir(t *testing.T) {
	fakeStratumHost(t)
	dir := stubSnapshotDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hn, err := host.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, ts := range []string{"20250101-000000", "20250102-000000", "20250103-000000"} {
		if err := os.WriteFile(filepath.Join(dir, etcdsnap.ScheduledName(hn, ts)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out SnapshotPruneOut
	captureStderrDiscard(func() {
		_, out, err = SnapshotPruneHandler()(context.Background(), nil, SnapshotPruneIn{Keep: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Before != 3 || out.After != 1 || out.Removed != 2 {
		t.Errorf("before/after/removed = %d/%d/%d, want 3/1/2", out.Before, out.After, out.Removed)
	}
}

// TestSnapshotTools_ReportAReturnedPruneFailure pins that the snapshot tools
// learn of a failed prune from the errors PruneScheduled returns, never by
// matching the text WARN in the captured output. The failures below
// deliberately contain no "WARN", so a tool that still reads the captured
// text reports nothing and fails here.
//
// The seam is part of the pin: the stubs below cannot compile against a
// PruneScheduled that returns nothing, and a snapshot_save that never
// looks at the prune fails the save subtest.
func TestSnapshotTools_ReportAReturnedPruneFailure(t *testing.T) {
	fakeStratumHost(t)

	resolved := t.TempDir()
	oldCfg := loadSnapshotConfig
	oldList, oldSave, oldPrune := snapshotList, snapshotSave, snapshotPrune
	t.Cleanup(func() {
		loadSnapshotConfig = oldCfg
		snapshotList, snapshotSave, snapshotPrune = oldList, oldSave, oldPrune
	})
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: resolved}, nil }
	snapshotList = func(string, *objstore.Target) ([]etcdsnap.SnapItem, error) { return nil, nil }

	t.Run("snapshot_prune names the returned failure", func(t *testing.T) {
		pruneFail := errors.New("remove etcd-snapshot-cp1-20260101-000000.db: permission denied")
		snapshotPrune = func(string, string, int, *objstore.Target) []error { return []error{pruneFail} }

		var out SnapshotPruneOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotPruneHandler()(context.Background(), nil, SnapshotPruneIn{Keep: 2})
		})
		if err != nil {
			t.Fatalf("a failed prune failed the tool: %v", err)
		}
		if !strings.Contains(out.Note, pruneFail.Error()) {
			t.Errorf("the result does not name the prune failure; note = %q", out.Note)
		}
	})

	t.Run("snapshot_save reports a failed prune beside the path", func(t *testing.T) {
		pruneFail := errors.New("S3 snapshot prune: delete etcd-snapshot-cp1-20260101-000000.db: forbidden")
		savedPath := filepath.Join(resolved, "etcd-snapshot-cp1-20261014-000000.db")
		snapshotPrune = func(string, string, int, *objstore.Target) []error { return nil }
		snapshotSave = func(_ context.Context, w io.Writer, o etcdsnap.SaveOptions) (string, error) {
			// The save reports its prune failures as data while it prints
			// them, so a stub that reports one exercises the result
			// assembly. A handler that never asks for the failures leaves
			// PruneWarn nil and this result carries no warning.
			if o.Retain != 2 {
				t.Errorf("save was sent Retain %d, want 2", o.Retain)
			}
			if o.PruneWarn != nil {
				o.PruneWarn(pruneFail)
			}
			return savedPath, nil
		}

		var out SnapshotSaveOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Retain: 2, SkipS3: true})
		})
		if err != nil {
			t.Fatalf("a failed prune turned the save into an error: %v", err)
		}
		if out.Path != savedPath {
			t.Errorf("out.Path = %q, want the successful save's path %q", out.Path, savedPath)
		}
		if !strings.Contains(out.Note, pruneFail.Error()) {
			t.Errorf("the result does not report the prune warning beside the path; note = %q", out.Note)
		}
	})
}

// TestSnapshotTools_CarryThePruneFailuresAsWarnings pins that the prune
// failures PruneScheduled returns travel as data in the snapshot tools'
// Warnings, beside the note that names them in prose — sourced from the
// operation's returned errors, never from text found in the captured output,
// and absent when the prune cleaned up everything.
func TestSnapshotTools_CarryThePruneFailuresAsWarnings(t *testing.T) {
	fakeStratumHost(t)

	resolved := t.TempDir()
	oldCfg := loadSnapshotConfig
	oldList, oldSave, oldPrune := snapshotList, snapshotSave, snapshotPrune
	t.Cleanup(func() {
		loadSnapshotConfig = oldCfg
		snapshotList, snapshotSave, snapshotPrune = oldList, oldSave, oldPrune
	})
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: resolved}, nil }
	snapshotList = func(string, *objstore.Target) ([]etcdsnap.SnapItem, error) { return nil, nil }

	t.Run("snapshot_prune's warnings are the returned failures", func(t *testing.T) {
		pruneFail := errors.New("remove etcd-snapshot-cp1-20260101-000000.db: permission denied")
		snapshotPrune = func(string, string, int, *objstore.Target) []error { return []error{pruneFail} }
		var out SnapshotPruneOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotPruneHandler()(context.Background(), nil, SnapshotPruneIn{Keep: 2})
		})
		if err != nil {
			t.Fatalf("a failed prune failed the tool: %v", err)
		}
		if !slices.Equal(out.Warnings, []string{pruneFail.Error()}) {
			t.Errorf("out.Warnings = %v, want the returned failure as data", out.Warnings)
		}
	})

	t.Run("snapshot_save's warnings are the prune failures", func(t *testing.T) {
		pruneFail := errors.New("S3 snapshot prune: delete etcd-snapshot-cp1-20260101-000000.db: forbidden")
		snapshotPrune = func(string, string, int, *objstore.Target) []error { return nil }
		snapshotSave = func(_ context.Context, w io.Writer, o etcdsnap.SaveOptions) (string, error) {
			if o.PruneWarn != nil {
				o.PruneWarn(pruneFail)
			}
			return filepath.Join(resolved, "etcd-snapshot-cp1-20261014-000000.db"), nil
		}
		var out SnapshotSaveOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Retain: 2, SkipS3: true})
		})
		if err != nil {
			t.Fatalf("a failed prune turned the save into an error: %v", err)
		}
		if !slices.Equal(out.Warnings, []string{pruneFail.Error()}) {
			t.Errorf("out.Warnings = %v, want the prune failure as data beside the path", out.Warnings)
		}
	})

	t.Run("a clean prune leaves no warnings", func(t *testing.T) {
		snapshotPrune = func(string, string, int, *objstore.Target) []error { return nil }
		snapshotSave = func(_ context.Context, w io.Writer, o etcdsnap.SaveOptions) (string, error) {
			return filepath.Join(o.Dir, "clean.db"), nil
		}
		var pruneOut SnapshotPruneOut
		var saveOut SnapshotSaveOut
		var err error
		captureStderrDiscard(func() {
			_, pruneOut, err = SnapshotPruneHandler()(context.Background(), nil, SnapshotPruneIn{Keep: 2})
		})
		if err != nil {
			t.Fatal(err)
		}
		captureStderrDiscard(func() {
			_, saveOut, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Retain: 2, SkipS3: true})
		})
		if err != nil {
			t.Fatal(err)
		}
		if pruneOut.Warnings != nil || saveOut.Warnings != nil {
			t.Errorf("warnings = %v / %v, want none on a clean prune", pruneOut.Warnings, saveOut.Warnings)
		}
	})
}

// TestSnapshotTools_UseTheConfiguredDir pins where every snapshot tool
// reads and writes:
// spec.snapshots.dir decides where snapshots live, and the scheduled loop owns
// that directory. A snapshot_list that reads anywhere else reports names
// snapshot_restore cannot resolve, a snapshot_save that writes anywhere else
// hides the backup from `stratum snapshot list`, and a snapshot_prune that
// prunes anywhere else removes nothing (or the wrong copies). All three tools
// must take the directory from the resolved snapshot settings — the
// loadSnapshotConfig seam (stratumdestructive.go) — and never from a
// directory read of their own, and an invalid request must reach no effect.
func TestSnapshotTools_UseTheConfiguredDir(t *testing.T) {
	fakeStratumHost(t)

	resolved := t.TempDir()
	oldCfg, oldDir := loadSnapshotConfig, snapshotDir
	oldList, oldSave, oldPrune := snapshotList, snapshotSave, snapshotPrune
	t.Cleanup(func() {
		loadSnapshotConfig, snapshotDir = oldCfg, oldDir
		snapshotList, snapshotSave, snapshotPrune = oldList, oldSave, oldPrune
	})
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: resolved}, nil }
	// A tool that bypasses the resolved settings and reads a directory of its
	// own lands here instead, and every assertion below fails.
	snapshotDir = func() string { return filepath.Join(resolved, "unread") }

	// The list, save and prune effects are injected separately, so each
	// subtest names the directory that one tool actually used.
	var listDirs []string
	snapshotList = func(dir string, _ *objstore.Target) ([]etcdsnap.SnapItem, error) {
		listDirs = append(listDirs, dir)
		return nil, nil
	}
	var saveOpts []etcdsnap.SaveOptions
	snapshotSave = func(_ context.Context, w io.Writer, o etcdsnap.SaveOptions) (string, error) {
		saveOpts = append(saveOpts, o)
		return filepath.Join(o.Dir, "manual.db"), nil
	}
	var pruneDirs []string
	snapshotPrune = func(dir, _ string, _ int, _ *objstore.Target) []error {
		pruneDirs = append(pruneDirs, dir)
		return nil
	}

	t.Run("snapshot_list lists the configured dir", func(t *testing.T) {
		listDirs = nil
		var out SnapshotListOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotListHandler()(context.Background(), nil, SnapshotListIn{})
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Dir != resolved || len(listDirs) != 1 || listDirs[0] != resolved {
			t.Errorf("list reported dir %q (its effect saw %v), want the configured %q", out.Dir, listDirs, resolved)
		}
	})

	t.Run("snapshot_save writes the configured dir", func(t *testing.T) {
		saveOpts = nil
		var out SnapshotSaveOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Name: "manual.db"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(saveOpts) != 1 || saveOpts[0].Dir != resolved {
			t.Errorf("save sent %+v to its effect, want Dir %q", saveOpts, resolved)
		}
		if out.Path != filepath.Join(resolved, "manual.db") {
			t.Errorf("out.Path = %q, want the snapshot in the configured dir", out.Path)
		}
	})

	t.Run("snapshot_prune prunes the configured dir", func(t *testing.T) {
		listDirs, pruneDirs = nil, nil
		var out SnapshotPruneOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = SnapshotPruneHandler()(context.Background(), nil, SnapshotPruneIn{Keep: 2})
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(pruneDirs) != 1 || pruneDirs[0] != resolved {
			t.Errorf("prune ran against %v, want the configured %q", pruneDirs, resolved)
		}
		// The before and after inventories must list the same directory the
		// prune ran in, or the counts count the wrong place.
		for _, dir := range listDirs {
			if dir != resolved {
				t.Errorf("the prune inventory listed %q, want the configured %q", dir, resolved)
			}
		}
		if out.Keep != 2 {
			t.Errorf("out.Keep = %d, want 2", out.Keep)
		}
	})

	t.Run("an invalid request reaches no effect", func(t *testing.T) {
		saves, prunes := len(saveOpts), len(pruneDirs)
		var err error
		captureStderrDiscard(func() {
			_, _, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Name: "../pwn.db"})
		})
		if err == nil {
			t.Fatal("snapshot_save accepted a traversing name")
		}
		captureStderrDiscard(func() {
			_, _, err = SnapshotPruneHandler()(context.Background(), nil, SnapshotPruneIn{Keep: 0})
		})
		if err == nil {
			t.Fatal("snapshot_prune accepted keep=0")
		}
		if len(saveOpts) != saves || len(pruneDirs) != prunes {
			t.Errorf("an invalid request ran an effect: saves %d -> %d, prunes %d -> %d",
				saves, len(saveOpts), prunes, len(pruneDirs))
		}
	})
}

// joinFlagDirs points the join-flag inputs at the test's own directories: the
// persisted network config (version.NetConfigPath) and the node state
// (version.K8sConfDir). The handler must derive the flags from the same
// persisted state the CLI does, so a test redirects the same paths.
func joinFlagDirs(t *testing.T) {
	t.Helper()
	oldConf, oldNet := version.K8sConfDir, version.NetConfigPath
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = oldConf, oldNet })
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
}

// okTokenDeps returns tokenDeps whose every step succeeds and which records
// any revoke, so a test can assert that a delivered grant was not revoked.
func okTokenDeps(revoked *[]string) tokenDeps {
	return tokenDeps{
		create: func(_ io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			return "abcdef.0123456789abcdef", "sha256:aa", nil, nil
		},
		newCertKey:  func() (string, error) { return "deadbeef", nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		nodeIP:      func() (string, error) { return "10.0.0.1", nil },
		revoke: func(tok string, cause error) error {
			*revoked = append(*revoked, tok)
			return cause
		},
	}
}

// TestTokenCreate_ReturnsTheJoinFlags pins that token_create's result
// carries the join flags the CLI derives (token.LoadJoinFlags), so a node
// joined through the MCP server runs the cluster's real ranges, domain and
// CIS profile instead of silently falling back to the defaults — a worker
// learns both only from its join line, and the MCP result is that line's
// only source. The flags travel as data next to the token and the CA pin,
// and the outcome records that credential bytes were delivered, because a
// delivered grant must not be revoked behind the caller's back.
func TestTokenCreate_ReturnsTheJoinFlags(t *testing.T) {
	registerProfiles(t, profiletest.Profile())
	fakeStratumHost(t)
	joinFlagDirs(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", CIS: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(version.NetConfigPath, []byte("10.42.0.0/16\n10.43.0.0/16\nfuzz.internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantFlags := token.JoinFlags{
		PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16",
		ClusterDomain: "fuzz.internal", Profile: "cis",
	}

	for _, cp := range []bool{false, true} {
		var revoked []string
		var out TokenCreateOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = tokenCreateWith(okTokenDeps(&revoked))(context.Background(), nil, TokenCreateIn{ControlPlane: cp})
		})
		if err != nil {
			t.Fatalf("control_plane=%v: %v", cp, err)
		}
		if out.JoinFlags != wantFlags {
			t.Errorf("control_plane=%v: out.JoinFlags = %+v, want %+v", cp, out.JoinFlags, wantFlags)
		}
		if !out.Delivered {
			t.Errorf("control_plane=%v: the result does not record that credential bytes were delivered", cp)
		}
		if len(revoked) != 0 {
			t.Errorf("control_plane=%v: a delivered grant was revoked: %v", cp, revoked)
		}
	}

	// A default cluster leaves the flags empty rather than rendering the
	// defaults into the result.
	t.Run("a default cluster carries no flags", func(t *testing.T) {
		joinFlagDirs(t)
		if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
			t.Fatal(err)
		}
		var revoked []string
		var out TokenCreateOut
		var err error
		captureStderrDiscard(func() {
			_, out, err = tokenCreateWith(okTokenDeps(&revoked))(context.Background(), nil, TokenCreateIn{})
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.JoinFlags != (token.JoinFlags{}) {
			t.Errorf("out.JoinFlags = %+v, want no flags on a default cluster", out.JoinFlags)
		}
	})
}

// TestTokenCreate_DerivesTheJoinFlagsBeforeMinting pins the ordering the CLI
// applies (`stratum token create` derives its join extras before the
// mint, so a failure here mints nothing to revoke): the flags describe the
// cluster the joiner is about to join, so a cluster whose network
// configuration cannot be read must fail token creation before a token
// exists — not mint a credential for a join that would point the joining
// node's kubelet at the wrong ranges and domain. A handler that never
// reads the network configuration mints happily on a malformed one: that
// is the failure this test observes.
func TestTokenCreate_DerivesTheJoinFlagsBeforeMinting(t *testing.T) {
	fakeStratumHost(t)
	joinFlagDirs(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
		t.Fatal(err)
	}
	// Malformed: one line where two or three are required.
	if err := os.WriteFile(version.NetConfigPath, []byte("10.42.0.0/16\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var minted, revoked bool
	deps := tokenDeps{
		create: func(_ io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			minted = true
			return "abcdef.0123456789abcdef", "sha256:aa", nil, nil
		},
		newCertKey:  func() (string, error) { return "deadbeef", nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		nodeIP:      func() (string, error) { return "10.0.0.1", nil },
		revoke:      func(tok string, cause error) error { revoked = true; return cause },
	}
	handler := tokenCreateWith(deps)

	var err error
	captureStderrDiscard(func() {
		_, _, err = handler(context.Background(), nil, TokenCreateIn{})
	})
	if err == nil {
		t.Fatal("token_create succeeded on a cluster whose network configuration cannot be read")
	}
	if minted {
		t.Error("the token was minted although the join flags could not be derived; a failure here must leave nothing to revoke")
	}
	if revoked {
		t.Error("a token that was never minted was revoked")
	}
}

// registerProfiles makes ps the binary's registered profiles for the test
// and puts back the registry it replaced.
func registerProfiles(t *testing.T, ps ...profile.Profile) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered(ps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

// TestTokenCreate_RefusesAProfileThisBinaryDoesNotRegister: token_create on
// a cis control plane run by an engine binary returned a grant whose join
// flags said --profile cis over a bundle without the at-rest encryption key.
// The handler derives the flags before it mints, so nothing is minted,
// uploaded or revoked, and the refusal carries the cis note.
func TestTokenCreate_RefusesAProfileThisBinaryDoesNotRegister(t *testing.T) {
	registerProfiles(t)
	fakeStratumHost(t)
	joinFlagDirs(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", Profile: profile.CISName}); err != nil {
		t.Fatal(err)
	}
	for _, cp := range []bool{true, false} {
		var revoked []string
		deps := okTokenDeps(&revoked)
		deps.create = func(io.Writer, string, time.Duration, bool, func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			t.Errorf("control_plane=%v: a token was minted although the node's profile is not registered", cp)
			return "abcdef.0123456789abcdef", "sha256:aa", nil, nil
		}
		deps.uploadCerts = func(string, *metav1.OwnerReference, func(context.Context) error) error {
			t.Errorf("control_plane=%v: the shared bundle was published although the node's profile is not registered", cp)
			return nil
		}
		var err error
		captureStderrDiscard(func() {
			_, _, err = tokenCreateWith(deps)(context.Background(), nil, TokenCreateIn{ControlPlane: cp})
		})
		if !errors.Is(err, profile.ErrUnregistered) {
			t.Fatalf("control_plane=%v: err = %v, want the refusal wrapping profile.ErrUnregistered", cp, err)
		}
		for _, want := range []string{profile.CISUnregisteredNote, `profile "cis"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("control_plane=%v: refusal is missing %q: %v", cp, want, err)
			}
		}
		if len(revoked) != 0 {
			t.Errorf("control_plane=%v: revoked %v; nothing was minted to revoke", cp, revoked)
		}
	}
}
