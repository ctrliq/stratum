package apparmor

import (
	"bytes"
	"context"
	"errors"
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
	coveredLabel   = "runc"
	uncoveredLabel = "unconfined"
	// notWidenable is a host profile with no attachment to widen.
	notWidenable = "profile runc flags=(unconfined) {\n}\n"
)

// errNoPtrace is what starting runc traced fails with where the kernel
// forbids ptrace.
var errNoPtrace = &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: syscall.EPERM}

type parseCall struct {
	args    []string
	profile string
}

// fakeHost stands in for a host's AppArmor: its kernel settings, the host's
// profile files, the label Stratum's runc starts with, and an apparmor_parser
// whose loads and removals change that label the way the kernel would.
type fakeHost struct {
	t     *testing.T
	calls []parseCall
	label string
	// parseErr fails every parse, labelErr every label read; ignoreLoad
	// leaves the label as it was after a load.
	parseErr, labelErr error
	ignoreLoad         bool
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h, root := &fakeHost{t: t, label: uncoveredLabel}, t.TempDir()
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
	})
	enabledPath = filepath.Join(root, "enabled")
	securityfsPath = filepath.Join(root, "securityfs")
	procDir = filepath.Join(root, "proc")
	restrictPath = filepath.Join(root, "restrict")
	hostProfile = filepath.Join(root, "apparmor.d", "runc")
	hostDisabled = filepath.Join(root, "apparmor.d", "disable", "runc")
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
	switch {
	case h.parseErr != nil:
		return h.parseErr
	case h.ignoreLoad, slices.Contains(args, "--skip-kernel-load"):
	case slices.Contains(args, "--remove"):
		h.label = uncoveredLabel
	default:
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
		uncoveredLabel:                     false,
		"runc-stratum":                     false,
		"cri-containerd.apparmor.d//&runc": false,
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

// Without a host profile it can widen, the profile is Stratum's own, for its
// runc alone, which leaves the host's runc as the operator set it. Only a host
// profile it cannot widen is a reason to warn, rather than a node that does not
// start; disabling that file is the way past it.
func TestProfileIsForStratumsRuncAloneWithoutAHostProfileToWiden(t *testing.T) {
	const twoProfiles = "profile runc /usr/bin/runc {\n}\nprofile runc /usr/sbin/runc {\n}\n"
	for name, tc := range map[string]struct {
		src        string // "" leaves the host's file out
		disabled   bool
		notWidened bool
	}{
		"no host profile":       {},
		"host profile disabled": {src: twoProfiles, disabled: true},
		"no attachment":         {src: notWidenable, notWidened: true},
		"two profiles":          {src: twoProfiles, notWidened: true},
		"only a comment":        {src: "# profile runc /usr/bin/runc\n", notWidened: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			if tc.src != "" {
				h.write(hostProfile, tc.src)
			}
			if tc.disabled {
				h.write(hostDisabled, "")
			}
			got, notWidened, err := Profile()
			if err != nil || (notWidened != nil) != tc.notWidened || errors.Is(notWidened, ErrHostProfileNotWidened) != tc.notWidened {
				t.Errorf("Profile() = %v, %v; want ErrHostProfileNotWidened: %v, and no error", notWidened, err, tc.notWidened)
			}
			if !bytes.Contains(got, []byte("profile runc /var/lib/stratum/bin/runc flags=(unconfined) {")) {
				t.Errorf("Profile() =\n%s\nwant a runc profile in unconfined mode for Stratum's runc alone", got)
			}
		})
	}
}

func TestEnsureFallsBackAndWarnsOnAHostProfileItCannotWiden(t *testing.T) {
	h := newFakeHost(t)
	h.restricted()
	h.write(hostProfile, notWidenable)
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
	h.labelErr = errNoPtrace
	var out bytes.Buffer
	if err := Ensure(&out); err != nil {
		t.Fatalf("Ensure() where ptrace is forbidden = %v, want the load to stand: the node must still start", err)
	}
	if len(h.calls) != 1 || !strings.Contains(out.String(), "WARN") {
		t.Errorf("Ensure() where ptrace is forbidden ran apparmor_parser %v and printed %q, want one load and a warning", h.calls, out.String())
	}
	if !strings.Contains(out.String(), "could not check it") || !strings.Contains(out.String(), "restart stratum") || strings.Contains(out.String(), "sysctl.d") {
		t.Errorf("Ensure() warned %q; want it to say what to do when the host reloads its profiles, without the remedy for a failed load", out.String())
	}
}

