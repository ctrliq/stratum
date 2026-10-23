package uninstall

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// The tests below pin per-run discovery: the probes, the discovered namespace
// state (stratumNetns, stratumNetnsErr and netnsEnumerated, with the netns
// record paths and the filesystem layout reads) are per-run values carried
// alongside the effects value, and the remaining shared mutation is gone, so
// two Runs in one process cannot race over it. They replace
// TestRunResetsSharedDiscoveryBeforeTheHostLock, which pinned the shared-state
// window these per-run values remove: a second Run can no longer clear or
// inherit what a first one discovered, because no run's discovery is shared.
// The names asserted here are the contract the implementation builds against.

// errStopAtTheHostLock stops a run at the etcd data lock, the first gate the
// forced path reaches, so the runs below exercise the discovery reset and the
// entry path and no teardown step after it.
var errStopAtTheHostLock = errors.New("stop at the host lock before any teardown")

// TestRunDiscoveryIsItsOwn: what one invocation learned about the node's
// network namespaces is its own. Run can be called again in a persistent
// process (the MCP server), so a run must reach the host lock with discovery
// seeded fresh — never an ownership record, error or enumeration flag a
// previous call left, on this value or another. The earlier call's discovery
// is learned here through the production enumeration path on the same effects
// value, and read back inside the run through the per-run fields.
func TestRunDiscoveryIsItsOwn(t *testing.T) {
	t.Parallel()
	fx := &hostEffects{}
	// One container whose spec names a namespace outside the netns directory,
	// so the enumeration records nothing to save and no record file is written:
	// what it leaves behind is exactly the per-run state a later call must not
	// inherit, the enumerated flag above all.
	fx.ctrRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "-n k8s.io containers ls -q":
			return "c1\n", nil
		case "-n k8s.io containers info c1":
			return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"/etc/netns/elsewhere"}]}}}`, nil
		case "-n k8s.io sandboxes ls":
			return "ID CREATED RUNTIME\n", nil
		}
		return "", errors.New("unexpected ctr call: " + strings.Join(args, " "))
	}
	if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
		t.Fatalf("the enumeration a previous call stands in for: %v", err)
	}
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) {
		run := reflect.ValueOf(fx).Elem()
		discovered := run.FieldByName("stratumNetns")
		if !discovered.IsValid() {
			t.Fatal("hostEffects has no stratumNetns field; the discovered namespace state is not a per-run value carried alongside the effects value")
		}
		if discovered.Len() != 0 {
			t.Errorf("the run reached the host lock with %d namespace(s) a previous call discovered; what an earlier call learned is not this run's discovery", discovered.Len())
		}
		if e := run.FieldByName("stratumNetnsErr"); !e.IsValid() || !e.IsNil() {
			t.Error("the run reached the host lock with a previous call's discovery error")
		}
		if en := run.FieldByName("netnsEnumerated"); !en.IsValid() || en.Bool() {
			t.Error("the run reached the host lock with a previous call's enumeration standing in for its own unread containers")
		}
		return nil, errStopAtTheHostLock
	}
	if err := runUninstall(io.Discard, true, fx); !errors.Is(err, errStopAtTheHostLock) {
		t.Errorf("run = %v, want the host-lock stop", err)
	}
}

// TestTwoRunsShareNoDiscovery runs two uninstalls in one process at the same
// time, the way an MCP server can. Each marks itself t.Parallel-eligible, so
// the pair is the representative Run test the -race gate runs: while the
// discovery state is still shared at package level, the two entry resets race
// and this test fails under -race; with per-run values the resets touch only
// each run's own, and the pair passes. Neither call reaches host teardown.
func TestTwoRunsShareNoDiscovery(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var runs sync.WaitGroup
	run := func() {
		defer runs.Done()
		fx := &hostEffects{
			acquireNodeLock: freeNodeLock,
			acquireEtcdDataLock: func() (func(), error) {
				arrived <- struct{}{}
				select {
				case <-release:
				case <-time.After(10 * time.Second):
					t.Error("the other run never reached the host lock; the two runs did not overlap")
				}
				return nil, errStopAtTheHostLock
			},
		}
		if err := runUninstall(io.Discard, true, fx); !errors.Is(err, errStopAtTheHostLock) {
			t.Errorf("run = %v, want the host-lock stop", err)
		}
	}
	runs.Add(2)
	go run()
	go run()
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("a run never reached the host lock")
		}
	}
	close(release)
	runs.Wait()
}

// TestRunDiscoveryIsPinnedInParallel keeps the two Run tests above marked
// t.Parallel: they are the representative parallel Run tests the -race gate
// runs, and a future edit that drops the marker would quietly return the
// package to sequential-only coverage without anything else failing.
func TestRunDiscoveryIsPinnedInParallel(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "uninstall_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse uninstall_test.go: %v", err)
	}
	bodies := map[string]*ast.BlockStmt{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			bodies[fn.Name.Name] = fn.Body
		}
	}
	for _, name := range []string{"TestRunDiscoveryIsItsOwn", "TestTwoRunsShareNoDiscovery"} {
		body := bodies[name]
		if body == nil {
			t.Fatalf("%s is missing; the per-run discovery pins must stay in uninstall_test.go", name)
		}
		parallel := false
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" {
					parallel = true
				}
			}
			return true
		})
		if !parallel {
			t.Errorf("%s never calls t.Parallel; the representative Run tests must run in parallel so -race pins the per-run discovery state", name)
		}
	}
}

// TestHostEffectsCarriesThePerRunValues pins where the probes, the discovered
// namespace state and the filesystem layout reads live: as fields on the one
// hostEffects value, built in Run, so a second run never inherits them and a
// forgotten stub cannot fall back to a package variable. The names are the
// ones the probes and discovery state carry today, so the migration stays
// mechanical and the phase pins keep matching them through the value. Extra
// fields beyond this list are allowed.
func TestHostEffectsCarriesThePerRunValues(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	probes := []string{
		"supervisorRunning",
		"unitActive",
		"supervisorStatus",
		"hostProcessRunning",
		"staticPodActive",
		"staticPodRunning",
		"unitEnabledFn",
		"startInProgress",
		"topLevelPIDs",
		"staticPodExists",
		"readManifestFn",
		"foreignRuntimesFn",
		"loadNodeState",
		"shimPIDs",
	}
	discovered := []string{"stratumNetns", "stratumNetnsErr", "netnsEnumerated"}
	layout := []string{
		"netnsRecordPath",
		"containerdStateDir",
		"netnsDir",
		"ciliumRunDir",
		"bpfRoot",
		"netClassDir",
		"cgroupRoot",
		"legacyKubeletDir",
		"localPathDir",
		"mountInfoPath",
		"procRoot",
	}
	fields := map[string]reflect.StructField{}
	ty := reflect.TypeOf(hostEffects{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		fields[f.Name] = f
	}
	for _, name := range probes {
		f, ok := fields[name]
		if !ok {
			t.Errorf("hostEffects has no %s field; the probe stays a package variable runs share", name)
			continue
		}
		if f.Type.Kind() != reflect.Func {
			t.Errorf("hostEffects.%s is a %s, want a probe function", name, f.Type.Kind())
		}
	}
	for _, name := range discovered {
		if _, ok := fields[name]; !ok {
			t.Errorf("hostEffects has no %s field; the discovered namespace state is not a per-run value", name)
		}
	}
	for _, name := range layout {
		if _, ok := fields[name]; !ok {
			t.Errorf("hostEffects has no %s field; the filesystem layout read stays a package variable runs share", name)
		}
	}
}

// TestPerRunValuesAreNotPackageState parses the package's production files and
// pins the other half of the step: none of the per-run probes, discovery state
// or filesystem layout reads remains a package-level variable, and the
// shared-mutation helper that reset them is gone with them. A variable left
// behind here is state two Runs in one process race over, whatever the effects
// value carries.
func TestPerRunValuesAreNotPackageState(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	perRun := map[string]bool{
		"supervisorRunning":  true,
		"unitActive":         true,
		"supervisorStatus":   true,
		"hostProcessRunning": true,
		"staticPodActive":    true,
		"staticPodRunning":   true,
		"unitEnabledFn":      true,
		"startInProgress":    true,
		"topLevelPIDs":       true,
		"staticPodExists":    true,
		"readManifestFn":     true,
		"foreignRuntimesFn":  true,
		"loadNodeState":      true,
		"shimPIDs":           true,
		"stratumNetns":       true,
		"stratumNetnsErr":    true,
		"netnsEnumerated":    true,
		"netnsRecordPath":    true,
		"containerdStateDir": true,
		"netnsDir":           true,
		"ciliumRunDir":       true,
		"bpfRoot":            true,
		"netClassDir":        true,
		"cgroupRoot":         true,
		"legacyKubeletDir":   true,
		"localPathDir":       true,
		"mountInfoPath":      true,
		"procRoot":           true,
	}
	fset := token.NewFileSet()
	for _, name := range []string{"uninstall.go", "containers.go", "datapath.go", "shims.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					if perRun[id.Name] {
						t.Errorf("%s declares package-level %s; the per-run value must be carried alongside the effects value, not shared between runs", name, id.Name)
					}
				}
			}
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "resetNetnsDiscovery" {
				t.Errorf("%s declares resetNetnsDiscovery; the shared mutation it performed goes with the package-level discovery state it reset", name)
			}
		}
	}
}

// TestTargetsOutsideEveryRootAreListed replaces TestTargetsCoverInstallerLayout.
// It fails when a path internal/version's layout variables or host.Footprint()
// declare lies outside every root uninstall removes — version.DataDir,
// version.ConfDir and version.RunDir, which Targets() deletes wholesale, so
// anything under them is covered implicitly — and is not covered by an
// explicit target of its own, using the same covering rule as
// TestTargetsCoverTheEnvironmentFile: the path is a target, or lies under one.
// The deliberate exceptions — the live stratum binary that uninstall runs
// from and keeps (see stratumBinaryBackups: only its .bak- sets are targets),
// and the footprint's kubelet dir, which the teardown's removeLegacyKubeletDir
// removes under its keep-foreign-state contract instead of the plain delete
// loop — are named in the kept sets so each exception is a decision the test
// can see, not an accident it forgives. The set of version variables is parsed
// from version.go's own declarations and cross-checked both ways against the
// values this test reads, so a new layout variable fails here until the test
// classifies it, and a removed one fails until the test drops it.
func TestTargetsOutsideEveryRootAreListed(t *testing.T) {
	t.Parallel() // host-free: touches only its own values

	// values names every layout variable version.go declares and the value it
	// carries as the tests stand; the guard checks each against Targets().
	values := map[string]string{
		"AdmissionConfigPath":       version.AdmissionConfigPath,
		"AdminConf":                 version.AdminConf,
		"AuditLogDir":               version.AuditLogDir,
		"AuditLogPath":              version.AuditLogPath,
		"AuditPolicyPath":           version.AuditPolicyPath,
		"BinDir":                    version.BinDir,
		"BootstrapKubeconfig":       version.BootstrapKubeconfig,
		"CertsDir":                  version.CertsDir,
		"CiliumSettingsPath":        version.CiliumSettingsPath,
		"ConfDir":                   version.ConfDir,
		"ContainerdConfig":          version.ContainerdConfig,
		"ContainerdConfD":           version.ContainerdConfD,
		"ContainerdRootDir":         version.ContainerdRootDir,
		"ContainerdStateDir":        version.ContainerdStateDir,
		"ControlSocket":             version.ControlSocket,
		"ControllerManagerManifest": version.ControllerManagerManifest,
		"CoreDNSUpstreamDir":        version.CoreDNSUpstreamDir,
		"DataDir":                   version.DataDir,
		"DefaultRoot":               version.DefaultRoot,
		"EncryptionConfigPath":      version.EncryptionConfigPath,
		"EtcdDataDir":               version.EtcdDataDir,
		"HelmCacheDir":              version.HelmCacheDir,
		"ModulesLoadFile":           version.ModulesLoadFile,
		"LegacyHelmCacheDir":        version.LegacyHelmCacheDir,
		"K8sConfDir":                version.K8sConfDir,
		"KubeconfigDir":             version.KubeconfigDir,
		"LogDir":                    version.LogDir,
		"ManifestsDir":              version.ManifestsDir,
		"NetConfigPath":             version.NetConfigPath,
		"NodeUpgradeLockPath":       version.NodeUpgradeLockPath,
		"PKIDir":                    version.PKIDir,
		"RegistriesConfig":          version.RegistriesConfig,
		"RunDir":                    version.RunDir,
		"StageDir":                  version.StageDir,
		"StartLockPath":             version.StartLockPath,
		"StratumInstallPath":        version.StratumInstallPath,
		"SysctlCISConf":             version.SysctlCISConf,
		"SysctlConf":                version.SysctlConf,
		"SystemdUnitPath":           version.SystemdUnitPath,
		"UpgradeStageDir":           version.UpgradeStageDir,
	}

	// The layout variables, parsed from version.go: every name in the var
	// block that declares DataDir, plus ControllerManagerManifest, which is
	// declared on its own.
	fset := token.NewFileSet()
	vf, err := parser.ParseFile(fset, "../version/version.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ../version/version.go: %v", err)
	}
	var parsed []string
	blockFound := false
	for _, d := range vf.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		names := []string{}
		hasDataDir := false
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				names = append(names, n.Name)
				switch n.Name {
				case "DataDir":
					hasDataDir = true
				case "ControllerManagerManifest":
					parsed = append(parsed, n.Name)
				}
			}
		}
		if hasDataDir {
			blockFound = true
			parsed = append(parsed, names...)
		}
	}
	if !blockFound {
		t.Fatal("version.go no longer declares DataDir in a var block; the guard derives the layout variables from it")
	}
	slices.Sort(parsed)
	parsed = slices.Compact(parsed)
	for _, name := range parsed {
		if _, ok := values[name]; !ok {
			t.Errorf("version.go declares layout variable %s, which this test does not classify; add it to the values map and say whether uninstall covers it", name)
		}
	}
	for name := range values {
		if !slices.Contains(parsed, name) {
			t.Errorf("the test pins version.%s, which version.go no longer declares in its layout blocks; drop it from the values map", name)
		}
	}

	roots := []string{version.DataDir, version.ConfDir, version.RunDir}
	targets := Targets()
	covered := func(p string) bool {
		for _, t := range targets {
			if p == t || strings.HasPrefix(p, strings.TrimSuffix(t, "/")+"/") {
				return true
			}
		}
		return false
	}

	// The roots are targets themselves: uninstall removes each wholesale, so a
	// root missing from the delete list would leave the whole tree behind.
	for _, r := range roots {
		if !slices.Contains(targets, r) {
			t.Errorf("Targets() does not list root %q, which uninstall removes wholesale", r)
		}
	}

	// kept excuses version paths outside every root that uninstall
	// deliberately does not remove, each carrying the reason it was decided.
	kept := map[string]string{
		version.StratumInstallPath: "the live binary uninstall runs from; stratumBinaryBackups removes only its .bak- sets",
		// Both locks are deliberately NOT targets (sanctioned by the human): a
		// running or crashed `stratum up` holds the start lock, and pkg/cli's
		// committed TestStartLockSurvivesUninstall pins that a concurrent
		// uninstall must leave it in place, so that process stays excluded
		// while the node is wiped; removing the lock files would break that
		// exclusion. They are runtime transients beside RunDir
		// (version.go: SetRoots derives both as RunDir + "-start.lock"/
		// "-upgrade.lock"), not node state, and /run is tmpfs a reboot clears.
		version.StartLockPath:       "the node-start exclusion lock; pkg/cli's TestStartLockSurvivesUninstall pins it outside every uninstall target so a live `stratum up` stays excluded during a wipe — a runtime transient, not node state",
		version.NodeUpgradeLockPath: "the node-upgrade exclusion lock, derived beside the start lock for the same reason; a runtime transient, not node state",
	}
	for _, name := range parsed {
		p := values[name]
		if covered(p) {
			continue
		}
		if _, excused := kept[p]; excused {
			continue
		}
		if p == "" {
			t.Errorf("version.%s is empty; every layout variable carries the path it names", name)
			continue
		}
		t.Errorf("version.%s = %q lies outside every root uninstall removes (%s, %s, %s) and no target covers it; uninstall would leave it on the node it wipes — name it in dirsAndFiles or excuse it in this test's kept set", name, p, version.DataDir, version.ConfDir, version.RunDir)
	}

	// footprintKept excuses host.Footprint() paths outside every root that
	// uninstall deliberately does not target, each carrying the reason it was
	// decided. The kubelet dir is the one: the datapath teardown's
	// removeLegacyKubeletDir is the directory's only remover, keeping whatever
	// is not Stratum's kubelet's (its stale sockets and stratum-CA-signed cert
	// pairs go), so a co-hosted kubelet's state survives the wipe — a plain
	// delete target would os.RemoveAll the directory right after that teardown
	// promised to keep it (mirrored in host_test's footprint coupling).
	footprintKept := map[string]string{
		host.KubeletDir: "not a delete target: removeLegacyKubeletDir is the directory's only remover, and it keeps whatever is not Stratum's kubelet's",
	}
	for _, p := range host.Footprint() {
		if covered(p) {
			continue
		}
		if _, excused := footprintKept[p]; excused {
			continue
		}
		t.Errorf("host.Footprint() path %q lies outside every root uninstall removes and no target covers it; uninstall would leave a path bring-up writes on the node it wipes — name it in dirsAndFiles or excuse it in this test's footprintKept set", p)
	}
}

// TestTargetsCoverTheEnvironmentFile is a regression test:
// InstallUnit's reboot-persistence unit points systemd's
// EnvironmentFile= at /etc/stratum/stratum.env, an operator-owned file for
// environment-only S3 credentials. This uninstall treats it the
// same as every other operator-owned file already under version.ConfDir
// (config.yaml, registries.yaml): no dedicated entry of its own, just swept
// up wholesale when version.ConfDir itself is removed. The wholesale sweep is
// what covers it: if ConfDir were ever narrowed to a subset of its own
// directory instead of the whole thing, stratum.env would survive an
// uninstall that is supposed to wipe the node's Stratum footprint.
func TestTargetsCoverTheEnvironmentFile(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	envFile := filepath.Join(version.ConfDir, "stratum.env")
	for _, target := range Targets() {
		if envFile == target || strings.HasPrefix(envFile, strings.TrimSuffix(target, "/")+"/") {
			return
		}
	}
	t.Errorf("no target in %q covers %q", Targets(), envFile)
}

// TestTargetsAreAbsolute keeps a relative path from ever sneaking into the
// destructive delete loop.
func TestTargetsAreAbsolute(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for _, p := range Targets() {
		if !strings.HasPrefix(p, "/") {
			t.Errorf("non-absolute uninstall target: %q", p)
		}
	}
}

// TestOrphanRefusal covers the fail-closed gate: uninstall refuses
// whenever it cannot manage still-running control-plane or host components —
// the state where it must not proceed to delete their state.
func TestOrphanRefusal(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for _, tc := range []struct {
		name                string
		supervisor, kubelet bool
		active, host        []string
		wantRefusal         bool
		wantText            []string
	}{
		{"nothing running", false, false, nil, nil, false, nil},
		{"mixed orphans", false, false, []string{"etcd"}, []string{"containerd"}, true, []string{"Do not start another", "containerd", "etcd"}},
		{"supervisor and kubelet running", true, true, []string{"etcd"}, nil, false, nil},
		{"supervisor down with control plane", false, false, []string{"etcd", "kube-apiserver"}, nil, true, []string{"etcd", "start `stratum up`", "Nothing was changed"}},
		{"supervisor down with host process", false, false, nil, []string{"containerd"}, true, []string{"containerd", "Nothing was changed"}},
		{"kubelet down under a live supervisor", true, false, []string{"etcd"}, nil, true, []string{"etcd", "do not start a second `stratum up`", "Nothing was changed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := orphanRefusal(tc.supervisor, tc.kubelet, tc.active, tc.host)
			if (err != nil) != tc.wantRefusal {
				t.Fatalf("orphanRefusal = %v, want refusal %v", err, tc.wantRefusal)
			}
			if !tc.wantRefusal {
				return
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message missing %q: %v", want, err)
				}
			}
		})
	}
}

// TestOrphanPreflightUsesProcessLiveness stubs the probes on a run of its own:
// the preflight reads them from the run's effects value, never a shared var.
func TestOrphanPreflightUsesProcessLiveness(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		supervisorRunning:  func() bool { return false },
		unitActive:         func(string) bool { t.Fatal("kubelet probe ran after a failed supervisor probe"); return false },
		hostProcessRunning: func(string) (bool, error) { return false, nil },
		staticPodActive:    func(string) bool { return false },
		staticPodRunning:   func(name string) (bool, error) { return name == "kube-vip", nil },
	}

	if err := orphanPreflight(fx); err == nil || !strings.Contains(err.Error(), "kube-vip") {
		t.Fatalf("orphanPreflight = %v, want a kube-vip refusal", err)
	}
}

func TestOrphanPreflightRefusesWhenKubeletIsDown(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		supervisorRunning:  func() bool { return true },
		unitActive:         func(name string) bool { return name != "kubelet" },
		hostProcessRunning: func(string) (bool, error) { return false, nil },
		staticPodActive:    func(name string) bool { return name == "etcd" },
		staticPodRunning:   func(string) (bool, error) { return false, nil },
	}

	if err := orphanPreflight(fx); err == nil || !strings.Contains(err.Error(), "etcd") {
		t.Fatalf("orphanPreflight = %v, want an etcd refusal while kubelet is down", err)
	}
}

func TestOrphanPreflightAllowsKubeletToReapControlPlane(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		supervisorRunning: func() bool { return true },
		unitActive:        func(name string) bool { return name == "kubelet" },
		hostProcessRunning: func(string) (bool, error) {
			t.Fatal("host process probe ran while supervisor was up")
			return false, nil
		},
		staticPodActive:  func(name string) bool { return name == "etcd" },
		staticPodRunning: func(string) (bool, error) { return false, nil },
	}

	if err := orphanPreflight(fx); err != nil {
		t.Fatalf("orphanPreflight = %v, want healthy supervisor path to proceed", err)
	}
}

func TestOrphanPreflightRefusesOrphanedHostProcess(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		supervisorRunning:  func() bool { return false },
		unitActive:         func(string) bool { t.Fatal("kubelet probe ran after a failed supervisor probe"); return false },
		hostProcessRunning: func(name string) (bool, error) { return name == "containerd", nil },
		staticPodActive:    func(string) bool { return false },
		staticPodRunning:   func(string) (bool, error) { return false, nil },
	}

	if err := orphanPreflight(fx); err == nil || !strings.Contains(err.Error(), "containerd") {
		t.Fatalf("orphanPreflight = %v, want a containerd refusal", err)
	}
}

func TestOrphanPreflightRefusesUnknownHostProcessState(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		supervisorRunning:  func() bool { return false },
		unitActive:         func(string) bool { t.Fatal("kubelet probe ran after a failed supervisor probe"); return false },
		hostProcessRunning: func(string) (bool, error) { return false, fmt.Errorf("/proc unavailable") },
		staticPodActive:    func(string) bool { return false },
		staticPodRunning:   func(string) (bool, error) { return false, nil },
	}

	if err := orphanPreflight(fx); err == nil || !strings.Contains(err.Error(), "cannot determine") {
		t.Fatalf("orphanPreflight = %v, want a refusal for an unknown process state", err)
	}
}

func TestOrphanPreflightRefusesUnknownStaticPodState(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		supervisorRunning:  func() bool { return false },
		unitActive:         func(string) bool { t.Fatal("kubelet probe ran after a failed supervisor probe"); return false },
		hostProcessRunning: func(string) (bool, error) { return false, nil },
		staticPodActive:    func(string) bool { return false },
		staticPodRunning:   func(string) (bool, error) { return false, fmt.Errorf("/proc unavailable") },
	}

	if err := orphanPreflight(fx); err == nil || !strings.Contains(err.Error(), "cannot determine") {
		t.Fatalf("orphanPreflight = %v, want a refusal for an unknown static-pod state", err)
	}
}

func TestActiveHostProcesses(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		hostProcessRunning: func(name string) (bool, error) { return name == "containerd" || name == "operator", nil },
	}
	got, err := activeHostProcesses(fx)
	if err != nil {
		t.Fatalf("activeHostProcesses() error = %v", err)
	}
	if want := []string{"operator", "containerd"}; !slices.Equal(got, want) {
		t.Errorf("activeHostProcesses() = %v, want %v", got, want)
	}
}

func TestActiveHostProcessesRefusesUnknownState(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		hostProcessRunning: func(string) (bool, error) { return false, fmt.Errorf("/proc unavailable") },
	}
	if _, err := activeHostProcesses(fx); err == nil || !strings.Contains(err.Error(), "cannot determine") {
		t.Fatalf("activeHostProcesses() error = %v, want an unknown-state refusal", err)
	}
}

func TestOrphanedRuntimeShimBlocksTeardown(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		hostProcessRunning: func(name string) (bool, error) { return name == "runtime-shim", nil },
	}
	if err := refuseActiveHostProcesses(fx); err == nil || !strings.Contains(err.Error(), "runtime-shim") {
		t.Fatalf("expected surviving shim refusal, got %v", err)
	}
}

func TestRunRefusesHostProcessBeforeUnmountOrDelete(t *testing.T) {
	fx := &hostEffects{}
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) { return func() {}, nil }
	oldConfig := version.K8sConfDir
	fx.stopHostProcesses = func() error { return nil }
	version.K8sConfDir = t.TempDir()
	stubTeardown(t, fx, nil)
	t.Cleanup(func() {
		version.K8sConfDir = oldConfig
	})

	fx.supervisorRunning = func() bool { return true }
	fx.unitActive = func(name string) bool { return name == "kubelet" }
	fx.hostProcessRunning = func(name string) (bool, error) { return name == "containerd", nil }
	fx.staticPodActive = func(string) bool { return false }
	fx.staticPodRunning = func(string) (bool, error) { return false, nil }
	fx.unmountStateFn = func() error { t.Fatal("unmount ran while a host process was live"); return nil }
	fx.removeAll = func(string) error { t.Fatal("target deletion ran while a host process was live"); return nil }

	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "containerd") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want live host-process refusal", err)
	}
}

func TestRunRefusesShimOrFailedStop(t *testing.T) {
	fx := &hostEffects{}
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) { return func() {}, nil }
	for _, failedStop := range []bool{false, true} {
		t.Run(fmt.Sprintf("failedStop=%v", failedStop), func(t *testing.T) {
			oldConfig := version.K8sConfDir
			t.Cleanup(func() {
				version.K8sConfDir = oldConfig
			})
			version.K8sConfDir = t.TempDir()
			stubTeardown(t, fx, nil)
			fx.supervisorRunning = func() bool { return true }
			fx.unitActive = func(string) bool { return true }
			fx.staticPodActive = func(string) bool { return false }
			fx.staticPodRunning = func(string) (bool, error) { return false, nil }
			stopRequested := false
			fx.hostProcessRunning = func(name string) (bool, error) { return stopRequested && !failedStop && name == "runtime-shim", nil }
			fx.stopHostProcesses = func() error {
				stopRequested = true
				if failedStop {
					return fmt.Errorf("lost stop acknowledgement")
				}
				return nil
			}
			fx.unmountStateFn = func() error { t.Fatal("unsafe unmount"); return nil }
			fx.removeAll = func(string) error { t.Fatal("unsafe deletion"); return nil }
			err := runUninstall(io.Discard, true, fx)
			if !stopRequested {
				t.Fatal("test did not reach host shutdown")
			}
			want := "runtime-shim"
			if failedStop {
				want = "lost stop acknowledgement"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("runUninstall = %v, want %s", err, want)
			}
		})
	}
}

// freeNodeLock is a node maintenance lock no other operation holds.
func freeNodeLock() (func(), error) { return func() {}, nil }

// TestRunRefusesBesideANodeMaintenanceOperation: an uninstall run beside a CA
// rotation on the same node removed the PKI and state the rotation was
// changing, and the rotation carried on, renewing its cluster leases. The node
// maintenance lock, held by the other operation, refuses the run before the
// data lock and before anything is stopped.
func TestRunRefusesBesideANodeMaintenanceOperation(t *testing.T) {
	swap(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "stratum-upgrade.lock"))
	releaseRotation, err := nodelock.Acquire("certs rotate-ca")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRotation()
	fx := &hostEffects{}
	fx.acquireNodeLock = func() (func(), error) { return nodelock.Acquire("uninstall") }
	fx.acquireEtcdDataLock = func() (func(), error) {
		t.Fatal("took the etcd data lock beside a running maintenance operation")
		return nil, nil
	}
	fx.stopHostProcesses = func() error { t.Fatal("stopped processes beside a running maintenance operation"); return nil }
	err = runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "certs rotate-ca is already running on this node") ||
		!strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("runUninstall = %v, want the node lock's refusal naming the rotation", err)
	}
}

// TestRunTakesTheNodeLockBeforeTheDataLock: the node lock first, the order
// rejoin and restore take them in, and both held until the run ends; a data
// lock that refuses gives the node lock back.
func TestRunTakesTheNodeLockBeforeTheDataLock(t *testing.T) {
	t.Run("both are held until the run ends", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		fx.acquireNodeLock = func() (func(), error) {
			*events = append(*events, "node lock")
			return func() { *events = append(*events, "release node lock") }, nil
		}
		fx.acquireEtcdDataLock = func() (func(), error) {
			*events = append(*events, "data lock")
			return func() { *events = append(*events, "release data lock") }, nil
		}
		if err := runUninstall(io.Discard, true, fx); err != nil {
			t.Fatal(err)
		}
		e := *events
		if len(e) < 4 || !slices.Equal(e[:2], []string{"node lock", "data lock"}) ||
			!slices.Equal(e[len(e)-2:], []string{"release data lock", "release node lock"}) {
			t.Errorf("events = %q, want the node lock then the data lock first, and both released last", e)
		}
	})
	t.Run("a refused data lock gives the node lock back", func(t *testing.T) {
		released := false
		fx := &hostEffects{}
		fx.acquireNodeLock = func() (func(), error) { return func() { released = true }, nil }
		fx.acquireEtcdDataLock = func() (func(), error) { return nil, errors.New("busy etcd data") }
		if err := runUninstall(io.Discard, true, fx); err == nil || !released {
			t.Fatalf("runUninstall = %v, node lock released = %t; want the refusal and the lock released", err, released)
		}
	})
}

func TestRunRefusesBusyEtcdDataBeforeStoppingAnything(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{}
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) { return nil, errors.New("busy etcd data") }
	fx.stopHostProcesses = func() error { t.Fatal("stopped processes before acquiring data lock"); return nil }
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "busy etcd data") {
		t.Fatalf("runUninstall(io.Discard, true, fx) with a busy data lock = %v", err)
	}
}

func TestStartLockHeld(t *testing.T) {
	// Sequential by swap's stated exception: the start-lock path is shared
	// package state in internal/version, redirected here for the whole binary.
	oldPath := version.StartLockPath
	version.StartLockPath = filepath.Join(t.TempDir(), "start.lock")
	t.Cleanup(func() { version.StartLockPath = oldPath })
	if busy, err := startLockHeld(); err != nil || busy {
		t.Fatalf("unheld start lock: busy=%v err=%v", busy, err)
	}
	f, err := os.OpenFile(version.StartLockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if busy, err := startLockHeld(); err != nil || !busy {
		t.Fatalf("held start lock: busy=%v err=%v", busy, err)
	}
}

func TestRunRefusesNodeStartBeforeStoppingAnything(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{}
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) { return func() {}, nil }
	fx.startInProgress = func() (bool, error) { return true, nil }
	fx.supervisorRunning = func() bool { return false }
	fx.stopHostProcesses = func() error { t.Fatal("stopped processes while node start was in progress"); return nil }
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "node is starting") {
		t.Fatalf("runUninstall(io.Discard, true, fx) during node start = %v", err)
	}
}

// TestRunDisablesUnitBeforeTeardownAndStopsItOnlyAfter is an
// ordering test, in the same style as TestRunStopsContainersBeforeHostProcesses:
// the reboot-persistence unit is DISABLED before anything else (left enabled,
// it would restart everything Run is about to tear down), and STOPPED only
// once the static pods, host processes and shims are all confirmed down —
// stopping it any earlier SIGTERMs the top-level `stratum up` process, which
// cascades into stopping kubelet through its own shutdown path before the
// static pods have been reaped (the bug being guarded: staticpod.Stop
// timing out because kubelet died first).
// It also covers drop-in cleanup: a successful
// uninstall must call allowRestartFn (removing the Restart=no override) right
// after stopping the unit, not just on the abort path — otherwise a same-boot
// reinstall before the next real reboot inherits a stale override.
func TestRunDisablesUnitBeforeTeardownAndStopsItOnlyAfter(t *testing.T) {
	fx := &hostEffects{}
	events, fx := runNode(t, "worker")
	fx.disableSystemdUnit = func() error { *events = append(*events, "disable unit"); return nil }
	fx.stopSystemdUnit = func() error { *events = append(*events, "stop unit"); return nil }
	fx.allowRestartFn = func() error { *events = append(*events, "allow restart"); return nil }
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	want := []string{"disable unit", "stop kubelet", "stop containers", "stop host processes",
		"wait exit", "stop idle shims", "stop unit", "allow restart", "teardown datapath", "untrust pod cidrs", "remove state", "mark node-a"}
	if !slices.Equal(*events, want) {
		t.Errorf("teardown = %q, want %q", *events, want)
	}
}

// TestRunStopsAManuallyLaunchedTopLevelProcess is a regression test: a node
// started by hand (not through systemd) has its unit loaded but
// ActiveState=inactive — systemctl stop is a real no-op there, since systemd
// never tracked an active instance to signal — so uninstall must find and stop
// the actual `stratum up` process on its own, and confirm it is really gone: no
// top-level process left behind, and the start lock it held released. Uses the
// real start lock (internal/etcddata's flock) so "released" is a genuine
// OS-level fact, not just a mocked bookkeeping flag. Stopping at
// stopSystemdUnit's no-op would leave the process (and its start lock)
// surviving the whole teardown.
func TestRunStopsAManuallyLaunchedTopLevelProcess(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.stopTopLevelFn = func() error { return stopTopLevelProcess(fx) } // undo stubTeardown's no-op: exercise the real logic
	swap(t, &topLevelStopWait, 50*time.Millisecond)
	swap(t, &topLevelKillWait, 50*time.Millisecond)
	swap(t, &pollInterval, time.Millisecond)

	oldPath := version.StartLockPath
	version.StartLockPath = filepath.Join(t.TempDir(), "start.lock")
	t.Cleanup(func() { version.StartLockPath = oldPath })
	lockFile, err := os.OpenFile(version.StartLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lockFile.Close() }) // no-op once SIGTERM below already closed it
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	const fakePID = 999999
	fakeProc(t, fx, map[int]string{fakePID: "S 1"}) // scanTopLevel's startTime read needs a real /proc/<pid>/stat
	alive := []int{fakePID}
	var terminated bool
	fx.topLevelPIDs = func() ([]int, error) { return slices.Clone(alive), nil }
	fx.signalPID = func(pid int, sig syscall.Signal) error {
		if pid == fakePID && sig == syscall.SIGTERM {
			terminated = true
			alive = nil
			// The real effect of SIGTERM reaching stratum up: the process
			// exits and the OS releases every flock it held — simulated here
			// by closing the same fd the "process" was holding.
			lockFile.Close()
		}
		return nil
	}

	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v", err)
	}
	if !terminated {
		t.Fatal("uninstall never signalled the manually launched top-level process")
	}
	pids, err := fx.topLevelPIDs()
	if err != nil || len(pids) != 0 {
		t.Fatalf("top-level process left behind: pids=%v err=%v", pids, err)
	}
	if busy, err := startLockHeld(); err != nil || busy {
		t.Fatalf("start lock still held after uninstall: busy=%v err=%v", busy, err)
	}
}

// withTopLevelSignals stubs the run's topLevelPIDs probe and signalPID effect,
// recording every signal — mirroring shims_test.go's withShimSignals for
// stopTopLevelProcess.
func withTopLevelSignals(t *testing.T, alive *[]int, onSignal func(pid int, sig syscall.Signal)) (*[]string, *hostEffects) {
	t.Helper()
	// Sequential by swap's stated exception: it shortens the shared
	// topLevel waits and pollInterval.
	fx := &hostEffects{}
	var signals []string
	swap(t, &topLevelStopWait, 20*time.Millisecond)
	swap(t, &topLevelKillWait, 20*time.Millisecond)
	swap(t, &pollInterval, time.Millisecond)
	fx.topLevelPIDs = func() ([]int, error) { return slices.Clone(*alive), nil }
	fx.signalPID = func(pid int, sig syscall.Signal) error {
		signals = append(signals, fmt.Sprintf("%v %d", sig, pid))
		if onSignal != nil {
			onSignal(pid, sig)
		}
		return nil
	}
	return &signals, fx
}

// TestStopTopLevelProcessDoesNotSignalAReusedPID is a regression test: the PID
// stopTopLevelProcess found at scan time can be reused by an unrelated process
// before the SIGKILL that follows a SIGTERM which the original process ignored,
// or one that took its time exiting — signalTopLevel/stillTopLevel must recheck
// the start time immediately before each signal, the same technique shims.go
// already uses, and refuse to signal a PID whose start time no longer matches.
// A plain PID check alone would see pid 500 still
// present (LoadState never asks who) and send it the SIGKILL meant for the
// original process.
func TestStopTopLevelProcessDoesNotSignalAReusedPID(t *testing.T) {
	alive := []int{500}
	// fx is assigned before the onSignal closure below can ever run: the
	// closure only fires once stopTopLevelProcess, after fakeProc, is called.
	var fx *hostEffects
	signals, created := withTopLevelSignals(t, &alive, func(pid int, sig syscall.Signal) {
		if sig == syscall.SIGTERM && pid == 500 {
			// The original process exits, and something else is assigned
			// the same PID number before the wait below re-checks it — same
			// pid, different start time.
			writeStat(t, fx, 500, "S 1", "2000")
		}
	})
	fx = created
	fakeProc(t, fx, map[int]string{500: "S 1"}) // start time "1000" (writeStat's default)

	if err := stopTopLevelProcess(fx); err != nil {
		t.Fatalf("stopTopLevelProcess(fx) = %v", err)
	}
	want := []string{"terminated 500"}
	if !slices.Equal(*signals, want) {
		t.Errorf("signals = %q, want %q (a PID reused by another process must never be sent SIGKILL)", *signals, want)
	}
}

// TestStopTopLevelProcessEscalatesToSigkillForTheSamePID is the other half:
// when the SAME process (same PID, same start time) is still there after
// SIGTERM, escalating to SIGKILL must still work.
func TestStopTopLevelProcessEscalatesToSigkillForTheSamePID(t *testing.T) {
	alive := []int{500}
	signals, fx := withTopLevelSignals(t, &alive, func(pid int, sig syscall.Signal) {
		if sig == syscall.SIGKILL && pid == 500 {
			alive = nil
		}
	})
	fakeProc(t, fx, map[int]string{500: "S 1"})

	if err := stopTopLevelProcess(fx); err != nil {
		t.Fatalf("stopTopLevelProcess(fx) = %v", err)
	}
	want := []string{"terminated 500", "killed 500"}
	if !slices.Equal(*signals, want) {
		t.Errorf("signals = %q, want %q", *signals, want)
	}
}

// TestScanTopLevelFailsClosedOnAnUnreadableStartTime is a regression test: a
// start-time read that fails for a reason OTHER than the pid genuinely having
// exited (a malformed stat line here; a permission error in production) must
// surface as an error, never be silently dropped the way a real exit is. An
// unconditional "continue" on any startTime error would read this as
// "nothing to stop" and return an empty, error-free result.
func TestScanTopLevelFailsClosedOnAnUnreadableStartTime(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	dir := t.TempDir()
	fx := &hostEffects{procRoot: dir}
	if err := os.MkdirAll(filepath.Join(dir, "500"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Too few fields after the command name for startTime to reach field 22
	// (the start time) — a read failure that is not "the process is gone".
	if err := os.WriteFile(filepath.Join(dir, "500", "stat"), []byte("500 (weird) name) S 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.topLevelPIDs = func() ([]int, error) { return []int{500}, nil }

	if _, err := scanTopLevel(fx); err == nil {
		t.Fatal("scanTopLevel swallowed a start-time read failure instead of returning it")
	}
}

// TestStillTopLevelFailsClosedOnATopLevelPIDsError is the other half: a
// topLevelPIDs failure inside stillTopLevel must also surface as an error,
// never an empty ("gone") result — this is what waitTopLevelGone and
// signalTopLevel both rely on to abort rather than proceed on an unconfirmed
// case. A nil return on any topLevelPIDs error is indistinguishable from
// every tracked process being confirmed gone.
func TestStillTopLevelFailsClosedOnATopLevelPIDsError(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{
		topLevelPIDs: func() ([]int, error) { return nil, errors.New("read /proc: permission denied") },
	}

	if _, err := stillTopLevel(fx, []topLevelProc{{pid: 500, start: "1000"}}); err == nil {
		t.Fatal("stillTopLevel swallowed a topLevelPIDs failure instead of returning it")
	}
}

// TestRunAbortsWhenTheStartLockCannotBeQueriedBeforeStopping is a regression
// test for the PRE-stop probe: a start-lock query failure here happens before
// stopSystemdUnit/stopTopLevelFn have run, so undo.abort can still fully
// restore what this uninstall has stopped so far — unlike the two POST-stop
// cases below, where the top-level process is already gone and there is nothing
// left to put back.
func TestRunAbortsWhenTheStartLockCannotBeQueriedBeforeStopping(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.startInProgress = func() (bool, error) { return false, errors.New("dbus wedged") }
	fx.stopSystemdUnit = func() error {
		t.Fatal("stopped the unit despite not being able to query the start lock first")
		return nil
	}
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "dbus wedged") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the pre-stop query failure surfaced", err)
	}
}

// TestRunKeepsStateWhenTheStartLockCannotBeQueriedAfterStopping is a
// regression test for the POST-stop gate's own query
// failure: by this point stopSystemdUnit and stopTopLevelFn have both
// already succeeded, so the top-level process this uninstall could find,
// and every static pod and host process it manages, are already confirmed
// down — there is nothing left running for undo.abort to restore, the same
// reasoning as the allowRestartFn warning elsewhere in this window. Unlike
// that warning, this still refuses rather than proceeding to delete state:
// it returns the refusal directly instead of going through undo.abort.
// Going through undo.abort would put abort's "Nothing was deleted"
// account in Run's error instead of this refusal's own.
func TestRunKeepsStateWhenTheStartLockCannotBeQueriedAfterStopping(t *testing.T) {
	fx := &hostEffects{}
	events, fx := runNode(t, "worker")
	calls := 0
	fx.startInProgress = func() (bool, error) {
		calls++
		if calls == 1 {
			return false, nil // the pre-stop probe succeeds
		}
		return false, errors.New("dbus wedged") // the post-stop gate cannot tell
	}

	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "dbus wedged") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the post-stop query failure surfaced", err)
	}
	if strings.Contains(err.Error(), "Nothing was deleted") {
		t.Errorf("uninstall went through undo.abort instead of returning the refusal directly: %v", err)
	}
	if !strings.Contains(err.Error(), "Node state was not deleted") {
		t.Errorf("refusal does not say node state was kept: %v", err)
	}
	if slices.Contains(*events, "remove state") {
		t.Error("uninstall deleted state despite not knowing whether the start lock is still held")
	}
}

// TestRunKeepsStateWhenTheStartLockIsStillHeldAfterStopping is a
// regression test for the start-lock gate itself: a `stratum up`
// running from a different copy of the binary than this `stratum uninstall`
// is not recognized by the exe check in isStratumTopLevel (different file,
// same process), so stopTopLevelFn can report success — nothing it could
// identify was left running — while the real process, and the start lock it
// holds, are still there. This checks that fact directly rather than
// widening the process matcher. Handled the same way as the query failure
// above: no undo.abort (nothing left to restore), state kept, not deleted.
func TestRunKeepsStateWhenTheStartLockIsStillHeldAfterStopping(t *testing.T) {
	fx := &hostEffects{}
	events, fx := runNode(t, "worker")
	fx.startInProgress = func() (bool, error) { return true, nil }

	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "start lock is still held") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the still-held start lock refused", err)
	}
	if strings.Contains(err.Error(), "Nothing was deleted") {
		t.Errorf("uninstall went through undo.abort instead of returning the refusal directly: %v", err)
	}
	if !strings.Contains(err.Error(), "Node state was not deleted") {
		t.Errorf("refusal does not say node state was kept: %v", err)
	}
	if slices.Contains(*events, "remove state") {
		t.Error("uninstall deleted state despite the start lock still being held")
	}
}

// TestRunAbortsWhenDisableUnitFails: a real disable failure must abort before
// Run touches anything, not be papered over.
func TestRunAbortsWhenDisableUnitFails(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.disableSystemdUnit = func() error { return fmt.Errorf("systemctl disable: dbus wedged") }
	fx.stopHostProcesses = func() error { t.Fatal("stopped host processes although disabling the unit failed"); return nil }
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "dbus wedged") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the disable failure surfaced before anything else runs", err)
	}
}

// TestRunAbortsBeforeDeletingWhenStopUnitFails: a real stop failure must not
// be papered over either — proceeding to delete state while the unit could
// still be armed to restart everything just torn down is exactly the gap this guards.
func TestRunAbortsBeforeDeletingWhenStopUnitFails(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.stopSystemdUnit = func() error { return fmt.Errorf("systemctl stop: dbus wedged") }
	fx.removeAll = func(string) error { t.Fatal("deleted state although stopping the unit failed"); return nil }
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "dbus wedged") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the stop failure surfaced before any deletion", err)
	}
}

// TestRunWarnsButDoesNotAbortWhenAllowRestartFailsAfterStop is a regression
// test: once StopUnit has already succeeded, the unit and every host process
// this uninstall manages are already confirmed down, so a failure only to
// remove the now-inert Restart=no drop-in must not unwind all of that back into
// a running node — the drop-in only matters to a future InstallUnit, which
// always strips it itself (internal/host.InstallUnit), so uninstall itself only
// needs to warn and keep going. An abort there would return undo.abort's
// error instead of finishing the teardown.
func TestRunWarnsButDoesNotAbortWhenAllowRestartFailsAfterStop(t *testing.T) {
	fx := &hostEffects{}
	events, fx := runNode(t, "worker")
	fx.allowRestartFn = func() error { return errors.New("dbus wedged") }

	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want a warning, not an abort, once the unit is already stopped", err)
	}
	if !slices.Contains(*events, "remove state") {
		t.Errorf("uninstall did not proceed to delete state after the warning: events=%q", *events)
	}
	if !slices.Contains(*events, "mark node-a") {
		t.Errorf("uninstall did not finish the teardown after the warning: events=%q", *events)
	}
}

// TestRunFoldsAReloadSystemdFailureIntoItsError is a regression test:
// the unit file is already deleted by that point, so a reload failure
// leaves systemd still serving the just-deleted unit's last-loaded definition —
// that must surface as part of Run's own error rather than being reported
// as a clean "stratum uninstalled".
func TestRunFoldsAReloadSystemdFailureIntoItsError(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.reloadSystemd = func() error { return errors.New("dbus wedged") }
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "dbus wedged") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the reload failure surfaced", err)
	}
}

// TestRunFailsClosedWhenUnitEnabledFnFails is a regression
// test for the caller side of UnitEnabled's own fail-closed fix: a query
// failure captured before anything is touched must stop uninstall right
// there, not silently treat "cannot tell whether it's enabled" as either
// answer and proceed to disable the unit anyway.
func TestRunFailsClosedWhenUnitEnabledFnFails(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.unitEnabledFn = func() (bool, error) { return false, errors.New("dbus wedged") }
	fx.disableSystemdUnit = func() error {
		t.Fatal("disabled the unit despite not knowing its enablement")
		return nil
	}
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "dbus wedged") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v, want the enablement query failure surfaced before anything else runs", err)
	}
}

// TestAbortRestoresTheUnitsEnablement is a regression test:
// teardownUndo.abort restarts this node's components, but a node put back
// running is worth nothing if it can no longer come back after a reboot.
// An abort that restored the components but left the unit disabled (and its
// Restart= suppressed) would silently strip reboot persistence from a node
// that otherwise looks fully recovered.
func TestAbortRestoresTheUnitsEnablement(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.unitEnabledFn = func() (bool, error) { return true, nil }
	var reenabled, restartAllowed bool
	fx.enableSystemdUnit = func() error { reenabled = true; return nil }
	fx.allowRestartFn = func() error { restartAllowed = true; return nil }
	fx.stopHostProcesses = func() error { return errors.New("supervisor unreachable") }

	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "supervisor unreachable") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v", err)
	}
	if !reenabled {
		t.Error("abort did not re-enable stratum.service")
	}
	if !restartAllowed {
		t.Error("abort did not restore the unit's Restart= (AllowRestart)")
	}
	if !strings.Contains(err.Error(), "re-enabled for the next reboot") {
		t.Errorf("abort's account of the node does not mention re-enabling: %v", err)
	}
}

// TestAbortLeavesAnAlreadyDisabledUnitDisabled: abort must restore what
// uninstall found, not force the unit enabled — an operator who had already
// disabled it (e.g. to keep the node from starting at boot) must not have
// that choice silently reversed by a failed uninstall.
func TestAbortLeavesAnAlreadyDisabledUnitDisabled(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.unitEnabledFn = func() (bool, error) { return false, nil }
	fx.enableSystemdUnit = func() error { t.Fatal("abort re-enabled a unit that was already disabled"); return nil }
	fx.stopHostProcesses = func() error { return errors.New("supervisor unreachable") }

	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "supervisor unreachable") {
		t.Fatalf("runUninstall(io.Discard, true, fx) = %v", err)
	}
}

// TestUninstallOnAMaskedUnit is a regression test:
// host.UnitEnabled reports a masked unit (an operator's own `systemctl
// mask`) as "not enabled" rather than an error, so
// uninstall must be able to tear a masked node down — not refuse it — and it
// must never try to touch the mask itself: DisableUnit only removes boot
// -enablement symlinks, a different systemctl operation from mask/unmask,
// and EnableUnit is exactly the call that would refuse to run against a
// masked unit, so abort must not call it just because it saw "not enabled".
func TestUninstallOnAMaskedUnit(t *testing.T) {
	t.Run("teardown proceeds, and disable never touches the mask", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		fx.unitEnabledFn = func() (bool, error) { return false, nil } // masked reports as "not enabled"
		var disabled bool
		fx.disableSystemdUnit = func() error { disabled = true; return nil }
		fx.enableSystemdUnit = func() error {
			t.Fatal("uninstall tried to enable a masked unit")
			return nil
		}

		if err := runUninstall(io.Discard, true, fx); err != nil {
			t.Fatalf("runUninstall(io.Discard, true, fx) on a masked unit = %v, want teardown to proceed", err)
		}
		if !disabled {
			t.Error("uninstall did not disable the unit's boot enablement")
		}
		if !slices.Contains(*events, "remove state") {
			t.Errorf("uninstall did not proceed to delete state: events=%q", *events)
		}
	})

	t.Run("abort leaves the mask in place", func(t *testing.T) {
		_, fx := runNode(t, "worker")
		fx.unitEnabledFn = func() (bool, error) { return false, nil }
		fx.enableSystemdUnit = func() error {
			t.Fatal("abort tried to enable a masked unit")
			return nil
		}
		fx.stopHostProcesses = func() error { return errors.New("supervisor unreachable") }

		if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "supervisor unreachable") {
			t.Fatalf("runUninstall(io.Discard, true, fx) = %v", err)
		}
	})
}

