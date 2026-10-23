#!/usr/bin/env bash
# Asserts that every stratum command and flag appearing in the documentation
# actually exists in the binary.
# Usage: tools/lint-docs-cli.sh [file ...]   (default: all tracked *.md)
#
# This exists because documentation drifts in one direction: a flag gets renamed
# in pkg/cli and the page that documents it does not. The reader then copies
# a command that exits 2. Nothing else in CI reads the docs, so nothing else
# notices. Flag-level drift — a flag that exists, on the wrong command or
# spelled for an older CLI — is the common case, which is why this checks
# flags and not just verbs.
#
# Flags are checked against the command they appear ON, not against the CLI as a
# whole. `stratum snapshot save --s3-bucket …` names a real flag, but --s3-bucket
# belongs to `snapshot schedule`; a global check passes it. That error reads more
# plausibly than an invented flag, so it is likelier to survive review — and the
# failure names the commands that do take the flag, because "wrong" is less useful
# than "you wanted this one".
#
# A command with no flag set of its own to model — a bare group (`certs`,
# `snapshot`, `token`, `registry`) or a passthrough — falls back to
# the global check rather than failing. Tightening must not invent errors on the
# parts of the CLI it cannot see.
#
# The check is deliberately narrow. It only looks inside code fences and inline
# code spans, and only at lines that invoke `stratum`, because:
#   - prose says things like "the stratum supervisor", which is not a command
#   - docs legitimately show other tools' flags: `git --oneline`, `pip --quiet`,
#     `kubectl --replicas` all appear, and none of them are stratum's
#   - a widened check would also have to reason about CSS custom properties, which
#     are indistinguishable from flags by shape (`--link-color` vs `--cluster`)
# Widening any of those turns this into a source of false failures, which is how
# a lint gets disabled.
#
# Scope is all tracked *.md, so once the public docs site lands under
# docs/ it covers that too — and most of the invocations checked then come
# from it. That is the tree most worth covering: it is written for readers rather
# than maintainers, so its drift is the drift that strands someone.
#
# Verified by mutation, not by observing it pass: a fake verb, a fake flag, a bad
# command inside a MkDocs `!!!` admonition — whose body, and any fence in it, is
# indented four spaces — and one inside a plain fenced block are each caught with
# file:line. A lint nobody has watched fail is a lint nobody knows is wired up.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

files="$*"
if [[ -z $files ]]; then
  files=$(git ls-files '*.md' 2>/dev/null)
fi
[[ -n $files ]] || { echo "lint-docs-cli: no markdown files to check"; exit 0; }

# shellcheck disable=SC2086  # $files is a space-separated path list; splitting it is the point
python3 - $files <<'PY'
import re, sys, pathlib

root = pathlib.Path(".")

# --- the binary's real surface -------------------------------------------------
#
# The CLI is a cobra tree rooted at pkg/cli.NewRootCommand (cmd/stratum/main.go is
# a thin shim that calls it and adds `dash`). Verbs and flags are parsed from that
# source rather than from `--help` output, so this runs without building the binary
# — a lint that needed a 25-minute component build would not run on every PR.
#
# Two commands live outside pkg/cli and are added onto the root by name:
#   - `stratum dash` is defined in internal/dashboard (wired in by cmd/stratum as
#     dashboard.Command() so the dashboard server does not compile into
#     downstream builds that import pkg/cli).
#   - `stratum operator` sets DisableFlagParsing and hands its args to
#     operator.ParseFlags, whose flag.FlagSet lives in internal/operator.
# Both are part of the binary's surface, so those packages are parsed too.
def go_files(*globs):
    out = []
    for g in globs:
        for p in sorted(root.glob(g)):
            if not p.name.endswith("_test.go"):   # test fixtures define throwaway cmds
                out.append(p.read_text(encoding="utf-8", errors="replace"))
    return out

# cmd/stratum/main.go is the entrypoint that assembles the binary's command set —
# it calls pkg/cli.NewRootCommand and adds `dash` — so it is the fail-closed
# anchor: if it is gone, the surface cannot be trusted and the lint must not pass
# (the "or cmd/stratum absent → fail" half of the guarantee). It also decides
# whether the dashboard commands are really in the binary: `stratum dash` exists
# only while main.go registers dashboard.Command() (the dashboard is kept out
# of pkg/cli itself), so its sources join the surface only then — otherwise a
# removed registration would leave `stratum dash` wrongly accepted.
main_go = root / "cmd/stratum/main.go"
if not main_go.is_file():
    print("lint-docs-cli: cmd/stratum/main.go not found — run this from the "
          "repository root (the CLI entrypoint is required to model the surface)",
          file=sys.stderr)
    sys.exit(1)
main_src = main_go.read_text(encoding="utf-8", errors="replace")

