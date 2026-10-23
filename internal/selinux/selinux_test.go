package selinux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/toolenv"
	"github.com/ctrliq/stratum/internal/version"
)

// fakeHost puts the test on a host in mode, with Stratum's paths and the
// local-path directory under a temp dir. mode Disabled leaves selinuxfs absent.
func fakeHost(t *testing.T, mode Mode) string {
	t.Helper()
	tmp := t.TempDir()
	fs := filepath.Join(tmp, "selinuxfs")
	if mode != Disabled {
		if err := os.MkdirAll(fs, 0o755); err != nil {
			t.Fatal(err)
		}
		enforce := "0"
		if mode == Enforcing {
			enforce = "1"
		}
		if err := os.WriteFile(filepath.Join(fs, "enforce"), []byte(enforce+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "mountinfo"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&selinuxFS, fs},
		{&mountInfo, filepath.Join(tmp, "mountinfo")},
		{&LocalPathDir, filepath.Join(tmp, "local-path")},
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.BinDir, filepath.Join(tmp, "data", "bin")},
		{&version.UpgradeStageDir, filepath.Join(tmp, "data", "upgrade")},
		{&version.RunDir, filepath.Join(tmp, "run")},
		{&version.StratumInstallPath, filepath.Join(tmp, "stratum")},
		{&version.SystemdUnitPath, filepath.Join(tmp, "stratum.service")},
		{&version.ContainerdRootDir, filepath.Join(tmp, "data", "containerd")},
		{&version.CoreDNSUpstreamDir, filepath.Join(tmp, "data", "dns")},
		{&version.ContainerdStateDir, filepath.Join(tmp, "run", "containerd")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	for _, d := range []string{version.BinDir, version.RunDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{version.StratumInstallPath, version.SystemdUnitPath} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

// realRunPaths points the run directory, containerd's state and the unit at
// their real paths, which the distribution's equivalences cover.
func realRunPaths(t *testing.T) {
	t.Helper()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.RunDir, "/run/stratum"},
		{&version.ContainerdStateDir, "/run/stratum/containerd"},
		{&version.SystemdUnitPath, "/etc/systemd/system/stratum.service"},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
}

type policyBase struct {
	rules  []rule
	distro [][2]string
}

// el9 is the part of a Rocky 9 policy these tests use: it labels /run as
// /var/run, and container-selinux's containerd rules are under /var/run.
var el9 = policyBase{
	rules: []rule{
		{"/var/run(/.*)?", "all files", "var_run_t"},
		{"/var/run/containerd(/.*)?", "all files", "container_var_run_t"},
		{"/var/run/containerd/[^/]*/sandboxes/[^/]*/shm(/.*)?", "all files", "container_runtime_tmpfs_t"},
	},
	distro: [][2]string{{"/run", "/var/run"}, {"/run/lock", "/var/lock"}, {"/etc/systemd/system", "/usr/lib/systemd/system"}, {"/bin", "/usr/bin"}},
}

// el10 labels /var/run as /run, the other way round.
var el10 = policyBase{
	rules: []rule{
		{"/run(/.*)?", "all files", "var_run_t"},
		{"/run/containerd(/.*)?", "all files", "container_var_run_t"},
		{"/run/containerd/[^/]*/sandboxes/[^/]*/shm(/.*)?", "all files", "container_runtime_tmpfs_t"},
	},
	distro: [][2]string{{"/var/run", "/run"}, {"/var/lock", "/run/lock"}, {"/etc/systemd/system", "/usr/lib/systemd/system"}},
}

// fakePolicy stands in for the policy tools of a host with base's policy. It
// keeps the local contexts semanage registers, so consecutive Ensure calls see
// what earlier ones did, and records every call.
type fakePolicy struct {
	base     policyBase
	module   bool
	local    []rule
	equivs   map[string]string
	calls    []string
	override func(name string, args []string) (string, error, bool)
}

func newFakePolicy(t *testing.T, base policyBase) *fakePolicy {
	t.Helper()
	p := &fakePolicy{base: base, module: true, equivs: map[string]string{}}
	old := run
	run = p.run
	t.Cleanup(func() { run = old })
	return p
}

func (p *fakePolicy) run(name string, args ...string) (string, error) {
	p.calls = append(p.calls, strings.Join(append([]string{name}, args...), " "))
	if p.override != nil {
		if out, err, ok := p.override(name, args); ok {
			return out, err
		}
	}
	joined := strings.Join(args, " ")
	switch {
	case name == "semodule":
		if p.module {
			return "base\ncontainer\nsandbox 1.0", nil
		}
		return "base\nsandbox 1.0", nil
	case name == "semanage" && joined == "fcontext -l -C":
		return p.render(false), nil
	case name == "semanage" && joined == "fcontext -l":
		return p.render(true), nil
	case name == "semanage" && len(args) == 5 && args[1] == "-d" && args[2] == "-f":
		p.local = slices.DeleteFunc(p.local, func(r rule) bool { return r.spec == args[4] })
	case name == "semanage" && len(args) == 7 && args[2] == "-f":
		ftype := ""
		for k, v := range fileTypes {
			if v == args[3] {
				ftype = k
			}
		}
		i := slices.IndexFunc(p.local, func(r rule) bool { return r.spec == args[6] && r.ftype == ftype })
		switch {
		case args[1] == "-m" && i >= 0:
			p.local[i].typ = args[5]
		case args[1] == "-a" && i < 0:
			p.local = append(p.local, rule{args[6], ftype, args[5]})
		default:
			return "", fmt.Errorf("semanage %s: %s does not fit the store", args[1], args[6])
		}
	case name == "semanage" && len(args) == 5 && args[1] == "-d" && args[2] == "-e":
		delete(p.equivs, args[4])
	case name == "semanage" && len(args) == 5 && args[2] == "-e":
		p.equivs[args[4]] = args[3]
	case name == "matchpathcon":
		return "system_u:object_r:container_runtime_exec_t:s0", nil
	}
	return "", nil
}

// render prints the store as semanage fcontext -l (all) or -l -C does.
func (p *fakePolicy) render(all bool) string {
	var b strings.Builder
	b.WriteString("SELinux fcontext                                   type               Context\n\n")
	if all {
		for _, r := range p.base.rules {
			fmt.Fprintf(&b, "%-50s %-18s system_u:object_r:%s:s0\n", r.spec, r.ftype, r.typ)
		}
	}
	for _, r := range p.local {
		fmt.Fprintf(&b, "%-50s %-18s system_u:object_r:%s:s0\n", r.spec, r.ftype, r.typ)
	}
	if all {
		b.WriteString("\nSELinux Distribution fcontext Equivalence \n\n")
		for _, e := range p.base.distro {
			fmt.Fprintf(&b, "%s = %s\n", e[0], e[1])
		}
	}
	if len(p.equivs) > 0 {
		b.WriteString("\nSELinux Local fcontext Equivalence \n\n")
		for t, s := range p.equivs {
			fmt.Fprintf(&b, "%s = %s\n", t, s)
		}
	}
	return b.String()
}

func (p *fakePolicy) count(prefix string) int {
	n := 0
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (p *fakePolicy) has(call string) bool { return slices.Contains(p.calls, call) }

func (p *fakePolicy) reset() { p.calls = nil }

func writeStamp(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(version.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stampPath(), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stamped() bool {
	_, err := os.Stat(stampPath())
	return err == nil
}

func TestCurrentMode(t *testing.T) {
	for _, m := range []Mode{Disabled, Permissive, Enforcing} {
		t.Run(m.String(), func(t *testing.T) {
			fakeHost(t, m)
			if got := CurrentMode(); got != m {
				t.Errorf("CurrentMode = %v, want %v", got, m)
			}
		})
	}
}

// An enforce file that exists but cannot be read must not turn the checks off.
func TestCurrentModeFailsClosed(t *testing.T) {
	fakeHost(t, Disabled)
	if err := os.MkdirAll(filepath.Join(selinuxFS, "enforce"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := CurrentMode(); got != Enforcing {
		t.Errorf("CurrentMode with an unreadable enforce file = %v, want enforcing", got)
	}
}

// The real run separates a tool's warnings on stderr from its output.
func TestRunKeepsStderrOutOfTheOutput(t *testing.T) {
	out, err := run("sh", "-c", "echo warning >&2; echo system_u:object_r:bin_t:s0")
	if err != nil || out != "system_u:object_r:bin_t:s0" {
		t.Errorf("run = %q, %v; want only stdout", out, err)
	}
	if _, err := run("sh", "-c", "echo broken >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("run of a failing tool = %v, want an error carrying its stderr", err)
	}
}

// On a host without SELinux nothing is run: the tools are not installed there.
func TestDisabledHostRunsNothing(t *testing.T) {
	fakeHost(t, Disabled)
	p := newFakePolicy(t, el9)
	writeStamp(t)
	if err := LabelLike("/tmp/x", "/usr/local/bin/stratum"); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(Options{RequiresSELinux: true}); err != nil {
		t.Fatal(err)
	}
	if err := Restore(true, version.DataDir); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 0 {
		t.Errorf("ran %q on an SELinux-disabled host", p.calls)
	}
}

// A temp file takes the label the policy gives its destination, so the rename
// never shows the destination with the temp name's label, but only once Ensure
// has labelled the node.
func TestLabelLikeWaitsForTheStamp(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	if err := LabelLike("/usr/local/bin/.stratum.tmp-1", "/usr/local/bin/stratum"); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 0 {
		t.Errorf("labelled before Ensure labelled the node: %q", p.calls)
	}
	writeStamp(t)
	if err := LabelLike("/usr/local/bin/.stratum.tmp-1", "/usr/local/bin/stratum"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"matchpathcon -n /usr/local/bin/stratum",
		"chcon system_u:object_r:container_runtime_exec_t:s0 /usr/local/bin/.stratum.tmp-1",
	}
	if !slices.Equal(p.calls, want) {
		t.Errorf("calls = %q, want %q", p.calls, want)
	}
}

func TestLabelLikeWithNoPolicyLabel(t *testing.T) {
	fakeHost(t, Permissive)
	writeStamp(t)
	p := newFakePolicy(t, el9)
	p.override = func(name string, _ []string) (string, error, bool) {
		return "<<none>>", nil, name == "matchpathcon"
	}
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err != nil {
		t.Fatal(err)
	}
	if p.count("chcon") != 0 {
		t.Errorf("labelled a file the policy gives no label: %q", p.calls)
	}
}

// A missing matchpathcon on a labelled node, as after an opt-out that removed
// the packages before Ensure ran, only warns; the next start relabels. Any
// other failure fails the write rather than leave a wrong label in place.
func TestLabelLikeMissingToolWarnsOtherFailuresFail(t *testing.T) {
	fakeHost(t, Enforcing)
	writeStamp(t)
	p := newFakePolicy(t, el9)
	p.override = func(name string, _ []string) (string, error, bool) {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}, name == "matchpathcon"
	}
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err != nil {
		t.Fatalf("LabelLike with no matchpathcon = %v, want a warning", err)
	}
	// The warning is delivered to the writer the caller hands, not
	// unconditionally to stdout: the shared atomic writer's default routes it
	// to stderr, so a write under a protocol handler cannot inject a
	// diagnostic into its stdout stream. The stamp again first: the call
	// above fired the warning and removed it, and an unlabelled node reports
	// nothing.
	writeStamp(t)
	var warned bytes.Buffer
	if err := LabelLikeWithWriter(&warned, "/x/.f.tmp-1", "/x/f"); err != nil {
		t.Fatalf("LabelLikeWithWriter with no matchpathcon = %v, want a warning", err)
	}
	if !strings.Contains(warned.String(), "WARN: /x/f is not labelled for SELinux") {
		t.Errorf("the policy-gone warning went to %q, want it in the writer the caller handed", warned.String())
	}
	if stamped() {
		t.Error("stamp kept, so the next start would not relabel what was written unlabelled")
	}
	writeStamp(t)
	if p.count("chcon") != 0 {
		t.Errorf("labelled without a label: %q", p.calls)
	}
	p.override = func(name string, _ []string) (string, error, bool) {
		return "", errors.New("exit status 1: chcon: Operation not supported"), name == "chcon"
	}
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("LabelLike with a failing chcon = %v, want its error", err)
	}
	if !stamped() {
		t.Error("a chcon failure with the policy installed removed the stamp")
	}
}

// With container-selinux removed but matchpathcon still installed, the local
// contexts name labels the policy no longer defines, and chcon fails. That is
// the policy being gone, not a failed write: the stamp goes and Ensure decides,
// so a staged payload or an upgrade on an existing node does not stop here
// before Ensure can warn.
func TestLabelLikeWarnsWhenTheModuleIsGone(t *testing.T) {
	fakeHost(t, Enforcing)
	writeStamp(t)
	p := newFakePolicy(t, el9)
	p.module = false
	p.override = func(name string, _ []string) (string, error, bool) {
		return "", errors.New("exit status 1: chcon: failed to change context: Invalid argument"), name == "chcon"
	}
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err != nil {
		t.Fatalf("LabelLike with the container module gone = %v, want a warning", err)
	}
	if stamped() {
		t.Error("stamp kept, so the next write would fail the same way before Ensure runs")
	}
}

// Ensure checks matchpathcon before it labels the node, so it never records a
// labelled node whose writes cannot be labelled.
func TestEnsureChecksMatchpathcon(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	p.override = func(name string, _ []string) (string, error, bool) {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}, name == "matchpathcon"
	}
	if err := Ensure(Options{}); err == nil || !strings.Contains(err.Error(), "libselinux-utils") {
		t.Fatalf("Ensure = %v, want a refusal naming libselinux-utils", err)
	}
	if stamped() || p.count("semanage fcontext -a") != 0 {
		t.Errorf("labelled a node without matchpathcon: stamp=%v, %q", stamped(), p.calls)
	}
}

// A new permissive node that is not CIS only warns when its files cannot be
// labelled, as it does for a missing policy.
func TestEnsurePermissiveWarnsOnAFailure(t *testing.T) {
	fakeHost(t, Permissive)
	p := newFakePolicy(t, el9)
	p.override = func(name string, args []string) (string, error, bool) {
		return "", errors.New("exit status 1: ValueError"), name == "semanage" && len(args) > 1 && args[1] == "-a"
	}
	if err := Ensure(Options{}); err != nil {
		t.Errorf("permissive: Ensure = %v, want a warning", err)
	}
	if err := Ensure(Options{RequiresSELinux: true}); err == nil {
		t.Error("permissive under CIS: Ensure = nil, want the error")
	}
}

// A copy of a policy rule that a container-selinux update removed is deleted;
// a context the operator added is not.
func TestEnsureRemovesAStaleManagedContext(t *testing.T) {
	fakeHost(t, Enforcing)
	realRunPaths(t)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	operator := rule{"/srv/data(/.*)?", "all files", "container_file_t"}
	p.local = append(p.local, operator)
	p.base.rules = slices.DeleteFunc(slices.Clone(p.base.rules), func(r rule) bool { return strings.Contains(r.spec, "shm") })
	p.reset()
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if !p.has("semanage fcontext -d -f a /var/run/stratum/containerd/[^/]*/sandboxes/[^/]*/shm(/.*)?") {
		t.Errorf("stale copy not removed: %q", p.calls)
	}
	if p.count("semanage fcontext -d -f a /srv/data") != 0 || !slices.Contains(p.local, operator) {
		t.Errorf("removed a context the operator added: %q", p.calls)
	}
}

// Without container-selinux a new node is refused when enforcing or under
// CIS, and warned otherwise; an opted-out node is not touched at all.
func TestEnsureWithoutContainerSELinux(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    Mode
		o       Options
		wantErr []string
	}{
		{"enforcing", Enforcing, Options{}, []string{"container-selinux", "spec.selinux: false"}},
		{"enforcing under CIS", Enforcing, Options{RequiresSELinux: true, Profile: "cis"}, []string{"container-selinux", "the cis profile requires it", "no opt-out"}},
		{"permissive under CIS", Permissive, Options{RequiresSELinux: true, Profile: "cis"}, []string{"container-selinux", "the cis profile"}},
		{"enforcing under another profile", Enforcing, Options{RequiresSELinux: true, Profile: "edge"}, []string{"container-selinux", "the edge profile requires it"}},
		{"permissive", Permissive, Options{}, nil},
		{"existing node", Enforcing, Options{Existing: true}, nil},
		{"existing node under CIS", Enforcing, Options{RequiresSELinux: true, Existing: true}, nil},
		{"opted out", Enforcing, Options{OptOut: true}, nil},
		{"opted out under CIS", Enforcing, Options{RequiresSELinux: true, Profile: "cis", OptOut: true}, []string{"spec.selinux: false", "the cis profile cannot"}},
		{"opted out under another profile", Enforcing, Options{RequiresSELinux: true, Profile: "edge", OptOut: true}, []string{"the edge profile cannot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHost(t, tc.mode)
			writeStamp(t)
			p := newFakePolicy(t, el9)
			p.module = false
			err := Ensure(tc.o)
			if len(tc.wantErr) == 0 && err != nil {
				t.Fatalf("Ensure = %v, want nil", err)
			}
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Ensure = nil, want an error naming %q", tc.wantErr)
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not name %q", err, w)
					}
				}
			}
			if n := p.count("semanage fcontext -a") + p.count("restorecon"); n != 0 {
				t.Errorf("changed the host without the policy: %q", p.calls)
			}
			if tc.o.OptOut && !tc.o.RequiresSELinux && len(p.calls) != 0 {
				t.Errorf("an opted-out node ran %q", p.calls)
			}
			if stamped() && !(tc.o.OptOut && tc.o.RequiresSELinux) {
				t.Error("left the stamp, so LabelLike keeps labelling a node that is not labelled")
			}
		})
	}
}

// Prerequisite refuses the nodes Ensure refuses for a missing policy, and
// passes the ones it warns or runs, without touching the host.
func TestPrerequisite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    Mode
		o       Options
		module  bool
		toolErr bool
		wantErr []string
	}{
		{"enforcing", Enforcing, Options{}, false, false, []string{"container-selinux", "spec.selinux: false"}},
		{"enforcing under CIS", Enforcing, Options{RequiresSELinux: true, Profile: "cis"}, false, false, []string{"container-selinux", "no opt-out"}},
		{"permissive under CIS", Permissive, Options{RequiresSELinux: true, Profile: "cis"}, false, false, []string{"container-selinux", "cis profile"}},
		{"opted out under CIS", Enforcing, Options{RequiresSELinux: true, Profile: "cis", OptOut: true}, false, false, []string{"spec.selinux: false", "cis profile"}},
		// The opt-out refusal comes before the Disabled pass, as in Ensure.
		{"disabled, opted out under CIS", Disabled, Options{RequiresSELinux: true, Profile: "cis", OptOut: true}, false, false, []string{"spec.selinux: false", "cis profile"}},
		// An existing node is refused like a new one: the upgrade checks nodes
		// that already run.
		{"enforcing, existing node", Enforcing, Options{Existing: true}, false, false, []string{"container-selinux", "spec.selinux: false"}},
		{"permissive", Permissive, Options{}, false, false, nil},
		{"opted out", Enforcing, Options{OptOut: true}, false, false, nil},
		{"disabled", Disabled, Options{}, false, false, nil},
		{"module present", Enforcing, Options{}, true, false, nil},
		{"tool failure", Enforcing, Options{}, false, true, []string{"could not tell", "policy store locked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHost(t, tc.mode)
			writeStamp(t)
			p := newFakePolicy(t, el9)
			p.module = tc.module
			if tc.toolErr {
				p.override = func(name string, _ []string) (string, error, bool) {
					return "", errors.New("exit status 1: ValueError: policy store locked"), name == "semodule"
				}
			}
			err := Prerequisite(tc.o)
			if len(tc.wantErr) == 0 && err != nil {
				t.Fatalf("Prerequisite = %v, want nil", err)
			}
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Prerequisite = nil, want an error naming %q", tc.wantErr)
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not name %q", err, w)
					}
				}
			}
			if n := p.count("semanage fcontext -a") + p.count("restorecon"); n != 0 {
				t.Errorf("changed the host: %q", p.calls)
			}
			if !stamped() {
				t.Error("removed the stamp, so a node that opts out later starts unlabelled")
			}
		})
	}
}

