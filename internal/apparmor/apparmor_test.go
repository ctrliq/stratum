package apparmor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// ubuntu2404Runc is the runc profile apparmor 4.0.1 ships, as Ubuntu 24.04
// does: abi 4.0, /usr/sbin/runc only.
const ubuntu2404Runc = `# This profile allows everything and only exists to give the
# application a name instead of having the label "unconfined"

abi <abi/4.0>,
include <tunables/global>

profile runc /usr/sbin/runc flags=(unconfined) {
  userns,

  # Site-specific additions and overrides. See local/README for details.
  include if exists <local/runc>
}
`

// ubuntuRunc is the runc profile Ubuntu 26.04's apparmor package ships.
const ubuntuRunc = `# This profile allows everything and only exists to give the
# application a name instead of having the label "unconfined"

abi <abi/5.0>,
include <tunables/global>

profile runc /usr/{bin,sbin}/runc flags=(unconfined) {
  userns,
  @{exec_path} mr,

  # Site-specific additions and overrides. See local/README for details.
  include if exists <local/runc>
}
`

const (
	coveredLabel   = "runc (unconfined)"
	uncoveredLabel = "unconfined"
)

type parseCall struct {
	args    []string
	profile string
}

// fakeHost stands in for a host's AppArmor: its kernel settings, the host's
// profile files, the label Stratum's runc starts with, and an apparmor_parser
// whose loads and removals change that label the way the kernel would.
type fakeHost struct {
	t     *testing.T
	root  string
	calls []parseCall
	label string
	// parseErr fails every parse, labelErr every label read; ignoreLoad
	// leaves the label as it was after a load or a removal.
	parseErr, labelErr error
	ignoreLoad         bool
	// noHash makes a load leave no hash, as a kernel that keeps none does.
	noHash bool
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, root: t.TempDir(), label: uncoveredLabel}
	saved := []*string{&enabledPath, &securityfsPath, &restrictPath, &hostProfile, &hostDisabled, &procDir}
	old := make([]string, len(saved))
	for i, p := range saved {
		old[i] = *p
	}
	oldParse, oldLabel := parse, runcLabel
	t.Cleanup(func() {
		for i, p := range saved {
			*p = old[i]
		}
		parse, runcLabel = oldParse, oldLabel
		loaded = loadState{}
	})
	loaded = loadState{}
	enabledPath = filepath.Join(h.root, "enabled")
	securityfsPath = filepath.Join(h.root, "securityfs")
	procDir = filepath.Join(h.root, "proc")
	restrictPath = filepath.Join(h.root, "restrict")
	hostProfile = filepath.Join(h.root, "apparmor.d", "runc")
	hostDisabled = filepath.Join(h.root, "apparmor.d", "disable", "runc")
	parse = h.parse
	runcLabel = func() (string, error) { return h.label, h.labelErr }
	h.write(enabledPath, "Y\n")
	if err := os.Mkdir(securityfsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *fakeHost) write(path, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// restricted turns the restriction on.
func (h *fakeHost) restricted() { h.write(restrictPath, "1\n") }

// attachLine finds the values of attachVar in a loaded profile; the fake
// parser reads the header's attachment when it has none.
var attachLine = regexp.MustCompile(`(?m)^@\{stratum_runc_attach\} = (.*)$`)

func (h *fakeHost) parse(_ context.Context, profile []byte, args ...string) error {
	h.calls = append(h.calls, parseCall{args: args, profile: string(profile)})
	if h.parseErr != nil {
		return h.parseErr
	}
	kernelDir := filepath.Join(securityfsPath, "policy", "profiles", "runc.12")
	switch {
	case h.ignoreLoad, slices.Contains(args, "--skip-kernel-load"):
	case slices.Contains(args, "--remove"):
		h.label = uncoveredLabel
		if err := os.RemoveAll(kernelDir); err != nil {
			h.t.Fatal(err)
		}
	default:
		h.write(filepath.Join(kernelDir, "name"), "runc\n")
		if !h.noHash {
			h.write(filepath.Join(kernelDir, "raw_sha256"), fmt.Sprintf("%x\n", sha256.Sum256(profile)))
		}
		attach := ""
		if m := attachLine.FindSubmatch(profile); m != nil {
			attach = string(m[1])
		} else if m := header.FindSubmatch(profile); m != nil {
			attach = string(m[2])
		}
		h.label = uncoveredLabel
		if slices.Contains(strings.Fields(attach), "/var/lib/stratum/bin/runc") {
			h.label = coveredLabel
		}
	}
	return nil
}

func TestRestrictedReadsAppArmorAndTheRestriction(t *testing.T) {
	for name, tc := range map[string]struct {
		enabled, restrict string // "" leaves the file out
		want              bool
	}{
		"no AppArmor":             {"", "1", false},
		"AppArmor disabled":       {"N", "1", false},
		"kernel without the knob": {"Y", "", false},
		"restriction off":         {"Y", "0", false},
		"restriction on":          {"Y", "1", true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			if err := os.Remove(enabledPath); err != nil {
				t.Fatal(err)
			}
			if tc.enabled != "" {
				h.write(enabledPath, tc.enabled+"\n")
			}
			if tc.restrict != "" {
				h.write(restrictPath, tc.restrict+"\n")
			}
			got, err := Restricted()
			if err != nil || got != tc.want {
				t.Errorf("Restricted() = %v, %v, want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestCoversReadsTheLabelRuncStartsWith(t *testing.T) {
	for label, want := range map[string]bool{
		coveredLabel:                       true,
		"runc (enforce)":                   true,
		uncoveredLabel:                     false,
		"runc-stratum (unconfined)":        false,
		"cri-containerd.apparmor.d//&runc": false,
		"crun (unconfined)":                false,
	} {
		h := newFakeHost(t)
		h.label = label
		if got, err := Covers(); err != nil || got != want {
			t.Errorf("Covers() with runc labelled %q = %v, %v, want %v, nil", label, got, err, want)
		}
	}
}

func TestCoversAsksNothingWithoutAppArmor(t *testing.T) {
	for name, without := range map[string]func(h *fakeHost){
		"AppArmor disabled": func(h *fakeHost) { h.write(enabledPath, "N\n") },
		// A node in a container without securityfs, where containerd applies
		// no profile at all.
		"no securityfs": func(h *fakeHost) {
			if err := os.Remove(securityfsPath); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			h.restricted()
			without(h)
			h.labelErr = errors.New("probed")
			if got, err := Covers(); err != nil || got {
				t.Errorf("Covers() = %v, %v, want false, nil and no probe of runc", got, err)
			}
			if on, err := Restricted(); err != nil || on {
				t.Errorf("Restricted() = %v, %v, want false, nil: no profile is stacked where none is applied", on, err)
			}
		})
	}
}

func TestProfileWidensTheHostsProfile(t *testing.T) {
	for name, tc := range map[string]struct{ src, attach string }{
		"Ubuntu 26.04": {ubuntuRunc, "/usr/{bin,sbin}/runc"},
		"Ubuntu 24.04": {ubuntu2404Runc, "/usr/sbin/runc"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			h.write(hostProfile, tc.src)
			got, notWidened, err := Profile()
			if err != nil || notWidened != nil {
				t.Fatal(err, notWidened)
			}
			want := strings.Replace(tc.src, "profile runc "+tc.attach+" flags",
				"@{stratum_runc_attach} = "+tc.attach+" /var/lib/stratum/bin/runc\nprofile runc @{stratum_runc_attach} flags", 1)
			if string(got) != want {
				t.Errorf("Profile() =\n%s\nwant the host's file with only its attachment widened:\n%s", got, want)
			}
		})
	}
}

func TestProfileAttachesStratumsRuncAloneWhenTheHostsIsDisabled(t *testing.T) {
	h := newFakeHost(t)
	// A host file Stratum could not widen: disabling it is the way past it.
	h.write(hostProfile, "profile runc /usr/bin/runc {\n}\nprofile runc /usr/sbin/runc {\n}\n")
	h.write(hostDisabled, "")
	got, notWidened, err := Profile()
	if err != nil || notWidened != nil {
		t.Fatal(err, notWidened)
	}
	if !bytes.Equal(got, builtinProfile()) {
		t.Errorf("Profile() with the host's runc profile disabled =\n%s\nwant the built-in profile for Stratum's runc alone, leaving the host's runc as the operator set it", got)
	}
}

func TestProfileWithoutAHostProfileCoversStratumsRuncAlone(t *testing.T) {
	newFakeHost(t)
	got, notWidened, err := Profile()
	if err != nil || notWidened != nil {
		t.Fatal(err, notWidened)
	}
	if !bytes.Contains(got, []byte("profile runc /var/lib/stratum/bin/runc flags=(unconfined) {")) {
		t.Errorf("Profile() on a host without a runc profile =\n%s\nwant a runc profile in unconfined mode for Stratum's runc alone", got)
	}
}

// A host profile Stratum cannot widen gets Stratum's own profile, and a reason
// to warn with, rather than a node that does not start.
func TestProfileFallsBackWhenItCannotWiden(t *testing.T) {
	for name, src := range map[string]string{
		"no attachment":  "profile runc flags=(unconfined) {\n}\n",
		"two profiles":   "profile runc /usr/bin/runc {\n}\nprofile runc /usr/sbin/runc {\n}\n",
		"only a comment": "# profile runc /usr/bin/runc\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			h.write(hostProfile, src)
			got, notWidened, err := Profile()
			if err != nil || !errors.Is(notWidened, ErrHostProfileNotWidened) || !bytes.Equal(got, builtinProfile()) {
				t.Errorf("Profile() on %q = %q, %v, %v; want the built-in profile, ErrHostProfileNotWidened and no error", src, got, notWidened, err)
			}
		})
	}
}

func TestEnsureFallsBackAndWarnsOnAHostProfileItCannotWiden(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.write(hostProfile, "profile runc flags=(unconfined) {\n}\n")
	var out bytes.Buffer
	if err := Ensure(&out); err != nil {
		t.Fatalf("Ensure() with a host profile it cannot widen = %v, want the node to start", err)
	}
	if len(h.calls) != 1 || h.calls[0].profile != string(builtinProfile()) || h.label != coveredLabel {
		t.Errorf("Ensure() loaded %v, leaving runc %q; want the built-in profile, covering runc", h.calls, h.label)
	}
	if !strings.Contains(out.String(), "WARN") || !strings.Contains(out.String(), "/etc/apparmor.d/disable/") {
		t.Errorf("Ensure() printed %q, want a warning saying how to fix the host profile", out.String())
	}
	if err := Check(); !errors.Is(err, ErrHostProfileNotWidened) {
		t.Errorf("Check() after the fallback = %v, want ErrHostProfileNotWidened, so runtime check says so", err)
	}
}

func TestEnsureLoadsNothingWithoutTheRestriction(t *testing.T) {
	h := newFakeHost(t)
	h.write(restrictPath, "0\n")
	if err := Ensure(&bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Errorf("Ensure() with the restriction off ran apparmor_parser %v; loading would only relabel unconfined pods", h.calls)
	}
}

func TestEnsureLoadsTheWidenedProfileAndChecksRuncTookIt(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.write(hostProfile, ubuntuRunc)
	if err := Ensure(&bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 || !slices.Equal(h.calls[0].args, []string{"--replace", "--skip-cache"}) {
		t.Fatalf("Ensure() ran apparmor_parser %v, want one --replace --skip-cache", h.calls)
	}
	if !strings.Contains(h.calls[0].profile, "profile runc @{stratum_runc_attach}") {
		t.Errorf("Ensure() loaded\n%s\nwant the widened host profile", h.calls[0].profile)
	}
	if h.label != coveredLabel {
		t.Errorf("after Ensure(), runc starts as %q, want %q", h.label, coveredLabel)
	}
}

func TestEnsureFailsWhenRuncDoesNotTakeTheProfile(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.write(hostProfile, ubuntuRunc)
	h.ignoreLoad = true
	err := Ensure(&bytes.Buffer{})
	if !errors.Is(err, ErrNotCovered) || !strings.Contains(err.Error(), "/var/lib/stratum/bin/runc") {
		t.Errorf("Ensure() with a load the kernel ignored = %v, want ErrNotCovered naming Stratum's runc: the parser's exit status alone does not prove runc runs under the profile", err)
	}
}

func TestEnsureWarnsWhenItCannotCheck(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.labelErr = &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: syscall.EPERM}
	var out bytes.Buffer
	if err := Ensure(&out); err != nil {
		t.Fatalf("Ensure() where ptrace is forbidden = %v, want the load to stand: the node must still start", err)
	}
	if len(h.calls) != 1 || !strings.Contains(out.String(), "WARN") {
		t.Errorf("Ensure() where ptrace is forbidden ran apparmor_parser %v and printed %q, want one load and a warning", h.calls, out.String())
	}
	if !strings.Contains(out.String(), "could not check it") || strings.Contains(out.String(), "sysctl.d") {
		t.Errorf("Ensure() warned %q; the load worked, so the remedy for a failed one does not belong in it", out.String())
	}
}

// A permission error before anything is loaded, as when apparmor_parser cannot
// be run, fails the start: only reading runc's label after a load may be
// skipped with a warning.
func TestEnsureFailsOnAPermissionErrorBeforeTheLoad(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.parseErr = &fs.PathError{Op: "fork/exec", Path: "/usr/sbin/apparmor_parser", Err: syscall.EACCES}
	var out bytes.Buffer
	if err := Ensure(&out); err == nil || strings.Contains(out.String(), "WARN") {
		t.Errorf("Ensure() when apparmor_parser cannot run = %v, printed %q; want an error and no warning that it loaded", err, out.String())
	}
}

func TestEnsureReportsAParserFailure(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.parseErr = errors.New("apparmor_parser --replace --skip-cache: exit status 1: syntax error")
	if err := Ensure(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("Ensure() with a failing parser = %v, want the parser's error", err)
	}
}

func TestReconcileLoadsOnlyWhenRuncLostTheProfile(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.write(hostProfile, ubuntuRunc)
	h.label = coveredLabel
	if loaded, err := Reconcile(context.Background()); err != nil || loaded != NotLoaded || len(h.calls) != 0 {
		t.Fatalf("Reconcile() with the profile in place = %v, %v and %d parser runs, want NotLoaded, nil and none", loaded, err, len(h.calls))
	}
	// The host's AppArmor reloaded its own runc profile.
	h.label = uncoveredLabel
	if loaded, err := Reconcile(context.Background()); err != nil || loaded != Loaded || len(h.calls) != 1 {
		t.Fatalf("Reconcile() after the host reloaded its profile = %v, %v and %d parser runs, want Loaded, nil and one", loaded, err, len(h.calls))
	}
	if h.label != coveredLabel {
		t.Errorf("after Reconcile(), runc starts as %q, want %q", h.label, coveredLabel)
	}
}

func TestReconcileDoesNothingWithoutTheRestriction(t *testing.T) {
	h := newFakeHost(t)
	h.write(restrictPath, "0\n")
	if loaded, err := Reconcile(context.Background()); err != nil || loaded != NotLoaded || len(h.calls) != 0 {
		t.Errorf("Reconcile() with the restriction off = %v, %v and %d parser runs, want NotLoaded, nil and none", loaded, err, len(h.calls))
	}
}

func TestReconcileReportsALabelItCannotRead(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.labelErr = errors.New("input/output error")
	if loaded, err := Reconcile(context.Background()); err == nil || loaded != NotLoaded || len(h.calls) != 0 {
		t.Errorf("Reconcile() with an unreadable label = %v, %v and %d parser runs, want NotLoaded, the error and none", loaded, err, len(h.calls))
	}
}

// Where the kernel forbids tracing runc, the kernel's hash of its runc profile
// tells whether the host replaced the one Stratum loaded: a look loads only
// then, so its own load does not make the next look load again.
func TestReconcileWhereRuncCannotBeTracedLoadsWhenTheHashChanges(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.write(hostProfile, ubuntuRunc)
	h.labelErr = &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: syscall.EPERM}
	look := func(want Reload, wantCalls int) {
		t.Helper()
		got, err := Reconcile(context.Background())
		if err != nil || got != want || len(h.calls) != wantCalls {
			t.Fatalf("Reconcile() = %v, %v after %d parser runs, want %v, nil after %d", got, err, len(h.calls), want, wantCalls)
		}
	}
	look(LoadedUnchecked, 1) // nothing loaded yet
	look(NotLoaded, 1)       // the kernel still has Stratum's
	// The host's AppArmor reloaded its own runc profile.
	h.write(filepath.Join(securityfsPath, "policy", "profiles", "runc.12", "raw_sha256"), "host\n")
	look(LoadedUnchecked, 2)
	look(NotLoaded, 2)
}

