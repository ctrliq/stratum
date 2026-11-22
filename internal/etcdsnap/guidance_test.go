package etcdsnap

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/rollout"
	"github.com/ctrliq/stratum/internal/staticpod"
)

// flat folds whitespace runs into one space, so assertions ignore rendering wraps.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// debracket strips the [k] that keeps pkill from matching itself; bracketed names still count.
func debracket(command string) string {
	return strings.ReplaceAll(strings.ReplaceAll(command, "[", ""), "]", "")
}

// hasPkillFor reports a sudo pkill naming name; each command is one line, name and pkill together.
func hasPkillFor(text, name string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "sudo pkill") && strings.Contains(debracket(line), name) {
			return true
		}
	}
	return false
}

// Entries the derivation tests append to the set.
var (
	syntheticHostWatcher = RestartEntry{
		Component: "synthetic-agent",
		How:       "supervisor",
		Command:   "sudo pkill -x synthetic-agent",
		Repeat:    true,
	}
	syntheticWorkload = RestartEntry{
		Component: "synthetic workload",
		How:       "rollout",
		Command:   "-n synthetic-ns rollout restart deploy/synthetic",
		Repeat:    false,
	}
)

func setWith(es ...RestartEntry) RestartSet { return append(RestoreRestartSet(), es...) }

func TestRestoreRestartSetIsTheRestoreWatchers(t *testing.T) {
	swap(t, &comps.Units, append(slices.Clone(comps.Units), "synthetic-agent"))
	commands := maps.Clone(supervisedWatcherCommands)
	commands["synthetic-agent"] = "sudo pkill -x synthetic-agent"
	swap(t, &supervisedWatcherCommands, commands)

	set := RestoreRestartSet()

	type watcher struct{ name, command string }
	var watchers []watcher
	for _, name := range comps.Units {
		if name == "containerd" {
			continue // containerd holds no long watch on the cluster
		}
		watchers = append(watchers, watcher{name, supervisedWatcherCommands[name]})
	}

	got := map[string]bool{}
	for _, e := range set {
		if got[e.Component] {
			t.Errorf("the set names %q twice", e.Component)
		}
		got[e.Component] = true
		switch {
		case e.Component == "":
			t.Errorf("an entry names no component: %+v", e)
		case e.How == "":
			t.Errorf("%s: the entry does not say how the restore restarts it", e.Component)
		case e.Command == "":
			t.Errorf("%s: the entry carries no command to restart it by hand", e.Component)
		}
	}

	seen := map[string]bool{}
	supervised := 0
	for _, e := range set {
		if e.How != howSupervisor {
			continue
		}
		supervised++
		owner := ""
		for _, w := range watchers {
			if w.command == e.Command && !seen[w.name] {
				owner = w.name
				break
			}
		}
		if owner == "" {
			t.Errorf("the set has a supervised entry no comps.Units watcher owns: %+v", e)
			continue
		}
		seen[owner] = true
		if !e.Repeat {
			t.Errorf("%s: Repeat=false, but it is restarted on every control plane once each has rejoined", e.label())
		}
		if !strings.HasPrefix(e.Command, "sudo pkill") {
			t.Errorf("%s: command %q is not a pkill; the supervisor restarts it once the process is killed", e.label(), e.Command)
		}
	}
	if supervised != len(watchers) {
		t.Errorf("the set has %d supervised entries, want one per roster watcher (%d): %+v", supervised, len(watchers), set)
	}
	for _, w := range watchers {
		if !seen[w.name] {
			t.Errorf("the set lacks a supervised entry for comps.Units member %q", w.name)
		}
	}

	pods := restoredStaticPods()
	pi := 0
	wi := 0
	for _, e := range set {
		switch e.How {
		case howManifest:
			if pi >= len(pods) {
				t.Errorf("the set restarts a static pod the roster does not have: %+v", e)
				continue
			}
			if e.Component != pods[pi] {
				t.Errorf("the set's static pod %d is %q, want the roster's %q", pi, e.Component, pods[pi])
			}
			if !e.Repeat {
				t.Errorf("%s: Repeat=false, but it is a control-plane static pod, restarted on every one once they have rejoined", e.Component)
			}
			if !strings.HasPrefix(e.Command, "sudo pkill") || !strings.Contains(debracket(e.Command), pods[pi]) {
				t.Errorf("%s: command %q is not a pkill that names it; kubelet recreates the container once the process is killed", e.Component, e.Command)
			}
			pi++
		case howRollout:
			if wi >= len(rollout.Workloads) {
				t.Errorf("the set rolls a workload the roster does not have: %+v", e)
				continue
			}
			w := rollout.Workloads[wi]
			if e.Component != w.Name {
				t.Errorf("the set's workload %d is %q, want the roster's %q", wi, e.Component, w.Name)
			}
			if e.Repeat {
				t.Errorf("%s: Repeat=true, but the in-cluster workloads are rolled once for the cluster, not on every control plane", e.Component)
			}
			if !strings.Contains(e.Command, "rollout restart") || strings.HasPrefix(e.Command, "sudo pkill") {
				t.Errorf("%s: command %q is not a rollout restart", e.Component, e.Command)
			}
			wi++
		}
	}
	if pi != len(pods) {
		t.Errorf("the set restarts %d static pods, want the roster's %d", pi, len(pods))
	}
	if wi != len(rollout.Workloads) {
		t.Errorf("the set rolls %d workloads, want the roster's %d", wi, len(rollout.Workloads))
	}

	for _, absent := range []string{"etcd", "kube-apiserver", "containerd"} {
		if got[absent] {
			t.Errorf("the set has %q: etcd and the apiserver are restarted and health-gated by the restore itself, and containerd holds no long watch", absent)
		}
	}
}

