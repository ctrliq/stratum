#!/usr/bin/env bash
# Asserts that every repository path docs/README.md names — in a backtick code
# span or as a Markdown link target — resolves to a real file or directory.
# Usage: tools/lint-docs-paths.sh [file ...]   (default: docs/README.md)
#
# This exists because documentation paths drift in one direction: the tree moves
# — the site's content gained its own root at docs/docs/ and the assets moved
# under it — and the README that maps the tree for a reader kept naming the
# old layout. The reader copies a path that 404s. Nothing else in CI reads the
# README's paths, so nothing else notices.
#
# The extraction is deliberately narrow, because a lint that false-positives is
# a lint that gets disabled. Only prose is read, never a fenced block — a fence
# holds commands or output, not prose naming a file. Code spans are paired the
# way CommonMark pairs them, within a paragraph, so a span that wraps a line or
# uses a longer backtick string cannot shift the spans after it. A span names
# a path only when it is shaped like nothing else a README legitimately holds:
#   - it contains a `/`: a bare filename (`mkdocs.yml`) names no location
#   - it carries no whitespace, so commands with their arguments (`make docs`,
#     `stratum <command> --help`) are never paths
#   - it starts with neither `-` (a flag) nor `/` (a published URL is absolute)
#   - it holds none of `< > ( ) : # *`: placeholders, markdown link tails,
#     anchors and globs each name no single path
#   - it has no `..` component: that climbs from a page the span does not name
# A span whose first component exists at the repository root (`build/
# versions.mk`, `docs/docs/assets/`) is checked from the root, which is how the
# README names the tree. Any other span is read relative to the README's own
# directory, the way the bare `site/` names docs/site/.
#
# Inline link targets (`[text](target)`) are checked relative to the README's
# directory, which is how GitHub renders them, or from the root for one that
# starts with `/`. URLs, mail links and bare `#anchors` name no file. Queries
# and fragments are dropped, and URL escapes decoded, before the check.
# Balanced parentheses, Markdown escapes and <angle brackets> are supported.
# A target that climbs out of the repository fails even if something exists
# where it lands. Reference-style
# definitions (`[id]: target`) are not read; the README uses none.
#
# Gitignored build outputs are skipped, not checked: `docs/site/` and
# `docs/.venv` are what `make docs` generates, and they are absent from a fresh
# checkout. Each .gitignore from the root down to the path applies to the paths
# under its own directory, as git scopes it: a slash-free pattern matches any
# component below that directory, and one with a slash matches from it.
# Negations are dropped, so a re-admitted path is skipped too: the skip can only
# silence a check, and only for paths git itself would ignore.
#
# Verified by mutation, not by observing it pass: tools/test-lint-docs-paths.sh
# drives a copy of this script against fixture READMEs in a throwaway tree and
# asserts each failure by name. A lint nobody has watched fail is a lint nobody
# knows is wired up.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

files="$*"
if [[ -z $files ]]; then
  files="docs/README.md"
fi

# shellcheck disable=SC2086  # $files is a space-separated path list; splitting it is the point
python3 - $files <<'PY'
import fnmatch, os, pathlib, re, string, sys
from urllib.parse import unquote, urlsplit

files = sys.argv[1:]

# A fence opens at any indent — one nested in a list item or an admonition is
# indented — and may follow a list marker on its own line.
FENCE = re.compile(r"^(\s*(?:(?:[-+*]|\d{1,9}[.)])\s+)?)(`{3,}|~{3,})(.*)$")
BACKTICKS = re.compile(r"`+")
LINK_START = re.compile(r"\]\(\s*")
LINK_END = re.compile(r'''(?:\s+(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|\((?:\\.|[^)\\])*\)))?\s*\)''')
MARKDOWN_ESCAPE = re.compile(r"\\([" + re.escape(string.punctuation) + r"])")
NOT_A_PATH = re.compile(r"[\s<>():#*]")
SCHEME = re.compile(r"[A-Za-z][A-Za-z0-9+.-]*:")

