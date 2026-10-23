package selinux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ctrliq/stratum/internal/version"
)

// Registered reports whether RemoveContexts would delete anything on this
// node, so uninstall can say it will: a context or equivalence Stratum
// registered, by its record or, without one, by what it would register. A
// policy it cannot list counts as yes, so the preview errs toward saying so.
func Registered() bool {
	rs, es, err := removable()
	if errors.Is(err, errNotInstalled) {
		return false
	}
	return err != nil || len(rs)+len(es) > 0
}

// RemoveContexts deletes the file contexts and equivalences Stratum registered,
// as recorded in managedPath, or, without a record, the set it would register
// on this policy. An entry is deleted only while the policy still holds it
// exactly as Stratum registered it, so a context the operator set for the same
// path is left alone, and one already gone is skipped. Rules go first: semanage
// refuses to delete an equivalence with rules beneath its target. The stratum
// binary, which uninstall keeps, is then relabelled to the policy's default.
//
// A host without semanage, as one without SELinux, has nothing to remove.
func RemoveContexts() error { return RemoveContextsWithWriter(os.Stdout) }

// RemoveContextsWithWriter removes owned contexts and reports progress to w.
func RemoveContextsWithWriter(w io.Writer) error {
	rs, es, err := removable()
	if errors.Is(err, errNotInstalled) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-d", "-f", fileTypes[r.ftype], r.spec); err != nil {
			errs = append(errs, fmt.Errorf("remove the SELinux file context %s %s: %w", r.spec, r.typ, err))
			continue
		}
		fmt.Fprintf(w, "  SELinux: removed the file context %s %s\n", r.spec, r.typ)
	}
	for _, e := range es {
		if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-d", "-e", e.source, e.target); err != nil {
			errs = append(errs, fmt.Errorf("remove the SELinux file context equivalence %s = %s: %w", e.target, e.source, err))
			continue
		}
		fmt.Fprintf(w, "  SELinux: removed the file context equivalence %s = %s\n", e.target, e.source)
	}
	if err := Restore(false, version.StratumInstallPath); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removable lists what RemoveContexts deletes: Stratum's contexts and
// equivalences that the local policy still holds exactly as Stratum
// registered them. A missing semanage matches errNotInstalled.
func removable() ([]rule, []equivalence, error) {
	all, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-l")
	if err != nil {
		return nil, nil, err
	}
	policy := parseListing(all)
	out, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-l", "-C")
	if err != nil {
		return nil, nil, err
	}
	local := parseListing(out)
	types := map[string]string{}
	for _, r := range local.rules {
		types[r.spec+"\x00"+r.ftype] = r.typ
	}
	wantRules, wantEquivs := managed(policy)
	var rs []rule
	for _, r := range wantRules {
		if types[r.spec+"\x00"+r.ftype] == r.typ {
			rs = append(rs, r)
		}
	}
	var es []equivalence
	for _, e := range wantEquivs {
		if local.equivs[e.target] == e.source {
			es = append(es, e)
		}
	}
	return rs, es, nil
}

// managed is what RemoveContexts removes: the recorded contexts, or, without a
// record, those Stratum would register on this policy.
func managed(policy listing) ([]rule, []equivalence) {
	b, err := os.ReadFile(managedPath())
	if err != nil {
		var rs []rule
		for _, r := range rules() {
			r.spec = canonical(r.spec, policy.distro)
			rs = append(rs, r)
		}
		var es []equivalence
		for _, e := range equivalences() {
			if copies := mirrored(e, policy); len(copies) > 0 {
				rs = append(rs, copies...)
				continue
			}
			es = append(es, equivalence{canonical(e.target, policy.distro), canonical(e.source, policy.distro)})
		}
		return rs, es
	}
	var es []equivalence
	for _, line := range strings.Split(string(b), "\n") {
		if target, source, ok := strings.Cut(line, " = "); ok {
			es = append(es, equivalence{strings.TrimSpace(target), strings.TrimSpace(source)})
		}
	}
	return managedRules(), es
}
