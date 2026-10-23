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

// kubeletServing sorts the nodes by whether their kubelet serves a certificate
// roots verify. untrusted names the nodes whose kubelet serves one no CA in
// roots signed: the apiserver verifies kubelet serving certs against that
// bundle in every profile, so kubectl logs and exec to those nodes fail. A node
// that finalized a `certs rotate-ca` on a release whose default profile did not
// verify them can still hold one signed by the CA the rotation dropped.
// unverified describes each node that could not be checked.
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

// warnUntrustedKubeletServing prints the upgrade's known gap: from the first
// control-plane upgrade until each named node is upgraded, logs and exec to it
// fail. The upgraded node's kubelet moves the untrusted cert aside as it starts
// and requests a new one, which closes the gap. A node it could not check is
// named too, so no warning never stands in for "checked and fine".
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
