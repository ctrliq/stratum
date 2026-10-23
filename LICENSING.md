# Licensing

Third-party code that stratum ships may carry only an allowed license, and
`make licenses` checks that it does. This file holds the policy, how to run
the check and read what it prints, how an exception is granted, and the
register of every unsupported license found so far with what was done about
it. [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) separately lists the
third-party files checked into this repository and their notices.

## Policy

### What the policy covers

The policy covers **code we ship**: the source built and packaged as the
`stratum` binary. That is two sets of third-party code:

- The Go packages linked into `./cmd/stratum` as `make build` compiles it:
  cgo off, `GOOS=linux`, `GOARCH` amd64 and arm64.
- The npm packages Vite bundles into `dash/dist`, the dashboard the binary
  embeds.

The policy does not cover internal tooling (everything under `tools/`), npm
packages that only run the build and are not bundled (such as vite and
lightningcss), or the runtime environment stratum runs on and stages: the
payload binaries (runc, containerd, etcd, kubernetes) and the container images
in `build/images.txt`.
First-party code is not checked either, and for Go that means the main
module only: a module whose `replace` target is a directory inside this
repository is still checked, since it is someone else's code linked into the
binary. The dashboard's own source under
`dash/src`, including the vendored Inter font, is first-party to the check
(`dash/bundled-packages.js:18-28`), so vendored files are tracked in
THIRD_PARTY_NOTICES.md instead.

### Allowed licenses