def prose(lines):
    """(line_number, line) for every line outside a fenced block.

    A block closes only with the opener's marker, at least its length, with no
    list marker and nothing after it. Other fence-shaped lines are literal
    block content; treating them as closers would hide paths in later prose.

    A fence still open at end-of-file never opened: its closer is missing
    (an authoring mistake), and honouring the dangling opener would
    silently skip every span after it — the silent direction. The opener is
    rescinded and its tail scanned as prose, so a stale path there still
    fails named.
    """
    out, pending, opener = [], [], None
    for n, line in enumerate(lines, 1):
        fence = FENCE.match(line)
        if opener is not None:
            if (fence and not fence.group(1).strip()
                    and fence.group(2)[0] == opener[0]
                    and len(fence.group(2)) >= len(opener)
                    and not fence.group(3).strip()):
                opener, pending = None, []
            elif not fence:
                pending.append((n, line))
            continue
        # A backtick fence's info string cannot hold a backtick: a line like
        # ```make docs``` is a code span, not an opener.
        if fence and not (fence.group(2)[0] == "`" and "`" in fence.group(3)):
            opener = fence.group(2)
            continue
        out.append((n, line))
    return out + pending

def paragraphs(numbered):
    """(first_line_number, text) for each run of consecutive non-blank lines."""
    run = []
    for n, line in numbered + [(None, "")]:
        if line.strip() and run and n == run[-1][0] + 1:
            run.append((n, line))
            continue
        if run:
            yield run[0][0], "\n".join(l for _, l in run)
        run = [(n, line)] if line.strip() else []

def code_spans(text, pos=0):
    """(start, end, content) for paired backtick runs within a paragraph."""
    while True:
        opener = BACKTICKS.search(text, pos)
        if opener is None:
            return
        start, end = opener.span()
        before = start
        while before > 0 and text[before - 1] == "\\":
            before -= 1
        # Only prose escapes backticks. Inside a span, even a backtick after
        # a backslash closes it. An escaped tick can precede an opening run.
        if (start - before) % 2:
            start += 1
        pos = end
        if start == end:
            continue
        for closer in BACKTICKS.finditer(text, end):
            if closer.end() - closer.start() == end - start:
                yield start, closer.end(), text[end:closer.start()]
                pos = closer.end()
                break

def link_targets(text, pos=0):
    """(opener_offset, target_offset, end, destination) for inline links."""
    consumed = pos
    for opener in LINK_START.finditer(text, pos):
        if opener.start() < consumed:
            continue
        start = pos = opener.end()
        angle = pos < len(text) and text[pos] == "<"
        if angle:
            start = pos = pos + 1
        depth = 0
        while pos < len(text):
            char = text[pos]
            if (char == "\\" and pos + 1 < len(text)
                    and text[pos + 1] in string.punctuation):
                pos += 2
                continue
            if angle:
                if char in "<>\n":
                    break
            else:
                if char.isspace() or char in "<>":
                    break
                if char == "(":
                    depth += 1
                elif char == ")":
                    if depth == 0:
                        break
                    depth -= 1
            pos += 1
        if angle:
            if pos == len(text) or text[pos] != ">":
                continue
            end = pos + 1
        else:
            if depth or pos == start:
                continue
            end = pos
        ending = LINK_END.match(text, end)
        if ending:
            consumed = ending.end()
            yield opener.start(), start, ending.end(), MARKDOWN_ESCAPE.sub(r"\1", text[start:pos])

def references(lines):
    """(line_number, kind, text) for each code span and link target in prose."""
    refs = []
    for first, text in paragraphs(prose(lines)):
        def line_of(i):
            return first + text.count("\n", 0, i)
        # Consume whichever construct starts first. A code span hides example
        # links inside it; a link destination and title hide literal backticks.
        # Labels remain prose, so code spans inside a label are still checked.
        pos = 0
        while pos < len(text):
            span = next(code_spans(text, pos), None)
            link = next(link_targets(text, pos), None)
            if link is not None and (span is None or link[0] < span[0]):
                _, start, pos, target = link
                refs.append((line_of(start), "link", target))
            elif span is not None:
                start, pos, content = span
                if content.startswith(" ") and content.endswith(" ") and content.strip():
                    content = content[1:-1]
                refs.append((line_of(start), "span", content))
            else:
                break
    return sorted(refs, key=lambda r: r[0])

