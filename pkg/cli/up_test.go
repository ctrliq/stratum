package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// TestProfileRequiresOperator pins the guard that stops a controller whose
// profile needs the operator from starting with the operator disabled.
//
// The failure it prevents is silent, which is why it is worth a test: the cluster
// comes up Ready, and only later does every `kubectl logs` and `kubectl exec`
// fail, because the profile makes kubelet serving certs bootstrap-issued and the
// approver that signs them lives in the operator.
func TestProfileRequiresOperator(t *testing.T) {
	on, off := true, false
	needs := profile.Profile{Name: "cis", RequiresOperator: true}
	none := profile.Profile{}

	for _, tc := range []struct {
		name    string
		p       profile.Profile
		op      *bool // nil = unset, which OperatorEnabled treats as enabled
		wantErr bool
	}{
		{"the profile with the operator left at its default", needs, nil, false},
		{"the profile with the operator explicitly on", needs, &on, false},
		{"the profile with the operator explicitly off", needs, &off, true},
		{"no profile, operator off — legitimate", none, &off, false},
		{"no profile, operator on", none, &on, false},
		{"no profile, operator unset", none, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &cluster.ClusterConfig{}
			cfg.Spec.Operator = tc.op

			err := checkProfileOperator(cfg, tc.p)
			if tc.wantErr && err == nil {
				t.Fatal("accepted a controller with no operator to approve kubelet serving CSRs")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refused a valid configuration: %v", err)
			}
			if !tc.wantErr {
				return
			}
			// The message has to be actionable: an operator hitting this at 3am
			// needs to know which two settings conflict and what to do.
			for _, want := range []string{"cis profile", "operator", "serving-certificate", "spec.operator: false"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q, so it is not actionable:\n  %v", want, err)
				}
			}
		})
	}
}

// TestProfileFromConfigFileIsGuardedToo is the case a flag-only guard would
// miss: the profile can equally arrive from spec.profile (or the legacy
// spec.cis) in the config file with no flag on the command line, so
// controllerConfig reads the resolved config instead.
func TestProfileFromConfigFileIsGuardedToo(t *testing.T) {
	registerCISForTest(t)
	dir := t.TempDir()
	for _, body := range []string{
		"apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  operator: false\n  profile: cis\n",
		"apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  operator: false\n  cis: true\n",
	} {
		cfgPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := controllerConfig(cfgPath, ""); err == nil {
			t.Fatalf("a profile set from the config file bypassed the guard:\n%s", body)
		}
	}
}

// TestControllerConfigAppliesTheGuard proves the guard is WIRED, not merely
// defined. TestCISRequiresOperator above would keep passing if the call site were
// deleted; this exercises the path runUp actually takes.
func TestControllerConfigAppliesTheGuard(t *testing.T) {
	registerCISForTest(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	write := func(body string) {
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("operator disabled plus --profile cis is refused", func(t *testing.T) {
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  operator: false\n")
		if _, err := controllerConfig(cfgPath, "cis"); err == nil {
			t.Fatal("--profile cis was accepted against a config with the operator disabled")
		}
	})

	t.Run("operator disabled plus spec.cis is refused", func(t *testing.T) {
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  operator: false\n  cis: true\n")
		if _, err := controllerConfig(cfgPath, ""); err == nil {
			t.Fatal("spec.cis was accepted against a config with the operator disabled")
		}
	})

	t.Run("selinux off plus --profile cis is refused", func(t *testing.T) {
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  selinux: false\n")
		if _, err := controllerConfig(cfgPath, "cis"); err == nil || !strings.Contains(err.Error(), "spec.selinux: false") {
			t.Fatalf("--profile cis with spec.selinux: false = %v, want a refusal naming spec.selinux", err)
		}
	})

	t.Run("selinux off without a profile is fine", func(t *testing.T) {
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  selinux: false\n")
		cfg, err := controllerConfig(cfgPath, "")
		if err != nil || !cfg.SELinuxOptOut() {
			t.Fatalf("controllerConfig = %v, %v; want spec.selinux: false accepted", cfg, err)
		}
	})

	t.Run("operator disabled without a profile is fine", func(t *testing.T) {
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  operator: false\n")
		if _, err := controllerConfig(cfgPath, ""); err != nil {
			t.Fatalf("refused a legitimate profile-less configuration: %v", err)
		}
	})

	t.Run("--profile cis with the operator at its default is fine", func(t *testing.T) {
		cfg, err := controllerConfig(filepath.Join(dir, "absent.yaml"), "cis")
		if err != nil {
			t.Fatalf("refused --profile cis on a default config: %v", err)
		}
		if cfg.ProfileName() != "cis" || !cfg.Spec.CIS {
			t.Errorf("--profile cis did not reach the resolved config: profile %q, cis %v", cfg.ProfileName(), cfg.Spec.CIS)
		}
	})

	t.Run("a profile this binary does not register is refused", func(t *testing.T) {
		_, err := controllerConfig(filepath.Join(dir, "absent.yaml"), "stig")
		if !errors.Is(err, profile.ErrUnregistered) || !strings.Contains(err.Error(), `"stig"`) || !strings.Contains(err.Error(), "registered: cis") {
			t.Fatalf("--profile stig = %v, want a refusal naming it and the registered profiles", err)
		}
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  profile: stig\n")
		if _, err := controllerConfig(cfgPath, ""); !errors.Is(err, profile.ErrUnregistered) {
			t.Fatalf("spec.profile: stig = %v, want the same refusal", err)
		}
	})

	t.Run("a flag and a config naming different profiles is refused", func(t *testing.T) {
		write("apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\nspec:\n  profile: cis\n")
		if _, err := controllerConfig(cfgPath, "stig"); err == nil || !strings.Contains(err.Error(), "two profiles") {
			t.Fatalf("--profile stig against spec.profile: cis = %v, want a refusal", err)
		}
	})
}

// TestRunningNodeError pins the preflight decision: refuse a start when a
// supervisor is already live, or when an earlier `stratum up` left processes
// running; proceed only when the node is genuinely clear.
func TestRunningNodeError(t *testing.T) {
	// A live supervisor: a plain double-start — refuse, pointing at the socket.
	if err := runningNodeError(true, false); err == nil {
		t.Error("did not refuse when a supervisor is already running")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Errorf("supervisor-running message unclear: %v", err)
	}
	// Supervisor down but containerd still serving with no supervisor: an orphan
	// from a kill -9 / crash. Refuse, and point at recovery so the operator isn't
	// left decoding a kubelet crash-loop.
	err := runningNodeError(false, true)
	if err == nil {
		t.Fatal("did not refuse an orphaned containerd — a fresh start would crash-loop kubelet")
	}
	for _, want := range []string{"containerd", "stratum uninstall", "supervisor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("orphan message missing %q: %v", want, err)
		}
	}
	// Clear node (no supervisor, containerd down — the normal post-clean-stop
	// state, control-plane containers surviving notwithstanding): proceed.
	if err := runningNodeError(false, false); err != nil {
		t.Errorf("refused a clear node: %v", err)
	}
}

// TestAlreadyJoinedDecision pins that `stratum up --join` on a node that
// already has state must never re-run the join, because the join deletes the
// etcd data dir. Same cluster → start as a member; anything else → refuse.
func TestAlreadyJoinedDecision(t *testing.T) {
	const (
		ours   = "sha256:" + "aa00000000000000000000000000000000000000000000000000000000000000"
		staged = "sha256:" + "bb00000000000000000000000000000000000000000000000000000000000000"
		theirs = "sha256:" + "cc00000000000000000000000000000000000000000000000000000000000000"
	)
	const members = "cp1=https://10.0.0.11:2380,cp2=https://10.0.0.12:2380"
	ctrl := &cluster.NodeState{Role: "controller", EtcdName: "cp2", EtcdInitialCluster: members, EtcdState: "existing"}
	boot := &cluster.NodeState{Role: "controller", EtcdName: "cp1", EtcdInitialCluster: "cp1=https://10.0.0.11:2380", EtcdState: "new"}
	// A parseable node.yaml whose membership is
	// incomplete would be restarted as a brand-new single-member etcd.
	noMembers := &cluster.NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "existing"}
	noName := &cluster.NodeState{Role: "controller", EtcdInitialCluster: members, EtcdState: "existing"}
	notListed := &cluster.NodeState{Role: "controller", EtcdName: "cp3", EtcdInitialCluster: members, EtcdState: "existing"}
	badState := &cluster.NodeState{Role: "controller", EtcdName: "cp2", EtcdInitialCluster: members, EtcdState: "joined"}
	multiNew := &cluster.NodeState{Role: "controller", EtcdName: "cp2", EtcdInitialCluster: members, EtcdState: "new"}
	worker := &cluster.NodeState{Role: "worker"}

	missing := fmt.Errorf("open node.yaml: %w", fs.ErrNotExist)
	unreadable := fmt.Errorf("open node.yaml: %w", fs.ErrPermission)
	malformed := errors.New("node.yaml: yaml: line 3: mapping values are not allowed in this context")

	for _, tc := range []struct {
		name     string
		st       *cluster.NodeState
		loadErr  error
		pins     []string
		want     string
		asMember bool
		errHas   []string
	}{
		{"no state: a fresh node joins", nil, missing, nil, ours, false, nil},
		{"joined member, same cluster: start, do not re-join", ctrl, nil, []string{ours}, ours, true, nil},
		{"bootstrap node of the same cluster: start", boot, nil, []string{ours}, ours, true, nil},
		{"multi-member topology with state=new: refuse", multiNew, nil, []string{ours}, ours, false, []string{"state=new with multiple etcd members", "single-member bootstrap", "stratum uninstall"}},
		{"token minted mid-rotation pins ca-new: still ours", ctrl, nil, []string{ours, staged}, staged, true, nil},
		{"different cluster: refuse", ctrl, nil, []string{ours}, theirs, false, []string{"different cluster", ours, theirs, "stratum uninstall"}},
		{"no readable CA: cannot tell, refuse", ctrl, nil, nil, ours, false, []string{"no readable", "without --join", "stratum uninstall"}},
		{"a worker: --join would convert it, refuse", worker, nil, []string{ours}, ours, false, []string{`"worker"`, "control plane", "stratum uninstall"}},
		// State that exists but cannot be read is not
		// "no state", and must not take the fresh-join path that wipes etcd.
		{"unreadable state: refuse, do not treat as fresh", nil, unreadable, []string{ours}, ours, false, []string{"cannot read", "permission denied", "stratum uninstall"}},
		{"malformed state: refuse, do not treat as fresh", nil, malformed, []string{ours}, ours, false, []string{"cannot read", "mapping values", "stratum uninstall"}},
		// ...and a bad pin is a bad pin, not "a different cluster".
		{"empty pin on a member: the pin diagnosis", ctrl, nil, []string{ours}, "", false, []string{"no CA pin supplied"}},
		{"member with no etcd membership: refuse", noMembers, nil, []string{ours}, ours, false, []string{"no etcd cluster membership", "state=new", "stratum uninstall"}},
		{"member with no etcd name: refuse", noName, nil, []string{ours}, ours, false, []string{"no etcd member name"}},
		{"membership that omits this member: refuse", notListed, nil, []string{ours}, ours, false, []string{"does not include this member (cp3)"}},
		{"unknown etcd state: refuse", badState, nil, []string{ours}, ours, false, []string{`etcd state "joined"`}},
		{"malformed pin on a member: the pin diagnosis", ctrl, nil, []string{ours}, "SHA256:AA00", false, []string{"malformed --discovery-token-ca-cert-hash", "NOT a certificate mismatch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asMember, err := alreadyJoinedDecision(tc.st, tc.loadErr, func() []string { return tc.pins }, tc.want)
			if asMember != tc.asMember {
				t.Errorf("asMember = %v, want %v", asMember, tc.asMember)
			}
			if (err != nil) != (tc.errHas != nil) {
				t.Fatalf("err = %v, want error: %v", err, tc.errHas != nil)
			}
			for _, s := range tc.errHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error missing %q: %v", s, err)
				}
			}
			if strings.Contains(fmt.Sprint(err), "different cluster") && tc.want != theirs {
				t.Errorf("a pin problem was reported as a different cluster: %v", err)
			}
		})
	}
}

