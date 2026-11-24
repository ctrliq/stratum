package selinux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ctrliq/stratum/internal/version"
)

// container_file_t from container-selinux; volumes inherit it, so only the directory is relabelled.
var LocalPathDir = "/opt/local-path-provisioner"

// files of kind ftype (as semanage names it) matching spec get type typ.
type rule struct{ spec, ftype, typ string }

// equivalence labels target and everything beneath it as source is labelled.
type equivalence struct{ target, source string }

// paths go through canonical: semanage refuses a rule under a distribution equivalence.
func rules() []rule {
	return []rule{
		{version.StratumInstallPath, "all files", "container_runtime_exec_t"},
		{version.DataDir + "(/.*)?", "all files", "container_var_lib_t"},
		{version.BinDir + "/.*", "all files", "container_runtime_exec_t"},
		{version.UpgradeStageDir + "/stratum[^/]*", "all files", "container_runtime_exec_t"},
		{version.CoreDNSUpstreamDir + "(/.*)?", "all files", "container_ro_file_t"},
		{version.RunDir + "(/.*)?", "all files", "container_var_run_t"},
		{filepath.Dir(version.SystemdUnitPath) + "/stratum.*", "all files", "container_unit_file_t"},
	}
}

// label Stratum's containerd and kubelet state like the standard paths.
func equivalences() []equivalence {
	return []equivalence{
		{version.ContainerdRootDir, "/var/lib/containerd"},
		{version.DataDir + "/kubelet", "/var/lib/kubelet"},
		{version.ContainerdStateDir, "/run/containerd"},
	}
}

// fileTypes maps semanage's file kinds to the -f letters it takes.
var fileTypes = map[string]string{
	"all files":        "a",
	"regular file":     "f",
	"directory":        "d",
	"character device": "c",
	"block device":     "b",
	"socket":           "s",
	"symbolic link":    "l",
	"named pipe":       "p",
}

// Options says how Ensure treats a host whose SELinux is enabled.
type Options struct {
	// RequiresSELinux is a profile that refuses a new node without a confining policy.
	RequiresSELinux bool
	Profile         string
	// OptOut is spec.selinux: false; contexts an earlier start registered stay.
	OptOut bool
	// Existing is a node already running: a labelling failure warns, not refuses.
	Existing bool
}

// returned by Prerequisite when the host's SELinux tools failed without an answer.
var ErrCannotTell = errors.New("could not tell whether the SELinux policy confines containers")

func optOutRefusal(profile string) error {
	return fmt.Errorf("the %s profile cannot be combined with spec.selinux: false: on an SELinux host the profile requires the container runtime and its containers to be confined. Remove spec.selinux: false, or drop the profile", profile)
}

func missingRefusal(mode Mode, o Options, err error) error {
	if o.RequiresSELinux {
		return fmt.Errorf("SELinux is %s and %w; the %s profile requires it, with no opt-out", mode, err, o.Profile)
	}
	if mode == Enforcing {
		return fmt.Errorf("SELinux is enforcing and %w. Without it Stratum's runtime and every container run unconfined. Install it, or set spec.selinux: false in the node's config file to run without SELinux labels", err)
	}
	return nil
}

// Prerequisite reports whether the node passes the upgrade's SELinux check.
func Prerequisite(o Options) error {
	if o.OptOut && o.RequiresSELinux {
		return optOutRefusal(o.Profile)
	}
	mode := CurrentMode()
	if mode == Disabled || o.OptOut {
		return nil
	}
	err := requirePolicy()
	if err == nil {
		_, err = tool("policycoreutils-python-utils", "semanage", "fcontext", "-l", "-C")
	}
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, errNotInstalled):
		// the tools are there but failed: a missing package can be neither ruled in nor out.
		return fmt.Errorf("%w: %w", ErrCannotTell, err)
	}
	return missingRefusal(mode, o, err)
}