The allowed licenses are MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC,
0BSD, Zlib and BSL-1.0, written as SPDX ids (`tools/licensecheck/policy.go:15`)
and compared case-insensitively (`canonical`, `policy.go:58-63`). OFL-1.1 is allowed only as a named
exception for the vendored Inter font (see [the register](#inter-font-4001)).

### What fails, what warns

Any well-formed license id outside the allowlist fails the check, even one
the SPDX license list does not define, such as `BSD`, `LicenseRef-Custom` or
`UNLICENSED`. An unknown or missing license prints a `WARN` line and does not
fail. Unknown means the check cannot read a license id at all: a license file
whose text matches no known license, or matches one only loosely (a changed
license text), or an npm license field that does not parse as an SPDX
expression. A license outside the allowlist that a recorded exception covers
passes, and the check prints a `NOTE` line naming the decision
(`tools/licensecheck/policy.go:34-57`).

When a package declares an SPDX license expression, `OR` is allowed when any
alternative is, and `AND` only when every term is. Several license files for
one package count as `AND`: every license found applies. `X WITH exception`
counts as license `X`, because a license exception only adds permissions. The
exception must be an id from the SPDX License List's exceptions (3.29.0,
`tools/licensecheck/spdxexceptions.go`) or an `AdditionRef-`; anything else
after `WITH`, such as a license id in `MIT WITH MPL-2.0`, makes the expression
one the check cannot read, which warns.

### How a package's license is decided

The check classifies each package, not each module, because one module can
mix licenses (`tools/licensecheck/gomode.go:261-339`):

1. When every compiled file of a Go package (its Go and assembly files, and
   the `.h` files assembly can include) opens with an
   `SPDX-License-Identifier` header that parses as a license expression and
   is not `NONE` or `NOASSERTION`, those headers are the package's license
   (`tools/licensecheck/gomode.go:568-573`). A header is every such tag in the
   comments before the file's first code, and several tags combine with AND.
   A prebuilt `.syso` object, or a file embedded with `//go:embed`, counts as
   a file without one. When only some files have one, the license files below
   decide, but a header outside the allowlist still fails the package, since
   it covers its own file, and a header that does not parse or says `NONE` or
   `NOASSERTION` gets a `WARN` line unless the package fails.
2. Otherwise the package's license is the nearest `LICENSE*`, `LICENCE*` or
   `COPYING*` files, walking up from the package directory to its module root
   and never above it. The check identifies each file's text offline with
   `github.com/google/licenseclassifier/v2`. A full license text counts only
   when it matches at a confidence of 0.95 or more, and a short license notice
   (such as the Apache-2.0 notice) at 0.9 or more. A notice that sits outside
   every full license text in the file counts as a license too.
3. A package gets a `WARN` line when it falls back to license files and none
   exists up to its module root, or when a license file's text matches no
   known license, matches one only below those confidence floors, or holds a
   line that no match covers and that is neither a license title (such as
   `The MIT License (MIT)`) nor a bare copyright notice: one sentence,
   optionally ending `All rights reserved.`, whose lowercase words are only
   ones like `by`, `and` and `the authors`, since such text could add terms
   to the licenses found; and no other file's license fails.

A Go package that fails to load (`go list` reports an error for it or for one
of its dependencies) stops the check with exit status 2. It is never skipped,
because a dropped package would pass silently
(`tools/licensecheck/gomode.go:195-205`).

An npm package's license is the SPDX expression in its `package.json`
`license` field. An empty field, `NONE`, `NOASSERTION`, a value that does not
parse as an SPDX expression (such as `SEE LICENSE IN LICENSE.md`,
`Apache 2.0`, or `MIT OR NOASSERTION`, since `NONE` and `NOASSERTION` only
stand alone), or the deprecated object and array forms all get a `WARN` line
(`tools/licensecheck/npm.go:62-72`, `dash/bundled-packages.js:46-48`). A value
that parses fails when it needs an id outside the allowlist, whether or not
SPDX defines that id: `BSD` and `UNLICENSED` both fail.

### The pathrs-lite guard

The check fails if stratum ever links the MPL-2.0 part of
`github.com/cyphar/filepath-securejoin`, whether stratum or a dependency such
as Helm starts importing it. That module's `COPYING.md` declares
`BSD-3-Clause AND MPL-2.0` and says per-file comments take precedence. Its
root package and `internal/consts`, the two packages stratum links today, are
headed BSD-3-Clause and pass. Its `pathrs-lite` and `pathrs-lite/procfs`
packages, the two that other modules can import, are headed MPL-2.0, so
linking either fails. Two tests prove it: one against a fixture with the real
module's headers and license files
(`tools/licensecheck/securejoin_test.go:21-71`), and one against the
published v0.7.0 module for both architectures
(`tools/licensecheck/securejoin_test.go:95-129`).

## Running the check

Run the full check from the repository root:

```
make licenses
```

`make licenses` first builds the dashboard (`make dash-bundle`: `pnpm install`
then `pnpm run build`, without the dashboard's tests, which its own CI job
runs; `Makefile:223`). The dashboard build writes
`dash/.bundle/packages.json`, the list of npm packages the bundle contains,
including those only a web worker or an asset such as a font brings in
(`dash/bundled-packages.js:176-280`). An asset under Vite's 4 KiB
`build.assetsInlineLimit` ships inlined as a `data:` URL; the plugin wraps
that setting to list each such file. A build fails as it starts if the
environment it builds no longer holds the wrapper (another plugin or the
environment's own options replaced it), or if it is a library build, which
inlines every asset without asking. A reference with an `inline` query
parameter (`?inline`, or `&inline` after another) skips the setting too, so the build fails on any module that writes
one, the dashboard's or a package's. Vite inlines a stylesheet that another
stylesheet `@import`s without telling the list, so the build fails when a
stylesheet `@import`s a package's stylesheet; import it from JavaScript
instead. The target then runs `tools/lint-licenses.sh` (`Makefile:407-408`),
which checks the Go packages for linux/amd64 and linux/arm64 and then the npm
list. Both runs always execute, so one failure never hides the other's
findings (`tools/lint-licenses.sh:63-75`). The dashboard build needs Node and pnpm;
`mise install` provides them.

To check only the Go packages, which needs no Node, build without the
dashboard:

```
make licenses TAGS=noui
```

The Go run uses the build tags of the build it checks. Without `noui`, the
binary embeds `dash/dist`, so the dashboard must be built first, which
`make licenses` does. With `TAGS=noui`, it checks the binary built without a
dashboard. The `noui` tag only swaps the dashboard's embed file, whose imports
are standard library, so today both builds link the same third-party packages.

CI runs the same gate in the `licenses` job of
`.github/workflows/ci.yml:186-205`, after `tools/test-lint-licenses.sh`, the
hermetic test that proves the gate fails, warns and passes what it claims to.
`make lint` also runs that test (`Makefile:364`).

### Reading the output

The check prints one line for each package that does not simply pass: the
level, the package (a Go import path, or an npm `name@version`), and the
reason, which for a license that was found ends with where it came from in
parentheses. The first two lines below
came from scratch fixtures, and the third from this repository:

```
FAIL github.com/cyphar/filepath-securejoin/pathrs-lite: license not allowed: MPL-2.0 (SPDX headers MPL-2.0)
WARN nolic@1.0.0: no license declared (license "")
NOTE github.com/opencontainers/go-digest: license allowed only by exception "Exception: docs license": CC-BY-SA-4.0 (license files LICENSE (Apache-2.0), LICENSE.docs (CC-BY-SA-4.0))
```

| Level | Meaning | Gate |
|---|---|---|
| `FAIL` | A license id outside the allowlist | Fails |
| `WARN` | An unknown or missing license | Passes |
| `NOTE` | Allowed only by a recorded exception | Passes |

A Go `WARN` line says either `no license found` or `unknown license`, and an
npm one says `no license declared` or `unknown license`. Under
`GITHUB_ACTIONS=true` the same findings print, in place of the `FAIL`, `WARN`
and `NOTE` lines, as `::error`, `::warning` and `::notice` workflow commands
titled `licensecheck`, such as
`::error title=licensecheck::<package>: <reason>`. GitHub shows them as
annotations (`tools/licensecheck/main.go:134-154`).

Each run that completes ends with a summary on stderr, such as
`licensecheck: 1147 third-party packages linked for linux/amd64, linux/arm64: 0 fail, 0 warn`.
The gate's last line is `lint-licenses: ok`, or `lint-licenses: FAILED:`
followed by the runs that failed, and then it exits 1. A run fails on a `FAIL`
line (licensecheck exit status 1) or when licensecheck cannot check at all
(exit status 2: a bad flag, unreadable input, or a package that does not load;
`tools/licensecheck/main.go:45-50`). The npm run also fails when
`dash/.bundle/packages.json` is missing, because nothing bundled would be
checked; build the dashboard (`make dash`) to write it.

### When a dependency fails

Never add an exception, and never widen the allowlist, to make the check
pass: only a human decides an exception. Instead:

1. Find what brings the package in. For a Go package,
   `go mod why <import path>` prints an import chain from stratum to it. For
   an npm package, run `pnpm why <name>` in `dash/`.
2. Bring the finding to a human with the package, its license, and that
   chain. The human chooses its disposition: remediate it (drop or replace
   the dependency), track it as a follow-up in the issue tracker, or grant
   an exception.
3. Record the decision in [the register](#disposition-register).

A new `WARN` line does not fail the check, but it still goes in the register,
so a human can review it.

## Exceptions

Only a human decision adds an exception. Each one is recorded in the register
below with the decision text and its date. When the check scans the excepted
package, the exception is also an entry in the `exceptions` map in
`tools/licensecheck/policy.go:26-30`, keyed by Go import path or npm package
name, listing the SPDX ids it allows and quoting the decision. An entry covers
only the package it names, so other packages of the same module are still
checked in full.

The map has one entry, `github.com/opencontainers/go-digest`. The other three
exceptions cover code the check does not scan (a build tool, a payload
binary, and a vendored font), so they need no map entry.

## Disposition register

Each entry records one unsupported or unclear license, found by the check or
by the dependency survey made when this policy was adopted, with exactly one
of these dispositions:

- remediated, by dropping or replacing the dependency;
- a follow-up tracked in the issue tracker;
- an exception a human decided;
- no action, because the package is not linked;
- a warning pending human review, for a `WARN` line the check printed.

The human made every decision quoted here on 2026-10-08, when this policy was
adopted.

| Component | License | Disposition |
|---|---|---|
| `github.com/hashicorp/go-multierror` v1.1.1 | MPL-2.0 | Remediated |
| `github.com/hashicorp/errwrap` v1.1.0 | MPL-2.0 | Remediated |
| `github.com/opencontainers/go-digest` v1.0.0 | CC-BY-SA-4.0 (docs) | Human exception |
| lightningcss 1.33.0 (npm) | MPL-2.0 | Human exception |
| libseccomp (static, in runc) | LGPL-2.1 | Human exception |
| `filepath-securejoin` v0.7.0 `pathrs-lite` | MPL-2.0 | No action: not linked |
| Inter font 4.001 | OFL-1.1 | Human exception |
| Quantic-derived styles | Unclear | Follow-up in the issue tracker |

### github.com/hashicorp/go-multierror v1.1.1

- **License:** MPL-2.0.
- **Where:** It was linked into `stratum` through `helm.sh/helm/v3`'s
  `pkg/kube`, which calls `multierror.Append`.
- **Disposition:** Remediated when the license check was introduced: stratum
  migrated from Helm v3 to `helm.sh/helm/v4` v4.3.0 (`go.mod:23`), which does
  not use it. The module is no longer in `go.mod`, and the check would fail it
  if it were linked again.

### github.com/hashicorp/errwrap v1.1.0

- **License:** MPL-2.0.
- **Where:** It was linked only because go-multierror imported it.
- **Disposition:** Remediated. The Helm v4 migration removed it with
  go-multierror, and the check would fail it too if it were linked again.

### github.com/opencontainers/go-digest v1.0.0

- **License:** Apache-2.0 for the code. Its `LICENSE.docs` puts the project's
  documentation under CC-BY-SA-4.0, and the check reads both files, because
  both sit in the package directory.
- **Where:** It is linked into `stratum` through Helm's `pkg/registry`, in
  Helm v3 and still in v4. No documentation is compiled in.
- **Disposition:** Exception decided by the human on 2026-10-08. Decision
  text: "Exception: docs license". The exception is the `exceptions` map entry
  in `tools/licensecheck/policy.go:29`, and the check prints a `NOTE` line for
  it.

### lightningcss 1.33.0 (npm)

- **License:** MPL-2.0.
- **Where:** It is a dependency of Vite (`dash/pnpm-lock.yaml:703-705`),
  which uses it to minify CSS while the dashboard builds. Its code is not in
  `dash/dist`.
- **Disposition:** Exception decided by the human on 2026-10-08. Decision
  text: "Exception: build-only". The check never sees it, because it is not
  in the bundle's package list.

### libseccomp (static)

- **License:** LGPL-2.1.
- **Where:** It is statically linked into the runc payload binary: the runc
  component build installs `libseccomp-static` and runs runc's `make static`
  (`build/components/runc/Dockerfile:15`,
  `build/components/runc/Dockerfile:50`).
- **Disposition:** Exception decided by the human on 2026-10-08. Decision
  text: "Exception: runtime env". The check does not scan payload binaries.

### filepath-securejoin v0.7.0, pathrs-lite

- **License:** MPL-2.0.
- **Where:** It is not linked. Only the module's BSD-3-Clause root package and
  `internal/consts` are.
- **Disposition:** No action. No MPL-2.0 package of the module is linked into
  the `stratum` binary, and [the pathrs-lite guard](#the-pathrs-lite-guard)
  fails the check if one ever is, by stratum or by Helm. This covers
  `./cmd/stratum` only: the payload binaries the release artifact appends,
  such as runc, are outside the check (see
  [What the policy covers](#what-the-policy-covers)).

### Inter font 4.001

- **License:** OFL-1.1.
- **Where:** The font files are vendored in `dash/src/fonts/` and shipped in
  the dashboard bundle (THIRD_PARTY_NOTICES.md, "Inter (font)").
- **Disposition:** Exception decided by the human on 2026-10-08, as part of
  confirming this license policy. Decision text: "OFL-1.1 only as a named
  exception for the vendored Inter font". The check treats the font as the
  dashboard's own source and does not scan it.

### Quantic-derived styles

- **License:** Unclear provenance.
- **Where:** `dash/src/theme.css` and `dash/src/ui/controls.css` were derived
  from an earlier design system, Quantic, and ship in the dashboard bundle.
- **Disposition:** A follow-up in the issue tracker, which predates this
  policy: the provenance clearance is still open (THIRD_PARTY_NOTICES.md,
  "Still to clear").

### Warnings from the check

On 2026-10-08, run after the dashboard build, `tools/lint-licenses.sh` checked
1147 third-party Go packages linked for linux/amd64 and linux/arm64 and the 6
npm packages in `dash/.bundle/packages.json`, with 0 fail and 0 warn in both
runs. Its only finding was the `NOTE` line for go-digest's exception.

On 2026-10-10 the check began to warn on license-file text that no license
matches, and the same run printed 20 `WARN` lines for the 8 modules below, 0
fail, and 0 warn for npm. Every license each file identifies is allowed; what
warns is prose beside them. Each has the disposition "a warning pending human
review". When a later run prints a new `WARN` line, add an entry here for it
with the same disposition.

| Component | Packages | Text that matches no license | How it is linked |
|---|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` v1.8.0 | 10 | A notice of the move from MIT to Apache-2.0, and that documentation (excluding specifications) is CC-BY-4.0 | `internal/mcp` |
| `github.com/Masterminds/squirrel` v1.5.4 | 1 | "Squirrel: The Masterminds", a heading under the MIT title | `helm.sh/helm/v4/pkg/storage/driver` |
| `github.com/russross/blackfriday/v2` v2.1.0 | 1 | "Blackfriday is distributed under the Simplified BSD License:" | `helm.sh/helm/v4/pkg/kube` → `k8s.io/kubectl/pkg/cmd/util` |
| `github.com/shopspring/decimal` v1.4.0 | 1 | "Based on https://github.com/oguzbilgic/fpd, which has the following license:" | `helm.sh/helm/v4/pkg/action` → `github.com/Masterminds/sprig/v3` |
| `go.yaml.in/yaml/v2` v2.4.4 | 1 | `LICENSE.libyaml`: which files were ported from libyaml and stay MIT | `internal/cluster` → `sigs.k8s.io/yaml` |
| `go.yaml.in/yaml/v3` v3.0.5 | 1 | Which files are MIT and which are Apache-2.0 | `helm.sh/helm/v4/pkg/action` |
| `sigs.k8s.io/json` v0.0.0-20250730193827-2d320260d730 | 2 | Which files are BSD-3-Clause and which are Apache-2.0 | `internal/certs` → `k8s.io/apimachinery/pkg/util/json` |
| `sigs.k8s.io/yaml` v1.6.0 | 3 | Which forked go-yaml files are MIT, which BSD-3-Clause, and which Apache-2.0 | `internal/cluster` |

### Follow-up tickets

No license finding needed a new follow-up ticket when this policy was
adopted, and the human chose to file none. Of the license findings not
remediated then, four received an exception the human decided: go-digest,
lightningcss, libseccomp and the Inter font. The pathrs-lite subpackage is not
linked, so its disposition is no action, and the Quantic-derived styles'
provenance clearance was already tracked before this policy.
