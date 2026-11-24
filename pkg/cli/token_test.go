package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

// okCmd is a tokenCmd whose every step succeeds.
func okCmd(out *bytes.Buffer, revoked *[]string) tokenCmd {
	return tokenCmd{
		create: func(w io.Writer, _ string, _ time.Duration, _ bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			return "abcdef.0123456789abcdef", "sha256:aa", nil, nil
		},
		newCertKey:   func() (string, error) { return "deadbeef", nil },
		uploadCerts:  func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		writeKeyFile: func(string) (string, error) { return "/run/stratum/ha-certificate-key-test", nil },
		// Behaves like the real revoke: a WARNING folded into the error, not the bare cause.
		revoke: func(tok string, cause error) error {
			*revoked = append(*revoked, tok)
			return fmt.Errorf("%w\n\nWARNING: could not remove bootstrap token %s", cause, tok)
		},
		out: out,
	}
}

// Both properties matter and are inseparable: the credential is revoked, AND nothing printed.
func TestCreateTokenPrintsNothingUntilEveryStepSucceeds(t *testing.T) {
	boom := errors.New("boom")
	steps := map[string]func(tokenCmd) tokenCmd{
		"newCertKey": func(c tokenCmd) tokenCmd {
			c.newCertKey = func() (string, error) { return "", boom }
			return c
		},
		"uploadCerts": func(c tokenCmd) tokenCmd {
			c.uploadCerts = func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return boom }
			return c
		},
		"writeKeyFile": func(c tokenCmd) tokenCmd {
			c.writeKeyFile = func(string) (string, error) { return "", boom }
			return c
		},
	}
	for name, mut := range steps {
		t.Run(name+" fails", func(t *testing.T) {
			var out bytes.Buffer
			var revoked []string
			err := createToken("https://cp:6443", true, token.DefaultTokenTTL, mut(okCmd(&out, &revoked)))
			if err == nil {
				t.Fatal("no error returned")
			}
			if !errors.Is(err, boom) {
				t.Errorf("original cause lost: %v", err)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q before the command succeeded; the operator now holds a token "+
					"that a failing command minted", out.String())
			}
			if len(revoked) != 1 || revoked[0] != "abcdef.0123456789abcdef" {
				t.Errorf("revoked = %v, want the minted token to have been revoked exactly once", revoked)
			}
			// Revoking is half the job; the operator must be told when cleanup failed.
			if !strings.Contains(err.Error(), "WARNING") {
				t.Errorf("revoke's cleanup warning did not reach the operator: %v", err)
			}
		})
	}
}

// token.Create already rolls back its own credential; the command must not revoke twice.
func TestCreateTokenMintFailureHasNothingToRevoke(t *testing.T) {
	var out bytes.Buffer
	var revoked []string
	c := okCmd(&out, &revoked)
	c.create = func(w io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
		return "", "", nil, errors.New("mint failed")
	}

	if err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c); err == nil {
		t.Fatal("no error returned")
	}
	if len(revoked) != 0 {
		t.Errorf("revoked %v after a failed mint", revoked)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q after a failed mint", out.String())
	}
}

// --control-plane must reach the mint: only a control-plane token may read the shared certs.
func TestCreateTokenMintsTheShapeItWasAskedFor(t *testing.T) {
	for _, cp := range []bool{false, true} {
		var out bytes.Buffer
		var revoked []string
		c := okCmd(&out, &revoked)
		var got, called bool
		create := c.create
		c.create = func(w io.Writer, url string, ttl time.Duration, controlPlane bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			got, called = controlPlane, true
			return create(w, url, ttl, controlPlane, nil)
		}
		if err := createToken("https://cp:6443", cp, token.DefaultTokenTTL, c); err != nil {
			t.Fatal(err)
		}
		if !called || got != cp {
			t.Errorf("--control-plane=%v: create was asked for controlPlane=%v", cp, got)
		}
	}
}