// A tool failure that is not a missing package answers the question neither
// way: Prerequisite returns it wrapped in ErrCannotTell, so the upgrade path
// can warn and let the node through instead of passing it silently.
func TestPrerequisiteCannotTell(t *testing.T) {
	fakeHost(t, Enforcing)
	writeStamp(t)
	p := newFakePolicy(t, el9)
	p.override = func(name string, _ []string) (string, error, bool) {
		return "", errors.New("exit status 1: ValueError: policy store locked"), name == "semodule"
	}
	err := Prerequisite(Options{})
	if !errors.Is(err, ErrCannotTell) {
		t.Fatalf("Prerequisite = %v, want the failure wrapped in ErrCannotTell", err)
	}
	if !strings.Contains(err.Error(), "policy store locked") {
		t.Errorf("the error does not name the failure: %v", err)
	}
	if n := p.count("semanage fcontext -a") + p.count("restorecon"); n != 0 {
		t.Errorf("changed the host: %q", p.calls)
	}
	if !stamped() {
		t.Error("removed the stamp, so a node that opts out later starts unlabelled")
	}
}

// A node with the container policy, semodule and matchpathcon but no
// semanage is refused like one without the policy: Ensure registers
// Stratum's contexts with semanage, so the upgrade would leave it unlabelled.
// A permissive node without it only warns at its start, so it passes.
func TestPrerequisiteNamesAMissingSemanage(t *testing.T) {
	for _, tc := range []struct {
		mode   Mode
		o      Options
		refuse bool
	}{
		{Enforcing, Options{}, true},
		{Permissive, Options{RequiresSELinux: true, Profile: "cis"}, true},
		{Permissive, Options{}, false},
	} {
		fakeHost(t, tc.mode)
		writeStamp(t)
		p := newFakePolicy(t, el9)
		p.override = func(name string, _ []string) (string, error, bool) {
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}, name == "semanage"
		}
		err := Prerequisite(tc.o)
		if !tc.refuse {
			if err != nil {
				t.Errorf("%s %+v: Prerequisite = %v, want nil", tc.mode, tc.o, err)
			}
			continue
		}
		if err == nil || errors.Is(err, ErrCannotTell) || !strings.Contains(err.Error(), "policycoreutils-python-utils") {
			t.Errorf("%s %+v: Prerequisite = %v, want a refusal naming policycoreutils-python-utils", tc.mode, tc.o, err)
		}
		if n := p.count("semanage fcontext -a") + p.count("restorecon"); n != 0 {
			t.Errorf("changed the host: %q", p.calls)
		}
	}
}

