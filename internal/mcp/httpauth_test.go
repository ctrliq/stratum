package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testToken is long enough to pass readToken's length floor.
const testToken = "0123456789abcdef0123456789abcdef"

// tokenFile writes tok to a private file and returns its path.
func tokenFile(t *testing.T, tok string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp-token")
	if err := os.WriteFile(p, []byte(tok+"\n"), mode); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return p
}

// bearer adds an Authorization header to every request.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// TestHTTPRefusesCallsWithoutTheToken: loopback is reachable by every local
// user and every hostNetwork pod, so a tools/call without the bearer token
// is refused before the tool runs — even one that rides a session a
// legitimate client opened.
func TestHTTPRefusesCallsWithoutTheToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := Options{Transport: "http", Addr: freePort(t), TokenFile: tokenFile(t, testToken, 0o600), Logger: discardLogger()}
	srv := NewServer(opts)
	var ran atomic.Int32
	sdk.AddTool(srv, &sdk.Tool{Name: "canary", Description: "records that it ran"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, struct{}, error) {
			ran.Add(1)
			return nil, struct{}{}, nil
		})

	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, opts) }()
	waitForListener(t, opts.Addr, served)
	endpoint := "http://" + opts.Addr

	connect := func(c *http.Client) (*sdk.ClientSession, error) {
		return sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
			Connect(ctx, &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: c, MaxRetries: -1}, nil)
	}
	for name, c := range map[string]*http.Client{
		"no token":    http.DefaultClient,
		"wrong token": {Transport: bearer{strings.Repeat("x", len(testToken))}},
	} {
		if cs, err := connect(c); err == nil {
			_ = cs.Close()
			t.Errorf("%s: the session was established", name)
		}
	}

	cs, err := connect(&http.Client{Transport: bearer{testToken}})
	if err != nil {
		t.Fatalf("with the token: connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	// A raw tools/call on the authenticated session, minus the token.
	body := `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"canary","arguments":{}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", cs.ID())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw tools/call: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tools/call without the token: status %d, want 401", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("401 does not name the Bearer scheme: %q", resp.Header.Get("WWW-Authenticate"))
	}
	if n := ran.Load(); n != 0 {
		t.Fatalf("the tool ran %d time(s) without the token", n)
	}

	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "canary"}); err != nil {
		t.Fatalf("tools/call with the token: %v", err)
	}
	if ran.Load() != 1 {
		t.Errorf("the tool did not run with the token")
	}
}

// TestHTTPRefusesToStartWithoutAPrivateToken: a token file other users can
// read, or a token short enough to guess, authenticates no one.
func TestHTTPRefusesToStartWithoutAPrivateToken(t *testing.T) {
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"no token file":  {"", "--token-file"},
		"missing file":   {filepath.Join(t.TempDir(), "absent"), "absent"},
		"group readable": {tokenFile(t, testToken, 0o640), "0600"},
		"world readable": {tokenFile(t, testToken, 0o644), "0600"},
		"empty":          {tokenFile(t, "", 0o600), "at least"},
		"short":          {tokenFile(t, "hunter2", 0o600), "at least"},
		"directory":      {privateDir(t), "not a regular file"},
		"fifo":           {fifo(t), "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := Serve(ctx, NewServer(Options{}), Options{
				Transport: "http", Addr: "127.0.0.1:0", TokenFile: tc.path, Logger: discardLogger(),
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
}

// TestHTTPRefusesATokenFileOwnedBySomeoneElse: whoever owns the file can
// read it whatever its mode, so a file another user owns hands them the
// server's root.
func TestHTTPRefusesATokenFileOwnedBySomeoneElse(t *testing.T) {
	old := geteuid
	t.Cleanup(func() { geteuid = old })
	geteuid = func() uint32 { return old() + 1 }

	// Bounded: if the check regresses, Serve serves until ctx ends.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := Serve(ctx, NewServer(Options{}), Options{
		Transport: "http", Addr: "127.0.0.1:0", TokenFile: tokenFile(t, testToken, 0o600), Logger: discardLogger(),
	})
	if err == nil || !strings.Contains(err.Error(), "not owned by") {
		t.Errorf("want a refusal naming the owner, got %v", err)
	}
}

// privateDir returns a 0700 directory: private, but not a file.
func privateDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "dir")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// fifo returns a 0600 FIFO with no writer, which would block a plain open.
func fifo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRequireBearerScheme: the auth scheme is case-insensitive (RFC 9110
// 11.1); the token is not.
func TestRequireBearerScheme(t *testing.T) {
	h := requireBearer(testToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for header, want := range map[string]int{
		"Bearer " + testToken:                  http.StatusOK,
		"bearer " + testToken:                  http.StatusOK,
		"BEARER " + testToken:                  http.StatusOK,
		"bearer " + strings.Repeat("x", 32):    http.StatusUnauthorized,
		"Bearer " + strings.ToUpper(testToken): http.StatusUnauthorized,
		"Basic " + testToken:                   http.StatusUnauthorized,
		testToken:                              http.StatusUnauthorized,
		"":                                     http.StatusUnauthorized,
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, rec.Code, want)
		}
	}
}