// Covers both forms; success-path cleanup firing at all would be worse than the defect.
func TestCreateTokenSuccessPrintsAndKeeps(t *testing.T) {
	for _, cp := range []bool{false, true} {
		name := "worker only"
		if cp {
			name = "--control-plane"
		}
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			var revoked []string
			if err := createToken("https://cp:6443", cp, token.DefaultTokenTTL, okCmd(&out, &revoked)); err != nil {
				t.Fatal(err)
			}
			if len(revoked) != 0 {
				t.Errorf("revoked %v on the success path", revoked)
			}
			got := out.String()
			workerLine := "stratum up --worker --server https://cp:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:aa"
			if has := strings.Contains(got, workerLine); has == cp {
				t.Errorf("--control-plane=%v: worker join line present = %v, want %v:\n%s", cp, has, !cp, got)
			}
			if cp && !strings.Contains(got, "mint a separate token with `stratum token create`") {
				t.Errorf("--control-plane output does not send workers to a separate token:\n%s", got)
			}
			// The pin is the joiner's MITM defence; a wiring slip here fails closed undiagnosably.
			if n := strings.Count(got, "--discovery-token-ca-cert-hash sha256:aa"); n != 1 {
				t.Errorf("CA pin appears %d time(s), want 1:\n%s", n, got)
			}
			// The raw certificate-key must never reach stdout; the join line names the 0600 file.
			if strings.Contains(got, "deadbeef") {
				t.Errorf("the raw certificate-key leaked to stdout:\n%s", got)
			}
			if hasFile := strings.Contains(got, "--certificate-key-file '/run/stratum/ha-certificate-key-test'"); hasFile != cp {
				t.Errorf("certificate-key-file referenced = %v, want %v:\n%s", hasFile, cp, got)
			}
			// Both join forms carry --token, so the safe-token guidance is for both.
			if !strings.Contains(got, "--token-file <path>") || !strings.Contains(got, tokenEnvVar) {
				t.Errorf("safe-token guidance missing for cp=%v (must name --token-file and $%s):\n%s", cp, tokenEnvVar, got)
			}
		})
	}
}

// The upload must have happened by the time anything reaches the operator.
func TestCreateTokenUploadsBeforePrinting(t *testing.T) {
	var out bytes.Buffer
	var revoked []string
	c := okCmd(&out, &revoked)
	c.uploadCerts = func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
		if out.Len() != 0 {
			t.Errorf("already printed %q before uploading the shared certs", out.String())
		}
		return nil
	}
	if err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c); err != nil {
		t.Fatal(err)
	}
}

// failingWriter fails every write after having accepted `accept` bytes in total.
type failingWriter struct {
	accept int
	got    []byte
}

func (w *failingWriter) Write(p []byte) (int, error) {
	room := w.accept - len(w.got)
	if room <= 0 {
		return 0, errors.New("no space left on device")
	}
	if room > len(p) {
		room = len(p)
	}
	w.got = append(w.got, p[:room]...)
	return room, errors.New("no space left on device")
}

// A write that fails cannot be reported as success: the write is the handover.
func TestCreateTokenTreatsTheWriteAsTheHandover(t *testing.T) {
	t.Run("nothing handed over: revoke", func(t *testing.T) {
		var revoked []string
		w := &failingWriter{accept: 0}
		c := okCmd(&bytes.Buffer{}, &revoked)
		c.out = w

		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if err == nil {
			t.Fatal("a failed write was reported as success")
		}
		if len(revoked) != 1 {
			t.Errorf("revoked = %v, want the credential revoked: none of it reached the operator", revoked)
		}
	})

	// Keep the token once the complete line reached the terminal; a partial heading is no handover.
	t.Run("partially handed over: report, do not revoke", func(t *testing.T) {
		var revoked []string
		prefix := "\nControl-plane (HA) join:\n\n  stratum up --join https://cp:6443 --token abcdef.0123456789abcdef"
		w := &failingWriter{accept: len(prefix)}
		c := okCmd(&bytes.Buffer{}, &revoked)
		c.out = w

		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if err == nil {
			t.Fatal("a partial write was reported as success")
		}
		if len(revoked) != 0 {
			t.Errorf("revoked %v after part of the credential was already printed; "+
				"that turns a stranded credential into output that lies", revoked)
		}
		for _, wantStr := range []string{"partially printed", "WARNING", "until its expiration", "field-selector"} {
			if !strings.Contains(err.Error(), wantStr) {
				t.Errorf("message missing %q\n got: %v", wantStr, err)
			}
		}
	})
}

