// This file pins the repo's gate lists against each other.
//
// What runs on every pull request is decided by two lists: the steps of the
// CI lint job (.github/workflows/ci.yml) and the recipes of the Makefile's
// lint target. Nothing else compares them, so a gate dropped from one
// list, or registered in neither, passes every check. The rule these tests
// keep is that the lint job runs `make lint` as its single command step:
//
// The first pin freezes the gate set and requires each gate to keep running
// on pull requests: as a step of the job, or in the lint target's commands.
// The second pins the other direction: every tools/test-*.sh self-test must
// be named in the lint target, so a self-test cannot be added without a
// gate list running it. Both are pins; anything that reopens the split
// fails here first.
//
// Two more pins keep that unification in force. The CI lint job runs
// `make lint`, so every self-test and the skew pin gate reach pull requests
// through it, and the pre-push stage of .pre-commit-config.yaml hands its
// gates to a make target via its make-lint hook. One keeps the Makefile's
// whole list on PRs forever, and the other keeps a third gate list from
// re-forming — either the stage hands its gates to a make target or it
// holds only fast hooks with the config saying so.
//
// The same defect exists in the build's image stamps, and the image-stamp
// pins below hold the same rule there. build/images.txt is
// the pin surface for images, and every image stamp the linker flags carry —
// including the CoreDNS, local-path and cilium versions, tags stamped as bare
// versions — is built from a variable derived from it through
// tools/mirror-images.sh --list. A stamp spelled instead from a
// build/versions.mk copy is a second pin held to images.txt by nothing but a
// comment; deriving every stamp is what lets images.txt carry no must-equal
// comments and tools/lint-kube-images.sh no checks comparing copies. The
// pins keep it that way: any stamp reintroduced as a second copy fails here.
//
// The pin-policy guard is the one guard here that runs the gate it
// guards. The policy is branch-aware, and the branch is the one input the
// pin files cannot carry, so the tests below copy today's tree — the
// kubernetes_version binaries of build/versions.mk, the go.mod libraries,
// the build/images.txt tags, the mise.toml kubectl — into scratch git
// repositories on branches of their own, skew one input at a time, and run
// tools/lint-kube-images.sh itself. The policy is a deliberate human
// choice: release/* branches enforce exact library/binary equality; every
// other branch, main included, allows the libraries' prerelease drift ahead
// of the binaries but must report it; kubectl stays within
// upstream-supported skew of kubernetes_version. Two paperwork tests pin
// the policy's record: where it is written, and that the gate's comment
// states its rule and cites that place. tools/lint-kube-images.sh reads
// go.mod and the run's branch and applies the branch-aware rule itself, so
// every verdict below, release equality, prerelease drift WARNed off
// release, kubectl within skew, and the image-tag check the gate also runs,
// is a pinned behavior of the shipped gate.

package gatelist

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The downloaded validator needs no repository credentials. Run its actual
// recipe with a stub npx so the check proves credential isolation without
// downloading or executing a package.
func TestRenovateValidatorDoesNotInheritGitHubCredentials(t *testing.T) {
	// A caller's make context must not turn this execution into a dry run.
	t.Setenv("MAKEFLAGS", "n")
	t.Setenv("MFLAGS", "-n")
	t.Setenv("MAKELEVEL", "2")
	commands, err := makefileCommands("lint")
	if err != nil {
		t.Fatal(err)
	}
	var validator string
	for _, command := range commands {
		if strings.Contains(command, "renovate-config-validator --strict") {
			validator = command
		}
	}
	if validator == "" {
		t.Fatal("lint has no Renovate validation command")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "npx")
	const script = "#!/bin/sh\nif [ -n \"${GITHUB_TOKEN-}\" ] || [ -n \"${GH_TOKEN-}\" ]; then\n echo 'unexpected GitHub credentials' >&2\n exit 1\nfi\nprintf 'validator-stub-ran: %s\\n' \"$*\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	makefile := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefile, []byte("renovate_version := 0.0.0-test\ncheck:\n\t"+validator+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "-f", makefile, "check")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_TOKEN=fixture-only", "GH_TOKEN=fixture-only",
		"MAKEFLAGS=", "MFLAGS=", "MAKELEVEL=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("validator inherited credentials or failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "validator-stub-ran: --yes --package renovate@0.0.0-test renovate-config-validator --strict renovate.json5") {
		t.Fatalf("validator did not run with the pinned package: %s", out)
	}
}

// ciStep is one step of a GitHub Actions job: its display name and the shell
// text it runs. A step that only runs an action (`uses:`) has no Run.
type ciStep struct {
	Name string `json:"name"`
	Run  string `json:"run"`
}