// With neither runc's label nor the kernel's hash to go by, a look loads at
// most once per blindInterval.
func TestReconcileWithNothingToCompareLoadsOncePerInterval(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.noHash = true
	h.labelErr = &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: syscall.EPERM}
	for i, want := range []Reload{LoadedUnchecked, NotLoaded, NotLoaded} {
		if got, err := Reconcile(context.Background()); err != nil || got != want {
			t.Fatalf("look %d: Reconcile() = %v, %v, want %v", i, got, err, want)
		}
	}
	if len(h.calls) != 1 {
		t.Errorf("three looks within blindInterval ran apparmor_parser %d times, want once", len(h.calls))
	}
}

func TestStackedListsProcessesWithAStackedLabel(t *testing.T) {
	h := newFakeHost(t)
	for pid, label := range map[string]string{
		"2700": "cri-containerd.apparmor.d//&unconfined (enforce)",
		"2701": "cri-containerd.apparmor.d (enforce)",
		"2702": "unconfined",
		"2703": "custom//&unconfined (enforce)",
		"self": "cri-containerd.apparmor.d//&unconfined (enforce)",
	} {
		h.write(filepath.Join(procDir, pid, "attr", "apparmor", "current"), label+"\n")
		h.write(filepath.Join(procDir, pid, "comm"), "proc"+pid+"\n")
		h.write(filepath.Join(procDir, pid, "cgroup"), "0::/kubepods.slice/kubepods-besteffort.slice/cri-containerd-"+pid+".scope\n")
	}
	// A kernel without the per-LSM attr directory.
	h.write(filepath.Join(procDir, "2704", "attr", "current"), "cri-containerd.apparmor.d//&unconfined (enforce)\n")
	h.write(filepath.Join(procDir, "2704", "cgroup"), "0::/kubepods/burstable/pod1/abc\n")
	// A stacked process outside any pod is not a container to recreate.
	h.write(filepath.Join(procDir, "2705", "attr", "apparmor", "current"), "custom//&unconfined (enforce)\n")
	h.write(filepath.Join(procDir, "2705", "cgroup"), "0::/user.slice/session-1.scope\n")
	got, err := Stacked()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	slices.Sort(pids)
	if !slices.Equal(pids, []int{2700, 2703, 2704}) {
		t.Errorf("Stacked() = %+v, want pids 2700, 2703 and 2704", got)
	}
	for _, p := range got {
		if p.PID == 2700 && (p.Comm != "proc2700" || p.Label != "cri-containerd.apparmor.d//&unconfined") {
			t.Errorf("Stacked() entry %+v, want its comm and its label without the mode", p)
		}
	}
	h.write(enabledPath, "N\n")
	if got, err := Stacked(); err != nil || len(got) != 0 {
		t.Errorf("Stacked() without AppArmor = %+v, %v, want none", got, err)
	}
}