// TestTargetsKeepEveryPathUninstallRemovesToday pins today's removal set as
// literals, so the refactor that moves these paths into internal/version
// cannot change what uninstall removes without this test saying so. The list
// is internal/uninstall/uninstall.go's dirsAndFiles as it stands; Targets()
// returns a clone of it. Each pinned path must be a
// target itself, or lie under a target that is a directory root (the covering
// rule of TestTargetsCoverTheEnvironmentFile). The CIS sysctl file
// /etc/sysctl.d/99-stratum-cis.conf is written by host.Prereqs under --cis,
// so the file is asserted as covered like every other path: an uninstall of a
// --cis node must leave behind as little as one of a plain node.
func TestTargetsKeepEveryPathUninstallRemovesToday(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	want := []string{
		"/var/lib/stratum",
		"/etc/stratum",
		"/run/stratum",
		"/etc/cni/net.d",
		"/etc/containerd/certs.d",
		"/etc/containerd/conf.d/stratum-registry.toml",
		"/etc/sysctl.d/99-stratum.conf",
		"/etc/sysctl.d/99-stratum-cis.conf",
		"/etc/modules-load.d/k8s.conf",
		"/var/log/stratum",
		"/etc/systemd/system/stratum.service",
	}
	targets := Targets()
	for _, p := range want {
		covered := false
		for _, target := range targets {
			if p == target || strings.HasPrefix(p, strings.TrimSuffix(target, "/")+"/") {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("no target in %q covers %q; a node would keep a file uninstall removed before", targets, p)
		}
	}
}

// Targets and both previews must follow the roots used by the writers.
func TestTargetsFollowSetRoots(t *testing.T) {
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	t.Cleanup(func() { version.SetRoots(data, conf, run) })
	root := t.TempDir()
	version.SetRoots(filepath.Join(root, "data"), filepath.Join(root, "conf"), filepath.Join(root, "run"))
	for _, p := range []string{version.DataDir, version.ConfDir, version.RunDir, version.CertsDir, version.ContainerdConfD, version.AuditLogDir} {
		if !slices.Contains(Targets(), p) {
			t.Errorf("Targets() missing redirected path %q", p)
		}
	}
	for _, p := range []string{data, conf, run} {
		if slices.Contains(Targets(), p) {
			t.Errorf("Targets() still deletes previous root %q", p)
		}
	}
	if !slices.Equal(Preview().Targets, Targets()) {
		t.Error("preview does not follow redirected targets")
	}
}

// TestDryRunListsEveryTargetAndEachConditionalItem pins the dry run's
// preview: one line per target in Targets(), plus the conditional items that
// are not targets — the CNI network-namespace mounts, the kubepods cgroups
// and the legacy /var/lib/kubelet — so a shared preview function cannot drop
// one from the CLI while keeping it elsewhere. Checked with strings.Contains,
// so lines that depend on the host (a legacy containerd config, firewalld
// sources) do not make it flaky.
func TestDryRunListsEveryTargetAndEachConditionalItem(t *testing.T) {
	fx := &hostEffects{
		// The layout the disclosure names, as Run wires it: the dry run reads
		// the paths from the run's own value.
		netnsDir:         "/run/netns",
		ciliumRunDir:     "/run/cilium",
		cgroupRoot:       "/sys/fs/cgroup",
		legacyKubeletDir: "/var/lib/kubelet",
	}
	fx.removeAll = func(string) error {
		t.Error("the dry run removed a target")
		return nil
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, false, fx) })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range Targets() {
		if !strings.Contains(out, "  - "+p) {
			t.Errorf("dry-run output does not list target %q:\n%s", p, out)
		}
	}
	for _, want := range []string{fx.netnsDir, fx.ciliumRunDir, fx.cgroupRoot, fx.legacyKubeletDir} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output does not mention %q:\n%s", want, out)
		}
	}
}

