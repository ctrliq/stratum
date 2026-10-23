package dashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/time/rate"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// goodToken is the only token the fake apiserver authenticates.
const goodToken = "sha256~good-token"

// seenRequest is what the fake apiserver recorded.
type seenRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// fakeAPIServer stands in for the cluster's apiserver. It answers
// SelfSubjectReview for goodToken and 401 for anything else, and records every
// other request so a test can assert on exactly what the proxy forwarded.
type fakeAPIServer struct {
	*httptest.Server
	seen []seenRequest
	// authStatus, when non-zero, is the status SelfSubjectReview answers with
	// instead of authenticating: 401 for a revoked token, 500 for an
	// apiserver that cannot answer at all.
	authStatus int
	// blankUsername makes SelfSubjectReview succeed but name nobody.
	blankUsername bool
	authReview    http.HandlerFunc
	// reply, when set, answers every recorded request in place of the
	// default Pod.
	reply http.HandlerFunc
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		if r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
			if f.authReview != nil && token != goodToken {
				f.authReview(w, r)
				return
			}
			if f.authStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.authStatus)
				_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":` +
					strconv.Itoa(f.authStatus) + `,"message":"no"}`))
				return
			}
			if token != goodToken {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401,"message":"Unauthorized"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			user := "system:serviceaccount:demo:viewer"
			if f.blankUsername {
				user = ""
			}
			_, _ = w.Write([]byte(`{"kind":"SelfSubjectReview","apiVersion":"authentication.k8s.io/v1",
			  "status":{"userInfo":{"username":"` + user + `","groups":["system:authenticated"]}}}`))
			return
		}

		f.seen = append(f.seen, seenRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: string(body),
		})
		if f.reply != nil {
			f.reply(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// A Set-Cookie the dashboard must strip on the way back.
		w.Header().Set("Set-Cookie", "upstream=1")
		_, _ = w.Write([]byte(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"proxied"}}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// last is the most recent request the fake apiserver actually received.
func (f *fakeAPIServer) last(t *testing.T) seenRequest {
	t.Helper()
	if len(f.seen) == 0 {
		t.Fatal("the apiserver received no proxied request")
	}
	return f.seen[len(f.seen)-1]
}

// harness is a fully wired dashboard in front of a fake apiserver, driven
// through the real mux and the real middleware stack.
type harness struct {
	*fakeAPIServer
	srv  *server
	dash *httptest.Server
	jar  []*http.Cookie
	csrf string
}

func newHarness(t *testing.T, nodeShell bool) *harness {
	t.Helper()
	api := newFakeAPIServer(t)
	cfg := &rest.Config{Host: api.URL}
	tgt, err := localTarget(cfg)
	if err != nil {
		t.Fatalf("localTarget: %v", err)
	}
	// secure=false: the test server is plain HTTP, and a Secure cookie would
	// never come back.
	sess, err := newSessions([]byte("test-secret-material"), time.Hour, false)
	if err != nil {
		t.Fatalf("newSessions: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The image the SPA is told to run the node-shell pod with. Mirrors run(),
	// which sets it from version.NsenterImage only when the feature is on.
	nodeShellImage := ""
	if nodeShell {
		nodeShellImage = testNodeShellImage
	}
	srv := &server{
		base:           cfg,
		sessions:       sess,
		proxy:          newProxy(tgt, nodeShell, log),
		spa:            newSPAHandler(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>spa")}}),
		nodeShell:      nodeShell,
		nodeShellImage: nodeShellImage,
		version:        "v-test",
		log:            log,
		logins:         rate.NewLimiter(rate.Inf, 1),
	}
	h := &harness{fakeAPIServer: api, srv: srv, dash: httptest.NewServer(srv.routes())}
	t.Cleanup(h.dash.Close)
	return h
}

// do sends a request to the dashboard, carrying whatever session the harness
// holds and the CSRF token that goes with it.
func (h *harness) do(t *testing.T, method, path string, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.dash.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	if h.csrf != "" {
		req.Header.Set(csrfHeader, h.csrf)
	}
	resp, err := h.dash.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// login signs in with token and stores the resulting session.
func (h *harness) login(t *testing.T, token string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Token: token})
	resp := h.do(t, http.MethodPost, "/api/v1/login", string(body))
	if resp.StatusCode == http.StatusOK {
		h.jar = resp.Cookies()
		var out struct {
			CSRFToken string `json:"csrfToken"`
		}
		decode(t, resp, &out)
		h.csrf = out.CSRFToken
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// ---- token validation ------------------------------------------------------

func TestLoginRequiresATokenTheClusterAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"the apiserver rejects it", "not-a-real-token", http.StatusUnauthorized},
		{"empty", "", http.StatusBadRequest},
		{"whitespace only", "   ", http.StatusBadRequest},
		{"the apiserver accepts it", goodToken, http.StatusOK},
		{"accepted with a pasted Bearer prefix", "Bearer " + goodToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			resp := h.login(t, tc.token)
			if resp.StatusCode != tc.want {
				t.Fatalf("login: got %d, want %d (%s)", resp.StatusCode, tc.want, bodyOf(t, resp))
			}
			gotCookie := false
			for _, c := range resp.Cookies() {
				if c.Name == cookieName && c.Value != "" {
					gotCookie = true
				}
			}
			if want := tc.want == http.StatusOK; gotCookie != want {
				t.Fatalf("session cookie issued = %t, want %t", gotCookie, want)
			}
		})
	}
}

// decodeJSON must refuse a body that carries anything after the first JSON
// value, not just ignore it. Trailing whitespace stays legal.
func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	valid := `{"token":"` + goodToken + `"}`
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"single object", valid, http.StatusOK},
		{"trailing whitespace", valid + " \r\n\t\n", http.StatusOK},
		{"trailing garbage", valid + "garbage", http.StatusBadRequest},
		{"trailing brace", valid + "}", http.StatusBadRequest},
		{"trailing bracket", valid + "]", http.StatusBadRequest},
		{"second value", valid + " {}", http.StatusBadRequest},
		{"malformed JSON", `{"token":`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			resp := h.do(t, http.MethodPost, "/api/v1/login", tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("login: got %d, want %d (%s)", resp.StatusCode, tc.want, bodyOf(t, resp))
			}
			if tc.want != http.StatusOK && len(resp.Cookies()) != 0 {
				t.Fatalf("a refused body issued a cookie: %v", resp.Cookies())
			}
		})
	}
}

// A rejected login must not leave anything behind that the rest of the stack
// would treat as a session.
func TestRejectedTokenYieldsNoUsableSession(t *testing.T) {
	h := newHarness(t, false)
	resp := h.login(t, "not-a-real-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login: got %d, want 401", resp.StatusCode)
	}
	h.jar = resp.Cookies() // whatever it sent, use it
	if got := h.do(t, http.MethodGet, "/api/v1/me", "").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("/me after a rejected login: got %d, want 401", got)
	}
	if got := h.do(t, http.MethodGet, "/k8s/api/v1/pods", "").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("proxy after a rejected login: got %d, want 401", got)
	}
	if len(h.seen) != 0 {
		t.Fatalf("the apiserver saw %d proxied requests from an unauthenticated caller, want 0", len(h.seen))
	}
}