_patterns = {}
def gitignore(d):
    """(pattern, anchored) from d/.gitignore, negations dropped.

    Read by hand rather than through `git check-ignore` so the check is
    hermetic: it behaves the same in a checkout, a worktree and the throwaway
    tree the self-test builds, none of which need be a git repository.
    """
    if d not in _patterns:
        pats, gi = [], pathlib.Path(d, ".gitignore")
        if gi.is_file():
            for line in gi.read_text(encoding="utf-8", errors="replace").splitlines():
                line = line.strip()
                if not line or line.startswith(("#", "!")):
                    continue
                pat = line.strip("/")
                pats.append((pat, line.startswith("/") or "/" in pat))
        _patterns[d] = pats
    return _patterns[d]

def ignored(path):
    comps = path.split("/")
    for i in range(len(comps)):
        rel = comps[i:]
        for pat, anchored in gitignore("/".join(comps[:i]) or "."):
            if anchored:
                if any(fnmatch.fnmatchcase("/".join(rel[:j]), pat)
                       for j in range(1, len(rel) + 1)):
                    return True
            elif any(fnmatch.fnmatchcase(c, pat) for c in rel):
                return True
    return False

def span_candidates(span, here):
    """The paths a span may name, root reading first; None if it names none."""
    if "/" not in span or span[0] in "-/" or NOT_A_PATH.search(span):
        return None
    comps = [c for c in span.split("/") if c not in ("", ".")]
    if not comps or ".." in comps:
        return None
    norm = "/".join(comps)
    if pathlib.Path(comps[0]).exists():
        return [norm]
    # Absent at the root: the README's directory, or a gitignored root output
    # a fresh checkout does not have.
    return [norm, os.path.normpath(os.path.join(here, norm))]

def link_candidates(target, here):
    if SCHEME.match(target) or target.startswith("//"):
        return None
    target = unquote(urlsplit(target).path)
    if not target:
        return None
    if target.startswith("/"):
        return [os.path.normpath(target.lstrip("/"))]
    return [os.path.normpath(os.path.join(here, target))]

fail = 0
checked = skipped = 0
for fname in files:
    path = pathlib.Path(fname)
    if not path.is_file():
        print(f"lint-docs-paths: {fname}: not a file — a check of the paths a "
              "README names cannot pass on a README that is not there",
              file=sys.stderr)
        fail = 1
        continue
    here = path.parent.as_posix()
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    for n, kind, ref in references(lines):
        if kind == "span":
            cands = span_candidates(ref, here)
        else:
            cands = link_candidates(ref, here)
        if cands is None:
            continue
        if any(ignored(c) for c in cands if not c.startswith("..")):
            skipped += 1
            continue
        checked += 1
        resolved = cands[-1]
        if resolved.startswith("..") or not pathlib.Path(resolved).exists():
            label = f"`{ref}`" if kind == "span" else f"link target `{ref.split('#')[0]}`"
            print(f"lint-docs-paths: {fname}:{n}: {label} names no file or "
                  f"directory ({resolved})")
            fail = 1

if checked + skipped == 0:
    print(f"lint-docs-paths: no path found in {', '.join(files)} — the README "
          "names its paths in backticks and links, so the extraction is "
          "broken; refusing to pass vacuously", file=sys.stderr)
    sys.exit(1)

print(f"lint-docs-paths: checked {checked} path(s) and skipped {skipped} "
      f"gitignored one(s) across {len(files)} file(s)")
sys.exit(fail)
PY
rc=$?
[[ $rc -eq 0 ]] || { echo "lint-docs-paths: FAILED — see the lines above" >&2; exit 1; }
echo "lint-docs-paths: ok"
