package rollout

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake answers each kubectl call by its verb and object ("get ds/cilium",
// "restart deploy/coredns", "status ds/cilium"), and records them.
type fake struct {
	mu     sync.Mutex
	calls  []string
	absent []string // probes `get` finds nothing for
	fail   []string // keys that exit non-zero
}

func (f *fake) kubectl(_ context.Context, _ time.Duration, _ bool, args ...string) ([]byte, error) {
	key := ""
	for _, verb := range []string{"get", "restart", "status"} {
		if i := slices.Index(args, verb); i >= 0 {
			key = verb + " " + args[i+1]
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if slices.Contains(f.fail, key) {
		return []byte("boom"), errors.New("exit status 1")
	}
	if strings.HasPrefix(key, "get ") && slices.Contains(f.absent, key[4:]) {
		return nil, nil
	}
	return []byte(key), nil
}

func (f *fake) roller() Roller {
	return Roller{Kubectl: f.kubectl, Hint: func(a string) string { return "kubectl " + a }, CallTimeout: time.Second, Wait: time.Second}
}

func TestRestart(t *testing.T) {
	tests := []struct {
		name        string
		absent      []string
		fail        []string
		wantCalls   []string
		wantPresent [4]bool
		wantErr     [4]string // substring of Err, "" for none
		wantWarn    [4]string
	}{
		{
			name: "all present",
			wantCalls: []string{"get deploy/stratum-dashboard", "restart deploy/stratum-dashboard", "status deploy/stratum-dashboard", "get ds/cilium", "get deploy/coredns", "get deploy/local-path-provisioner",
				"restart ds/cilium", "restart deploy/cilium-operator", "restart deploy/coredns", "restart deploy/local-path-provisioner",
				"status ds/cilium", "status deploy/cilium-operator", "status deploy/coredns", "status deploy/local-path-provisioner"},
			wantPresent: [4]bool{true, true, true, true},
		},
		{
			name:        "absent workload is skipped, not touched",
			absent:      []string{"deploy/local-path-provisioner"},
			wantCalls:   []string{"get deploy/stratum-dashboard", "restart deploy/stratum-dashboard", "status deploy/stratum-dashboard", "get ds/cilium", "get deploy/coredns", "get deploy/local-path-provisioner", "restart ds/cilium", "restart deploy/cilium-operator", "restart deploy/coredns", "status ds/cilium", "status deploy/cilium-operator", "status deploy/coredns"},
			wantPresent: [4]bool{true, true, false, true},
		},
		{
			name: "failed restart does not skip its sibling, and is not waited on",
			fail: []string{"restart ds/cilium"},
			wantCalls: []string{"get deploy/stratum-dashboard", "restart deploy/stratum-dashboard", "status deploy/stratum-dashboard", "get ds/cilium", "get deploy/coredns", "get deploy/local-path-provisioner",
				"restart ds/cilium", "restart deploy/cilium-operator", "restart deploy/coredns", "restart deploy/local-path-provisioner",
				"status deploy/cilium-operator", "status deploy/coredns", "status deploy/local-path-provisioner"},
			wantPresent: [4]bool{true, true, true, true},
			wantErr:     [4]string{"restart ds/cilium", "", "", ""},
		},
		{
			name: "unfinished rollout is a warning, not an error",
			fail: []string{"status deploy/coredns"},
			wantCalls: []string{"get deploy/stratum-dashboard", "restart deploy/stratum-dashboard", "status deploy/stratum-dashboard", "get ds/cilium", "get deploy/coredns", "get deploy/local-path-provisioner",
				"restart ds/cilium", "restart deploy/cilium-operator", "restart deploy/coredns", "restart deploy/local-path-provisioner",
				"status ds/cilium", "status deploy/cilium-operator", "status deploy/coredns", "status deploy/local-path-provisioner"},
			wantPresent: [4]bool{true, true, true, true},
			wantWarn:    [4]string{"", "deploy/coredns did not finish rolling out in 1s (on the seed: `kubectl -n kube-system rollout status deploy/coredns`)", "", ""},
		},
		{
			name:        "failed discovery is present and an error",
			fail:        []string{"get deploy/coredns"},
			wantCalls:   []string{"get deploy/stratum-dashboard", "restart deploy/stratum-dashboard", "status deploy/stratum-dashboard", "get ds/cilium", "get deploy/coredns", "get deploy/local-path-provisioner", "restart ds/cilium", "restart deploy/cilium-operator", "restart deploy/local-path-provisioner", "status ds/cilium", "status deploy/cilium-operator", "status deploy/local-path-provisioner"},
			wantPresent: [4]bool{true, true, true, true},
			wantErr:     [4]string{"", "look for deploy/coredns", "", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fake{absent: tt.absent, fail: tt.fail}
			rs := f.roller().Restart()
			if len(rs) != len(Workloads) {
				t.Fatalf("%d results, want %d", len(rs), len(Workloads))
			}
			for i, r := range rs {
				if r.Name != Workloads[i].Name || r.Present != tt.wantPresent[i] {
					t.Errorf("%s: name=%q present=%v, want %q %v", Workloads[i].Name, r.Name, r.Present, Workloads[i].Name, tt.wantPresent[i])
				}
				for _, c := range []struct {
					what string
					got  error
					want string
				}{{"err", r.Err, tt.wantErr[i]}, {"warn", r.Warn, tt.wantWarn[i]}} {
					if (c.got == nil) != (c.want == "") || (c.got != nil && !strings.Contains(c.got.Error(), c.want)) {
						t.Errorf("%s %s = %v, want containing %q", Workloads[i].Name, c.what, c.got, c.want)
					}
				}
			}
			slices.Sort(f.calls)
			slices.Sort(tt.wantCalls)
			if !slices.Equal(f.calls, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", f.calls, tt.wantCalls)
			}
		})
	}
}

func TestRestartCommands(t *testing.T) {
	want := []string{
		"kubectl -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns",
		"kubectl -n local-path-storage rollout restart deploy/local-path-provisioner",
		"kubectl -n stratum-dashboard rollout restart deploy/stratum-dashboard",
	}
	if got := RestartCommands("kubectl"); !slices.Equal(got, want) {
		t.Errorf("RestartCommands = %q, want %q", got, want)
	}
}

func TestRestartOfRollsTheGivenWorkloads(t *testing.T) {
	f := &fake{}
	r := Roller{Kubectl: f.kubectl, Hint: func(a string) string { return a }, CallTimeout: time.Second, Wait: time.Second}
	got := r.RestartOf([]Workload{{"kong/ingress", "kong", "deployment/ingress", []string{"deployment/ingress"}}})
	if len(got) != 1 || !got[0].Present || got[0].Err != nil || got[0].Warn != nil {
		t.Fatalf("results = %+v", got)
	}
	if want := []string{"get deployment/ingress", "restart deployment/ingress", "status deployment/ingress"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestOwned(t *testing.T) {
	for _, tc := range []struct {
		ns, kind, name string
		want           bool
	}{
		{"kube-system", "DaemonSet", "cilium", true},
		{"kube-system", "Deployment", "cilium-operator", true},
		{"kube-system", "Deployment", "coredns", true},
		{"local-path-storage", "Deployment", "local-path-provisioner", true},
		{"stratum-dashboard", "Deployment", "stratum-dashboard", true},
		{"kube-system", "DaemonSet", "cilium-envoy", false},
		{"kube-system", "StatefulSet", "coredns", false},
		{"default", "Deployment", "coredns", false},
		{"kube-system", "Job", "coredns", false},
	} {
		if got := Owned(tc.ns, tc.kind, tc.name); got != tc.want {
			t.Errorf("Owned(%q, %q, %q) = %v, want %v", tc.ns, tc.kind, tc.name, got, tc.want)
		}
	}
}