func TestCheckNamesWhatContainersLose(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	err := Check()
	if !errors.Is(err, ErrNotCovered) {
		t.Fatalf("Check() = %v, want ErrNotCovered", err)
	}
	for _, want := range []string{"/var/lib/stratum/bin/runc", "apparmor_restrict_unprivileged_unconfined", "cannot signal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check() = %q, want it to say %q", err, want)
		}
	}
	h.label = coveredLabel
	if err := Check(); err != nil {
		t.Errorf("Check() with the profile in place = %v, want nil", err)
	}
}

func TestRestoreReloadsTheHostsOwnProfile(t *testing.T) {
	h := newFakeHost(t)
	h.write(hostProfile, ubuntuRunc)
	h.label = coveredLabel
	var out bytes.Buffer
	if err := RestoreWithWriter(&out); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 || h.calls[0].profile != ubuntuRunc || !slices.Contains(h.calls[0].args, "--replace") {
		t.Fatalf("RestoreWithWriter() ran apparmor_parser %v, want one --replace of the host's file as it ships", h.calls)
	}
	if h.label != uncoveredLabel {
		t.Errorf("after RestoreWithWriter(), Stratum's runc starts as %q, want %q", h.label, uncoveredLabel)
	}
	if !strings.Contains(out.String(), "restored the host's runc profile") {
		t.Errorf("RestoreWithWriter() printed %q, want it to say it restored the host's profile", out.String())
	}
}