cmd_globs = ["pkg/cli/*.go"]
if "dashboard.Command()" in main_src:
    cmd_globs.append("internal/dashboard/*.go")
cmd_files = go_files(*cmd_globs)                      # where cobra commands live
op_src = "".join(go_files("internal/operator/*.go"))  # operator's own FlagSet
cmd_src = "".join(cmd_files)

# A pflag/flag registration in any form — plain (`String`), pointer (`StringVar`)
# and shorthand (`…P`) — carries the flag name in the first string literal, after
# an optional &target argument.
FLAGDEF = re.compile(
    r'\.(?:String|Bool|Int|Int8|Int16|Int32|Int64|Uint|Uint16|Uint32|Uint64'
    r'|Float32|Float64|Duration|Count|IP|BytesHex)(?:Slice|Array)?(?:Var)?P?'
    r'\(\s*(?:&[A-Za-z_][\w.]*\s*,\s*)?"([a-z][a-z0-9-]*)"')

# Verbs: every cobra command name. Two spellings — a literal `Use: "verb ..."` (the
# first token names the command) and the groups built by newGroupCommand, whose
# name is its first string argument (certs, snapshot, token, ...).
verbs = set(re.findall(r'Use:\s*"([a-z][a-z0-9-]*)', cmd_src))
verbs |= set(re.findall(r'newGroupCommand\(\s*"([a-z][a-z0-9-]*)"', cmd_src))
verbs.discard("stratum")   # the root itself; never written as `stratum stratum`

# One verb this binary does not have: `stratum hardening` is a command of the
# Stratum PRO binary, which carries the CIS profile and its auditor. The
# release notes are history and keep what each release shipped, so there the
# verb is accepted without being checked against this binary; everywhere else
# the generic checks below refuse it as the unknown command it is here.
HISTORY_DOCS = "docs/docs/releases/"
HISTORY_VERBS = {"hardening"}

# The cis profile is not carried by this binary either: `--cis` is the
# engine's legacy alias, kept one release so a node that still names it is
# refused by name, and `--profile cis` names a profile only the PRO binary
# registers. Both parse here, so the flag checks below would pass them;
# outside the release notes a page that documents them fails. `--profile
# <name>` stays: the profile mechanism is the engine's. The value may be
# quoted either way (`--profile "cis"`, `--profile='cis'`): the shell strips
# the quotes, so they select the profile all the same.
CIS_FLAGS = {"cis"}
PROFILE_CIS = re.compile(r"""(?<![\w-])--profile(?:=|\s+)(['"]?)cis\1(?![\w-])""")

def is_history_doc(fname):
    return pathlib.PurePosixPath(fname).as_posix().startswith(HISTORY_DOCS)

# The flat set: a flag defined anywhere in the CLI. It is the fallback for the
# invocations we cannot scope to one command (a bare group, a passthrough).
# Keep the PARSED flags separate from cobra's implicit --help/-h: the vacuity
# guard below rejects an empty parsed set, and adding help/h first would keep it
# non-empty and let the lint pass having modelled no real flag at all.
parsed_flags = set(FLAGDEF.findall(cmd_src)) | set(FLAGDEF.findall(op_src))
flags = parsed_flags | {"help", "h"}   # cobra/pflag add these to every command

# Which flags belong to WHICH command. The flat set only proves a flag exists
# somewhere, so `stratum token create --worker` passes it: --worker is real but
# belongs to `up`. Using a real flag on the wrong command is the same
# class of error as inventing one, and reads more plausibly, so it is likelier to
# survive review.
#
# cobra attaches flags to a command in its own constructor, in source order: the
# flags between one `Use: "name"` and the next (within a file) belong to that
# command. Groups built by newGroupCommand declare no literal Use and no flags, so
# they never key this map — an invocation landing on a group falls through to the
# flat check. Segments are bounded per file so one file's trailing code can never
# be attributed to a command declared in another.
cmd_flags = {}
for _text in cmd_files:
    _uses = [(m.start(), m.group(1))
             for m in re.finditer(r'Use:\s*"([a-z][a-z0-9-]*)', _text)]
    _bounds = [p for p, _ in _uses] + [len(_text)]
    for _i, (_pos, _name) in enumerate(_uses):
        cmd_flags.setdefault(_name, set()).update(
            FLAGDEF.findall(_text[_pos:_bounds[_i + 1]]))
cmd_flags.pop("stratum", None)   # the root takes no flags of its own (no PersistentFlags)
# operator delegates: its flag.FlagSet does not sit beside Use:"operator", so add it here.
cmd_flags.setdefault("operator", set()).update(FLAGDEF.findall(op_src))