// TestLocalCAPinsMatchTheJoinPin pins the other half of the comparison: the pins
// alreadyJoinedDecision is handed are computed exactly as a joiner's pin is
// (clusterinfo.PinFor, the value `stratum token create` prints), so a member of
// the cluster being joined is recognised — and ca-new.crt counts while staged.
func TestLocalCAPinsMatchTheJoinPin(t *testing.T) {
	orig := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = orig })

	if pins := localCAPins(); len(pins) != 0 {
		t.Fatalf("no CA on disk should yield no pins, got %v", pins)
	}
	ca := writeTestCA(t, "ca")
	pins := localCAPins()
	if len(pins) != 1 || pins[0] != clusterinfo.PinFor(ca) {
		t.Fatalf("pins = %v, want [%s]", pins, clusterinfo.PinFor(ca))
	}
	staged := writeTestCA(t, "ca-new")
	pins = localCAPins()
	if len(pins) != 2 || pins[1] != clusterinfo.PinFor(staged) {
		t.Fatalf("with ca-new staged, pins = %v, want [.., %s]", pins, clusterinfo.PinFor(staged))
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if pins := localCAPins(); len(pins) != 0 {
		t.Fatalf("missing live CA key must refuse even a staged CA pin, got %v", pins)
	}
}

func TestRequireLocalEtcdMember(t *testing.T) {
	dir := t.TempDir()
	if err := requireLocalEtcdMember(dir); err == nil {
		t.Fatal("accepted saved state without local etcd member data")
	}
	if err := os.Mkdir(filepath.Join(dir, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireLocalEtcdMember(dir); err == nil {
		t.Fatal("accepted an empty member directory")
	}
	walDir := filepath.Join(dir, "member", "wal")
	if err := os.Mkdir(walDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireLocalEtcdMember(dir); err == nil {
		t.Fatal("accepted an empty WAL directory")
	}
	if err := os.WriteFile(filepath.Join(walDir, "0000000000000000-0000000000000000.wal"), []byte("persisted WAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireLocalEtcdMember(dir); err != nil {
		t.Fatalf("refused existing member data: %v", err)
	}
}

// writeTestCA writes a self-signed CA certificate and matching key.
func writeTestCA(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(version.PKIDir, name+".crt"), pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, name+".key"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestResolveCertKey pins key sourcing: --certificate-key wins when
// given, otherwise the key is read (trimmed) from --certificate-key-file so it
// never has to travel on a command line; empty inputs are not an error (the join
// path validates downstream), and an unreadable file is a clear error.
func TestResolveCertKey(t *testing.T) {
	if got, err := resolveCertKey("inline", "/nonexistent"); err != nil || got != "inline" {
		t.Fatalf("--certificate-key should win: got %q err %v", got, err)
	}
	if got, err := resolveCertKey("", ""); err != nil || got != "" {
		t.Fatalf("no key material: want empty/nil, got %q err %v", got, err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("  filekey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveCertKey("", p); err != nil || got != "filekey" {
		t.Fatalf("--certificate-key-file should be read and trimmed: got %q err %v", got, err)
	}
	if _, err := resolveCertKey("", filepath.Join(dir, "nope")); err == nil {
		t.Fatal("an unreadable --certificate-key-file must error, not resolve to empty")
	}
}

// TestUpFlagConflict: --worker beside a join flag would silently drop the
// join flag, and on a controller it would rewrite node.yaml as a worker.
// The combination is refused up front, before any lock, stage or state read.
func TestUpFlagConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    runUpOptions
		bad  []string
	}{
		{"worker alone", runUpOptions{worker: true, server: "https://10.0.0.11:6443"}, nil},
		{"control-plane join alone", runUpOptions{join: "https://10.0.0.11:6443", certKeyFile: "/k"}, nil},
		{"worker and join", runUpOptions{worker: true, join: "https://10.0.0.11:6443", server: "https://10.0.0.11:6443"}, []string{"--join"}},
		{"worker and a certificate key", runUpOptions{worker: true, certKey: "k"}, []string{"--certificate-key"}},
		{"worker and all three", runUpOptions{worker: true, join: "x", certKey: "k", certKeyFile: "/k"}, []string{"--join", "--certificate-key", "--certificate-key-file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := upFlagConflict(tc.o)
			if (err != nil) != (tc.bad != nil) {
				t.Fatalf("err = %v, want error: %v", err, tc.bad != nil)
			}
			for _, f := range tc.bad {
				if !strings.Contains(err.Error(), f) {
					t.Errorf("error does not name %s: %v", f, err)
				}
			}
		})
	}
}

// writeEmptyConfig writes an empty cluster config into dir and returns its
// path. runUp refuses a --config naming a file that does not exist, so a test
// that wants the built-in defaults names an empty file that does.
func writeEmptyConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestUpConfigPath: a --config naming a missing file must not start the
// node on pure defaults and record the bad path in the unit. Only the
// default path may be absent; a relative path is judged by, and resolved to,
// the file it names.
func TestUpConfigPath(t *testing.T) {
	tmp := t.TempDir()
	present := writeEmptyConfig(t, tmp)
	t.Chdir(tmp)
	for _, tc := range []struct {
		name    string
		path    string
		want    string
		refused bool
	}{
		{"no --config", "", "", false},
		{"the default path, present or not", cluster.DefaultConfigPath, cluster.DefaultConfigPath, false},
		{"the default path, spelled unclean", "/etc/stratum/../stratum/config.yaml", cluster.DefaultConfigPath, false},
		{"an existing file", present, present, false},
		{"an existing file, relative", "config.yaml", present, false},
		{"a missing file", filepath.Join(tmp, "nope.yaml"), "", true},
		{"a missing file, relative", "nope.yaml", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := upConfigPath(tc.path, false)
			if (err != nil) != tc.refused {
				t.Fatalf("upConfigPath(%q) = %q, %v, want refused: %v", tc.path, got, err, tc.refused)
			}
			if got != tc.want {
				t.Errorf("upConfigPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
			if tc.refused {
				for _, want := range []string{filepath.Join(tmp, "nope.yaml"), "If the stratum unit runs this node", "stratum up --config <the intended file>", "sudo systemctl start stratum", "Nothing on this host was changed"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal missing %q: %v", want, err)
					}
				}
			}
		})
	}
	// A worker's unit never carries --config, so its refusal does not claim
	// that running `stratum up --config` records a path for later starts.
	_, err := upConfigPath("nope.yaml", true)
	if err == nil || strings.Contains(err.Error(), "If the stratum unit runs this node") || !strings.Contains(err.Error(), "name the intended one") {
		t.Errorf("worker refusal = %v, want the worker wording", err)
	}
}

// TestUpRefusesAnExplicitEmptyConfig: `--config ""` (an unset "$CFG") read
// no file at all, not even the default, and started on pure defaults with the
// empty path recorded in the unit. Refused at the flag, before runUp.
func TestUpRefusesAnExplicitEmptyConfig(t *testing.T) {
	// Stamped and redirected like a start the guard let through, so the
	// refusal is not mistaken for the unstamped-build one, and a command that
	// skipped it stops at the stubbed preparation instead of touching the host.
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
		{&version.K8sVersion, "v1.37.1"},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	oldInstall, oldRun, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	t.Cleanup(func() { installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRun, oldPrepare })
	installUnit = func([]string) error { return errors.New("installed a unit for an empty --config") }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error {
		return errors.New("ran a control plane for an empty --config")
	}
	prepared := false
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		prepared = true
		return nil, errors.New("prepared a node for an empty --config")
	}
	root := NewRootCommand()
	root.SetArgs([]string{"up", "--config", ""})
	err := root.Execute()
	// A unit an earlier build wrote may carry the empty --config, so the
	// refusal says how to rewrite it.
	if err == nil || !strings.Contains(err.Error(), "--config is empty") || !strings.Contains(err.Error(), "sudo systemctl start stratum") {
		t.Fatalf("up --config \"\" = %v, want the empty-config refusal", err)
	}
	if prepared {
		t.Error("up --config \"\" prepared the node before refusing")
	}
}

// TestRunUpRefusesAMissingConfigBeforeTouchingAnything drives both roles with a
// --config that names a missing file: the control plane must not prepare the
// node on defaults and install a unit carrying the missing path, and a
// worker join must not go on to join without the file's node IP and SELinux
// setting.
func TestRunUpRefusesAMissingConfigBeforeTouchingAnything(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	// Stub every step past the refusal so that a runUp which skipped it still
	// never touches the test machine.
	oldInstall, oldRun, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	oldRunWorker, oldPrepareWorker := runWorkerFn, prepareWorkerFn
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRun, oldPrepare
		runWorkerFn, prepareWorkerFn = oldRunWorker, oldPrepareWorker
	})
	var touched []string
	installUnit = func([]string) error { touched = append(touched, "installed the unit"); return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error {
		touched = append(touched, "ran the control plane")
		return nil
	}
	runWorkerFn = func(context.Context, *cluster.NodeState) error {
		touched = append(touched, "ran the worker")
		return nil
	}
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		touched = append(touched, "prepared the control plane")
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}
	prepareWorkerFn = func(context.Context, cluster.WorkerOptions) (*cluster.NodeState, error) {
		touched = append(touched, "prepared the worker")
		return &cluster.NodeState{Role: "worker", Hostname: "w1"}, nil
	}

	missing := filepath.Join(tmp, "nope.yaml")
	for _, o := range []runUpOptions{
		{configPath: missing},
		{configPath: missing, worker: true, server: "https://10.0.0.11:6443", token: "abcdef.0123456789abcdef"},
	} {
		touched = nil
		err := runUp(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), missing+" does not exist") {
			t.Fatalf("runUp(worker=%v, --config %s) = %v, want the missing-config refusal", o.worker, missing, err)
		}
		if len(touched) != 0 {
			t.Errorf("runUp(worker=%v) %v before refusing", o.worker, touched)
		}
		// The start lock is the first thing runUp creates (O_CREATE), ahead of
		// the data lock and staging; DataDir alone proves nothing here, since
		// a test binary has no payload to stage.
		for _, p := range []string{startLockPath, version.DataDir, version.EtcdDataDir} {
			if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("runUp(worker=%v) created %s before refusing (stat: %v)", o.worker, p, err)
			}
		}
	}
}