func TestRestoreRestartSetIsBuiltFromTheRosters(t *testing.T) {
	oldNames := staticpod.Names
	t.Cleanup(func() { staticpod.Names = oldNames })
	staticpod.Names = append(slices.Clone(oldNames), "synthetic-manager")
	oldWorkloads := rollout.Workloads
	t.Cleanup(func() { rollout.Workloads = oldWorkloads })
	rollout.Workloads = append(slices.Clone(oldWorkloads),
		rollout.Workload{Name: "synthetic workload", Namespace: "synthetic-ns", Probe: "deploy/synthetic", Targets: []string{"deploy/synthetic"}})

	set := RestoreRestartSet()
	idx := slices.IndexFunc(set, func(e RestartEntry) bool { return e.Component == "synthetic-manager" })
	if idx < 0 {
		t.Fatalf("a static pod added to staticpod.Names did not join the set: %+v", set)
	}
	if e := set[idx]; !e.Repeat || !strings.HasPrefix(e.Command, "sudo pkill") ||
		!strings.Contains(debracket(e.Command), "synthetic-manager") {
		t.Errorf("synthetic-manager: %+v, want a per-control-plane entry whose pkill names it", e)
	}
	idx = slices.IndexFunc(set, func(e RestartEntry) bool { return e.Component == "synthetic workload" })
	if idx < 0 {
		t.Fatalf("a workload added to rollout.Workloads did not join the set: %+v", set)
	}
	if e := set[idx]; e.Repeat || !strings.Contains(e.Command, "-n synthetic-ns") ||
		!strings.Contains(e.Command, "rollout restart deploy/synthetic") {
		t.Errorf("synthetic workload: %+v, want a once-for-the-cluster entry with its rollout restart", e)
	}
}

func TestASyntheticWatcherRendersThroughEverySurface(t *testing.T) {
	swap(t, &comps.Units, append(slices.Clone(comps.Units), "synthetic-agent"))
	commands := maps.Clone(supervisedWatcherCommands)
	commands["synthetic-agent"] = "sudo pkill -x synthetic-agent"
	swap(t, &supervisedWatcherCommands, commands)

	if !slices.Contains(RepeatRestartCommands(), "sudo pkill -x synthetic-agent") {
		t.Errorf("the per-control-plane block lacks the synthetic watcher's command: %q", RepeatRestartCommands())
	}
	guidance := StillToDo(true)
	if !strings.Contains(guidance, "synthetic-agent") {
		t.Errorf("the guidance's still-to-do list does not name the synthetic watcher:\n%s", guidance)
	}
	if !strings.Contains(guidance, "sudo pkill -x synthetic-agent") {
		t.Errorf("the guidance's still-to-do list does not carry the synthetic watcher's command:\n%s", guidance)
	}
	prose := flat(RestoreRestartSet().RestartedProse())
	if !strings.Contains(prose, "synthetic-agent") {
		t.Errorf("the restarted sentence, which the CLI success text and the MCP next-step text are built from, does not name the synthetic watcher: %s", prose)
	}
}