func TestRestoreRemovesTheProfileTheHostDoesNotLoad(t *testing.T) {
	for name, disabled := range map[string]bool{"no host profile": false, "host profile disabled": true} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			if disabled {
				h.write(hostProfile, ubuntuRunc)
				h.write(hostDisabled, "")
			}
			h.label = coveredLabel
			if err := RestoreWithWriter(&bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if len(h.calls) != 1 || !slices.Contains(h.calls[0].args, "--remove") {
				t.Fatalf("RestoreWithWriter() ran apparmor_parser %v, want one --remove", h.calls)
			}
		})
	}
}

func TestRestoreLeavesTheHostsProfileAlone(t *testing.T) {
	h := newFakeHost(t)
	h.write(hostProfile, ubuntuRunc)
	if err := RestoreWithWriter(&bytes.Buffer{}); err != nil || len(h.calls) != 0 {
		t.Errorf("RestoreWithWriter() with runc unconfined = %v and parser runs %v, want nil and none", err, h.calls)
	}
}

func TestRestoreFailsWhenRuncKeepsTheProfile(t *testing.T) {
	h := newFakeHost(t)
	h.label = coveredLabel
	h.ignoreLoad = true
	if err := RestoreWithWriter(&bytes.Buffer{}); err == nil {
		t.Error("RestoreWithWriter() with a removal the kernel ignored = nil, want an error")
	}
}