func TestMeReportsTheIdentityTheClusterGave(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	var me struct {
		Username  string   `json:"username"`
		Groups    []string `json:"groups"`
		Provider  string   `json:"provider"`
		CSRFToken string   `json:"csrfToken"`
	}
	resp := h.do(t, http.MethodGet, "/api/v1/me", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	decode(t, resp, &me)
	if me.Username != "system:serviceaccount:demo:viewer" {
		t.Errorf("username = %q, want the apiserver's answer", me.Username)
	}
	if me.Provider != provider {
		t.Errorf("provider = %q, want %q", me.Provider, provider)
	}
	if me.CSRFToken == "" {
		t.Error("/me must carry the CSRF token; every write in the SPA needs it")
	}
}

// ---- header scrubbing and token passthrough --------------------------------

func TestProxyForwardsTheSessionTokenAndScrubsCallerHeaders(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}

	req, _ := http.NewRequest(http.MethodGet, h.dash.URL+"/k8s/api/v1/namespaces/kube-system/pods?limit=5", nil)
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	// Everything a hostile caller might try to smuggle through.
	req.Header.Set("Authorization", "Bearer someone-elses-token")
	req.Header.Set("Impersonate-User", "system:admin")
	req.Header.Set("Impersonate-Group", "system:masters")
	req.Header.Set("Impersonate-Extra-Scopes", "everything")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	// Port and Uri matter as much as the three net/http's ReverseProxy drops
	// on its own: without them in the list this assertion passes whether or
	// not the scrub list exists.
	req.Header.Set("X-Forwarded-Port", "8443")
	req.Header.Set("X-Forwarded-Uri", "/somewhere-else")
	req.Header.Set("X-Forwarded-Server", "evil.example")
	req.Header.Set("X-Forwarded-For-Extra", "10.0.0.2")
	req.Header.Set("Forwarded", "for=10.0.0.1")
	req.Header.Set(csrfHeader, h.csrf)
	// Stored non-canonically on purpose: Header.Del canonicalises before
	// deleting, so a prefix scrub that used it would leave this one in place.
	req.Header["impersonate-uid"] = []string{"1234"}

	resp, err := h.dash.Client().Do(req)
	if err != nil {
		t.Fatalf("proxied GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied GET: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}

	seen := h.last(t)
	if seen.Path != "/api/v1/namespaces/kube-system/pods" {
		t.Errorf("apiserver path = %q, want the /k8s prefix stripped", seen.Path)
	}
	if seen.Query != "limit=5" {
		t.Errorf("apiserver query = %q, want it preserved", seen.Query)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer "+goodToken {
		t.Errorf("Authorization = %q, want the session's own token", got)
	}
	for name := range seen.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "impersonate-") {
			t.Errorf("%s reached the apiserver; impersonation headers must never survive the proxy", name)
		}
		if strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" || lower == "cookie" || lower == strings.ToLower(csrfHeader) {
			t.Errorf("%s reached the apiserver; it must be scrubbed", name)
		}
	}
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Errorf("the apiserver's Set-Cookie reached the browser: %q", got)
	}
}

// A pod's own HTML, served through pods/proxy from the dashboard's origin, must
// not run script with the signed-in user's session.
func TestProxySandboxesWorkloadHTML(t *testing.T) {
	h := newHarness(t, false)
	h.reply = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "script-src *")
		_, _ = w.Write([]byte(`<script src="x.js"></script>`))
	}
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}

	resp := h.do(t, http.MethodGet, "/k8s/api/v1/namespaces/team-a/services/web:80/proxy/", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied GET: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	csp := resp.Header.Values("Content-Security-Policy")
	if !slices.Contains(csp, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want a sandbox policy on proxied content", csp)
	}
	if !slices.Contains(csp, contentSecurityPolicy) {
		t.Errorf("Content-Security-Policy = %q, want the dashboard's own policy kept", csp)
	}
	if slices.Contains(csp, "script-src *") {
		t.Errorf("Content-Security-Policy = %q: the workload's own policy reached the browser", csp)
	}
}

func TestProxyRequiresASessionAndACSRFTokenForWrites(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	h.csrf = "" // a write with a valid session but no CSRF token
	resp := h.do(t, http.MethodDelete, "/k8s/api/v1/namespaces/default/pods/demo", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without a CSRF token: got %d, want 403", resp.StatusCode)
	}
	if len(h.seen) != 0 {
		t.Fatalf("the apiserver saw a write that failed CSRF")
	}
}

// ---- a write without a session answers 401, not the CSRF 403 ---------------

// The SPA routes any 401 to the sign-in screen, so a write made after the
// session ended must come back as 401 unauthenticated — not the CSRF
// middleware's 403. Without a session this is the one case where the order of
// the two layers in authed (server.go) answers differently: requireSession
// outermost refuses with 401 before the CSRF middleware ever sees the
// request, while swapped, the CSRF layer finds no session in the request
// context and answers 403 csrf (csrf.go).
func TestProxyAnswersAWriteWithoutASessionWith401(t *testing.T) {
	const path = "/k8s/api/v1/namespaces/default/pods/demo"
	t.Run("never signed in", func(t *testing.T) {
		h := newHarness(t, false)
		assertUnauthenticatedWrite(t, h, path)
	})
	t.Run("after logout", func(t *testing.T) {
		h := newHarness(t, false)
		if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
			t.Fatalf("login: got %d", got)
		}
		resp := h.do(t, http.MethodPost, "/api/v1/logout", "")
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("logout: got %d", resp.StatusCode)
		}
		// The browser keeps whatever logout Set-Cookie'd, and the SPA's logout
		// drops the CSRF token together with the session.
		h.jar = resp.Cookies()
		h.csrf = ""
		assertUnauthenticatedWrite(t, h, path)
	})
}

// assertUnauthenticatedWrite sends a pod DELETE with whatever session the
// harness holds and pins the answer: 401 unauthenticated, and nothing
// forwarded.
func assertUnauthenticatedWrite(t *testing.T, h *harness, path string) {
	t.Helper()
	resp := h.do(t, http.MethodDelete, path, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("write without a session: got %d, want 401 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	var out struct {
		Error string `json:"error"`
	}
	decode(t, resp, &out)
	if out.Error != "unauthenticated" {
		t.Errorf("error code = %q, want unauthenticated; the SPA only redirects a 401 to sign-in", out.Error)
	}
	if len(h.seen) != 0 {
		t.Fatalf("the apiserver saw %d request(s) from a write that had no session", len(h.seen))
	}
}

// ---- the node-shell gate ---------------------------------------------------

const nodeShellPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"stratum-node-shell-",` +
	`"labels":{"app":"stratum-node-shell"}},"spec":{"nodeName":"n1","hostPID":true,"restartPolicy":"Never",` +
	`"containers":[{"name":"shell","image":"alpine:3.20","securityContext":{"privileged":true}}]}}`

const hostPIDPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"innocuous-"},` +
	`"spec":{"hostPID":true,"containers":[{"name":"c","image":"alpine:3.20"}]}}`

// namedNodeShellPodJSON carries the node-shell name but NOT hostPID, so only
// the name check can catch it. Without this case the hostPID check alone
// covers the real node-shell body and a broken name check goes unnoticed.
const namedNodeShellPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"stratum-node-shell-"},` +
	`"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}`

const explicitlyNamedNodeShellPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"stratum-node-shell-fixed"},` +
	`"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}`

const hostPIDApplyYAML = "apiVersion: v1\nkind: Pod\nmetadata:\n  name: innocuous\n" +
	"spec:\n  hostPID: true\n  containers:\n  - name: c\n    image: alpine:3.20\n"

const ordinaryApplyYAML = "apiVersion: v1\nkind: Pod\nmetadata:\n  name: ordinary\n" +
	"spec:\n  containers:\n  - name: c\n    image: alpine:3.20\n"

const (
	applyPatch     = "application/apply-patch+yaml"
	mergePatch     = "application/merge-patch+json"
	strategicPatch = "application/strategic-merge-patch+json"
	jsonPatch      = "application/json-patch+json"
)