func TestRestoreRestartSetRefusesAWatcherWithoutACommand(t *testing.T) {
	swap(t, &comps.Units, append(slices.Clone(comps.Units), "synthetic-agent"))
	defer func() {
		if recover() == nil {
			t.Error("RestoreRestartSet accepted a supervised watcher with no command to restart it by hand")
		}
	}()
	RestoreRestartSet()
}

func TestRepeatRestartCommandsComeFromTheSet(t *testing.T) {
	want := RestoreRestartSet().RepeatCommands()
	if got := RepeatRestartCommands(); !slices.Equal(got, want) {
		t.Errorf("RepeatRestartCommands() = %q, want the set's per-control-plane block %q", got, want)
	}
	for _, c := range want {
		if !strings.HasPrefix(c, "sudo pkill") {
			t.Errorf("%q is not a pkill; the repeat block runs on each control plane by hand", c)
		}
	}
	for _, c := range []string{
		`sudo pkill -x kubelet`,
		`sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-controller-manager([[:space:]]|$)'`,
		`sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-scheduler([[:space:]]|$)'`,
		// the operator's pattern names its subcommand and flags, not its executable
		"sudo pkill -TERM -f '" + comps.OperatorCommandLinePattern() + "'",
	} {
		if !slices.Contains(want, c) {
			t.Errorf("the per-control-plane block lacks %q", c)
		}
	}
	// Dependency order: kubelet first (it recreates the pods); the operator, a supervisor unit, last.
	last := -1
	for _, name := range []string{"kubelet", "kube-controller-manager", "kube-scheduler", "kube-vip", "operator"} {
		idx := slices.IndexFunc(want, func(c string) bool { return strings.Contains(debracket(c), name) })
		if idx < 0 {
			t.Errorf("the per-control-plane block has no command for %s: %q", name, want)
			continue
		}
		if idx <= last {
			t.Errorf("the command for %s is out of roster order: %q", name, want)
		}
		last = idx
	}
}

func TestKubeVipIsRestartedOnEveryControlPlane(t *testing.T) {
	set := RestoreRestartSet()
	idx := slices.IndexFunc(set, func(e RestartEntry) bool { return e.Component == "kube-vip" })
	if idx < 0 {
		t.Fatal("the set has no kube-vip entry")
	}
	if c := set[idx].Command; !strings.HasPrefix(c, "sudo pkill") || !strings.Contains(debracket(c), "kube-vip") {
		t.Errorf("kube-vip: command %q is not a pkill that names it", c)
	}
	if !slices.ContainsFunc(RepeatRestartCommands(), func(c string) bool {
		return strings.HasPrefix(c, "sudo pkill") && strings.Contains(debracket(c), "kube-vip")
	}) {
		t.Errorf("RepeatRestartCommands has no command for kube-vip: %q", RepeatRestartCommands())
	}
	if !hasPkillFor(StillToDo(true), "kube-vip") {
		t.Errorf("the guidance's repeat item has no pkill for kube-vip:\n%s", StillToDo(true))
	}
}

func TestAddingAComponentUpdatesEverySurface(t *testing.T) {
	host := setWith(syntheticHostWatcher)
	workload := setWith(syntheticWorkload)

	if !slices.Contains(host.RepeatCommands(), syntheticHostWatcher.Command) {
		t.Errorf("the per-control-plane block lacks the synthetic watcher's command: %q", host.RepeatCommands())
	}
	if !strings.Contains(host.RepeatItem(), syntheticHostWatcher.Command) {
		t.Errorf("the guidance's repeat item lacks the synthetic watcher's command:\n%s", host.RepeatItem())
	}
	if !strings.Contains(flat(host.RestartedProse()), "synthetic-agent") {
		t.Errorf("the restarted sentence does not name the synthetic watcher: %s", host.RestartedProse())
	}

	want := "stratum kubectl " + syntheticWorkload.Command
	if !slices.Contains(workload.RolloutRestartCommands("stratum kubectl"), want) {
		t.Errorf("the once-for-the-cluster block lacks %q: %q", want, workload.RolloutRestartCommands("stratum kubectl"))
	}
	if !strings.Contains(workload.RepeatItem(), want) {
		t.Errorf("the guidance's repeat item lacks %q:\n%s", want, workload.RepeatItem())
	}
	if !strings.Contains(flat(workload.RestartedProse()), "synthetic workload") {
		t.Errorf("the restarted sentence does not name the synthetic workload: %s", workload.RestartedProse())
	}

	if slices.Contains(workload.RepeatCommands(), syntheticWorkload.Command) {
		t.Errorf("the synthetic workload joined the per-control-plane block: %q", workload.RepeatCommands())
	}
	if slices.Contains(host.RolloutRestartCommands("stratum kubectl"), "sudo pkill -x synthetic-agent") {
		t.Errorf("the synthetic watcher joined the once-for-the-cluster block: %q", host.RolloutRestartCommands("stratum kubectl"))
	}
}