// The host's own profile may cover Stratum's runc itself, as an operator's
// workaround for the stacked labels does; restoring it is then done, not a
// failure every uninstall repeats.
func TestRestoreAcceptsAHostProfileThatCoversStratumsRunc(t *testing.T) {
	h := newFakeHost(t)
	h.write(hostProfile, strings.Replace(ubuntuRunc, "/usr/{bin,sbin}/runc", "/var/lib/stratum/bin/runc", 1))
	h.label = coveredLabel
	if err := RestoreWithWriter(&bytes.Buffer{}); err != nil {
		t.Errorf("RestoreWithWriter() with a host profile that covers Stratum's runc = %v, want nil", err)
	}
}

func TestPrerequisiteCompilesWithoutLoading(t *testing.T) {
	h := newFakeHost(t)
	if err := Prerequisite(); err != nil || len(h.calls) != 0 {
		t.Fatalf("Prerequisite() without the restriction = %v and parser runs %v, want nil and none", err, h.calls)
	}
	h.restricted()
	h.write(hostProfile, ubuntuRunc)
	if err := Prerequisite(); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 || !slices.Contains(h.calls[0].args, "--skip-kernel-load") || h.label != uncoveredLabel {
		t.Errorf("Prerequisite() ran apparmor_parser %v and left runc labelled %q, want one --skip-kernel-load compile and nothing loaded", h.calls, h.label)
	}
	// A host profile it cannot widen is no refusal: the node starts with the
	// built-in profile.
	h.write(hostProfile, "profile runc flags=(unconfined) {\n}\n")
	if err := Prerequisite(); err != nil || h.calls[len(h.calls)-1].profile != string(builtinProfile()) {
		t.Errorf("Prerequisite() with a host profile it cannot widen = %v, compiling %q; want nil, compiling the built-in profile", err, h.calls[len(h.calls)-1].profile)
	}
	h.parseErr = errors.New("apparmor_parser is not installed (it is in the apparmor package)")
	if err := Prerequisite(); err == nil || !strings.Contains(err.Error(), "/var/lib/stratum/bin/runc") {
		t.Errorf("Prerequisite() with no parser = %v, want an error naming Stratum's runc", err)
	}
}

