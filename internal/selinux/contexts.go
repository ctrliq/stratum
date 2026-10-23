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

// LocalPathDir is where the local-path provisioner keeps volumes. container-
// selinux labels it container_file_t, which a directory created before that
// rule, or by a process that does not get the label, does not have, and a
// container_t helper pod may then not create volumes in it. Volumes created in
// it inherit the label, so only the directory itself is relabelled. A var so a
// test can redirect it.
var LocalPathDir = "/opt/local-path-provisioner"

// rule is a file context: files of kind ftype (as semanage names them, "all
// files" for every kind) matching spec get type typ.
type rule struct{ spec, ftype, typ string }

// equivalence labels target and everything beneath it as source is labelled.
type equivalence struct{ target, source string }

// rules are the file contexts that give Stratum's paths container-selinux's
// types, written against the paths Stratum uses. ensureContexts registers
// each through canonical, because semanage refuses a rule beneath a
// distribution equivalence's target: Rocky 9 makes /run equivalent to
// /var/run, and every release makes /etc/systemd/system equivalent to
// /usr/lib/systemd/system.
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

// equivalences make the containerd and kubelet state under Stratum's paths
// labelled as container-selinux labels the standard ones, which give image
// snapshots and pod volumes the types containers may use.
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
	// RequiresSELinux is a node profile that requires SELinux (the CIS
	// profile does): a new node whose policy cannot confine containers is
	// refused, enforcing or permissive, with no opt-out.
	RequiresSELinux bool
	// Profile is the name of the node's profile, which the refusals name.
	Profile string
	// OptOut is spec.selinux: false. Stratum then registers no contexts,
	// relabels nothing and labels no file it writes. It does not remove
	// contexts an earlier start registered.
	OptOut bool
	// Existing is a node that was brought up before. Such a node was running
	// without the contexts, or with them, and keeps running: a missing policy,
	// or contexts that cannot be registered or files relabelled, is a warning
	// there, not a refusal that would keep it down.
	Existing bool
}

// ErrCannotTell is what Prerequisite returns when the host's SELinux tools
// failed in a way that says nothing about the packages: whether the policy
// confines containers could not be asked, so the caller cannot answer the
// question either way and warns instead of passing or refusing. The error it
// wraps names the failure.
var ErrCannotTell = errors.New("could not tell whether the SELinux policy confines containers")

// optOutRefusal is the refusal for a node that sets spec.selinux: false under
// a profile that requires SELinux, which has no opt-out.
func optOutRefusal(profile string) error {
	return fmt.Errorf("the %s profile cannot be combined with spec.selinux: false: on an SELinux host the profile requires the container runtime and its containers to be confined. Remove spec.selinux: false, or drop the profile", profile)
}

// missingRefusal is the refusal for a new node whose policy cannot confine
// containers, the one Ensure and Prerequisite share: under a profile that
// requires SELinux, with no opt-out, and on an enforcing host, where the
// remedy is installing container-selinux or opting out. A permissive node
// whose profile does not require SELinux is not refused.
func missingRefusal(mode Mode, o Options, err error) error {
	if o.RequiresSELinux {
		return fmt.Errorf("SELinux is %s and %w; the %s profile requires it, with no opt-out", mode, err, o.Profile)
	}
	if mode == Enforcing {
		return fmt.Errorf("SELinux is enforcing and %w. Without it Stratum's runtime and every container run unconfined. Install it, or set spec.selinux: false in the node's config file to run without SELinux labels", err)
	}
	return nil
}

// Prerequisite reports whether this node passes the check an upgrade makes
// before it touches the host, with the rules Ensure applies to a new node: an
// enforcing node, or one whose profile requires SELinux, without
// container-selinux is refused; an opted-out, disabled or other permissive
// node passes. Existing does not matter: the caller decides whether an
// already-running node may resume. It changes nothing, so a node that fails
// can still label itself on a later start. Besides the policy, it asks
// semanage, read-only, for the local file contexts, since Ensure registers
// Stratum's with it: a node without semanage would take the upgrade and stay
// unlabelled. A tool failure that is not a missing package is returned
// wrapped in ErrCannotTell: the question could not be asked, so the caller
// warns and lets the node through rather than answer either way.
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
		// The tools are there but failed, so a missing package can be neither
		// ruled in nor ruled out: the check does not answer.
		return fmt.Errorf("%w: %w", ErrCannotTell, err)
	}
	return missingRefusal(mode, o, err)
}