// The tests below pin that one preview function in this package
// returns what an uninstall would do as data — Targets() plus the conditional
// effects, each as a PreviewItem carrying what it is (Kind), what path, CIDR
// or interface it touches (Path), the one explanation both surfaces render
// (Detail), and, when a probe could not answer, that error (Unknown) — so the
// CLI dry run's prose and the MCP uninstall_preview's structured output are
// rendered from the same value and cannot enumerate different effects. The
// contract the implementation builds against:
//
//   - preview.go declares PreviewItem{Kind, Path, Detail, Unknown string},
//     PreviewReport{Targets []string, Items []PreviewItem} and
//     Preview() PreviewReport.
//   - Preview consults the package variable PreviewProbes — a struct of four
//     read-only host probes (LegacyHostConfig, TrustedPodCIDRs,
//     OwnedFirewalldInterfaces, SELinuxRegistered) with the production
//     implementations wired as its default — because the MCP handler builds no
//     hostEffects, and a package variable is the seam both this package's and
//     internal/mcp's tests can stub.
//   - The layout paths the dry run names today (/run/netns, /run/cilium,
//     /sys/fs/cgroup, /var/lib/kubelet) come back as fixed items, so
//     TestDryRunListsEveryTargetAndEachConditionalItem's items survive the
//     shared function.
//
// The Kind strings are data both surfaces and callers read; they are pinned
// here as literals, not Go constants, so a rename has to change this test on
// purpose.