// TestUpNetworkFlagRefusalAdvisesByRole: the network flags without --worker
// are refused with advice the user can follow. A control-plane join takes the
// cluster's network, so the advice is to drop the flags: adding --worker to a
// --join line is itself refused, and spec.network can only repeat the
// cluster's values. A workload-only node is pointed at the worker line
// `stratum token create` prints, never at this line with --worker swapped in:
// a --join line's token can read the shared cluster CA material, and a worker
// join keeps its token on disk. Any other start is pointed at spec.network,
// read at the cluster's first boot. Every flag given is named in the one
// refusal.
func TestUpNetworkFlagRefusalAdvisesByRole(t *testing.T) {
	all := runUpOptions{podCIDR: "10.50.0.0/16", serviceCIDR: "10.60.0.0/16", clusterDomain: "corp.example"}
	join := all
	join.join, join.certKeyFile = "https://10.0.0.11:6443", "/k"
	named := []string{
		`--pod-cidr "10.50.0.0/16" (spec.network.podCIDR)`,
		`--service-cidr "10.60.0.0/16" (spec.network.serviceCIDR)`,
		`--cluster-domain "corp.example" (spec.network.clusterDomain)`,
	}
	for _, tc := range []struct {
		name          string
		o             runUpOptions
		want, notWant []string
	}{
		{"a control-plane join", join,
			[]string{"remove these flags", "the worker join line `stratum token create` prints", "without --control-plane"},
			[]string{"add --worker", "move each value", "in place of --join"}},
		{"any other control-plane start", all,
			[]string{"move each value into the field named beside it", "first boot", "add --worker and --server"},
			[]string{"remove these flags"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := upFlagConflict(tc.o)
			if err == nil {
				t.Fatal("upFlagConflict = <nil>, want the network flags refused without --worker")
			}
			for _, want := range append(named, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not say %q: %v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("refusal says %q, advice this start cannot follow: %v", bad, err)
				}
			}
		})
	}
}

// TestRunUpRefusesWorkerJoinBeforeTouchingAnything: the refusal is the first
// thing runUp does, so it cannot be reached only after preflight, the start
// lock or staging has run.
func TestRunUpRefusesWorkerJoinBeforeTouchingAnything(t *testing.T) {
	err := runUp(context.Background(), runUpOptions{worker: true, join: "https://10.0.0.11:6443", server: "https://10.0.0.11:6443"})
	if err == nil || !strings.Contains(err.Error(), "--worker cannot be combined with --join") {
		t.Fatalf("runUp(--worker --join) = %v", err)
	}
}