func TestCreateTokenWorkerHandoverEndsAtTheToken(t *testing.T) {
	prefix := "\nWorker join:\n\n  stratum up --worker --server https://cp:6443 --token abcdef.0123456789abcdef"
	for _, tc := range []struct {
		name   string
		accept int
		revoke bool
	}{
		{name: "nothing written", accept: 0, revoke: true},
		{name: "one byte short of the token", accept: len(prefix) - 1, revoke: true},
		{name: "whole token written", accept: len(prefix), revoke: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revoked []string
			c := okCmd(&bytes.Buffer{}, &revoked)
			c.out = &failingWriter{accept: tc.accept}
			if err := createToken("https://cp:6443", false, token.DefaultTokenTTL, c); err == nil {
				t.Fatal("a failed write was reported as success")
			}
			if got := len(revoked) == 1; got != tc.revoke {
				t.Errorf("revoked = %v, want revoked %v", revoked, tc.revoke)
			}
		})
	}
}

// The shared-cert decryption key lands in a 0600 file, never stdout, readable back.
func TestWriteHACertKeyFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // keep the key file inside the test's temp dir
	path, err := writeHACertKeyFile("s3cr3t-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600 (it holds the CA-decrypting key)", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "s3cr3t-key" {
		t.Errorf("key file content = %q, want the key back", string(b))
	}
}

func TestCreateTokenKeyFileHandover(t *testing.T) {
	for _, accepted := range []int{-1, 0, 20} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			var out bytes.Buffer
			var revoked []string
			c := okCmd(&out, &revoked)
			var path string
			c.writeKeyFile = func(key string) (string, error) {
				var err error
				path, err = writeHACertKeyFile(key)
				return path, err
			}
			if accepted >= 0 {
				c.out = &failingWriter{accept: accepted}
			}
			err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
			if (err != nil) != (accepted >= 0) {
				t.Fatalf("handover error: %v", err)
			}
			data, readErr := os.ReadFile(path)
			if accepted >= 0 {
				if !os.IsNotExist(readErr) || len(revoked) != 1 {
					t.Fatalf("zero handover must remove file and revoke: file=%v revoked=%v", readErr, revoked)
				}
			} else {
				if readErr != nil || string(data) != "deadbeef\n" || len(revoked) != 0 {
					t.Fatalf("handover must retain key and token: file=%v revoked=%v", readErr, revoked)
				}
			}
		})
	}
}

type handoverWriter func([]byte) (int, error)

func (w handoverWriter) Write(p []byte) (int, error) { return w(p) }

func TestCreateTokenIndependentHandoverBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		boundary                     string
		delta                        int
		keepToken, keepKey, shortNil bool
		extra                        string
	}{
		{name: "heading", boundary: "Control-plane (HA) join:", keepToken: false},
		{name: "partial token", boundary: "abcdef.0123456789abcdef", delta: -1},
		{name: "complete token", boundary: "abcdef.0123456789abcdef", keepToken: true},
		{name: "partial path", boundary: "path", delta: -1, keepToken: true},
		{name: "complete path", boundary: "path", keepToken: true, keepKey: true},
		// The path was handed over; a later failure must not delete the file it names.
		{name: "complete path, then --cis", boundary: "path", keepToken: true, keepKey: true, extra: " --cis"},
		{name: "short nil error", boundary: "abcdef.0123456789abcdef", delta: -1, shortNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			var revoked []string
			c := okCmd(&bytes.Buffer{}, &revoked)
			c.joinExtra = tc.extra
			var path string
			c.writeKeyFile = func(key string) (string, error) {
				var err error
				path, err = writeHACertKeyFile(key)
				return path, err
			}
			c.out = handoverWriter(func(p []byte) (int, error) {
				needle := tc.boundary
				if needle == "path" {
					needle = shellQuote(path)
				}
				start := bytes.Index(p, []byte(needle))
				if start < 0 {
					t.Fatalf("output missing boundary %q", needle)
				}
				n := start + len(needle) + tc.delta
				if tc.shortNil {
					return n, nil
				}
				return n, errors.New("output failed")
			})
			err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
			if err == nil {
				t.Fatal("short output succeeded")
			}
			if (len(revoked) == 0) != tc.keepToken {
				t.Fatalf("revoked=%v, keepToken=%v", revoked, tc.keepToken)
			}
			data, readErr := os.ReadFile(path)
			if tc.keepKey {
				if readErr != nil || string(data) != "deadbeef\n" {
					t.Fatalf("key not retained: %v", readErr)
				}
				if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "delete") {
					t.Fatal("retained key not identified")
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("undisclosed key remains: %v", readErr)
			}
		})
	}
}

func TestTokenJoinCommandQuotesKeyPath(t *testing.T) {
	path := "/tmp/key space 'quote' ; $(printf SUB) `printf TICK`"
	var out bytes.Buffer
	var revoked []string
	c := okCmd(&out, &revoked)
	c.writeKeyFile = func(string) (string, error) { return path, nil }
	if err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c); err != nil {
		t.Fatal(err)
	}
	var join string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "stratum up --join") {
			join = line
		}
	}
	if join == "" {
		t.Fatal("missing HA command")
	}
	// Execute the printed command with a harmless stub, not a real join.
	got, err := exec.Command("sh", "-c", "stratum() { printf '%s\\n' \"$@\"; };\n"+join).CombinedOutput()
	if err != nil {
		t.Fatalf("printed command failed: %v: %s", err, got)
	}
	args := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(args) != 9 || args[7] != "--certificate-key-file" || args[8] != path {
		t.Fatalf("shell did not preserve key path as one literal argument: %q", args)
	}
}

func TestKeyFileRejectsMultilineTempDirectory(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()+"\nambiguous")
	if _, err := writeHACertKeyFile("secret"); err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Fatalf("want explicit multiline-path rejection, got %v", err)
	}
}

// Floored, never rounded up: the operator is never told there is more time than there is.
func TestRemainingFloors(t *testing.T) {
	if got := remaining(91 * time.Second); got != "1m0s" {
		t.Errorf("remaining(1m31s) = %q, want 1m0s (floored)", got)
	}
	if got := remaining(30 * time.Second); got != "<1m" {
		t.Errorf("remaining(30s) = %q, want <1m", got)
	}
	if got := remaining(2 * time.Hour); got != "2h0m0s" {
		t.Errorf("remaining(2h) = %q, want 2h0m0s", got)
	}
}

// A negative --ttl must fail before minting; 0 stays the non-expiring opt-out.
func TestTokenCreateRejectsNegativeTTL(t *testing.T) {
	cmd := newTokenCreateCommand()
	cmd.SetArgs([]string{"--ttl", "-1h"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--ttl must be >= 0") {
		t.Fatalf("want a negative-ttl rejection before minting, got %v", err)
	}
}

// A node without a VIP defaults to its recorded address; --server still overrides.
func TestTokenCreateDefaultServerWithoutAVIP(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	// $NODE_IP differs, so the pin proves the recorded address wins over fresh detection.
	t.Setenv("NODE_IP", "10.9.9.9")
	if err := nodestate.SaveState(&nodestate.NodeState{NodeIP: "192.168.99.12"}); err != nil {
		t.Fatal(err)
	}

	got, err := defaultServerURL("")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://192.168.99.12:6443"; got != want {
		t.Errorf("default server = %q, want %q (the recorded node IP, no VIP)", got, want)
	}
	if got, err := defaultServerURL("https://10.0.0.10:6443"); err != nil || got != "https://10.0.0.10:6443" {
		t.Errorf("explicit --server = %q, %v; want it returned unchanged", got, err)
	}
}