// The case-folded bodies carry the same key twice, differing only in case. The
// apiserver reads only the exact-case key and drops the other, so the gate has
// to read them the same way whichever order they come in.
const (
	hostPIDThenCaseFoldedPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"innocuous-"},` +
		`"spec":{"hostPID":true,"HOSTPID":false,"containers":[{"name":"c","image":"alpine:3.20"}]}}`
	caseFoldedThenHostPIDPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"innocuous-"},` +
		`"spec":{"hostpid":false,"hostPID":true,"containers":[{"name":"c","image":"alpine:3.20"}]}}`
	nameThenCaseFoldedPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"stratum-node-shell-",` +
		`"GenerateName":"innocuous-"},"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}`
	caseFoldedHostPIDOnlyPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"innocuous-"},` +
		`"spec":{"HostPID":true,"containers":[{"name":"c","image":"alpine:3.20"}]}}`
)

const ordinaryPodJSON = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"ordinary"},` +
	`"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}`

// ---- the gate against the Job redesign ------------------------------------
//
// The shell stops being a pod the SPA creates directly and becomes a batch/v1
// Job whose template carries the same pod. The Job is only another way to ask
// for the same host shell, so the gate judges a Job create or patch by its
// template the way it judges a pod body, and exec/attach keep matching the
// Job-derived pod names (a Job pod's name is the Job's generateName plus a
// suffix, so it carries the same prefix).

// nodeShellJobJSON is the Job the SPA's NodeShell POSTs: a batch/v1 Job whose
// template carries today's pod spec — hostPID, privileged nsenter shell — and
// whose spec carries ttlSecondsAfterFinished (the TTL controller's cleanup)
// and a Job-level activeDeadlineSeconds.
const nodeShellJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"stratum-node-shell-",` +
	`"labels":{"app":"stratum-node-shell"}},"spec":{"activeDeadlineSeconds":3600,"ttlSecondsAfterFinished":600,` +
	`"template":{"metadata":{"labels":{"app":"stratum-node-shell"}},` +
	`"spec":{"nodeName":"n1","hostPID":true,"restartPolicy":"Never","activeDeadlineSeconds":3600,` +
	`"tolerations":[{"operator":"Exists"}],"containers":[{"name":"shell","image":"alpine:3.20",` +
	`"command":["sleep","3600"],"stdin":true,"tty":true,"securityContext":{"privileged":true}}]}}}}`

// namedNodeShellJobJSON carries the node-shell generateName in the template
// but NOT hostPID, so only the template's name check can catch it.
const namedNodeShellJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"ordinary-"},` +
	`"spec":{"template":{"metadata":{"generateName":"stratum-node-shell-"},` +
	`"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}}}`