// previewItemsByKind returns the items of one kind, in order.
func previewItemsByKind(items []PreviewItem, kind string) []PreviewItem {
	var got []PreviewItem
	for _, it := range items {
		if it.Kind == kind {
			got = append(got, it)
		}
	}
	return got
}

func TestPreviewEnumeratesTargetsAndConditionals(t *testing.T) {
	saved := PreviewProbes
	t.Cleanup(func() { PreviewProbes = saved })
	PreviewProbes.LegacyHostConfig = func() (string, bool) { return "/etc/containerd/config.toml", true }
	PreviewProbes.TrustedPodCIDRs = func() ([]string, error) {
		return []string{"10.245.0.0/16", "fd00:1:2:3::/64"}, nil
	}
	PreviewProbes.OwnedFirewalldInterfaces = func() ([]string, error) { return []string{"lxc+"}, nil }
	PreviewProbes.SELinuxRegistered = func() bool { return true }

	var p PreviewReport
	printed := captureStdout(t, func() { p = Preview() })
	if printed != "" {
		t.Errorf("Preview printed %q; it returns data for both surfaces to render and prints nothing itself", printed)
	}

	if !slices.Equal(p.Targets, Targets()) {
		t.Errorf("Preview.Targets = %q, want Targets() unchanged so both previews delete exactly what the runner deletes", p.Targets)
	}

	// The four layout items the dry run names today are unconditional: the
	// datapath teardown always runs, so the preview always says so.
	for _, want := range []struct{ kind, path string }{
		{"netns", "/run/netns"},
		{"cilium", "/run/cilium"},
		{"cgroups", "/sys/fs/cgroup"},
		{"legacy-kubelet", "/var/lib/kubelet"},
	} {
		got := previewItemsByKind(p.Items, want.kind)
		if len(got) != 1 || got[0].Path != want.path {
			t.Errorf("Preview() items of kind %q = %+v, want one naming %q", want.kind, got, want.path)
			continue
		}
		if got[0].Detail == "" {
			t.Errorf("the %q item carries no explanation; the MCP preview has nothing to show for it", want.kind)
		}
	}

	// The host-dependent conditionals appear exactly as the probes answered.
	legacy := previewItemsByKind(p.Items, "legacy-containerd-config")
	if len(legacy) != 1 || legacy[0].Path != "/etc/containerd/config.toml" {
		t.Errorf("Preview() items of kind legacy-containerd-config = %+v, want one naming the config the probe reported present", legacy)
	}
	sources := previewItemsByKind(p.Items, "firewalld-source")
	var gotPaths []string
	for _, s := range sources {
		gotPaths = append(gotPaths, s.Path)
	}
	slices.Sort(gotPaths)
	if !slices.Equal(gotPaths, []string{"10.245.0.0/16", "fd00:1:2:3::/64"}) {
		t.Errorf("Preview() items of kind firewalld-source name %q, want one per trusted source", gotPaths)
	}
	ifaces := previewItemsByKind(p.Items, "firewalld-interface")
	if len(ifaces) != 1 || ifaces[0].Path != "lxc+" {
		t.Errorf("Preview() items of kind firewalld-interface = %+v, want one naming lxc+", ifaces)
	}
	if selinux := previewItemsByKind(p.Items, "selinux-contexts"); len(selinux) != 1 {
		t.Errorf("Preview() items of kind selinux-contexts = %+v, want one when contexts are registered", selinux)
	}
	for _, it := range p.Items {
		if it.Unknown != "" {
			t.Errorf("Preview() item %+v reports an unknown; every probe answered", it)
		}
	}
}

