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

// A worker picks its address by the rule every controller start uses:
// --node-ip, then the address saved in node.yaml, then spec.network.nodeIP,
// then $NODE_IP, then detection. A join resolves it with host.ResolveNodeIP
// and has no saved address to offer. A restart never reads
// spec.network.nodeIP, so it skips that step by resolving with
// host.ResolveNodeIPWithoutConfig, and it offers the address it saved: the
// bare `stratum up --worker` the reboot unit runs keeps that address, while a
// restart the operator runs with --node-ip moves the worker and saves the new
// address for every start after it.

// stubNodeIPDetection answers address detection with addrs and records the
// dual-stack argument of every call. hostDetectNodeIPs only detects: the
// resolver (host.ResolveNodeIP on a join, host.ResolveNodeIPWithoutConfig on a
// restart) weighs $NODE_IP before detection is reached, so a test fails on
// where the worker's address came from, and never looks at this host's
// interfaces.
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

// captureWorkerOutput runs fn and returns what it printed. Standard output
// and standard error go to one pipe, so a test pins what the operator is told
// rather than which stream tells them.
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

// A restart the operator runs with --node-ip is the one time a worker's new
// address is named: the reboot unit and a re-exec both run a bare `stratum up
// --worker`. That restart must start the worker on the new address and save
// it, or the next start quietly puts the kubelet back on the old one. Saving
// the address must leave every other field of node.yaml as the join wrote it.
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

// The reboot unit's bare restart must keep the address the worker registered
// with. $NODE_IP is whatever the environment of the shell or unit running the
// restart says now, so the saved address outranks it, as it does on a
// controller, and detection never runs. Older state records only the primary
// address, which is then the saved one. Keeping the address writes nothing.
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

	// Older state records only the primary address. Even when the flag
	// matches it, the kubelet must receive the explicit address on this
	// start and on the reboot unit's following bare restart.
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

// A worker whose state records no address has none for its restart to keep,
// so the restart resolves one by the same rule -- $NODE_IP, then detection --
// and saves it. The reboot unit's next bare restart then keeps that address
// instead of detecting again, perhaps a different one on a multi-NIC host.
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

// A restart reads its config file only for spec.selinux, never for
// spec.network.nodeIP, so when its state records no address and detection
// fails, the error must send the operator to a source the restart reads --
// --node-ip or $NODE_IP -- and not to spec.network.nodeIP, which it would
// never see. The refused restart leaves node.yaml as it was.
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

// A dual-stack worker moved with --node-ip v4,v6 registers both addresses: its
// primary address is the IPv4, and the kubelet is started with the pair. The
// kubelet's --node-ip is rendered from the node state, so the roster built from
// the state the restart returns, and from the one it saved for the next
// start, must carry the new pair. A worker that joined with a pair but without
// --dual-stack registered both families too, so it is moved by its pair as
// well: the restart judges the stack by the addresses the worker registered.
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

// A restart saves the address it moves to, and the reboot unit's bare restarts
// reuse it, so an address the kubelet cannot register must be refused before
// it is saved: a --node-ip that is not an IP address, or one that drops or
// adds an address family the worker registered with, would otherwise break a working worker
// on this start and on every reboot after it. The refusal names what was
// given, where it came from and the address the worker keeps, and node.yaml
// keeps the address it had.
func TestWorkerRestartRefusesANodeIPItCannotRegister(t *testing.T) {
	single := nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4",
		Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	dual := nodestate.NodeState{Role: "worker", Hostname: "w-8", NodeIP: "10.80.0.4", KubeletNodeIP: "10.80.0.4,fd80::4",
		DualStack: true, Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	// A worker joined with a pair but without --dual-stack registered both
	// families, and records only the pair.
	pair := dual
	pair.DualStack = false
	// A worker whose state records no address resolves one at its restart and
	// saves it, so a detected address is checked like a given one.
	unaddressed := nodestate.NodeState{Role: "worker", Hostname: "w-8", Server: "https://10.80.0.1:6443", PodCIDR: "10.82.0.0/16"}
	for _, tc := range []struct {
		name   string
		saved  nodestate.NodeState
		nodeIP string
		detect string   // what detection answers; "" answers 10.89.0.1
		want   []string // each in the refusal
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

// Every refusal a restart can give comes before the restart changes anything.
// A start that refuses, and says the worker keeps its saved node IP or pod
// CIDR, must not have saved another setting it was given, or prepared the host
// with that setting. Each case also carries a spec.selinux change, and the
// legacy worker a pod CIDR to record, which a restart that went ahead would
// save: after the refusal node.yaml is byte for byte what it was, and neither
// the host preparation nor the firewall step has run.
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
		env   string // $NODE_IP
		want  string // in the refusal
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

// Every worker start says which node IP it runs on and where the address came
// from, as every controller start does, so a worker that registered the wrong
// NIC's address, or picked up a stale $NODE_IP, can be told from its own log.
// A restart that keeps its saved address says so too.
func TestWorkerReportsItsNodeIPAndWhereItCameFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved *nodestate.NodeState // nil: a join on a fresh host
		opts  WorkerOptions
		env   string // $NODE_IP
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

// A bare `stratum up --worker`, which the reboot unit runs, resumes from
// node.yaml. When that file is there but cannot be read, the restart says so
// and names it, rather than asking for the join credentials a first join
// needs: joining again is a new registration, and what needs repairing is
// the file.
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

// A join saves the address it resolved, and every later start reuses it, so a
// join refuses an address the kubelet could not register before it touches
// the host or the cluster: no directories, no host preparation, no CA
// discovery, no trust files and no node.yaml. The refusal names the value and
// where it came from. The stack is read from the value: a v4,v6 pair is
// checked as a pair, and one address must be an IPv4 one. A join that main
// accepted with a well-formed value still joins: a pair without --dual-stack,
// and one IPv4 address with it.
func TestWorkerJoinRefusesANodeIPItCannotRegister(t *testing.T) {
	join := WorkerOptions{Server: "https://10.80.0.1:6443", Token: "join-token-8", CAHash: "sha256:88"}
	for _, tc := range []struct {
		name   string
		nodeIP string // --node-ip
		config string // spec.network.nodeIP
		env    string // $NODE_IP
		detect string // what detection answers; "" answers 10.89.0.1
		dual   bool   // --dual-stack
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

// A join takes the address the operator gave it -- --node-ip, or the
// spec.network.nodeIP that runUp passes in its place -- then $NODE_IP, then
// detection, which asks for both address families only on a dual-stack join.
// $NODE_IP is decided by the shared rule before detection is reached, as on a
// controller. A join is a new registration, so an address an earlier join
// saved on this host is not a source.
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
		saved      *nodestate.NodeState // nil: no node.yaml on this host
		opts       WorkerOptions
		env        string // $NODE_IP
		detect     string // what detection answers
		want       string
		wantDetect []bool // the dual-stack argument of each detection call
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
