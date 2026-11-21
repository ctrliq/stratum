## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you write code, a comment, a commit or a pull request. It holds the setup, build and test commands, the code conventions, and the branch, commit and sign-off rules.

Issues and pull requests live on GitHub. A tracker key may appear in a branch name or a pull request title, never in code, a comment or a doc: each states its reason itself. `make lint` fails on an internal tracker key or a tracker or wiki URL anywhere in the tree.

## Licenses

Third-party code that stratum ships may carry only **MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, 0BSD, Zlib or BSL-1.0**. That code is the Go packages linked into `./cmd/stratum` for linux amd64 and arm64, plus the npm packages bundled into `dash/dist`, whichever `package.json` section declares them (react is a devDependency and is checked); internal tooling under `tools/`, npm packages that only run the build and are not bundled, payload binaries and container images are out of scope. OFL-1.1 is allowed only for the vendored Inter font.

Any well-formed license id outside that list fails the check, even one SPDX does not define (`BSD`, `UNLICENSED`). Only a **missing license, or one the check cannot read (an unparseable license field, an unrecognised license file), prints a `WARN` line and does not fail**. Run the check with `make licenses`, or `make licenses TAGS=noui` to check only the Go packages without building the dashboard.

When a dependency fails the check, **never add an exception or widen the allowlist** in `tools/licensecheck/policy.go`: only a human decides an exception. Stop and ask the human, giving the package, its license, and how it is linked (`go mod why <import path>` for a Go package, `pnpm why <name>` in `dash/` for an npm one). Report any new `WARN` line too. [LICENSING.md](LICENSING.md) has the full policy, how to read the output, and the register of every license decision.

## Comments

Names, small functions and tests carry the meaning. Comments drift from the code and
then mislead the next reader, human or agent, so this repository keeps almost none.

- Write a comment only for a reason the code cannot show: an outside constraint or a
  choice that looks wrong but is not. One line, at most 100 characters.
- Do not restate a name or describe what the code does. No narrative comments: no
  history ("used to", "the old version", "once accepted"), no story of a change or a bug
  ("now uses", "the bug"), no task state ("written first", "red until the fix lands",
  "for now"), no first-person voice ("we"), no step-by-step walkthroughs ("Step 1:").
  Say what is true now and why. Do not cite spec sections (`§16`), ADR numbers or
  tracker ids.
- If a block needs a heading to be understood, extract a named function. If a test
  proves the behavior, the test is the documentation.
- When you change code, delete or fix every comment the change makes untrue.
- `make lint` and CI run `tools/commentcheck`, which fails on a Go comment longer than one
  line or 100 characters, a `§`, a tracker id or a narrative phrase. Directives
  (`//go:`, `//nolint`), generated-file headers and `testdata` are exempt.