// ciLintJobSteps returns the steps of the `lint` job in
// .github/workflows/ci.yml, in workflow order. The workflow is read
// relative to this package's directory — the directory `go test` runs in —
// so the reader needs no network and no build.
func ciLintJobSteps() ([]ciStep, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		return nil, err
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []ciStep `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		return nil, fmt.Errorf("parsing ci.yml: %w", err)
	}
	job, ok := workflow.Jobs["lint"]
	if !ok {
		return nil, fmt.Errorf("ci.yml declares no lint job")
	}
	return job.Steps, nil
}

// makeRule is one rule of the Makefile: its literal prerequisite names and
// the commands of its recipe.
type makeRule struct {
	prerequisites []string
	commands      []string
}

// makefileCommands returns every command `make <target>` runs in the repo
// Makefile: the recipes of the target's prerequisites first, then the
// target's own recipe, in the order make runs them. Recipe lines that are
// only shell comments are dropped (a comment runs no gate) and backslash
// continuations are joined into the single command the shell executes.
// Conditionals inside a recipe are read, not evaluated, so a rule returns
// every recipe line it spells.
//
// The Makefile is read as text, relative to this package's directory, so
// there is no network and no build. Only literal prerequisite names are
// resolved: a prerequisite that is a file, or spelled with a make variable
// (`build: $(NEEDS_DASH)`), names no rule and contributes no commands.
func makefileCommands(target string) ([]string, error) {
	rules, err := parseMakefile()
	if err != nil {
		return nil, err
	}
	if _, ok := rules[target]; !ok {
		return nil, fmt.Errorf("the Makefile has no rule for target %q", target)
	}

	var commands []string
	expanded := map[string]bool{}
	var expand func(name string)
	expand = func(name string) {
		if expanded[name] {
			return // already contributed: make runs a target once
		}
		expanded[name] = true
		rule, ok := rules[name]
		if !ok {
			return // a file or a variable prerequisite: no recipe to run
		}
		for _, prereq := range rule.prerequisites {
			expand(prereq)
		}
		commands = append(commands, rule.commands...)
	}
	expand(target)
	return commands, nil
}

// parseMakefile indexes the Makefile's rules by target name. Only rules a
// caller can name are indexed: special targets (.PHONY), pattern rules
// (component-%) and variable targets ($(IMAGETAR)) are skipped, as are
// assignments, which also carry a colon.
func parseMakefile() (map[string]makeRule, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		return nil, err
	}

	rules := make(map[string]makeRule)
	current := "" // the target whose recipe the following tab lines belong to

	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "\t"):
			if current == "" {
				// Tab-indented lines outside any recipe belong to
				// multi-line variable assignments, not to commands.
				continue
			}
			command := strings.TrimPrefix(line, "\t")
			if strings.HasPrefix(command, "#") {
				continue // a shell comment: make passes it, nothing runs
			}
			command = strings.TrimRight(command, " \t")
			if command == "" {
				continue
			}
			rule := rules[current]
			rule.commands = append(rule.commands, command)
			rules[current] = rule
		case isConditionalDirective(line):
			// ifeq/else/endif can sit inside a recipe without ending it;
			// outside one they are no rule either.
		default:
			current = ""
			target, prereqs, commands, ok := parseRuleLine(line)
			if !ok {
				continue
			}
			rules[target] = makeRule{prerequisites: prereqs, commands: commands}
			if len(commands) == 0 {
				current = target // a multi-line recipe follows
			}
		}
	}

	for target, rule := range rules {
		rule.commands = joinContinuations(rule.commands)
		rules[target] = rule
	}
	return rules, nil
}

// isConditionalDirective reports whether the line is one of make's
// conditionals.
func isConditionalDirective(line string) bool {
	for _, directive := range []string{"ifeq", "ifneq", "ifdef", "ifndef", "else", "endif"} {
		if line == directive || strings.HasPrefix(line, directive+" ") {
			return true
		}
	}
	return false
}

// parseRuleLine recognizes `target: prerequisites` and the one-line form
// `target: ; command`, and reports whether the line opens a rule a caller
// can ask for by name.
func parseRuleLine(line string) (target string, prereqs, commands []string, ok bool) {
	if line == "" || line[0] == '#' || line[0] == ' ' || line[0] == '\t' {
		return "", nil, nil, false
	}
	colon := strings.IndexByte(line, ':')
	if colon < 0 || strings.HasPrefix(line[colon+1:], "=") {
		return "", nil, nil, false // no rule, or an := assignment
	}
	target = line[:colon]
	if strings.HasPrefix(target, ".") || strings.ContainsAny(target, "$% \t") {
		return "", nil, nil, false // special, pattern, variable or multi-target
	}
	rest := line[colon+1:]
	if inline := strings.IndexByte(rest, ';'); inline >= 0 {
		if command := strings.TrimSpace(rest[inline+1:]); command != "" {
			commands = append(commands, command)
		}
		rest = rest[:inline]
	}
	return target, strings.Fields(rest), commands, true
}

// joinContinuations folds recipe lines that end in a backslash into the one
// command the shell executes, so a gate spelled across a continuation — the
// lint target's renovate validator — is a single matchable string.
func joinContinuations(lines []string) []string {
	joined := make([]string, 0, len(lines))
	for _, line := range lines {
		if n := len(joined); n > 0 && strings.HasSuffix(joined[n-1], "\\") {
			joined[n-1] = strings.TrimSuffix(joined[n-1], "\\") + " " + line
			continue
		}
		joined = append(joined, line)
	}
	return joined
}

// makefileVariables returns every variable the Makefile assigns, keyed by
// name, with an assignment's backslash continuations joined into the single
// value make expands. This is the recipe reader's other half: parseMakefile
// indexes rules and deliberately skips assignments, and the guards that ask
// what a variable is built from — not what a recipe runs — read them here.
// build/versions.mk, which the Makefile includes, is not read: the
// derivation under guard is defined in the Makefile itself, the way
// ETCD_IMAGE and SANDBOX_IMAGE are.
func makefileVariables() (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		return nil, err
	}

	vars := make(map[string]string)
	pending := "" // the variable whose assignment the previous line continued

	for _, line := range strings.Split(string(raw), "\n") {
		if pending == "" {
			name, value, ok := parseAssignment(line)
			if !ok {
				continue
			}
			if strings.HasSuffix(value, "\\") {
				value = strings.TrimSuffix(strings.TrimRight(value, " \t"), "\\")
				pending = name
			}
			vars[name] = value
			continue
		}

		// A continuation line: make folds the backslash, the newline and
		// the leading whitespace into one separator.
		name := pending
		text := strings.TrimSpace(line)
		if strings.HasSuffix(text, "\\") {
			text = strings.TrimSuffix(text, "\\")
		} else {
			pending = ""
		}
		if text = strings.TrimSpace(text); text != "" {
			vars[name] += " " + text
		}
	}
	return vars, nil
}

// assignmentRE matches `NAME := value` at the start of a line, for the
// assignment operators the Makefile uses. `+=` and `!=` appear nowhere in
// the file and stay unrecognized: a variable only appended to would resolve
// through its base assignment, never the appends.
var assignmentRE = regexp.MustCompile(`^([A-Za-z0-9_]+)[ \t]*(::=|:=|\?=|=)[ \t]*(.*)$`)

// parseAssignment recognizes the Makefile's variable assignments and returns
// the value as written, backslash included. Rules are not assignments: a
// rule's colon is never followed by one of the operators, so `lint:
// lint-shell` and the one-line `target: ; command` stay rules, and
// tab-indented recipe lines, .PHONY-style special targets and directives
// (ifeq, else, endif) name no variable either.
func parseAssignment(line string) (name, value string, ok bool) {
	match := assignmentRE.FindStringSubmatch(line)
	if match == nil {
		return "", "", false
	}
	return match[1], match[3], true
}

// makefileVariable returns the Makefile's definition of one variable, from
// the index makefileVariables built. A variable the Makefile no longer
// assigns is an error, so a guard reading a renamed variable fails on the
// rename, not on an empty answer.
func makefileVariable(vars map[string]string, name string) (string, error) {
	value, ok := vars[name]
	if !ok {
		return "", fmt.Errorf("the Makefile assigns no %s", name)
	}
	return value, nil
}

// ciLintGates is the frozen gate list: every gate the CI lint job runs today
// (.github/workflows/ci.yml), in job order — the eight lint and check
// scripts, the Cilium manifest render check, the Renovate config validator,
// the version-fences check, the nine self-tests the job runs, and
// print-version. A gate leaves this list only deliberately, in the change
// that retires it.
var ciLintGates = []string{
	"tools/lint-versions.sh",
	"tools/lint-kube-images.sh",
	"tools/gen-cilium-manifest.sh --check",
	"tools/lint-patches.sh",
	"tools/lint-docs-cli.sh",
	"tools/check-droppable-patches.sh",
	"renovate-config-validator --strict renovate.json5",
	"tools/gen-fences.sh --check",
	"tools/lint-shell.sh",
	"tools/test-lint-shell.sh",
	"tools/test-fences.sh",
	"tools/test-check-supervisor-restart.sh",
	"tools/test-host-arch.sh",
	"tools/test-verify-payload.sh",
	"tools/lint-revisions.sh",
	"tools/test-revisions.sh",
	"tools/lint-go-version.sh",
	"tools/test-go-version.sh",
	"tools/test-version.sh",
	"tools/test-build-id.sh",
	"print-version",
}

// makeLintStep matches a job step that invokes make's lint target — `make
// lint`, alone or among other words. The trailing boundary matters:
// `make lint-versions` must not count as running the lint target.
var makeLintStep = regexp.MustCompile(`(^|[\s;&|])make[ \t]+lint([\s;&|]|$)`)

// stepRunsGate reports whether one of the steps runs the gate's command.
func stepRunsGate(steps []ciStep, gate string) bool {
	for _, step := range steps {
		if strings.Contains(step.Run, gate) {
			return true
		}
	}
	return false
}

// commandRunsGate reports whether one of the commands runs the gate.
func commandRunsGate(commands []string, gate string) bool {
	for _, command := range commands {
		if strings.Contains(command, gate) {
			return true
		}
	}
	return false
}

// Every gate today's CI lint job runs still runs on pull requests. The
// job may keep running each gate as its own step, or collapse into
// `make lint` and hand the gate list to the Makefile — then the lint target
// must carry the gate. A gate that is neither has stopped running on pull
// requests, which nothing else notices while the two lists are only
// hand-maintained.
func TestEveryCIGateStillRunsOnPullRequests(t *testing.T) {
	steps, err := ciLintJobSteps()
	if err != nil {
		t.Fatalf("reading the lint job's steps: %v", err)
	}

	runsMakeLint := false
	for _, step := range steps {
		if makeLintStep.MatchString(step.Run) {
			runsMakeLint = true
		}
	}
	var lintCommands []string
	if runsMakeLint {
		lintCommands, err = makefileCommands("lint")
		if err != nil {
			t.Fatalf("reading the lint target's commands: %v", err)
		}
	}

	for _, gate := range ciLintGates {
		if stepRunsGate(steps, gate) {
			continue
		}
		if runsMakeLint && commandRunsGate(lintCommands, gate) {
			continue
		}
		if runsMakeLint {
			t.Errorf("the lint job runs `make lint`, but the lint target runs no %q — the gate no longer runs on pull requests", gate)
		} else {
			t.Errorf("the lint job runs no step for %q and does not run `make lint` — the gate no longer runs on pull requests", gate)
		}
	}
}

// Every self-test under tools/ is named in the lint target. A self-test
// is how a gate proves it rejects what it claims to; one that no gate list
// runs proves nothing. Today twenty scripts exist and the lint target names
// all twenty.
func TestEverySelfTestIsInTheLintTarget(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join("..", "test-*.sh"))
	if err != nil {
		t.Fatalf("listing tools/test-*.sh: %v", err)
	}
	if len(scripts) == 0 {
		t.Fatal("no tools/test-*.sh found relative to this package — the glob or the scripts have moved")
	}

	commands, err := makefileCommands("lint")
	if err != nil {
		t.Fatalf("reading the lint target's commands: %v", err)
	}

	for _, script := range scripts {
		name := "tools/" + filepath.Base(script)
		if !commandRunsGate(commands, name) {
			t.Errorf("the lint target names no %s — the self-test runs in no gate list", name)
		}
	}
}