// registers Stratum's file contexts and relabels its files; failures may warn rather than refuse.
func Ensure(o Options) error {
	if o.OptOut && o.RequiresSELinux {
		return optOutRefusal(o.Profile)
	}
	mode := CurrentMode()
	if mode == Disabled {
		return removeStamp()
	}
	if o.OptOut {
		if err := removeStamp(); err != nil {
			return err
		}
		fmt.Printf("  WARN: SELinux is %s and spec.selinux is false: Stratum does not label its files, so its runtime and containers are not confined\n", mode)
		return nil
	}
	err := requirePolicy()
	if err == nil {
		err = label(mode)
	}
	if err == nil {
		return nil
	}
	missing := errors.Is(err, errNotInstalled)
	if missing {
		if rmErr := removeStamp(); rmErr != nil {
			return errors.Join(err, rmErr)
		}
	}
	switch {
	case o.Existing && missing:
		fmt.Printf("  WARN: SELinux is %s and %v; Stratum's runtime and containers run unconfined until it is installed and Stratum restarted\n", mode, err)
		return nil
	case o.Existing:
		fmt.Printf("  WARN: SELinux is %s and Stratum's files could not be labelled: %v; its runtime and containers may run unconfined\n", mode, err)
		return nil
	case missing:
		if refusal := missingRefusal(mode, o, err); refusal != nil {
			return refusal
		}
		fmt.Printf("  WARN: SELinux is permissive and %v; Stratum's files are not labelled until it is installed\n", err)
		return nil
	case mode == Permissive && !o.RequiresSELinux:
		fmt.Printf("  WARN: SELinux is permissive and Stratum's files could not be labelled: %v\n", err)
		return nil
	}
	return fmt.Errorf("label Stratum's files for SELinux: %w", err)
}