// A failure before anything is loaded fails the start, a permission error
// included: only reading runc's label after a load may be skipped with a
// warning.
func TestEnsureFailsWhenTheParserDoes(t *testing.T) {
	for _, parseErr := range []error{
		errors.New("apparmor_parser --replace --skip-cache: exit status 1: syntax error"),
		&fs.PathError{Op: "fork/exec", Path: "/usr/sbin/apparmor_parser", Err: syscall.EACCES},
	} {
		h := newFakeHost(t)
		h.restricted()
		h.parseErr = parseErr
		var out bytes.Buffer
		err := Ensure(&out)
		if !errors.Is(err, parseErr) || !strings.Contains(err.Error(), "/etc/sysctl.d") || out.Len() != 0 {
			t.Errorf("Ensure() with a parser failing with %q = %v, printed %q; want the parser's error with the remedy, and no warning that it loaded", parseErr, err, out.String())
		}
	}
}

func TestReconcileLoadsOnlyWhenRuncLostTheProfile(t *testing.T) {
	h := newFakeHost(t)
	h.write(hostProfile, ubuntuRunc)
	look := func(when string, want bool, wantCalls int) {
		t.Helper()
		if loaded, err := Reconcile(context.Background()); err != nil || loaded != want || len(h.calls) != wantCalls {
			t.Fatalf("Reconcile() %s = %v, %v and %d parser runs, want %v, nil and %d", when, loaded, err, len(h.calls), want, wantCalls)
		}
	}
	h.write(restrictPath, "0\n")
	look("with the restriction off", false, 0)
	h.restricted()
	h.label = coveredLabel
	look("with the profile in place", false, 0)
	// The host's AppArmor reloaded its own runc profile.
	h.label = uncoveredLabel
	look("after the host reloaded its profile", true, 1)
	if h.label != coveredLabel {
		t.Errorf("after Reconcile(), runc starts as %q, want %q", h.label, coveredLabel)
	}
}