// The N-3 kubelet pin gate is the one gate the lint target spells as
// neither a tools/test-*.sh self-test nor a tools/lint-*.sh script, so the
// pull-request list below names it separately. The flag is part of the
// name: run-skew.sh without it drives a live cluster (the weekly job),
// which is not a pull-request gate.
const skewPinGate = "tools/ostests/run-skew.sh --check-pins"

// Every self-test under tools/, and the pin gate, runs on every pull
// request — directly as a step of the CI lint job, or through the lint target
// because the job runs `make lint`. The self-test pin above covers the
// Makefile half of this (the lint target names every self-test); this one
// pins the workflow half, keeping the gap between the two lists — a gate
// registered only where no pull request would run it — from reopening as a
// hand-trimmed step list.
func TestEverySelfTestRunsOnPullRequests(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join("..", "test-*.sh"))
	if err != nil {
		t.Fatalf("listing tools/test-*.sh: %v", err)
	}
	if len(scripts) == 0 {
		t.Fatal("no tools/test-*.sh found relative to this package — the glob or the scripts have moved")
	}

	gates := make([]string, 0, len(scripts)+1)
	for _, script := range scripts {
		gates = append(gates, "tools/"+filepath.Base(script))
	}
	gates = append(gates, skewPinGate)

	steps, err := ciLintJobSteps()
	if err != nil {
		t.Fatalf("reading the lint job's steps: %v", err)
	}

	runsMakeLint := false
	for _, step := range steps {
		if makeLintStep.MatchString(step.Run) {
			runsMakeLint = true
		}
	}
	var lintCommands []string
	if runsMakeLint {
		lintCommands, err = makefileCommands("lint")
		if err != nil {
			t.Fatalf("reading the lint target's commands: %v", err)
		}
	}

	for _, gate := range gates {
		if stepRunsGate(steps, gate) {
			continue
		}
		if runsMakeLint && commandRunsGate(lintCommands, gate) {
			continue
		}
		if runsMakeLint {
			t.Errorf("the lint job runs `make lint`, but the lint target runs no %q — the gate runs on no pull request", gate)
		} else {
			t.Errorf("the lint job runs no step for %q and does not run `make lint` — the gate runs on no pull request", gate)
		}
	}
}

// preCommitHook is one hook of .pre-commit-config.yaml: its id and the
// command pre-commit runs for it. A hook with no entry runs its id
// (pre-commit's default), so the pre-push check below falls back to the id.
type preCommitHook struct {
	id    string
	entry string
}

