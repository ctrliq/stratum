package ha

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"
)

func shortCertsReach(t *testing.T, wait time.Duration) {
	t.Helper()
	savedWait, savedAttempt, savedInterval := certsReachWait, certsAttemptTimeout, certsReachInterval
	t.Cleanup(func() {
		certsReachWait, certsAttemptTimeout, certsReachInterval = savedWait, savedAttempt, savedInterval
	})
	certsReachWait, certsAttemptTimeout, certsReachInterval = wait, wait, time.Millisecond
}

// failingGets is a clientset whose first n reads of stratum-certs fail with
// err, after which the Secret is served.
func failingGets(n int, err error) (*fake.Clientset, *int) {
	cs := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system"}})
	calls := 0
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls <= n {
			return true, nil, err
		}
		return false, nil, nil
	})
	return cs, &calls
}

var refused = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

// A join reads stratum-certs through the VIP, which can refuse a connection
// while the previous control plane settles: the read waits it out.
func TestGetCertsSecretRetriesAnUnreachableControlPlane(t *testing.T) {
	shortCertsReach(t, time.Minute)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"connection refused", refused},
		{"no route to host", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}},
		{"service unavailable", apierrors.NewServiceUnavailable("starting")},
		{"too many requests", apierrors.NewTooManyRequests("busy", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := failingGets(3, tc.err)
			var out bytes.Buffer
			sec, err := getCertsSecret(t.Context(), cs.CoreV1().Secrets("kube-system"), &out)
			if err != nil || sec == nil || sec.Name != certsSecret {
				t.Fatalf("getCertsSecret = %v, %v; want the Secret once the control plane answers", sec, err)
			}
			if *calls != 4 {
				t.Errorf("reads = %d, want 3 failures and the answer", *calls)
			}
			if got := strings.Count(out.String(), "retrying"); got != 3 {
				t.Errorf("printed %d retry lines, want one per failure:\n%s", got, out.String())
			}
		})
	}
}

// An answer is a decision, not an outage: it returns at once.
func TestGetCertsSecretReturnsAnAnswerAtOnce(t *testing.T) {
	shortCertsReach(t, time.Minute)
	gr := schema.GroupResource{Resource: "secrets"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", apierrors.NewForbidden(gr, certsSecret, errors.New("not a control-plane token"))},
		{"not found", apierrors.NewNotFound(gr, certsSecret)},
		{"unauthorized", apierrors.NewUnauthorized("expired token")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, calls := failingGets(100, tc.err)
			if _, err := getCertsSecret(t.Context(), cs.CoreV1().Secrets("kube-system"), &bytes.Buffer{}); !errors.Is(err, tc.err) {
				t.Fatalf("getCertsSecret = %v, want %v", err, tc.err)
			}
			if *calls != 1 {
				t.Errorf("reads = %d, want 1: an answer is not retried", *calls)
			}
		})
	}
}

// An apiserver that has just come up behind the VIP refuses a valid token
// until it has read the token Secrets: after an unreachable control plane, an
// unauthorized answer is waited out too.
func TestGetCertsSecretWaitsOutUnauthorizedAfterAnOutage(t *testing.T) {
	shortCertsReach(t, time.Minute)
	cs := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system"}})
	answers := []error{refused, refused, apierrors.NewUnauthorized("token not found"), apierrors.NewUnauthorized("token not found")}
	calls := 0
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls <= len(answers) {
			return true, nil, answers[calls-1]
		}
		return false, nil, nil
	})
	sec, err := getCertsSecret(t.Context(), cs.CoreV1().Secrets("kube-system"), &bytes.Buffer{})
	if err != nil || sec == nil || sec.Name != certsSecret {
		t.Fatalf("getCertsSecret = %v, %v; want the Secret once the new apiserver knows the token", sec, err)
	}
	if calls != len(answers)+1 {
		t.Errorf("reads = %d, want %d failures and the answer", calls, len(answers))
	}
}

// An expired token still fails within the budget after an outage, with the
// unauthorized answer.
func TestGetCertsSecretGivesUpOnUnauthorizedAfterTheBudget(t *testing.T) {
	shortCertsReach(t, 20*time.Millisecond)
	cs := fake.NewSimpleClientset()
	calls := 0
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, refused
		}
		return true, nil, apierrors.NewUnauthorized("expired token")
	})
	_, err := getCertsSecret(t.Context(), cs.CoreV1().Secrets("kube-system"), &bytes.Buffer{})
	if !apierrors.IsUnauthorized(err) {
		t.Fatalf("getCertsSecret = %v, want the unauthorized answer once the budget is spent", err)
	}
}

// The wait is bounded, and the error says how long it waited and why.
func TestGetCertsSecretGivesUpAfterTheBudget(t *testing.T) {
	shortCertsReach(t, 20*time.Millisecond)
	cs, calls := failingGets(1<<30, refused)
	_, err := getCertsSecret(t.Context(), cs.CoreV1().Secrets("kube-system"), &bytes.Buffer{})
	if !errors.Is(err, syscall.ECONNREFUSED) || !strings.Contains(err.Error(), "still unreachable after") {
		t.Fatalf("getCertsSecret = %v, want the refusal and the budget it spent", err)
	}
	if *calls < 2 {
		t.Errorf("reads = %d, want retries before giving up", *calls)
	}
}

// stallingSecrets is a Secret client whose every read blocks until its
// context ends, as a read does when the VIP holder accepted the connection
// and then stopped answering.
type stallingSecrets struct {
	corev1client.SecretInterface
	reads int
}

func (s *stallingSecrets) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*corev1.Secret, error) {
	s.reads++
	<-ctx.Done()
	return nil, ctx.Err()
}

// A read that stalls ends with the budget instead of outlasting it, and no
// read starts after the budget is spent.
func TestGetCertsSecretBoundsAStalledRead(t *testing.T) {
	shortCertsReach(t, 100*time.Millisecond)
	certsAttemptTimeout = 30 * time.Millisecond
	s := &stallingSecrets{}
	start := time.Now()
	_, err := getCertsSecret(t.Context(), s, &bytes.Buffer{})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("getCertsSecret took %s with a %s budget; a stalled read outlasted it", took, certsReachWait)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still unreachable after") {
		t.Fatalf("getCertsSecret = %v, want the read's deadline and the budget it spent", err)
	}
	if s.reads < 2 || s.reads > 4 {
		t.Errorf("reads = %d, want a few bounded attempts within the budget", s.reads)
	}
}

// A join that is cancelled stops waiting at once.
func TestGetCertsSecretStopsOnCancel(t *testing.T) {
	shortCertsReach(t, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := getCertsSecret(ctx, &stallingSecrets{}, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("getCertsSecret on a cancelled context = %v, want context.Canceled", err)
	}
}
