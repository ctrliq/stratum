package dashboard

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testSessions(t *testing.T) *sessions {
	t.Helper()
	m, err := newSessions([]byte("session-secret-material"), time.Hour, true)
	if err != nil {
		t.Fatalf("newSessions: %v", err)
	}
	return m
}

func TestSessionRoundTripsTheToken(t *testing.T) {
	m := testSessions(t)
	value, sess, err := m.issue("system:serviceaccount:demo:viewer", []string{"system:authenticated"}, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	got, err := m.parse(value)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Subject != sess.Subject || got.Nonce != sess.Nonce {
		t.Fatalf("parsed session does not match the issued one")
	}
	token, err := m.open(got.Token)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if token != goodToken {
		t.Fatalf("unsealed token = %q, want the one that was sealed", token)
	}
}

func TestSessionCookieDoesNotCarryTheTokenInTheClear(t *testing.T) {
	m := testSessions(t)
	value, _, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if strings.Contains(value, goodToken) {
		t.Fatal("the cookie value contains the bearer token verbatim")
	}
	payload, sig, _ := strings.Cut(value, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if strings.Contains(string(decoded), goodToken) {
		t.Fatal("the decoded cookie payload contains the bearer token; it is signed but not sealed")
	}
	if sig == "" {
		t.Fatal("the cookie carries no signature")
	}
}

func TestSealUsesAFreshNoncePerSession(t *testing.T) {
	m := testSessions(t)
	a, _, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	b, _, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if a == b {
		t.Fatal("two sessions for the same token are byte-identical")
	}
	sa, _ := m.parse(a)
	sb, _ := m.parse(b)
	if sa.Token == sb.Token {
		t.Fatal("the sealed token is identical across sessions; the GCM nonce is being reused")
	}
}

func TestSessionRefusesTamperedAndExpiredCookies(t *testing.T) {
	m := testSessions(t)
	value, _, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	payload, sig, _ := strings.Cut(value, ".")

	for _, tc := range []struct{ name, cookie string }{
		{"no signature", payload},
		{"empty", ""},
		{"wrong signature", payload + "." + strings.Repeat("A", len(sig))},
		{"unsigned payload swapped in", forgePayload(t, `{"sub":"admin","nonce":"n","exp":9999999999,"tok":"x"}`) + "." + sig},
		{"garbage payload", "!!!." + sig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.parse(tc.cookie); err == nil {
				t.Fatal("parse accepted a cookie it must refuse")
			}
		})
	}

	expired := testSessions(t)
	expired.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	old, _, err := expired.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := m.parse(old); err == nil {
		t.Fatal("parse accepted an expired session")
	}
}

func TestSessionWithoutATokenIsRefused(t *testing.T) {
	m := testSessions(t)
	s := session{Subject: "someone", Nonce: "n", Expires: time.Now().Add(time.Hour).Unix()}
	value := signSession(t, m, s)
	if _, err := m.parse(value); err == nil {
		t.Fatal("parse accepted a session carrying no bearer token")
	}
}

// Asserted by opening with the signing key: comparing derive outputs would restate it.
func TestTheSealingKeyIsNotTheSigningKey(t *testing.T) {
	m := testSessions(t)
	value, _, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	s, err := m.parse(value)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s.Token)
	if err != nil {
		t.Fatalf("decode sealed token: %v", err)
	}
	block, err := aes.NewCipher(m.key)
	if err != nil {
		t.Fatalf("aes with the signing key: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	if len(raw) < aead.NonceSize() {
		t.Fatalf("sealed token is %d bytes, too short to be GCM output", len(raw))
	}
	if _, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil); err == nil {
		t.Fatal("the sealed token opens with the signing key: one secret is doing both jobs")
	}
}

func TestSessionsFromAnotherSecretAreRefused(t *testing.T) {
	a := testSessions(t)
	b, err := newSessions([]byte("a completely different secret"), time.Hour, true)
	if err != nil {
		t.Fatalf("newSessions: %v", err)
	}
	value, _, err := a.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := b.parse(value); err == nil {
		t.Fatal("a session signed with another secret was accepted")
	}
}

func TestCSRFTokenIsBoundToTheSession(t *testing.T) {
	m := testSessions(t)
	_, a, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, b, err := m.issue("someone", nil, goodToken)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if m.csrfToken(a) == m.csrfToken(b) {
		t.Fatal("two sessions share a CSRF token")
	}
	if !m.checkCSRF(a, m.csrfToken(a)) {
		t.Fatal("a session's own CSRF token was refused")
	}
	if m.checkCSRF(a, m.csrfToken(b)) {
		t.Fatal("another session's CSRF token was accepted")
	}
	if m.checkCSRF(a, "") {
		t.Fatal("an empty CSRF token was accepted")
	}
}

func TestCookieAttributes(t *testing.T) {
	m := testSessions(t)
	c := m.cookie("v", false)
	if !c.HttpOnly {
		t.Error("the session cookie must be HttpOnly: it carries a bearer token")
	}
	if !c.Secure {
		t.Error("the session cookie must be Secure when the server serves TLS")
	}
	if c.SameSite != 3 {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if cleared := m.cookie("v", true); cleared.Value != "" || cleared.MaxAge != -1 {
		t.Errorf("clearing the cookie left value=%q maxAge=%d", cleared.Value, cleared.MaxAge)
	}
}

func forgePayload(t *testing.T, json string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(json))
}

func signSession(t *testing.T, m *sessions, s session) string {
	t.Helper()
	b := mustJSON(t, s)
	return base64.RawURLEncoding.EncodeToString(b) + "." + m.sign(b)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