// prePushHooks returns every hook registered at the pre-push stage of
// .pre-commit-config.yaml — the third gate list after the CI lint job and
// the Makefile's lint target. A hook with no stages key runs at every
// stage, pre-push included, so it counts too. The file is read relative to
// this package's directory, like the workflow and the Makefile.
func prePushHooks() ([]preCommitHook, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".pre-commit-config.yaml"))
	if err != nil {
		return nil, err
	}
	var config struct {
		Repos []struct {
			Hooks []struct {
				ID     string   `json:"id"`
				Entry  string   `json:"entry"`
				Stages []string `json:"stages"`
			} `json:"hooks"`
		} `json:"repos"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("parsing .pre-commit-config.yaml: %w", err)
	}

	var hooks []preCommitHook
	for _, repo := range config.Repos {
		for _, hook := range repo.Hooks {
			if len(hook.Stages) == 0 || slices.Contains(hook.Stages, "pre-push") {
				hooks = append(hooks, preCommitHook{id: hook.ID, entry: hook.Entry})
			}
		}
	}
	return hooks, nil
}

// makeTargetEntry matches a hook command that hands its gate list to the
// Makefile: `make` with a target, alone or inside the shell command the
// entry spells. The target word is required, so `command -v make` (asking
// whether the binary exists, not running it) and any "make" inside a
// longer word do not count.
var makeTargetEntry = regexp.MustCompile(`(?:^|\W)make[ \t]+\S+`)

// configDeclaresFastOnly reports whether .pre-commit-config.yaml says, in
// a comment, that the pre-push stage holds only fast hooks. The
// declaration must name the stage and put "fast" beside "only": today's
// header — anything that builds or reads the whole tree runs on
// `pre-push` — describes the opposite and must not count, and the section
// comment "pre-push only: these build, or read the whole tree" puts
// "only" beside "pre-push", not beside "fast".
func configDeclaresFastOnly() (bool, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".pre-commit-config.yaml"))
	if err != nil {
		return false, err
	}

	var blocks []string
	var comment []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			comment = append(comment, strings.TrimPrefix(trimmed, "#"))
			continue
		}
		if len(comment) > 0 {
			blocks = append(blocks, strings.Join(comment, " "))
			comment = nil
		}
	}
	if len(comment) > 0 {
		blocks = append(blocks, strings.Join(comment, " "))
	}

	for _, block := range blocks {
		text := strings.ToLower(block)
		if !strings.Contains(text, "pre-push") {
			continue
		}
		for _, phrase := range []string{"only fast", "fast only", "only-fast", "fast-only", "fast hooks only"} {
			if strings.Contains(text, phrase) {
				return true, nil
			}
		}
	}
	return false, nil
}

// The pre-push stage is not a third gate list. Either it hands its
// gates to the Makefile — some hook runs a make target, the one list the
// CI lint job also runs — or every hook in it is also run by the lint
// target and the config declares the stage fast-only, which is what makes
// a push-time-only subset deliberate rather than drift. Anything else is a
// gate a developer passes locally that no pull request runs. The stage hands
// to `make lint` today through its make-lint hook; this test keeps
// whichever arrangement the stage ends up with an honest one.
func TestPrePushRunsTheLintTarget(t *testing.T) {
	hooks, err := prePushHooks()
	if err != nil {
		t.Fatalf("reading the pre-push hooks: %v", err)
	}
	if len(hooks) == 0 {
		t.Fatal("no hook runs at the pre-push stage — a push passes ungated locally")
	}

	for _, hook := range hooks {
		if makeTargetEntry.MatchString(hook.entry) {
			return // the stage hands its gate list to the Makefile
		}
	}

	commands, err := makefileCommands("lint")
	if err != nil {
		t.Fatalf("reading the lint target's commands: %v", err)
	}
	for _, hook := range hooks {
		command := hook.entry
		if command == "" {
			command = hook.id // pre-commit's default entry
		}
		if !commandRunsGate(commands, command) {
			t.Errorf("the pre-push stage runs no make target, and the lint target runs no %q — the %s hook is a gate list of its own", command, hook.id)
		}
	}

	declared, err := configDeclaresFastOnly()
	if err != nil {
		t.Fatalf("reading .pre-commit-config.yaml: %v", err)
	}
	if !declared {
		t.Errorf(`the pre-push stage runs no make target, and the config declares nowhere that the stage holds only fast hooks — the fallback costs a comment that says so, e.g. "# The pre-push stage is fast-only: every hook also runs in make lint"`)
	}
}

// imageListCommand is the one derivation a stamped tag may be built from:
// the mirror's --list mode, which prints the mirrored ref of every pin in
// build/images.txt — registry, name and tag together — so a tag built from
// it cannot disagree with the payload the airgap bundle carries.
const imageListCommand = "tools/mirror-images.sh --list"

// ldflagStamp is one -X flag of GO_LDFLAGS: the symbol it stamps and the
// value it stamps into that symbol.
type ldflagStamp struct {
	symbol string
	value  string
}

// ldflagStamps returns the -X flags of a joined ldflags value, in the order
// they are stamped. -X takes its symbol=value as one shell word, the same
// boundary the go build line splits on, so every stamp survives the
// whitespace the joined continuations leave between the flags.
func ldflagStamps(ldflags string) []ldflagStamp {
	words := strings.Fields(ldflags)
	var stamps []ldflagStamp
	for i := 0; i+1 < len(words); i++ {
		if words[i] != "-X" {
			continue
		}
		if symbol, value, ok := strings.Cut(words[i+1], "="); ok {
			stamps = append(stamps, ldflagStamp{symbol: symbol, value: value})
		}
	}
	return stamps
}

// variableRefRE matches the variable references a make value can carry:
// $(NAME), and the name a value hands to make's $(call NAME,...). A
// function call — $(shell ...), $(filter a,b) — opens with the function's
// name and then a separator a reference cannot contain, so it names no
// variable. Variable names may carry a hyphen, so the class allows one —
// build/versions.mk's last hyphenated names (kube-vip_version,
// local-path_version) are gone now that image tags derive from
// build/images.txt, but a make name is free to hyphenate.
var variableRefRE = regexp.MustCompile(`\$\([ \t]*(?:call[ \t]+)?([A-Za-z0-9_-]+)[ \t]*[,)]`)

// variableRefs returns the names a make value references, in order of
// appearance.
func variableRefs(value string) []string {
	var refs []string
	for _, match := range variableRefRE.FindAllStringSubmatch(value, -1) {
		refs = append(refs, match[1])
	}
	return refs
}

// builtFromTheImageList reports whether a stamped value is built from the
// image list: the value references a variable — following the variables
// that variable references, so a derivation may be spelled through helpers —
// whose definition runs the list command. ETCD_IMAGE and SANDBOX_IMAGE are
// the shape this accepts: the flag references the variable, and the
// variable's definition reads build/images.txt through the mirror. A value
// spelled from build/versions.mk pins references variables defined outside
// the Makefile, or none at all, and is refused.
func builtFromTheImageList(value string, vars map[string]string) bool {
	walked := map[string]bool{}
	var derived func(name string) bool
	derived = func(name string) bool {
		if walked[name] {
			return false // a recursive variable ends the walk, not the answer
		}
		walked[name] = true
		definition, ok := vars[name]
		if !ok {
			return false // defined in build/versions.mk, or not a variable
		}
		if strings.Contains(definition, imageListCommand) {
			return true
		}
		for _, ref := range variableRefs(definition) {
			if derived(ref) {
				return true
			}
		}
		return false
	}
	for _, ref := range variableRefs(value) {
		if derived(ref) {
			return true
		}
	}
	return false
}

// stampedVersionSymbols are the -X symbols that stamp version facts without an
// *Image name: CoreDNS is stamped twice — once for main, once for the add-on
// package — and local-path and cilium once each. The list is frozen the way
// ciLintGates is: a symbol leaves it only deliberately, in the change that
// moves or retires the stamp, and the test says so when one has.
// github.com/ctrliq/stratum/internal/version.CiliumVersion joined last; until
// then the cilium stamp was the one version fact still resolved through a
// build/versions.mk copy, outside this net.
var stampedVersionSymbols = []string{
	"github.com/ctrliq/stratum/internal/version.CorednsVersion",
	"github.com/ctrliq/stratum/internal/version.CiliumVersion",
	"github.com/ctrliq/stratum/internal/addons.CorednsVersion",
	"github.com/ctrliq/stratum/internal/addons.LocalPathVersion",
}

// stampsAnImageTag reports whether a -X symbol stamps a tag build/images.txt
// pins. The repo's naming for an image stamp is a symbol ending in Image —
// every one today: NsenterImage, EtcdImage, the three kube-* images,
// KubeVipImage and SandboxImage — so the rule also picks up an image
// stamped later, the drift the image-stamp pin exists to catch. CoreDNS and
// local-path stamp their tags as bare versions, so their symbols are held in
// the frozen list beside this.
func stampsAnImageTag(symbol string) bool {
	name := symbol
	if i := strings.LastIndex(symbol, "."); i >= 0 {
		name = symbol[i+1:]
	}
	return strings.HasSuffix(name, "Image") || slices.Contains(stampedVersionSymbols, symbol)
}

// Every image tag the binary stamps — and the version facts stamped
// without an *Image symbol (the CoreDNS, local-path and cilium versions) — is
// built from a variable derived from the image list, as ETCD_IMAGE and
// SANDBOX_IMAGE are. build/images.txt is the pin surface for
// images: a tag stamped from a build/versions.mk copy is a second pin of the
// same tag, and the two can drift apart exactly the way the gate lists could.
// Every stamp (the three kube-* images, kube-vip, busybox-as-NsenterImage,
// CoreDNS, local-path, cilium) is derived off tools/mirror-images.sh --list,
// with no must-equal comments or lint-kube-images.sh checks guarding copies.
// This test is the pin — reintroducing a stamp as a second copy fails here,
// not on an air-gapped node.
func TestStampedImagesComeFromTheImageList(t *testing.T) {
	vars, err := makefileVariables()
	if err != nil {
		t.Fatalf("reading the Makefile's variables: %v", err)
	}
	ldflags, err := makefileVariable(vars, "GO_LDFLAGS")
	if err != nil {
		t.Fatalf("reading GO_LDFLAGS: %v", err)
	}
	engine, err := makefileVariable(vars, "ENGINE_LDFLAGS")
	if err != nil {
		t.Fatalf("reading ENGINE_LDFLAGS: %v", err)
	}
	if !strings.Contains(ldflags, "$(ENGINE_LDFLAGS)") {
		t.Fatalf("GO_LDFLAGS does not link $(ENGINE_LDFLAGS): %s", ldflags)
	}
	ldflags = strings.ReplaceAll(ldflags, "$(ENGINE_LDFLAGS)", engine)

	stamps := ldflagStamps(ldflags)
	stamped := make(map[string]bool, len(stamps))
	for _, stamp := range stamps {
		stamped[stamp.symbol] = true
	}
	for _, symbol := range stampedVersionSymbols {
		if !stamped[symbol] {
			t.Errorf("GO_LDFLAGS stamps no %s — the frozen version-symbol list is stale; move the entry to whatever now stamps that tag", symbol)
		}
	}

	for _, stamp := range stamps {
		if !stampsAnImageTag(stamp.symbol) {
			continue // a version or build identifier, not an image tag
		}
		if want, ok := builtInRepoImages[stamp.symbol]; ok {
			if v, err := makefileVariable(vars, want); err != nil || stamp.value != "$("+want+")" || !strings.Contains(v, "$(VERSION)") {
				t.Errorf("GO_LDFLAGS stamps %s with %q; want $(%s), named by the release $(VERSION)", stamp.symbol, stamp.value, want)
			}
			continue
		}
		if builtFromTheImageList(stamp.value, vars) {
			continue
		}
		t.Errorf("GO_LDFLAGS stamps %s with %q — a second copy of a tag build/images.txt pins by digest; build the value from a variable derived from %s, as ETCD_IMAGE and SANDBOX_IMAGE are", stamp.symbol, stamp.value, imageListCommand)
	}
}

// builtInRepoImages are the stamped images this repo builds rather than
// mirrors, so build/images.txt cannot pin them; each maps to the Makefile
// variable that names it.
var builtInRepoImages = map[string]string{
	"github.com/ctrliq/stratum/internal/addons.DashImage": "DASH_IMAGE",
}

// ciliumVersionPinRE matches a cilium_version assignment in
// build/versions.mk — the second copy of the version build/images.txt pins.
var ciliumVersionPinRE = regexp.MustCompile(`(?m)^cilium_version[ \t]*=[ \t]*(\S+)`)

// stampedVariablePointers are the spellings of the cilium stamp the
// build/images.txt comment may point at: the -X symbol the stamp writes, or
// the Makefile variable derived from this list that feeds it.
var stampedVariablePointers = []string{"github.com/ctrliq/stratum/internal/version.CiliumVersion", "CILIUM_VERSION"}

// ciliumImageListComment returns the comment block above the
// quay.io/cilium/cilium pin in build/images.txt: the lines a reader meets
// before the image line itself. A non-comment line resets the block, so the
// block is the one belonging to this pin and no other.
func ciliumImageListComment() (string, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "build", "images.txt"))
	if err != nil {
		return "", err
	}
	var comment []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "quay.io/cilium/cilium:") {
			if len(comment) == 0 {
				return "", fmt.Errorf("build/images.txt pins quay.io/cilium/cilium with no comment above it")
			}
			return strings.Join(comment, "\n"), nil
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			comment = append(comment, line)
		} else {
			comment = nil
		}
	}
	return "", fmt.Errorf("build/images.txt pins no quay.io/cilium/cilium image")
}

// The cilium version is pinned by build/images.txt alone. A version copy in
// build/versions.mk (cilium_version, held to the images.txt tags
// by nothing but the must-equal comment above them) must stay gone — no gate
// compares a second copy to the tags, so a re-added one drifts in silence — and
// the comment must point the reader at the stamp instead: the Makefile derives
// the stamped value from this list and writes it into
// github.com/ctrliq/stratum/internal/version.CiliumVersion, the same
// single-gate pattern every other image fact follows. Either half slipping back
// reopens that drift, so both are pinned here.
func TestCiliumVersionIsPinnedOnlyByTheImageList(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "build", "versions.mk"))
	if err != nil {
		t.Fatalf("reading build/versions.mk: %v", err)
	}
	if match := ciliumVersionPinRE.FindStringSubmatch(string(raw)); match != nil {
		t.Errorf("build/versions.mk still pins cilium_version = %s — build/images.txt is the one pin surface for the cilium version now, and nothing compares a second copy to its tags; delete the pin", match[1])
	}

	comment, err := ciliumImageListComment()
	if err != nil {
		t.Fatalf("reading the cilium comment in build/images.txt: %v", err)
	}
	pointed := false
	for _, pointer := range stampedVariablePointers {
		if strings.Contains(comment, pointer) {
			pointed = true
			break
		}
	}
	if !pointed {
		t.Errorf("the build/images.txt comment above the cilium pin names no stamped variable (one of %s) — the comment is where the next reader learns what pins the version, so point it at the Makefile's image-list stamp rather than at a build/versions.mk pin", strings.Join(stampedVariablePointers, " or "))
	}
}

// kubernetesVersionRE reads kubernetes_version out of build/versions.mk,
// where the version of the built binaries and the mirrored images is
// pinned.
var kubernetesVersionRE = regexp.MustCompile(`(?m)^kubernetes_version *= *(\S+) *$`)

// kubernetesVersion returns kubernetes_version from build/versions.mk.
func kubernetesVersion() (string, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "build", "versions.mk"))
	if err != nil {
		return "", err
	}
	if match := kubernetesVersionRE.FindStringSubmatch(string(raw)); match != nil {
		return match[1], nil
	}
	return "", fmt.Errorf("build/versions.mk pins no kubernetes_version")
}

// libraryVersionEqualTo maps a kubernetes_version to the k8s.io library
// version that exactly equals it, the way upstream tags its staging
// repositories: 1.37.1 ↔ v0.37.1. This is the version a release branch's
// go.mod must carry — what exact library/binary equality means.
func libraryVersionEqualTo(kubernetesVersion string) string {
	return "v0" + strings.TrimPrefix(kubernetesVersion, "1")
}

func prereleaseLibraryAheadOf(t *testing.T, kube string) string {
	t.Helper()
	minor, err := strconv.Atoi(strings.Split(kube, ".")[1])
	if err != nil {
		t.Fatalf("reading the minor of kubernetes_version %q: %v", kube, err)
	}
	return fmt.Sprintf("v0.%d.0-alpha.0", minor+1)
}

// kubePinFiles are the files tools/lint-kube-images.sh reads, copied into
// every scratch run: the gate itself, the binary pins, the image pins, the
// library pins, and the dev-tool pins. A directory with exactly these is a
// tree the gate can run in.
var kubePinFiles = []string{
	"tools/lint-kube-images.sh",
	"build/versions.mk",
	"build/images.txt",
	"go.mod",
	"mise.toml",
}

// pinScratch is a throwaway checkout of the pin surface on one branch: the
// gate's files as the single commit of a private git repository under a
// temp dir go test removes with the test.
type pinScratch struct {
	root string
}

// newPinScratch builds the scratch: the repo's pin files copied in, the
// edits applied, then everything committed on branch. The commit exists
// so every way the gate can ask git for the current branch — rev-parse,
// symbolic-ref, branch --show-current — answers with the branch, not with
// an unborn-HEAD error; and the scratch being its own repository means no
// branch of the repo running the tests leaks into the verdict.
func newPinScratch(t *testing.T, branch string, edits ...func(root string) error) *pinScratch {
	t.Helper()

	root := t.TempDir()
	for _, file := range kubePinFiles {
		raw, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatalf("copying %s into the scratch: %v", file, err)
		}
		dest := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("creating %s in the scratch: %v", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			t.Fatalf("writing %s in the scratch: %v", dest, err)
		}
	}
	for _, edit := range edits {
		if err := edit(root); err != nil {
			t.Fatalf("skewing the scratch copy: %v", err)
		}
	}

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in the scratch: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "--quiet", "-b", branch)
	git("add", "-A")
	git("-c", "user.name=gatelist", "-c", "user.email=gatelist@example.com",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		"commit", "--quiet", "--message", "pin policy scratch")
	return &pinScratch{root: root}
}

// runGate runs the scratch's copy of tools/lint-kube-images.sh and returns
// its combined output and exit status. The environment is cut to PATH and
// HOME so nothing of the caller — a CI ref, a make context — can flip the
// branch the gate detects or change what it runs.
func (s *pinScratch) runGate() (string, int, error) {
	cmd := exec.Command("bash", "tools/lint-kube-images.sh")
	cmd.Dir = s.root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode(), nil
	}
	if err != nil {
		return string(out), 0, err
	}
	return string(out), 0, nil
}

// rewriteLines rewrites a file line by line: every line becomes what edit
// returns for it.
func rewriteLines(path string, edit func(string) string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		lines[i] = edit(line)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// stagingLibraryVersionRE matches the version of a kubernetes staging
// library requirement — v0.<minor>.<patch>[-prerelease], the shape whose
// minor and patch mirror kubernetes_version — and not the k8s.io modules
// that version on their own: k8s.io/utils and kube-openapi carry v0.0.0-
// pseudo-versions, klog a v2.
var stagingLibraryVersionRE = regexp.MustCompile(`^v0\.[1-9][0-9]*\.`)

// pinLibrariesTo returns a scratch edit that rewrites every kubernetes
// staging library requirement of the scratch go.mod to one version — the
// shape of a tree whose libraries exactly track its binaries.
func pinLibrariesTo(version string) func(root string) error {
	return func(root string) error {
		return rewriteLines(filepath.Join(root, "go.mod"), func(line string) string {
			if !strings.HasPrefix(line, "\tk8s.io/") {
				return line
			}
			words := strings.Fields(line)
			if len(words) < 2 || !stagingLibraryVersionRE.MatchString(words[1]) {
				return line
			}
			words[1] = version
			return "\t" + strings.Join(words, " ")
		})
	}
}

// kubectlPinRE matches the kubectl entry of mise.toml, minor-pinned the
// way the repo pins it today.
var kubectlPinRE = regexp.MustCompile(`^(\s*kubectl\s*=\s*")[^"]*(")`)

// pinKubectlTo returns a scratch edit that rewrites the kubectl pin of the
// scratch mise.toml, keeping the entry's own shape.
func pinKubectlTo(version string) func(root string) error {
	return func(root string) error {
		return rewriteLines(filepath.Join(root, "mise.toml"), func(line string) string {
			return kubectlPinRE.ReplaceAllString(line, "${1}"+version+"${2}")
		})
	}
}

// kubeApiserverTagRE matches the tag on the kube-apiserver line of
// build/images.txt.
var kubeApiserverTagRE = regexp.MustCompile(`(registry\.k8s\.io/kube-apiserver:)[^@]+(@)`)

// skewKubeApiserverImage returns a scratch edit that rewrites the
// kube-apiserver tag of the scratch build/images.txt to a version
// kubernetes_version does not pin — the drift the gate has failed on
// since before this guard, and must keep failing on after it.
func skewKubeApiserverImage(tag string) func(root string) error {
	return func(root string) error {
		return rewriteLines(filepath.Join(root, "build", "images.txt"), func(line string) string {
			return kubeApiserverTagRE.ReplaceAllString(line, "${1}"+tag+"${2}")
		})
	}
}

// pinPolicyClause is one clause of the branch-aware pin policy: groups of
// alternative phrases, where a text states the clause when it matches at
// least one phrase from every group. The alternatives give the wording its
// freedom; what is pinned is the substance. Phrases are regexps matched
// against lowercased text.
type pinPolicyClause struct {
	name   string
	groups [][]string
}

// phraseAsWords shows a clause phrase as the words a writer writes: the
// word boundaries and escapes are the matcher's, not the message's.
func phraseAsWords(phrase string) string {
	return strings.NewReplacer(`\b`, "", `\.`, ".", `\s`, " ").Replace(phrase)
}

// missingFrom names one group the text nowhere matches, shown as the words
// a writer would use, or "" when the text states the clause.
func (c pinPolicyClause) missingFrom(text string) string {
	for _, group := range c.groups {
		stated := false
		for _, phrase := range group {
			if regexp.MustCompile(phrase).MatchString(text) {
				stated = true
				break
			}
		}
		if !stated {
			words := make([]string, 0, len(group))
			for _, phrase := range group {
				words = append(words, phraseAsWords(phrase))
			}
			return strings.Join(words, " / ")
		}
	}
	return ""
}

// pinPolicyClauses are the three clauses of the pin policy: release
// branches enforce exact library/binary equality; main allows prerelease
// drift but reports it (today's 1.37.1 binaries vs v0.38.0-alpha.0
// libraries warns, not fails); kubectl is enforced within
// upstream-supported skew (1.36 vs 1.37 passes).
var pinPolicyClauses = []pinPolicyClause{
	{
		name: "release branches enforce exact library/binary equality",
		groups: [][]string{
			{`\brelease\b`},
			{`exact`, `equal`, `identical`, `the same version as`},
			{`librar`, `go\.mod`, `client-go`, `k8s\.io`},
		},
	},
	{
		name: "main allows prerelease library drift but reports it",
		groups: [][]string{
			{`\bmain\b`},
			{`prerelease`, `pre-release`, `alpha`, `beta`, `\brc\b`},
			{`warn`, `report`, `notice`},
		},
	},
	{
		name: "kubectl stays within upstream-supported skew of kubernetes_version",
		groups: [][]string{
			{`\bkubectl\b`},
			{`skew`, `one minor`},
		},
	},
}

// pinPolicyMissing returns the first clause the text does not state, with
// the group of phrases it nowhere matches — or "" when the text states
// all three.
func pinPolicyMissing(text string) string {
	lower := strings.ToLower(text)
	for _, clause := range pinPolicyClauses {
		if missing := clause.missingFrom(lower); missing != "" {
			return clause.name + " — nowhere does it say (one of) " + missing
		}
	}
	return ""
}

// pinPolicyDocuments are the two places the repo states build policy:
// the pin manifest's
// header rules, which already state the other pin rules, and AGENTS.md.
var pinPolicyDocuments = []string{"build/versions.mk", "AGENTS.md"}

// pinPolicyHolders returns the build-policy documents that state all
// three clauses of the pin policy.
func pinPolicyHolders() ([]string, error) {
	var holders []string
	for _, doc := range pinPolicyDocuments {
		raw, err := os.ReadFile(filepath.Join("..", "..", doc))
		if err != nil {
			return nil, err
		}
		if pinPolicyMissing(string(raw)) == "" {
			holders = append(holders, doc)
		}
	}
	return holders, nil
}

// gateCommentText returns the comment text of tools/lint-kube-images.sh —
// the lines that make the gate self-describing.
func gateCommentText() (string, error) {
	raw, err := os.ReadFile(filepath.Join("..", "lint-kube-images.sh"))
	if err != nil {
		return "", err
	}
	var comments []string
	for _, line := range strings.Split(string(raw), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "#") {
			comments = append(comments, strings.TrimLeft(trimmed, "# "))
		}
	}
	return strings.Join(comments, "\n"), nil
}

// The branch-aware pin policy is documented where the repo states
// build policy — build/versions.mk's header rules or AGENTS.md. A rule a
// human chose that no document states lives only in the gate that enforces
// it, which is how the next reader comes to change it without knowing it
// was a decision.
func TestPinPolicyIsDocumented(t *testing.T) {
	var lacking []string
	for _, doc := range pinPolicyDocuments {
		raw, err := os.ReadFile(filepath.Join("..", "..", doc))
		if err != nil {
			t.Fatalf("reading %s: %v", doc, err)
		}
		if missing := pinPolicyMissing(string(raw)); missing != "" {
			lacking = append(lacking, fmt.Sprintf("%s: %s", doc, missing))
		}
	}
	if len(lacking) == len(pinPolicyDocuments) {
		t.Errorf("no build-policy document states the branch-aware pin policy — write it in build/versions.mk's header or in AGENTS.md. What each lacks: %s", strings.Join(lacking, "; "))
	}
}

// The gate's own comment states the rule it enforces and cites the
// document the policy is written in. The branch rule is a decision a human
// made, not a fact of the tree, and the comment is where the next reader
// learns that and where they are sent to read the policy itself. A target
// guard — red until the gate carries that comment.
func TestPinPolicyGateStatesItsRuleAndCitesItsPolicy(t *testing.T) {
	comments, err := gateCommentText()
	if err != nil {
		t.Fatalf("reading the gate's comments: %v", err)
	}
	if missing := pinPolicyMissing(comments); missing != "" {
		t.Errorf("the gate's comment does not state the branch-aware pin policy — %s", missing)
	}

	holders, err := pinPolicyHolders()
	if err != nil {
		t.Fatalf("reading the build-policy documents: %v", err)
	}
	if len(holders) == 0 {
		t.Errorf("the gate's comment has nothing to cite — no build-policy document states the pin policy yet")
		return
	}
	for _, holder := range holders {
		if strings.Contains(comments, filepath.Base(holder)) {
			return // the comment cites a document that states the policy
		}
	}
	bases := make([]string, 0, len(holders))
	for _, holder := range holders {
		bases = append(bases, filepath.Base(holder))
	}
	t.Errorf("the gate's comment cites none of the documents that state the pin policy — name %s in it", strings.Join(bases, " or "))
}

// Off a release branch the gate reports prerelease library drift
// without failing. Set the drift explicitly so these cases still exercise
// that policy when the repository's real pins legitimately match.
func TestPinPolicyGateReportsDriftWithoutFailingOffRelease(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatalf("reading today's kubernetes_version: %v", err)
	}
	library := prereleaseLibraryAheadOf(t, kube)

	// The second branch is the shape pull requests, worktrees and any
	// other non-release checkout carry: the gate must treat every branch
	// that is not a release branch the way it treats main, or `make lint`
	// fails on the very branches it runs on.
	for _, branch := range []string{"main", "feature/pin-policy-guard"} {
		t.Run(branch, func(t *testing.T) {
			scratch := newPinScratch(t, branch, pinLibrariesTo(library))
			out, code, err := scratch.runGate()
			if err != nil {
				t.Fatalf("running the gate's scratch copy: %v", err)
			}
			if code != 0 {
				t.Fatalf("the gate failed (exit %d) on %s, where the %s libraries against the %s binaries must WARN, not fail. Output:\n%s", code, branch, library, kube, out)
			}
			if !strings.Contains(strings.ToLower(out), "warn") || !strings.Contains(out, library) {
				t.Errorf("the gate ran green on %s but reported nothing: the drift between the %s binaries and the %s libraries must WARN, naming the library version. Output:\n%s", branch, kube, library, out)
			}
		})
	}
}

