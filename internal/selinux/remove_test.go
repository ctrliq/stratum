package selinux

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// Uninstall removes every context and equivalence Stratum registered, rules
// before equivalences, leaves an operator's context for the same path, and
// returns the stratum binary to its default label.
func TestRemoveContexts(t *testing.T) {
	for _, tc := range []struct {
		name string
		base policyBase
	}{{"rocky 9", el9}, {"rocky 10", el10}} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHost(t, Enforcing)
			realRunPaths(t)
			p := newFakePolicy(t, tc.base)
			if err := Ensure(Options{}); err != nil {
				t.Fatal(err)
			}
			operator := rule{"/srv/data(/.*)?", "all files", "container_file_t"}
			p.local = append(p.local, operator)
			p.reset()
			if err := RemoveContexts(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(p.local, []rule{operator}) || len(p.equivs) != 0 {
				t.Errorf("left %v and %v, want only the operator's context", p.local, p.equivs)
			}
			lastRule, firstEquiv := -1, len(p.calls)
			for i, c := range p.calls {
				if strings.HasPrefix(c, "semanage fcontext -d -f") {
					lastRule = i
				}
				if strings.HasPrefix(c, "semanage fcontext -d -e") && i < firstEquiv {
					firstEquiv = i
				}
			}
			if lastRule > firstEquiv {
				t.Errorf("removed an equivalence before the rules beneath it: %q", p.calls)
			}
			if p.calls[len(p.calls)-1] != "restorecon "+version.StratumInstallPath {
				t.Errorf("stratum binary not relabelled last: %q", p.calls)
			}
		})
	}
}

// A context already gone is skipped, and one the operator changed to another
// type is left alone.
func TestRemoveContextsSkipsWhatIsNotStratums(t *testing.T) {
	fakeHost(t, Enforcing)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	changed := p.local[0]
	p.local[0].typ = "bin_t"
	gone := p.local[len(p.local)-1]
	p.local = p.local[:len(p.local)-1]
	p.reset()
	if err := RemoveContexts(); err != nil {
		t.Fatal(err)
	}
	if p.has("semanage fcontext -d -f a " + gone.spec) {
		t.Errorf("deleted %s, which was already gone: %q", gone.spec, p.calls)
	}
	if p.count("semanage fcontext -d -f a "+changed.spec) != 0 || !slices.ContainsFunc(p.local, func(r rule) bool { return r.spec == changed.spec }) {
		t.Errorf("removed a context the operator changed: %q", p.calls)
	}
}

// Without the record, as on a node labelled by an older build, uninstall
// removes the set Stratum would register.
func TestRemoveContextsWithoutTheRecord(t *testing.T) {
	fakeHost(t, Enforcing)
	realRunPaths(t)
	p := newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(managedPath()); err != nil {
		t.Fatal(err)
	}
	if err := RemoveContexts(); err != nil {
		t.Fatal(err)
	}
	if len(p.local) != 0 || len(p.equivs) != 0 {
		t.Errorf("left %v and %v", p.local, p.equivs)
	}
}

// A host without semanage has nothing to remove; a failing removal is
// reported.
func TestRemoveContextsErrors(t *testing.T) {
	fakeHost(t, Disabled)
	p := newFakePolicy(t, el9)
	p.override = func(name string, _ []string) (string, error, bool) {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}, name == "semanage"
	}
	if err := RemoveContexts(); err != nil {
		t.Errorf("without semanage: RemoveContexts = %v, want nil", err)
	}

	fakeHost(t, Enforcing)
	p = newFakePolicy(t, el9)
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	p.override = func(name string, args []string) (string, error, bool) {
		return "", errors.New("exit status 1: policy store locked"), name == "semanage" && len(args) > 1 && args[1] == "-d"
	}
	if err := RemoveContexts(); err == nil || !strings.Contains(err.Error(), "policy store locked") {
		t.Errorf("RemoveContexts = %v, want the semanage error", err)
	}
}

// The uninstall preview says contexts will go exactly when RemoveContexts
// would delete some: with a record, and without one on a node labelled before
// the record existed, but not when nothing of Stratum's is registered.
func TestRegisteredMatchesWhatRemoveContextsDeletes(t *testing.T) {
	fakeHost(t, Enforcing)
	realRunPaths(t)
	p := newFakePolicy(t, el9)
	if Registered() {
		t.Error("nothing registered, but the preview would list SELinux contexts")
	}
	if err := Ensure(Options{}); err != nil {
		t.Fatal(err)
	}
	if !Registered() {
		t.Error("contexts registered with a record, but the preview would not list them")
	}
	if err := os.Remove(managedPath()); err != nil {
		t.Fatal(err)
	}
	if !Registered() {
		t.Error("contexts registered before the record existed, but the preview would not list them")
	}
	p.local, p.equivs = nil, map[string]string{}
	if Registered() {
		t.Error("contexts already removed, but the preview would list them")
	}
}