// The refusal must keep its own reason and name --server, or it is a dead end.
func TestTokenCreateRefusalNamesTheServerFlag(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state nodestate.NodeState
		why   string // the refusal's own reason, which the hint must not replace
	}{
		{name: "a link-local node address and no VIP", state: nodestate.NodeState{NodeIP: "169.254.1.1"}, why: "link-local"},
		{name: "a loopback VIP", state: nodestate.NodeState{NodeIP: "192.168.99.12", VIP: nodestate.VIP{Address: "127.0.0.1"}}, why: "loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := version.K8sConfDir
			version.K8sConfDir = t.TempDir()
			t.Cleanup(func() { version.K8sConfDir = orig })
			if err := nodestate.SaveState(&tc.state); err != nil {
				t.Fatal(err)
			}

			got, err := defaultServerURL("")
			if err == nil {
				t.Fatalf("default server = %q, want a refusal", got)
			}
			if msg := err.Error(); !strings.Contains(msg, tc.why) || !strings.Contains(msg, "--server") {
				t.Errorf("refusal %q; want it to keep its reason %q and name --server", msg, tc.why)
			}
			const lb = "https://lb.example:6443"
			if got, err := defaultServerURL(lb); err != nil || got != lb {
				t.Errorf("explicit --server = %q, %v; want %q returned unchanged", got, err, lb)
			}
		})
	}
}

// The flags must come from token.LoadJoinFlags, so CLI and MCP tool cannot disagree.
func TestCLIJoinFlagsAreTheSharedDerivation(t *testing.T) {
	registerCISForTest(t)
	for _, tc := range []struct {
		name      string
		netconfig []string // nil = lost (never written): recover from node state
		state     nodestate.NodeState
		wantExtra string // the worker line's rendered flags
		wantCIS   string // rendered on both join lines
		wantFlags token.JoinFlags
	}{
		{
			name:      "a default cluster",
			state:     nodestate.NodeState{Role: "controller"},
			wantExtra: "",
			wantCIS:   "",
			wantFlags: token.JoinFlags{},
		},
		{
			name:      "a CIS cluster",
			state:     nodestate.NodeState{Role: "controller", CIS: true},
			wantExtra: "",
			wantCIS:   " --profile cis",
			wantFlags: token.JoinFlags{Profile: "cis"},
		},
		{
			name:      "custom ranges and domain",
			netconfig: []string{"10.42.0.0/16", "10.43.0.0/16", "fuzz.internal"},
			state:     nodestate.NodeState{Role: "controller"},
			wantExtra: " --pod-cidr '10.42.0.0/16' --service-cidr '10.43.0.0/16' --cluster-domain 'fuzz.internal'",
			wantCIS:   "",
			wantFlags: token.JoinFlags{PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "fuzz.internal"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldConf, oldNet, oldPKI := version.K8sConfDir, version.NetConfigPath, version.PKIDir
			t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath, version.PKIDir = oldConf, oldNet, oldPKI })
			version.K8sConfDir = t.TempDir()
			version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
			version.PKIDir = t.TempDir()

			if tc.netconfig != nil {
				if err := os.WriteFile(version.NetConfigPath, []byte(strings.Join(tc.netconfig, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := nodestate.SaveState(&tc.state); err != nil {
				t.Fatal(err)
			}

			extra, err := workerJoinExtra()
			if err != nil || extra != tc.wantExtra {
				t.Errorf("workerJoinExtra = %q, %v; want %q", extra, err, tc.wantExtra)
			}
			cis, err := profileJoinFlag()
			if err != nil || cis != tc.wantCIS {
				t.Errorf("profileJoinFlag = %q, %v; want %q", cis, err, tc.wantCIS)
			}
			flags, err := token.LoadJoinFlags()
			if err != nil || flags != tc.wantFlags {
				t.Errorf("token.LoadJoinFlags = %+v, %v; want %+v", flags, err, tc.wantFlags)
			}
		})
	}
}

// The key's bytes reach neither stdout nor an error message; naming its path is fine.
func TestCreateTokenNeverPutsTheCertificateKeyInTheErrorOrTheOutput(t *testing.T) {
	t.Run("the shared-certificate upload fails", func(t *testing.T) {
		var out bytes.Buffer
		var revoked []string
		c := okCmd(&out, &revoked)
		c.uploadCerts = func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
			return errors.New("boom")
		}
		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if err == nil {
			t.Fatal("no error returned")
		}
		if strings.Contains(out.String(), "deadbeef") || strings.Contains(err.Error(), "deadbeef") {
			t.Errorf("the raw certificate-key leaked to stdout or the error:\n%s\n%v", out.String(), err)
		}
	})

	t.Run("the key file write fails", func(t *testing.T) {
		var out bytes.Buffer
		var revoked []string
		c := okCmd(&out, &revoked)
		c.writeKeyFile = func(string) (string, error) { return "", errors.New("boom") }
		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if err == nil {
			t.Fatal("no error returned")
		}
		if strings.Contains(out.String(), "deadbeef") || strings.Contains(err.Error(), "deadbeef") {
			t.Errorf("the raw certificate-key leaked to stdout or the error:\n%s\n%v", out.String(), err)
		}
	})

	// The key's path was handed over; the error must point at the file, not the key.
	t.Run("the key file was retained when the output failed", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		var revoked []string
		c := okCmd(&bytes.Buffer{}, &revoked)
		var path string
		c.writeKeyFile = func(key string) (string, error) {
			var err error
			path, err = writeHACertKeyFile(key)
			return path, err
		}
		c.out = handoverWriter(func(p []byte) (int, error) {
			start := bytes.Index(p, []byte(shellQuote(path)))
			if start < 0 {
				t.Fatalf("output missing the key file path %q", path)
			}
			return start + len(shellQuote(path)), errors.New("output failed")
		})
		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if err == nil {
			t.Fatal("a failed write was reported as success")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("the retained-key error does not name the file: %v", err)
		}
		if strings.Contains(err.Error(), "deadbeef") {
			t.Errorf("the retained-key error carries the raw key: %v", err)
		}
	})
}