func TestPinPolicyGateRejectsUnsupportedLibraryDrift(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatal(err)
	}
	minor, err := strconv.Atoi(strings.Split(kube, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	equal := libraryVersionEqualTo(kube)
	for _, tc := range []struct {
		name, library string
		split         bool
	}{
		{"downgrade", "v0.1.0", false},
		{"stable next minor", fmt.Sprintf("v0.%d.0", minor+1), false},
		{"prerelease two minors ahead", fmt.Sprintf("v0.%d.0-alpha.0", minor+2), false},
		{"current minor prerelease", equal + "-rc.1", false},
		{"patch skew", equal + "1", false},
		{"split exact and allowed prerelease", prereleaseLibraryAheadOf(t, kube), true},
	} {
		for _, branch := range []string{"main", "feature/pin-policy-guard"} {
			t.Run(tc.name+"/"+branch, func(t *testing.T) {
				edits := []func(string) error{pinLibrariesTo(tc.library)}
				if tc.split {
					edits = append(edits, func(root string) error {
						return rewriteLines(filepath.Join(root, "go.mod"), func(line string) string {
							if strings.HasPrefix(line, "\tk8s.io/client-go ") {
								return "\tk8s.io/client-go " + equal
							}
							return line
						})
					})
				}
				scratch := newPinScratch(t, branch, edits...)
				out, code, err := scratch.runGate()
				if err != nil {
					t.Fatal(err)
				}
				if code == 0 || !strings.Contains(out, "ERROR") || !strings.Contains(out, tc.library) {
					t.Fatalf("unsupported %s libraries passed or lacked diagnostic (exit %d):\n%s", tc.library, code, out)
				}
			})
		}
	}
}

func TestPinPolicyGateReadsSingleLineStagingRequirement(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatal(err)
	}
	equal := libraryVersionEqualTo(kube)
	branch := "release/1." + strings.Split(kube, ".")[1]
	moveClientGo := func(root string) error {
		path := filepath.Join(root, "go.mod")
		moved := 0
		if err := rewriteLines(path, func(line string) string {
			if strings.HasPrefix(line, "\tk8s.io/client-go ") {
				moved++
				return ""
			}
			return line
		}); err != nil {
			return err
		}
		if moved != 1 {
			return fmt.Errorf("expected one block-form client-go requirement, found %d", moved)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(raw, []byte("\nrequire k8s.io/client-go v0.1.0\n")...), 0o644)
	}
	scratch := newPinScratch(t, branch, pinLibrariesTo(equal), moveClientGo)
	out, code, err := scratch.runGate()
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || !strings.Contains(out, "v0.1.0") {
		t.Fatalf("single-line staging requirement drift passed or was unnamed (exit %d):\n%s", code, out)
	}
}