// A look that cannot read runc's label does not know the profile is gone, so
// it loads nothing. Where the kernel forbids tracing runc that is every look,
// and a load on each would change the policy and wake the next.
func TestReconcileLoadsNothingWhenItCannotReadRuncsLabel(t *testing.T) {
	for _, labelErr := range []error{errors.New("input/output error"), errNoPtrace} {
		h := newFakeHost(t)
		h.restricted()
		h.labelErr = labelErr
		if loaded, err := Reconcile(context.Background()); !errors.Is(err, labelErr) || loaded || len(h.calls) != 0 {
			t.Errorf("Reconcile() with a label read failing with %q = %v, %v and %d parser runs, want false, the error and none", labelErr, loaded, err, len(h.calls))
		}
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

// Uninstall puts the kernel's runc profile back as the host has it, where
// Stratum's runc runs under it. Where runc cannot be traced, it goes by the
// restriction and the kernel's loaded profiles instead: without the
// restriction Stratum loaded nothing, so there is nothing to restore and
// nothing to fail on.
func TestRestorePutsBackTheHostsProfile(t *testing.T) {
	for name, tc := range map[string]struct {
		label              string // "" when runc cannot be traced
		restricted, loaded bool
		hostFile           string // "" for none
		disabled           bool
		want               string // the parser's first argument; "" for no run
	}{
		"host profile": {label: coveredLabel, hostFile: ubuntuRunc, want: "--replace"},
		// An operator's workaround for the stacked labels: restoring it is
		// done, not a failure every uninstall repeats.
		"host profile covering Stratum's runc": {label: coveredLabel, hostFile: strings.Replace(ubuntuRunc, "/usr/{bin,sbin}/runc", "/var/lib/stratum/bin/runc", 1), want: "--replace"},
		"no host profile":                      {label: coveredLabel, want: "--remove"},
		"host profile disabled":                {label: coveredLabel, hostFile: ubuntuRunc, disabled: true, want: "--remove"},
		"runc unconfined":                      {label: uncoveredLabel, hostFile: ubuntuRunc},
		"untraceable, restriction off":         {loaded: true, hostFile: ubuntuRunc},
		"untraceable, no runc profile loaded":  {restricted: true},
		"untraceable, host profile":            {restricted: true, loaded: true, hostFile: ubuntuRunc, want: "--replace"},
		"untraceable, no host profile":         {restricted: true, loaded: true, want: "--remove"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			if h.label = tc.label; tc.label == "" {
				h.labelErr = errNoPtrace
			}
			profiles := filepath.Join(securityfsPath, "policy", "profiles")
			// Another profile, whose directory also starts with runc.
			h.write(filepath.Join(profiles, "runc.stratum.7", "name"), "runc.stratum\n")
			if tc.restricted {
				h.restricted()
			}
			if tc.loaded {
				h.write(filepath.Join(profiles, "runc.12", "name"), "runc\n")
			}
			if tc.hostFile != "" {
				h.write(hostProfile, tc.hostFile)
			}
			if tc.disabled {
				h.write(hostDisabled, "")
			}
			if got := Widened(); got != (tc.want != "") {
				t.Errorf("Widened() = %v, want %v", got, tc.want != "")
			}
			var out bytes.Buffer
			if err := RestoreWithWriter(&out); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == "":
				if len(h.calls) != 0 || out.Len() != 0 {
					t.Errorf("RestoreWithWriter() ran apparmor_parser %v and printed %q, want neither", h.calls, out.String())
				}
			case len(h.calls) != 1 || h.calls[0].args[0] != tc.want || (tc.want == "--replace" && h.calls[0].profile != tc.hostFile):
				t.Errorf("RestoreWithWriter() ran apparmor_parser %v, want one %s, a --replace being of the host's file as it ships", h.calls, tc.want)
			case !strings.Contains(out.String(), map[string]string{"--replace": "restored the host's runc profile", "--remove": "removed the runc profile"}[tc.want]):
				t.Errorf("RestoreWithWriter() printed %q, want it to say what it did", out.String())
			}
		})
	}
}

// What uninstall cannot read is an error for the restore, and a yes for the
// preview, which errs toward saying so; a host without Stratum's runc has
// nothing to restore.
func TestRestoreWhenItCannotTell(t *testing.T) {
	h := newFakeHost(t)
	h.labelErr = errors.New("input/output error")
	if err := RestoreWithWriter(&bytes.Buffer{}); !errors.Is(err, h.labelErr) || !Widened() {
		t.Errorf("with an unreadable label, RestoreWithWriter() = %v and Widened() = %v, want the error and true", err, Widened())
	}
	h.labelErr = &fs.PathError{Op: "fork/exec", Path: "/var/lib/stratum/bin/runc", Err: fs.ErrNotExist}
	if err := RestoreWithWriter(&bytes.Buffer{}); err != nil || Widened() {
		t.Errorf("without Stratum's runc, RestoreWithWriter() = %v and Widened() = %v, want nil and false", err, Widened())
	}
	if len(h.calls) != 0 {
		t.Errorf("RestoreWithWriter() ran apparmor_parser %v, want no run", h.calls)
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
	h.write(hostProfile, notWidenable)
	if err := Prerequisite(); err != nil || h.calls[len(h.calls)-1].profile != string(builtinProfile()) {
		t.Errorf("Prerequisite() with a host profile it cannot widen = %v, compiling %q; want nil, compiling the built-in profile", err, h.calls[len(h.calls)-1].profile)
	}
	h.parseErr = errors.New("apparmor_parser is not installed (it is in the apparmor package)")
	if err := Prerequisite(); err == nil || !strings.Contains(err.Error(), "/var/lib/stratum/bin/runc") {
		t.Errorf("Prerequisite() with no parser = %v, want an error naming Stratum's runc", err)
	}
}