// explicitlyNamedNodeShellJobJSON names the template's pod outright, the way a
// server-side apply to a fresh name would.
const explicitlyNamedNodeShellJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"ordinary-"},` +
	`"spec":{"template":{"metadata":{"name":"stratum-node-shell-fixed"},` +
	`"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}}}`

// hostPIDJobJSON hides the hostPID behind an innocuous template name: renaming
// the Job must not be enough to reach the host, exactly as for pods.
const hostPIDJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"innocuous-"},` +
	`"spec":{"template":{"metadata":{"generateName":"ordinary-"},` +
	`"spec":{"hostPID":true,"containers":[{"name":"c","image":"alpine:3.20"}]}}}}`

// The case-folded Job bodies carry the same key twice, differing only in case,
// shaped like their pod versions: the apiserver reads only the exact-case key
// and drops the other, so the gate has to read the template the same way.
const (
	hostPIDThenCaseFoldedJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"innocuous-"},` +
		`"spec":{"template":{"metadata":{"generateName":"ordinary-"},` +
		`"spec":{"hostPID":true,"HOSTPID":false,"containers":[{"name":"c","image":"alpine:3.20"}]}}}}`
	templateNameThenCaseFoldedJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"innocuous-"},` +
		`"spec":{"template":{"metadata":{"generateName":"stratum-node-shell-","GenerateName":"ordinary-"},` +
		`"spec":{"containers":[{"name":"c","image":"alpine:3.20"}]}}}}`
)

const ordinaryJobJSON = `{"apiVersion":"batch/v1","kind":"Job","metadata":{"generateName":"ordinary-"},` +
	`"spec":{"template":{"metadata":{"labels":{"app":"batch"}},"spec":` +
	`{"restartPolicy":"Never","containers":[{"name":"c","image":"alpine:3.20"}]}}}}`

const hostPIDJobApplyYAML = "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: innocuous\n" +
	"spec:\n  template:\n    spec:\n      hostPID: true\n      containers:\n      - name: c\n        image: alpine:3.20\n"

const ordinaryJobApplyYAML = "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: ordinary\n" +
	"spec:\n  template:\n    spec:\n      containers:\n      - name: c\n        image: alpine:3.20\n"

// TestNodeShellDefaultsToEnabled pins the default the command ships with: the
// node shell is available unless an operator withholds it.
func TestNodeShellDefaultsToEnabled(t *testing.T) {
	// The flag's default is what most deployments will run with, so it is
	// asserted on the command itself, not only on a hand-built server.
	f := Command().Flags().Lookup("enable-node-shell")
	if f == nil {
		t.Fatal("stratum dash has no --enable-node-shell flag")
	}
	if f.DefValue != "true" {
		t.Fatalf("--enable-node-shell defaults to %q, want true", f.DefValue)
	}
}

func TestNodeShellFallsBackOnlyByDefault(t *testing.T) {
	const unstamped = "ghcr.io/ctrliq/stratum/busybox:unknown"
	for _, tc := range []struct {
		name              string
		enabled, explicit bool
		image             string
		want              bool
	}{
		{"default on, unstamped build", true, false, unstamped, true},
		{"default on, no image", true, false, "", true},
		{"default on, stamped build", true, false, testNodeShellImage, false},
		{"explicitly on, unstamped build fails closed instead", true, true, unstamped, false},
		{"explicitly off", false, true, unstamped, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeShellFallsBack(tc.enabled, tc.explicit, tc.image); got != tc.want {
				t.Errorf("nodeShellFallsBack(%v, %v, %q) = %v, want %v", tc.enabled, tc.explicit, tc.image, got, tc.want)
			}
		})
	}
}

func TestNodeShellGateRefusesBeforeForwarding(t *testing.T) {
	pods := "/k8s/api/v1/namespaces/default/pods"
	jobs := "/k8s/apis/batch/v1/namespaces/default/jobs"
	for _, tc := range []struct {
		name, method, path, body, ct string
	}{
		{"create by generateName", http.MethodPost, pods, nodeShellPodJSON, ""},
		{"create by generateName with no hostPID", http.MethodPost, pods, namedNodeShellPodJSON, ""},
		{"create by an explicit node-shell name", http.MethodPost, pods, explicitlyNamedNodeShellPodJSON, ""},
		{"create any hostPID pod, whatever it is called", http.MethodPost, pods, hostPIDPodJSON, ""},
		{"create hostPID followed by a case-folded hostPID false", http.MethodPost, pods, hostPIDThenCaseFoldedPodJSON, ""},
		{"create a case-folded hostPID false followed by hostPID", http.MethodPost, pods, caseFoldedThenHostPIDPodJSON, ""},
		{"create by generateName followed by a case-folded generateName", http.MethodPost, pods, nameThenCaseFoldedPodJSON, ""},
		{"create through the legacy watch-form path", http.MethodPost, "/k8s/api/v1/watch/namespaces/default/pods", nodeShellPodJSON, ""},
		{"exec into a node-shell pod", http.MethodGet, pods + "/stratum-node-shell-abc12/exec?command=sh", "", ""},
		{"attach to a node-shell pod", http.MethodGet, pods + "/stratum-node-shell-abc12/attach", "", ""},
		{"exec into a Job-derived shell pod", http.MethodGet, pods + "/stratum-node-shell-abc12-9f2kx/exec?command=sh", "", ""},
		{"attach to a Job-derived shell pod", http.MethodGet, pods + "/stratum-node-shell-abc12-9f2kx/attach", "", ""},
		{"a body it cannot parse", http.MethodPost, pods, `{"metadata":{`, ""},
		{"a body it cannot inspect because it is YAML", http.MethodPost, pods, "kind: Pod\n", "application/yaml"},
		{"server-side apply of a hostPID pod", http.MethodPatch, pods + "/innocuous", hostPIDApplyYAML, applyPatch},
		{"server-side apply of a hostPID pod sent as JSON", http.MethodPatch, pods + "/innocuous", hostPIDPodJSON, applyPatch},
		{"server-side apply with a charset parameter", http.MethodPatch, pods + "/innocuous", hostPIDApplyYAML, applyPatch + "; charset=utf-8"},
		{"server-side apply of a node-shell generateName", http.MethodPatch, pods + "/innocuous", namedNodeShellPodJSON, applyPatch},
		{"server-side apply to a node-shell pod name", http.MethodPatch, pods + "/stratum-node-shell-abc12", ordinaryApplyYAML, applyPatch},
		{"server-side apply YAML it cannot parse", http.MethodPatch, pods + "/innocuous", "spec: [\n", applyPatch},
		{"merge patch setting hostPID", http.MethodPatch, pods + "/innocuous", `{"spec":{"hostPID":true}}`, mergePatch},
		{"merge patch setting hostPID beside a case-variant duplicate", http.MethodPatch, pods + "/innocuous", `{"spec":{"hostPID":true,"HOSTPID":false}}`, mergePatch},
		{"merge patch setting hostPID after a case-variant duplicate", http.MethodPatch, pods + "/innocuous", `{"spec":{"hostpid":false,"hostPID":true}}`, mergePatch},
		{"strategic merge patch setting hostPID", http.MethodPatch, pods + "/innocuous", `{"spec":{"hostPID":true}}`, strategicPatch},
		{"json patch adding hostPID", http.MethodPatch, pods + "/innocuous", `[{"op":"add","path":"/spec/hostPID","value":true}]`, jsonPatch},
		{"json patch adding hostPID through an escaped pointer", http.MethodPatch, pods + "/innocuous", `[{"op":"add","path":"/spec~1hostPID","value":true}]`, jsonPatch},
		{"json patch replacing the whole spec", http.MethodPatch, pods + "/innocuous", `[{"op":"replace","path":"/spec","value":{"hostPID":true}}]`, jsonPatch},
		{"json patch it cannot parse", http.MethodPatch, pods + "/innocuous", `{"op":"add"}`, jsonPatch},
		{"a patch content type it does not inspect", http.MethodPatch, pods + "/innocuous", "\xa1", "application/apply-patch+cbor"},
		{"a patch with no content type", http.MethodPatch, pods + "/innocuous", `{"spec":{"hostPID":true}}`, ""},
		{"create a node-shell Job by generateName", http.MethodPost, jobs, nodeShellJobJSON, ""},
		{"create a Job whose template carries the node-shell generateName and no hostPID", http.MethodPost, jobs, namedNodeShellJobJSON, ""},
		{"create a Job whose template carries an explicit node-shell name", http.MethodPost, jobs, explicitlyNamedNodeShellJobJSON, ""},
		{"create any hostPID Job, whatever its template is called", http.MethodPost, jobs, hostPIDJobJSON, ""},
		{"create a Job with hostPID followed by a case-folded hostPID false", http.MethodPost, jobs, hostPIDThenCaseFoldedJobJSON, ""},
		{"create a Job whose template carries the node-shell name followed by a case-folded name", http.MethodPost, jobs, templateNameThenCaseFoldedJobJSON, ""},
		{"create a node-shell Job through the legacy watch-form path", http.MethodPost, "/k8s/apis/batch/v1/watch/namespaces/default/jobs", nodeShellJobJSON, ""},
		{"a Job body it cannot parse", http.MethodPost, jobs, `{"spec":{"template":{`, ""},
		{"a Job body it cannot inspect because it is YAML", http.MethodPost, jobs, "kind: Job\n", "application/yaml"},
		{"server-side apply of a hostPID Job", http.MethodPatch, jobs + "/innocuous", hostPIDJobApplyYAML, applyPatch},
		{"server-side apply of a Job whose template carries the node-shell generateName", http.MethodPatch, jobs + "/innocuous", namedNodeShellJobJSON, applyPatch},
		{"server-side apply to a node-shell Job name", http.MethodPatch, jobs + "/stratum-node-shell-abc12", ordinaryJobApplyYAML, applyPatch},
		{"merge patch setting the template's hostPID", http.MethodPatch, jobs + "/innocuous", `{"spec":{"template":{"spec":{"hostPID":true}}}}`, mergePatch},
		{"json patch adding the template's hostPID", http.MethodPatch, jobs + "/innocuous", `[{"op":"add","path":"/spec/template/spec/hostPID","value":true}]`, jsonPatch},
		{"json patch adding the template's hostPID through an escaped pointer", http.MethodPatch, jobs + "/innocuous", `[{"op":"add","path":"/spec~1template~1spec~1hostPID","value":true}]`, jsonPatch},
		{"json patch replacing the template's spec", http.MethodPatch, jobs + "/innocuous", `[{"op":"replace","path":"/spec/template/spec","value":{"hostPID":true}}]`, jsonPatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
				t.Fatalf("login: got %d", got)
			}
			req, _ := http.NewRequest(tc.method, h.dash.URL+tc.path, strings.NewReader(tc.body))
			for _, c := range h.jar {
				req.AddCookie(c)
			}
			req.Header.Set(csrfHeader, h.csrf)
			if tc.ct != "" {
				req.Header.Set("Content-Type", tc.ct)
			} else if tc.body != "" && tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := h.dash.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("got %d, want 403 (%s)", resp.StatusCode, bodyOf(t, resp))
			}
			var out struct {
				Error string `json:"error"`
			}
			decode(t, resp, &out)
			if out.Error != "node-shell-disabled" {
				t.Errorf("error code = %q, want node-shell-disabled", out.Error)
			}
			// The whole point of a server-side gate: the apiserver never sees it.
			if len(h.seen) != 0 {
				t.Fatalf("the request reached the apiserver anyway: %+v", h.seen)
			}
		})
	}
}

func TestNodeShellGateForwardsWhatItDoesNotGuard(t *testing.T) {
	pods := "/k8s/api/v1/namespaces/default/pods"
	jobs := "/k8s/apis/batch/v1/namespaces/default/jobs"
	for _, tc := range []struct {
		name, method, path, body, ct string
		nodeShell                    bool
	}{
		{name: "an ordinary pod create, gate off", method: http.MethodPost, path: pods, body: ordinaryPodJSON},
		{name: "a case-folded hostPID the apiserver drops, gate off", method: http.MethodPost, path: pods, body: caseFoldedHostPIDOnlyPodJSON},
		{name: "exec into an ordinary pod, gate off", method: http.MethodGet, path: pods + "/web-0/exec?command=sh"},
		{name: "a node-shell create, gate on", method: http.MethodPost, path: pods, body: nodeShellPodJSON, nodeShell: true},
		{name: "a hostPID create, gate on", method: http.MethodPost, path: pods, body: hostPIDPodJSON, nodeShell: true},
		{name: "exec into a node-shell pod, gate on", method: http.MethodGet, path: pods + "/stratum-node-shell-abc12/exec?command=sh", nodeShell: true},
		{name: "deleting the node-shell pod, gate off", method: http.MethodDelete, path: pods + "/stratum-node-shell-abc12?gracePeriodSeconds=1"},
		{name: "an ordinary Job create, gate off", method: http.MethodPost, path: jobs, body: ordinaryJobJSON},
		{name: "a node-shell Job create, gate on", method: http.MethodPost, path: jobs, body: nodeShellJobJSON, nodeShell: true},
		{name: "a hostPID Job create, gate on", method: http.MethodPost, path: jobs, body: hostPIDJobJSON, nodeShell: true},
		{name: "an ordinary Job create, gate on", method: http.MethodPost, path: jobs, body: ordinaryJobJSON, nodeShell: true},
		{name: "deleting the node-shell Job, gate off", method: http.MethodDelete, path: jobs + "/stratum-node-shell-abc12?gracePeriodSeconds=1"},
		{name: "deleting the node-shell Job, gate on", method: http.MethodDelete, path: jobs + "/stratum-node-shell-abc12?gracePeriodSeconds=1", nodeShell: true},
		{name: "server-side apply of an ordinary pod, gate off", method: http.MethodPatch, path: pods + "/ordinary", body: ordinaryApplyYAML, ct: applyPatch},
		{name: "a merge patch adding a label, gate off", method: http.MethodPatch, path: pods + "/web-0", body: `{"metadata":{"labels":{"tier":"web"}}}`, ct: mergePatch},
		{name: "a json patch changing an image, gate off", method: http.MethodPatch, path: pods + "/web-0", body: `[{"op":"replace","path":"/spec/containers/0/image","value":"alpine:3.21"}]`, ct: jsonPatch},
		{name: "server-side apply of a hostPID pod, gate on", method: http.MethodPatch, path: pods + "/innocuous", body: hostPIDApplyYAML, ct: applyPatch, nodeShell: true},
		{name: "server-side apply of an ordinary Job, gate off", method: http.MethodPatch, path: jobs + "/ordinary", body: ordinaryJobApplyYAML, ct: applyPatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.nodeShell)
			if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
				t.Fatalf("login: got %d", got)
			}
			req, _ := http.NewRequest(tc.method, h.dash.URL+tc.path, strings.NewReader(tc.body))
			for _, c := range h.jar {
				req.AddCookie(c)
			}
			req.Header.Set(csrfHeader, h.csrf)
			if tc.ct != "" {
				req.Header.Set("Content-Type", tc.ct)
			} else if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := h.dash.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got %d, want 200 (%s)", resp.StatusCode, bodyOf(t, resp))
			}
			seen := h.last(t)
			// The body has to survive being read for inspection, or the
			// gate would quietly break every pod create it allows.
			if tc.body != "" && seen.Body != tc.body {
				t.Errorf("the apiserver received body %q, want it forwarded intact", seen.Body)
			}
		})
	}
}

// The Job redesign makes sign-out's shell delete a best-effort acceleration of
// the TTL teardown: one DELETE against the Job, sent while the session still
// stands. When the token the session carries is already revoked, that delete
// comes back 401 — the gate must not have swallowed it, because it judges
// creates and patches, never deletes — and sign-out still completes: logout
// clears the cookie with a 204 exactly as it does with no shell open.
func TestRevokedTokenDoesNotBlockTheShellJobTeardownOrLogout(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	h.authStatus = http.StatusUnauthorized
	h.reply = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401,"message":"Unauthorized"}`))
	}
	// The SPA's sign-out recovers the logout CSRF from the refused identity
	// probe, the way TestRevokedMeAllowsExplicitLogout pins it.
	me := h.do(t, http.MethodGet, "/api/v1/me", "")
	if me.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked identity probe: got %d, want 401", me.StatusCode)
	}
	var rejected struct {
		CSRFToken string `json:"csrfToken"`
	}
	decode(t, me, &rejected)
	if rejected.CSRFToken == "" {
		t.Fatal("a valid sealed session must provide logout CSRF without an authenticated identity")
	}
	h.csrf = rejected.CSRFToken

	// The best-effort teardown reaches the apiserver ungated and fails there on
	// the dead token; the failure is the SPA's to report and ignore.
	teardown := h.do(t, http.MethodDelete,
		"/k8s/apis/batch/v1/namespaces/default/jobs/stratum-node-shell-abc12?gracePeriodSeconds=1", "")
	if teardown.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Job teardown with a revoked token: got %d, want the apiserver's 401", teardown.StatusCode)
	}
	if len(h.seen) != 1 {
		t.Fatalf("the apiserver saw %d request(s), want exactly the one Job delete", len(h.seen))
	}
	if seen := h.last(t); seen.Method != http.MethodDelete ||
		seen.Path != "/apis/batch/v1/namespaces/default/jobs/stratum-node-shell-abc12" ||
		seen.Query != "gracePeriodSeconds=1" {
		t.Fatalf("the apiserver saw %+v, want the Job delete with its one-second grace period", seen)
	}

	logout := h.do(t, http.MethodPost, "/api/v1/logout", "")
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout after a failed Job teardown: got %d, want 204", logout.StatusCode)
	}
	deleted := logout.Cookies()
	if len(deleted) != 1 || deleted[0].Name != cookieName || deleted[0].Value != "" ||
		deleted[0].MaxAge >= 0 || deleted[0].Path != "/" || deleted[0].Domain != "" {
		t.Fatalf("logout must expire the session cookie despite the failed teardown: %v", deleted)
	}
}

