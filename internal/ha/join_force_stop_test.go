package ha

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// Kill a join after its membership commits, on either side of node.yaml's
// publication. The next process must recover using durable local preparation
// and a quorumless member list, without another add or access to the API.
func TestJoinRecoversAfterForceStop(t *testing.T) {
	if root := os.Getenv("STRATUM_JOIN_FORCE_ROOT"); root != "" {
		runForceStopJoinChild(t, root, os.Getenv("STRATUM_JOIN_FORCE_PHASE"))
		return
	}
	for _, phase := range []string{"member-add", "state-save"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGKILL} {
			t.Run(fmt.Sprintf("%s/%s", phase, sig), func(t *testing.T) {
				root := t.TempDir()
				cmd, lines, waited, stderr := startForceStopJoinChild(t, root, phase)
				waitForceStopJoinLine(t, lines, "committed")
				if sig != syscall.SIGKILL {
					if err := cmd.Process.Signal(sig); err != nil {
						t.Fatal(err)
					}
					waitForceStopJoinLine(t, lines, "cancelled")
					select {
					case err := <-waited:
						t.Fatalf("first signal ended the commit window: %v; %s", err, stderr.String())
					default:
					}
				}
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-waited:
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Fatalf("force-stop result = %v, want signal termination; %s", err, stderr.String())
					}
					status, ok := exit.Sys().(syscall.WaitStatus)
					if !ok || status.Signal() != sig {
						t.Fatalf("force-stop status = %v, want %s", exit.ProcessState, sig)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("force-stop did not end the join")
				}
				data, err := os.ReadFile(filepath.Join(root, "node", "config", "join-intent.json"))
				if err != nil {
					t.Fatal(err)
				}
				var journal joinIntent
				if err := json.Unmarshal(data, &journal); err != nil || journal.Recovery == nil {
					t.Fatalf("force-stop lost the prepared recovery snapshot: %+v, %v", journal, err)
				}
				_, stateErr := os.Stat(filepath.Join(root, "node", "config", "node.yaml"))
				if phase == "state-save" && stateErr != nil {
					t.Fatalf("published state was lost: %v", stateErr)
				}
				if phase == "member-add" && !errors.Is(stateErr, os.ErrNotExist) {
					t.Fatalf("state was published before member-add returned: %v", stateErr)
				}
				_, resumed, resumeWaited, resumeStderr := startForceStopJoinChild(t, root, "resume")
				waitForceStopJoinLine(t, resumed, "recovered")
				select {
				case err := <-resumeWaited:
					if err != nil {
						t.Fatalf("recovery failed: %v; %s", err, resumeStderr.String())
					}
				case <-time.After(10 * time.Second):
					t.Fatal("recovery did not complete")
				}
				data, err = os.ReadFile(filepath.Join(root, "node", "config", "node.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				var st nodestate.NodeState
				if err := yaml.Unmarshal(data, &st); err != nil || st.EtcdState != "existing" || !strings.Contains(st.EtcdInitialCluster, journal.PeerURL) {
					t.Fatalf("recovery state = %+v, error %v", st, err)
				}
				if _, err := os.Stat(filepath.Join(root, "node", "config", "join-intent.json")); err != nil {
					t.Fatalf("recovery cleared its journal before a WAL existed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "etcd", "member", "wal")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fixture unexpectedly has a WAL: %v", err)
				}
			})
		}
	}
}

func startForceStopJoinChild(t *testing.T, root, phase string) (*exec.Cmd, <-chan string, <-chan error, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJoinRecoversAfterForceStop$")
	cmd.Env = append(os.Environ(), "STRATUM_JOIN_FORCE_ROOT="+root, "STRATUM_JOIN_FORCE_PHASE="+phase)
	stderr := new(bytes.Buffer)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	lines := make(chan string, 3)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "committed" || line == "cancelled" || line == "recovered" {
				lines <- line
			}
		}
	}()
	waited := make(chan error, 1)
	go func() {
		<-readDone
		waited <- cmd.Wait()
	}()
	return cmd, lines, waited, stderr
}

func waitForceStopJoinLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-lines:
		if !ok || got != want {
			t.Fatalf("join barrier = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("join did not reach %q", want)
	}
}