// Prerequisite refuses with the message Ensure refuses a new node with.
func TestPrerequisiteMatchesEnsure(t *testing.T) {
	for _, o := range []Options{{}, {RequiresSELinux: true, Profile: "cis"}} {
		fakeHost(t, Enforcing)
		writeStamp(t)
		p := newFakePolicy(t, el9)
		p.module = false
		ensureErr := Ensure(o)
		prereqErr := Prerequisite(o)
		if ensureErr == nil || prereqErr == nil {
			t.Fatalf("Ensure = %v, Prerequisite = %v, want both refused", ensureErr, prereqErr)
		}
		if ensureErr.Error() != prereqErr.Error() {
			t.Errorf("Ensure = %q, Prerequisite = %q, want the same refusal", ensureErr, prereqErr)
		}
	}
}

func TestEnsureNamesAMissingSemanage(t *testing.T) {
	for _, existing := range []bool{false, true} {
		fakeHost(t, Enforcing)
		p := newFakePolicy(t, el9)
		p.override = func(name string, _ []string) (string, error, bool) {
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}, name == "semanage"
		}
		err := Ensure(Options{Existing: existing})
		if existing && err != nil {
			t.Errorf("existing node: Ensure = %v, want a warning", err)
		}
		if !existing && (err == nil || !strings.Contains(err.Error(), "policycoreutils-python-utils")) {
			t.Errorf("new node: Ensure = %v, want an error naming policycoreutils-python-utils", err)
		}
	}
}

