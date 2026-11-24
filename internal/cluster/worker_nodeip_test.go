package cluster

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
)

// The stub only detects; the resolver weighs $NODE_IP before detection is reached.
func stubNodeIPDetection(t *testing.T, addrs string) *[]bool {
	t.Helper()
	var calls []bool
	old := hostDetectNodeIPs
	hostDetectNodeIPs = func(dualStack bool) (string, error) {
		calls = append(calls, dualStack)
		return addrs, nil
	}
	t.Cleanup(func() { hostDetectNodeIPs = old })
	return &calls
}

// One pipe for stdout and stderr pins what the operator is told, not which stream.
func captureWorkerOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		r.Close()
		read <- string(b)
	}()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	func() {
		defer func() {
			os.Stdout, os.Stderr = oldOut, oldErr
			w.Close()
		}()
		fn()
	}()
	return <-read
}

// A bare restart must keep the moved address, or the next start reverts the kubelet.
func TestWorkerRestartMovesToTheNodeIPFlagAndKeepsIt(t *testing.T) {
	registerCISForTest(t)
	redirectJoinDirs(t)
	stubJoinHost(t, "w-8", "", nil)
	recordTrust(t)
	detected := stubNodeIPDetection(t, "10.89.0.1")
	t.Setenv("NODE_IP", "")
	saved := nodestate.NodeState{
		Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4",
		Server: "https://10.80.0.1:6443", Profile: "cis", CIS: true, ServiceCIDR: "10.83.0.0/16", PodCIDR: "10.82.0.0/16",
		ClusterDomain: "w8.example.test",
	}
	if err := nodestate.SaveState(&saved); err != nil {
		t.Fatal(err)
	}
	const moved = "10.81.0.9"
	want := saved
	want.NodeIP, want.KubeletNodeIP = moved, moved

	var st *nodestate.NodeState
	var err error
	out := captureWorkerOutput(t, func() { st, err = PrepareWorker(context.Background(), WorkerOptions{NodeIP: moved}) })
	if err != nil {
		t.Fatalf("restart with --node-ip %s: PrepareWorker = %v", moved, err)
	}
	if st.NodeIP != moved || st.KubeletNodeIP != moved {
		t.Errorf("the flagged restart starts the worker on node IP %q / kubelet IPs %q, want the --node-ip %s in both",
			st.NodeIP, st.KubeletNodeIP, moved)
	}
	if after := mustLoadState(t); !reflect.DeepEqual(*after, want) {
		t.Errorf("node.yaml after the flagged restart = %+v, want %+v: the new address saved and every other field kept",
			*after, want)
	}
	if !strings.Contains(out, moved) || !strings.Contains(out, "--node-ip") {
		t.Errorf("the flagged restart printed %q, want the new address %s and its source, --node-ip", out, moved)
	}

	st, err = PrepareWorker(context.Background(), WorkerOptions{})
	if err != nil {
		t.Fatalf("restart without --node-ip: PrepareWorker = %v", err)
	}
	if st.NodeIP != moved || st.KubeletNodeIP != moved {
		t.Errorf("the following bare restart starts the worker on node IP %q / kubelet IPs %q, want the %s the operator moved it to",
			st.NodeIP, st.KubeletNodeIP, moved)
	}
	if after := mustLoadState(t); !reflect.DeepEqual(*after, want) {
		t.Errorf("node.yaml after the following bare restart = %+v, want %+v: a reboot after it must still find the new address",
			*after, want)
	}
	if len(*detected) != 0 {
		t.Errorf("address detection ran %d time(s), want none: each restart had --node-ip or a saved address", len(*detected))
	}
}

