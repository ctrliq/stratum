#!/usr/bin/env bash
# Asserts tools/lint-docs-paths.sh passes a README whose code spans and link
# targets name only paths that resolve, and fails one naming a path that does
# not — naming each offender at its line and no valid path beside it.
#
# Each extraction rule is pinned by a case only that rule decides: every
# not-a-path filter, the fence rules (a span inside a block is not read, a span
# after one still is), code spans that wrap or use a longer backtick string,
# which root a span resolves against, which link targets are read, and the
# gitignore skip, scoped to the directory of the .gitignore it came from. The
# lint's own guards are pinned too: a missing file argument, the no-argument
# default, and the refusal to pass a README in which it found nothing.
#
# Hermetic: every case runs a copy of the real script against a fixture README
# inside a throwaway tree, so the repo's own README is never what is asserted
# on.
# shellcheck disable=SC2016  # every fixture README below is Markdown: its
# backtick code spans are the literal thing under test and must not expand
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
fail=0

TREE=$WORK/tree
mkdir -p "$TREE/tools" "$TREE/build" "$TREE/docs/overrides" \
  "$TREE/docs/docs/assets" "$TREE/docs/docs/appendices"
cp tools/lint-docs-paths.sh "$TREE/tools/"
printf 'mkdocs_version = 1.6.0\n' > "$TREE/build/versions.mk"
printf '# fixture\n' > "$TREE/docs/mkdocs.yml"
printf '<!-- fixture -->\n' > "$TREE/docs/overrides/main.html"
printf '/* fixture */\n' > "$TREE/docs/docs/assets/copy-commands.js"
printf '/* fixture */\n' > "$TREE/docs/docs/assets/extra.css"
printf '# Glossary\n' > "$TREE/docs/docs/appendices/glossary.md"
# Beside the tree, not in it: a link that climbs out of the repository must
# fail even when something happens to exist where it lands.
printf 'outside\n' > "$WORK/outside.md"
# What the README documents as generated: gitignored, so absent from a fresh
# tree, and their absence is not a failure.
printf 'site/\n.venv/\n' > "$TREE/docs/.gitignore"

# description, want_exit, readme_body, [output must contain ...]. An expected
# string starting with ! must NOT appear in the output.
expect() {
  local what=$1 want=$2 body=$3
  shift 3
  printf '%s\n' "$body" > "$TREE/docs/README.md"
  run "$what" "$want" docs/README.md -- "$@"
}

# description, want_exit, lint arguments ..., --, [output must contain ...].
run() {
  local what=$1 want=$2 got=0 g ok=1 args=()
  shift 2
  while [[ $1 != -- ]]; do args+=("$1"); shift; done
  shift
  # Bash 3.2 treats an empty array as unset under set -u.
  (cd "$TREE" && tools/lint-docs-paths.sh ${args[@]+"${args[@]}"}) >"$WORK/out" 2>&1 || got=$?
  [[ $got -eq $want ]] || ok=0
  for g in "$@"; do
    if [[ $g == '!'* ]]; then
      if grep -qF -- "${g#!}" "$WORK/out"; then ok=0; fi
    else
      grep -qF -- "$g" "$WORK/out" || ok=0
    fi
  done
  if [[ $ok -eq 1 ]]; then
    printf '  ok    %s\n' "$what"
  else
    printf '  FAIL  %s: want exit %s, got %s\n' "$what" "$want" "$got" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  fi
}

expect "a README naming only real paths passes" 0 'The build pins its versions in `build/versions.mk`, reads `docs/mkdocs.yml`
and loads the stylesheet `docs/docs/assets/extra.css` out of `docs/docs/assets/`.' 'checked 4 path(s)'

expect "a README naming nonexistent paths fails, naming each at its line" 1 'Versions are pinned in `build/versions.mk`; the extra guides live in
`docs/nowhere.md` and the pins table in `build/missing.mk`.' \
  'docs/README.md:2: `docs/nowhere.md`' 'docs/README.md:2: `build/missing.mk`' '!`build/versions.mk`'

expect "a README naming a nonexistent directory fails, naming it" 1 'The appendix tables live in `docs/gone/`.' 'docs/gone'

# The layout this lint was written for: the content moved under docs/docs/
# and the README kept naming docs/assets/. That the file exists one level
# down must not rescue the stale path.
expect "a root path that moved under the content root fails" 1 'The palette lives in `docs/assets/extra.css`.' \
  'docs/README.md:1: `docs/assets/extra.css`'

expect "a span whose first component is not at the root resolves against the README's directory" 0 'Theme overrides live in `overrides/main.html` (or `./overrides/main.html`).' \
  'checked 2 path(s)'

