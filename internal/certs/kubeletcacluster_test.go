package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

type kubeletClusterLog struct {
	calls   []string
	failAt  string
	cleaned int
}

func (l *kubeletClusterLog) ops() kubeletClusterOps {
	rec := func(s string) error {
		l.calls = append(l.calls, s)
		if s == l.failAt {
			return errors.New("boom")
		}
		return nil
	}
	return kubeletClusterOps{
		local: func(w io.Writer, o KubeletCAOptions) error {
			return rec(fmt.Sprintf("cp1 %s force=%v", o.Phase, o.Force))
		},
		remote: func(n cnode, phase string, fetchShared, force bool) error {
			return rec(fmt.Sprintf("%s %s fetch=%v force=%v", n.Name, phase, fetchShared, force))
		},
		publishKey: func() (func(), error) {
			return func() { l.cleaned++ }, rec("publish key")
		},
		unpublish: func() error { return rec("unpublish") },
		checkClient: func(leaf string, trusted bool) error {
			return rec(fmt.Sprintf("check %s trusted=%v", leaf, trusted))
		},
		confirm: func(string) error { return nil },
	}
}

var (
	kcaOtherCPs = []cnode{{Name: "cp2", IsCP: true}}
	kcaWorkers  = []cnode{{Name: "w1"}}
)

// Each phase reaches every node, control planes first, before the next
// begins; the API servers' certificate is checked against every kubelet
// before drop-root, and drop-root ends with every kubelet refusing a
// root-signed one.
func TestKubeletCAClusterRunsEachPhaseEverywhereInOrder(t *testing.T) {
	var log kubeletClusterLog
	if err := runKubeletCACluster(io.Discard, KubeletCAOptions{}, kcaOtherCPs, kcaWorkers, log.ops()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cp1 trust-both force=false", "publish key", "cp2 trust-both fetch=true force=false", "w1 trust-both fetch=false force=false",
		"cp1 issue-new force=false", "cp2 issue-new fetch=false force=false", "check apiserver-kubelet-client trusted=true",
		"cp1 drop-root force=false", "cp2 drop-root fetch=false force=false", "w1 drop-root fetch=false force=false",
		"check apiserver-kubelet-client trusted=true", "check admin trusted=false",
	}
	if !slices.Equal(log.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(log.calls, "\n  "), strings.Join(want, "\n  "))
	}
	if log.cleaned != 1 {
		t.Errorf("the staged key was cleaned up %d times, want 1", log.cleaned)
	}
}

// The rollback re-adds the root everywhere before any certificate moves back,
// and unpublishes the kubelet-client CA before the workers drop it.
func TestKubeletCAClusterRollbackRunsInReverse(t *testing.T) {
	var log kubeletClusterLog
	if err := runKubeletCACluster(io.Discard, KubeletCAOptions{Rollback: true, Force: true}, kcaOtherCPs, kcaWorkers, log.ops()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cp1 re-add-root force=true", "cp2 re-add-root fetch=false force=true", "w1 re-add-root fetch=false force=true",
		"cp1 reissue-root force=true", "cp2 reissue-root fetch=false force=true", "check apiserver-kubelet-client trusted=true",
		"cp1 remove-ca force=true", "cp2 remove-ca fetch=false force=true", "unpublish", "w1 remove-ca fetch=false force=true",
		"check apiserver-kubelet-client trusted=true",
	}
	if !slices.Equal(log.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(log.calls, "\n  "), strings.Join(want, "\n  "))
	}
	if log.cleaned != 0 {
		t.Errorf("a rollback staged a key")
	}
}

// A failure stops the run where it is, says how to resume or roll back, and
// still removes the staged key.
func TestKubeletCAClusterHaltsOnTheFirstFailure(t *testing.T) {
	for _, tc := range []struct {
		failAt, phase string
		rollback      bool
	}{
		{"w1 trust-both fetch=false force=false", pki.KubeletPhaseTrustBoth, false},
		{"check apiserver-kubelet-client trusted=true", pki.KubeletPhaseIssueNew, false},
		{"cp2 drop-root fetch=false force=false", pki.KubeletPhaseDropRoot, false},
		{"check admin trusted=false", pki.KubeletPhaseDropRoot, false},
		{"unpublish", pki.KubeletPhaseRemoveCA, true},
	} {
		t.Run(tc.failAt, func(t *testing.T) {
			log := kubeletClusterLog{failAt: tc.failAt}
			err := runKubeletCACluster(io.Discard, KubeletCAOptions{Rollback: tc.rollback}, kcaOtherCPs, kcaWorkers, log.ops())
			if err == nil {
				t.Fatal("the run went on past a failure")
			}
			if log.calls[len(log.calls)-1] != tc.failAt {
				t.Errorf("the run went on after %q: %v", tc.failAt, log.calls)
			}
			if !strings.Contains(err.Error(), "halted in "+tc.phase) || !strings.Contains(err.Error(), "--cluster") {
				t.Errorf("the error does not say where the run halted or how to go on: %v", err)
			}
			if !tc.rollback && log.cleaned != 1 {
				t.Errorf("the staged key was cleaned up %d times, want 1", log.cleaned)
			}
		})
	}
}

// A single control plane stages no key, since no follower fetches one.
func TestKubeletCAClusterOnOneControlPlaneStagesNoKey(t *testing.T) {
	var log kubeletClusterLog
	if err := runKubeletCACluster(io.Discard, KubeletCAOptions{}, nil, kcaWorkers, log.ops()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(log.calls, "publish key") {
		t.Errorf("a run with no follower staged a key: %v", log.calls)
	}
}

func TestCheckKubeletClientAuth(t *testing.T) {
	probe := func(code int, err error) func(context.Context, string, []*x509.Certificate, tls.Certificate) (int, error) {
		return func(context.Context, string, []*x509.Certificate, tls.Certificate) (int, error) { return code, err }
	}
	for _, tc := range []struct {
		name    string
		ip      string
		trusted bool
		code    int
		err     error
		want    string
	}{
		{"accepted", "10.0.0.6", true, 200, nil, ""},
		{"authorized elsewhere", "10.0.0.6", true, 403, nil, ""},
		{"refused", "10.0.0.6", true, 401, nil, "refuses apiserver-kubelet-client"},
		{"closed", "10.0.0.6", false, 401, nil, ""},
		{"still open", "10.0.0.6", false, 200, nil, "still accepts"},
		{"unreachable", "10.0.0.6", true, 0, errors.New("dial"), "check node n1"},
		{"no address", "", true, 200, nil, "no InternalIP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKubeletClientAuth(context.Background(), "n1", tc.ip, "apiserver-kubelet-client", nil, tls.Certificate{}, tc.trusted, probe(tc.code, tc.err))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}