// TestPreviewReportsAnUnanswerableProbe pins the unknown state: a probe that
// cannot answer becomes an item whose Unknown carries the error and whose Path
// is empty, so the preview says what it could not learn instead of staying
// silent while the effect would be left in place.
func TestPreviewReportsAnUnanswerableProbe(t *testing.T) {
	saved := PreviewProbes
	t.Cleanup(func() { PreviewProbes = saved })
	PreviewProbes.LegacyHostConfig = func() (string, bool) { return "/etc/containerd/config.toml", false }
	PreviewProbes.TrustedPodCIDRs = func() ([]string, error) { return nil, fmt.Errorf("record unreadable") }
	PreviewProbes.OwnedFirewalldInterfaces = func() ([]string, error) { return nil, fmt.Errorf("firewall-cmd failed") }
	PreviewProbes.SELinuxRegistered = func() bool { return false }

	p := Preview()
	if got := previewItemsByKind(p.Items, "firewalld-source"); len(got) != 1 || got[0].Path != "" || !strings.Contains(got[0].Unknown, "record unreadable") {
		t.Errorf("Preview() items of kind firewalld-source = %+v, want one carrying the probe's error as Unknown and no path", got)
	}
	if got := previewItemsByKind(p.Items, "firewalld-interface"); len(got) != 1 || got[0].Path != "" || !strings.Contains(got[0].Unknown, "firewall-cmd failed") {
		t.Errorf("Preview() items of kind firewalld-interface = %+v, want one carrying the probe's error as Unknown and no path", got)
	}
	// Absent probes answer nothing, and nothing stands in for them.
	for _, kind := range []string{"legacy-containerd-config", "selinux-contexts"} {
		if got := previewItemsByKind(p.Items, kind); len(got) != 0 {
			t.Errorf("Preview() items of kind %q = %+v, want none when the probe reports the effect absent", kind, got)
		}
	}
}

// TestPreviewKeepsTheFixedItemsOnAPlainHost pins the floor: with every
// host-dependent probe answering "none", the preview still returns the five
// fixed layout items, so a surface rendering only Preview() can never drop
// what TestDryRunListsEveryTargetAndEachConditionalItem pins.
func TestPreviewKeepsTheFixedItemsOnAPlainHost(t *testing.T) {
	saved := PreviewProbes
	t.Cleanup(func() { PreviewProbes = saved })
	PreviewProbes.LegacyHostConfig = func() (string, bool) { return "/etc/containerd/config.toml", false }
	PreviewProbes.TrustedPodCIDRs = func() ([]string, error) { return nil, nil }
	PreviewProbes.OwnedFirewalldInterfaces = func() ([]string, error) { return nil, nil }
	PreviewProbes.SELinuxRegistered = func() bool { return false }

	p := Preview()
	var kinds []string
	for _, it := range p.Items {
		kinds = append(kinds, it.Kind)
	}
	slices.Sort(kinds)
	want := []string{"cgroups", "cilium", "legacy-kubelet", "local-path-dir", "netns"}
	if !slices.Equal(kinds, want) {
		t.Errorf("Preview() item kinds = %q, want exactly the fixed %q", kinds, want)
	}
	for _, it := range p.Items {
		if it.Path == "" || it.Detail == "" {
			t.Errorf("the fixed item %+v names no path or carries no explanation", it)
		}
	}
}

// TestDryRunDisclosesTheSharedPreviewConditionals pins the CLI side of the one
// preview: with the probes stubbed, the dry run's prose names every
// conditional item Preview() returns — the legacy containerd config, the
// firewalld bindings and the registered SELinux contexts — so the two surfaces
// enumerate the same effects because they render the same value.
func TestDryRunDisclosesTheSharedPreviewConditionals(t *testing.T) {
	saved := PreviewProbes
	t.Cleanup(func() { PreviewProbes = saved })
	PreviewProbes.LegacyHostConfig = func() (string, bool) { return "/etc/containerd/config.toml", true }
	PreviewProbes.TrustedPodCIDRs = func() ([]string, error) { return []string{"10.245.0.0/16"}, nil }
	PreviewProbes.OwnedFirewalldInterfaces = func() ([]string, error) { return []string{"lxc+"}, nil }
	PreviewProbes.SELinuxRegistered = func() bool { return true }

	fx := &hostEffects{
		netnsDir:         "/run/netns",
		ciliumRunDir:     "/run/cilium",
		cgroupRoot:       "/sys/fs/cgroup",
		legacyKubeletDir: "/var/lib/kubelet",
	}
	fx.removeAll = func(string) error {
		t.Error("the dry run removed a target")
		return nil
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, false, fx) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/etc/containerd/config.toml",
		"10.245.0.0/16",
		"lxc+",
		"SELinux",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not disclose %q; it must render the shared preview's conditional items:\n%s", want, out)
		}
	}
}

// TestDryRunReachesNoEffect pins the dry run free of effects: runUninstall(io.Discard, false, fx)
// returns nil having touched nothing — no lock, no stop, no disable, no
// removal, no untrust. Every effect is swapped for one that fails the test if
// it is ever reached, so a probe or effect moved ahead of the dry-run branch
// fails here instead of changing the host.
func TestDryRunReachesNoEffect(t *testing.T) {
	_, fx := runNode(t, "worker")
	var reached []string
	fail := func(name string) { reached = append(reached, name) }
	fx.acquireNodeLock = func() (func(), error) { fail("node maintenance lock"); return func() {}, nil }
	fx.acquireEtcdDataLock = func() (func(), error) { fail("etcd data lock"); return func() {}, nil }
	fx.startInProgress = func() (bool, error) { fail("start lock query"); return false, nil }
	fx.unitEnabledFn = func() (bool, error) { fail("enablement query"); return false, nil }
	fx.disableSystemdUnit = func() error { fail("disable unit"); return nil }
	fx.suppressRestartFn = func() error { fail("suppress restart"); return nil }
	fx.stopKubeletFn = func() error { fail("stop kubelet"); return nil }
	fx.stopContainersFn = func() error { fail("stop containers"); return nil }
	fx.stopHostProcesses = func() error { fail("stop host processes"); return nil }
	fx.stopSystemdUnit = func() error { fail("stop unit"); return nil }
	fx.stopTopLevelFn = func() error { fail("stop the stratum up process"); return nil }
	fx.allowRestartFn = func() error { fail("allow restart"); return nil }
	fx.unmountStateFn = func() error { fail("unmount state"); return nil }
	fx.teardownDatapathFn = func() error { fail("teardown datapath"); return nil }
	fx.untrustPodCIDRs = func() error { fail("untrust pod cidrs"); return nil }
	fx.removeAll = func(string) error { fail("remove a target"); return nil }
	fx.reloadSystemd = func() error { fail("reload systemd"); return nil }

	if err := runUninstall(io.Discard, false, fx); err != nil {
		t.Fatal(err)
	}
	if len(reached) > 0 {
		t.Errorf("the dry run reached effects it must not: %q", reached)
	}
}

// TestStubTeardownsDefangKeepsRunOffTheHostConfig pins the one thing that keeps
// every Run test off the host's /etc/containerd/config.toml today: stubTeardown
// redirects the legacy-config heal seam with a counted no-op, so no test reads
// or deletes that file, whatever the host keeps there. This pin holds before
// and after the seam replaced the version.ContainerdConfig defang trick. The
// heal's removal semantics themselves are pinned in internal/runtime's own
// suite (TestRemoveLegacyHostConfig); this pin covers what the uninstall
// boundary must keep off the host path.
func TestStubTeardownsDefangKeepsRunOffTheHostConfig(t *testing.T) {
	swap(t, &version.ContainerdConfig, "/etc/containerd/config.toml")

	t.Run("the forced run removes nothing and reports nothing", func(t *testing.T) {
		events, fx := runNode(t, "worker")
		var err error
		out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"removed /etc/containerd/config.toml",
			"could not remove legacy containerd config",
		} {
			if strings.Contains(out, want) {
				t.Errorf("the forced run said %q under the defang; the guard must keep the heal off the host path:\n%s", want, out)
			}
		}
		if !slices.Contains(*events, "remove state") {
			t.Errorf("events = %q; the teardown never reached the removal that precedes the heal", *events)
		}
	})

	t.Run("the dry run does not disclose the host config", func(t *testing.T) {
		fx := &hostEffects{}
		var err error
		out := captureWriter(func(w io.Writer) { err = runUninstall(w, false, fx) })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "containerd config a previous stratum wrote over the host's") {
			t.Errorf("the dry run disclosed a legacy config the guard reports as absent:\n%s", out)
		}
	})

	t.Run("the probe reports the legacy path absent", func(t *testing.T) {
		p, present := runtime.LegacyHostConfig()
		if present {
			t.Errorf("LegacyHostConfig() = (%q, true) under the defang; the guard must report absent without reading the host path", p)
		}
		if p != "/etc/containerd/config.toml" {
			t.Errorf("LegacyHostConfig() = %q, want the host's legacy path", p)
		}
	})
}

// TestHealKeepsAHostConfigItCannotProveIsStratums pins the heal's attribution
// gate at the exported entry the forced path calls. With version's containerd
// root and state redirected to temp paths, no file at the host's legacy path
// can prove stratum's, so the real runtime.RemoveLegacyHostConfig must leave
// /etc/containerd/config.toml exactly as it found it and log no removal —
// whatever the host keeps there (nothing, a distro config, or an older
// stratum's own file). The version-path redirect keeps the call read-only on
// the real host in every one of those cases, and the live-config redirect off
// the legacy path keeps the guard from standing in for the attribution gate.
func TestHealKeepsAHostConfigItCannotProveIsStratums(t *testing.T) {
	dir := t.TempDir()
	swap(t, &version.ContainerdRootDir, filepath.Join(dir, "containerd"))
	swap(t, &version.ContainerdStateDir, filepath.Join(dir, "run-containerd"))
	swap(t, &version.ContainerdConfig, filepath.Join(dir, "config", "containerd.toml"))

	const hostPath = "/etc/containerd/config.toml"
	before, beforeErr := os.ReadFile(hostPath)
	out := captureStdout(t, func() { runtime.RemoveLegacyHostConfig(os.Stdout) })
	if strings.Contains(out, "removed "+hostPath) {
		t.Errorf("the heal logged a removal at %s although no config can prove stratum's under the redirected dirs:\n%s", hostPath, out)
	}
	after, afterErr := os.ReadFile(hostPath)
	if (beforeErr == nil) != (afterErr == nil) {
		t.Fatalf("the heal changed whether %s exists: read error before %v, after %v", hostPath, beforeErr, afterErr)
	}
	if beforeErr == nil && string(before) != string(after) {
		t.Errorf("the heal modified %s: %q -> %q", hostPath, before, after)
	}
}

// The tests below pin the host-effects value: the package's injected effect
// variables, the legacy-config heal seam among them, are one complete
// host-effects value, Run keeps its exported signature and hands a value it
// constructs itself to an unexported runner, and a test that forgets a stub
// fails closed on a sentinel instead of reaching the host. The names and
// signatures asserted here are the contract the implementation builds against.

// TestUnsetEffectsFailClosed is the fail-closed harness proof: a hostEffects
// with every effect unset must make the runner return the sentinel, naming the
// effect it stopped at, instead of reaching the host through a production
// default or panicking through a nil function. The sentinel is what the
// harness captures when a test forgets a stub, so it must be identifiable
// with errors.Is and must say which field was missing; and it must fire
// before any host call, which here is the etcd data lock — the first effect
// the forced path reaches, ahead of every probe, stop and removal. No effect
// may answer an unset field by falling back to its real implementation: a
// fake that forgot a stub would then perform the very host effect it exists
// to keep off the node.
func TestUnsetEffectsFailClosed(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	err := runUninstall(io.Discard, true, &hostEffects{})
	if err == nil {
		t.Fatal("runUninstall with every effect unset succeeded; want the unset-effect sentinel")
	}
	if !errors.Is(err, errUnsetEffect) {
		t.Fatalf("runUninstall = %v, want it to wrap the unset-effect sentinel", err)
	}
	if !strings.Contains(err.Error(), "acquireNodeLock") {
		t.Errorf("the sentinel does not name the effect it stopped at, so the harness cannot say which stub is missing: %v", err)
	}
}

// TestDryRunNeedsNoEffects: the preview only prints what a forced run would
// do, so it consults none of the effects, and even a zero hostEffects — every
// one of them unset — must return nil, not the sentinel. A fail-closed guard
// that fired on the dry-run path would make the preview fail before any effect
// could have been reached.
func TestDryRunNeedsNoEffects(t *testing.T) {
	err := runUninstall(io.Discard, false, &hostEffects{})
	if err != nil {
		t.Fatalf("the dry run with every effect unset = %v; the preview performs no effects and must not fail closed", err)
	}
}