// dashReadme is the dashboard's README, read relative to the package directory
// go test runs in — the same stance spa_contract_test.go takes: the contract is
// with the SPA tree's own sources.
const dashReadme = "../../dash/README.md"

// The Job design changes what an operator must hold and what remains behind:
// the signing user's own RBAC needs batch jobs create and jobs delete, and
// the finished object's story is the TTL controller's —
// spec.ttlSecondsAfterFinished — not a pending sweep. The README is where
// both live for the operator, so this pins that it says so.
func TestDashReadmeDocumentsTheNodeShellJobRBACAndTTL(t *testing.T) {
	src, err := os.ReadFile(dashReadme)
	if err != nil {
		t.Fatalf("read the dashboard README: %v", err)
	}
	readme := string(src)
	for _, want := range []string{"jobs create", "jobs delete", "ttlSecondsAfterFinished", "pods get"} {
		if !strings.Contains(readme, want) {
			t.Errorf("dash/README.md never names %q; the node shell needs batch jobs create/delete in the caller's own RBAC, and the TTL controller deletes the finished Job", want)
		}
	}
	if strings.Contains(readme, "provides no controller") {
		t.Error("dash/README.md still says this build provides no controller for finished node-shell objects; ttlSecondsAfterFinished is that controller")
	}
	// The error panel is the hint an operator actually sees when a shell fails
	// for lack of RBAC; the shell runs as a Job, so
	// advice naming pods create/delete is neither necessary nor sufficient.
	// The create alone is not sufficient either: the readiness poll lists and
	// gets the Job's pod before the exec opens, so a hint naming only the Job
	// verbs and pods/exec sends a token granted exactly that into a two-minute
	// timeout it cannot read its way out of.
	hint, err := os.ReadFile(filepath.Join("..", "..", "dash", "src", "screens", "terminal.jsx"))
	if err != nil {
		t.Fatalf("read the terminal screen: %v", err)
	}
	hintText := string(hint)
	if strings.Contains(hintText, "pods create/delete") {
		t.Error("terminal.jsx still tells the operator the node shell needs pods create/delete; the create and the deletes target the Job (jobs create/delete), and only the exec touches pods")
	}
	for _, want := range []string{"jobs create", "pods/exec", "pods get/list"} {
		if !strings.Contains(hintText, want) {
			t.Errorf("terminal.jsx's host-shell hint never names %q; that is what opening a shell needs (the poll reads the Job's pod before the exec)", want)
		}
	}
}

// ---- /api/v1/config --------------------------------------------------------

// testNodeShellImage is a stamped-looking payload image the harness serves when
// node-shell is enabled — anything without the ":unknown" sentinel.
const testNodeShellImage = "ghcr.io/ctrliq/stratum/busybox:1.37.0"

