package upgrade

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ctrliq/stratum/internal/pki"
)

// A node can hold a serving cert signed by a CA an earlier rotate-ca dropped.
func kubeletServing(ctx context.Context, nodes []Node, roots []*x509.Certificate, probe pki.KubeletProbe) (untrusted, unverified []string) {
	targets := make([]pki.KubeletTarget, len(nodes))
	for i, n := range nodes {
		targets[i] = pki.KubeletTarget{Name: n.Name, IP: n.IP}
	}
	for i, err := range pki.ProbeKubelets(ctx, targets, roots, probe) {
		switch {
		case errors.Is(err, pki.ErrKubeletServingUntrusted):
			untrusted = append(untrusted, nodes[i].Name)
		case err != nil:
			unverified = append(unverified, fmt.Sprintf("%s (%v)", nodes[i].Name, err))
		}
	}
	return untrusted, unverified
}

// The gap closes once the node upgrades: its kubelet requests a new serving cert.
func warnUntrustedKubeletServing(w io.Writer, ctx context.Context, nodes []Node) {
	roots, err := pki.KubeletServingRoots()
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not read the cluster CA bundle to check kubelet serving certs: %v\n", err)
		return
	}
	untrusted, unverified := kubeletServing(ctx, nodes, roots, pki.ProbeKubeletServing)
	if len(untrusted) > 0 {
		fmt.Fprintf(w, "  WARN: the kubelet on %s serves a certificate the cluster CA does not sign, likely left over from an earlier `stratum certs rotate-ca`.\n"+
			"        The apiserver verifies kubelet serving certs in every profile, so kubectl logs/exec to those nodes fail\n"+
			"        from the first control-plane upgrade until each is upgraded; its kubelet then requests a new serving cert.\n",
			strings.Join(untrusted, ", "))
	}
	for _, n := range unverified {
		fmt.Fprintf(w, "  WARN: could not check the kubelet serving cert on %s; if it is not signed by the cluster CA, kubectl logs/exec to it fail after the upgrade\n", n)
	}
}
