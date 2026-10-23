// Package dashboard serves the Stratum dashboard from the stratum binary:
// the embedded SPA, a reverse proxy to this cluster's own apiserver, and a
// session layer that authenticates users by ServiceAccount bearer-token
// passthrough.
//
// Every user acts as themselves. A login hands the server a bearer token,
// which the apiserver itself validates (SelfSubjectReview); the token is then
// sealed into a session cookie and forwarded verbatim on every proxied
// request. The dashboard is never a shared privileged identity, holds no
// accounts of its own, and its own ServiceAccount needs no cluster permissions
// at all — so what a user can see and change is exactly their own RBAC.
//
// It serves one cluster: the one it runs in.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// provider is what /me and /config report as the authentication mechanism.
// There is exactly one, and it is the cluster's.
const provider = "kubernetes"

// server holds the handler dependencies.
type server struct {
	// base is the apiserver connection WITHOUT credentials; a per-user client
	// is built from it by adding the caller's own bearer token.
	base     *rest.Config
	sessions *sessions
	proxy    *proxy
	spa      http.Handler
	origin   originPolicy

	nodeShell bool
	// nodeShellImage is the container image the SPA puts in the node-shell debug
	// pod. It is the payload-carried busybox (version.NsenterImage), served to
	// the SPA so it never hard-codes a public-registry tag on an airgapped node.
	// Empty when the feature is off.
	nodeShellImage string
	// https is true when the browser reaches the dashboard over TLS, whether
	// this process terminates it or something in front does. It decides HSTS,
	// and is the same answer the session cookie's Secure flag is built from.
	https   bool
	version string
	log     *slog.Logger

	// logins bounds how fast unauthenticated callers can make the dashboard
	// call the apiserver.
	//
	// One global limiter, not one per IP or per account: a bearer token is not
	// guessable, so there is no brute force to slow down here, and the only
	// thing worth bounding is the dashboard being used as an amplifier against
	// the apiserver by an unauthenticated caller. A shared bucket says exactly
	// that and needs no map and no eviction.
	logins *rate.Limiter
}

// routes assembles the mux. Middleware order, outermost first:
// request-id -> access log -> recover -> security headers -> (per route)
// session -> CSRF -> handler.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	csrf := s.sessions.csrf(s.origin)
	authed := func(h http.Handler) http.Handler { return s.sessions.requireSession(csrf(h)) }

	mux.HandleFunc("GET /healthz", healthz)

	// Unauthenticated: the SPA needs the config to know whether to show a
	// login screen at all, and login is how you get a session.
	mux.HandleFunc("GET /api/v1/config", s.getConfig)
	// Origin-guarded but session-free: login has no session to derive a CSRF
	// token from, and it sets a cookie, so a cross-site POST to it would fix
	// the attacker's identity onto the victim's browser. The guard runs ahead
	// of the rate limiter and the token, so a foreign origin costs neither.
	mux.Handle("POST /api/v1/login", s.origin.guard(http.HandlerFunc(s.login)))

	mux.Handle("GET /api/v1/me", authed(http.HandlerFunc(s.me)))
	mux.Handle("POST /api/v1/logout", authed(http.HandlerFunc(s.logout)))
	mux.Handle("GET "+secretSummariesPath, authed(http.HandlerFunc(s.secretSummaries)))

	// The Kubernetes proxy. Both forms: {path...} does not match the bare
	// prefix.
	mux.Handle(k8sPrefix+"/{path...}", authed(s.proxy))
	mux.Handle(k8sPrefix, authed(s.proxy))

	// The SPA, and JSON 404s for unmatched /api and /k8s paths.
	mux.Handle("/", s.spa)

	// Reads inside-out: the last wrap is the outermost. recover must sit
	// INSIDE accessLog so it receives the responseRecorder accessLog
	// installs — wrapped the other way it only ever sees the bare
	// ResponseWriter, so its "did the handler already write?" check can never
	// be true and it appends an error body to a partial response.
	var h http.Handler = mux
	h = securityHeaders(s.https)(h)
	h = recoverMiddleware(s.log)(h)
	h = accessLog(s.log)(h)
	h = requestIDMiddleware(h)
	return h
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// ---- GET /api/v1/config ----------------------------------------------------

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "standalone",
		"version": s.version,
		"k8sBase": k8sPrefix,
		"auth":    map[string]any{"required": true, "providers": []string{"token"}},
		// nodeShell lets the SPA hide a control it knows the server will
		// refuse. It is not the gate — checkNodeShell is, server-side,
		// before anything is forwarded. nodeShellImage is the payload image the
		// SPA must use for the debug pod, so it never falls back to a public
		// registry on an airgapped node.
		"features": map[string]any{"nodeShell": s.nodeShell, "nodeShellImage": s.nodeShellImage, "secretSummaries": true},
		"links":    map[string]string{"docs": "https://docs.ciq.com/stratum"},
	})
}

// ---- POST /api/v1/login ----------------------------------------------------

type loginRequest struct {
	Token string `json:"token"`
}