// TestHostEffectsCoverTheInventory pins one complete host-effects value: every
// teardown effect the package injects today is a function field on it, so a
// run's stubs cannot miss one and no effect stays behind as a package
// variable. The read-only probes, the discovery state and the filesystem
// layout are per-run values too (TestHostEffectsCarriesThePerRunValues pins
// that), but they are not effects, so they are deliberately absent from this
// list; extra fields beyond it are allowed.
func TestHostEffectsCoverTheInventory(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	inventory := []string{
		"acquireNodeLock",
		"acquireEtcdDataLock",
		"removeAll",
		"stopHostProcesses",
		"disableSystemdUnit",
		"stopSystemdUnit",
		"stopSupervisedUnit",
		"stopTopLevelFn",
		"reloadSystemd",
		"enableSystemdUnit",
		"suppressRestartFn",
		"allowRestartFn",
		"untrustPodCIDRs",
		"removeSELinuxContexts",
		"unmountStateFn",
		"unmountFn",
		"putManifestFn",
		"stopStaticPodFn",
		"startUnitFn",
		"startContainerdFn",
		"stopContainerdFn",
		"stopKubeletFn",
		"stopContainersFn",
		"waitHostExitFn",
		"startAndWaitActive",
		"ctrRun",
		"runCmd",
		"nodeMarkerFn",
		"teardownDatapathFn",
		"stopIdleShimsFn",
		"signalPID",
		"removeLegacyHostConfig",
	}
	fields := map[string]reflect.StructField{}
	ty := reflect.TypeOf(hostEffects{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		fields[f.Name] = f
	}
	for _, name := range inventory {
		f, ok := fields[name]
		if !ok {
			t.Errorf("hostEffects has no %s field; the inventoried effect would stay a package variable", name)
			continue
		}
		if f.Type.Kind() != reflect.Func {
			t.Errorf("hostEffects.%s is a %s, want a host effect function", name, f.Type.Kind())
		}
	}
}

// TestRunOwnsTheProductionHostEffects parses the package's production files
// and pins the entry-point structure the seam depends on: Run keeps the
// exported signature the CLI and MCP callers compile against — the writer
// convention: io.Writer as the first parameter, the force flag second — calls
// the unexported runner with that writer and the effects value, wires the
// real local-path directory and its empty-only removal into that value, and is
// the only place that constructs one — the production value is never a
// package-level variable a second run could inherit, so every call gets its
// own. The CLI passes os.Stdout, so its output stays byte-identical to what
// it printed when the prints went straight to os.Stdout; the MCP handler
// passes the buffer its result is built from, so the same progress reaches
// the result as data with no stdout redirect.
func TestRunOwnsTheProductionHostEffects(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	funcs := map[string]*ast.FuncDecl{}
	for _, name := range []string{"uninstall.go", "containers.go", "datapath.go", "shims.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	isType := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	isPointerTo := func(e ast.Expr, name string) bool {
		star, ok := e.(*ast.StarExpr)
		return ok && isType(star.X, name)
	}

	entry := funcs["Run"]
	if entry == nil {
		t.Fatal("uninstall.go declares no Run; the entry point must keep its exported signature")
	}
	if entry.Recv != nil || entry.Type.Params.NumFields() != 2 ||
		!isIoWriter(entry.Type.Params.List[0].Type) || !isType(entry.Type.Params.List[1].Type, "bool") ||
		entry.Type.Results.NumFields() != 1 || !isType(entry.Type.Results.List[0].Type, "error") {
		t.Error("Run's signature changed; the writer convention is Run(w io.Writer, force bool) error — the writer first, so the CLI's os.Stdout and the MCP handler's buffer are interchangeable — and it must not")
	}
	entryDelegates := false
	ast.Inspect(entry.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "RunWithOptions" {
			entryDelegates = true
		}
		return true
	})
	if !entryDelegates {
		t.Error("Run does not delegate to RunWithOptions, the one function that builds the production hostEffects")
	}
	run := funcs["RunWithOptions"]
	if run == nil {
		t.Fatal("uninstall.go declares no RunWithOptions; it builds the production hostEffects")
	}
	if run.Recv != nil || run.Type.Params.NumFields() != 2 ||
		!isIoWriter(run.Type.Params.List[0].Type) || !isType(run.Type.Params.List[1].Type, "Options") ||
		run.Type.Results.NumFields() != 1 || !isType(run.Type.Results.List[0].Type, "error") {
		t.Error("RunWithOptions's signature changed; want RunWithOptions(w io.Writer, o Options) error, the writer first")
	}

	declared := false
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "hostEffects" {
					continue
				}
				declared = true
				if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
					t.Errorf("hostEffects is %T, want a struct of function fields", ts.Type)
				}
			}
		}
	}
	if !declared {
		t.Fatal("no hostEffects struct in the production files; the one host-effects value is the seam")
	}

	runner := funcs["runUninstall"]
	if runner == nil {
		t.Fatal("no unexported runUninstall; the runner is what takes the effects value")
	}
	if runner.Recv != nil || runner.Type.Params.NumFields() != 3 ||
		!isIoWriter(runner.Type.Params.List[0].Type) ||
		!isType(runner.Type.Params.List[1].Type, "bool") ||
		!isPointerTo(runner.Type.Params.List[2].Type, "hostEffects") ||
		runner.Type.Results.NumFields() != 1 || !isType(runner.Type.Results.List[0].Type, "error") {
		t.Error("runUninstall's signature changed; want func runUninstall(w io.Writer, force bool, fx *hostEffects) error — the runner carries the writer every print below it writes to")
	}

	delegates := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "runUninstall" {
			delegates = true
		}
		return true
	})
	if !delegates {
		t.Error("RunWithOptions never mentions runUninstall; the runner must be what the builder calls")
	}

	// Run wires the real directory and the empty-only removal into its effects
	// value as the selectors of the packages that own them: without the path
	// the run skips the local-path directory, and without syscall.Rmdir it
	// fails as an unset effect. It is rmdir, not os.Remove, which would also
	// unlink a file that replaced the directory.
	wired := map[string]bool{}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return true
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case key.Name == "localPathDir" && pkg.Name == "selinux" && sel.Sel.Name == "LocalPathDir":
			wired[key.Name] = true
		case key.Name == "removeEmptyDir" && pkg.Name == "syscall" && sel.Sel.Name == "Rmdir":
			wired[key.Name] = true
		}
		return true
	})
	for _, name := range []string{"localPathDir", "removeEmptyDir"} {
		if !wired[name] {
			t.Errorf("RunWithOptions does not wire %s into its hostEffects value; without the path the run skips the local-path directory, and without syscall.Rmdir it fails as an unset effect", name)
		}
	}

	// Production construction only in Run: every hostEffects composite literal
	// in the production files sits inside Run's body, and no package-level
	// variable has the type. The value is per-run, never a shared singleton.
	for name, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "RunWithOptions" || d.Body == nil {
					continue
				}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if cl, ok := n.(*ast.CompositeLit); ok && isType(cl.Type, "hostEffects") {
						t.Errorf("%s constructs a production hostEffects; only RunWithOptions does", name)
					}
					return true
				})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if vs.Type != nil && (isType(vs.Type, "hostEffects") || isPointerTo(vs.Type, "hostEffects")) {
						t.Errorf("package-level variable %s is a hostEffects; the effects value is per-run, built in Run", vs.Names[0].Name)
					}
					for _, v := range vs.Values {
						ast.Inspect(v, func(n ast.Node) bool {
							if cl, ok := n.(*ast.CompositeLit); ok && isType(cl.Type, "hostEffects") {
								t.Errorf("a package-level variable is initialized with a hostEffects literal; the production value is built in Run")
							}
							return true
						})
					}
				}
			}
		}
	}
}

// isIoWriter reports whether e is the type io.Writer, the parameter the writer
// convention puts first on every operation the MCP tools call.
func isIoWriter(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "io" && sel.Sel.Name == "Writer"
}

// TestUninstallPrintsThroughTheWriter pins the other half of the writer
// convention over the whole package: every preview, progress and warning
// print is a write to the run's writer, never an implicit write to the
// process's stdout. fmt.Print* always writes os.Stdout, so none may remain in
// a production file — each one became an fmt.Fprint* to the writer handed
// down from Run (the census this replaced counted 36: uninstall.go 29,
// containers.go 5, shims.go 1, datapath.go 1) — and no package-level variable
// may carry a writer for the prints to fall back to, which two Runs in one
// process would race over.
func TestUninstallPrintsThroughTheWriter(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fset := token.NewFileSet()
	for _, name := range []string{"uninstall.go", "containers.go", "datapath.go", "shims.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "fmt" {
				return true
			}
			switch sel.Sel.Name {
			case "Print", "Printf", "Println":
				t.Errorf("%s: fmt.%s writes to os.Stdout; every print goes to the run's writer, as fmt.Fprint* to it",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if isIoWriter(vs.Type) {
					t.Errorf("%s: package-level %s carries an io.Writer; the writer is per-run, handed down from Run, never shared between runs",
						fset.Position(vs.Pos()), vs.Names[0].Name)
				}
				for _, v := range vs.Values {
					if sel, ok := v.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && sel.Sel.Name == "Stdout" {
							t.Errorf("%s: package-level %s is initialized with os.Stdout; the writer is per-run, handed down from Run",
								fset.Position(vs.Pos()), vs.Names[0].Name)
						}
					}
				}
			}
		}
	}
}

// The tests below pin the teardown phases: Run's teardown sequence is split
// into named phases along its refusal, preparation, commit and removal
// boundaries, and each step's error class and rollback behavior is declared
// with its phase instead of decided inline where the step happens to sit. The
// phase names asserted here are the contract the phase split builds against:
// refuseUninstall (refusal — the fail-closed gates), prepareTeardown
// (reversible preparation — the stops, whose failures go through undo.abort),
// commitTeardown (commit — the post-stop start-lock gate and the Restart=
// restore, past which nothing is left to restore) and removeState (commit and
// removal — the deletes, whose errors accumulate and are never rolled back).

// phasesOfTeardown is the declared phase list, each with the steps it owns.
var phasesOfTeardown = []struct {
	name  string
	role  string
	steps string
}{
	{"refuseUninstall", "refusal", "the etcd data lock, the start-in-progress probe, the orphan preflight and the enablement query"},
	{"prepareTeardown", "reversible preparation", "the early systemd disable, the restart suppression, the static-pod and container stops, the host-process stop, the sweeps, the unmount, the pre-stop start-lock probe, the unit stop and the top-level process stop"},
	{"commitTeardown", "commit", "the post-stop start-lock gate and the Restart= restore"},
	{"removeState", "commit/removal", "the datapath teardown, the firewalld untrust, the SELinux removal, the delete loop, the legacy-config heal and the systemd reload"},
}

// TestRunTeardownIsSplitIntoDeclaredPhases parses uninstall.go and pins the
// phase structure: the named phase functions exist, the runner hands the run
// to them in teardown order, undo.abort is called from the preparation phase
// and from nowhere else (a rollback decision taken inline anywhere else is a
// step without a declared boundary), the early systemd disable runs in the
// preparation phase and not in the refusal one, the post-stop start-lock gate
// and the Restart= restore run in the commit phase, and the legacy-config
// heal runs in the removal phase.
func TestRunTeardownIsSplitIntoDeclaredPhases(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "uninstall.go", nil, 0)
	if err != nil {
		t.Fatalf("parse uninstall.go: %v", err)
	}
	bodies := map[string]*ast.BlockStmt{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			bodies[fn.Name.Name] = fn.Body
		}
	}

	var missing []string
	for _, p := range phasesOfTeardown {
		if bodies[p.name] == nil {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("uninstall.go declares no %v; Run's teardown must be split into named refusal, preparation, commit and removal phases with each step's error class declared by its phase", missing)
	}

	// runUninstall hands the phases the run, in teardown order: the phases are
	// the runner's structure, not a set of helpers it scatters.
	if run := bodies["runUninstall"]; run != nil {
		at := map[string]token.Pos{}
		ast.Inspect(run, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if _, seen := at[id.Name]; !seen {
					for _, p := range phasesOfTeardown {
						if id.Name == p.name {
							at[id.Name] = id.Pos()
						}
					}
				}
			}
			return true
		})
		var notCalled []string
		ordered, prev := true, token.Pos(0)
		for _, p := range phasesOfTeardown {
			pos, ok := at[p.name]
			if !ok {
				notCalled = append(notCalled, p.name)
				continue
			}
			if prev > 0 && pos < prev {
				ordered = false
			}
			prev = pos
		}
		if len(notCalled) > 0 {
			t.Errorf("runUninstall never calls %v; the runner must hand the run to its phases in teardown order", notCalled)
		} else if !ordered {
			t.Error("runUninstall calls the phases out of order; the teardown runs refusal, preparation, commit, then removal")
		}
	}

	// undo.abort is the declared rollback of the preparation phase and of
	// nothing else. Counting call sites per function, instead of per step,
	// keeps the pin honest: a rollback decision that migrates to a new phase,
	// or back into the runner's body, changes the count here.
	aborts := map[string]int{}
	for name, body := range bodies {
		if name == "abort" {
			continue // the rollback itself, not a decision about one
		}
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "abort" {
					aborts[name]++
				}
			}
			return true
		})
	}
	totalAborts := 0
	for name, n := range aborts {
		totalAborts += n
		if name != "prepareTeardown" {
			t.Errorf("%s decides %d rollback(s) with undo.abort inline; only prepareTeardown, the preparation phase, may abort — every other phase's failure class is declared with the phase, not decided at the call site", name, n)
		}
	}
	if totalAborts == 0 {
		t.Error("no undo.abort call site in uninstall.go; the rollback-boundary pin is vacuous")
	}

	// Where the pinned steps live: the early systemd disable is preparation,
	// not preflight (it is the run's first host change, not a gate), so its
	// call belongs to prepareTeardown and to no earlier phase; the commit
	// phase is the post-stop start-lock gate plus the Restart= restore; the
	// heal is a commit/removal step.
	uses := func(fn, ident string) bool {
		body := bodies[fn]
		if body == nil {
			return false
		}
		found := false
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch e := call.Fun.(type) {
			case *ast.Ident:
				if e.Name == ident {
					found = true
				}
			case *ast.SelectorExpr:
				if e.Sel.Name == ident {
					found = true
				}
			}
			return true
		})
		return found
	}
	if bodies["prepareTeardown"] != nil {
		if uses("refuseUninstall", "disableSystemdUnit") {
			t.Error("refuseUninstall disables the unit; the early systemd disable is preparation, not preflight")
		}
		if !uses("prepareTeardown", "disableSystemdUnit") {
			t.Error("prepareTeardown never disables the unit; the early systemd disable is the preparation phase's first step")
		}
		if uses("prepareTeardown", "allowRestartFn") {
			t.Error("prepareTeardown restores the unit's Restart= itself; that restore belongs to the commit phase, after the start-lock gate")
		}
	}
	if bodies["commitTeardown"] != nil {
		if !uses("commitTeardown", "startInProgress") {
			t.Error("commitTeardown never consults the start lock; the post-stop start-lock gate is the commit decision")
		}
		if !uses("commitTeardown", "allowRestartFn") {
			t.Error("commitTeardown never restores the unit's Restart=; the restore is the commit phase's warning-only step")
		}
	}
	if bodies["removeState"] != nil {
		if !uses("removeState", "removeLegacyHostConfig") {
			t.Error("removeState never runs the legacy-config heal; the heal is a commit/removal step, self-logging and warning-only")
		}
	}
}

