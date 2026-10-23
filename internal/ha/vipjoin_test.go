package ha

import (
	"io"

	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// TestJoinEtcdEndpoints pins that a join through the VIP dialled etcd on
// the VIP, where nothing listens. The published member list wins; without it
// the join host is used when it is a node, and a VIP is refused up front.
func TestJoinEtcdEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name            string
		published       []string
		joinHost, vip   string
		want            []string
		wantErrContains string
	}{
		{"published list wins over the VIP", []string{"10.0.0.11", "10.0.0.12"}, "10.0.0.10", "10.0.0.10", []string{"10.0.0.11", "10.0.0.12"}, ""},
		{"published list wins over a node", []string{"10.0.0.11"}, "10.0.0.12", "", []string{"10.0.0.11"}, ""},
		{"older cluster, joined via a node", nil, "10.0.0.11", "10.0.0.10", []string{"10.0.0.11"}, ""},
		{"older cluster, no VIP", nil, "10.0.0.11", "", []string{"10.0.0.11"}, ""},
		{"older cluster, joined via the VIP", nil, "10.0.0.10", "10.0.0.10", nil, "is the VIP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := joinEtcdEndpoints(tc.published, tc.joinHost, tc.vip)
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEtcdEndpointsAnnotation: the annotation is data a joiner dials, so only
// literal IPs survive the parse — no hostnames, no URLs, no duplicates.
func TestEtcdEndpointsAnnotation(t *testing.T) {
	in := []string{"10.0.0.11", "fd00::12", "10.0.0.13"}
	if got := parseEtcdEndpoints(formatEtcdEndpoints(in)); !slices.Equal(got, in) {
		t.Errorf("round trip = %q, want %q", got, in)
	}
	got := parseEtcdEndpoints(" 10.0.0.11 ,evil.example.com,https://10.0.0.9:2379,,10.0.0.11,10.0.0.12")
	if want := []string{"10.0.0.11", "10.0.0.12"}; !slices.Equal(got, want) {
		t.Errorf("parse = %q, want %q", got, want)
	}
	if got := parseEtcdEndpoints(""); got != nil {
		t.Errorf("empty annotation = %q, want nil (the older-cluster fallback keys on it)", got)
	}
}

// TestIsUnhealthyCluster: MemberAdd retries etcd's strict-reconfig refusal and
// nothing else. The client hands back the raw gRPC status, so the match has to
// work on that form and on the wrapped one.
func TestIsUnhealthyCluster(t *testing.T) {
	if !isUnhealthyCluster(rpctypes.ErrGRPCUnhealthy) {
		t.Error("the raw gRPC unhealthy-cluster status is not recognised")
	}
	if !isUnhealthyCluster(rpctypes.ErrUnhealthy) {
		t.Error("the typed ErrUnhealthy is not recognised")
	}
	for _, err := range []error{nil, rpctypes.ErrGRPCPeerURLExist, rpctypes.ErrGRPCMemberBadURLs, errors.New("connection refused")} {
		if isUnhealthyCluster(err) {
			t.Errorf("%v would be retried; only the unhealthy-cluster refusal is safe to retry", err)
		}
	}
}

// TestPublishableEtcdEndpoints: publishing the member list is
// not best-effort. A token minted without it prints a join that a VIP join
// refuses, and would have replaced a good list on an existing Secret.
func TestPublishableEtcdEndpoints(t *testing.T) {
	if got, err := publishableEtcdEndpoints([]string{"10.0.0.11", "10.0.0.12"}, nil); err != nil || got != "10.0.0.11,10.0.0.12" {
		t.Errorf("healthy etcd: %q, %v", got, err)
	}
	if _, err := publishableEtcdEndpoints(nil, errors.New("context deadline exceeded")); err == nil ||
		!strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("an etcd listing failure was not refused: %v", err)
	}
	if _, err := publishableEtcdEndpoints(nil, nil); err == nil {
		t.Error("an empty member list was accepted")
	}
	if _, err := publishableEtcdEndpoints([]string{"not-an-ip"}, nil); err == nil {
		t.Error("a list with no usable address was accepted")
	}
}

// TestUploadCertsNeedsAControlPlane: the upload reads this
// node's CA keys and its local etcd, so from an admin workstation it has to say
// that plainly, before any other step fails in a way that names etcd.
func TestUploadCertsNeedsAControlPlane(t *testing.T) {
	orig := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = orig })
	err := UploadCerts(strings.Repeat("a", 64), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "must run on a control plane") {
		t.Fatalf("UploadCerts without CA material = %v", err)
	}
}

// TestRejoinRefusesTheVIP: etcd does not listen on the VIP,
// and the seed is one specific control plane, so rejoin through the VIP is
// refused before anything is inspected or changed.
func TestRejoinRefusesTheVIP(t *testing.T) {
	touched := []string{}
	restoreSeams(t,
		func() error { touched = append(touched, "pki"); return nil },
		func(string) error { touched = append(touched, "seed"); return nil },
		func(w io.Writer) error { touched = append(touched, "wipe"); return nil })
	rejoinSupervisorRunning = func() bool { touched = append(touched, "supervisor"); return true }
	rejoinLoadState = func() (*nodestate.NodeState, error) {
		return &nodestate.NodeState{Role: "controller", VIP: nodestate.VIP{Address: "10.0.0.10"}}, nil
	}
	err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.10", false)
	if err == nil || !strings.Contains(err.Error(), "is the cluster VIP") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("rejoin through the VIP = %v", err)
	}
	if len(touched) != 0 {
		t.Errorf("work ran before the VIP refusal: %v", touched)
	}
}

// TestUploadCertsMintsTheEtcdTrustBeforeListing: the member
// listing authenticates with the etcd trust bundle, which a pre-etcd-CA-split control
// plane only has once EnsureEtcdTrustBundle has minted it.
func TestUploadCertsMintsTheEtcdTrustBeforeListing(t *testing.T) {
	oldReq, oldEnsure, oldList := requireControlPlanePKI, ensureEtcdTrust, uploadListMembers
	t.Cleanup(func() { requireControlPlanePKI, ensureEtcdTrust, uploadListMembers = oldReq, oldEnsure, oldList })
	var order []string
	requireControlPlanePKI = func(bool) error { return nil }
	ensureEtcdTrust = func() error { order = append(order, "ensure"); return nil }
	uploadListMembers = func() ([]string, error) { order = append(order, "list"); return nil, errors.New("stop here") }
	_ = UploadCerts(strings.Repeat("a", 64), nil, nil)
	if len(order) != 2 || order[0] != "ensure" || order[1] != "list" {
		t.Fatalf("order = %v, want the trust bundle ensured before the member listing", order)
	}
}

// TestSameIP: the VIP refusals compared strings, so an IPv6
// VIP written in another form slipped past them.
func TestSameIP(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"10.0.0.10", "10.0.0.10", true},
		{"fd00::10", "fd00:0:0::10", true},
		{"fd00::10", "FD00::10", true},
		{"10.0.0.10", "10.0.0.11", false},
		{"10.0.0.10", "", false},
		{"", "", false},
		{"vip.example", "vip.example", false},
	} {
		if got := sameIP(tc.a, tc.b); got != tc.want {
			t.Errorf("sameIP(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if _, err := joinEtcdEndpoints(nil, "fd00::10", "fd00:0:0::10"); err == nil {
		t.Error("a join through an IPv6 VIP written in another form was not refused")
	}
}