// The saved address outranks $NODE_IP, which says only what the environment says now.
func TestWorkerRestartKeepsTheSavedAddressOverTheNodeIPEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved nodestate.NodeState
	}{
		{
			name: "saved kubelet addresses",
			saved: nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4,fd80::4",
				DualStack: true, Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"},
		},
		{
			name:  "older state with only the primary address",
			saved: nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", Server: "https://10.80.0.1:6443"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "", nil)
			recordTrust(t)
			detected := stubNodeIPDetection(t, "10.89.0.1")
			t.Setenv("NODE_IP", "10.74.0.9")
			saved := tc.saved
			if err := nodestate.SaveState(&saved); err != nil {
				t.Fatal(err)
			}

			st, err := PrepareWorker(context.Background(), WorkerOptions{})
			if err != nil {
				t.Fatalf("restart without --node-ip: PrepareWorker = %v", err)
			}
			if st.NodeIP != tc.saved.NodeIP || st.KubeletNodeIP != tc.saved.KubeletNodeIP {
				t.Errorf("the restart starts the worker on node IP %q / kubelet IPs %q, want the saved %q / %q, not $NODE_IP's 10.74.0.9",
					st.NodeIP, st.KubeletNodeIP, tc.saved.NodeIP, tc.saved.KubeletNodeIP)
			}
			if after := mustLoadState(t); !reflect.DeepEqual(*after, tc.saved) {
				t.Errorf("node.yaml after the restart = %+v, want it unchanged at %+v", *after, tc.saved)
			}
			if len(*detected) != 0 {
				t.Errorf("address detection ran %d time(s), want none: the state records an address", len(*detected))
			}
		})
	}
}

func TestWorkerRestartPinsTheNodeIPFlagMatchingLegacyState(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "w-8", "", nil)
	recordTrust(t)
	stubNodeIPDetection(t, "10.89.0.1")
	t.Setenv("NODE_IP", "10.74.0.9")
	saved := nodestate.NodeState{
		Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4",
		Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16",
	}
	if err := nodestate.SaveState(&saved); err != nil {
		t.Fatal(err)
	}
	want := saved
	want.KubeletNodeIP = saved.NodeIP

	// Older state records only the primary address; the kubelet still needs it pinned.
	for _, opts := range []WorkerOptions{{NodeIP: saved.NodeIP}, {}} {
		st, err := PrepareWorker(context.Background(), opts)
		if err != nil {
			t.Fatalf("restart with --node-ip %q: %v", opts.NodeIP, err)
		}
		for _, got := range []*nodestate.NodeState{st, mustLoadState(t)} {
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("restart with --node-ip %q: state = %+v, want %+v", opts.NodeIP, *got, want)
			}
			kubelet := namedProcess(t, Processes(got, false), "kubelet")
			if args := hostArgv(kubelet); !slices.Contains(args, "--node-ip="+saved.NodeIP) {
				t.Errorf("restart with --node-ip %q: kubelet argv = %q, want --node-ip=%s", opts.NodeIP, args, saved.NodeIP)
			}
		}
	}
}

func TestWorkerRestartRepeatsTheSingleIPFromADualStackJoin(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "w-8", "", []byte("discovered CA"))
	recordTrust(t)
	t.Setenv("NODE_IP", "")
	const nodeIP = "10.80.0.4"
	joined, err := PrepareWorker(context.Background(), WorkerOptions{
		Server: "https://10.80.0.1:6443", Token: "join-token-8", CAHash: "sha256:88",
		NodeIP: nodeIP, DualStack: true,
	})
	if err != nil {
		t.Fatalf("join with --dual-stack and one IPv4 address: %v", err)
	}
	before := mustRead(t, nodestate.StatePath())

	st, err := PrepareWorker(context.Background(), WorkerOptions{NodeIP: nodeIP})
	if err != nil {
		t.Fatalf("restart repeating the join's --node-ip: %v", err)
	}
	if !reflect.DeepEqual(*st, *joined) {
		t.Errorf("restart state = %+v, want the joined state %+v", *st, *joined)
	}
	if after := mustRead(t, nodestate.StatePath()); !bytes.Equal(after, before) {
		t.Errorf("restart rewrote node.yaml:\n%s\nwant it unchanged:\n%s", after, before)
	}
}