// TestRunFailureAtEachStepMatchesItsDeclaredPhaseAndClass is the rollback
// boundary table: one row per teardown step, naming the phase the step runs
// in and the error class the phase declares for it — refusal (nothing stopped
// or removed, no undo), undo.abort (reversible preparation: what was stopped
// is put back and nothing is deleted), accumulated (commit/removal: every
// other target is still removed and nothing is put back) and warning-only
// (the run continues). Each row injects a failure at its step and asserts
// exactly the declared outcome: the host state it leaves, the error class,
// and which undo calls ran. A step moved across a phase boundary, or a class
// changed without this table changing with it, fails here.
func TestRunFailureAtEachStepMatchesItsDeclaredPhaseAndClass(t *testing.T) {
	const stateRemovedHint = "The node's state was removed; fix the reported cleanup failures and re-run `stratum uninstall --yes` to finish"
	for _, tc := range []struct {
		name string
		// phase is the declared phase the step runs in; class is the error
		// class its phase declares: refusal, undo.abort, accumulated or
		// warning.
		phase string
		class string
		role  string
		// controlPlane gives the run manifests to read and stop (an etcd and a
		// kube-apiserver static pod), so abort's manifest restore has something
		// to put back.
		controlPlane bool
		// keepDataDir marks a row whose failure makes Run keep DataDir on
		// purpose (the firewalld ownership record lives under it).
		keepDataDir bool
		// fail injects the failure at the row's step. removed is the removal
		// recorder, for the row whose failing step is the removal itself.
		fail func(t *testing.T, fx *hostEffects, removed *[]string)
		// wantText is what the error must say; wantWarn, what the run must
		// print, for the warning rows that succeed.
		wantText []string
		wantWarn []string
		// wantPuts is how many manifests an abort puts back; wantStarts, the
		// units an abort starts again, in abort's restart order.
		wantPuts   int
		wantStarts []string
		// after checks cross-step facts once the run has returned.
		after func(t *testing.T)
	}{
		// refuseUninstall: the fail-closed gates. Nothing has been touched, so
		// a refusal restores nothing and deletes nothing.
		{
			name:  "the node maintenance lock refuses",
			phase: "refuseUninstall",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.acquireNodeLock = func() (func(), error) {
					return nil, errors.New("stratum uninstall: certs rotate-ca is already running on this node (pid 7 holds /run/stratum-upgrade.lock); nothing was changed")
				}
			},
			wantText: []string{"certs rotate-ca is already running on this node"},
		},
		{
			name:  "the etcd data lock refuses",
			phase: "refuseUninstall",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.acquireEtcdDataLock = func() (func(), error) { return nil, errors.New("busy etcd data") }
			},
			wantText: []string{"busy etcd data"},
		},
		{
			name:  "the node-start probe refuses",
			phase: "refuseUninstall",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.supervisorRunning = func() bool { return false }
				fx.startInProgress = func() (bool, error) { return true, nil }
			},
			wantText: []string{"node is starting"},
		},
		{
			name:  "the orphan preflight refuses",
			phase: "refuseUninstall",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.supervisorRunning = func() bool { return false }
				fx.staticPodActive = func(name string) bool { return name == "etcd" }
				fx.hostProcessRunning = func(string) (bool, error) { return false, nil }
			},
			wantText: []string{"control-plane processes are still up", "Nothing was changed"},
		},
		{
			name:  "the enablement query refuses",
			phase: "refuseUninstall",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.unitEnabledFn = func() (bool, error) { return false, errors.New("dbus wedged") }
			},
			wantText: []string{"determine whether stratum.service is enabled"},
		},
		// prepareTeardown: the reversible stops. The early systemd disable is
		// the phase's first step — preparation, not preflight: it is the run's
		// first host change (boot enablement), but when it fails nothing has
		// changed that undo could put back, so its class is refusal.
		{
			name:  "the early systemd disable refuses",
			phase: "prepareTeardown",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.disableSystemdUnit = func() error { return errors.New("systemctl disable: dbus wedged") }
			},
			wantText: []string{"systemctl disable: dbus wedged"},
		},
		{
			name:  "the restart suppression aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.suppressRestartFn = func() error { return errors.New("cannot write the drop-in") }
			},
			wantText:   []string{"suppress the unit's Restart= during teardown", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: nil,
		},
		{
			name:         "an unreadable manifest aborts",
			phase:        "prepareTeardown",
			class:        "undo.abort",
			role:         "controller",
			controlPlane: true,
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.readManifestFn = func(name string) ([]byte, error) {
					if name == "kube-apiserver" {
						return nil, errors.New("permission denied")
					}
					return []byte(name + " manifest"), nil
				}
			},
			wantText:   []string{"read the kube-apiserver manifest", "Nothing was deleted"},
			wantPuts:   1, // etcd was read and stored; the apiserver's was not removed
			wantStarts: nil,
		},
		{
			name:         "a failed etcd stop aborts",
			phase:        "prepareTeardown",
			class:        "undo.abort",
			role:         "controller",
			controlPlane: true,
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.stopStaticPodFn = func(name string) error {
					if name == "etcd" {
						return errors.New("etcd wedged")
					}
					return nil
				}
			},
			wantText:   []string{"stop etcd", "Nothing was deleted"},
			wantPuts:   1,
			wantStarts: nil, // kubelet is still up; nothing was stopped that abort must start
		},
		{
			name:  "an unreadable supervisor status aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.supervisorStatus = func() (map[string]supervisor.ProcStatus, error) { return nil, errors.New("control socket timeout") }
			},
			wantText:   []string{"read the supervisor's status", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: nil,
		},
		{
			name:  "a kubelet that cannot be stopped aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.stopKubeletFn = func() error { return errors.New("kubelet will not stop") }
			},
			wantText: []string{"kubelet will not stop", "Nothing was deleted"},
			wantPuts: 0,
			// kubelet was recorded stopped before the stop was attempted, so
			// the abort starts it again; the host processes were not stopped.
			wantStarts: []string{"kubelet"},
		},
		{
			name:  "a containerd that cannot start aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.unitActive = func(string) bool { return false }
				fx.startContainerdFn = func() error { return errors.New("cannot start containerd") }
			},
			wantText: []string{"start containerd to stop this node's containers", "Nothing was deleted"},
			wantPuts: 0,
			// The containerd started for the teardown is stopped first, then
			// containerd and kubelet are started again in roster order.
			wantStarts: []string{"containerd", "kubelet"},
		},
		{
			name:  "containers that cannot be stopped abort",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.stopContainersFn = func() error { return errors.New("containerd refused") }
			},
			wantText:   []string{"containerd refused", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"kubelet"},
		},
		{
			name:         "a failed host-process stop aborts",
			phase:        "prepareTeardown",
			class:        "undo.abort",
			role:         "controller",
			controlPlane: true,
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.stopHostProcesses = func() error { return errors.New("lost stop acknowledgement") }
			},
			wantText: []string{"lost stop acknowledgement", "Nothing was deleted", "services were started again (containerd, kubelet, operator)"},
			wantPuts: 2,
			// The supervisor was already asked to stop everything, so the abort
			// starts every wanted unit again, containerd first.
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		{
			name:  "a static pod that survives its stopped manifest aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.staticPodRunning = func(name string) (bool, error) { return name == "etcd", nil }
			},
			wantText:   []string{"still running after its manifest was removed", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		{
			name:  "a shim that stays aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.hostProcessRunning = func(name string) (bool, error) { return name == "runtime-shim", nil }
			},
			wantText:   []string{"runtime-shim", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		{
			name:  "a mount that cannot be unmounted aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.unmountStateFn = func() error { return errors.New("1 mounts could not be unmounted") }
			},
			wantText:   []string{"mounts could not be unmounted", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		{
			name:  "a start-lock query that fails before the stop aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.startInProgress = func() (bool, error) { return false, errors.New("dbus wedged") }
			},
			wantText:   []string{"the start lock cannot be queried", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		{
			name:  "a failed unit stop aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.stopSystemdUnit = func() error { return errors.New("systemctl stop: dbus wedged") }
			},
			wantText:   []string{"stop stratum.service", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		{
			name:  "a top-level process that survives aborts",
			phase: "prepareTeardown",
			class: "undo.abort",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.stopTopLevelFn = func() error { return errors.New("stratum up still running") }
			},
			wantText:   []string{"stop the stratum up process", "Nothing was deleted"},
			wantPuts:   0,
			wantStarts: []string{"containerd", "kubelet", "operator"},
		},
		// commitTeardown: the post-stop start-lock gate is the commit decision.
		// Its refusal returns directly, not through undo.abort: everything the
		// run could find is already stopped, so there is nothing left to
		// restore — it keeps the node stopped and its state undeleted.
		{
			name:  "the post-stop start-lock gate refuses",
			phase: "commitTeardown",
			class: "refusal",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				calls := 0
				fx.startInProgress = func() (bool, error) {
					calls++
					if calls == 1 {
						return false, nil // the pre-stop probe succeeds
					}
					return false, errors.New("dbus wedged")
				}
			},
			wantText: []string{"Node state was not deleted"},
		},
		{
			name:  "the Restart= restore only warns",
			phase: "commitTeardown",
			class: "warning",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.allowRestartFn = func() error { return errors.New("dbus wedged") }
			},
			wantWarn: []string{"restore stratum.service's Restart=:"},
		},
		// removeState: commit and removal. Errors accumulate and the rest of
		// the teardown still runs; nothing is rolled back.
		{
			name:  "the datapath teardown accumulates",
			phase: "removeState",
			class: "accumulated",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.teardownDatapathFn = func() error { return errors.New("bpffs wedged") }
			},
			wantText: []string{"not fully removed", stateRemovedHint},
		},
		{
			name:        "the firewalld untrust accumulates",
			phase:       "removeState",
			class:       "accumulated",
			role:        "worker",
			keepDataDir: true, // the ownership record under DataDir is kept for retry
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.untrustPodCIDRs = func() error { return errors.New("firewalld still trusts pod CIDRs stratum added") }
			},
			wantText: []string{"for the firewalld ownership record"},
		},
		{
			name:  "the SELinux removal accumulates",
			phase: "removeState",
			class: "accumulated",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.removeSELinuxContexts = func() error { return errors.New("semanage failed") }
			},
			wantText: []string{"were not all removed", stateRemovedHint},
		},
		{
			name:  "a failed target removal accumulates",
			phase: "removeState",
			class: "accumulated",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, removed *[]string) {
				fx.removeAll = func(p string) error {
					*removed = append(*removed, p)
					return errors.New("device or resource busy")
				}
			},
			wantText: []string{"some paths could not be removed"},
			after: func(t *testing.T) {
				if healRuns != 1 {
					t.Errorf("the legacy-config heal ran %d times although a target removal failed; it is an independent healing step no removal failure may skip", healRuns)
				}
			},
		},
		{
			name:  "the systemd reload accumulates",
			phase: "removeState",
			class: "accumulated",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.reloadSystemd = func() error { return errors.New("dbus wedged") }
			},
			wantText: []string{"reload systemd", stateRemovedHint},
		},
		{
			name:  "an unreadable kubelet credential only warns",
			phase: "removeState",
			class: "warning",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.nodeMarkerFn = func() (func(string, time.Time) error, error) {
					return nil, errors.New("open kubelet.kubeconfig: no such file or directory")
				}
			},
			wantWarn: []string{"could not read kubelet's credential", "Node object stays"},
		},
		{
			name:  "a failed Node mark only warns",
			phase: "removeState",
			class: "warning",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.nodeMarkerFn = func() (func(string, time.Time) error, error) {
					return func(string, time.Time) error { return errors.New("forbidden") }, nil
				}
			},
			wantWarn: []string{"could not mark Node node-a", "Node object stays"},
		},
		{
			name:  "the legacy-config heal only warns",
			phase: "removeState",
			class: "warning",
			role:  "worker",
			fail: func(t *testing.T, fx *hostEffects, _ *[]string) {
				fx.removeLegacyHostConfig = func(w io.Writer) {
					healRuns++
					// The heal's warning travels the run's writer, like every
					// other self-logged step's report.
					fmt.Fprintln(w, "  WARN: could not remove legacy containerd config")
				}
			},
			wantWarn: []string{"could not remove legacy containerd config"},
			after: func(t *testing.T) {
				if healRuns != 1 {
					t.Errorf("the legacy-config heal ran %d times on the forced path, want exactly once", healRuns)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fx := runNode(t, tc.role)
			var reenabled, restartAllowed bool
			var removed, puts, started []string
			// The unit was enabled when found, so an abort re-enables it and
			// removes the Restart= drop-in: those are the undo calls every
			// abort makes, and a refusal must make none of them.
			fx.unitEnabledFn = func() (bool, error) { return true, nil }
			fx.enableSystemdUnit = func() error { reenabled = true; return nil }
			fx.allowRestartFn = func() error { restartAllowed = true; return nil }
			fx.removeAll = func(p string) error { removed = append(removed, p); return nil }
			fx.startUnitFn = func(name string) error { started = append(started, name); return nil }
			if tc.controlPlane {
				fx.staticPodExists = func(name string) bool { return name == "etcd" || name == "kube-apiserver" }
				fx.readManifestFn = func(name string) ([]byte, error) { return []byte(name + " manifest"), nil }
				fx.stopStaticPodFn = func(string) error { return nil }
				fx.putManifestFn = func(name string, b []byte) error { puts = append(puts, name+"="+string(b)); return nil }
			}
			tc.fail(t, fx, &removed)

			var err error
			out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
			wantRemoved := Targets()
			if tc.keepDataDir {
				wantRemoved = slices.DeleteFunc(slices.Clone(wantRemoved), func(p string) bool { return p == version.DataDir })
			}
			switch tc.class {
			case "refusal":
				if err == nil {
					t.Fatal("Run succeeded; want a refusal")
				}
				if reenabled || restartAllowed {
					t.Errorf("refusal restored the unit (re-enabled %v, restart allowed %v); a refusal makes no undo calls", reenabled, restartAllowed)
				}
				if len(removed) > 0 {
					t.Errorf("refusal removed %q; want nothing removed", removed)
				}
				if len(puts) > 0 || len(started) > 0 {
					t.Errorf("refusal put back %q and started %q; a refusal makes no undo calls", puts, started)
				}
			case "undo.abort":
				if err == nil {
					t.Fatal("Run succeeded; want an abort")
				}
				if !strings.Contains(err.Error(), "Nothing was deleted") {
					t.Errorf("abort error does not say nothing was deleted: %v", err)
				}
				if !reenabled || !restartAllowed {
					t.Errorf("abort did not restore the unit: re-enabled %v, restart allowed %v", reenabled, restartAllowed)
				}
				if len(removed) > 0 {
					t.Errorf("abort removed %q; want nothing removed", removed)
				}
				if len(puts) != tc.wantPuts {
					t.Errorf("abort put back %q, want %d manifest(s)", puts, tc.wantPuts)
				}
				if !slices.Equal(started, tc.wantStarts) {
					t.Errorf("abort started %q, want %q", started, tc.wantStarts)
				}
			case "accumulated":
				if err == nil {
					t.Fatal("Run succeeded; want accumulated errors")
				}
				if !slices.Equal(removed, wantRemoved) {
					t.Errorf("removed = %q, want every removal except those the code skips on purpose: %q", removed, wantRemoved)
				}
				if len(puts) > 0 || len(started) > 0 {
					t.Errorf("removal put back %q and started %q; past the commit boundary nothing is rolled back", puts, started)
				}
			case "warning":
				if err != nil {
					t.Fatalf("runUninstall = %v; want the warning not to fail the run", err)
				}
				if !slices.Equal(removed, wantRemoved) {
					t.Errorf("removed = %q, want every target removed after the warning: %q", removed, wantRemoved)
				}
				if len(puts) > 0 || len(started) > 0 {
					t.Errorf("a warning put back %q and started %q; a warning makes no undo calls", puts, started)
				}
			default:
				t.Fatalf("unknown class %q", tc.class)
			}
			for _, want := range tc.wantText {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Run = %v, want its error to contain %q", err, want)
				}
			}
			for _, want := range tc.wantWarn {
				if !strings.Contains(out, want) {
					t.Errorf("the run printed\n%s, want it to contain %q", out, want)
				}
			}
			if tc.after != nil {
				tc.after(t)
			}
		})
	}
}