// Ensure registers Stratum's file contexts with the host policy and relabels
// Stratum's files to match. It needs container-selinux, which defines the types
// the contexts use, and semanage.
//
// Without them a new node is refused when SELinux is enforcing or the CIS
// profile is on, because it would run its runtime and every container
// unconfined; a non-CIS node may opt out with spec.selinux: false instead. A
// permissive non-CIS host, and a node that was already running, gets a
// warning, and the labels are applied on the first start after the packages
// are installed. The contexts are applied in permissive mode too, so switching
// to enforcing later needs no reinstall.
//
// The full relabel walks the data and run directories, so it runs only when a
// context was added or changed, or when the stamp of the last full relabel is
// missing: an interrupted or failed relabel, or a reinstall, runs it again.
// Every other start relabels the binaries, the unit and the local-path
// directory, which an upgrade or an operator may have replaced.
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

// label registers the contexts and relabels Stratum's files. CoreDNS's
// upstream directory is created here, before any relabel, so it carries its
// read-only type from the node's first start rather than the data
// directory's, which containers may not read. The binaries, the unit, that
// directory and the local-path directory are relabelled first, before anything that
// can fail, so a start always corrects what an upgrade wrote before the
// runtime execs it, whatever the policy's own state.
//
// The full relabel runs when a context changed, when the stamp is missing or
// records other contexts, or when a previous full relabel did not finish,
// which pendingPath records. A failure leaves an existing stamp in place: the
// node's files keep the labels its last full relabel gave them, so it stays
// labelled, and the next start runs the relabel again.
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

// removeStamp forgets that the node is labelled, so LabelLike stops, the
// runtime stops confining containers, and the next Ensure that labels
// relabels everything. It is for a node whose labels can no longer be relied
// on: one that opted out or lost the policy.
func removeStamp() error {
	if err := os.Remove(stampPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", stampPath(), err)
	}
	return nil
}

// requirePolicy reports whether the container-selinux policy module, which
// defines the types Stratum's contexts use, is loaded, and whether
// matchpathcon, which labels the files Stratum writes once the node is
// labelled, works. A missing module or tool matches errNotInstalled.
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

// hasModule reports whether semodule -l lists name. Older semodule prints a
// version after the name.
func hasModule(list, name string) bool {
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return true
		}
	}
	return false
}

// ensureContexts adds each of Stratum's file contexts the policy lacks and
// corrects any that differ, reporting the contexts it wants, one per line, and
// whether it changed anything. Equivalences come first: an ineffective one is
// removed before the rules that replace it are added. Contexts the operator
// added for other paths are left alone.
//
// libselinux matches local regular expressions newest first, not most specific
// first, and semanage lists them oldest first. So contexts are registered
// broadest first, and a narrower context listed before a broader one is
// registered again after it, or the broader one would shadow it: a data
// directory context re-added after the binaries' would label them
// container_var_lib_t. Reading the order from the store, rather than from what
// this start added, also repairs an update an earlier start left half done. A
// changed context keeps its place.
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

// managedPath records the file contexts Stratum registered last, one per line
// as ensureContexts reports them, so a context it no longer wants, such as a
// copy of a policy rule a container-selinux update removed, is deleted rather
// than left in force.
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

// mirrored returns, for an equivalence the distribution's policy makes
// ineffective, copies of the policy's own rules for its source registered
// under its target, which ensureContexts registers in place of the
// equivalence (semanage refuses a rule beneath a local equivalence's target).
// libselinux applies the local equivalences to a path before the
// distribution's, so on Rocky 9, which labels /run as /var/run, a real
// /run/stratum/containerd path never matches the canonical local equivalence
// /var/run/stratum/containerd, and without the copies a sandbox's shm would be
// labelled container_var_run_t instead of container_runtime_tmpfs_t.
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

// policyRulesUnder lists the policy's rules for source and beneath it, one
// "policy SPEC FTYPE TYPE" line each, sorted. They are part of what the stamp
// records, so a container-selinux update that changes how an equivalence's
// source is labelled relabels the tree labelled through it.
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

// shadowedBy reports whether r, a registered regular expression, comes before
// a broader one of rules in the local store, pos giving each registered
// context's place: libselinux then takes the broader one for paths under
// both. An exact path takes precedence over every regular expression,
// wherever it is.
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

// stem is the literal start of a file context's path expression, before its
// first regular-expression character. A context nested under another has a
// longer stem.
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

// canonical rewrites path through the distribution's equivalences, the
// longest matching target first, so a rule is registered on the path the
// policy labels it by.
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

// listing is a parsed `semanage fcontext -l` or `-l -C`: its file contexts,
// the distribution's equivalences and the local ones, each keyed by target
// with their source.
type listing struct {
	rules          []rule
	distro, equivs map[string]string
}

// parseListing reads `semanage fcontext -l` or `-l -C`:
//
//	SELinux fcontext                                   type               Context
//
//	/usr/local/bin/stratum                             all files          system_u:object_r:container_runtime_exec_t:s0
//
//	SELinux Distribution fcontext Equivalence
//
//	/run = /var/run
//
//	SELinux Local fcontext Equivalence
//
//	/var/lib/stratum/containerd = /var/lib/containerd
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