// login validates a bearer token by asking the apiserver who it belongs to,
// and seals it into a session on success.
//
// SelfSubjectReview rather than TokenReview, deliberately. TokenReview would
// need the dashboard's own ServiceAccount to hold `create tokenreviews`, a
// cluster-scoped privilege; SelfSubjectReview is issued AS the user, with the
// token under test, so the dashboard needs no permissions of its own at all.
// It also makes login and the proxy agree by construction: the answer comes
// from the same authentication the apiserver will perform on every forwarded
// request, including audience checks on a projected token, rather than from a
// second code path that could accept something the proxy then cannot use.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !s.logins.Allow() {
		writeJSONError(w, http.StatusTooManyRequests, "rate-limited", "too many sign-in attempts; try again shortly")
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.Token)
	// Tolerate a pasted "Bearer <token>": it is what `kubectl describe secret`
	// output and most docs put in front of a token, and rejecting it teaches
	// nobody anything.
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		writeJSONError(w, http.StatusBadRequest, "bad-request", "a bearer token is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	who, err := s.whoami(ctx, token)
	switch {
	case apierrors.IsUnauthorized(err):
		s.log.Info("login refused", "id", requestID(r.Context()), "remote", remoteIP(r), "reason", "the apiserver rejected the token")
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", "the cluster did not accept that token")
		return
	case apierrors.IsForbidden(err):
		// The user authenticated but may not ask who they are, which means
		// the cluster's default system:basic-user binding is gone. Say so:
		// the alternative is an operator staring at a token they know is good.
		s.log.Error("login cannot verify identity", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "auth-unavailable",
			"the cluster refused a selfsubjectreview for this token; the default system:basic-user ClusterRoleBinding is required")
		return
	case err != nil:
		s.log.Error("login backend", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "auth-unavailable", "the apiserver could not be reached")
		return
	}

	value, sess, err := s.sessions.issue(who.Username, who.Groups, token)
	if err != nil {
		s.log.Error("could not issue a session after a valid token", "subject", who.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not issue session")
		return
	}
	http.SetCookie(w, s.sessions.cookie(value, false))
	s.log.Info("login", "id", requestID(r.Context()), "actor", who.Username, "remote", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      identity{Username: who.Username, Groups: who.Groups, Provider: provider},
		"csrfToken": s.sessions.csrfToken(sess),
	})
}

// ---- GET /api/v1/me --------------------------------------------------------

type identity struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	Provider string   `json:"provider"`
}

// me re-asks the apiserver who the session's token belongs to on every call,
// so a token revoked or expired mid-session ends the session rather than
// leaving a signed-in dashboard that 401s on everything it tries.
func (s *server) me(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	token, _ := tokenFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	who, err := s.whoami(ctx, token)
	switch {
	case apierrors.IsUnauthorized(err):
		// Refuse the expired credential without changing the browser cookie:
		// this response may arrive after a newer login and would delete that
		// session too. Login replaces the cookie; explicit logout clears it.
		// The sealed dashboard session is still valid. Provide its CSRF so
		// explicit logout can clear that cookie without authenticating the UI.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":     "unauthenticated",
			"message":   "the cluster no longer accepts this session's token",
			"csrfToken": s.sessions.csrfToken(sess),
		})
		return
	case err != nil:
		// Anything that is not an authentication failure — an apiserver
		// restart, a network blip, a missing basic-user binding — must not
		// sign everyone out. The cookie is signed and unexpired, so answer
		// from it and say so in the log.
		s.log.Warn("could not refresh identity; answering from the session", "actor", sess.Subject, "err", err)
		who = &authenticationv1.UserInfo{Username: sess.Subject, Groups: sess.Groups}
	}

	groups := who.Groups
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":  who.Username,
		"groups":    groups,
		"provider":  provider,
		"csrfToken": s.sessions.csrfToken(sess),
	})
}

// ---- POST /api/v1/logout ---------------------------------------------------

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessions.cookie("", true))
	s.log.Info("logout", "id", requestID(r.Context()), "actor", actorFrom(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// ---- apiserver ------------------------------------------------------------

// whoami asks the apiserver to identify the holder of token. A nil error means
// the token authenticates; apierrors.IsUnauthorized means it does not.
func (s *server) whoami(ctx context.Context, token string) (*authenticationv1.UserInfo, error) {
	cfg := rest.AnonymousClientConfig(s.base)
	cfg.BearerToken = token
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client for the presented token: %w", err)
	}
	rev, err := client.AuthenticationV1().SelfSubjectReviews().
		Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if rev.Status.UserInfo.Username == "" {
		// Authenticated as nobody should not be reachable, but a session with
		// an empty subject would be one that parse() then refuses forever.
		return nil, apierrors.NewUnauthorized("the apiserver returned no username for this token")
	}
	return &rev.Status.UserInfo, nil
}

// ---- JSON helpers ----------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// decodeJSON reads a bounded JSON body, answering 400 itself on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "bad-request", "a JSON body is required")
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "bad-request", "malformed JSON body")
		return false
	}
	// Decode stops after the first value; anything but clean EOF (trailing
	// whitespace is fine) means the caller sent bytes we would silently drop.
	// Not dec.More(): it reports false before a stray '}' or ']'.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "bad-request", "unexpected data after the JSON body")
		return false
	}
	return true
}