// A policy tool that fails is reported as a failure, not as a missing package
// to install.
func TestEnsureToolFailureIsNotAMissingPackage(t *testing.T) {
	for _, existing := range []bool{false, true} {
		fakeHost(t, Enforcing)
		p := newFakePolicy(t, el9)
		p.override = func(name string, args []string) (string, error, bool) {
			return "", errors.New("exit status 1: ValueError: policy store locked"), name == "semanage" && strings.Join(args, " ") == "fcontext -l"
		}
		err := Ensure(Options{Existing: existing})
		if existing && err != nil {
			t.Errorf("existing node: Ensure = %v, want a warning", err)
		}
		if !existing && (err == nil || !strings.Contains(err.Error(), "policy store locked") || strings.Contains(err.Error(), "Install it")) {
			t.Errorf("new node: Ensure = %v, want the tool's error without an install remedy", err)
		}
	}
}

// A host with none of Stratum's contexts gets every one, then a full relabel
// that stays on each root's filesystem, and the stamp.
func TestEnsureRegistersEveryContextAndRelabels(t *testing.T) {
	for _, m := range []Mode{Enforcing, Permissive} {
		t.Run(m.String(), func(t *testing.T) {
			fakeHost(t, m)
			p := newFakePolicy(t, el9)
			if err := Ensure(Options{}); err != nil {
				t.Fatal(err)
			}
			for _, r := range rules() {
				if want := "semanage fcontext -a -f a -t " + r.typ + " " + r.spec; !p.has(want) {
					t.Errorf("no %q in %q", want, p.calls)
				}
			}
			distro := map[string]string{}
			for _, e := range el9.distro {
				distro[e[0]] = e[1]
			}
			for _, e := range equivalences() {
				if want := "semanage fcontext -a -e " + canonical(e.source, distro) + " " + e.target; !p.has(want) {
					t.Errorf("no %q in %q", want, p.calls)
				}
			}
			full := "restorecon -R -x -i -T 0 " + strings.Join([]string{version.StratumInstallPath, version.SystemdUnitPath, version.DataDir, version.RunDir, LocalPathDir}, " ")
			if !p.has(full) {
				t.Errorf("no %q in %q", full, p.calls)
			}
			if fi, err := os.Stat(LocalPathDir); err != nil || !fi.IsDir() {
				t.Errorf("local-path directory not created: %v", err)
			}
			if !stamped() {
				t.Error("no stamp after a full relabel")
			}
		})
	}
}