// A restart with no saved address resolves and saves one, so later restarts keep it.
func TestWorkerRestartWithoutASavedAddressDetectsAndSavesOne(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "w-8", "", nil)
	recordTrust(t)
	detected := stubNodeIPDetection(t, "10.89.0.1")
	t.Setenv("NODE_IP", "")
	saved := nodestate.NodeState{Role: "worker", Hostname: "w-8", Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	if err := nodestate.SaveState(&saved); err != nil {
		t.Fatal(err)
	}
	want := saved
	want.NodeIP, want.KubeletNodeIP = "10.89.0.1", "10.89.0.1"

	var st *nodestate.NodeState
	var err error
	out := captureWorkerOutput(t, func() { st, err = PrepareWorker(context.Background(), WorkerOptions{}) })
	if err != nil {
		t.Fatalf("restart without a saved address: PrepareWorker = %v", err)
	}
	if st.NodeIP != "10.89.0.1" || st.KubeletNodeIP != "10.89.0.1" {
		t.Errorf("the restart starts the worker on node IP %q / kubelet IPs %q, want the detected 10.89.0.1 in both",
			st.NodeIP, st.KubeletNodeIP)
	}
	if after := mustLoadState(t); !reflect.DeepEqual(*after, want) {
		t.Errorf("node.yaml after the restart = %+v, want %+v: the detected address saved and every other field kept",
			*after, want)
	}
	if !slices.Equal(*detected, []bool{false}) {
		t.Errorf("address detection was called with dual-stack %v, want once with the saved single-stack false", *detected)
	}
	if !strings.Contains(out, "10.89.0.1") || !strings.Contains(out, "autodetected") {
		t.Errorf("the restart printed %q, want the detected address 10.89.0.1 and its source", out)
	}
}

// A restart reads no spec.network.nodeIP, so the refusal must not name it.
func TestWorkerRestartDetectionFailureNamesOnlySourcesARestartReads(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dualStack bool
	}{
		{name: "a single-stack worker"},
		{name: "a dual-stack worker", dualStack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "", nil)
			recordTrust(t)
			t.Setenv("NODE_IP", "")
			undetected := errors.New("no default route")
			old := hostDetectNodeIPs
			hostDetectNodeIPs = func(bool) (string, error) { return "", undetected }
			t.Cleanup(func() { hostDetectNodeIPs = old })
			saved := nodestate.NodeState{Role: "worker", Hostname: "w-8", DualStack: tc.dualStack,
				Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
			if err := nodestate.SaveState(&saved); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(nodestate.StatePath())
			if err != nil {
				t.Fatal(err)
			}

			st, err := PrepareWorker(context.Background(), WorkerOptions{})
			if err == nil {
				t.Fatalf("restart with no address to keep and a failing detection: PrepareWorker = %+v, nil; want a refusal", *st)
			}
			if !errors.Is(err, undetected) {
				t.Errorf("the refusal %q does not carry the detection failure", err)
			}
			for _, want := range []string{"--node-ip", "$NODE_IP"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s, a source the restart reads", err, want)
				}
			}
			if strings.Contains(err.Error(), "spec.network.nodeIP") {
				t.Errorf("the refusal %q names spec.network.nodeIP, which a restart never reads", err)
			}
			if after, err := os.ReadFile(nodestate.StatePath()); err != nil {
				t.Fatal(err)
			} else if !bytes.Equal(after, before) {
				t.Errorf("the refused restart rewrote node.yaml:\n%s\nwant it unchanged:\n%s", after, before)
			}
		})
	}
}

