package cluster

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestDashboardHint(t *testing.T) {
	const tokenCmd = "dashboard sign-in token: sudo /usr/local/bin/stratum kubectl -n stratum-dashboard get secret " +
		"stratum-dashboard-admin-token -o jsonpath='{.data.token}' | base64 -d; echo"
	for _, tc := range []struct {
		name   string
		yaml   string
		nodeIP string
		want   []string
	}{
		{name: "default: this node's address on the NodePort, and the admin token",
			yaml: "spec: {}\n", nodeIP: "10.0.0.5",
			want: []string{"dashboard: https://10.0.0.5:30443 (any node's address works)", tokenCmd}},
		{name: "IPv6 node address is bracketed",
			yaml: "spec: {}\n", nodeIP: "fd00::5",
			want: []string{"dashboard: https://[fd00::5]:30443 (any node's address works)", tokenCmd}},
		{name: "a custom node port",
			yaml: "spec:\n  dashboard:\n    nodePort: 31000\n", nodeIP: "10.0.0.5",
			want: []string{"dashboard: https://10.0.0.5:31000 (any node's address works)", tokenCmd}},
		{name: "publicURL wins",
			yaml: "spec:\n  dashboard:\n    publicURL: https://dash.example.com\n", nodeIP: "10.0.0.5",
			want: []string{"dashboard: https://dash.example.com", tokenCmd}},
		{name: "disabled: nothing",
			yaml: "spec:\n  dashboard:\n    enabled: false\n", nodeIP: "10.0.0.5"},
		{name: "operator off: nothing",
			yaml: "spec:\n  operator: false\n", nodeIP: "10.0.0.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dashboardHint(loadClusterConfig(t, tc.yaml), tc.nodeIP); !slices.Equal(got, tc.want) {
				t.Errorf("dashboardHint =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestDashboardHintClusterIPOnly(t *testing.T) {
	got := dashboardHint(loadClusterConfig(t, "spec:\n  dashboard:\n    nodePort: 0\n"), "10.0.0.5")
	if len(got) != 2 || !strings.Contains(got[0], "`sudo /usr/local/bin/stratum kubectl -n stratum-dashboard port-forward svc/stratum-dashboard 8443:443`") || strings.Contains(got[0], "10.0.0.5") {
		t.Errorf("dashboardHint = %q, want a sudo port-forward hint and no node address", got)
	}
}

func TestDashboardHintWithoutTokenSecrets(t *testing.T) {
	got := dashboardHint(loadClusterConfig(t, "spec:\n  dashboard:\n    tokenSecrets: false\n"), "10.0.0.5")
	if len(got) != 2 || strings.Contains(got[1], "stratum-dashboard-admin-token") || !strings.Contains(got[1], "create token") {
		t.Errorf("dashboardHint = %q, want a create-token hint, not the stored Secret", got)
	}
}

// TestNodeRegisteredTaskPrintsTheHintOnceRegistered pins that the hint is
// printed once, only after the node registers and is labelled.
func TestNodeRegisteredTaskPrintsTheHintOnceRegistered(t *testing.T) {
	for _, registers := range []bool{true, false} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		origErr := os.Stderr
		os.Stderr = w
		const budget = 100 * time.Millisecond
		labels, _, _ := stubNodeTask(t, 10*time.Millisecond, budget, "cp1",
			func(context.Context, string) bool { return registers })
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			g := newRunGroup(ctx)
			g.add("node registration", func(ctx context.Context) {
				nodeRegisteredTask(ctx, "cp1", []string{"dashboard: https://10.0.0.5:30443"})
			})
			time.Sleep(3 * budget)
			synctest.Wait()
			cancel()
			g.wait()
		})
		os.Stderr = origErr
		w.Close()
		out, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if registers {
			want = 1
		}
		if n := strings.Count(string(out), "stratum: dashboard: https://10.0.0.5:30443\n"); n != want || labels() != int32(want) {
			t.Errorf("registers=%v: hint printed %d times with %d labels, want %d: %s", registers, n, labels(), want, out)
		}
		if registers && strings.Index(string(out), "dashboard:") < strings.Index(string(out), "labeling it control-plane") {
			t.Errorf("hint printed before the node was labelled: %s", out)
		}
	}
}