// The VIP is the one address that survives the minting controller going away.
func TestTokenCreateNamesTheVIP(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	// $NODE_IP differs, so the pin proves the recorded address wins over fresh detection.
	t.Setenv("NODE_IP", "10.9.9.9")
	if err := nodestate.SaveState(&nodestate.NodeState{
		NodeIP: "192.168.99.12",
		VIP:    nodestate.VIP{Address: "10.0.0.10"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := defaultServerURL("")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://10.0.0.10:6443"; got != want {
		t.Errorf("default server = %q, want the VIP %q: a join line naming this control plane strands the joiner when this controller fails", got, want)
	}
	if got, err := defaultServerURL("https://lb.example:6443"); err != nil || got != "https://lb.example:6443" {
		t.Errorf("explicit --server = %q, %v; want it returned unchanged", got, err)
	}

	// The rule reaches the operator through the printed line, so pin it end to end.
	var out bytes.Buffer
	var revoked []string
	if err := createToken("https://10.0.0.10:6443", false, token.DefaultTokenTTL, okCmd(&out, &revoked)); err != nil {
		t.Fatal(err)
	}
	if want := "stratum up --worker --server https://10.0.0.10:6443 --token abcdef.0123456789abcdef"; !strings.Contains(out.String(), want) {
		t.Errorf("printed worker join line does not name the VIP:\n%s", out.String())
	}
}

// The URL brackets the IPv6 address; createToken gets defaultServerURL's output, never a literal.
func TestTokenCreateJoinURLParsesBackOnIPv6(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state nodestate.NodeState
		want  string // the address both join lines must name
	}{
		{
			name:  "an IPv6 node address and no VIP",
			state: nodestate.NodeState{NodeIP: "fd00::12"},
			want:  "fd00::12",
		},
		{
			// The node's address differs from the VIP, so this also proves the VIP wins.
			name:  "an IPv6 VIP",
			state: nodestate.NodeState{NodeIP: "fd00::12", VIP: nodestate.VIP{Address: "fd00::10"}},
			want:  "fd00::10",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := version.K8sConfDir
			version.K8sConfDir = t.TempDir()
			t.Cleanup(func() { version.K8sConfDir = orig })
			// $NODE_IP differs, so the pin proves the recorded address wins over fresh detection.
			t.Setenv("NODE_IP", "10.9.9.9")
			if err := nodestate.SaveState(&tc.state); err != nil {
				t.Fatal(err)
			}

			server, err := defaultServerURL("")
			if err != nil {
				t.Fatal(err)
			}
			checkJoinURL(t, "default server", server, tc.want)

			for _, cp := range []bool{false, true} {
				flag := "--server"
				if cp {
					flag = "--join"
				}
				var out bytes.Buffer
				var revoked []string
				if err := createToken(server, cp, token.DefaultTokenTTL, okCmd(&out, &revoked)); err != nil {
					t.Fatal(err)
				}
				checkJoinURL(t, "printed "+flag, joinLineValue(t, out.String(), flag), tc.want)
			}
		})
	}
}

