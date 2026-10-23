package ha

import (
	"errors"
	"strings"
	"testing"
)

// EtcdClusterHealthy gates the one phase of the etcd-CA migration that cannot
// be walked back node-by-node, so every way it can say "healthy" is worth
// pinning. The branch that matters most is the split brain: per-member health
// is satisfied by one, because each partition has a leader and each is locally
// convinced it is fine.

func member(ip string, leader uint64, errs []string, err error) EtcdMemberHealth {
	return EtcdMemberHealth{Endpoint: ip, Leader: leader, Errors: errs, Err: err}
}

func healthyFrom(m map[string]EtcdMemberHealth) func(string) EtcdMemberHealth {
	return func(ip string) EtcdMemberHealth { return m[ip] }
}

func TestEtcdClusterHealthy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ips     []string
		members map[string]EtcdMemberHealth
		wantErr string // substring; "" means healthy
	}{
		{
			name:    "every member agrees on one leader",
			ips:     []string{"a", "b", "c"},
			members: map[string]EtcdMemberHealth{"a": member("a", 7, nil, nil), "b": member("b", 7, nil, nil), "c": member("c", 7, nil, nil)},
		},
		{
			// The whole point of the function. Both partitions look healthy
			// member-by-member; only comparing leaders across them shows it.
			name:    "split brain is not healthy",
			ips:     []string{"a", "b", "c"},
			members: map[string]EtcdMemberHealth{"a": member("a", 7, nil, nil), "b": member("b", 7, nil, nil), "c": member("c", 9, nil, nil)},
			wantErr: "partitioned",
		},
		{
			name:    "a member that did not answer",
			ips:     []string{"a", "b"},
			members: map[string]EtcdMemberHealth{"a": member("a", 7, nil, nil), "b": member("b", 0, nil, errors.New("dial timeout"))},
			wantErr: "dial timeout",
		},
		{
			// Up and answering, but not in a working quorum. Reported, not hidden.
			name:    "a member with no leader",
			ips:     []string{"a", "b"},
			members: map[string]EtcdMemberHealth{"a": member("a", 7, nil, nil), "b": member("b", 0, nil, nil)},
			wantErr: "no leader",
		},
		{
			name:    "a member raising its own alarm",
			ips:     []string{"a", "b"},
			members: map[string]EtcdMemberHealth{"a": member("a", 7, nil, nil), "b": member("b", 7, []string{"NOSPACE"}, nil)},
			wantErr: "NOSPACE",
		},
		{
			// Fail closed: "nothing to check" must never read as "all clear" on
			// the gate for an irreversible step.
			name:    "no addresses at all",
			ips:     nil,
			wantErr: "no control-plane addresses",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := etcdClusterHealthyWith(tc.ips, healthyFrom(tc.members))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want healthy, got: %v", err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("want an error containing %q, got healthy", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// The refusal has to name which members disagreed, or an operator staring at a
// halted migration has nothing to act on.
func TestSplitBrainNamesBothPartitions(t *testing.T) {
	err := etcdClusterHealthyWith([]string{"a", "b", "c"}, healthyFrom(map[string]EtcdMemberHealth{
		"a": member("a", 7, nil, nil), "b": member("b", 9, nil, nil), "c": member("c", 9, nil, nil),
	}))
	if err == nil {
		t.Fatal("split brain reported healthy")
	}
	for _, want := range []string{"a", "b,c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name the partition %q, got: %v", want, err)
		}
	}
}

func TestMemberHealthy(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    EtcdMemberHealth
		want bool
	}{
		{"answered, has a leader, no alarms", member("a", 7, nil, nil), true},
		{"did not answer", member("a", 7, nil, errors.New("x")), false},
		{"no leader", member("a", 0, nil, nil), false},
		{"own alarm", member("a", 7, []string{"CORRUPT"}, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.Healthy(); got != tc.want {
				t.Errorf("Healthy() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEtcdEndpointBracketsIPv6. A bare IPv6 address concatenated with ":port"
// reads as another hextet, so every member lookup, the quorum check and the
// migration's closing verification would dial an address that does not exist —
// on a cluster this project supports (nodestate.DualStack; kubelet's node IP is
// "v4[,v6]").
func TestEtcdEndpointBracketsIPv6(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"10.0.0.1", "https://10.0.0.1:2379"},
		{"fd00::1", "https://[fd00::1]:2379"},
		{"::1", "https://[::1]:2379"},
	} {
		if got := etcdEndpoint(tc.ip); got != tc.want {
			t.Errorf("etcdEndpoint(%q) = %q, want %q", tc.ip, got, tc.want)
		}
	}
}