def command_scope(words):
    """Index of the deepest invocation word that names a command we have flags for.

    cobra attaches flags to the leaf command, so in `stratum certs rotate` the flags
    belong to "rotate". None means no flag model for this command (a bare group like
    `certs`, or a passthrough) — the flat check applies instead, so tightening never
    invents errors on a command it cannot see.
    """
    for _i in (1, 0):
        if _i < len(words) and words[_i] in cmd_flags:
            return _i
    return None

if not verbs or not parsed_flags:
    print("lint-docs-cli: could not parse the CLI surface from pkg/cli "
          f"(verbs={len(verbs)}, flags={len(parsed_flags)}) — refusing to pass "
          "vacuously", file=sys.stderr)
    sys.exit(1)

# --- what the docs claim -------------------------------------------------------
FENCE = re.compile(r"^\s*(```|~~~)")
INLINE = re.compile(r"`([^`]+)`")
INVOKE = re.compile(r"\bstratum\s+([a-z][a-z0-9-]*(?:\s+[a-z][a-z0-9-]*)?)")
FLAG = re.compile(r"(?<![\w-])--([a-z][a-z0-9-]*)")

# `stratum kubectl` and `stratum etcdctl` exec the bundled tool and hand it the
# rest of the line. Their flags are kubectl's and etcdctl's, not ours, so the
# verb is checked and the flags are not.
PASSTHROUGH = {"kubectl", "etcdctl"}

fail = 0
checked_v = checked_f = 0

for fname in sys.argv[1:]:
    path = pathlib.Path(fname)
    if not path.is_file():
        continue
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()

    # Gather candidate command lines: fenced-block lines, plus inline code spans.
    cmds, in_fence, pending = [], False, ""
    for n, line in enumerate(lines, 1):
        if FENCE.match(line):
            in_fence = not in_fence
            continue
        if in_fence:
            texts = [line]
        else:
            # Each inline span is its OWN snippet. Joining every span on a line
            # conflates unrelated ones: a sentence containing `--network none`
            # and `stratum stage` is not a command with a --network flag.
            texts = INLINE.findall(line)
            if not texts:
                continue
        for text in texts:
            # Join backslash continuations so a wrapped command is one command.
            joined = pending + " " + text if pending else text
            if joined.rstrip().endswith("\\"):
                pending = joined.rstrip()[:-1]
                continue
            pending = ""
            cmds.append((n, joined))

    for n, cmd in cmds:
        m = INVOKE.search(cmd)
        if not m:
            continue                      # not a stratum invocation; ignore its flags
        words = m.group(1).split()
        verb = words[0]
        checked_v += 1
        if is_history_doc(fname):
            if verb in HISTORY_VERBS:
                continue
        elif verb not in PASSTHROUGH:
            tail = cmd[m.end():]
            cis = [f"--{f}" for f in FLAG.findall(tail) if f in CIS_FLAGS]
            if PROFILE_CIS.search(tail):
                cis.append("--profile cis")
            for what in cis:
                print(f"lint-docs-cli: {fname}:{n}: `{what}` names the cis profile, which "
                      f"this binary does not carry; link the Stratum PRO page instead")
                fail = 1
            if cis:
                continue
        if verb not in verbs:
            print(f"lint-docs-cli: {fname}:{n}: `stratum {verb}` is not a command")
            fail = 1
        if verb in PASSTHROUGH:
            continue
        scope_i = command_scope(words)
        # Only what follows the verb. Flags before it belong to whatever wraps the
        # invocation — `docker run --network none stratum stage`, `sudo -E`.
        for f in FLAG.findall(cmd[m.end():]):
            checked_f += 1
            if scope_i is not None:
                key = words[scope_i]
                if f not in cmd_flags[key] and f not in ("help", "h"):
                    # Naming the commands that DO take it turns "wrong" into
                    # "you wanted this one", which is the whole value of scoping.
                    owners = sorted(c for c, fl in cmd_flags.items() if f in fl)
                    hint = (" — it belongs to " + ", ".join(f"`{c}`" for c in owners)
                            if owners else "")
                    disp = " ".join(words[:scope_i + 1])
                    print(f"lint-docs-cli: {fname}:{n}: `--{f}` is not a flag of "
                          f"`stratum {disp}`{hint}")
                    fail = 1
            elif f not in flags:
                print(f"lint-docs-cli: {fname}:{n}: `--{f}` is not a flag "
                      f"(on a `stratum {verb}` line)")
                fail = 1

print(f"lint-docs-cli: checked {checked_v} invocation(s) and {checked_f} flag(s) "
      f"against {len(verbs)} verbs and {len(flags)} flags "
      f"scoped across {len(cmd_flags)} commands")
sys.exit(fail)
PY
rc=$?
[[ $rc -eq 0 ]] || { echo "lint-docs-cli: FAILED — the docs describe a CLI the binary does not have" >&2; exit 1; }
echo "lint-docs-cli: ok"
