package dashboard

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// cookieName is the session cookie.
	cookieName = "stratum_session"
	// csrfHeader carries the per-session token on mutating requests.
	csrfHeader = "X-Stratum-CSRF"
	// defaultTTL is the session lifetime. A session cannot outlive it even if
	// the bearer token inside would still be accepted by the apiserver.
	defaultTTL = 12 * time.Hour
)

// errInvalidSession covers a missing, tampered or expired cookie.
var errInvalidSession = errors.New("invalid session")

// session is the cookie payload. Subject and Groups are what the apiserver
// said the token belongs to (SelfSubjectReview at login); Token is the bearer
// token itself, sealed.
//
// The envelope is base64url(payload) "." base64url(HMAC(payload)),
// which supplies integrity and a server-checked expiry. The payload carries
// a live credential, and a signature is not confidentiality — a signed
// cookie is readable by anyone holding it. So Token is separately sealed
// with AES-GCM under a key derived from the same secret.
// The HMAC still covers the sealed bytes, so neither layer can be stripped or
// swapped without the other noticing.
type session struct {
	Subject  string   `json:"sub"`
	Groups   []string `json:"grp,omitempty"`
	IssuedAt int64    `json:"iat"`
	Expires  int64    `json:"exp"`
	Nonce    string   `json:"nonce"`
	// Token is base64url(gcmNonce || ciphertext) of the user's bearer token.
	Token string `json:"tok"`
}

// sessions mints and verifies cookies.
type sessions struct {
	key    []byte
	aead   cipher.AEAD
	ttl    time.Duration
	secure bool
	now    func() time.Time
}

// newSessions derives both the signing key and the sealing key from secret.
//
// The keys are separated rather than shared: the sealing key encrypts a live
// cluster credential and the signing key's output is published in every
// cookie, and one secret used for two purposes is the kind of reuse that only
// looks harmless until one of the two primitives changes.
func newSessions(secret []byte, ttl time.Duration, secure bool) (*sessions, error) {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	sign := derive(secret, "sign")
	block, err := aes.NewCipher(derive(secret, "seal"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sessions{key: sign, aead: aead, ttl: ttl, secure: secure, now: time.Now}, nil
}

func derive(secret []byte, purpose string) []byte {
	sum := sha256.Sum256(append([]byte("stratum-dash:"+purpose+":"), secret...))
	return sum[:]
}

// newSessionSecret is the process's session secret: 32 random bytes, held in
// memory only, used when no --session-key-file is given.
func newSessionSecret() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

// minSessionKey is the shortest --session-key-file accepted.
const minSessionKey = 32

// loadSessionSecret reads the session secret from path, or makes an in-memory
// one when path is empty.
func loadSessionSecret(path string) ([]byte, error) {
	if path == "" {
		return newSessionSecret()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--session-key-file: %w", err)
	}
	if len(b) < minSessionKey {
		return nil, fmt.Errorf("--session-key-file %s holds %d bytes, want at least %d", path, len(b), minSessionKey)
	}
	return b, nil
}

// seal encrypts the bearer token. A fresh GCM nonce per session is required:
// reusing one under the same key is a total loss of confidentiality for both
// sessions involved.
func (m *sessions) seal(token string) (string, error) {
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := m.aead.Seal(nonce, nonce, []byte(token), nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

func (m *sessions) open(sealed string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(raw) < m.aead.NonceSize() {
		return "", errInvalidSession
	}
	nonce, ct := raw[:m.aead.NonceSize()], raw[m.aead.NonceSize():]
	pt, err := m.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", errInvalidSession
	}
	return string(pt), nil
}

// issue creates a session carrying token and returns the cookie value.
func (m *sessions) issue(subject string, groups []string, token string) (string, session, error) {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return "", session{}, err
	}
	sealed, err := m.seal(token)
	if err != nil {
		return "", session{}, err
	}
	now := m.now()
	s := session{
		Subject:  subject,
		Groups:   groups,
		IssuedAt: now.Unix(),
		Expires:  now.Add(m.ttl).Unix(),
		Nonce:    base64.RawURLEncoding.EncodeToString(nb[:]),
		Token:    sealed,
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return "", session{}, err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + m.sign(payload), s, nil
}

func (m *sessions) sign(payload []byte) string {
	h := hmac.New(sha256.New, m.key)
	h.Write(payload)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// parse verifies a cookie value: signature, then expiry, then that the
// payload still carries everything a session needs.
func (m *sessions) parse(value string) (session, error) {
	p, sig, ok := strings.Cut(value, ".")
	if !ok {
		return session{}, errInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return session{}, errInvalidSession
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(m.sign(payload))) != 1 {
		return session{}, errInvalidSession
	}
	var s session
	if err := json.Unmarshal(payload, &s); err != nil {
		return session{}, errInvalidSession
	}
	// Token is checked here and not only where it is used: a session with no
	// credential in it would otherwise reach the proxy and be forwarded with
	// no Authorization header at all.
	if s.Subject == "" || s.Nonce == "" || s.Token == "" || m.now().Unix() >= s.Expires {
		return session{}, errInvalidSession
	}
	return s, nil
}

// cookie builds the Set-Cookie for value; expire=true clears it.
func (m *sessions) cookie(value string, expire bool) *http.Cookie {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
	}
	if expire {
		c.MaxAge = -1
		c.Value = ""
	} else {
		c.MaxAge = int(m.ttl.Seconds())
	}
	return c
}

// csrfToken derives the CSRF token for a session: HMAC(key, "csrf:"+nonce).
func (m *sessions) csrfToken(s session) string {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte("csrf:" + s.Nonce))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (m *sessions) checkCSRF(s session, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(m.csrfToken(s))) == 1
}

// fromRequest reads and verifies the session cookie.
func (m *sessions) fromRequest(r *http.Request) (session, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return session{}, errInvalidSession
	}
	return m.parse(c.Value)
}

// ---- request context -------------------------------------------------------

type ctxKey int

const (
	ctxSession ctxKey = iota
	ctxToken
	ctxActor
	ctxRequestID
)

// actorHolder lets the access log, which runs outside the session layer, learn
// who the request ran as once the inner layer has resolved it.
type actorHolder struct{ subject string }

func withActorHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxActor, &actorHolder{})
}

// actorFrom is read by the access log after the handler has returned, so the
// holder is only ever written on the one goroutine serving the request and
// needs no lock.
func actorFrom(ctx context.Context) string {
	if h, ok := ctx.Value(ctxActor).(*actorHolder); ok {
		return h.subject
	}
	return ""
}

func sessionFrom(ctx context.Context) (session, bool) {
	s, ok := ctx.Value(ctxSession).(session)
	return s, ok
}

// tokenFrom returns the caller's bearer token, unsealed by requireSession.
// The proxy reads it here rather than unsealing again so there is exactly one
// place a cookie becomes a credential.
func tokenFrom(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(ctxToken).(string)
	return t, ok
}

// requireSession resolves the cookie, unseals the bearer token into the
// request context, or answers 401.
func (m *sessions) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := m.fromRequest(r)
		if err == nil {
			var token string
			if token, err = m.open(s.Token); err == nil {
				if h, ok := r.Context().Value(ctxActor).(*actorHolder); ok {
					h.subject = s.Subject
				}
				ctx := context.WithValue(r.Context(), ctxSession, s)
				ctx = context.WithValue(ctx, ctxToken, token)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "login required")
	})
}
