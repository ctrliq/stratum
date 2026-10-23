package upgrade

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

func TestKubeletServing(t *testing.T) {
	nodes := []Node{
		{Name: "cp1", IP: "192.0.2.10", IsCP: true},
		{Name: "w1", IP: "192.0.2.20"},
		{Name: "w2", IP: "192.0.2.21"},
		{Name: "w3", IP: "192.0.2.22"},
		{Name: "w4"},
	}
	results := map[string]error{
		"192.0.2.20:10250": errors.Join(pki.ErrKubeletServingUntrusted, errors.New("x509: certificate signed by unknown authority")),
		"192.0.2.21:10250": errors.New("connection refused"),
	}
	var mu sync.Mutex
	var probed []string
	untrusted, unverified := kubeletServing(t.Context(), nodes, nil, func(_ context.Context, addr string, _ []*x509.Certificate) error {
		mu.Lock()
		probed = append(probed, addr)
		mu.Unlock()
		return results[addr]
	})
	slices.Sort(probed)
	if want := []string{"w1"}; !slices.Equal(untrusted, want) {
		t.Errorf("untrusted = %v, want %v: only a kubelet that answered with an unverified cert is named", untrusted, want)
	}
	if len(unverified) != 2 || !strings.HasPrefix(unverified[0], "w2 (") || !strings.Contains(unverified[0], "connection refused") ||
		!strings.HasPrefix(unverified[1], "w4 (") {
		t.Errorf("unverified = %v, want w2 (unreachable) and w4 (no InternalIP) named with their reasons", unverified)
	}
	if want := []string{"192.0.2.10:10250", "192.0.2.20:10250", "192.0.2.21:10250", "192.0.2.22:10250"}; !slices.Equal(probed, want) {
		t.Errorf("probed %v, want %v (a node with no InternalIP is not dialled)", probed, want)
	}
}