// --------------------------- the full footprint ----------------------------
//
// Two parts of the node footprint sit outside the three roots but are still
// stratum's to remove: the operator's helm cache at the legacy
// /var/lib/stratum-helm on nodes brought up by earlier releases (today's
// releases keep it under version.DataDir, which the DataDir target already
// removes), and the upgrade's binary backup sets, written as
// stratum.bak-<ts> beside version.StratumInstallPath. These tests pin both
// node layouts across the delete list, the forced delete loop and the dry
// run; the MCP uninstall_preview derives from the same Targets().

// coveredByTargets reports whether p is a target itself or lies under a
// target that is a directory root — the covering rule
// TestTargetsCoverTheEnvironmentFile established.
func coveredByTargets(p string) bool {
	for _, target := range Targets() {
		if p == target || strings.HasPrefix(p, strings.TrimSuffix(target, "/")+"/") {
			return true
		}
	}
	return false
}

// TestTargetsCoverTheOperatorHelmCacheOnNewAndLegacyNodes pins the operator
// helm cache on both layouts: a node brought up by the current release keeps
// it under the data dir (version.HelmCacheDir, covered by the DataDir
// target), and a node brought up by an earlier release keeps it at the
// literal /var/lib/stratum-helm — outside the three roots, so it needs a
// target of its own. Without that target nothing covers
// /var/lib/stratum-helm, and an uninstalled legacy node keeps the operator's
// whole helm cache (the repository index cache, repositories.yaml and the
// written CA files).
func TestTargetsCoverTheOperatorHelmCacheOnNewAndLegacyNodes(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	if !coveredByTargets(version.HelmCacheDir) {
		t.Errorf("no target in %q covers version.HelmCacheDir %q; the operator's helm cache would survive an uninstall of a current node", Targets(), version.HelmCacheDir)
	}
	const legacy = "/var/lib/stratum-helm"
	if !coveredByTargets(legacy) {
		t.Errorf("no target in %q covers %q; an uninstalled node brought up by an earlier release keeps the operator's whole helm cache", Targets(), legacy)
	}
}

// TestTargetsCoverTheStratumBinaryBackupsBesideTheInstallPath pins the
// upgrade's binary backup sets beside the installed stratum binary: an
// upgrade saves the live binary as stratum.bak-<ts> beside version.StratumInstallPath
// and keeps the newest sets there (internal/upgrade), outside the three
// roots, so uninstall needs its own target for them or a node keeps every
// old binary it ever rolled back from. The target must follow
// version.StratumInstallPath at call time, and cover only the backup sets:
// the live binary, its non-backup neighbours and another tool's backups in
// the same directory are not uninstall's to remove. Without that target
// the created .bak- files never reach Targets().
func TestTargetsCoverTheStratumBinaryBackupsBesideTheInstallPath(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "stratum")
	swap(t, &version.StratumInstallPath, install)
	baks := []string{install + ".bak-20260101-000000", install + ".bak-20260201-000000"}
	others := []string{install, install + ".old", install + ".bak-not-a-stamp", install + ".bak-20260230-000000", filepath.Join(dir, "other-tool.bak-20260101-000000")}
	for _, p := range append(slices.Clone(baks), others...) {
		if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	backupDir := install + ".bak-20260301-000000"
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupLink := install + ".bak-20260401-000000"
	if err := os.Symlink(install, backupLink); err != nil {
		t.Fatal(err)
	}
	others = append(others, backupDir, backupLink)
	targets := Targets()
	for _, bak := range baks {
		if !slices.Contains(targets, bak) {
			t.Errorf("Targets() = %q is missing the upgrade backup set %q beside version.StratumInstallPath; uninstall would leave every old stratum binary on the node", targets, bak)
		}
	}
	for _, keep := range others {
		if slices.Contains(targets, keep) {
			t.Errorf("Targets() includes %q; only the stratum backup sets beside the install path are uninstall's to remove", keep)
		}
	}
}

// TestRunRemovesTheLegacyHelmCacheAndStratumBackups drives the forced
// teardown across both layouts at once: the delete loop must visit the data
// dir (which carries the new helm cache location), the legacy
// /var/lib/stratum-helm, and every stratum.bak-<ts> backup beside
// version.StratumInstallPath, while leaving the live binary itself alone.
// Without those targets the recorder never sees the legacy helm dir or
// the backups.
func TestRunRemovesTheLegacyHelmCacheAndStratumBackups(t *testing.T) {
	_, fx := runNode(t, "worker")
	dir := t.TempDir()
	install := filepath.Join(dir, "stratum")
	swap(t, &version.StratumInstallPath, install)
	baks := []string{install + ".bak-20260101-000000", install + ".bak-20260201-000000"}
	for _, p := range append(slices.Clone(baks), install) {
		if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var removed []string
	fx.removeAll = func(p string) error { removed = append(removed, p); return nil }

	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	for _, want := range append([]string{version.DataDir, "/var/lib/stratum-helm"}, baks...) {
		if !slices.Contains(removed, want) {
			t.Errorf("the delete loop never visited %q; removed = %q", want, removed)
		}
	}
	if slices.Contains(removed, install) {
		t.Errorf("the delete loop removed the live stratum binary %q; only the backup sets beside it are uninstall's to remove", install)
	}
}

// TestDryRunDisclosesTheLegacyHelmCacheAndStratumBackups pins the CLI
// preview's half of the footprint derivation: the dry run prints one line per
// target, so the legacy helm dir and the backup sets beside the install path
// are disclosed to the operator before --yes exactly when the delete loop
// would visit them. Without those targets neither is printed.
func TestDryRunDisclosesTheLegacyHelmCacheAndStratumBackups(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "stratum")
	swap(t, &version.StratumInstallPath, install)
	bak := install + ".bak-20260101-000000"
	for _, p := range []string{bak, install} {
		if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, false, &hostEffects{}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  - /var/lib/stratum-helm") {
		t.Errorf("the dry run does not disclose the legacy operator helm cache:\n%s", out)
	}
	if !strings.Contains(out, "  - "+bak) {
		t.Errorf("the dry run does not disclose the upgrade backup set %q:\n%s", bak, out)
	}
}

// testCA is a self-signed CA with the PEM forms the kubelet cert-pair tests
// write: the bundle file's cert, and a cert-and-key pair file as the kubelet's
// certificate manager keeps one.
type testCA struct {
	certPEM []byte
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
}

// newTestCA generates a throwaway self-signed CA.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
		cert:    cert,
	}
}

// pairPEM writes a kubelet-style cert-and-key pair file signed by this CA,
// named after the role the pair serves (kubelet-server-, kubelet-client-).
func (ca *testCA) pairPEM(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "system:node:test-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...,
	)
}

// TestRemoveLegacyKubeletDirRemovesStratumsKubeletCertPairs: stratum's kubelet
// keeps its certificate-manager pairs in the kubelet's fixed --cert-dir
// default (internal/pki's kubeletDefaultCertDir) although its --root-dir is
// under DataDir, so on a stratum-only node they are the pairs stratum's CA
// signed — removed with the stale sockets, and the emptied cert dir and kubelet
// dir with them. The pairs are the gap the kubelet dir's footprint entry
// covers; removeLegacyKubeletDir is the directory's only remover, which is why
// it is not a plain delete target.
func TestRemoveLegacyKubeletDirRemovesStratumsKubeletCertPairs(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	ca := newTestCA(t)
	pkiDir := t.TempDir()
	// Sequential by swap's stated exception: it redirects version.PKIDir.
	swap(t, &version.PKIDir, pkiDir)
	if err := os.WriteFile(filepath.Join(pkiDir, "ca-bundle.crt"), ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(dir, "pki")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []string{"kubelet-server", "kubelet-client"} {
		name := pair + "-2026-10-15-12-00-00.pem"
		if err := os.WriteFile(filepath.Join(certDir, name), ca.pairPEM(t, pair), 0o600); err != nil {
			t.Fatal(err)
		}
		target := name
		if pair == "kubelet-server" {
			target = filepath.Join(certDir, name)
		}
		if err := os.Symlink(target, filepath.Join(certDir, pair+"-current.pem")); err != nil {
			t.Fatal(err)
		}
	}

	var out string
	var err error
	out = captureWriter(func(w io.Writer) { err = removeLegacyKubeletDir(w, fx) })
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Lstat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("%s still exists (%v), want it gone with stratum's kubelet's sockets and cert pairs:\n%s", dir, serr, out)
	}
}

// TestRemoveLegacyKubeletDirKeepsAnotherKubeletsCertPairs: a co-hosted
// kubelet's pairs live in the same cert dir, signed by a CA stratum's bundle
// does not carry, so they are kept and said so, and with them the cert dir and
// the kubelet dir — while stratum's own pair beside them still goes. What
// cannot be judged (an unreadable file, no bundle) is kept the same way.
func TestRemoveLegacyKubeletDirKeepsAnotherKubeletsCertPairs(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	stratum := newTestCA(t)
	foreign := newTestCA(t)
	pkiDir := t.TempDir()
	// Sequential by swap's stated exception: it redirects version.PKIDir.
	swap(t, &version.PKIDir, pkiDir)
	if err := os.WriteFile(filepath.Join(pkiDir, "ca-bundle.crt"), stratum.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(dir, "pki")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "kubelet-server-current.pem"), stratum.pairPEM(t, "server"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "kubelet-server-old.pem"), foreign.pairPEM(t, "server"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "garbage.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureWriter(func(w io.Writer) { err = removeLegacyKubeletDir(w, fx) })
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Lstat(filepath.Join(certDir, "kubelet-server-current.pem")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("stratum's own pair was kept (%v):\n%s", serr, out)
	}
	for _, kept := range []string{"kubelet-server-old.pem", "garbage.pem"} {
		if _, serr := os.Lstat(filepath.Join(certDir, kept)); serr != nil {
			t.Errorf("%s was removed: %v\n%s", kept, serr, out)
		}
	}
	if _, serr := os.Lstat(dir); serr != nil {
		t.Errorf("the kubelet dir was removed although a foreign kubelet's state is in it: %v\n%s", serr, out)
	}
	if !strings.Contains(out, "no CA in stratum's bundle signed it") {
		t.Errorf("the run did not say why it kept the foreign pair:\n%s", out)
	}
}

func TestRemoveLegacyKubeletDirKeepsNonDirectoryPKI(t *testing.T) {
	for _, kind := range []string{"symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			fx := &hostEffects{}
			newDatapathHost(t, fx)
			dir := shortKubeletDir(t, fx)
			certDir := filepath.Join(dir, "pki")
			foreignDir := t.TempDir()
			foreignFile := filepath.Join(foreignDir, "kubelet-client-current.pem")
			contents := []byte("foreign kubelet state")
			if err := os.WriteFile(foreignFile, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(foreignDir, certDir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(certDir, contents, 0o600); err != nil {
				t.Fatal(err)
			}

			if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if target, err := os.Readlink(certDir); err != nil || target != foreignDir {
					t.Errorf("foreign PKI symlink = %q, %v; want %q", target, err, foreignDir)
				}
			} else if got, err := os.ReadFile(certDir); err != nil || string(got) != string(contents) {
				t.Errorf("foreign PKI file = %q, %v; want %q", got, err, contents)
			}
			if got, err := os.ReadFile(foreignFile); err != nil || string(got) != string(contents) {
				t.Errorf("foreign certificate = %q, %v; want %q", got, err, contents)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("removed kubelet directory containing foreign state: %v", err)
			}
		})
	}
}

func TestKubeletCertCleanupKeepsUnprovenSymlinks(t *testing.T) {
	ca := newTestCA(t)
	pkiDir := t.TempDir()
	swap(t, &version.PKIDir, pkiDir)
	if err := os.WriteFile(filepath.Join(pkiDir, "ca-bundle.crt"), ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"foreign", "external", "dangling", "chained"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "kubelet-client-2026.pem")
			pair := ca.pairPEM(t, "client")
			switch kind {
			case "foreign":
				pair = newTestCA(t).pairPEM(t, "client")
			case "external":
				target = filepath.Join(t.TempDir(), "kubelet-client-2026.pem")
			}
			if kind != "dangling" {
				if err := os.WriteFile(target, pair, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "chained" {
				alias := filepath.Join(dir, "alias.pem")
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				target = alias
			}
			link := filepath.Join(dir, "kubelet-client-current.pem")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := removeStratumSignedKubeletCerts(io.Discard, dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Readlink(link); err != nil {
				t.Errorf("unproven link removed: %v", err)
			}
			if kind == "foreign" || kind == "external" {
				if _, err := os.Stat(target); err != nil {
					t.Errorf("unowned target removed: %v", err)
				}
			}
		})
	}
}

// TestFailedCertRemovalKeepsTheNodeState pins the recovery contract of the
// kubelet cert-dir sweep: a verified pair whose removal fails makes the
// datapath failure carry errKeepState, so Run keeps DataDir — the CA bundle
// under it is what decides whose a certificate is, and deleting it would turn
// a fixable unlink failure into a leftover no later uninstall can ever
// identify, let alone remove. The removal here fails because the cert dir is
// not writable (skipped under root, which bypasses the permission).
func TestFailedCertRemovalKeepsTheNodeState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	stratum := newTestCA(t)
	pkiDir := t.TempDir()
	swap(t, &version.PKIDir, pkiDir)
	if err := os.WriteFile(filepath.Join(pkiDir, "ca-bundle.crt"), stratum.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(dir, "pki")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "kubelet-server-current.pem"), stratum.pairPEM(t, "server"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(certDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(certDir, 0o700) })

	err := removeLegacyKubeletDir(io.Discard, fx)
	if !errors.Is(err, errKeepState) {
		t.Fatalf("removeLegacyKubeletDir over an unlink failure = %v; want errKeepState, so the delete loop keeps the CA bundle the next attempt needs", err)
	}
}