// A URL holds one host, so the line must name the v4,v6 pair's primary.
func TestTokenCreateDefaultServerNamesTheDualStackPrimary(t *testing.T) {
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
	// No SaveState: without a state file the node's address comes from $NODE_IP.
	t.Setenv("NODE_IP", "10.0.0.7,fd00::7")

	got, err := defaultServerURL("")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://10.0.0.7:6443"; got != want {
		t.Errorf("default server = %q, want %q (the primary of the node's v4,v6 address)", got, want)
	}
}

// Returns the field an operator copies; a missing line or flag fails the test.
func joinLineValue(t *testing.T, out, flag string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "stratum" || fields[1] != "up" {
			continue
		}
		if i := slices.Index(fields, flag); i >= 0 && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Fatalf("no printed `stratum up` line carries %s:\n%s", flag, out)
	return ""
}

// Compares by value on the apiserver port; an unbracketed IPv6 host must fail.
func checkJoinURL(t *testing.T, what, raw, want string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Errorf("%s %q does not parse as a URL: %v", what, raw, err)
		return
	}
	if u.Scheme != "https" {
		t.Errorf("%s %q has scheme %q, want https", what, raw, u.Scheme)
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		t.Errorf("%s %q has a host:port a joiner cannot dial: %v", what, raw, err)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.Equal(net.ParseIP(want)) {
		t.Errorf("%s %q names host %q, want the address %s", what, raw, u.Hostname(), want)
	}
	if port := strconv.Itoa(version.APIServerPort); u.Port() != port {
		t.Errorf("%s %q names port %q, want %s", what, raw, u.Port(), port)
	}
}

