## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you write code, a comment, a commit or a pull request. It holds the setup, build and test commands, the code conventions, and the branch, commit and sign-off rules.

Issues and pull requests live on GitHub. A tracker key may appear in a branch name or a pull request title, never in code, a comment or a doc: each states its reason itself. `make lint` fails on an internal tracker key or a tracker or wiki URL anywhere in the tree.

## Licenses

Third-party code that stratum ships may carry only **MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, 0BSD, Zlib or BSL-1.0**. That code is the Go packages linked into `./cmd/stratum` for linux amd64 and arm64, plus the npm packages bundled into `dash/dist`, whichever `package.json` section declares them (react is a devDependency and is checked); internal tooling under `tools/`, npm packages that only run the build and are not bundled, payload binaries and container images are out of scope. OFL-1.1 is allowed only for the vendored Inter font.

Any well-formed license id outside that list fails the check, even one SPDX does not define (`BSD`, `UNLICENSED`). Only a **missing license, or one the check cannot read (an unparseable license field, an unrecognised license file), prints a `WARN` line and does not fail**. Run the check with `make licenses`, or `make licenses TAGS=noui` to check only the Go packages without building the dashboard.

When a dependency fails the check, **never add an exception or widen the allowlist** in `tools/licensecheck/policy.go`: only a human decides an exception. Stop and ask the human, giving the package, its license, and how it is linked (`go mod why <import path>` for a Go package, `pnpm why <name>` in `dash/` for an npm one). Report any new `WARN` line too. [LICENSING.md](LICENSING.md) has the full policy, how to read the output, and the register of every license decision.