// With the real run paths, a second start on a labelled node registers
// nothing and relabels only what an upgrade or an operator may replace.
func TestEnsureSecondStartIsANoop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		base    policyBase
		want    []string
		without []string
	}{
		{"rocky 9", el9, []string{
			"semanage fcontext -a -f a -t container_var_run_t /var/run/stratum(/.*)?",
			"semanage fcontext -a -f a -t container_unit_file_t /usr/lib/systemd/system/stratum.*",
			"semanage fcontext -a -f a -t container_var_run_t /var/run/stratum/containerd(/.*)?",
			"semanage fcontext -a -f a -t container_runtime_tmpfs_t /var/run/stratum/containerd/[^/]*/sandboxes/[^/]*/shm(/.*)?",
		}, []string{
			"semanage fcontext -a -f a -t container_var_run_t /run/stratum(/.*)?",
			"semanage fcontext -a -e /var/run/containerd /var/run/stratum/containerd",
		}},
		{"rocky 10", el10, []string{
			"semanage fcontext -a -f a -t container_var_run_t /run/stratum(/.*)?",
			"semanage fcontext -a -f a -t container_unit_file_t /usr/lib/systemd/system/stratum.*",
			"semanage fcontext -a -e /run/containerd /run/stratum/containerd",
		}, []string{
			"semanage fcontext -a -f a -t container_runtime_tmpfs_t /run/stratum/containerd/[^/]*/sandboxes/[^/]*/shm(/.*)?",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHost(t, Enforcing)
			realRunPaths(t)
			p := newFakePolicy(t, tc.base)
			if err := Ensure(Options{}); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !p.has(w) {
					t.Errorf("no %q in %q", w, p.calls)
				}
			}
			for _, w := range tc.without {
				if p.has(w) {
					t.Errorf("unexpected %q", w)
				}
			}

			p.reset()
			if err := Ensure(Options{}); err != nil {
				t.Fatal(err)
			}
			if n := p.count("semanage fcontext -a") + p.count("semanage fcontext -m"); n != 0 {
				t.Errorf("second start changed contexts already in place: %q", p.calls)
			}
			if p.count("restorecon -R -x -i -T 0 "+version.StratumInstallPath) != 0 {
				t.Errorf("second start ran the full relabel again: %q", p.calls)
			}
			if !p.has("restorecon -R -x -i -T 0 " + version.BinDir + " " + version.CoreDNSUpstreamDir) {
				t.Errorf("second start did not relabel the binaries: %q", p.calls)
			}
		})
	}
}

// A full relabel that fails is retried on the next start, though the contexts
// are registered by then.
func TestEnsureRetriesAFailedRelabel(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	p.override = func(name string, args []string) (string, error, bool) {
		return "", errors.New("exit status 1"), name == "restorecon" && slices.Contains(args, version.DataDir)
	}
	if err := Ensure(Options{Existing: true}); err != nil {
		t.Fatalf("existing node: Ensure = %v, want a warning", err)
	}
	if stamped() {
		t.Fatal("stamp written though the full relabel failed")
	}
	p.override = nil
	p.reset()
	if err := Ensure(Options{Existing: true}); err != nil {
		t.Fatal(err)
	}
	if p.count("semanage fcontext -a") != 0 {
		t.Errorf("registered contexts again: %q", p.calls)
	}
	if p.count("restorecon -R -x -i -T 0 "+version.StratumInstallPath) != 1 || !stamped() {
		t.Errorf("full relabel not retried: %q", p.calls)
	}
}