func TestCreateTokenHoldsTheMaintenanceFence(t *testing.T) {
	fencingCmd := func(out *bytes.Buffer, events *[]string) tokenCmd {
		c := okCmd(out, new([]string))
		c.maintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
			*events = append(*events, "fence")
			return &k8sutil.MaintenanceLease{Release: func() { *events = append(*events, "fence released") }}, nil
		}
		create := c.create
		c.create = func(w io.Writer, url string, ttl time.Duration, cp bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			*events = append(*events, "mint")
			return create(w, url, ttl, cp, nil)
		}
		key := c.newCertKey
		c.newCertKey = func() (string, error) {
			*events = append(*events, "new cert key")
			return key()
		}
		up := c.uploadCerts
		c.uploadCerts = func(certKey string, owner *metav1.OwnerReference, _ func(context.Context) error) error {
			*events = append(*events, "upload")
			return up(certKey, owner, nil)
		}
		return c
	}

	for _, cp := range []bool{false, true} {
		name := "a worker mint holds the fence"
		if cp {
			name = "a --control-plane mint holds the fence"
		}
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			events := &[]string{}
			if err := createToken("https://cp:6443", cp, token.DefaultTokenTTL, fencingCmd(&out, events)); err != nil {
				t.Fatal(err)
			}
			want := []string{"fence", "mint"}
			if cp {
				want = append(want, "new cert key", "upload")
			}
			want = append(want, "fence released")
			if !slices.Equal(*events, want) {
				t.Errorf("the mint ran %v, want %v: the fence must be held before the mint and released only once the command is done", *events, want)
			}
		})
	}

	t.Run("released even when a later step fails", func(t *testing.T) {
		var out bytes.Buffer
		events := &[]string{}
		c := fencingCmd(&out, events)
		boom := errors.New("boom")
		c.newCertKey = func() (string, error) { *events = append(*events, "new cert key"); return "", boom }
		if err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the step's own cause", err)
		}
		if got := *events; !slices.Equal(got, []string{"fence", "mint", "new cert key", "fence released"}) {
			t.Errorf("the mint ran %v; the fence must be released even on a failing mint", got)
		}
	})

	t.Run("a held fence refuses the mint before anything is minted", func(t *testing.T) {
		var out bytes.Buffer
		revoked := 0
		events := &[]string{}
		c := fencingCmd(&out, events)
		// The refusal's wording is maintlease's own: the holder and its renewal age.
		held := errors.New("another maintenance run is in progress (cp1, renewed 5s ago)")
		c.maintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
			*events = append(*events, "fence")
			return nil, held
		}
		savedRevoke := c.revoke
		c.revoke = func(tok string, cause error) error {
			revoked++
			return savedRevoke(tok, cause)
		}
		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if !errors.Is(err, held) {
			t.Fatalf("err = %v, want the fence refusal itself, with the holder's identity", err)
		}
		if got := *events; !slices.Equal(got, []string{"fence"}) {
			t.Errorf("the mint ran %v, want only the refused fence: nothing may be minted, keyed or uploaded under another run's fence", got)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q for a mint that never happened", out.String())
		}
		if revoked != 0 {
			t.Errorf("revoked %d token(s): the fence is taken before the token exists, so there is nothing to revoke", revoked)
		}
	})

	// A worker mint republishes cluster trust too, so it holds the fence and guard.
	t.Run("a worker mint consults the re-key guard before minting", func(t *testing.T) {
		var out bytes.Buffer
		events := &[]string{}
		c := fencingCmd(&out, events)
		refusal := errors.New("a cluster-wide CA rotation or etcd-CA migration has not finished")
		c.rekeyGuard = func() error {
			*events = append(*events, "guard")
			return refusal
		}
		err := createToken("https://cp:6443", false, token.DefaultTokenTTL, c)
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v, want the guard refusal itself", err)
		}
		if got := *events; !slices.Equal(got, []string{"fence", "guard", "fence released"}) {
			t.Errorf("the mint ran %v, want fence then guard: the refusal must precede the cluster-info publication", got)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q for a mint that never happened", out.String())
		}
	})
}

// The refusal comes before a token or a bundle exists, and carries the cis note.
func TestTokenCreateRefusesAProfileThisBinaryDoesNotRegister(t *testing.T) {
	registerProfilesForTest(t)
	oldConf, oldNet := version.K8sConfDir, version.NetConfigPath
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = oldConf, oldNet })
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", Profile: profile.CISName, NodeIP: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--control-plane"}, {}} {
		cmd := newTokenCreateCommand()
		cmd.SetArgs(append([]string{"--server", "https://cp:6443"}, args...))
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		err := cmd.Execute()
		if !errors.Is(err, profile.ErrUnregistered) {
			t.Fatalf("token create %v = %v, want the refusal wrapping profile.ErrUnregistered before anything is minted", args, err)
		}
		for _, want := range []string{profile.CISUnregisteredNote, `profile "cis"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("token create %v: refusal is missing %q: %v", args, want, err)
			}
		}
		if out.Len() != 0 {
			t.Errorf("token create %v printed %q for a mint that never happened", args, out.String())
		}
	}
}
