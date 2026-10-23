package certs

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestNodeJobNamesAreValidAndDistinctOnLongNodeNames(t *testing.T) {
	nodes := []string{
		"ske-39099666359-0-gcp-rocky-9-amd64-controller-0",
		"ske-39099666359-0-gcp-rocky-9-amd64-controller-1",
		"ske-39099666359-0-gcp-rocky-9-amd64-controller-2",
		"ske-39099666359-0-gcp-rocky-9-amd64-worker-0",
		"ske-39099666359-0-gcp-rocky-9-amd64-worker-1",
	}
	builders := map[string]func(node string) string{
		"caJobName trust-new":      func(n string) string { return caJobName(n, PhaseTrustNew) },
		"caJobName rollback check": func(n string) string { return caJobName(n, rollbackCheck) },
		"kubeletCAJobName":         func(n string) string { return kubeletCAJobName(n, "trust-both") },
	}
	for what, build := range builders {
		seen := map[string]string{}
		for _, n := range nodes {
			got := build(n)
			if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 || len(got) > 63 {
				t.Errorf("%s(%s) = %q is not a valid Job name: %v", what, n, got, errs)
			}
			if prev, ok := seen[got]; ok {
				t.Errorf("%s gives %s and %s the same Job name %q", what, prev, n, got)
			}
			seen[got] = n
		}
	}
	if got, want := caJobName("cp1", PhaseTrustNew), "stratum-carotate-trust-new-cp1"; got != want {
		t.Errorf("a short name changed: %q, want %q", got, want)
	}
}