// TestNodeShellImageOrErr pins the fail-closed guard: with the feature on, an
// unstamped build must refuse rather than let the SPA fall back to a public
// registry on an airgapped node.
func TestNodeShellImageOrErr(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		image   string
		want    string
		wantErr bool
	}{
		{"disabled serves no image, even an unstamped one", false, "ghcr.io/ctrliq/stratum/busybox:unknown", "", false},
		{"enabled with a stamped image passes it through", true, testNodeShellImage, testNodeShellImage, false},
		{"enabled with the :unknown sentinel fails closed", true, "ghcr.io/ctrliq/stratum/busybox:unknown", "", true},
		{"enabled with no image fails closed", true, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nodeShellImageOrErr(tc.enabled, tc.image)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a fail-closed error, got nil (the SPA would default to a public registry)")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigReportsTheNodeShellFlagAndTheProxyBase(t *testing.T) {
	for _, nodeShell := range []bool{false, true} {
		h := newHarness(t, nodeShell)
		resp := h.do(t, http.MethodGet, "/api/v1/config", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/config: got %d", resp.StatusCode)
		}
		var cfg struct {
			Mode    string `json:"mode"`
			K8sBase string `json:"k8sBase"`
			Auth    struct {
				Required  bool     `json:"required"`
				Providers []string `json:"providers"`
			} `json:"auth"`
			Features struct {
				NodeShell       bool   `json:"nodeShell"`
				NodeShellImage  string `json:"nodeShellImage"`
				SecretSummaries bool   `json:"secretSummaries"`
			} `json:"features"`
		}
		decode(t, resp, &cfg)
		if cfg.Features.NodeShell != nodeShell {
			t.Errorf("features.nodeShell = %t, want %t", cfg.Features.NodeShell, nodeShell)
		}
		// The SPA must get the debug-pod image from here, never hard-code it, so
		// the node shell works on an airgapped node.
		wantImage := ""
		if nodeShell {
			wantImage = testNodeShellImage
		}
		if cfg.Features.NodeShellImage != wantImage {
			t.Errorf("features.nodeShellImage = %q, want %q", cfg.Features.NodeShellImage, wantImage)
		}
		if !cfg.Features.SecretSummaries {
			t.Errorf("features.secretSummaries = false; the SPA would fall back to reading Secret metadata through the proxy")
		}
		if cfg.Mode != "standalone" {
			t.Errorf("mode = %q, want standalone (the SPA rejects anything else)", cfg.Mode)
		}
		if cfg.K8sBase != k8sPrefix {
			t.Errorf("k8sBase = %q, want %q", cfg.K8sBase, k8sPrefix)
		}
		if !cfg.Auth.Required {
			t.Error("auth.required must be true: there is no unauthenticated mode")
		}
		if len(cfg.Auth.Providers) != 1 || cfg.Auth.Providers[0] != "token" {
			t.Errorf("auth.providers = %v, want [token]", cfg.Auth.Providers)
		}
	}
}

// /config and /login are the only routes reachable without a session; the
// proxy and /me must not be.
func TestOnlyConfigAndLoginAreUnauthenticated(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{"/api/v1/me", secretSummariesPath, "/k8s/api/v1/pods", "/k8s"} {
		if got := h.do(t, http.MethodGet, path, "").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: got %d, want 401", path, got)
		}
	}
	if got := h.do(t, http.MethodGet, "/api/v1/config", "").StatusCode; got != http.StatusOK {
		t.Errorf("GET /api/v1/config: got %d, want 200", got)
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	resp := h.do(t, http.MethodPost, "/api/v1/logout", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: got %d", resp.StatusCode)
	}
	h.jar = resp.Cookies()
	if got := h.do(t, http.MethodGet, "/api/v1/me", "").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("/me after logout: got %d, want 401", got)
	}
}

// ---- the SPA ---------------------------------------------------------------

func TestSPAServesIndexForClientRoutesButNotForAPIPaths(t *testing.T) {
	h := newHarness(t, false)
	if body := bodyOf(t, h.do(t, http.MethodGet, "/nodes", "")); !strings.Contains(body, "spa") {
		t.Errorf("GET /nodes served %q, want index.html for a client-side route", body)
	}
	resp := h.do(t, http.MethodGet, "/api/v1/nope", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/v1/nope: got %d, want 404 rather than the SPA", resp.StatusCode)
	}
}

// ---- origin policy ---------------------------------------------------------

func TestOriginPolicyRejectsForeignWritesAndUpgrades(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	u, _ := url.Parse(h.dash.URL)
	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"same-origin write", http.MethodDelete, map[string]string{"Origin": "http://" + u.Host}, http.StatusOK},
		{"foreign origin write", http.MethodDelete, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"cross-site write", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin upgrade", http.MethodGet, map[string]string{"Origin": "http://evil.example", "Upgrade": "websocket"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, h.dash.URL+"/k8s/api/v1/namespaces/default/pods/demo", nil)
			for _, c := range h.jar {
				req.AddCookie(c)
			}
			req.Header.Set(csrfHeader, h.csrf)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := h.dash.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d (%s)", resp.StatusCode, tc.want, bodyOf(t, resp))
			}
		})
	}
}

// ---- refusing plaintext on a routable address ------------------------------

func TestLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8443", true},
		{"[::1]:8443", true},
		{"localhost:8443", true},
		// The cases this guard exists for: an unspecified address is reachable
		// from the network, and so is a named host.
		{":8443", false},
		{"0.0.0.0:8443", false},
		{"[::]:8443", false},
		{"10.0.0.5:8443", false},
		{"dash.example:8443", false},
		{"", false},
		{"not-an-address", false},
	} {
		if got := loopbackOnly(tc.addr); got != tc.want {
			t.Errorf("loopbackOnly(%q) = %t, want %t", tc.addr, got, tc.want)
		}
	}
}

// The session cookie carries a live bearer token, so run() refuses plaintext
// on anything the network can reach — before it touches a cluster, which is
// why a nonexistent kubeconfig still produces the plaintext error.
func TestRunRefusesPlainHTTPOffLoopback(t *testing.T) {
	err := run(t.Context(), options{Listen: "0.0.0.0:0", Kubeconfig: "/nonexistent", Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("run on a routable address over HTTP: err = %v, want the plaintext refusal", err)
	}
	err = run(t.Context(), options{Listen: "0.0.0.0:0", Kubeconfig: "/nonexistent", AllowInsecure: true,
		PublicURL: "https://dash.example", Out: io.Discard})
	if err == nil || strings.Contains(err.Error(), "plain HTTP") || strings.Contains(err.Error(), "--public-url") {
		t.Fatalf("run with --insecure-http and --public-url: err = %v, want it past both refusals", err)
	}
	err = run(t.Context(), options{Listen: "127.0.0.1:0", TLSCert: "cert.pem", Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "--tls-cert and --tls-key") {
		t.Fatalf("run with only --tls-cert: err = %v, want the paired-flag error", err)
	}
}

func TestPublicOriginIsValidated(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"https://dash.example", "https://dash.example"},
		{"https://Dash.Example:443/ignored", "https://dash.example"},
		{"http://dash.example:8080", "http://dash.example:8080"},
	} {
		got, err := publicOrigin(tc.in)
		if err != nil {
			t.Errorf("publicOrigin(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("publicOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"dash.example", "ftp://dash.example", "not a url"} {
		if _, err := publicOrigin(bad); err == nil {
			t.Errorf("publicOrigin(%q) was accepted", bad)
		}
	}
	// The cookie's Secure flag and HSTS both follow this.
	if !reachedOverHTTPS(false, "https://dash.example") || reachedOverHTTPS(false, "http://dash.example") || reachedOverHTTPS(false, "") {
		t.Error("reachedOverHTTPS does not follow the public URL's scheme")
	}
	if !reachedOverHTTPS(true, "") {
		t.Error("serving TLS ourselves must count as reached over HTTPS")
	}
}

// ---- security headers, revocation, rate limiting ---------------------------

// The security headers are middleware precisely so a new route cannot forget
// them, which only holds if something checks they are there.
func TestSecurityHeadersAreSetOnEveryRoute(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{"/", "/nodes", "/api/v1/config", "/api/v1/me", "/healthz"} {
		resp := h.do(t, http.MethodGet, path, "")
		for header, want := range map[string]string{
			"Content-Security-Policy": contentSecurityPolicy,
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"X-Content-Type-Options":  "nosniff",
		} {
			if got := resp.Header.Get(header); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, header, got, want)
			}
		}
		// HSTS only where the browser actually reaches us over TLS.
		// Announcing it from the plain-HTTP loopback default would be wrong.
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("GET %s: HSTS announced over plain HTTP: %q", path, got)
		}
	}
	// The CSP has to stay all-'self': the fonts, tokens and
	// icons are vendored, so an air-gapped cluster reaches for nothing it cannot
	// see.
	if strings.Contains(contentSecurityPolicy, "http") {
		t.Errorf("the CSP names an off-origin source: %q", contentSecurityPolicy)
	}
	if !strings.Contains(contentSecurityPolicy, "frame-ancestors 'none'") {
		t.Error("the CSP does not forbid framing")
	}
}