func TestWidenedSaysYesWhenItCannotTell(t *testing.T) {
	h := newFakeHost(t)
	if Widened() {
		t.Error("Widened() with runc unconfined = true, want false")
	}
	h.label = coveredLabel
	if !Widened() {
		t.Error("Widened() with runc under the profile = false, want true")
	}
	h.labelErr = errors.New("operation not permitted")
	if !Widened() {
		t.Error("Widened() with an unreadable label = false, want true so the preview errs toward saying so")
	}
	h.labelErr = &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: fs.ErrNotExist}
	if Widened() {
		t.Error("Widened() without Stratum's runc = true, want false")
	}
}

// Where runc cannot be traced, uninstall goes by the restriction: without it
// Stratum loaded nothing, so there is nothing to restore and nothing to fail
// on; with it, the host's own profile is reloaded, unchecked.
func TestRestoreWhereRuncCannotBeTraced(t *testing.T) {
	cannot := &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: syscall.EPERM}

	h := newFakeHost(t)
	h.write(hostProfile, ubuntuRunc)
	h.labelErr = cannot
	if err := RestoreWithWriter(&bytes.Buffer{}); err != nil || len(h.calls) != 0 {
		t.Errorf("RestoreWithWriter() with the restriction off = %v and parser runs %v, want nil and none", err, h.calls)
	}
	if Widened() {
		t.Error("Widened() with the restriction off = true, want false")
	}

	h = newFakeHost(t)
	h.restricted()
	h.write(hostProfile, ubuntuRunc)
	h.write(filepath.Join(securityfsPath, "policy", "profiles", "runc.12", "name"), "runc\n")
	h.labelErr = cannot
	if !Widened() {
		t.Error("Widened() with the restriction on = false, want true")
	}
	if err := RestoreWithWriter(&bytes.Buffer{}); err != nil || len(h.calls) != 1 || h.calls[0].profile != ubuntuRunc {
		t.Errorf("RestoreWithWriter() with the restriction on = %v and parser runs %v, want nil and one reload of the host's profile", err, h.calls)
	}

	// No host profile and none loaded: nothing to remove.
	h = newFakeHost(t)
	h.restricted()
	if err := os.MkdirAll(filepath.Join(securityfsPath, "policy", "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.labelErr = cannot
	if err := RestoreWithWriter(&bytes.Buffer{}); err != nil || len(h.calls) != 0 {
		t.Errorf("RestoreWithWriter() with no runc profile loaded = %v and parser runs %v, want nil and none", err, h.calls)
	}
}