// TestUpRefusesTheNetworkFlagsWithoutWorker: --pod-cidr, --service-cidr and
// --cluster-domain are worker-join flags, and runUp reads them only in the
// worker branch, so a control plane given one, at first boot or at --join,
// would start on a network its operator did not ask for. Each is refused,
// naming the flag and pointing at spec.network, before the start lock is
// taken or any preparation runs; a worker join still takes all three. The
// refusal must name the flag given, with its value and its own spec.network
// field: its fixed text lists all three flags and fields, so a refusal that
// named no flag would pass a check for those alone.
func TestUpRefusesTheNetworkFlagsWithoutWorker(t *testing.T) {
	const (
		server  = "https://10.0.0.11:6443"
		podCIDR = "10.50.0.0/16"
		svcCIDR = "10.60.0.0/16"
		domain  = "corp.example"
	)
	given := map[string]string{"--pod-cidr": podCIDR, "--service-cidr": svcCIDR, "--cluster-domain": domain}
	for _, tc := range []struct {
		name  string
		o     runUpOptions
		flag  string // the flag the refusal names; "" for a start that is accepted
		field string // the spec.network field the refusal points at
	}{
		{"pod CIDR at first boot", runUpOptions{podCIDR: podCIDR}, "--pod-cidr", "podCIDR"},
		{"service CIDR at first boot", runUpOptions{serviceCIDR: svcCIDR}, "--service-cidr", "serviceCIDR"},
		{"cluster domain at first boot", runUpOptions{clusterDomain: domain}, "--cluster-domain", "clusterDomain"},
		{"pod CIDR at join", runUpOptions{join: server, podCIDR: podCIDR}, "--pod-cidr", "podCIDR"},
		{"service CIDR at join", runUpOptions{join: server, serviceCIDR: svcCIDR}, "--service-cidr", "serviceCIDR"},
		{"cluster domain at join", runUpOptions{join: server, clusterDomain: domain}, "--cluster-domain", "clusterDomain"},
		{"worker join takes all three", runUpOptions{worker: true, server: server, token: "abcdef.0123456789abcdef",
			podCIDR: podCIDR, serviceCIDR: svcCIDR, clusterDomain: domain}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			for _, sw := range []struct {
				p *string
				v string
			}{
				{&version.DataDir, filepath.Join(tmp, "data")},
				{&version.K8sConfDir, filepath.Join(tmp, "config")},
				{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
				{&startLockPath, filepath.Join(tmp, "start.lock")},
			} {
				old := *sw.p
				*sw.p = sw.v
				t.Cleanup(func() { *sw.p = old })
			}
			t.Setenv(tokenEnvVar, "")

			oldInstall, oldRunWorker, oldRunController := installUnit, runWorkerFn, runControllerFn
			oldPrepareWorker, oldPrepareController, oldPrepareJoin := prepareWorkerFn, prepareControllerFn, prepareControlPlaneJoinFn
			t.Cleanup(func() {
				installUnit, runWorkerFn, runControllerFn = oldInstall, oldRunWorker, oldRunController
				prepareWorkerFn, prepareControllerFn, prepareControlPlaneJoinFn = oldPrepareWorker, oldPrepareController, oldPrepareJoin
			})
			var calls []string
			var gotWorker *cluster.WorkerOptions
			installUnit = func([]string) error { calls = append(calls, "installUnit"); return nil }
			runWorkerFn = func(context.Context, *cluster.NodeState) error { calls = append(calls, "runWorker"); return nil }
			runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error {
				calls = append(calls, "runController")
				return nil
			}
			prepareWorkerFn = func(_ context.Context, wo cluster.WorkerOptions) (*cluster.NodeState, error) {
				calls = append(calls, "prepareWorker")
				gotWorker = &wo
				return &cluster.NodeState{Role: "worker", Hostname: "w1", Server: server}, nil
			}
			prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
				calls = append(calls, "prepareController")
				return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
			}
			prepareControlPlaneJoinFn = func(context.Context, ha.JoinOptions) (*cluster.NodeState, error) {
				calls = append(calls, "prepareControlPlaneJoin")
				return &cluster.NodeState{Role: "controller", Hostname: "cp2"}, nil
			}

			o := tc.o
			o.configPath = writeEmptyConfig(t, tmp)
			err := runUp(context.Background(), o)

			if tc.flag == "" {
				if err != nil {
					t.Fatalf("runUp(--worker --server with the network flags) = %v, want it accepted", err)
				}
				if gotWorker == nil {
					t.Fatalf("the worker join never reached its preparation; calls = %v", calls)
				}
				if gotWorker.PodCIDR != podCIDR || gotWorker.ServiceCIDR != svcCIDR || gotWorker.ClusterDomain != domain {
					t.Errorf("worker join got PodCIDR %q, ServiceCIDR %q, ClusterDomain %q; want %q, %q, %q",
						gotWorker.PodCIDR, gotWorker.ServiceCIDR, gotWorker.ClusterDomain, podCIDR, svcCIDR, domain)
				}
				return
			}

			if err == nil {
				t.Fatalf("runUp(%s) = <nil>, want a refusal naming %s and spec.network; calls = %v", tc.flag, tc.flag, calls)
			}
			for _, want := range []string{fmt.Sprintf("%s %q", tc.flag, given[tc.flag]), "spec.network." + tc.field} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("runUp(%s) refusal does not say %q: %v", tc.flag, want, err)
				}
			}
			for flag := range given {
				if flag != tc.flag && strings.Contains(err.Error(), flag+` "`) {
					t.Errorf("runUp(%s) refusal names %s, which this start did not give: %v", tc.flag, flag, err)
				}
			}
			if len(calls) != 0 {
				t.Errorf("runUp(%s) ran %v before refusing; want nothing prepared, installed or run", tc.flag, calls)
			}
			if _, statErr := os.Stat(startLockPath); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("runUp(%s) took the start lock %s before refusing (stat: %v); want the refusal before anything on the host is touched",
					tc.flag, startLockPath, statErr)
			}
		})
	}
}

// TestUpJoinHandsSpecNetworkToTheJoin: a control plane's --join reads its
// network from spec.network, and the join refuses ranges or a domain that
// differ from the cluster's only when runUp passes them in. A field runUp
// leaves out reaches the join as unset, so the join takes the cluster's value
// and the operator's is dropped with no word until the node's first restart.
func TestUpJoinHandsSpecNetworkToTheJoin(t *testing.T) {
	const (
		podCIDR = "10.50.0.0/16"
		svcCIDR = "10.60.0.0/16"
		domain  = "corp.example"
	)
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	config := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(config, []byte("spec:\n  network:\n    podCIDR: "+podCIDR+
		"\n    serviceCIDR: "+svcCIDR+"\n    clusterDomain: "+domain+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldInstall, oldRun, oldJoin := installUnit, runControllerFn, prepareControlPlaneJoinFn
	t.Cleanup(func() { installUnit, runControllerFn, prepareControlPlaneJoinFn = oldInstall, oldRun, oldJoin })
	installUnit = func([]string) error { return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
	var got *ha.JoinOptions
	prepareControlPlaneJoinFn = func(_ context.Context, jo ha.JoinOptions) (*cluster.NodeState, error) {
		got = &jo
		return &cluster.NodeState{Role: "controller", Hostname: "cp2"}, nil
	}

	err := runUp(context.Background(), runUpOptions{configPath: config, join: "https://10.0.0.11:6443",
		token: "abcdef.0123456789abcdef", caHash: "sha256:" + strings.Repeat("ab", 32), certKey: "k"})
	if err != nil {
		t.Fatalf("runUp(--join) = %v", err)
	}
	if got == nil {
		t.Fatal("runUp(--join) never reached the join")
	}
	for _, f := range []struct{ field, got, want string }{
		{"spec.network.podCIDR", got.PodCIDR, podCIDR},
		{"spec.network.serviceCIDR", got.ServiceCIDR, svcCIDR},
		{"spec.network.clusterDomain", got.ClusterDomain, domain},
	} {
		if f.got != f.want {
			t.Errorf("the join got %s %q, want %q from the config", f.field, f.got, f.want)
		}
	}
}

// TestRunUpInstallsTheSteadyStateUnitOnWorkerRestart closes a wiring
// gap: neither the container check (no systemd there,
// deliberately — see tools/inttest/check-reboot-persists.sh) nor
// internal/host/systemd_test.go (which hands InstallUnit its own argv
// directly) can tell you whether runUp still CALLS InstallUnit at all. FAILS
// WITHOUT THE FIX: deleting the InstallUnit call from runUp's worker branch
// leaves both of those green.
//
// Drives runUp's worker restart, with prepareWorkerFn stubbed to hand back the
// persisted NodeState (no token needed), because it is the shortest path
// through runUp that reaches InstallUnit — no host prereqs, PKI or bootstrap,
// and no systemd: installUnit is stubbed, so host.SystemdAvailable() is never
// consulted. PrepareWorker's own restart path is internal/cluster's to test. TestRunUpInstallsTheSteadyStateUnitOnControlPlaneRestart is the
// sibling for the OTHER branch — this test alone leaves the
// control-plane call site (up.go:265) unguarded.
func TestRunUpInstallsTheSteadyStateUnitOnWorkerRestart(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}

	if err := cluster.SaveState(&cluster.NodeState{Role: "worker", Hostname: "n1", Server: "https://10.0.0.1:6443"}); err != nil {
		t.Fatal(err)
	}

	oldInstall, oldRunWorker, oldPrepare := installUnit, runWorkerFn, prepareWorkerFn
	t.Cleanup(func() {
		installUnit, runWorkerFn, prepareWorkerFn = oldInstall, oldRunWorker, oldPrepare
	})
	var gotArgv []string
	installUnit = func(argv []string) error { gotArgv = argv; return nil }
	runWorkerFn = func(context.Context, *cluster.NodeState) error { return nil }
	// The restart path prepares the host, never the test machine's.
	prepareWorkerFn = func(context.Context, cluster.WorkerOptions) (*cluster.NodeState, error) { return cluster.LoadState() }

	if err := runUp(context.Background(), runUpOptions{worker: true}); err != nil {
		t.Fatalf("runUp(--worker) = %v", err)
	}
	if gotArgv == nil {
		t.Fatal("runUp never called InstallUnit")
	}
	want := []string{selfExePath(), "up", "--worker"}
	if !slices.Equal(gotArgv, want) {
		t.Errorf("InstallUnit argv = %v, want %v (steady state, no join flags)", gotArgv, want)
	}
}

// TestRunUpInstallsTheSteadyStateUnitOnControlPlaneRestart is the
// control-plane sibling of TestRunUpInstallsTheSteadyStateUnitOnWorkerRestart:
// deleting the control-plane InstallUnit call affects only the non-worker
// branch, so the worker test stays green — this test is what turns that
// deletion red.
//
// prepareControllerFn is stubbed because the real cluster.PrepareController
// generates PKI and calls host.Prereqs, which runs modprobe/sysctl/swapoff
// against the actual host — not something a unit test may do. No systemd
// either: installUnit is stubbed, so host.SystemdAvailable() is never
// consulted.
func TestRunUpInstallsTheSteadyStateUnitOnControlPlaneRestart(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}

	oldInstall, oldRunController, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRunController, oldPrepare
	})
	var gotArgv []string
	installUnit = func(argv []string) error { gotArgv = argv; return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}

	configPath := writeEmptyConfig(t, tmp)
	if err := runUp(context.Background(), runUpOptions{configPath: configPath}); err != nil {
		t.Fatalf("runUp() = %v", err)
	}
	if gotArgv == nil {
		t.Fatal("runUp never called InstallUnit")
	}
	want := []string{selfExePath(), "up", "--config", configPath}
	if !slices.Equal(gotArgv, want) {
		t.Errorf("InstallUnit argv = %v, want %v (steady state, no join flags)", gotArgv, want)
	}
}

