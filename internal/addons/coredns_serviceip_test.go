package addons

import (
	"bytes"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCoreDNSServiceIPIsStagedAsASentinel pins the staging render's split:
// the mirror registry and the pinned helper image resolve at render, but the
// kube-dns Service's clusterIP and the Corefile's zone are per-cluster inputs
// that stay as the __STRATUM_DNS_IP__ and __STRATUM_CLUSTER_DOMAIN__
// sentinels — every control plane stages the same bytes, and the deployer
// fills both at apply time from the cluster-inputs record the staging writes
// beside the prune inventory.
func TestCoreDNSServiceIPIsStagedAsASentinel(t *testing.T) {
	m := RenderCoreDNS()
	if got := bytes.Count(m, []byte(DNSAddressSentinel)); got != 1 {
		t.Errorf("the staged render carries the DNS IP sentinel %d times, want exactly once, as the Service's clusterIP", got)
	}
	if !bytes.Contains(m, []byte(ClusterDomainSentinel)) {
		t.Error("the staged render does not carry the cluster-domain sentinel, as the Corefile's zone")
	}
	// The mirror, version and helper-image pins still fill at render.
	for _, filled := range []string{registrySentinel, busyboxImageSentinel, "__STRATUM_COREDNS_VERSION__"} {
		if bytes.Contains(m, []byte(filled)) {
			t.Errorf("the staged render still carries %s; the registry, version and busybox sentinels fill at render", filled)
		}
	}
	found := false
	for _, o := range decodeAll(t, m) {
		if o.GetKind() == "Service" && o.GetName() == "kube-dns" {
			found = true
			if got, _, _ := unstructured.NestedString(o.Object, "spec", "clusterIP"); got != DNSAddressSentinel {
				t.Errorf("kube-dns clusterIP = %q, want the %s sentinel the deployer fills", got, DNSAddressSentinel)
			}
		}
	}
	if !found {
		t.Fatal("no kube-dns Service in the CoreDNS stack")
	}
	if bytes.Count(corednsYAML, []byte(DNSAddressSentinel)) != 1 {
		t.Error("the template should carry the DNS IP sentinel exactly once, as the Service's clusterIP")
	}
}
