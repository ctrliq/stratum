package ha

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Etcd health, for callers that have to prove a raft cluster is whole before
// doing something irreversible to it (the etcd CA drop-root gate).
//
// This package already dials peers' etcd for MemberAdd/MemberRemove, but it had
// no way to ASK one how it is — so "is the cluster healthy" could only be
// answered indirectly, by whether the apiservers were serving. That is a real
// signal and the gate still uses it, but it is not the same question: an
// apiserver talks to its own local etcd, so N healthy apiservers prove N etcds
// are individually up and prove nothing about whether they agree.
//
// Dropping a trust anchor is exactly the operation where that distinction
// matters. A member that is up but partitioned will not survive its peers
// narrowing their trust, and it is invisible to a per-node liveness check.

// EtcdMemberHealth is one member's answer. Err set means the member did not
// answer, which is never treated as healthy.
type EtcdMemberHealth struct {
	Endpoint string
	Leader   uint64
	Errors   []string
	Err      error
}

// Healthy reports whether this member answered, knows a leader, and raised no
// alarms of its own.
func (m EtcdMemberHealth) Healthy() bool {
	return m.Err == nil && m.Leader != 0 && len(m.Errors) == 0
}

// EtcdMemberStatus asks the etcd at cpIP how it is. A member with no leader is
// reported, not hidden: it is up, it is answering, and it is not part of a
// working quorum.
func EtcdMemberStatus(cpIP string) EtcdMemberHealth {
	ep := etcdEndpoint(cpIP)
	h := EtcdMemberHealth{Endpoint: ep}
	cli, err := etcdClient(cpIP)
	if err != nil {
		h.Err = err
		return h
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := cli.Status(ctx, ep)
	if err != nil {
		h.Err = fmt.Errorf("etcd status %s: %w", ep, err)
		return h
	}
	h.Leader, h.Errors = st.Leader, st.Errors
	return h
}

// EtcdClusterHealthy fails closed: every named member must answer, must know a
// leader, and every member must name the SAME leader.
//
// The agreement check is the point. Per-member health is satisfied by a split
// brain — two partitions, each with a leader, each locally convinced it is fine
// — and a split brain is the one condition under which narrowing etcd's trust
// cluster-wide cannot be walked back, because the partition that loses will not
// accept the anchor the other half kept.
func EtcdClusterHealthy(cpIPs []string) error {
	return etcdClusterHealthyWith(cpIPs, EtcdMemberStatus)
}

// Pass the per-member query explicitly so the aggregation — which is the part
// with the branches, and where the split-brain check that justifies this
// function lives — can be tested without a cluster.
func etcdClusterHealthyWith(cpIPs []string, status func(string) EtcdMemberHealth) error {
	if len(cpIPs) == 0 {
		return fmt.Errorf("etcd health: no control-plane addresses to check")
	}
	leaders := map[uint64][]string{}
	var problems []string
	for _, ip := range cpIPs {
		h := status(ip)
		switch {
		case h.Err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", ip, h.Err))
		case h.Leader == 0:
			problems = append(problems, fmt.Sprintf("%s: no leader (member is up but not in a working quorum)", ip))
		case len(h.Errors) > 0:
			problems = append(problems, fmt.Sprintf("%s: etcd reports %s", ip, strings.Join(h.Errors, "; ")))
		default:
			leaders[h.Leader] = append(leaders[h.Leader], ip)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("etcd is not healthy on every control plane: %s", strings.Join(problems, " | "))
	}
	if len(leaders) > 1 {
		return fmt.Errorf("etcd members do not agree on a leader (%s) — the cluster is partitioned", describeLeaders(leaders))
	}
	return nil
}

func describeLeaders(leaders map[uint64][]string) string {
	parts := make([]string, 0, len(leaders))
	for id, ips := range leaders {
		sort.Strings(ips)
		parts = append(parts, fmt.Sprintf("%x claimed by %s", id, strings.Join(ips, ",")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