// the binaries, unit and CoreDNS directory relabel first, before the full relabel that can fail.
func label(mode Mode) error {
	for _, d := range []string{LocalPathDir, version.CoreDNSUpstreamDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	want, changed, err := ensureContexts()
	if err != nil {
		if os.MkdirAll(version.DataDir, 0o755) == nil {
			_ = os.WriteFile(pendingPath(), nil, 0o644)
		}
		return err
	}
	if err := Restore(false, version.StratumInstallPath, version.SystemdUnitPath, LocalPathDir); err != nil {
		return err
	}
	if err := Restore(true, version.BinDir, version.CoreDNSUpstreamDir); err != nil {
		return err
	}
	_, pendingErr := os.Stat(pendingPath())
	if cur, err := os.ReadFile(stampPath()); !changed && err == nil && string(cur) == want && errors.Is(pendingErr, fs.ErrNotExist) {
		return nil
	}
	if err := os.WriteFile(pendingPath(), nil, 0o644); err != nil {
		return err
	}
	fmt.Printf("  SELinux (%s): relabelling %s, %s and %s\n", mode, version.DataDir, version.RunDir, LocalPathDir)
	if err := Restore(true, version.StratumInstallPath, version.SystemdUnitPath, version.DataDir, version.RunDir, LocalPathDir); err != nil {
		return err
	}
	if err := os.WriteFile(stampPath(), []byte(want), 0o644); err != nil {
		return err
	}
	if err := os.Remove(pendingPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// pendingPath marks a full relabel that has started and not finished.
func pendingPath() string { return filepath.Join(version.DataDir, "selinux-relabel-pending") }

// the node's labels can no longer be relied on, so the next labelling Ensure relabels all.
func removeStamp() error {
	if err := os.Remove(stampPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", stampPath(), err)
	}
	return nil
}

// a missing container module or matchpathcon tool matches errNotInstalled.
func requirePolicy() error {
	out, err := tool("policycoreutils", "semodule", "-l")
	if err != nil {
		return fmt.Errorf("list the loaded SELinux policy modules: %w", err)
	}
	if !hasModule(out, "container") {
		return fmt.Errorf("the container-selinux package is %w", errNotInstalled)
	}
	if _, err := tool("libselinux-utils", "matchpathcon", "-n", version.StratumInstallPath); err != nil {
		return err
	}
	return nil
}

// older semodule prints a version after the name, so only the first field counts.
func hasModule(list, name string) bool {
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return true
		}
	}
	return false
}

// libselinux matches local contexts newest first, so shadowed ones are registered again.
func ensureContexts() (string, bool, error) {
	all, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-l")
	if err != nil {
		return "", false, err
	}
	policy := parseListing(all)
	out, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-l", "-C")
	if err != nil {
		return "", false, err
	}
	local := parseListing(out)

	var want []string
	changed := false
	wantRules := rules()
	for i := range wantRules {
		wantRules[i].spec = canonical(wantRules[i].spec, policy.distro)
	}
	for _, e := range equivalences() {
		target, source := canonical(e.target, policy.distro), canonical(e.source, policy.distro)
		cur, ok := local.equivs[target]
		if copies := mirrored(e, policy); len(copies) > 0 {
			wantRules = append(wantRules, copies...)
			if !ok {
				continue
			}
			if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-d", "-e", cur, target); err != nil {
				return "", changed, fmt.Errorf("remove the SELinux file context equivalence %s = %s: %w", target, cur, err)
			}
			fmt.Printf("  SELinux: removed the file context equivalence %s = %s, which this policy does not apply\n", target, cur)
			changed = true
			continue
		}
		want = append(want, target+" = "+source)
		want = append(want, policyRulesUnder(source, policy)...)
		if ok && cur == source {
			continue
		}
		op := "-a"
		if ok {
			op = "-m"
		}
		if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", op, "-e", source, target); err != nil {
			return "", changed, fmt.Errorf("register the SELinux file context equivalence %s = %s: %w", target, source, err)
		}
		if ok {
			fmt.Printf("  SELinux: changed the file context equivalence %s from = %s to = %s\n", target, cur, source)
		} else {
			fmt.Printf("  SELinux: added the file context equivalence %s = %s\n", target, source)
		}
		changed = true
	}

	types := map[string]string{}
	for _, r := range local.rules {
		types[r.spec+"\x00"+r.ftype] = r.typ
	}
	wanted := map[string]bool{}
	for _, r := range wantRules {
		wanted[r.spec+"\x00"+r.ftype] = true
	}
	for _, r := range managedRules() {
		key := r.spec + "\x00" + r.ftype
		if wanted[key] || types[key] != r.typ {
			continue
		}
		if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-d", "-f", fileTypes[r.ftype], r.spec); err != nil {
			return "", changed, fmt.Errorf("remove the SELinux file context %s %s: %w", r.spec, r.typ, err)
		}
		fmt.Printf("  SELinux: removed the file context %s %s, which Stratum no longer registers\n", r.spec, r.typ)
		delete(types, key)
		changed = true
	}
	slices.SortStableFunc(wantRules, func(a, b rule) int { return len(stem(a.spec)) - len(stem(b.spec)) })
	pos := map[string]int{}
	for i, r := range local.rules {
		pos[r.spec+"\x00"+r.ftype] = i
	}
	next := len(local.rules)
	for _, r := range wantRules {
		want = append(want, r.spec+" "+r.ftype+" "+r.typ)
		key := r.spec + "\x00" + r.ftype
		cur, ok := types[key]
		shadowed := ok && shadowedBy(r, wantRules, pos)
		if ok && cur == r.typ && !shadowed {
			continue
		}
		op := "-a"
		switch {
		case shadowed:
			if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", "-d", "-f", fileTypes[r.ftype], r.spec); err != nil {
				return "", changed, fmt.Errorf("remove the SELinux file context %s %s to register it again: %w", r.spec, cur, err)
			}
		case ok:
			op = "-m"
		}
		if _, err := tool("policycoreutils-python-utils", "semanage", "fcontext", op, "-f", fileTypes[r.ftype], "-t", r.typ, r.spec); err != nil {
			return "", changed, fmt.Errorf("register the SELinux file context %s %s: %w", r.spec, r.typ, err)
		}
		if op == "-a" {
			pos[key] = next
			next++
		}
		switch {
		case shadowed:
			fmt.Printf("  SELinux: registered the file context %s %s again, after the broader context it was shadowed by\n", r.spec, r.typ)
		case ok:
			fmt.Printf("  SELinux: changed the file context %s from %s to %s\n", r.spec, cur, r.typ)
		default:
			fmt.Printf("  SELinux: added the file context %s %s\n", r.spec, r.typ)
		}
		changed = true
	}
	contexts := strings.Join(want, "\n") + "\n"
	if err := os.WriteFile(managedPath(), []byte(contexts), 0o644); err != nil {
		return "", changed, err
	}
	return contexts, changed, nil
}