func runForceStopJoinChild(t *testing.T, root, phase string) {
	version.DataDir = filepath.Join(root, "node")
	version.K8sConfDir = filepath.Join(version.DataDir, "config")
	version.PKIDir = filepath.Join(version.DataDir, "pki")
	version.KubeconfigDir = filepath.Join(version.DataDir, "kubeconfig")
	version.NetConfigPath = filepath.Join(version.K8sConfDir, "netconfig")
	version.EncryptionConfigPath = filepath.Join(version.K8sConfDir, "encryption-config.yaml")
	version.CiliumSettingsPath = filepath.Join(version.K8sConfDir, "cilium.json")
	joinIntentPath = filepath.Join(version.K8sConfDir, "join-intent.json")
	remotePath := filepath.Join(root, "remote-members.json")
	for _, dir := range []string{version.K8sConfDir, version.PKIDir, version.KubeconfigDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if phase != "resume" {
		if err := netcfg.Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{version.PKIDir, version.KubeconfigDir} {
			if err := os.WriteFile(filepath.Join(dir, "prepared"), []byte("retained trust/configuration"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := &recordedJoin{}
	fx := fakeJoinEffects(r)
	fx.persistPreparation = persistJoinPreparation
	fx.verifyResumeTrust = func(string) error {
		for _, dir := range []string{version.PKIDir, version.KubeconfigDir} {
			data, err := os.ReadFile(filepath.Join(dir, "prepared"))
			if err != nil || string(data) != "retained trust/configuration" {
				return fmt.Errorf("retained preparation was lost: %s, %v", dir, err)
			}
		}
		return nil
	}
	fx.clusterMembers = func([]string) (etcdMembership, error) {
		data, err := os.ReadFile(remotePath)
		if err != nil {
			return etcdMembership{}, err
		}
		var members etcdMembership
		if err := json.Unmarshal(data, &members); err != nil {
			return etcdMembership{}, err
		}
		members.Quorum = false // the unfinished voting member has not started
		return members, nil
	}
	fx.memberRemoveByID = func([]string, uint64) error {
		t.Fatal("recovery tried to remove the committed member")
		return nil
	}
	ctx, stop := interrupt.Context()
	defer stop()
	pause := func() {
		fmt.Println("committed")
		<-ctx.Done()
		fmt.Println("cancelled")
		for {
			time.Sleep(time.Hour) // stalled commit; the second signal must kill
		}
	}
	fx.memberAdd = func(_ []string, name, peer string) (string, error) {
		if phase == "resume" {
			t.Fatal("recovery attempted a second member-add")
		}
		if _, err := os.Stat(remotePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("membership already exists: %v", err)
		}
		members := listedMembers()
		members.Members[1].PeerURLs = []string{peer}
		data, err := json.Marshal(members)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(remotePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := syncJoinFile(remotePath); err != nil {
			t.Fatal(err)
		}
		if err := syncJoinFile(root); err != nil {
			t.Fatal(err)
		}
		if phase == "member-add" {
			pause()
		}
		return initialClusterFor(members.Members, peer, name), nil
	}
	fx.saveState = func(st *nodestate.NodeState) error {
		if err := saveJoinState(st); err != nil {
			return err
		}
		if phase == "state-save" {
			pause()
		}
		return nil
	}
	if phase == "resume" {
		fx.discover = func(string, string, string) ([]byte, error) {
			t.Fatal("recovery attempted API discovery without quorum")
			return nil, nil
		}
		fx.downloadGrant = func(string, string, func(bundleProfile) error) (bundleGrant, error) {
			t.Fatal("recovery attempted a certificate download without quorum")
			return bundleGrant{}, nil
		}
	}
	if _, err := prepareControlPlaneJoin(ctx, freshJoinOpts(), filepath.Join(root, "etcd"), fx); err != nil {
		t.Fatal(err)
	}
	if phase != "resume" {
		t.Fatal("join never reached the force-stop barrier")
	}
	// The real state save and journal remain readable in this new process.
	if _, err := nodestate.LoadState(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("recovered")
}