expect "a span naming a file only under the content root fails" 1 'The palette lives in `assets/extra.css`.' \
  '`assets/extra.css` names no file or directory (docs/assets/extra.css)'

expect "a span relative to an unnamed page is not checked" 0 'Link up with `../cluster-admin/hardening.md`; the pins live in `./build/versions.mk`.' \
  'checked 1 path(s)'

expect "the gitignored build outputs the README documents as generated are not failures" 0 'The build writes `docs/site/` and keeps its virtualenv at `docs/.venv`;
both are gitignored. The pins live in `build/versions.mk`.' 'skipped 2'

expect "the bare gitignored names the README uses in prose are not failures" 0 '`site/` and `.venv/` are gitignored; do not commit either. The pins live in
`build/versions.mk`.' 'skipped 2'

expect "spans that are commands, flags, filenames or URL shapes are not paths" 0 'Run `make docs` and `mkdocs build --strict`; the binary documents its flags
as `stratum <command> --help`. The theme maps `--md-primary-*`, `site_url` only
feeds `sitemap.xml`, and page order is the `nav:` block in `mkdocs.yml`. A
directory-style link like `](../cluster-admin/hardening/)` renders. Published
URLs look like `/<section>/`. The pins live in `build/versions.mk`.' 'checked 1 path(s)'

# One span per rule, each excluded by that rule alone, so dropping any one
# rule turns its span into a checked path that fails.
expect "each not-a-path rule alone keeps its span out" 0 'Run `ls docs/nowhere/`, pass `--config=docs/nowhere.yml`, open `/cluster-admin/nowhere/`,
see `docs/<nowhere`, `docs/nowhere>`, `docs/(nowhere`, `docs/nowhere)`, `build/nowhere.mk:12`,
`docs/nowhere.md#install` and `docs/docs/*.md`. The pins live in `build/versions.mk`.' \
  'checked 1 path(s)'

expect "a span inside a closed fenced block is not checked" 0 '```text
cat `build/nowhere.mk`
```
~~~
`docs/gone/`
~~~
The pins live in `build/versions.mk`.' 'checked 1 path(s)'

expect "a span after a fenced block is still checked, at its line" 1 '```bash
make docs          # build once into docs/site/
make docs-serve    # live preview on http://localhost:8000/
```
Both create a virtualenv at `docs/.venv` and install the versions pinned
in `build/nowhere.mk`.' 'docs/README.md:6: `build/nowhere.mk`'

expect "a fence of the other marker inside a block does not close it" 1 '```text
~~~
```
The stale pins table lives in `build/nowhere.mk`.

```text
ordinary output
```
The pins live in `build/versions.mk`.' 'docs/README.md:4: `build/nowhere.mk`'

expect "a fence line with an info string inside a block does not close it" 1 '```text
```bash
```
The stale pins table lives in `build/nowhere.mk`.

```text
ordinary output
```
The pins live in `build/versions.mk`.' 'docs/README.md:4: `build/nowhere.mk`'

expect "a list-item fence line inside a block does not close it" 1 '```text
- ```
```
The stale pins table lives in `build/nowhere.mk`.

```text
ordinary output
```
The pins live in `build/versions.mk`.' 'docs/README.md:4: `build/nowhere.mk`'

expect "a shorter fence inside a block does not close it" 1 '````text
```
````
The stale pins table lives in `build/nowhere.mk`.

```text
ordinary output
```
The pins live in `build/versions.mk`.' 'docs/README.md:4: `build/nowhere.mk`'

expect "a line opening with a triple-backtick code span opens no fence" 1 '```make docs``` builds from `build/versions.mk`.
The stale pins table lives in `build/nowhere.mk`.

```text
output
```' 'docs/README.md:2: `build/nowhere.mk`'

expect "a fence opened on a list-marker line hides its spans" 0 '- ```bash
  cat `build/nowhere.mk`
  ```

The pins live in `build/versions.mk`.' 'checked 1 path(s)'

expect "a fence opened on a list-marker line closes, and the prose after it is checked" 1 'The pins live in `build/versions.mk`.

- ```bash
  make docs
  ```

The stale pins table lives in `build/nowhere.mk`.

```text
ordinary output
```' 'docs/README.md:7: `build/nowhere.mk`'

expect "a fence indented under a list item hides its spans" 0 '1. Run:

    ~~~bash
    cat `build/nowhere.mk`
    ~~~

The pins live in `build/versions.mk`.' 'checked 1 path(s)'

# A fence the author forgot to close: honouring it would silently skip every
# span after it, so it is rescinded and the tail scanned as prose.
expect "a fence left unterminated at end-of-file does not swallow the spans after it" 1 'The pins live in `build/versions.mk`.

```bash
make docs          # build once into docs/site/
The stale pins table lives in `build/nowhere.mk`.' 'build/nowhere.mk'