func TestSecurityHeadersAnnounceHSTSOnlyWhenReachedOverTLS(t *testing.T) {
	h := newHarness(t, false)
	for _, https := range []bool{false, true} {
		h.srv.https = https
		rec := httptest.NewRecorder()
		h.srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		got := rec.Header().Get("Strict-Transport-Security")
		if https && got == "" {
			t.Error("no HSTS header when the dashboard is reached over TLS")
		}
		if !https && got != "" {
			t.Errorf("HSTS announced over plain HTTP: %q", got)
		}
	}
}

// Browser cookie updates happen before JavaScript sees a response. An old
// identity probe must not erase a login that completed while it was waiting.
func TestDelayedRevokedMeDoesNotClearANewerLogin(t *testing.T) {
	h := newHarness(t, false)
	started, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	t.Cleanup(release)
	h.authReview = func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized", "revoked token")
	}
	value, _, err := h.srv.sessions.issue("old", nil, "revoked-token")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(h.dash.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{h.srv.sessions.cookie(value, false)})
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	type result struct {
		response *http.Response
		err      error
	}
	old := make(chan result, 1)
	go func() {
		response, err := client.Get(h.dash.URL + "/api/v1/me")
		old <- result{response, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old identity probe did not reach the apiserver")
	}
	response, err := client.Post(h.dash.URL+"/api/v1/login", "application/json", strings.NewReader(`{"token":"`+goodToken+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("new login: got %d, want 200", response.StatusCode)
	}
	release()
	previous := <-old
	if previous.err != nil {
		t.Fatal(previous.err)
	}
	_ = previous.response.Body.Close()
	if previous.response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old probe: got %d, want 401", previous.response.StatusCode)
	}
	current, err := client.Get(h.dash.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Body.Close()
	if current.StatusCode != http.StatusOK {
		t.Fatalf("delayed old probe invalidated the new browser session: got %d, want 200", current.StatusCode)
	}
}

// Re-asking the apiserver detects revoked tokens without mutating a cookie
// that may already belong to a newer login.
func TestMeEndsTheSessionWhenTheTokenIsRevoked(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	h.authStatus = http.StatusUnauthorized

	resp := h.do(t, http.MethodGet, "/api/v1/me", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me with a revoked token: got %d, want 401 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	if len(resp.Cookies()) != 0 {
		t.Error("an identity probe must not change a cookie that a newer login may have replaced")
	}
}

func TestRevokedMeAllowsExplicitLogout(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	h.authStatus = http.StatusUnauthorized
	h.csrf = "" // another document must recover the current cookie's token
	resp := h.do(t, http.MethodGet, "/api/v1/me", "")
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
		t.Fatalf("revoked identity probe: status %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	var rejected struct {
		CSRFToken string `json:"csrfToken"`
		Username  string `json:"username"`
	}
	decode(t, resp, &rejected)
	if rejected.CSRFToken == "" || rejected.Username != "" {
		t.Fatalf("a valid sealed session must provide logout CSRF without an authenticated identity: %+v", rejected)
	}
	h.csrf = rejected.CSRFToken
	logout := h.do(t, http.MethodPost, "/api/v1/logout", "")
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout of revoked token: got %d", logout.StatusCode)
	}
	deleted := logout.Cookies()
	if len(deleted) != 1 || deleted[0].Name != cookieName || deleted[0].Value != "" ||
		deleted[0].MaxAge >= 0 || deleted[0].Path != "/" || deleted[0].Domain != "" {
		t.Fatalf("logout must expire the matching browser session cookie: %v", deleted)
	}
	h.jar = deleted
	resp = h.do(t, http.MethodGet, "/api/v1/me", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: got %d", resp.StatusCode)
	}
	var missing map[string]any
	decode(t, resp, &missing)
	if _, ok := missing["csrfToken"]; ok {
		t.Fatal("a cleared cookie must not receive a session CSRF token")
	}
}

// ...but an apiserver that merely cannot answer must not sign everyone out.
// The cookie is signed and unexpired, so it is still a session.
func TestMeSurvivesAnApiserverThatCannotAnswer(t *testing.T) {
	h := newHarness(t, false)
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}
	h.authStatus = http.StatusInternalServerError

	resp := h.do(t, http.MethodGet, "/api/v1/me", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me during an apiserver blip: got %d, want 200 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	var me struct {
		Username string `json:"username"`
	}
	decode(t, resp, &me)
	if me.Username != "system:serviceaccount:demo:viewer" {
		t.Errorf("username = %q, want the identity from the session", me.Username)
	}
}

// login is unauthenticated and makes the dashboard call the apiserver, so it
// is the one place an anonymous caller can drive load into the control plane.
func TestLoginIsRateLimited(t *testing.T) {
	h := newHarness(t, false)
	h.srv.logins = rate.NewLimiter(0, 1) // one attempt, then nothing refills

	if got := h.login(t, "wrong-token").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("first attempt: got %d, want 401", got)
	}
	resp := h.login(t, goodToken)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second attempt: got %d, want 429 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
}

// A SelfSubjectReview that succeeds but names nobody must be refused at
// login. Minting the session instead would produce a cookie that parse()
// then rejects forever: signed in, and 401 on everything.
func TestLoginRefusesAnIdentityWithNoUsername(t *testing.T) {
	h := newHarness(t, false)
	h.blankUsername = true
	resp := h.login(t, goodToken)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login: got %d, want 401 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			t.Fatal("a session cookie was issued for an identity with no username")
		}
	}
}

// ---- the body cap cannot be lifted by a header -----------------------------

// The 4 MiB cap covers everything, Upgrade headers included:
// checkNodeShell reads a pod create into memory whole — so an authenticated
// caller could otherwise POST an unbounded body with `Upgrade: websocket` and
// make the dashboard buffer all of it. The gate is on by default, so this is
// the default configuration.
func TestOversizedBodyIsCappedEvenWithAnUpgradeHeader(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "plain POST"
		if upgrade {
			name = "POST claiming to be an upgrade"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, false)
			if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
				t.Fatalf("login: got %d", got)
			}
			// Comfortably over maxBodyBytes, and shaped like a pod create so
			// it reaches the gate's body inspection.
			huge := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"big"},"spec":{"nodeName":"` +
				strings.Repeat("x", maxBodyBytes+(1<<20)) + `"}}`
			req, _ := http.NewRequest(http.MethodPost, h.dash.URL+"/k8s/api/v1/namespaces/default/pods", strings.NewReader(huge))
			for _, c := range h.jar {
				req.AddCookie(c)
			}
			req.Header.Set(csrfHeader, h.csrf)
			req.Header.Set("Content-Type", "application/json")
			if upgrade {
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Connection", "Upgrade")
			}
			resp, err := h.dash.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("got %d, want 413 — the body cap must not depend on a caller-supplied header", resp.StatusCode)
			}
			if len(h.seen) != 0 {
				t.Fatalf("an oversized body reached the apiserver")
			}
		})
	}
}