func TestRestartedProseNamesWhatTheRestoreRestarted(t *testing.T) {
	prose := flat(RestoreRestartSet().RestartedProse())
	for _, want := range []string{"This node's", "kubelet", "kube-controller-manager", "kube-scheduler",
		"kube-vip", "stratum operator", "the cluster's", "local-path provisioner", "were restarted"} {
		if !strings.Contains(prose, want) {
			t.Errorf("the restarted sentence lacks %q: %s", want, prose)
		}
	}
	lower := strings.ToLower(prose)
	for _, want := range []string{"coredns", "cilium"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the restarted sentence does not name %q: %s", want, prose)
		}
	}
	for _, bad := range []string{"kube-apiserver", "containerd"} {
		if strings.Contains(prose, bad) {
			t.Errorf("the restarted sentence names %q, which the restore restarts itself", bad)
		}
	}
}

func TestTheSetRendersTheWorkloadCommandsAsTheirOwnerDoes(t *testing.T) {
	set := RestoreRestartSet()
	for _, kubectl := range []string{"stratum kubectl", "stratum kubectl --server https://127.0.0.1:6443"} {
		if got, want := set.RolloutRestartCommands(kubectl), rollout.RestartCommands(kubectl); !slices.Equal(got, want) {
			t.Errorf("the set's once-for-the-cluster block with %q is %q, want the roster owner's %q", kubectl, got, want)
		}
	}
	if got, want := SeedRolloutRestartCommands(), set.RolloutRestartCommands("stratum kubectl --server https://127.0.0.1:6443"); !slices.Equal(got, want) {
		t.Errorf("SeedRolloutRestartCommands() = %q, want the set's seed-prefixed block %q", got, want)
	}
}

func TestStillToDoIsOneListForRestoreAndRollback(t *testing.T) {
	full, single := StillToDo(true), StillToDo(false)
	for _, c := range append(RepeatRestartCommands(), RolloutRestartCommands("stratum kubectl")...) {
		if !strings.Contains(full, c) {
			t.Errorf("the restore's list lacks %q", c)
		}
	}
	for _, want := range []string{"Rejoin every OTHER control plane", "once EVERY one has rejoined"} {
		if !strings.Contains(full, want) {
			t.Errorf("the restore's list lacks %q", want)
		}
		if strings.Contains(single, want) {
			t.Errorf("the rollback's list has %q, but a rollback has no control planes to rejoin", want)
		}
	}
	for _, want := range []string{"sudo pkill -x kubelet", "stratum kubectl get pods -A -o wide",
		"stratum kubectl -n <namespace> rollout restart deploy/<name>"} {
		if !strings.Contains(single, want) || !strings.Contains(full, want) {
			t.Errorf("a list lacks the item %q", want)
		}
	}
	for _, bad := range []string{"each control plane", "rejoined", "other control planes"} {
		if strings.Contains(single, bad) {
			t.Errorf("the rollback's list mentions %q:\n%s", bad, single)
		}
		if bad != "other control planes" && !strings.Contains(full, bad) {
			t.Errorf("the restore's list lost %q", bad)
		}
	}
	for name, list := range map[string]string{"restore": full, "rollback": single} {
		var got []string
		for _, m := range regexp.MustCompile(`(?m)^  (\d+)\. `).FindAllStringSubmatch(list, -1) {
			got = append(got, m[1])
		}
		want := "1 2 3 4 5"
		if name == "rollback" {
			want = "1 2 3"
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s: numbered %v, want %s", name, got, want)
		}
	}
}
