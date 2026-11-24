package bootstrap

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
)

func closedPortURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return "https://" + addr
}

func unreachableFor(t *testing.T, n int32, real string) (calls *atomic.Int32) {
	t.Helper()
	calls = &atomic.Int32{}
	dead := closedPortURL(t)
	old := fetchClusterInfo
	fetchClusterInfo = func(_ string, timeout time.Duration) (map[string]string, error) {
		if calls.Add(1) <= n {
			return fetchClusterInfoData(dead, timeout)
		}
		return fetchClusterInfoData(real, timeout)
	}
	t.Cleanup(func() { fetchClusterInfo = old })
	return calls
}

func TestDiscoverCARetriesAnUnreachableControlPlane(t *testing.T) {
	caPEM, pin := makeCA(t)
	url, _ := clusterInfoServer(t, signedData(kubeconfigWith(caPEM)))
	calls := unreachableFor(t, 3, url)

	if _, err := DiscoverCA(url, testToken, pin); err != nil {
		t.Fatalf("a control plane unreachable for three attempts failed the join: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("fetched %d times, want 3 refused and 1 answered", got)
	}
}

func TestDiscoverCAGivesUpOnAControlPlaneThatNeverAnswers(t *testing.T) {
	_, pin := makeCA(t)
	calls := unreachableFor(t, 1<<30, "")

	_, err := DiscoverCA(closedPortURL(t), testToken, pin)
	// The final dial may time out instead of reporting the earlier refusals.
	if err == nil || !transientDialError(err) {
		t.Errorf("want a transient network failure, got: %v", err)
	}
	for _, want := range []string{"still unreachable after", "attempts"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if calls.Load() < 2 {
		t.Errorf("gave up after %d attempt(s); an unreachable control plane must be retried", calls.Load())
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "Client.Timeout exceeded while awaiting headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestDiscoverCAReportsTheCauseNotTheClippedTimeout(t *testing.T) {
	_, pin := makeCA(t)
	dead := closedPortURL(t)
	// A budget shorter than one fetch's timeout makes the second attempt the clipped final one.
	oldBudget, oldInterval, oldFetch := reachWaitTimeout, reachWaitInterval, fetchTimeout
	reachWaitTimeout, reachWaitInterval, fetchTimeout = 200*time.Millisecond, 5*time.Millisecond, time.Hour
	t.Cleanup(func() { reachWaitTimeout, reachWaitInterval, fetchTimeout = oldBudget, oldInterval, oldFetch })
	old := fetchClusterInfo
	var calls atomic.Int32
	fetchClusterInfo = func(_ string, timeout time.Duration) (map[string]string, error) {
		if calls.Add(1) == 1 {
			return fetchClusterInfoData(dead, timeout)
		}
		time.Sleep(timeout) // spends the rest of the budget, as a slow dial would
		return nil, timeoutErr{}
	}
	t.Cleanup(func() { fetchClusterInfo = old })

	_, err := DiscoverCA(dead, testToken, pin)
	if err == nil || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "Client.Timeout") {
		t.Errorf("want the refusal that made it unreachable, got: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("fetched %d times, want the refusal and one clipped attempt", got)
	}
}

func TestDiscoverCADoesNotRetryAnAnswer(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	_, pin := makeCA(t)

	if _, err := DiscoverCA(srv.URL, testToken, pin); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("want the 401 back, got: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("a server that answered 401 was asked %d times, want once", got)
	}
}

func TestDiscoverCAReachWaitDoesNotSpendTheSignatureBudget(t *testing.T) {
	caPEM, pin := makeCA(t)
	kc := kubeconfigWith(caPEM)
	var answered atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := map[string]string{clusterinfo.KubeConfigKey: kc}
		if answered.Add(1) > 2 { // unsigned twice, then signed
			data = signedData(kc)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = jsonEncode(w, data)
	}))
	t.Cleanup(srv.Close)

	dead := closedPortURL(t)
	unreachableUntil := time.Now().Add(signatureWaitTimeout + 20*time.Millisecond)
	old := fetchClusterInfo
	fetchClusterInfo = func(_ string, timeout time.Duration) (map[string]string, error) {
		if time.Now().Before(unreachableUntil) {
			return fetchClusterInfoData(dead, timeout)
		}
		return fetchClusterInfoData(srv.URL, timeout)
	}
	t.Cleanup(func() { fetchClusterInfo = old })

	if _, err := DiscoverCA(srv.URL, testToken, pin); err != nil {
		t.Fatalf("the signature wait was spent waiting for the network: %v", err)
	}
}

func TestTransientDialErrorClassifiesRealFailures(t *testing.T) {
	_, err := fetchClusterInfoData(closedPortURL(t), fetchTimeout)
	if err == nil || !transientDialError(err) {
		t.Errorf("a refused dial must be transient: %v", err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, err = fetchClusterInfoData(srv.URL, fetchTimeout)
	if err == nil || transientDialError(err) {
		t.Errorf("an HTTP status is an answer, not a transient dial failure: %v", err)
	}
}

func jsonEncode(w io.Writer, data map[string]string) error {
	return json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "data": data})
}

// Answers none of the first n requests (n < 0: never), then serves signed cluster-info.
func hangingServer(t *testing.T, n int32, kc string) (url string, requests *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	done := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := requests.Add(1); n < 0 || c <= n {
			select {
			case <-done:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = jsonEncode(w, signedData(kc))
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv.URL, requests
}

func TestDiscoverCARetriesATimeout(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	caPEM, pin := makeCA(t)
	url, requests := hangingServer(t, 1, kubeconfigWith(caPEM))

	if _, err := fetchClusterInfoData(url, 30*time.Millisecond); err == nil || !transientDialError(err) {
		t.Errorf("a request that timed out must be transient: %v", err)
	}
	requests.Store(0)
	if _, err := DiscoverCA(url, testToken, pin); err != nil {
		t.Fatalf("a control plane that timed out once failed the join: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("fetched %d times, want one timed out and one answered", got)
	}
}

func TestDiscoverCAReachBudgetIsABound(t *testing.T) {
	_, pin := makeCA(t)
	url, _ := hangingServer(t, -1, "")

	start := time.Now()
	_, err := DiscoverCA(url, testToken, pin)
	if err == nil || !strings.Contains(err.Error(), "still unreachable after") {
		t.Errorf("want the reach budget's error, got: %v", err)
	}
	if took := time.Since(start); took > reachWaitTimeout+time.Second {
		t.Errorf("the reach wait took %v against a %v budget", took, reachWaitTimeout)
	}
}

func TestFetchRefusesANonPositiveTimeout(t *testing.T) {
	url, _ := hangingServer(t, -1, "")
	for _, timeout := range []time.Duration{0, -time.Millisecond} {
		start := time.Now()
		_, err := fetchClusterInfoData(url, timeout)
		if err == nil || !strings.Contains(err.Error(), "no time left") {
			t.Errorf("timeout %v: want a refusal, got: %v", timeout, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("timeout %v: the fetch waited %v on a hanging endpoint", timeout, took)
		}
	}
}

func TestDiscoverCAStartsNoAttemptOnASpentBudget(t *testing.T) {
	oldInterval, oldBudget := reachWaitInterval, reachWaitTimeout
	reachWaitInterval, reachWaitTimeout = time.Second, 50*time.Millisecond
	t.Cleanup(func() { reachWaitInterval, reachWaitTimeout = oldInterval, oldBudget })

	dead := closedPortURL(t)
	var timeouts []time.Duration
	old := fetchClusterInfo
	fetchClusterInfo = func(_ string, timeout time.Duration) (map[string]string, error) {
		timeouts = append(timeouts, timeout)
		return fetchClusterInfoData(dead, time.Second)
	}
	t.Cleanup(func() { fetchClusterInfo = old })
	_, pin := makeCA(t)

	_, err := DiscoverCA(dead, testToken, pin)
	if err == nil || !strings.Contains(err.Error(), "still unreachable after") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("want the reach budget's error wrapping the last dial failure, got: %v", err)
	}
	for i, to := range timeouts {
		if to <= 0 {
			t.Errorf("attempt %d was handed a non-positive timeout (%v)", i+1, to)
		}
	}
	if len(timeouts) != 1 {
		t.Errorf("made %d attempts, want 1: the budget was spent before the second", len(timeouts))
	}
}

func TestDiscoverCARetriesAnonymousRBACPropagation(t *testing.T) {
	caPEM, pin := makeCA(t)
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 3 {
			http.Error(w, "anonymous discovery grant has not propagated", http.StatusForbidden)
			return
		}
		_ = jsonEncode(w, signedData(kubeconfigWith(caPEM)))
	}))
	t.Cleanup(srv.Close)
	if _, err := DiscoverCA(srv.URL, testToken, pin); err != nil {
		t.Fatalf("discovery failed during RBAC propagation: %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("requests=%d; want three denials followed by signed discovery", calls.Load())
	}
}

func TestDiscoverCABoundsAnonymousRBACDenial(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, pin := makeCA(t)
	_, err := DiscoverCA(srv.URL, testToken, pin)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "after") || calls.Load() < 2 {
		t.Fatalf("requests=%d err=%v; want bounded retries preserving the denial", calls.Load(), err)
	}
}