// records the contexts Stratum registered, so stale ones get deleted.
func managedPath() string { return filepath.Join(version.DataDir, "selinux-contexts") }

// managedRules reads the rules managedPath records; equivalences are skipped.
func managedRules() []rule {
	b, err := os.ReadFile(managedPath())
	if err != nil {
		return nil
	}
	var out []rule
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.Contains(line, " = ") || fileTypes[strings.Join(f[1:len(f)-1], " ")] == "" {
			continue
		}
		out = append(out, rule{f[0], strings.Join(f[1:len(f)-1], " "), f[len(f)-1]})
	}
	return out
}

// the distro's rewrite makes the equivalence never match the path; the source rules are copied.
func mirrored(e equivalence, policy listing) []rule {
	target := canonical(e.target, policy.distro)
	if target == e.target {
		return nil
	}
	source := canonical(e.source, policy.distro)
	var out []rule
	for _, r := range policy.rules {
		if !under(r.spec, source) || fileTypes[r.ftype] == "" || r.typ == "" {
			continue
		}
		out = append(out, rule{target + strings.TrimPrefix(r.spec, source), r.ftype, r.typ})
	}
	slices.SortFunc(out, func(a, b rule) int { return strings.Compare(a.spec+a.ftype, b.spec+b.ftype) })
	return slices.CompactFunc(out, func(a, b rule) bool { return a == b })
}

// recorded in the stamp, so a policy update relabels trees labelled through it.
func policyRulesUnder(source string, policy listing) []string {
	var out []string
	for _, r := range policy.rules {
		if under(r.spec, source) {
			out = append(out, "policy "+r.spec+" "+r.ftype+" "+r.typ)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// a broader context registered after r wins: libselinux matches newest first.
func shadowedBy(r rule, rules []rule, pos map[string]int) bool {
	if stem(r.spec) == r.spec {
		return false
	}
	at, rs := pos[r.spec+"\x00"+r.ftype], stem(r.spec)
	for _, b := range rules {
		p, ok := pos[b.spec+"\x00"+b.ftype]
		bs := stem(b.spec)
		if !ok || p <= at || bs == b.spec || len(bs) >= len(rs) {
			continue
		}
		if under(strings.TrimSuffix(rs, "/"), strings.TrimSuffix(bs, "/")) {
			return true
		}
	}
	return false
}

// the literal start of a context's path expression, before its first regex character.
func stem(spec string) string {
	if i := strings.IndexAny(spec, `.*+?[(|^$\{`); i >= 0 {
		return spec[:i]
	}
	return spec
}

// under reports whether the file spec path is prefix or lies beneath it.
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/") || strings.HasPrefix(path, prefix+"(")
}

// rewritten through the distribution's equivalences, longest target first.
func canonical(path string, distro map[string]string) string {
	best := ""
	for target := range distro {
		if under(path, target) && len(target) > len(best) {
			best = target
		}
	}
	if best == "" {
		return path
	}
	return distro[best] + strings.TrimPrefix(path, best)
}

// a parsed semanage fcontext listing.
type listing struct {
	rules          []rule
	distro, equivs map[string]string
}

func parseListing(out string) listing {
	l := listing{distro: map[string]string{}, equivs: map[string]string{}}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "SELinux") {
			section = line
			continue
		}
		if target, source, ok := strings.Cut(line, " = "); ok {
			target, source = strings.TrimSpace(target), strings.TrimSpace(source)
			if strings.Contains(section, "Distribution") {
				l.distro[target] = source
			} else {
				l.equivs[target] = source
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		parts := strings.Split(f[len(f)-1], ":")
		if len(parts) < 3 {
			continue
		}
		l.rules = append(l.rules, rule{f[0], strings.Join(f[1:len(f)-1], " "), parts[2]})
	}
	return l
}