// TestRunUpPersistsAnAbsoluteConfigPath is a regression test: a
// relative --config reads correctly from the caller's cwd on THIS run, but a
// system service starts with / as its cwd by default, so the same relative
// string in the persisted unit would name a different file, or none, after
// reboot. Fails when the persisted argv carries the relative string
// verbatim.
func TestRunUpPersistsAnAbsoluteConfigPath(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}

	oldInstall, oldRunController, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRunController, oldPrepare
	})
	var gotArgv []string
	installUnit = func(argv []string) error { gotArgv = argv; return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}

	t.Chdir(tmp)
	relConfig := "relative-config.yaml"
	if err := os.WriteFile(relConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(relConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := runUp(context.Background(), runUpOptions{configPath: relConfig}); err != nil {
		t.Fatalf("runUp() = %v", err)
	}
	if len(gotArgv) == 0 || gotArgv[len(gotArgv)-1] != want {
		t.Fatalf("InstallUnit argv = %v, want the --config value resolved to %q", gotArgv, want)
	}
	if !filepath.IsAbs(gotArgv[len(gotArgv)-1]) {
		t.Errorf("persisted --config %q is not absolute", gotArgv[len(gotArgv)-1])
	}
}

// TestRunUpRefusesToStartAnEmptyEtcdOverRecordedMembership covers the
// empty-datastore case: a controller whose saved state records membership of a
// larger etcd cluster (state=existing, or several initial members), restored
// without its datastore, must not be started on an empty data dir while its
// peers may still hold quorum.
// A single-member state=new record is the sole-controller replacement the docs
// describe and still starts. Fails if the WAL guard runs only on the --join
// path: every row below would start.
func TestRunUpRefusesToStartAnEmptyEtcdOverRecordedMembership(t *testing.T) {
	const (
		single = "cp1=https://10.0.0.1:2380"
		multi  = single + ",cp2=https://10.0.0.2:2380"
	)
	for _, tc := range []struct {
		name     string
		st       cluster.NodeState
		wal      bool
		confirm  bool
		wantStop bool
	}{
		{"joined member, no data", cluster.NodeState{EtcdState: "existing", EtcdInitialCluster: multi}, false, false, true},
		{"seed that recorded growth, no data", cluster.NodeState{EtcdState: "new", EtcdInitialCluster: single, EtcdHasPeers: true}, false, false, true},
		{"explicit override", cluster.NodeState{EtcdState: "existing", EtcdInitialCluster: multi}, false, true, false},
		{"member with its WAL", cluster.NodeState{EtcdState: "existing", EtcdInitialCluster: multi}, true, false, false},
		{"sole controller replacement", cluster.NodeState{EtcdState: "new", EtcdInitialCluster: single}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			for _, sw := range []struct {
				p *string
				v string
			}{
				{&version.DataDir, filepath.Join(tmp, "data")},
				{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
				{&version.K8sConfDir, filepath.Join(tmp, "config")},
				{&startLockPath, filepath.Join(tmp, "start.lock")},
			} {
				old := *sw.p
				*sw.p = sw.v
				t.Cleanup(func() { *sw.p = old })
			}
			oldInstall, oldRun, oldPrepare := installUnit, runControllerFn, prepareControllerFn
			t.Cleanup(func() { installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRun, oldPrepare })
			installUnit = func([]string) error { return nil }
			runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
			prepared := false
			prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
				prepared = true
				return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
			}

			st := tc.st
			st.Role, st.Hostname, st.EtcdName = "controller", "cp1", "cp1"
			if err := cluster.SaveState(&st); err != nil {
				t.Fatal(err)
			}
			if tc.wal {
				walDir := filepath.Join(version.EtcdDataDir, "member", "wal")
				if err := os.MkdirAll(walDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(walDir, "0000000000000000-0000000000000000.wal"), []byte("wal"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := runUp(context.Background(), runUpOptions{configPath: writeEmptyConfig(t, tmp), confirmEtcdQuorumLost: tc.confirm})
			if tc.wantStop {
				if err == nil || !strings.Contains(err.Error(), "etcd") {
					t.Fatalf("runUp = %v, want a refusal naming the missing etcd data", err)
				}
				if prepared {
					t.Error("the node was prepared (and would start etcd) before the refusal")
				}
				return
			}
			if err != nil || !prepared {
				t.Fatalf("runUp = %v, prepared = %v; want a normal start", err, prepared)
			}
		})
	}
}

// TestRunUpRefusesToBootstrapWhenAJoinIntentSurvives pins the bare-start side
// of the join's resume. A failed `stratum up --join` can leave the join's
// intent journal on disk with no node.yaml beside it — the state save never
// ran. Bare `stratum up` on that node looks fresh: no saved state, an empty
// etcd data directory — and would bootstrap a separate single-member cluster
// while the joined cluster still lists this node as its member. The start
// refuses and names the resume path instead; without an intent journal the
// same bare start is the ordinary fresh bring-up and still works.
func TestRunUpRefusesToBootstrapWhenAJoinIntentSurvives(t *testing.T) {
	for _, tc := range []struct {
		name     string
		intent   bool
		wantStop bool
	}{
		{"an intent journal with no node state", true, true},
		{"no intent journal: the ordinary fresh bring-up", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			for _, sw := range []struct {
				p *string
				v string
			}{
				{&version.DataDir, filepath.Join(tmp, "data")},
				{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
				{&version.K8sConfDir, filepath.Join(tmp, "config")},
				{&startLockPath, filepath.Join(tmp, "start.lock")},
			} {
				old := *sw.p
				*sw.p = sw.v
				t.Cleanup(func() { *sw.p = old })
			}
			if tc.intent {
				// The journal a failed join leaves beside where node.yaml would
				// be: a member was added to the cluster, and this node never
				// saved its state. Its shape is the on-disk contract pinned in
				// internal/ha/join_test.go.
				intent := `{"memberName":"cp2","peerURL":"https://192.0.2.9:2380","memberID":1234,"phase":"added"}`
				if err := os.MkdirAll(version.K8sConfDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(version.K8sConfDir, "join-intent.json"), []byte(intent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			oldInstall, oldRun, oldPrepare := installUnit, runControllerFn, prepareControllerFn
			t.Cleanup(func() { installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRun, oldPrepare })
			installUnit = func([]string) error { return nil }
			runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
			prepared := false
			prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
				prepared = true
				return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
			}

			err := runUp(context.Background(), runUpOptions{configPath: writeEmptyConfig(t, tmp)})
			if tc.wantStop {
				if err == nil {
					t.Fatal("bare `stratum up` bootstrapped a node whose join intent survives")
				}
				if !strings.Contains(err.Error(), "intent") || !strings.Contains(err.Error(), "--join") {
					t.Errorf("the refusal does not name the intent file and the resume path: %v", err)
				}
				if prepared {
					t.Error("the node was prepared (a separate single-member cluster) before the refusal")
				}
				return
			}
			if err != nil || !prepared {
				t.Fatalf("runUp = %v, prepared = %v; want the ordinary fresh bring-up", err, prepared)
			}
		})
	}
}

// TestRunUpEndsTheJoinJournalOnceLocalEtcdDataIsConfirmed pins when the join's
// recovery journal ends. The join keeps the journal across its durable state
// save, so the crash window between that save and etcd's first WAL — a state
// bare `stratum up` refuses (requireLocalEtcdMember) — keeps its recovery
// record. Once a start has confirmed the local member data, the journal has
// done its job and ends; a start the gate refuses keeps it.
func TestRunUpEndsTheJoinJournalOnceLocalEtcdDataIsConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wal         bool
		wantErr     string
		wantJournal bool
	}{
		{"the WAL gate confirms the member data: the journal ends", true, "", false},
		{"no WAL yet: the refusal keeps the journal", false, "WAL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			for _, sw := range []struct {
				p *string
				v string
			}{
				{&version.DataDir, filepath.Join(tmp, "data")},
				{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
				{&version.K8sConfDir, filepath.Join(tmp, "config")},
				{&startLockPath, filepath.Join(tmp, "start.lock")},
			} {
				old := *sw.p
				*sw.p = sw.v
				t.Cleanup(func() { *sw.p = old })
			}
			// A node that finished its join and crashed before etcd first ran:
			// published state, the join's recovery journal, no local etcd data.
			st := &cluster.NodeState{Role: "controller", Hostname: "cp2", EtcdName: "cp2",
				EtcdState:          "existing",
				EtcdInitialCluster: "cp1=https://192.0.2.1:2380,cp2=https://192.0.2.9:2380"}
			if err := cluster.SaveState(st); err != nil {
				t.Fatal(err)
			}
			intent := `{"memberName":"cp2","peerURL":"https://192.0.2.9:2380","memberID":1234,"phase":"added"}`
			if err := os.WriteFile(filepath.Join(version.K8sConfDir, "join-intent.json"), []byte(intent), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.wal {
				walDir := filepath.Join(version.EtcdDataDir, "member", "wal")
				if err := os.MkdirAll(walDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(walDir, "0000000000000000-0000000000000000.wal"), []byte("wal"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			oldInstall, oldRun, oldPrepare := installUnit, runControllerFn, prepareControllerFn
			t.Cleanup(func() { installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRun, oldPrepare })
			installUnit = func([]string) error { return nil }
			runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
			prepared := false
			prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
				prepared = true
				return st, nil
			}

			err := runUp(context.Background(), runUpOptions{configPath: writeEmptyConfig(t, tmp)})
			_, statErr := os.Stat(filepath.Join(version.K8sConfDir, "join-intent.json"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("runUp = %v; want a refusal naming %q", err, tc.wantErr)
				}
				if prepared {
					t.Error("the node was prepared although its etcd data was missing")
				}
			} else if err != nil || !prepared {
				t.Fatalf("runUp = %v, prepared = %v; want the member restart", err, prepared)
			}
			if tc.wantJournal {
				if statErr != nil {
					t.Fatalf("the refused start lost the journal: %v", statErr)
				}
				return
			}
			if statErr == nil {
				t.Fatal("the confirmed member restart left the join journal behind")
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("inspect the journal after the restart: %v", statErr)
			}
		})
	}
}

func TestPublishedJoinStateWithIntentResumesOnlyBeforeEtcdStarts(t *testing.T) {
	pin := "sha256:" + strings.Repeat("a", 64)
	st := &cluster.NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "existing",
		EtcdInitialCluster: "cp1=https://192.0.2.1:2380,cp2=https://192.0.2.2:2380"}
	for _, tc := range []struct {
		name                         string
		intent, retained, wantMember bool
	}{
		{"published state with pending intent", true, false, false},
		{"etcd has already started", true, true, true},
		{"completed join", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.retained {
				if err := os.Mkdir(filepath.Join(dir, "member"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := alreadyJoinedWithIntentDecision(st, nil, func() []string { return []string{pin} }, pin, tc.intent, dir)
			if err != nil || got != tc.wantMember {
				t.Fatalf("member restart=%v error=%v, want %v", got, err, tc.wantMember)
			}
		})
	}
}

// writePayload writes a minimal self-extracting payload (a ZIP with one bin/
// member) that assets.StageFrom accepts.
func writePayload(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "stratum")
	f, err := os.Create(exe)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("bin/containerd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestNodeUmaskReachesStageThenUp: a hardened caller umask (027, 077) must not
// reach the directories `stratum stage` or `stratum up` create, or non-root
// containers cannot traverse them. `umask 077; stratum stage;
// stratum up` is the sequence that slipped past a pin placed only in runUp:
// stage created the staging root 0700 and the later up inherited it.
func TestNodeUmaskReachesStageThenUp(t *testing.T) {
	root := NewRootCommand()
	stage, _, err := root.Find([]string{"stage"})
	if err != nil {
		t.Fatal(err)
	}
	up, _, err := root.Find([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	exe := writePayload(t)

	for _, caller := range []int{0o027, 0o077} {
		t.Run(fmt.Sprintf("caller %04o", caller), func(t *testing.T) {
			old := syscall.Umask(caller)
			defer syscall.Umask(old)

			// stage: the root and bin/ must come out 0755 under the real
			// extraction code, through the real root pre-run.
			stateRoot := filepath.Join(t.TempDir(), "var", "lib", "stratum")
			root.PersistentPreRun(stage, nil)
			if _, err := assets.StageFrom(exe, stateRoot); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{stateRoot, filepath.Join(stateRoot, assets.BinDir)} {
				if got := statMode(t, d); got != 0o755 {
					t.Errorf("after stage: %s = %04o, want 0755", d, got)
				}
			}

			// up: a separate invocation, so it starts from the caller's umask
			// again, not the 022 the stage hook left behind. Without the
			// reset, `up` losing its annotation would still pass here.
			// State it creates is 0755 too, and an explicit 0700 (PKI,
			// secrets) is left alone.
			syscall.Umask(caller)
			root.PersistentPreRun(up, nil)
			kubelet := filepath.Join(stateRoot, "kubelet")
			if err := os.MkdirAll(kubelet, 0o755); err != nil {
				t.Fatal(err)
			}
			pki := filepath.Join(stateRoot, "pki")
			if err := os.Mkdir(pki, 0o700); err != nil {
				t.Fatal(err)
			}
			if got := statMode(t, kubelet); got != 0o755 {
				t.Errorf("after up: %s = %04o, want 0755", kubelet, got)
			}
			if got := statMode(t, pki); got != 0o700 {
				t.Errorf("explicit 0700 dir came out %04o", got)
			}
		})
	}
}

// TestNodeUmaskNotAppliedRootWide: commands that do not own node state keep
// the caller's umask — `snapshot restore` downloads a secret-bearing file with
// os.Create (0666) and depends on it to come out 0600 under 077.
func TestNodeUmaskNotAppliedRootWide(t *testing.T) {
	root := NewRootCommand()
	for _, path := range [][]string{{"which"}, {"snapshot", "save"}, {"snapshot", "rejoin"}, {"upgrade"}, {"version"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		old := syscall.Umask(0o077)
		root.PersistentPreRun(cmd, nil)
		got := syscall.Umask(old)
		if got != 0o077 {
			t.Errorf("%v changed the umask to %04o", path, got)
		}
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestNodeUmaskHookFiresOnExecute: the pre-run is wired into the real cobra
// dispatch, not just callable. The test binary carries no payload, so stage
// fails with ErrNoPayload — after the hook has run.
func TestNodeUmaskHookFiresOnExecute(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	root := NewRootCommand()
	root.SetArgs([]string{"stage", "--root", t.TempDir()})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	_ = root.Execute()
	if got := syscall.Umask(0o077); got != nodeUmask {
		t.Fatalf("umask after `stage` = %04o, want %04o", got, nodeUmask)
	}
}

// From here down: the signal handling tests for runUp owning its signals from preflight
// (the handlers must exist from before the first preflight, so a signal during
// preparation stops at a step boundary rather than killing the process by the
// default action — for an HA join, mid-join, possibly already
// past the live etcd member-add).

// TestRunUpOwnsSignalsFromPreflight pins the wiring: ONE
// interrupt context created before the first preflight call, no NotifyContext
// handler of runUp's own left in the file, and that same run's context handed
// to both preparation paths (PrepareWorker and the HA join). A source pin
// because the property is WHERE the handler exists relative to runUp's own
// steps; the re-exec children below prove the behaviour that wiring produces.
// The pattern is the house one for wiring a unit test cannot drive
// (TestJoinRefusesAKeylessBundleBeforeTheMemberAdd,
// internal/ha/join_test.go:238).
func TestRunUpOwnsSignalsFromPreflight(t *testing.T) {
	src, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(src, []byte("signal.NotifyContext")); n != 0 {
		t.Fatalf("up.go still installs %d signal handler(s) of its own; the one interrupt context, created before preflight, must be the only handler", n)
	}
	creation := bytes.Index(src, []byte("interrupt.DerivedContext("))
	if creation < 0 {
		creation = bytes.Index(src, []byte("interrupt.Context("))
	}
	if creation < 0 {
		creation = bytes.Index(src, []byte("commandInterruptContext("))
	}
	if creation < 0 {
		t.Fatal("runUp creates no interrupt context (internal/interrupt): preflight, preparation and the supervisor run unowned, so a signal kills the process by the default action")
	}
	preflight := bytes.Index(src, []byte("preflightNoRunningNode()"))
	if preflight < 0 {
		t.Fatal("runUp no longer calls preflightNoRunningNode — re-express this pin")
	}
	if creation > preflight {
		t.Fatal("the interrupt context is created after preflight: a signal arriving during preflight still meets no handler and kills the process")
	}
	// The context must reach the preparation paths: the HA join runs before
	// the supervisor, and it is the one place a kill is worst (a voting member
	// already added to etcd, no node state saved).
	for _, call := range []string{"prepareWorkerFn(", "prepareControlPlaneJoinFn("} {
		i := bytes.Index(src, []byte(call))
		if i < 0 {
			t.Fatalf("runUp no longer calls %s — re-express this pin", call)
		}
		window := src[i:]
		if len(window) > 80 {
			window = window[:80]
		}
		if !bytes.Contains(window, []byte("ctx")) {
			t.Fatalf("%s is called without the run's context: %q", call, window)
		}
	}
}

// TestRunUpStopsWhenTheParentContextIsCancelled pins runUp's side of the
// derivation contract the operator's, mcp's and dash's tests already pin:
// the interrupt context is derived from the command's parent context, so a
// host that runs the tree through ExecuteContext and cancels its own
// context stops `stratum up` — here at the boundary before the run phase,
// before the reboot-persistence unit is installed or the worker started —
// instead of leaving the node running until a signal arrives. The parent's
// cancellation surfaces as a plain context.Canceled, never the signal
// stop error, so it keeps its own meaning and exit code.
func TestRunUpStopsWhenTheParentContextIsCancelled(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}

	oldJoin, oldRun, oldInstall := prepareWorkerFn, runWorkerFn, installUnit
	t.Cleanup(func() { prepareWorkerFn, runWorkerFn, installUnit = oldJoin, oldRun, oldInstall })
	prepareWorkerFn = func(context.Context, cluster.WorkerOptions) (*cluster.NodeState, error) {
		return &cluster.NodeState{Role: "worker", Hostname: "w1", Server: "https://10.0.0.1:6443"}, nil
	}
	var ran, installed bool
	runWorkerFn = func(context.Context, *cluster.NodeState) error { ran = true; return nil }
	installUnit = func([]string) error { installed = true; return nil }

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	err := runUp(parent, runUpOptions{worker: true})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("runUp with a cancelled parent context = %v, want the parent's cancellation", err)
	}
	if ran {
		t.Error("the worker run phase started although the parent context was cancelled")
	}
	if installed {
		t.Error("the reboot-persistence unit was installed although the parent context was cancelled")
	}
}

// upScratchHost points runUp's host writes, locks and node state at a scratch
// tree, so a re-exec child can run the real runUp — worker or controller role
// — without touching this machine. The redirections are the ones
// TestRunUpInstallsTheSteadyStateUnitOnWorkerRestart uses.
func upScratchHost(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
}

// waitForLine blocks until the child prints a stdout line containing substr —
// the barrier that a given runUp phase has begun, so a signal cannot race
// ahead of the phase it is meant to interrupt.
func (p *cliProc) waitForLine(t *testing.T, substr string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatalf("child exited before printing %q", substr)
			}
			if strings.Contains(l, substr) {
				return
			}
		case <-deadline:
			t.Fatalf("child never printed %q", substr)
		}
	}
}

// childLinesFrom blocks until the child's stdout closes, returning every line
// printed from now on plus the exit code the child printed it mapped its
// result to. Unlike cliProc.waitForExitCode (pkg/cli/operator_test.go), which
// discards the lines it reads past, this keeps them: the lines a stopped run
// prints BEFORE its verdict — "stratum: stopped", the phases it reached — are
// exactly what these pins assert on.
func childLinesFrom(t *testing.T, p *cliProc) (code int, lines []string) {
	t.Helper()
	code = -1
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				if code < 0 {
					t.Fatal("the child exited without printing its exit-code verdict — a signal killed it before the run could stop by itself, the un-owned case these tests catch")
				}
				return code, lines
			}
			lines = append(lines, l)
			if strings.HasPrefix(l, "exit code: ") {
				if c, err := strconv.Atoi(strings.TrimPrefix(l, "exit code: ")); err == nil {
					code = c
				}
			}
		case <-deadline:
			t.Fatal("the child did not come to a stop within 10s")
		}
	}
}

// waitChildExitsByItself waits for the child to end and fails unless it
// terminated by itself — never killed by a signal. A stop at a step boundary
// is an exit; a kill is the default action meeting an unowned run.
func waitChildExitsByItself(t *testing.T, p *cliProc) int {
	t.Helper()
	select {
	case err := <-p.waited:
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		if got := ee.ExitCode(); got == -1 {
			t.Fatal("the child was killed by a signal: the run must stop by itself at a step boundary, not die mid-preparation")
		} else {
			return got
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the signaled run did not come to a stop within 10s")
	}
	return -1
}

// waitChildKilledBySignal waits for the child to end and fails unless a signal
// killed it — what the second signal owes an operator whose first one is stuck
// in a step that has not reached its boundary.
func waitChildKilledBySignal(t *testing.T, p *cliProc) {
	t.Helper()
	select {
	case err := <-p.waited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		if ee.ExitCode() != -1 {
			t.Fatalf("the second signal left the child exiting by itself (code %d); the default action must be back so a second signal kills", ee.ExitCode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not kill the child within 5s")
	}
}

// TestRunUpWorkerJoinStopsAtTheBoundaryOnASignal is the worker-role red: the
// interrupt context must exist before preparation, so a signal during the
// join stops the run at its step boundary — the worker is never started and
// the reboot-persistence unit is not installed — instead of killing the
// process. The whole shared set
// is covered: SIGINT, SIGTERM and SIGHUP.
//
// The child runs the real runUp with prepareWorkerFn stubbed as a preparation
// step sitting at its boundary: it holds until the context is cancelled and
// returns that stop — exactly what the threaded context is for. The stop
// exits 0 (cleanStopOr): an operator who interrupts a join asked for a
// stop, the same as one interrupting a running node, and a stop before
// start must not surface as a failed run either.
func TestRunUpWorkerJoinStopsAtTheBoundaryOnASignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("STRATUM_UP_WJOIN_STOP_CHILD") == "1" {
				upScratchHost(t)
				installUnit = func([]string) error { fmt.Println("unit-installed"); return nil }
				runWorkerFn = func(ctx context.Context, _ *cluster.NodeState) error {
					fmt.Println("worker-started")
					<-ctx.Done()
					return ctx.Err()
				}
				prepareWorkerFn = func(ctx context.Context, _ cluster.WorkerOptions) (*cluster.NodeState, error) {
					fmt.Println("join-entered")
					<-ctx.Done()
					return nil, ctx.Err()
				}
				code := exitAsMainMaps(runUp(context.Background(), runUpOptions{worker: true}))
				fmt.Printf("exit code: %d\n", code)
				os.Exit(code)
			}
			p := startCLIChild(t, "STRATUM_UP_WJOIN_STOP_CHILD", "TestRunUpWorkerJoinStopsAtTheBoundaryOnASignal")
			p.waitForLine(t, "join-entered")
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			code, out := childLinesFrom(t, p)
			if code != 0 {
				t.Errorf("the stopped run exits %d, want 0: a signal-asked stop at the boundary is a clean stop", code)
			}
			waitChildExitsByItself(t, p)
			for _, banned := range []string{"unit-installed", "worker-started"} {
				for _, l := range out {
					if strings.Contains(l, banned) {
						t.Errorf("after the signal the run went on and reached %s (%q): it must stop at the boundary", banned, l)
					}
				}
			}
		})
	}
}

// TestRunUpWorkerJoinSurvivesTheFirstSignalAndDiesToTheSecond pins the
// second-signal rule during preparation: the first signal only stops the run
// (the shared interrupt context cancels and restores the signals' default
// action, internal/interrupt/interrupt.go:93-95), and because a step can take
// its time reaching its boundary, the second signal must kill. Fails if
// the first signal kills instead: a handler installed only after the
// worker setup would leave the run to die by the default action rather
// than stopping at its boundary.
//
// The child's join step holds blindly for three seconds past the barrier — a
// step mid-flight, like a real cert download — so the first signal cannot end
// the run by itself and only the restored default action can.
func TestRunUpWorkerJoinSurvivesTheFirstSignalAndDiesToTheSecond(t *testing.T) {
	if os.Getenv("STRATUM_UP_WJOIN_KILL_CHILD") == "1" {
		upScratchHost(t)
		installUnit = func([]string) error { fmt.Println("unit-installed"); return nil }
		runWorkerFn = func(ctx context.Context, _ *cluster.NodeState) error {
			fmt.Println("worker-started")
			<-ctx.Done()
			return ctx.Err()
		}
		prepareWorkerFn = func(ctx context.Context, _ cluster.WorkerOptions) (*cluster.NodeState, error) {
			fmt.Println("join-entered")
			time.Sleep(3 * time.Second)
			return nil, ctx.Err()
		}
		code := exitAsMainMaps(runUp(context.Background(), runUpOptions{worker: true}))
		fmt.Printf("exit code: %d\n", code)
		os.Exit(code)
	}
	p := startCLIChild(t, "STRATUM_UP_WJOIN_KILL_CHILD", "TestRunUpWorkerJoinSurvivesTheFirstSignalAndDiesToTheSecond")
	p.waitForLine(t, "join-entered")
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.waited:
		t.Fatalf("one SIGINT ended the run (%v): the first signal must stop it, not kill it", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitChildKilledBySignal(t, p)
}

// TestRunUpControllerPreparationStopsOnASignalInsteadOfDying is the
// controller-role red: the HA join and the controller preparation must run
// under a handler, so a signal during preparation stops `stratum up` mid-join
// — possibly past the live etcd member-add — instead of killing it. The
// process survives the signal (the context exists from preflight) and
// comes to a stop by itself. The prepare stub sleeps past the signal like
// the real host preparation does, which takes no context.
func TestRunUpControllerPreparationStopsOnASignalInsteadOfDying(t *testing.T) {
	if os.Getenv("STRATUM_UP_CPREP_CHILD") == "1" {
		upScratchHost(t)
		installUnit = func([]string) error { fmt.Println("unit-installed"); return nil }
		runControllerFn = func(ctx context.Context, _ *cluster.ClusterConfig, _ *cluster.NodeState) error {
			fmt.Println("controller-started")
			<-ctx.Done()
			return ctx.Err()
		}
		prepareControllerFn = func(_ context.Context, _ *cluster.ClusterConfig, _ string, _ *k8sutil.MaintenanceLease, _ func()) (*cluster.NodeState, error) {
			fmt.Println("prepare-entered")
			time.Sleep(time.Second)
			return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
		}
		code := exitAsMainMaps(runUp(context.Background(), runUpOptions{configPath: writeEmptyConfig(t, t.TempDir())}))
		fmt.Printf("exit code: %d\n", code)
		os.Exit(code)
	}
	p := startCLIChild(t, "STRATUM_UP_CPREP_CHILD", "TestRunUpControllerPreparationStopsOnASignalInsteadOfDying")
	p.waitForLine(t, "prepare-entered")
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitChildExitsByItself(t, p)
}

// TestRunUpWorkerRunStopsCleanlyOnASignal guards the behaviour the move must
// keep: once the worker is RUNNING, a signal in the shared set stops it as a
// clean stop — "stratum: stopped", exit 0 — which the old late handler
// already provided after the worker setup. Moving the context ahead of preflight
// must not lose the filter that maps the stop to a clean exit, nor hand the
// run phase a context the signals cannot reach (the stub waits on it; a
// detached context hangs the child into the deadline).
func TestRunUpWorkerRunStopsCleanlyOnASignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("STRATUM_UP_WRUN_CHILD") == "1" {
				upScratchHost(t)
				installUnit = func([]string) error { fmt.Println("unit-installed"); return nil }
				prepareWorkerFn = func(context.Context, cluster.WorkerOptions) (*cluster.NodeState, error) {
					return &cluster.NodeState{Role: "worker", Hostname: "w1", Server: "https://10.0.0.1:6443"}, nil
				}
				runWorkerFn = func(ctx context.Context, _ *cluster.NodeState) error {
					fmt.Println("worker-started")
					<-ctx.Done()
					return ctx.Err()
				}
				code := exitAsMainMaps(runUp(context.Background(), runUpOptions{worker: true}))
				fmt.Printf("exit code: %d\n", code)
				os.Exit(code)
			}
			p := startCLIChild(t, "STRATUM_UP_WRUN_CHILD", "TestRunUpWorkerRunStopsCleanlyOnASignal")
			p.waitForLine(t, "worker-started")
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			code, out := childLinesFrom(t, p)
			if code != 0 {
				t.Fatalf("a signal-caused stop of the RUNNING worker maps to exit %d, want 0 (a clean stop)", code)
			}
			waitChildExitsByItself(t, p)
			stopped := false
			for _, l := range out {
				if strings.Contains(l, "stratum: stopped") {
					stopped = true
				}
			}
			if !stopped {
				t.Error("the clean stop never printed \"stratum: stopped\"")
			}
			for _, l := range p.stderrSnapshot(t) {
				if strings.HasPrefix(l, "stratum: ") {
					t.Errorf("the signal stop was logged as a failed exit: %q", l)
				}
			}
		})
	}
}