// ---- --insecure-http has to say what scheme users arrive on ----------------

// Behind a TLS terminator the browser is on HTTPS, so the cookie must be
// Secure — and this process cannot know that unless it is told.
func TestInsecureHTTPRequiresAPublicURL(t *testing.T) {
	err := run(t.Context(), options{Listen: "0.0.0.0:0", AllowInsecure: true, Kubeconfig: "/nonexistent", Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "--insecure-http requires --public-url") {
		t.Fatalf("err = %v, want the missing-public-url refusal", err)
	}
	// With one, it gets past this check and fails on the cluster instead.
	err = run(t.Context(), options{Listen: "0.0.0.0:0", AllowInsecure: true, PublicURL: "https://dash.example",
		Kubeconfig: "/nonexistent", Out: io.Discard})
	if err == nil || strings.Contains(err.Error(), "--public-url") {
		t.Fatalf("err = %v, want it past the public-url check", err)
	}
	// Loopback needs no public URL: nothing observes the connection, and the
	// flag is a no-op there.
	err = run(t.Context(), options{Listen: "127.0.0.1:0", AllowInsecure: true, Kubeconfig: "/nonexistent", Out: io.Discard})
	if err == nil || strings.Contains(err.Error(), "--public-url") {
		t.Fatalf("err = %v, want loopback exempt from the public-url requirement", err)
	}
}

// The cookie's Secure flag follows the scheme users actually arrive on, which
// is the point of requiring the public URL above.
func TestCookieIsSecureWhenUsersArriveOverHTTPS(t *testing.T) {
	for _, tc := range []struct {
		name, publicURL string
		serveTLS        bool
		want            bool
	}{
		{name: "behind an https terminator", publicURL: "https://dash.example", want: true},
		{name: "serving TLS ourselves", serveTLS: true, want: true},
		{name: "an http public url is an explicit plaintext deployment", publicURL: "http://dash.example", want: false},
		{name: "the loopback default", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reachedOverHTTPS(tc.serveTLS, tc.publicURL)
			if got != tc.want {
				t.Fatalf("secure cookie = %t, want %t", got, tc.want)
			}
			sess, err := newSessions([]byte("secret"), time.Hour, got)
			if err != nil {
				t.Fatalf("newSessions: %v", err)
			}
			if sess.cookie("v", false).Secure != tc.want {
				t.Fatalf("the cookie's Secure flag did not follow")
			}
		})
	}
}

// ---- login CSRF ------------------------------------------------------------

// Login sets a cookie and has no session to derive a CSRF token from, so the
// session csrf() middleware cannot protect it. Unguarded, a cross-site form
// POST (a simple request — no CORS preflight to stop it) submits the
// ATTACKER'S token, the browser keeps the Set-Cookie that comes back, and the
// victim then drives the dashboard as whoever that token belongs to. SameSite
// does not help: it decides when a cookie is SENT, not whether one is stored.
func TestLoginRefusesACrossSiteOrigin(t *testing.T) {
	for _, tc := range []struct {
		name          string
		origin, site  string
		want          int
		wantSetCookie bool
	}{
		{name: "the dashboard's own origin", origin: "self", want: http.StatusOK, wantSetCookie: true},
		{name: "no origin at all (curl, CI)", want: http.StatusOK, wantSetCookie: true},
		{name: "a foreign origin", origin: "http://evil.example", want: http.StatusForbidden},
		{name: "a cross-site fetch-metadata label", site: "cross-site", want: http.StatusForbidden},
		{name: "the null origin", origin: "null", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			body, _ := json.Marshal(loginRequest{Token: goodToken})
			req, _ := http.NewRequest(http.MethodPost, h.dash.URL+"/api/v1/login", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			if tc.origin == "self" {
				req.Header.Set("Origin", h.dash.URL)
			} else if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			resp, err := h.dash.Client().Do(req)
			if err != nil {
				t.Fatalf("login: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d (%s)", resp.StatusCode, tc.want, bodyOf(t, resp))
			}
			issued := false
			for _, c := range resp.Cookies() {
				if c.Name == cookieName && c.Value != "" {
					issued = true
				}
			}
			if issued != tc.wantSetCookie {
				t.Fatalf("session cookie issued = %t, want %t", issued, tc.wantSetCookie)
			}
		})
	}
}

// The guard has to run before the token is spent: a foreign origin must not
// cost an apiserver round trip or a slot in the login rate limiter.
func TestLoginGuardRunsBeforeTheRateLimiterAndTheApiserver(t *testing.T) {
	h := newHarness(t, false)
	h.srv.logins = rate.NewLimiter(0, 1) // exactly one attempt available

	req, _ := http.NewRequest(http.MethodPost, h.dash.URL+"/api/v1/login", strings.NewReader(`{"token":"`+goodToken+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := h.dash.Client().Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: got %d, want 403", resp.StatusCode)
	}
	// The one available attempt must still be there for a legitimate caller.
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("a real login after a refused one: got %d, want 200 — the refusal spent the rate limit", got)
	}
}

// whoami is the other client built from s.base, and the one where an ambient
// credential is worst. It sets BearerToken to the token the caller presented,
// so a leaked client certificate is the only identity that could survive — and
// if one did, the apiserver would authenticate the REQUEST by certificate after
// the garbage token failed, hand back a SelfSubjectReview naming the dashboard,
// and whoami would report success. Login would then mint a session as the
// dashboard's own ServiceAccount for any string a caller typed into the token
// box: a full authentication bypass, not merely a privilege leak.
//
// The fake apiserver here behaves like that: cert present means authenticated
// as the dashboard whatever the token says, no cert means 401.
func TestWhoamiCannotAuthenticateByProcessCertificate(t *testing.T) {
	const dashboardIdentity = "system:serviceaccount:stratum-system:dashboard"
	_, clientPEM, clientKeyPEM := generateCert(t, "the-dashboard-process")

	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if len(r.TLS.PeerCertificates) == 0 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401,"message":"Unauthorized"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"SelfSubjectReview","apiVersion":"authentication.k8s.io/v1",
		  "status":{"userInfo":{"username":"` + dashboardIdentity + `","groups":["system:authenticated"]}}}`))
	}))
	api.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	api.StartTLS()
	defer api.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	base := &rest.Config{Host: api.URL, TLSClientConfig: rest.TLSClientConfig{
		CAData: caPEM, ServerName: "example.com",
		CertData: clientPEM, KeyData: clientKeyPEM,
	}}

	// Control arm: the fixture really can authenticate by certificate alone,
	// so a pass below means whoami declined to, not that mTLS never happened.
	control, err := kubernetes.NewForConfig(rest.CopyConfig(base))
	if err != nil {
		t.Fatalf("control client: %v", err)
	}
	rev, err := control.AuthenticationV1().SelfSubjectReviews().
		Create(context.Background(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("control arm: %v; the fixture is not exercising mTLS", err)
	}
	if rev.Status.UserInfo.Username != dashboardIdentity {
		t.Fatalf("control arm got %q, want %q", rev.Status.UserInfo.Username, dashboardIdentity)
	}

	// The real path, with a token the apiserver would never accept.
	s := &server{base: base}
	got, err := s.whoami(context.Background(), "not-a-real-token")
	if err == nil {
		t.Fatalf("whoami accepted a bogus token and returned %q; the process certificate authenticated the request", got.Username)
	}
	if !apierrors.IsUnauthorized(err) {
		t.Fatalf("whoami failed with %v, want an Unauthorized so login answers 401 rather than 500", err)
	}
}
