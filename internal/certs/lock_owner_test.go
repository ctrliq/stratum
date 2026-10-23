package certs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// These regression tests drive the migration lock through the real client-go HTTP
// transport, because the failures they pin live there: client-go retries a DELETE
// answered with 503 + Retry-After, and a context can expire between the API
// server's response and the caller reading it. Both are modeled below as
// reproductions against a real transport.

// lockServer is an API server holding one lock Secret, with UID preconditions.
type lockServer struct {
	mu      sync.Mutex
	present bool
	token   string
	uid     string
	seq     int
	deletes int

	postCommitsThen int // if set, POST stores the lock and then answers with this status
	// successorAfterFirstDelete: the first DELETE commits, another migration takes
	// the freed name, and a proxy answers 503 + Retry-After for that DELETE.
	successorAfterFirstDelete bool
}

func (l *lockServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		status := func(code int, reason string) {
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":%q,"code":%d}`, reason, code)
		}
		secret := func() *corev1.Secret {
			return &corev1.Secret{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
				ObjectMeta: metav1.ObjectMeta{Name: etcdMigrationLockSecret, Namespace: caNamespace, UID: types.UID(l.uid)},
				Data:       map[string][]byte{"token": []byte(l.token), "holder": []byte("cp?")},
			}
		}
		switch r.Method {
		case http.MethodPost:
			if l.present {
				status(409, "AlreadyExists")
				return
			}
			var sec corev1.Secret
			if err := json.NewDecoder(r.Body).Decode(&sec); err != nil {
				t.Error(err)
				status(400, "BadRequest")
				return
			}
			l.seq++
			l.present, l.token, l.uid = true, string(sec.Data["token"]), fmt.Sprintf("uid-%d", l.seq)
			switch l.postCommitsThen {
			case 0:
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(secret())
			case 409:
				status(409, "AlreadyExists")
			default:
				status(l.postCommitsThen, "InternalError")
			}
		case http.MethodGet:
			if !l.present {
				status(404, "NotFound")
				return
			}
			_ = json.NewEncoder(w).Encode(secret())
		case http.MethodDelete:
			l.deletes++
			var opts metav1.DeleteOptions
			if body, _ := io.ReadAll(r.Body); len(body) > 0 {
				_ = json.Unmarshal(body, &opts)
			}
			if !l.present {
				status(404, "NotFound")
				return
			}
			if opts.Preconditions != nil && opts.Preconditions.UID != nil && string(*opts.Preconditions.UID) != l.uid {
				status(409, "Conflict")
				return
			}
			l.present = false
			if l.successorAfterFirstDelete && l.deletes == 1 {
				l.seq++
				l.present, l.token, l.uid = true, "successor-token", fmt.Sprintf("uid-%d", l.seq)
				w.Header().Set("Retry-After", "0")
				status(503, "ServiceUnavailable")
				return
			}
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Success","code":200}`)
		}
	})
}

func (l *lockServer) client(t *testing.T, wrap func(http.RoundTripper) http.RoundTripper) kubernetes.Interface {
	t.Helper()
	srv := httptest.NewServer(l.handler(t))
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{
		Host:          srv.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json"},
		WrapTransport: wrap,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func (l *lockServer) state() (present bool, token string, deletes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.present, l.token, l.deletes
}

// A DELETE that committed, got a retryable 503, and is retried must not remove
// the lock a successor took in between: the release and the orphan removal both
// delete by UID.
func TestLockDeleteNeverRemovesASuccessorAfterARetriedDelete(t *testing.T) {
	for _, mode := range []string{"release", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			l := &lockServer{successorAfterFirstDelete: true}
			if mode == "orphan" {
				l.postCommitsThen = 500 // the Create stored the lock, then failed
			}
			cs := l.client(t, nil)
			release, err := acquireMigrationLock(io.Discard, t.Context(), cs, "cp1")
			if mode == "orphan" {
				if err == nil {
					t.Fatal("want the failed Create reported")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				release()
			}
			present, token, deletes := l.state()
			if deletes < 2 {
				t.Fatalf("scenario not exercised: %d DELETE(s), want the retry too", deletes)
			}
			if !present || token != "successor-token" {
				t.Errorf("successor's lock was removed (present=%v token=%q) after %d deletes", present, token, deletes)
			}
		})
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A Create that committed and answered AlreadyExists, with the caller's context
// expiring before the owner lookup, must not leave our lock behind and must
// report the cancellation rather than proceed as the holder.
func TestAcquireMigrationLockReconcilesAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := &lockServer{postCommitsThen: 409}
	cs := l.client(t, func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := rt.RoundTrip(r)
			if err == nil && r.Method == http.MethodPost && resp.StatusCode == 409 {
				resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			}
			return resp, err
		})
	})
	release, err := acquireMigrationLock(io.Discard, ctx, cs, "cp1")
	if err == nil || release != nil {
		t.Fatalf("release=%v err=%v, want an error and no release closure", release != nil, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap the cancellation", err)
	}
	if present, _, _ := l.state(); present {
		t.Error("our own lock was left behind and would block every later migration")
	}
}

// The same lookup failure against someone else's lock must leave it alone.
func TestAcquireMigrationLockKeepsAForeignLockWhenTheLookupFails(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := &lockServer{present: true, token: "someone-else", uid: "uid-9", seq: 9}
	cs := l.client(t, func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := rt.RoundTrip(r)
			if err == nil && r.Method == http.MethodPost && resp.StatusCode == 409 {
				resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			}
			return resp, err
		})
	})
	if _, err := acquireMigrationLock(io.Discard, ctx, cs, "cp1"); err == nil {
		t.Fatal("want an error")
	}
	if present, token, _ := l.state(); !present || token != "someone-else" {
		t.Errorf("a foreign lock was removed (present=%v token=%q)", present, token)
	}
}
