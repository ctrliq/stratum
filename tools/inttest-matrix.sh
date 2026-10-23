#!/usr/bin/env bash
# Emits the GitHub Actions matrix for the integration suite by scanning
# tools/inttest/ — so adding a test file adds a CI job, with no workflow edit.
#
# A hardcoded list in workflow YAML would be a second place to forget.
#
# Each check declares its needs in a header comment:
#   # ske-inttest: network=none      run with --network none (default: bridge)
#   # ske-inttest: pending=<reason>  known-not-yet-possible; emitted as skipped
#   # ske-inttest: topology=ha3      three node containers on one network, the
#                                   check run on the host (tools/inttest/harness-ha.sh)
#   # ske-inttest: flaky=until-YYYY-MM-DD   quarantined; ci.yml retries
#                                   the check once, until the date passes
#   # ske-inttest: previous=<tag>   the check needs that published engine
#                                   release at /dist/stratum-previous; ci.yml
#                                   downloads it, tools/inttest-run.sh stages
#                                   it from PREVIOUS=<path>
#
# flaky and previous are read in-workflow, NOT emitted here: every non-empty
# matrix value lands in the job name, and renaming a job silently drops it
# from the branch rule's required-check list. ci.yml requires the flaky value
# to match ^until-[0-9]{4}-[0-9]{2}-[0-9]{2}$ — a quarantine with no expiry
# gets no retry, and nothing else from the file reaches the workflow command.
# Say why the check is quarantined in a comment beside the directive.
# previous is read through --directive, below.
#
# Usage:
#   tools/inttest-matrix.sh                          the GitHub Actions matrix
#   tools/inttest-matrix.sh --directive <k> <check>  one directive's value,
#                                                    or its default if absent
#
# --directive exists so nothing else needs a second parser for the same
# header. A second reader would drift — a truncating charset, an empty value,
# a duplicate key, a key this rejects as unknown — and each divergence runs
# a check differently from what CI thinks it runs. There is one parser, and
# it is below.
#
# ci.yml still reads flaky itself, with a sed of its own (the value must not
# reach the workflow through the matrix, which renames the job); it reads
# previous through --directive, like inttest-run.sh.
# tools/test-inttest-header.sh holds the flaky pair to this parser via --directive.
set -euo pipefail
cd "$(dirname "$0")/.."

python3 - "$@" <<'PY'
import glob, itertools, json, os, re, sys

# Everything emitted here ends up in a job name and in ci.yml's shell step, and
# the filenames are whatever a PR happened to add under tools/inttest/. Validate
# once, at the single place these values are produced, rather than escaping at
# each use site — a name that cannot hold a quote, a semicolon or a newline
# cannot break out of anything downstream. Fail the matrix job loudly instead of
# emitting a poisoned matrix.
SAFE_FILE = re.compile(r"^check-[a-z0-9]+(?:-[a-z0-9]+)*\.sh$")
# An unknown key is a typo, and a typo that is merely dropped changes how the
# check runs with nothing said — `netowrk=none` quietly gets the default bridge.
KNOWN = ("flaky", "network", "pending", "previous", "topology")
SAFE_VALUE = re.compile(r"^[A-Za-z0-9._-]*$")

def directives_of(path):
    """The one parse of the `# ske-inttest:` header. Everything that reads a
    directive — the matrix below, and tools/inttest-run.sh via
    --directive —
    comes through here, so there is nothing for a second reader to disagree
    with."""
    if not SAFE_FILE.match(os.path.basename(path)):
        # %r, not %s: the offending name is exactly the one that may carry a
        # newline, and a multi-line message on stderr is what Actions' log
        # parsing then has to guess at. repr() keeps it one line, escaped.
        sys.exit("inttest-matrix: %r: check filenames must match "
                 "check-<lowercase-dash-separated>.sh" % path)
    with open(path) as f:  # the directives live in the header; islice stops there
        head = "".join(itertools.islice(f, 20))
        # ...and a directive BELOW the header would be read by nobody and
        # silently do nothing — a check that thinks it is quarantined, or
        # network=none, and is not. Same handle, so this costs one pass and no
        # re-read. Refuse it rather than ignore it.
        for line in f:
            if line.startswith("# ske-inttest:"):
                sys.exit("inttest-matrix: %r: `# ske-inttest:` directive below the "
                         "20-line header would be ignored — move it up" % path)
    # Built with a duplicate check rather than dict() straight off: dict keeps
    # the last of a repeated key, so `network=none network=bridge` would quietly
    # run on bridge. Every other ambiguity here is refused; this one was not.
    directives = {}
    for line in re.findall(r"^# ske-inttest:\s*(.+)$", head, re.M):
        for kv in line.split():
            if "=" not in kv:
                continue
            k, v = kv.split("=", 1)
            if k in directives:
                sys.exit("inttest-matrix: %r: directive %r given twice "
                         "(%r, then %r)" % (path, k, directives[k], v))
            directives[k] = v
    for k, v in sorted(directives.items()):
        if k not in KNOWN:
            sys.exit("inttest-matrix: %r: unknown directive %r (known: %s)"
                     % (path, k, ", ".join(KNOWN)))
        # A directive that is present but empty (`flaky=`) is always a mistake,
        # and downstream it is indistinguishable from the directive being absent
        # — so it would silently mean "not quarantined" rather than being
        # reported. Refuse it here, where the two ARE distinguishable.
        if not v:
            sys.exit("inttest-matrix: %r: %r= has no value" % (path, k))
        if not SAFE_VALUE.match(v):
            sys.exit("inttest-matrix: %r: %r=%r has characters outside "
                     "[A-Za-z0-9._-]" % (path, k, v))
    return directives


# What a directive resolves to when the header does not carry it. `network`
# is the only one with a meaningful default; the rest are absent-means-empty,
# which is what both the matrix and ci.yml already treat them as.
DEFAULTS = {"network": "bridge", "topology": "single"}

# Callers asking for one check's directive: tools/inttest-run.sh for network,
# topology and previous, ci.yml for previous,
# tools/test-inttest-header.sh for flaky. Same parse, same refusals, so a
# header CI rejects is one a local run rejects.
if sys.argv[1:2] == ["--directive"]:
    if len(sys.argv) != 4:
        sys.exit("inttest-matrix: usage: inttest-matrix.sh --directive <key> <check.sh>")
    key = sys.argv[2]
    if key not in KNOWN:
        sys.exit("inttest-matrix: unknown directive %r (known: %s)"
                 % (key, ", ".join(KNOWN)))
    print(directives_of(sys.argv[3]).get(key, DEFAULTS.get(key, "")))
    raise SystemExit(0)
if sys.argv[1:]:
    sys.exit("inttest-matrix: unknown argument %r" % sys.argv[1])

jobs = []
for path in sorted(glob.glob("tools/inttest/check-*.sh")):
    directives = directives_of(path)
    name = os.path.basename(path)[len("check-"):-len(".sh")]
    jobs.append({
        "name": name,
        "script": path,
        "network": directives.get("network", "bridge"),
        "pending": directives.get("pending", ""),
    })

# Pending entries are emitted so they are visible as skipped jobs rather than
# silently absent. A test nobody can see is not a plan to write it.
print(json.dumps({"include": jobs}, separators=(",", ":")))
PY