// The restart judges the stack by the addresses the worker registered.
func TestWorkerRestartTakesADualStackNodeIPFlag(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dualStack bool
	}{
		{name: "a worker joined with --dual-stack", dualStack: true},
		{name: "a worker joined with a pair but without --dual-stack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "", nil)
			recordTrust(t)
			stubNodeIPDetection(t, "10.89.0.1,fd89::1")
			t.Setenv("NODE_IP", "")
			if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4",
				KubeletNodeIP: "10.80.0.4,fd80::4", DualStack: tc.dualStack, Server: "https://10.80.0.1:6443"}); err != nil {
				t.Fatal(err)
			}

			st, err := PrepareWorker(context.Background(), WorkerOptions{NodeIP: "10.81.0.9,fd81::9"})
			if err != nil {
				t.Fatalf("restart with --node-ip 10.81.0.9,fd81::9: PrepareWorker = %v", err)
			}
			for _, got := range []struct {
				what string
				st   *nodestate.NodeState
			}{
				{"the state the restart returns", st},
				{"the state the restart saved", mustLoadState(t)},
			} {
				if got.st.NodeIP != "10.81.0.9" || got.st.KubeletNodeIP != "10.81.0.9,fd81::9" {
					t.Errorf("%s has node IP %q / kubelet IPs %q, want 10.81.0.9 / 10.81.0.9,fd81::9",
						got.what, got.st.NodeIP, got.st.KubeletNodeIP)
				}
				kubelet := namedProcess(t, Processes(got.st, false), "kubelet")
				if args := hostArgv(kubelet); !slices.Contains(args, "--node-ip=10.81.0.9,fd81::9") {
					t.Errorf("the kubelet rendered from %s runs with %q, want --node-ip=10.81.0.9,fd81::9", got.what, args)
				}
			}
		})
	}
}

