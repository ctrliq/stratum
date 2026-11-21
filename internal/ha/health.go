package ha

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type EtcdMemberHealth struct {
	Leader uint64
	Errors []string
	Err    error
}

func (m EtcdMemberHealth) Healthy() bool {
	return m.Err == nil && m.Leader != 0 && len(m.Errors) == 0
}

func EtcdMemberStatus(cpIP string) EtcdMemberHealth {
	ep := etcdEndpoint(cpIP)
	var h EtcdMemberHealth
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

// Per-member health alone is satisfied by a split brain; the leader-agreement check catches it.
func EtcdClusterHealthy(cpIPs []string) error {
	return etcdClusterHealthyWith(cpIPs, EtcdMemberStatus)
}

// Takes the per-member query as a parameter so the aggregation can be tested without a cluster.
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