// Release branches enforce exact library/binary equality. The same
// tree that warns on main fails on release/* — a release branch is where
// the shipped binaries and the compiled-in libraries must be one version —
// and a go.mod whose libraries equal kubernetes_version passes. The branch
// is numbered after the manifest the way a real release branch is.
func TestPinPolicyGateEnforcesEqualityOnReleaseBranches(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatalf("reading today's kubernetes_version: %v", err)
	}
	library := prereleaseLibraryAheadOf(t, kube)
	equal := libraryVersionEqualTo(kube)
	branch := "release/1." + strings.Split(kube, ".")[1]

	t.Run("prerelease drift fails", func(t *testing.T) {
		scratch := newPinScratch(t, branch, pinLibrariesTo(library))
		out, code, err := scratch.runGate()
		if err != nil {
			t.Fatalf("running the gate's scratch copy: %v", err)
		}
		if code == 0 {
			t.Errorf("the gate passed on %s with the %s libraries against the %s binaries — release branches enforce exact equality. Output:\n%s", branch, library, kube, out)
			return
		}
		if !strings.Contains(out, library) {
			t.Errorf("the gate failed on %s but its output names neither the %s libraries nor the drift it failed on. Output:\n%s", branch, library, out)
		}
	})

	t.Run("exact equality passes", func(t *testing.T) {
		scratch := newPinScratch(t, branch, pinLibrariesTo(equal))
		out, code, err := scratch.runGate()
		if err != nil {
			t.Fatalf("running the gate's scratch copy: %v", err)
		}
		if code != 0 {
			t.Errorf("the gate failed (exit %d) on %s with the libraries equal to %s — exact equality is what release branches enforce. Output:\n%s", code, branch, equal, out)
		}
	})
}