// An unregistrable --node-ip must be refused before it is saved and reused at every boot.
func TestWorkerRestartRefusesANodeIPItCannotRegister(t *testing.T) {
	single := nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4",
		Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	dual := nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4,fd80::4",
		DualStack: true, Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	// A worker joined with a pair but without --dual-stack registered both families.
	pair := dual
	pair.DualStack = false
	unaddressed := nodestate.NodeState{Role: "worker", Hostname: "w-8", Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	for _, tc := range []struct {
		name   string
		saved  nodestate.NodeState
		nodeIP string
		detect string
		want   []string
	}{
		{
			name: "an address that does not parse", saved: single, nodeIP: "10.80.0.300",
			want: []string{`"10.80.0.300"`, "--node-ip", "not an IP address", "10.80.0.4"},
		},
		{
			name: "a dual-stack worker given only its IPv4", saved: dual, nodeIP: "10.80.0.4",
			want: []string{`"10.80.0.4"`, "--node-ip", `"v4,v6"`, "10.80.0.4,fd80::4"},
		},
		{
			name: "a single-stack worker given a pair", saved: single, nodeIP: "10.81.0.9,fd81::9",
			want: []string{`"10.81.0.9,fd81::9"`, "--node-ip", "single-stack", "10.80.0.4"},
		},
		{
			name: "a dual-stack worker given its pair IPv6 first", saved: dual, nodeIP: "fd81::9,10.81.0.9",
			want: []string{`"fd81::9,10.81.0.9"`, "--node-ip", "IPv4 first", "10.80.0.4,fd80::4"},
		},
		{
			name: "a single-stack worker given an IPv6", saved: single, nodeIP: "fd81::9",
			want: []string{`"fd81::9"`, "--node-ip", "IPv4 address", "10.80.0.4"},
		},
		{
			name: "a single-stack worker given an IPv4-mapped IPv6", saved: single, nodeIP: "::ffff:10.81.0.9",
			want: []string{`"::ffff:10.81.0.9"`, "--node-ip", "IPv4 address", "10.80.0.4"},
		},
		{
			name: "a single-stack worker given a loopback address", saved: single, nodeIP: "127.0.0.1",
			want: []string{`"127.0.0.1"`, "--node-ip", "loopback", "10.80.0.4"},
		},
		{
			name: "a worker that registered a pair given only its IPv4", saved: pair, nodeIP: "10.81.0.9",
			want: []string{`"10.81.0.9"`, "--node-ip", `"v4,v6"`, "10.80.0.4,fd80::4"},
		},
		{
			name: "a dual-stack worker given a pair whose IPv6 does not parse", saved: dual, nodeIP: "10.81.0.9,fd81::zz",
			want: []string{`"10.81.0.9,fd81::zz"`, "--node-ip", `"fd81::zz" is not an IP address`, "10.80.0.4,fd80::4"},
		},
		{
			name: "a worker with no saved address that detects a link-local address", saved: unaddressed, detect: "169.254.1.1",
			want: []string{`"169.254.1.1"`, "autodetected", "link-local", "(none)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "", nil)
			recordTrust(t)
			stubNodeIPDetection(t, cmp.Or(tc.detect, "10.89.0.1"))
			t.Setenv("NODE_IP", "")
			saved := tc.saved
			if err := nodestate.SaveState(&saved); err != nil {
				t.Fatal(err)
			}

			st, err := PrepareWorker(context.Background(), WorkerOptions{NodeIP: tc.nodeIP})
			if err == nil {
				t.Fatalf("restart with --node-ip %q: PrepareWorker = %+v, nil; want a refusal", tc.nodeIP, *st)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
			if after := mustLoadState(t); !reflect.DeepEqual(*after, tc.saved) {
				t.Errorf("node.yaml after the refused restart = %+v, want it unchanged at %+v", *after, tc.saved)
			}
		})
	}
}

// A refused restart must not save one setting and then refuse another.
func TestWorkerRestartRefusesBeforeItChangesAnything(t *testing.T) {
	optOut := true
	legacy := nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4",
		Server: "https://10.80.0.1:6443"}
	recorded := legacy
	recorded.PodCIDR = "10.82.0.0/16"
	damagedSingle := legacy
	damagedSingle.NodeIP, damagedSingle.KubeletNodeIP = "10.80.0.300", "10.80.0.300"
	damagedDual := legacy
	damagedDual.KubeletNodeIP, damagedDual.DualStack = "10.80.0.4,fd80::zz", true
	unaddressed := nodestate.NodeState{Role: "worker", Hostname: "w-8", Server: "https://10.80.0.1:6443"}
	for _, tc := range []struct {
		name  string
		saved nodestate.NodeState
		opts  WorkerOptions
		env   string
		want  string
	}{
		{
			name: "a --node-ip that does not parse", saved: legacy,
			opts: WorkerOptions{NodeIP: "10.80.0.300", PodCIDR: "10.82.0.0/16", SELinuxOptOut: &optOut},
			want: "cannot be this worker's node IP",
		},
		{
			name: "a malformed --node-ip matching damaged single-stack state", saved: damagedSingle,
			opts: WorkerOptions{NodeIP: damagedSingle.KubeletNodeIP, PodCIDR: "10.82.0.0/16", SELinuxOptOut: &optOut},
			want: `"10.80.0.300" is not an IP address`,
		},
		{
			name: "a malformed --node-ip matching damaged dual-stack state", saved: damagedDual,
			opts: WorkerOptions{NodeIP: damagedDual.KubeletNodeIP, PodCIDR: "10.82.0.0/16", SELinuxOptOut: &optOut},
			want: `"fd80::zz" is not an IP address`,
		},
		{
			name: "a $NODE_IP that does not parse, on a worker with no saved address", saved: unaddressed,
			opts: WorkerOptions{PodCIDR: "10.82.0.0/16", SELinuxOptOut: &optOut}, env: "not-an-ip",
			want: "cannot be this worker's node IP",
		},
		{
			name: "a --pod-cidr that does not parse", saved: legacy,
			opts: WorkerOptions{PodCIDR: "10.82.0.0/33", SELinuxOptOut: &optOut},
			want: "--pod-cidr",
		},
		{
			name: "a --pod-cidr that conflicts with the saved one", saved: recorded,
			opts: WorkerOptions{PodCIDR: "10.99.0.0/16", SELinuxOptOut: &optOut},
			want: "conflicts with this worker's saved pod CIDR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "", nil)
			trusted := recordTrust(t)
			stubNodeIPDetection(t, "10.89.0.1")
			t.Setenv("NODE_IP", tc.env)
			prepared := 0
			oldPrereqs := hostPrereqs
			hostPrereqs = func(host.PrereqOptions) error { prepared++; return nil }
			t.Cleanup(func() { hostPrereqs = oldPrereqs })
			saved := tc.saved
			if err := nodestate.SaveState(&saved); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(nodestate.StatePath())
			if err != nil {
				t.Fatal(err)
			}

			st, err := PrepareWorker(context.Background(), tc.opts)
			if err == nil {
				t.Fatalf("restart with %+v: PrepareWorker = %+v, nil; want a refusal naming %q", tc.opts, *st, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal %q does not name %q", err, tc.want)
			}
			if after, err := os.ReadFile(nodestate.StatePath()); err != nil {
				t.Fatal(err)
			} else if !bytes.Equal(after, before) {
				t.Errorf("the refused restart rewrote node.yaml:\n%s\nwant it unchanged:\n%s", after, before)
			}
			if prepared != 0 {
				t.Errorf("the refused restart prepared the host %d time(s), want none", prepared)
			}
			if len(*trusted) != 0 {
				t.Errorf("the refused restart trusted pod CIDRs %v, want none", *trusted)
			}
		})
	}
}

// The log line tells a worker on the wrong NIC's address or a stale $NODE_IP.
func TestWorkerReportsItsNodeIPAndWhereItCameFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved *nodestate.NodeState
		opts  WorkerOptions
		env   string
		want  string
	}{
		{
			name: "a join that takes $NODE_IP",
			opts: WorkerOptions{Server: "https://10.80.0.1:6443", Token: "join-token-8", CAHash: "sha256:88"},
			env:  "10.74.0.9", want: "stratum: node IP 10.74.0.9 ($NODE_IP)",
		},
		{
			name: "a join that takes spec.network.nodeIP",
			opts: WorkerOptions{Server: "https://10.80.0.1:6443", Token: "join-token-8", CAHash: "sha256:88", ConfigNodeIP: "10.72.0.9"},
			env:  "10.74.0.9", want: "stratum: node IP 10.72.0.9 (spec.network.nodeIP)",
		},
		{
			name: "a restart that keeps its saved address",
			saved: &nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4",
				Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"},
			env: "10.74.0.9", want: "stratum: node IP 10.80.0.4 (saved node state)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "10.89.0.1", []byte("discovered CA"))
			recordTrust(t)
			t.Setenv("NODE_IP", tc.env)
			if tc.saved != nil {
				if err := nodestate.SaveState(tc.saved); err != nil {
					t.Fatal(err)
				}
			}

			var err error
			out := captureWorkerOutput(t, func() { _, err = PrepareWorker(context.Background(), tc.opts) })
			if err != nil {
				t.Fatalf("PrepareWorker = %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the worker printed %q, want %q", out, tc.want)
			}
		})
	}
}