// Opting out of a labelled node removes the stamp, so the writers stop
// labelling, and runs no tool.
func TestEnsureOptOutStopsLabelling(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	p.reset()
	if err := Ensure(Options{OptOut: true}); err != nil {
		t.Fatal(err)
	}
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err != nil {
		t.Fatal(err)
	}
	if stamped() || len(p.calls) != 0 {
		t.Errorf("opted out, but stamp=%v and ran %q", stamped(), p.calls)
	}
}

// A context with another type, or an equivalence to another path, is
// corrected in place: semanage refuses to add one that exists.
func TestEnsureCorrectsADifferingContext(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	p.local = []rule{{version.StratumInstallPath, "all files", "bin_t"}}
	p.equivs[version.ContainerdRootDir] = "/srv/containerd"
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"semanage fcontext -m -f a -t container_runtime_exec_t " + version.StratumInstallPath,
		"semanage fcontext -m -e /var/lib/containerd " + version.ContainerdRootDir,
	} {
		if !p.has(want) {
			t.Errorf("no %q in %q", want, p.calls)
		}
	}
}

// effective is the type libselinux gives path from local rules: an exact path
// first, then the newest regular expression that matches.
func effective(local []rule, path string) string {
	for _, r := range local {
		if stem(r.spec) == r.spec && r.spec == path {
			return r.typ
		}
	}
	for i := len(local) - 1; i >= 0; i-- {
		if regexp.MustCompile("^(?:" + local[i].spec + ")$").MatchString(path) {
			return local[i].typ
		}
	}
	return ""
}

// A broad context registered again, after an operator removed it, comes after
// the narrower contexts under it, and libselinux takes the newest match: the
// narrower ones are registered again after it, so binaries and sandbox shm keep
// their types.
func TestEnsureKeepsNarrowerContextsAheadOfARepairedBroadOne(t *testing.T) {
	fakeHost(t, Enforcing)
	realRunPaths(t)
	p := newFakePolicy(t, el9)
	check := func(when string) {
		t.Helper()
		for path, want := range map[string]string{
			filepath.Join(version.BinDir, "containerd"):                    "container_runtime_exec_t",
			filepath.Join(version.DataDir, "pki", "ca.crt"):                "container_var_lib_t",
			filepath.Join(version.UpgradeStageDir, "stratum-v1.37.2"):      "container_runtime_exec_t",
			filepath.Join(version.UpgradeStageDir, "staged", "bin", "x"):   "container_var_lib_t",
			filepath.Join(version.UpgradeStageDir, "manifests-bak-1", "y"): "container_var_lib_t",
			"/var/run/stratum/containerd/io.containerd/sandboxes/abc/shm":  "container_runtime_tmpfs_t",
			"/var/run/stratum/containerd/io.containerd/runtime/task/x":     "container_var_run_t",
		} {
			if got := effective(p.local, path); got != want {
				t.Errorf("%s: %s is %s, want %s", when, path, got, want)
			}
		}
	}
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	check("first start")
	p.local = slices.DeleteFunc(p.local, func(r rule) bool {
		return r.spec == version.DataDir+"(/.*)?" || r.spec == "/var/run/stratum(/.*)?"
	})
	p.reset()
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	check("after repairing the broad contexts")
	if p.count("semanage fcontext -d -f a "+version.BinDir) != 1 {
		t.Errorf("the binaries' context was not registered again after the data directory's: %q", p.calls)
	}
}

// On Rocky 9 an equivalence an earlier start registered for the containerd
// state is removed before the rules that replace it, which semanage would
// otherwise refuse beneath it.
func TestEnsureReplacesAnIneffectiveEquivalence(t *testing.T) {
	fakeHost(t, Enforcing)
	realRunPaths(t)
	p := newFakePolicy(t, el9)
	p.equivs["/var/run/stratum/containerd"] = "/var/run/containerd"
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	del := slices.Index(p.calls, "semanage fcontext -d -e /var/run/containerd /var/run/stratum/containerd")
	add := slices.Index(p.calls, "semanage fcontext -a -f a -t container_runtime_tmpfs_t /var/run/stratum/containerd/[^/]*/sandboxes/[^/]*/shm(/.*)?")
	if del < 0 || add < 0 || del > add {
		t.Errorf("equivalence not removed before the shm rule (delete at %d, add at %d): %q", del, add, p.calls)
	}
	if _, ok := p.equivs["/var/run/stratum/containerd"]; ok {
		t.Error("ineffective equivalence left in place")
	}
}

// A node that was running before only warns when a context cannot be
// registered; a new node stops with the error. Either way nothing is
// relabelled with contexts that failed to register, and the next start that
// registers them relabels everything.
func TestEnsureRegistrationFailure(t *testing.T) {
	for _, existing := range []bool{true, false} {
		fakeHost(t, Enforcing)
		p := newFakePolicy(t, el9)
		p.override = func(name string, args []string) (string, error, bool) {
			return "", errors.New("exit status 1: ValueError: File spec conflicts with equivalency rule"), name == "semanage" && len(args) > 1 && args[1] == "-a"
		}
		err := Ensure(Options{Existing: existing})
		if existing && err != nil {
			t.Errorf("existing node: Ensure = %v, want a warning", err)
		}
		if !existing && (err == nil || !strings.Contains(err.Error(), "equivalency")) {
			t.Errorf("new node: Ensure = %v, want semanage's error", err)
		}
		if p.count("restorecon") != 0 {
			t.Errorf("relabelled with contexts that failed to register: %q", p.calls)
		}
		if _, err := os.Stat(pendingPath()); err != nil {
			t.Errorf("no pending relabel left for the next start: %v", err)
		}
	}
}

// A start with SELinux disabled drops the stamp: files written while it was
// off have no labels, so the next start with it on relabels everything.
func TestEnsureForgetsTheStampWhileSELinuxIsDisabled(t *testing.T) {
	fakeHost(t, Disabled)
	writeStamp(t)
	newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if stamped() {
		t.Error("stamp kept through a start with SELinux disabled")
	}
}