// kubectl is checked within upstream-supported skew of
// kubernetes_version — one minor older or newer, upstream's own rule — on
// every branch, because mise.toml's kubectl is the one dev tool pinned
// against the pinned cluster. The libraries are equalized in every case
// so a failure can name only kubectl. Within-skew holds today (1.36
// against 1.37); beyond skew, either
// direction, fails. The one-minor-ahead case is left out on purpose: the
// repo's own tighter rule — mise.toml's kubectl tracks
// kubernetes_version rather than drifting ahead of it — may forbid what
// upstream allows, and this guard pins only the upstream edge.
func TestPinPolicyGateChecksKubectlWithinUpstreamSkew(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatalf("reading today's kubernetes_version: %v", err)
	}
	minor, err := strconv.Atoi(strings.Split(kube, ".")[1])
	if err != nil {
		t.Fatalf("reading the minor of kubernetes_version %q: %v", kube, err)
	}
	equal := libraryVersionEqualTo(kube)
	within, tooOld, tooNew := fmt.Sprintf("1.%d", minor-1), fmt.Sprintf("1.%d", minor-2), fmt.Sprintf("1.%d", minor+2)

	gate := func(t *testing.T, branch, kubectl string) (string, int) {
		t.Helper()
		scratch := newPinScratch(t, branch, pinLibrariesTo(equal), pinKubectlTo(kubectl))
		out, code, err := scratch.runGate()
		if err != nil {
			t.Fatalf("running the gate's scratch copy: %v", err)
		}
		return out, code
	}

	t.Run("one minor behind passes", func(t *testing.T) {
		out, code := gate(t, "main", within)
		if code != 0 {
			t.Errorf("the gate failed (exit %d) with kubectl %s against kubernetes_version %s — one minor is within upstream-supported skew. Output:\n%s", code, within, kube, out)
		}
	})
	t.Run("two minors behind fails", func(t *testing.T) {
		out, code := gate(t, "main", tooOld)
		if code == 0 {
			t.Errorf("the gate passed with kubectl %s against kubernetes_version %s — two minors is beyond upstream-supported skew. Output:\n%s", tooOld, kube, out)
			return
		}
		if !strings.Contains(strings.ToLower(out), "kubectl") || !strings.Contains(out, tooOld) {
			t.Errorf("the gate failed but its output does not name kubectl %s as the reason. Output:\n%s", tooOld, out)
		}
	})
	t.Run("two minors ahead fails", func(t *testing.T) {
		out, code := gate(t, "main", tooNew)
		if code == 0 {
			t.Errorf("the gate passed with kubectl %s against kubernetes_version %s — two minors is beyond upstream-supported skew. Output:\n%s", tooNew, kube, out)
			return
		}
		if !strings.Contains(strings.ToLower(out), "kubectl") || !strings.Contains(out, tooNew) {
			t.Errorf("the gate failed but its output does not name kubectl %s as the reason. Output:\n%s", tooNew, out)
		}
	})
	t.Run("on a release branch too", func(t *testing.T) {
		branch := "release/1." + strconv.Itoa(minor)
		out, code := gate(t, branch, tooOld)
		if code == 0 {
			t.Errorf("the gate passed on %s with kubectl %s against kubernetes_version %s — the skew rule holds on every branch. Output:\n%s", branch, tooOld, kube, out)
		}
	})
}