expect "a code span wrapped across lines does not hide the span after it" 1 'The pins live in `build/versions.mk`.
Run `stratum
<command> --help`, then read `build/nowhere.mk`.' 'docs/README.md:3: `build/nowhere.mk`'

expect "a double-backtick code span does not hide the span after it" 1 'Write ``a `b` c`` and keep `build/nowhere.mk` current.' \
  'docs/README.md:1: `build/nowhere.mk`'

expect "a padded double-backtick span is read without its padding" 1 'Keep `` build/nowhere.mk `` current.' \
  'docs/README.md:1: `build/nowhere.mk`'

expect "an escaped backtick does not hide a later stale path" 1 'Use a literal \` in prose; see `build/nowhere.mk`.

The pins live in `build/versions.mk`.' 'docs/README.md:1: `build/nowhere.mk`'

expect "an even number of backslashes still permits a code span" 1 'Read \\`build/nowhere.mk`.

The pins live in `build/versions.mk`.' 'docs/README.md:1: `build/nowhere.mk`'

expect "a backslash inside a code span does not escape its closer" 1 'Write `a\` and read `build/nowhere.mk`.

The pins live in `build/versions.mk`.' 'docs/README.md:1: `build/nowhere.mk`'

expect "an escaped first backtick can precede an opening run" 1 'Read \```build/nowhere.mk``.

The pins live in `build/versions.mk`.' 'docs/README.md:1: `build/nowhere.mk`'

expect "a link target resolves against the README's directory" 0 'Terms go in the [Glossary](docs/appendices/glossary.md#terms).' \
  'checked 1 path(s)'

expect "a link target starting with / resolves from the repository root" 0 'The pins live in [versions.mk](/build/versions.mk).' \
  'checked 1 path(s)'