// assertStampRefusal checks an unstamped-build refusal says what happened,
// which stamp is missing, and that the value is the unstamped default: the
// binary carries no engine link-time stamps, the Kubernetes version stamp
// among them, and it reads "unknown". An error that merely fails for another
// reason (a missing kubeconfig, a cluster connection) is not the refusal.
func assertStampRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the command ran on an unstamped build (version.K8sVersion \"unknown\") without refusing")
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"stamp", "kubernetes", "unknown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the unstamped-build refusal does not mention %q, so it does not name the missing stamp: %v", want, err)
		}
	}
}

// TestUpAndUpgradeRefuseAnUnstampedBuild pins the stamp guard: a binary
// linked without the engine's -X stamp list carries "unknown" stamps
// (internal/version), and running a node from it renders unpullable :unknown
// image references and would roll a cluster toward Kubernetes version
// "unknown". `stratum up` and `stratum upgrade --cluster` must refuse with a
// clear error naming the missing stamp BEFORE they touch the host or the
// cluster, and the exported read-only accessor must hand out the stamped
// values (its second case lives in root_test.go).
//
// Fails when neither command checks the stamp: `up` would run to completion
// against the stubbed seams, and `upgrade --cluster` would fail only on
// its missing --from, an error that nowhere mentions the stamp.
func TestUpAndUpgradeRefuseAnUnstampedBuild(t *testing.T) {
	old := version.K8sVersion
	version.K8sVersion = "unknown"
	t.Cleanup(func() { version.K8sVersion = old })

	t.Run("stratum up refuses before it touches the host", func(t *testing.T) {
		upScratchHost(t)
		oldInstall, oldRun, oldPrepare := installUnit, runControllerFn, prepareControllerFn
		t.Cleanup(func() { installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRun, oldPrepare })
		installUnit = func([]string) error { return nil }
		runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
		prepared := false
		prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
			prepared = true
			return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
		}

		root := NewRootCommand()
		root.SetArgs([]string{"up", "--config", writeEmptyConfig(t, t.TempDir())})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		err := root.Execute()

		assertStampRefusal(t, err)
		if prepared {
			t.Error("the node was prepared (PKI, manifests, etcd state) before the unstamped refusal")
		}
		if _, statErr := os.Stat(version.DataDir); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the unstamped start wrote under %s before refusing: %v", version.DataDir, statErr)
		}
	})

	t.Run("stratum upgrade --cluster refuses before it touches the cluster", func(t *testing.T) {
		// Hermetic even while the refusal is missing: --from satisfies the
		// orchestrator's first precondition, and the admin kubeconfig points
		// into a temp dir, so the red path stops at the client build.
		oldAdmin := version.AdminConf
		version.AdminConf = filepath.Join(t.TempDir(), "admin.conf")
		t.Cleanup(func() { version.AdminConf = oldAdmin })

		root := NewRootCommand()
		root.SetArgs([]string{"upgrade", "--cluster", "--yes",
			"--from", filepath.Join(t.TempDir(), "stratum-next")})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		err := root.Execute()

		assertStampRefusal(t, err)
	})
}