// An unreadable node.yaml needs repairing, not a new registration.
func TestWorkerRestartReportsAStateItCannotRead(t *testing.T) {
	redirectJoinDirs(t)
	rec := stubJoinHost(t, "w-8", "10.89.0.1", nil)
	recordTrust(t)
	if err := os.WriteFile(nodestate.StatePath(), []byte("role: [worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := PrepareWorker(context.Background(), WorkerOptions{})
	if err == nil {
		t.Fatalf("restart over an unreadable node.yaml: PrepareWorker = %+v, nil; want a refusal", *st)
	}
	if !strings.Contains(err.Error(), nodestate.StatePath()) {
		t.Errorf("the refusal %q does not name the state file %s", err, nodestate.StatePath())
	}
	if strings.Contains(err.Error(), "needs --server") {
		t.Errorf("the refusal %q asks for join credentials; the worker has joined, and its state file is the problem", err)
	}
	if rec.discover != nil {
		t.Errorf("the restart went on to a join (discover=%v)", rec.discover)
	}
}

// A join checks the address before it touches the host or the cluster.
func TestWorkerJoinRefusesANodeIPItCannotRegister(t *testing.T) {
	join := WorkerOptions{Server: "https://10.80.0.1:6443", Token: "join-token-8", CAHash: "sha256:88"}
	for _, tc := range []struct {
		name   string
		nodeIP string
		config string
		env    string
		detect string
		dual   bool
		want   []string
	}{
		{name: "a --node-ip that is not an IP address", nodeIP: "not-an-ip",
			want: []string{`"not-an-ip"`, "--node-ip", "not an IP address"}},
		{name: "a spec.network.nodeIP that is not an IP address", config: "10.80.0.300",
			want: []string{`"10.80.0.300"`, "spec.network.nodeIP", "not an IP address"}},
		{name: "a $NODE_IP pair missing its IPv6", env: "10.80.0.5,", dual: true,
			want: []string{`"10.80.0.5,"`, "$NODE_IP", "not an IP address"}},
		{name: "a lone IPv6 --node-ip", nodeIP: "fd80::5",
			want: []string{`"fd80::5"`, "--node-ip", "IPv4 address"}},
		{name: "a loopback --node-ip", nodeIP: "127.0.0.1",
			want: []string{`"127.0.0.1"`, "--node-ip", "loopback"}},
		{name: "a pair with its IPv6 first", nodeIP: "fd80::5,10.80.0.5", dual: true,
			want: []string{`"fd80::5,10.80.0.5"`, "--node-ip", "IPv4 first"}},
		{name: "a pair whose IPv6 does not parse", nodeIP: "10.80.0.5,fd80::zz", dual: true,
			want: []string{`"10.80.0.5,fd80::zz"`, "--node-ip", `"fd80::zz" is not an IP address`}},
		{name: "a detected link-local address", detect: "169.254.1.1",
			want: []string{`"169.254.1.1"`, "autodetected", "link-local"}},
		{name: "a pair without --dual-stack joins", nodeIP: "10.80.0.5,fd80::5"},
		{name: "one IPv4 with --dual-stack joins", nodeIP: "10.80.0.5", dual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pkiDir := redirectJoinDirs(t)
			rec := stubJoinHost(t, "w-8", cmp.Or(tc.detect, "10.89.0.1"), []byte("discovered CA"))
			trusted := recordTrust(t)
			t.Setenv("NODE_IP", tc.env)
			prepared, dirs := 0, 0
			oldPrereqs, oldDirs := hostPrereqs, hostDirs
			hostPrereqs = func(host.PrereqOptions) error { prepared++; return nil }
			hostDirs = func() error { dirs++; return nil }
			t.Cleanup(func() { hostPrereqs, hostDirs = oldPrereqs, oldDirs })
			opts := join
			opts.NodeIP, opts.ConfigNodeIP, opts.DualStack = tc.nodeIP, tc.config, tc.dual

			st, err := PrepareWorker(context.Background(), opts)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("PrepareWorker = %v, want the join to go ahead as it did before the check", err)
				}
				if want := cmp.Or(tc.nodeIP, tc.config, tc.env); st.KubeletNodeIP != want {
					t.Errorf("the join saved kubelet IPs %q, want %q", st.KubeletNodeIP, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("PrepareWorker = %+v, nil; want a refusal", *st)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
			if _, err := nodestate.LoadState(); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the refused join left a node.yaml (load: %v), want none", err)
			}
			if rec.discover != nil || dirs != 0 || prepared != 0 || len(*trusted) != 0 {
				t.Errorf("the refused join reached the host or the cluster: discover=%v dirs=%d prerequisites=%d trust=%v",
					rec.discover, dirs, prepared, *trusted)
			}
			if entries, _ := os.ReadDir(pkiDir); len(entries) != 0 {
				t.Errorf("the refused join wrote trust files %v", entries)
			}
		})
	}
}

// The shared precedence rule applies; a saved address is not a source on a join.
func TestWorkerJoinNodeIPPrecedence(t *testing.T) {
	const server = "https://10.80.0.1:6443"
	join := WorkerOptions{Server: server, Token: "join-token-8", CAHash: "sha256:88"}
	dualJoin := join
	dualJoin.DualStack = true
	flagged := join
	flagged.NodeIP = "10.73.0.9"
	configured := join
	configured.ConfigNodeIP = "10.72.0.9"
	both := flagged
	both.ConfigNodeIP = "10.72.0.9"
	earlier := &nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4", Server: server}

	for _, tc := range []struct {
		name       string
		saved      *nodestate.NodeState
		opts       WorkerOptions
		env        string
		detect     string
		want       string
		wantDetect []bool
	}{
		{
			name: "--node-ip outranks $NODE_IP and detection",
			opts: flagged, env: "10.74.0.9", detect: "10.89.0.1", want: "10.73.0.9",
		},
		{
			name: "--node-ip outranks spec.network.nodeIP",
			opts: both, env: "10.74.0.9", detect: "10.89.0.1", want: "10.73.0.9",
		},
		{
			name: "spec.network.nodeIP outranks $NODE_IP and detection",
			opts: configured, env: "10.74.0.9", detect: "10.89.0.1", want: "10.72.0.9",
		},
		{
			name: "$NODE_IP outranks detection",
			opts: dualJoin, env: "10.74.0.9,fd74::9", detect: "10.89.0.1,fd89::1", want: "10.74.0.9,fd74::9",
		},
		{
			name: "detection when nothing is explicit",
			opts: join, detect: "10.89.0.1", want: "10.89.0.1", wantDetect: []bool{false},
		},
		{
			name: "dual-stack detection when nothing is explicit",
			opts: dualJoin, detect: "10.89.0.1,fd89::1", want: "10.89.0.1,fd89::1", wantDetect: []bool{true},
		},
		{
			name:  "an earlier join's saved address is not a source",
			saved: earlier, opts: join, detect: "10.89.0.1", want: "10.89.0.1", wantDetect: []bool{false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "w-8", "", []byte("discovered CA"))
			recordTrust(t)
			detected := stubNodeIPDetection(t, tc.detect)
			t.Setenv("NODE_IP", tc.env)
			if tc.saved != nil {
				saved := *tc.saved
				if err := nodestate.SaveState(&saved); err != nil {
					t.Fatal(err)
				}
			}

			st, err := PrepareWorker(context.Background(), tc.opts)
			if err != nil {
				t.Fatalf("PrepareWorker = %v", err)
			}
			primary, _, _ := strings.Cut(tc.want, ",")
			for _, got := range []struct {
				what string
				st   *nodestate.NodeState
			}{
				{"the joined worker starts on", st},
				{"the join saved", mustLoadState(t)},
			} {
				if got.st.NodeIP != primary || got.st.KubeletNodeIP != tc.want {
					t.Errorf("%s node IP %q / kubelet IPs %q, want %q / %q", got.what, got.st.NodeIP, got.st.KubeletNodeIP, primary, tc.want)
				}
			}
			if !slices.Equal(*detected, tc.wantDetect) {
				t.Errorf("address detection was called with dual-stack %v, want %v", *detected, tc.wantDetect)
			}
		})
	}
}