expect "a link target naming no file fails, naming it at its line" 1 'The pins live in `build/versions.mk`.
Terms go in the [Glossary](docs/appendices/nowhere.md#terms).' \
  'docs/README.md:2: link target `docs/appendices/nowhere.md`' '!`build/versions.mk`'

expect "a missing destination with balanced parentheses is checked" 1 'The pins live in `build/versions.mk`.
Read [the guide](missing(v2).md).' 'docs/README.md:2: link target `missing(v2).md`'

expect "a missing angle-bracket destination with parentheses is checked" 1 'The pins live in `build/versions.mk`.
Read [the guide](<missing(v2).md>).' 'docs/README.md:2: link target `missing(v2).md`'

expect "escaped parentheses in a missing destination are unescaped" 1 'The pins live in `build/versions.mk`.
Read [the guide](missing\(v2\).md).' 'docs/README.md:2: link target `missing(v2).md`'

printf '# Guide\n' > "$TREE/docs/guide(v2(extra)).md"
expect "nested parentheses and an optional link title resolve" 0 'Read [the guide](guide(v2(extra)).md "Guide title").' 'checked 1 path(s)'
expect "escaped parentheses in an existing destination resolve" 0 'Read [the guide](guide\(v2\(extra\)\).md).' 'checked 1 path(s)'
expect "angle-bracket destinations with parentheses resolve" 0 'Read [the guide](<guide(v2(extra)).md> "Guide title").' 'checked 1 path(s)'

printf '# Guide\n' > "$TREE/docs/guide.md"
printf '# Guide\n' > "$TREE/docs/space name.md"
expect "an unmatched backtick in a link title cannot hide a later path" 1 'Read [the guide](guide.md "literal ` character") then `build/nowhere.mk`.

The pins live in `build/versions.mk`.' 'docs/README.md:1: `build/nowhere.mk`' 'checked 3 path(s)'
expect "backticks in a link title do not name code paths" 0 'Read [the guide](guide.md "See `build/nowhere.mk`").' \
  'checked 1 path(s)' '!build/nowhere.mk'
expect "code paths in link labels are still checked" 1 'Read [`build/nowhere.mk`](guide.md "See `build/example.mk`").' \
  'checked 2 path(s)' '`build/nowhere.mk` names no file' '!build/example.mk'
expect "existing code paths in link labels resolve" 0 'Read [`build/versions.mk`](guide.md "literal ` character").' \
  'checked 2 path(s)'
printf '# Guide\n' > "$TREE/docs/"'guide`file.md'
expect "a backtick in a link destination cannot hide a later path" 1 'Read [the guide](guide`file.md) then `build/nowhere.mk`.

The pins live in `build/versions.mk`.' 'docs/README.md:1: `build/nowhere.mk`' 'checked 3 path(s)'
expect "example links with backticks inside code are not destinations" 0 'Write ``[guide](missing.md "literal ` character")``.

The pins live in `build/versions.mk`.' 'checked 1 path(s)' '!missing.md'
expect "a local link query and fragment are removed before checking" 0 'Read [the guide](guide.md?plain=1#install).' 'checked 1 path(s)'
expect "link-looking text inside a title is not a destination" 0 'Read [the guide](guide.md "See [the guide](missing.md)").' \
  'checked 1 path(s)' '!missing.md'
expect "a link after a title is still checked" 1 'Read [the guide](guide.md "See [the guide](example.md)"), then [the notes](missing.md).' \
  'checked 2 path(s)' 'link target `missing.md`' '!link target `example.md`'
expect "a percent-encoded local filename resolves" 0 'Read [the guide](space%20name.md).' 'checked 1 path(s)'
expect "an angle-bracket destination may contain spaces" 0 'Read [the guide](<space name.md>).' 'checked 1 path(s)'
expect "a protocol-relative URL is not a filesystem path" 0 'The pins live in `build/versions.mk`.
Read [the guide](//example.com/help).' 'checked 1 path(s)'
expect "a missing destination with a query still fails" 1 'The pins live in `build/versions.mk`.
Read [the guide](missing.md?plain=1#install).' 'directory (docs/missing.md)'
expect "a missing percent-encoded destination still fails" 1 'The pins live in `build/versions.mk`.
Read [the guide](missing%20guide.md).' 'directory (docs/missing guide.md)'
expect "a percent-encoded traversal out of the repository fails" 1 'Read [the notes](%2e%2e/%2e%2e/outside.md).' \
  'directory (../outside.md)'

expect "a link target that climbs out of the repository fails" 1 'See [the notes](../../outside.md).' \
  'docs/README.md:1: link target `../../outside.md`'

expect "URLs, anchors and links inside code are not link targets" 0 'Built with [MkDocs](https://www.mkdocs.org/), mail [us](mailto:docs@example.com),
jump [below](#install), and write `[Hardening](../nowhere.md)` by file path.

```markdown
[Hardening](../nowhere.md)
```
The pins live in `build/versions.mk`.' 'checked 1 path(s)'

expect "a README with no path in it fails rather than passing vacuously" 1 'Run `make docs` to build the site.' \
  'refusing to pass vacuously'

# The gitignore skip, scoped the way git scopes it. Each case writes its own
# ignore files and the fixture's are restored after.
printf '/dist/\n!/build/\ndocs/generated/\n' > "$TREE/.gitignore"
expect "a root-anchored gitignore entry skips the path it names" 0 'The release binary lands in `dist/stratum`.' 'skipped 1'
expect "a gitignore entry with an inner slash skips the path it names" 0 'Generated pages land in `docs/generated/out.html`.' 'skipped 1'
expect "a gitignore negation is not read as an ignore" 1 'The stale pins table lives in `build/nowhere.mk`.' \
  'docs/README.md:1: `build/nowhere.mk`'
rm -f "$TREE/.gitignore"

printf '/site/\n/.venv/\n' > "$TREE/docs/.gitignore"
expect "an anchored entry in a nested gitignore skips the path under that directory" 0 'The build writes `docs/site/index.html` and keeps its virtualenv at `docs/.venv`.' \
  'skipped 2'
printf 'site/\n.venv/\n' > "$TREE/docs/.gitignore"

expect "a nested gitignore entry does not skip a path outside its directory" 1 'The stale table lives in `build/site/nowhere.mk`.' \
  'docs/README.md:1: `build/site/nowhere.mk`'

printf 'The pins live in `build/versions.mk`.\n' > "$TREE/docs/README.md"
printf 'The stale table lives in `build/nowhere.mk`.\n' > "$TREE/docs/OTHER.md"
run "each file argument is checked" 1 docs/README.md docs/OTHER.md -- \
  'docs/OTHER.md:1: `build/nowhere.mk`' 'across 2 file(s)'
rm -f "$TREE/docs/OTHER.md"

# The not-a-file branch, beside a README that passes: the refusal must be
# the missing file's own failure, not the vacuous-pass guard's.
run "an argument naming a file that is not there fails, saying so" 1 docs/absent.md docs/README.md -- \
  'docs/absent.md: not a file' '!refusing to pass vacuously'

# CI runs the lint with no arguments; the default is the docs README.
printf 'The stale pins table lives in `build/nowhere.mk`.\n' > "$TREE/docs/README.md"
run "with no arguments the docs README is checked" 1 -- 'docs/README.md:1: `build/nowhere.mk`'

if [[ $fail -ne 0 ]]; then
  echo "test-lint-docs-paths: FAILED" >&2
  exit 1
fi
echo "test-lint-docs-paths: ok"