// TestRunUpControllerRunStopsCleanlyOnASignal is the controller sibling of
// TestRunUpWorkerRunStopsCleanlyOnASignal: the run-phase clean stop the old
// late handler already gave must survive the move — one context, from
// preflight, through preparation into RunController — on the whole shared
// set, SIGHUP included: a dropped SSH session must not kill a running
// control plane when one signal's worth of asking suffices.
func TestRunUpControllerRunStopsCleanlyOnASignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("STRATUM_UP_CRUN_CHILD") == "1" {
				upScratchHost(t)
				installUnit = func([]string) error { fmt.Println("unit-installed"); return nil }
				prepareControllerFn = func(_ context.Context, _ *cluster.ClusterConfig, _ string, _ *k8sutil.MaintenanceLease, _ func()) (*cluster.NodeState, error) {
					return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
				}
				runControllerFn = func(ctx context.Context, _ *cluster.ClusterConfig, _ *cluster.NodeState) error {
					fmt.Println("controller-started")
					<-ctx.Done()
					return ctx.Err()
				}
				code := exitAsMainMaps(runUp(context.Background(), runUpOptions{configPath: writeEmptyConfig(t, t.TempDir())}))
				fmt.Printf("exit code: %d\n", code)
				os.Exit(code)
			}
			p := startCLIChild(t, "STRATUM_UP_CRUN_CHILD", "TestRunUpControllerRunStopsCleanlyOnASignal")
			p.waitForLine(t, "controller-started")
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			code, out := childLinesFrom(t, p)
			if code != 0 {
				t.Fatalf("a %s-caused stop of the RUNNING control plane maps to exit %d, want 0 (a clean stop)", tc.name, code)
			}
			waitChildExitsByItself(t, p)
			stopped := false
			for _, l := range out {
				if strings.Contains(l, "stratum: stopped") {
					stopped = true
				}
			}
			if !stopped {
				t.Error("the clean stop never printed \"stratum: stopped\"")
			}
			for _, l := range p.stderrSnapshot(t) {
				if strings.HasPrefix(l, "stratum: ") {
					t.Errorf("the signal stop was logged as a failed exit: %q", l)
				}
			}
		})
	}
}