// A narrower context listed before a broader one, as an update interrupted
// after re-adding the broad context leaves them, is registered again after it,
// though every type already matches.
func TestEnsureRepairsAnInterruptedReordering(t *testing.T) {
	fakeHost(t, Enforcing)
	realRunPaths(t)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	broad := slices.IndexFunc(p.local, func(r rule) bool { return r.spec == version.DataDir+"(/.*)?" })
	moved := p.local[broad]
	p.local = append(slices.Delete(p.local, broad, broad+1), moved)
	if got := effective(p.local, filepath.Join(version.BinDir, "containerd")); got != "container_var_lib_t" {
		t.Fatalf("setup: binaries are %s, want them shadowed", got)
	}
	p.reset()
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := effective(p.local, filepath.Join(version.BinDir, "containerd")); got != "container_runtime_exec_t" {
		t.Errorf("after the repair binaries are %s, want container_runtime_exec_t: %q", got, p.calls)
	}
	if p.count("semanage fcontext -d -f a "+version.DataDir+"(/.*)?") != 0 {
		t.Errorf("moved the broad context, not the narrower one: %q", p.calls)
	}
}

// The CIS profile refuses spec.selinux: false on every host, so a worker cannot
// join with it where SELinux is disabled and keep it.
func TestEnsureRefusesCISOptOutWhereSELinuxIsDisabled(t *testing.T) {
	fakeHost(t, Disabled)
	newFakePolicy(t, el9)
	if err := Ensure(Options{RequiresSELinux: true, OptOut: true}); err == nil || !strings.Contains(err.Error(), "spec.selinux: false") {
		t.Fatalf("Ensure = %v, want the CIS refusal", err)
	}
}

// A path that exists but cannot be inspected is an error: skipping it could
// record a relabel that never happened.
func TestRestoreReportsAnUninspectablePath(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	locked := filepath.Join(version.RunDir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root can inspect any path")
	}
	if err := Restore(true, filepath.Join(locked, "x")); err == nil {
		t.Errorf("Restore = nil for a path that cannot be inspected; ran %q", p.calls)
	}
}

// The real run uses the C locale, so semanage's section headings are English.
func TestRunUsesTheCLocale(t *testing.T) {
	out, err := run("sh", "-c", "echo $LC_ALL")
	if err != nil || out != "C" {
		t.Errorf("run's LC_ALL = %q, %v; want C", out, err)
	}
}

func TestRestoreSkipsMissingPaths(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	if err := Restore(true, version.RunDir, filepath.Join(version.RunDir, "absent")); err != nil {
		t.Fatal(err)
	}
	if err := Restore(false, filepath.Join(version.RunDir, "absent")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"restorecon -R -x -i -T 0 " + version.RunDir}; !slices.Equal(p.calls, want) {
		t.Errorf("calls = %q, want %q", p.calls, want)
	}
}

// semanage refuses a rule beneath a distribution equivalence's target, so each
// path is registered as the policy labels it.
func TestCanonical(t *testing.T) {
	for name, tc := range map[string]struct {
		base policyBase
		want map[string]string
	}{
		"rocky 9": {el9, map[string]string{
			"/run/stratum(/.*)?":            "/var/run/stratum(/.*)?",
			"/run/stratum/containerd":       "/var/run/stratum/containerd",
			"/run/lock/stratum":             "/var/lock/stratum",
			"/etc/systemd/system/stratum.*": "/usr/lib/systemd/system/stratum.*",
			"/run":                          "/var/run",
			"/runner":                       "/runner",
			"/var/lib/stratum(/.*)?":        "/var/lib/stratum(/.*)?",
		}},
		"rocky 10": {el10, map[string]string{
			"/run/stratum(/.*)?":      "/run/stratum(/.*)?",
			"/run/stratum/containerd": "/run/stratum/containerd",
			"/var/run/containerd":     "/run/containerd",
		}},
	} {
		distro := map[string]string{}
		for _, e := range tc.base.distro {
			distro[e[0]] = e[1]
		}
		for in, want := range tc.want {
			if got := canonical(in, distro); got != want {
				t.Errorf("%s: canonical(%s) = %s, want %s", name, in, got, want)
			}
		}
	}
}

func TestParseListing(t *testing.T) {
	out := `SELinux fcontext                                   type               Context

/usr/local/bin/stratum                             all files          system_u:object_r:container_runtime_exec_t:s0
/srv/data                                          directory          system_u:object_r:container_file_t:s0
/srv/none                                          all files          <<None>>

SELinux Distribution fcontext Equivalence

/run = /var/run

SELinux Local fcontext Equivalence

/var/lib/stratum/containerd = /var/lib/containerd
`
	l := parseListing(out)
	want := []rule{
		{"/usr/local/bin/stratum", "all files", "container_runtime_exec_t"},
		{"/srv/data", "directory", "container_file_t"},
	}
	if !slices.Equal(l.rules, want) {
		t.Errorf("rules = %v, want %v", l.rules, want)
	}
	if l.distro["/run"] != "/var/run" || len(l.distro) != 1 {
		t.Errorf("distro = %v", l.distro)
	}
	if l.equivs["/var/lib/stratum/containerd"] != "/var/lib/containerd" || len(l.equivs) != 1 {
		t.Errorf("equivs = %v", l.equivs)
	}
}

// A failure on a labelled node keeps the stamp, so the node stays labelled and
// its containers confined, and the pending marker makes the next start run
// the full relabel again.
func TestEnsureFailureKeepsALabelledNodeLabelled(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	p.local = nil
	p.override = func(name string, args []string) (string, error, bool) {
		return "", errors.New("exit status 1: policy store locked"), name == "semanage" && len(args) > 1 && args[1] == "-a"
	}
	if err := Ensure(Options{Existing: true}); err != nil {
		t.Fatalf("existing node: Ensure = %v, want a warning", err)
	}
	if on, _ := labelling(); !on {
		t.Error("a failed registration took the node out of confinement")
	}

	p.override = func(name string, args []string) (string, error, bool) {
		return "", errors.New("exit status 1"), name == "restorecon" && slices.Contains(args, version.DataDir)
	}
	if err := Ensure(Options{Existing: true}); err != nil {
		t.Fatalf("existing node: Ensure = %v, want a warning", err)
	}
	if on, _ := labelling(); !on {
		t.Error("a failed relabel took the node out of confinement")
	}
	p.override = nil
	p.reset()
	if err := Ensure(Options{Existing: true}); err != nil {
		t.Fatal(err)
	}
	if p.count("restorecon -R -x -i -T 0 "+version.StratumInstallPath) != 1 {
		t.Errorf("the unfinished relabel was not run again: %q", p.calls)
	}
	if _, err := os.Stat(pendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending marker left after a finished relabel: %v", err)
	}
}

