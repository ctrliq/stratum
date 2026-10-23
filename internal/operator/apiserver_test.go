package operator

import (
	"context"
	"strings"
	"testing"
)

// TestNodeInternalIP: the address a control plane's Node reports is the one its
// apiserver certificate carries, so that is what Cilium is pointed at.
func TestNodeInternalIP(t *testing.T) {
	node := func(addrs ...map[string]any) map[string]any {
		list := make([]any, len(addrs))
		for i, a := range addrs {
			list[i] = a
		}
		return map[string]any{"status": map[string]any{"addresses": list}}
	}
	addr := func(typ, a string) map[string]any { return map[string]any{"type": typ, "address": a} }
	for _, tc := range []struct {
		name string
		node map[string]any
		want string
	}{
		{"InternalIP", node(addr("Hostname", "cp1"), addr("InternalIP", "10.0.0.11")), "10.0.0.11"},
		{"IPv4 before IPv6", node(addr("InternalIP", "fd00::11"), addr("InternalIP", "10.0.0.11")), "10.0.0.11"},
		{"IPv6 only", node(addr("InternalIP", "fd00::11")), "fd00::11"},
		{"ExternalIP is not used", node(addr("ExternalIP", "203.0.113.5")), ""},
		{"garbage is not used", node(addr("InternalIP", "not-an-ip")), ""},
		{"no status", map[string]any{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeInternalIP(tc.node); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveAPIServerSkipsWhenAControlPlaneHasNoAddress: a
// labelled control plane whose InternalIP is momentarily missing must not be
// dropped from the URL list, nor, as the only one, replaced by this leader's
// own IP. Either re-renders Cilium and rolls every agent over a transient Node status.
//
// resolveAPIServer reads the deployer's dynamic client, so the tests reach it
// through the shared deployFixture's reactor, so how the deployer holds that
// client is a change to the fixture alone.
func TestResolveAPIServerSkipsWhenAControlPlaneHasNoAddress(t *testing.T) {
	t.Run("all addressed: every control plane, sorted", func(t *testing.T) {
		r := deployFixture(t, cp("cp2", "10.0.0.12"), cp("cp1", "10.0.0.11"))
		a, err := r.resolveAPIServer(context.Background(), "", "10.0.0.12")
		if err != nil || a.Host != "10.0.0.11" || len(a.URLs) != 2 {
			t.Fatalf("got %+v, %v", a, err)
		}
	})
	t.Run("one without an address: skip the pass", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"), cp("cp2", ""))
		if a, err := r.resolveAPIServer(context.Background(), "", "10.0.0.11"); err == nil || !strings.Contains(err.Error(), "cp2") {
			t.Fatalf("got %+v, %v; want an error naming cp2", a, err)
		}
	})
	t.Run("the only one without an address: skip, do not fall back to self", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", ""))
		if a, err := r.resolveAPIServer(context.Background(), "", "10.0.0.11"); err == nil {
			t.Fatalf("got %+v; the self-IP fallback replaced a labelled node's missing address", a)
		}
	})
	t.Run("nothing labelled yet: this node", func(t *testing.T) {
		r := deployFixture(t)
		a, err := r.resolveAPIServer(context.Background(), "", "10.0.0.11")
		if err != nil || a.Host != "10.0.0.11" {
			t.Fatalf("got %+v, %v; want the self fallback on a cluster with no labelled node", a, err)
		}
	})
}