// TestUpCISRefusesWithTheCisNoteWhenNoProfileIsRegistered: a cis
// node's restart unit runs `up --cis` (cluster.UpArgv), so on a binary
// that registers no profile the flag must still parse, and what the start
// gets back is the profile refusal carrying the cis note, not cobra's
// unknown-flag usage error, before anything on the host is touched.
func TestUpCISRefusesWithTheCisNoteWhenNoProfileIsRegistered(t *testing.T) {
	upScratchHost(t)
	old := version.K8sVersion
	version.K8sVersion = "v1.37.1"
	t.Cleanup(func() { version.K8sVersion = old; _ = profile.SetRegistered(nil) })
	oldPrepare := prepareControllerFn
	t.Cleanup(func() { prepareControllerFn = oldPrepare })
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		t.Fatal("prepare ran although the cis profile is not registered")
		return nil, nil
	}

	root := NewRootCommand()
	root.SetArgs([]string{"up", "--cis", "--config", writeEmptyConfig(t, t.TempDir())})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	if err == nil {
		t.Fatal("up --cis ran on a binary with no profile registered without refusing")
	}
	var usage *ErrUsage
	if errors.As(err, &usage) || strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("up --cis failed to parse: %v; the restart unit writes that flag", err)
	}
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Errorf("up --cis = %v, want it to wrap profile.ErrUnregistered", err)
	}
	for _, want := range []string{profile.CISUnregisteredNote, "nothing on this host was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("up --cis refusal is missing %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(version.DataDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused start wrote under %s: %v", version.DataDir, statErr)
	}
}
