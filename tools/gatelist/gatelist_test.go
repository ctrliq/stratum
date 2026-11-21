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

// A stub npx proves credential isolation without downloading the validator.
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

// A step that only runs an action (`uses:`) has no Run.
type ciStep struct {
	Name string `json:"name"`
	Run  string `json:"run"`
}

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

type makeRule struct {
	prerequisites []string
	commands      []string
}

// Only literal prerequisite names resolve; shell comments run no gate.
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
				// Tab lines outside any recipe belong to variable assignments.
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
			// ifeq/else/endif can sit inside a recipe without ending it.
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

func isConditionalDirective(line string) bool {
	for _, directive := range []string{"ifeq", "ifneq", "ifdef", "ifndef", "else", "endif"} {
		if line == directive || strings.HasPrefix(line, directive+" ") {
			return true
		}
	}
	return false
}

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

// A gate spelled across a continuation must match as one command.
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

// build/versions.mk is not read: the derivations under guard live in the Makefile.
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

		// make folds the backslash, newline and leading whitespace into one separator.
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

// `+=` and `!=` appear nowhere in the Makefile and stay unrecognized.
var assignmentRE = regexp.MustCompile(`^([A-Za-z0-9_]+)[ \t]*(::=|:=|\?=|=)[ \t]*(.*)$`)

func parseAssignment(line string) (name, value string, ok bool) {
	match := assignmentRE.FindStringSubmatch(line)
	if match == nil {
		return "", "", false
	}
	return match[1], match[3], true
}

// A guard reading a renamed variable must fail on the rename, not answer empty.
func makefileVariable(vars map[string]string, name string) (string, error) {
	value, ok := vars[name]
	if !ok {
		return "", fmt.Errorf("the Makefile assigns no %s", name)
	}
	return value, nil
}

// A gate leaves this list only deliberately, in the change that retires it.
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

// The trailing boundary: `make lint-versions` must not count as `make lint`.
var makeLintStep = regexp.MustCompile(`(^|[\s;&|])make[ \t]+lint([\s;&|]|$)`)

func stepRunsGate(steps []ciStep, gate string) bool {
	for _, step := range steps {
		if strings.Contains(step.Run, gate) {
			return true
		}
	}
	return false
}

func commandRunsGate(commands []string, gate string) bool {
	for _, command := range commands {
		if strings.Contains(command, gate) {
			return true
		}
	}
	return false
}

// A gate neither a job step nor carried by `make lint` has left pull requests.
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

// A self-test that no gate list runs proves nothing.
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

// Without --check-pins run-skew.sh drives a live cluster, not a pull request.
const skewPinGate = "tools/ostests/run-skew.sh --check-pins"

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

// A hook with no entry runs its id, pre-commit's default.
type preCommitHook struct {
	id    string
	entry string
}

// A hook with no stages key runs at every stage, pre-push included.
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

// The target word is required, so `command -v make` does not count.
var makeTargetEntry = regexp.MustCompile(`(?:^|\W)make[ \t]+\S+`)

// The wording must name the stage and put "fast" beside "only"; the header says the opposite.
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

// A push-time-only gate list is drift unless the stage hands to make or is fast-only.
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

// A tag built from the mirror's --list output cannot disagree with the payload.
const imageListCommand = "tools/mirror-images.sh --list"

type ldflagStamp struct {
	symbol string
	value  string
}

// -X takes symbol=value as one shell word, the boundary go build splits on.
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

// $(shell ...) and friends open with the function's name, so they name no variable.
var variableRefRE = regexp.MustCompile(`\$\([ \t]*(?:call[ \t]+)?([A-Za-z0-9_-]+)[ \t]*[,)]`)

func variableRefs(value string) []string {
	var refs []string
	for _, match := range variableRefRE.FindAllStringSubmatch(value, -1) {
		refs = append(refs, match[1])
	}
	return refs
}

// A versions.mk-derived value references variables outside the Makefile, or none.
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