// The image-tag check keeps its verdict alongside the pin-policy checks.
// The scratch skews only the kube-apiserver tag, with the
// libraries equalized, so the failure can name only the image — the guard
// file's "test mismatched image tags independently". A pin: the gate
// fails on this today.
func TestPinPolicyGateStillFailsOnMismatchedImageTags(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatalf("reading today's kubernetes_version: %v", err)
	}
	minor, err := strconv.Atoi(strings.Split(kube, ".")[1])
	if err != nil {
		t.Fatalf("reading the minor of kubernetes_version %q: %v", kube, err)
	}
	skewed := fmt.Sprintf("v1.%d.9", minor-1)

	scratch := newPinScratch(t, "main", pinLibrariesTo(libraryVersionEqualTo(kube)), skewKubeApiserverImage(skewed))
	out, code, err := scratch.runGate()
	if err != nil {
		t.Fatalf("running the gate's scratch copy: %v", err)
	}
	if code == 0 {
		t.Errorf("the gate passed with kube-apiserver pinned at %s against kubernetes_version %s — the image check predates this guard and must keep failing. Output:\n%s", skewed, kube, out)
		return
	}
	if !strings.Contains(out, "kube-apiserver") {
		t.Errorf("the gate failed but its output does not name kube-apiserver as the reason. Output:\n%s", out)
	}
}
