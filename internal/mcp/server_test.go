package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServeHTTPReturnsWhenItCannotBind is the regression test for a hang,
// not for an error message.
func TestServeHTTPReturnsWhenItCannotBind(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), NewServer(Options{}), Options{
			Transport: "http", TokenFile: tf,
			Addr:   ln.Addr().String(),
			Logger: discardLogger(),
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("binding an occupied port reported success")
		}
		if !strings.Contains(err.Error(), ln.Addr().String()) {
			t.Errorf("error does not name the address it failed on: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after a bind failure while ctx was still live")
	}
}

// TestServeHTTPStopsOnCancel: cancellation is the only way this server is
// meant to stop, and it has to drain rather than report the close as a
// failure.
func TestServeHTTPStopsOnCancel(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, NewServer(Options{}), Options{
			Transport: "http", TokenFile: tf,
			Addr:   "127.0.0.1:0",
			Logger: discardLogger(),
		})
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cancelled server reported an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

func TestServeRejectsUnknownTransport(t *testing.T) {
	err := Serve(context.Background(), NewServer(Options{}), Options{Transport: "carrier-pigeon"})
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("want an error naming the transport, got %v", err)
	}
}

// TestRefusesNonLoopbackBind: the HTTP transport has no TLS, so
// --addr is the one input that turns a local tool into a network service.
// A log line is not a control; refuse, and name the opt-out.
func TestRefusesNonLoopbackBind(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	// Bounded, not Background: if the refusal ever regresses, Serve would
	// happily serve until the package test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Serve(ctx, NewServer(Options{}), Options{
		Transport: "http", TokenFile: tf,
		Addr:   "0.0.0.0:0",
		Logger: discardLogger(),
	})
	if err == nil {
		t.Fatal("a 0.0.0.0 bind was accepted")
	}
	if !strings.Contains(err.Error(), "--insecure-listen") {
		t.Errorf("the refusal does not name its opt-out: %v", err)
	}
}

// TestInsecureListenIsTheOnlyWayPast keeps the refusal from being a wall
// with no door — an operator who has put TLS in front of this
// has to be able to say so.
func TestInsecureListenIsTheOnlyWayPast(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, NewServer(Options{}), Options{
			Transport:      "http",
			Addr:           "0.0.0.0:0",
			InsecureListen: true,
			TokenFile:      tf,
			Logger:         discardLogger(),
		})
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("--insecure-listen still refused the bind: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// TestLoopbackIsDecidedByTheBoundSocket.
func TestLoopbackIsDecidedByTheBoundSocket(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	for _, tc := range []struct {
		addr       string
		wantServed bool
	}{
		{"127.0.0.1:0", true},
		{"[::1]:0", true},
		{"0.0.0.0:0", false},
		{"[::]:0", false},
		{":0", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, NewServer(Options{}), Options{
					Transport: "http", TokenFile: tf, Addr: tc.addr, Logger: discardLogger(),
				})
			}()

			if !tc.wantServed {
				select {
				case err := <-done:
					if err == nil {
						t.Fatalf("%s was accepted as loopback", tc.addr)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s was served instead of refused", tc.addr)
				}
				return
			}

			// Served: nothing comes back until we cancel.
			select {
			case err := <-done:
				t.Fatalf("%s was refused: %v", tc.addr, err)
			case <-time.After(200 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s: %v", tc.addr, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not stop on cancellation", tc.addr)
			}
		})
	}
}

// TestHostnameFollowsWhereItBinds is the localhost case, written as the
// property rather than as an expected outcome.
func TestHostnameFollowsWhereItBinds(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	probe, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Skipf("this host cannot bind localhost: %v", err)
	}
	wantServed := probe.Addr().(*net.TCPAddr).IP.IsLoopback()
	_ = probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, NewServer(Options{}), Options{
			Transport: "http", TokenFile: tf, Addr: "localhost:0", Logger: discardLogger(),
		})
	}()

	select {
	case err := <-done:
		if wantServed {
			t.Errorf("localhost binds %v (loopback) but Serve refused: %v", probe.Addr(), err)
		} else if err == nil {
			t.Errorf("localhost binds %v (not loopback) and Serve served it anyway", probe.Addr())
		}
		return
	case <-time.After(300 * time.Millisecond):
	}

	if !wantServed {
		t.Fatalf("localhost binds %v (not loopback) but Serve did not refuse", probe.Addr())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serving localhost stopped with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop on cancellation")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestVersionGetOverStreamableHTTP drives a real MCP request through the
// HTTP transport.
func TestVersionGetOverStreamableHTTP(t *testing.T) {
	tf := tokenFile(t, testToken, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := Options{Version: "v1.2.3-test", Transport: "http", TokenFile: tf, Addr: freePort(t), Logger: discardLogger()}

	srv := NewServer(opts)
	sdk.AddTool(srv, &sdk.Tool{Name: "version_get", Description: "report the version"},
		VersionHandler(opts.Version))

	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, opts) }()
	waitForListener(t, opts.Addr, served)

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: "http://" + opts.Addr, HTTPClient: &http.Client{Transport: bearer{testToken}}}, nil)
	if err != nil {
		t.Fatalf("connect over streamable HTTP: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list over HTTP: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "version_get") {
		t.Fatalf("tools/list over HTTP returned %q, want version_get among them", names)
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "version_get"})
	if err != nil {
		t.Fatalf("tools/call over HTTP: %v", err)
	}
	if res.IsError {
		t.Fatalf("version_get reported an error: %+v", res.Content)
	}
	var out VersionOut
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if out.Version != opts.Version {
		t.Errorf("version over HTTP = %q, want %q", out.Version, opts.Version)
	}
	if out.OS == "" || out.Arch == "" {
		t.Errorf("os/arch not populated over HTTP: %+v", out)
	}

	// Bounded under Serve's shutdown grace, which is what this guards: an
	// abandoned SSE stream never goes idle, so without closing sessions first
	// Shutdown waits the grace out. Not tighter. A streamable client
	// reconnects 1-2s after its stream ends (the SDK's 1s backoff plus up to
	// 1s of jitter), and http.Server.Shutdown re-checks connections on a
	// backoff that reaches 500ms, so a sound shutdown that meets a reconnect
	// can take ~2.1s. A bound tighter than that fails sound shutdowns that
	// reconnect mid-grace; the grace period is the backstop, not the
	// measure.
	stopping := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serving stopped with an error: %v", err)
		}
		if took := time.Since(stopping); took > httpShutdownGrace-time.Second {
			t.Errorf("shutdown with a live HTTP session took %v", took)
		}
	case <-time.After(10 * time.Second):
		t.Error("Serve did not return after cancellation with a live HTTP session")
	}
}

// freePort returns a loopback address nothing is listening on. Serve takes
// an address rather than a listener, so the port has to be named up front.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// waitForListener blocks until addr accepts a connection, failing fast if
// Serve gives up first.
func waitForListener(t *testing.T, addr string, served <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-served:
			t.Fatalf("Serve returned before it was listening: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after 10s", addr)
}