// A symbol leaves this frozen list only in the change that moves or retires it.
var stampedVersionSymbols = []string{
	"github.com/ctrliq/stratum/internal/version.CorednsVersion",
	"github.com/ctrliq/stratum/internal/version.CiliumVersion",
	"github.com/ctrliq/stratum/internal/addons.CorednsVersion",
	"github.com/ctrliq/stratum/internal/addons.LocalPathVersion",
}

// Image stamps end in Image, so the rule also picks up an image stamped later.
func stampsAnImageTag(symbol string) bool {
	name := symbol
	if i := strings.LastIndex(symbol, "."); i >= 0 {
		name = symbol[i+1:]
	}
	return strings.HasSuffix(name, "Image") || slices.Contains(stampedVersionSymbols, symbol)
}

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

// Images this repo builds rather than mirrors; images.txt cannot pin them.
var builtInRepoImages = map[string]string{
	"github.com/ctrliq/stratum/internal/addons.DashImage": "DASH_IMAGE",
}

var ciliumVersionPinRE = regexp.MustCompile(`(?m)^cilium_version[ \t]*=[ \t]*(\S+)`)

var stampedVariablePointers = []string{"github.com/ctrliq/stratum/internal/version.CiliumVersion", "CILIUM_VERSION"}

// A non-comment line resets the block, so it belongs to this pin alone.
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

var kubernetesVersionRE = regexp.MustCompile(`(?m)^kubernetes_version *= *(\S+) *$`)

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

// Upstream tags staging repos to mirror kubernetes_version: 1.37.1 ↔ v0.37.1.
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

var kubePinFiles = []string{
	"tools/lint-kube-images.sh",
	"build/versions.mk",
	"build/images.txt",
	"go.mod",
	"mise.toml",
}

type pinScratch struct {
	root string
}

// The commit keeps the branch resolvable; the scratch is its own repository.
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

// The environment is cut so a CI ref or make context cannot flip the branch.
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

// Must not match k8s.io/utils, kube-openapi or klog, which version on their own.
var stagingLibraryVersionRE = regexp.MustCompile(`^v0\.[1-9][0-9]*\.`)

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

var kubectlPinRE = regexp.MustCompile(`^(\s*kubectl\s*=\s*")[^"]*(")`)

func pinKubectlTo(version string) func(root string) error {
	return func(root string) error {
		return rewriteLines(filepath.Join(root, "mise.toml"), func(line string) string {
			return kubectlPinRE.ReplaceAllString(line, "${1}"+version+"${2}")
		})
	}
}

var kubeApiserverTagRE = regexp.MustCompile(`(registry\.k8s\.io/kube-apiserver:)[^@]+(@)`)

func skewKubeApiserverImage(tag string) func(root string) error {
	return func(root string) error {
		return rewriteLines(filepath.Join(root, "build", "images.txt"), func(line string) string {
			return kubeApiserverTagRE.ReplaceAllString(line, "${1}"+tag+"${2}")
		})
	}
}

// Phrases are regexps on lowercased text; the alternatives give wording freedom.
type pinPolicyClause struct {
	name   string
	groups [][]string
}

func phraseAsWords(phrase string) string {
	return strings.NewReplacer(`\b`, "", `\.`, ".", `\s`, " ").Replace(phrase)
}

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

func pinPolicyMissing(text string) string {
	lower := strings.ToLower(text)
	for _, clause := range pinPolicyClauses {
		if missing := clause.missingFrom(lower); missing != "" {
			return clause.name + " — nowhere does it say (one of) " + missing
		}
	}
	return ""
}

var pinPolicyDocuments = []string{"build/versions.mk", "AGENTS.md"}

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

// A human rule no document states lives only in the gate that enforces it.
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

// The drift is set explicitly so the cases exercise the policy when real pins match.
func TestPinPolicyGateReportsDriftWithoutFailingOffRelease(t *testing.T) {
	kube, err := kubernetesVersion()
	if err != nil {
		t.Fatalf("reading today's kubernetes_version: %v", err)
	}
	library := prereleaseLibraryAheadOf(t, kube)

	// The gate must treat every non-release branch the way it treats main.
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

// Libraries equalized so a failure names only kubectl; one minor ahead untested.
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
