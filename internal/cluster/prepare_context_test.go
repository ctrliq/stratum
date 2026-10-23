package cluster

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func TestPrepareControllerContextStopsBeforeLaterMutations(t *testing.T) {
	boom := errors.New("host preparation failed")
	for _, tc := range []struct {
		name, phase string
		effectError error
	}{
		{"hostname", "hostname", nil},
		{"directories", "directories", nil},
		{"prerequisites", "prerequisites", nil},
		{"certificates", "certificates", nil},
		{"runtime configs", "runtime configs", nil},
		{"in-flight error", "directories", boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assemblyFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ran []string
			step := func(name string) error {
				ran = append(ran, name)
				if name == tc.phase {
					cancel()
					return tc.effectError
				}
				return nil
			}
			hostHostname = func() (string, error) { return "cp1", step("hostname") }
			oldDirs, oldPrereqs, oldTrust, oldAssembly := hostDirs, hostPrereqs, hostTrustPodCIDRs, assembleNode
			t.Cleanup(func() {
				hostDirs, hostPrereqs, hostTrustPodCIDRs, assembleNode = oldDirs, oldPrereqs, oldTrust, oldAssembly
			})
			hostDirs = func() error { return step("directories") }
			hostPrereqs = func(host.PrereqOptions) error { return step("prerequisites") }
			hostTrustPodCIDRs = func(string) { _ = step("firewall") }
			assembleNode = func(assemblyCtx context.Context, a ha.NodeAssembly, req ha.AssembleRequest) (host.NodeIPChoice, error) {
				if assemblyCtx != ctx {
					t.Fatal("controller preparation did not pass its context to assembly")
				}
				// Simulate the atomic host writes while retaining the real shared
				// assembly and controller's network/address/state preparation.
				a.IssueCerts = func(string, string, *pki.APISANs) error { return step("certificates") }
				a.WriteKubeconfigs = func(string, string, bool) error { return step("kubeconfigs") }
				a.WriteRuntimeConfigs = func(*nodestate.NodeState) error { return step("runtime configs") }
				return ha.AssembleNodeContext(assemblyCtx, a, req)
			}
			_, err := PrepareControllerContext(ctx, &ClusterConfig{}, "192.0.2.10", nil)
			wantErr := error(context.Canceled)
			if tc.effectError != nil {
				wantErr = tc.effectError
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("prepare error = %v, want %v", err, wantErr)
			}
			order := []string{"hostname", "directories", "prerequisites", "firewall", "certificates", "kubeconfigs", "runtime configs"}
			want := order[:slices.Index(order, tc.phase)+1]
			if !slices.Equal(ran, want) {
				t.Fatalf("mutations after cancellation: got %v, want %v", ran, want)
			}
			if _, err := LoadState(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancelled preparation persisted node state: %v", err)
			}
			if slices.Index(order, tc.phase) <= slices.Index(order, "prerequisites") {
				if _, err := os.Stat(version.NetConfigPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("network config written after an earlier step cancelled: %v", err)
				}
			}
		})
	}
}