// A container-selinux update that changes the rules behind an equivalence
// relabels the tree labelled through it, though Stratum's own contexts did not
// change.
func TestEnsureRelabelsWhenThePolicyChanges(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	p.base.rules = append(slices.Clone(p.base.rules), rule{"/var/lib/containerd(/.*)?", "all files", "container_var_lib_t"})
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	p.base.rules = append(slices.Clone(p.base.rules), rule{"/var/lib/containerd/[^/]*/snapshots(/.*)?", "all files", "container_file_t"})
	p.reset()
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if p.count("semanage fcontext -a") != 0 || p.count("restorecon -R -x -i -T 0 "+version.StratumInstallPath) != 1 {
		t.Errorf("a policy change did not relabel, or re-registered contexts: %q", p.calls)
	}
}

// A stamp recording other contexts, as after an upgrade that adds one before
// it is registered, makes the next start relabel everything.
func TestEnsureRelabelsOnAStaleStamp(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stampPath(), []byte("something else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.reset()
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if p.count("semanage fcontext -a") != 0 || p.count("restorecon -R -x -i -T 0 "+version.StratumInstallPath) != 1 {
		t.Errorf("a stale stamp did not relabel, or re-registered contexts: %q", p.calls)
	}
}

// A stamp that cannot be inspected fails the write rather than letting it
// through unlabelled.
func TestLabelLikeFailsOnAnUninspectableStamp(t *testing.T) {
	fakeHost(t, Enforcing)
	newFakePolicy(t, el9)
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	version.DataDir = notDir
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err == nil {
		t.Error("LabelLike = nil with a stamp that cannot be inspected")
	}
}

func TestForgetLabels(t *testing.T) {
	fakeHost(t, Enforcing)
	writeStamp(t)
	if err := ForgetLabels(); err != nil {
		t.Fatal(err)
	}
	if stamped() {
		t.Error("stamp kept")
	}
}

// A file written while SELinux is disabled gets no label, so the stamp goes
// at once: a start that fails before Ensure runs must not leave the next
// enabled start trusting it.
func TestLabelLikeForgetsTheStampWhileSELinuxIsDisabled(t *testing.T) {
	fakeHost(t, Disabled)
	writeStamp(t)
	p := newFakePolicy(t, el9)
	if err := LabelLike("/x/.f.tmp-1", "/x/f"); err != nil {
		t.Fatal(err)
	}
	if stamped() {
		t.Error("stamp kept through a write with SELinux disabled")
	}
	if len(p.calls) != 0 {
		t.Errorf("ran policy tools with SELinux disabled: %q", p.calls)
	}
}

// A recursive restore excludes every directory mounted beneath its roots,
// bind mounts from the same filesystem included, since restorecon -x stops
// only where the device changes; mounts elsewhere, and the roots themselves,
// are not excluded.
func TestRestoreExcludesMountsBeneathItsRoots(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	root := filepath.Join(version.DataDir, "kubelet")
	nested := filepath.Join(root, "pods", "u1", "volume-subpaths", "with space")
	other := t.TempDir()
	for _, d := range []string{nested, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	esc := strings.ReplaceAll(nested, " ", `\040`)
	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := "22 1 253:0 / / rw - xfs /dev/vda1 rw\n" +
		"30 22 253:0 /k " + root + " rw - xfs /dev/vda1 rw\n" +
		"31 22 253:0 /srv/data " + esc + " rw - xfs /dev/vda1 rw\n" +
		"32 22 253:0 /x " + other + " rw - xfs /dev/vda1 rw\n"
	if err := os.WriteFile(info, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountInfo
	mountInfo = info
	t.Cleanup(func() { mountInfo = old })
	if err := Restore(true, root); err != nil {
		t.Fatal(err)
	}
	want := "restorecon -R -x -i -T 0 -e " + nested + " " + root
	if !p.has(want) {
		t.Errorf("no %q in %q", want, p.calls)
	}
}

// CoreDNS's upstream directory exists, and is relabelled, from the first
// start: created after the one full relabel, it would keep the data
// directory's type, which CoreDNS may not read.
func TestEnsureCreatesTheDNSDirBeforeTheRelabel(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	p.override = func(name string, args []string) (string, error, bool) {
		if name == "restorecon" && slices.Contains(args, version.DataDir) {
			if _, err := os.Stat(version.CoreDNSUpstreamDir); err != nil {
				t.Errorf("full relabel ran before %s existed", version.CoreDNSUpstreamDir)
			}
		}
		return "", nil, false
	}
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
}

// The policy tools run as root, so nothing of the caller's environment but
// what toolenv names may reach them.
func TestRunEnvIsExplicit(t *testing.T) {
	t.Setenv("XTABLES_LIBDIR", "/evil")
	t.Setenv("TC_LIB_DIR", "/evil")

	out, err := run("env")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(out, "\n")
	slices.Sort(got)
	if want := []string{"LC_ALL=C", "PATH=" + toolenv.Path}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want exactly %q", got, want)
	}
}

// A shim of a policy tool first on the caller's PATH must not be what runs.
func TestRunIgnoresTheCallersPath(t *testing.T) {
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "env"), []byte("#!/bin/sh\necho SHIM\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim)

	out, err := run("env")
	if err != nil || strings.Contains(out, "SHIM") {
		t.Errorf("run(env) = %q, %v; want the tool from %s, not the shim", out, err, toolenv.Path)
	}
}
