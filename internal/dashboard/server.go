// Package dashboard serves the SPA, an apiserver proxy, and sessions; each user acts as themselves.
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

// What /me and /config report as the authentication mechanism: only one, the cluster's own.
const provider = "kubernetes"

// server holds the handler dependencies.
type server struct {
	// Without credentials; a per-user client adds the caller's own bearer token.
	base     *rest.Config
	sessions *sessions
	proxy    *proxy
	spa      http.Handler
	origin   originPolicy

	nodeShell bool
	// The payload image the SPA must use for the node-shell debug pod; empty when the feature is off.
	nodeShellImage string
	// True when the browser reaches the dashboard over TLS, whichever side terminates it.
	https   bool
	version string
	log     *slog.Logger

	// One global limiter: the risk is an anonymous caller amplifying apiserver load.
	logins *rate.Limiter
}

// Middleware order, outermost first: request-id, access log, recover, headers, session, CSRF.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	csrf := s.sessions.csrf(s.origin)
	authed := func(h http.Handler) http.Handler { return s.sessions.requireSession(csrf(h)) }

	mux.HandleFunc("GET /healthz", healthz)

	// Unauthenticated: /config decides whether to show a login screen at all.
	mux.HandleFunc("GET /api/v1/config", s.getConfig)
	mux.Handle("POST /api/v1/login", s.origin.guard(http.HandlerFunc(s.login)))

	mux.Handle("GET /api/v1/me", authed(http.HandlerFunc(s.me)))
	mux.Handle("POST /api/v1/logout", authed(http.HandlerFunc(s.logout)))
	mux.Handle("GET "+secretSummariesPath, authed(http.HandlerFunc(s.secretSummaries)))

	// Both forms: {path...} does not match the bare prefix.
	mux.Handle(k8sPrefix+"/{path...}", authed(s.proxy))
	mux.Handle(k8sPrefix, authed(s.proxy))

	mux.Handle("/", s.spa)

	// recover sits inside accessLog to receive its recorder; else the written check never fires.
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

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "standalone",
		"version": s.version,
		"k8sBase": k8sPrefix,
		"auth":    map[string]any{"required": true, "providers": []string{"token"}},
		// Convenience only: checkNodeShell is the server-side gate.
		"features": map[string]any{"nodeShell": s.nodeShell, "nodeShellImage": s.nodeShellImage, "secretSummaries": true},
		"links": map[string]string{
			"docs": "https://docs.ciq.com/stratum",
			"pro":  "https://ciq.com/products/stratum",
		},
	})
}

type loginRequest struct {
	Token string `json:"token"`
}

// SelfSubjectReview, not TokenReview: issued as the user, no cluster permission of its own.
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
	// kubectl describe secret output and most docs put "Bearer " in front.
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
		// The token is good but the default system:basic-user binding is gone.
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

type identity struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	Provider string   `json:"provider"`
}

// Re-asks the apiserver on every call so a revoked token ends the session.
func (s *server) me(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	token, _ := tokenFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	who, err := s.whoami(ctx, token)
	switch {
	case apierrors.IsUnauthorized(err):
		// Without clearing the cookie: it may belong to a newer login; the CSRF still allows logout.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":     "unauthenticated",
			"message":   "the cluster no longer accepts this session's token",
			"csrfToken": s.sessions.csrfToken(sess),
		})
		return
	case err != nil:
		// Not an authentication failure; the cookie is signed and unexpired, so answer from it.
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

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessions.cookie("", true))
	s.log.Info("logout", "id", requestID(r.Context()), "actor", actorFrom(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// A nil error means the token authenticates; apierrors.IsUnauthorized means it does not.
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
		// parse() refuses an empty subject forever.
		return nil, apierrors.NewUnauthorized("the apiserver returned no username for this token")
	}
	return &rev.Status.UserInfo, nil
}

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
	// Decode stops after the first value; not dec.More(), which misses a stray '}' or ']'.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "bad-request", "unexpected data after the JSON body")
		return false
	}
	return true
}
