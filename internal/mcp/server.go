// Package mcp serves Stratum's Model Context Protocol tools.
package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultAddr is where the HTTP transport listens when --addr is not given.
const DefaultAddr = "127.0.0.1:8092"

// Options configure a server. The zero value is a read-only stdio server.
type Options struct {
	Version string

	AllowWrite       bool
	AllowDestructive bool

	Transport      string
	Addr           string
	InsecureListen bool
	// TokenFile holds the bearer token every HTTP request must carry.
	TokenFile string

	// Logger receives diagnostics. Nil means a stderr logger.
	Logger *slog.Logger
}

// NewLogger returns the configured logger, defaulting to stderr.
func (o Options) NewLogger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// NewServer builds the Stratum MCP server. Every tool result it returns is
// fenced as untrusted data (see Fence).
func NewServer(o Options) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{
		Name:    "stratum",
		Version: o.Version,
	}, &sdk.ServerOptions{Instructions: UntrustedNotice})
	srv.AddReceivingMiddleware(fenceToolResults)
	return srv
}

// Serve runs the server on the configured transport until ctx is done.
func Serve(ctx context.Context, srv *sdk.Server, o Options) error {
	log := o.NewLogger()
	switch o.Transport {
	case "", "stdio":
		log.Info("serving MCP over stdio",
			"write", o.AllowWrite, "destructive", o.AllowDestructive)
		// Signal and parent cancellation both report context.Canceled.
		// Both are expected session shutdowns, matching the HTTP path's
		// nil after shutdown; a transport failure keeps its error.
		if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil

	case "http":
		addr := o.Addr
		if addr == "" {
			addr = DefaultAddr
		}
		// Loopback is no boundary: every local user and every hostNetwork
		// pod can reach it, and the tools run as root.
		token, err := readToken(o.TokenFile)
		if err != nil {
			return err
		}
		var stopping atomic.Bool
		sdkHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
		h := refuseWhileStopping(&stopping, requireBearer(token, sdkHandler))

		// Bind first, then decide.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen for MCP over HTTP on %s: %w", addr, err)
		}
		bound := ln.Addr()

		// DefaultAddr's rationale is only a rationale until something acts on it.
		if !addrIsLoopback(bound) && !o.InsecureListen {
			_ = ln.Close()
			return fmt.Errorf("refusing to serve MCP on %s: the HTTP transport has no TLS, "+
				"so its bearer token would cross the network in the clear. "+
				"Bind loopback (%s), or pass --insecure-listen if something in front of it terminates TLS",
				bound, DefaultAddr)
		}
		if !addrIsLoopback(bound) {
			log.Warn("MCP HTTP transport is reachable from the network and has no TLS of its own",
				"addr", bound.String(), "write", o.AllowWrite, "destructive", o.AllowDestructive)
		}
		log.Info("serving MCP over streamable HTTP",
			"addr", bound.String(), "write", o.AllowWrite, "destructive", o.AllowDestructive)

		s := &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 20 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}

		errc := make(chan error, 1)
		go func() { errc <- s.Serve(ln) }()

		select {
		case err := <-errc:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve MCP over HTTP on %s: %w", bound, err)
			}
			return nil

		case <-ctx.Done():
			stopping.Store(true)
			s.SetKeepAlivesEnabled(false)
			closeSessions := func() {
				for session := range srv.Sessions() {
					_ = session.Close()
				}
			}
			closeSessions()
			shutCtx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
			defer cancel()
			shut := make(chan error, 1)
			go func() { shut <- s.Shutdown(shutCtx) }()
			sweep := time.NewTicker(sessionSweepInterval)
			defer sweep.Stop()
			for {
				select {
				case err := <-shut:
					if err != nil {
						log.Warn("MCP HTTP server did not shut down cleanly", "err", err)
						_ = s.Close()
					}
					<-errc
					return nil
				case <-sweep.C:
					closeSessions()
				}
			}
		}

	default:
		return fmt.Errorf("unknown transport %q: want stdio or http", o.Transport)
	}
}

// addrIsLoopback reports whether a bound listener is reachable only from
// this host.
func addrIsLoopback(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	if !ok || tcp.IP == nil {
		return false
	}
	return tcp.IP.IsLoopback()
}

// VersionIn is version_get's (empty) input.
type VersionIn struct{}

// VersionOut is version_get's result.
type VersionOut struct {
	Version string `json:"version" jsonschema:"the Stratum release this binary reports"`
	OS      string `json:"os" jsonschema:"the operating system the binary was built for"`
	Arch    string `json:"arch" jsonschema:"the CPU architecture the binary was built for"`
}

// VersionHandler returns the running binary's version.
func VersionHandler(display string) sdk.ToolHandlerFor[VersionIn, VersionOut] {
	return func(context.Context, *sdk.CallToolRequest, VersionIn) (*sdk.CallToolResult, VersionOut, error) {
		return nil, VersionOut{
			Version: display,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
		}, nil
	}
}

const sessionSweepInterval = 50 * time.Millisecond

// httpShutdownGrace is how long Serve lets the HTTP server drain before
// forcing its connections closed.
const httpShutdownGrace = 5 * time.Second

// minTokenLen is the shortest bearer token readToken accepts: 32 hex
// characters, 128 bits.
const minTokenLen = 32

// readToken loads the HTTP transport's bearer token. The file must be private
// to its owner: a token other local users can read authenticates no one.
func readToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("--transport=http needs --token-file: a file, mode 0600, holding the bearer token " +
			"clients must send (create one with: install -m 0600 /dev/null FILE && openssl rand -hex 32 > FILE)")
	}
	// O_NONBLOCK so a FIFO with no writer fails the regular-file check
	// instead of hanging the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("read MCP token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Checked on the open fd, so the inode checked is the one read.
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read MCP token file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("MCP token file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("MCP token file %s is mode %04o: other users can read it; chmod it to 0600", path, perm)
	}
	// The owner can read the file whatever its mode.
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != geteuid() {
		return "", fmt.Errorf("MCP token file %s is not owned by the user running the server (uid %d)", path, geteuid())
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read MCP token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < minTokenLen {
		return "", fmt.Errorf("MCP token in %s must be at least %d characters (openssl rand -hex 32)", path, minTokenLen)
	}
	return tok, nil
}

// geteuid is os.Geteuid, indirected for tests.
var geteuid = func() uint32 { return uint32(os.Geteuid()) }

// requireBearer refuses any request whose Authorization header is not
// "Bearer <token>", before it reaches the SDK and any tool. The scheme is
// case-insensitive (RFC 9110 11.1); the token is not.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, got, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="stratum-mcp"`)
			http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func refuseWhileStopping(stopping *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stopping.Load() {
			w.Header().Set("Connection", "close")
			http.Error(w, "MCP server is shutting down", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
