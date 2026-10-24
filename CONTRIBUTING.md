# Contributing to Stratum

Thank you for helping improve Stratum. This guide takes a change from a fresh
clone to a merged pull request: how to report a problem, set up a machine,
build and test, and the conventions every commit and pull request follows.

Stratum is open source under the [Apache License 2.0](LICENSE). The CIS
profile and its auditor are part of [Stratum PRO](docs/docs/pro/index.md), a
separate commercial product, and are not developed here.

## Reporting a problem or proposing a change

**Bugs and feature requests** go in
[GitHub Issues](https://github.com/ctrliq/stratum/issues). For a bug, include:

- the output of `stratum version`;
- the host OS and architecture, and whether the node is a control plane or a
  worker;
- what you ran, what you expected, and what happened, with the relevant log
  lines (`journalctl -u stratum` on the node).

**Security vulnerabilities do not go in public issues.** See
[SECURITY.md](SECURITY.md).

**Discuss a large change first.** Before you write a new subsystem, change how
the PKI or etcd trust works, add a dependency or change a command's behaviour,
open an issue describing the problem and the approach. Agreeing the design
first saves rewriting the pull request.

## Setting up

Stratum builds on Linux and macOS. Running a node, and the integration tests,
need Linux, or Docker on macOS.

**Go.** The Go version is pinned as `go_version` in
[`build/versions.mk`](build/versions.mk), the one file where every third-party
version lives. `go.mod` can lag that pin, so build and test with the pinned
toolchain:

```bash
export GOTOOLCHAIN=go$(sed -n 's/^go_version *= *//p' build/versions.mk)
```

**Everything else** comes from [mise](https://mise.jdx.dev): node and pnpm for
the dashboard, shellcheck for `make lint`, kubectl, and syft, cosign and crane
for the release targets. Go and Docker are not managed by mise.

```bash
mise install
```

**Docker** is needed for `make smoke` and the integration tests.

**pre-commit** runs the fast checks and the sign-off check on every commit,
and `make lint` before every push. Enable all three stages once per clone:

```bash
pre-commit install
```

## Building

`make help` lists every target. The ones you will use most:

| Command | What it does |
|---|---|
| `make build` | Builds the `stratum` binary into `dist/`, with the dashboard embedded. |
| `make build TAGS=noui` | The same, without building the dashboard. |
| `make dash` | Builds the dashboard into `dash/dist`. See [dash/README.md](dash/README.md). |
| `make components` | Builds every pinned component (containerd, runc, etcd, Kubernetes and the rest) from source. |
| `make package` | Builds the binary and appends the component payload: the release artifact. |
| `make smoke` | Runs the packaged binary in a clean container. |

The build targets Linux by default (`TARGET_OS=linux`), for the host's
architecture unless `TARGET_ARCH` says otherwise.

## Testing

CI runs everything below on every pull request. Run what your change touches
before you push.

**Unit tests.** `make test` checks gofmt, runs `go vet` and runs every Go test.
`TAGS=noui` skips the dashboard:

```bash
make test TAGS=noui
```

**The gate list.** `make lint` is the one list of repository checks CI and the
pre-push hook run. It checks, among others, that:

- every third-party version appears only in `build/versions.mk`;
- every `stratum` command and flag the Markdown documents exists in the CLI;
- every node file is written by the package that owns it;
- the tooling scripts pass their own self-tests.

**Licenses.** `make licenses` checks the licenses of the Go and npm code
Stratum ships. Only MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, 0BSD,
Zlib and BSL-1.0 are allowed. If a dependency you add fails the check, say so
in the pull request; never widen the allowlist in
`tools/licensecheck/policy.go`. [LICENSING.md](LICENSING.md) has the policy.

**Integration tests.** Each check is one script in
[`tools/inttest/`](tools/inttest/), run against the packaged binary inside
containers that act as nodes. Build the package for the architecture your
Docker runs, then run one check or all of them:

```bash
make package TARGET_ARCH=$(docker version --format '{{.Server.Arch}}')
tools/inttest-run.sh tools/inttest/check-single-node.sh
make inttest
```

To add a check, add a `check-<name>.sh` file to `tools/inttest/`. CI builds
its job list from that directory, so no workflow change is needed. A header
comment declares what the check needs. For example,
`# ske-inttest: topology=ha3` runs it against three control planes;
`tools/inttest-matrix.sh` lists every directive. Single-node checks source
`harness.sh`, and three-node checks source `harness-ha.sh`.

**Documentation.** `make docs` builds the documentation site in strict mode,
so a broken link fails the build.

## Code conventions

- **Match the code around you**: its naming, its error messages and how much
  it comments. gofmt and `go vet` are gates.
- **A comment states its reason itself.** It does not point to a ticket or a
  design record, which a reader of the code may not be able to open.
- **Third-party versions live only in `build/versions.mk`.** Do not write a
  version anywhere else, including in comments; `make lint` fails on one.
- **Node files are written by their owning package**, through the shared atomic
  writer. `make lint` enforces this.
- **`pkg/cli` exports no internal types.** It is the API that downstream builds
  import.
- **A behaviour change comes with a test**: a unit test, plus an integration
  check when the behaviour spans nodes or needs a running cluster. A bug fix
  includes a test that fails without the fix.

## Commits and pull requests

### Branch names

Branches follow [Conventional Branch](https://conventional-branch.github.io/),
lowercase:

```text
<type>/[<tracker-key>-]<short-description>
```

`<type>` is `feat`, `fix`, `hotfix`, `release` or `chore`, as in
`fix/etcd-peer-allowed-cn`. The lowercase tracker key is optional. A tracker
key may appear in a branch name or a pull request title, never in code, a
comment or a doc.

### Commit messages

Commit subjects and pull request titles follow
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```text
<type>(<scope>): <description>
```

- `<type>` is one of `feat`, `fix`, `docs`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore` and `revert`.
- `<scope>` is the package or area changed, such as `etcd`, `upgrade` or
  `ha`.
- `<description>` is imperative and lowercase.
- A breaking change puts `!` after the type or scope and adds a
  `BREAKING CHANGE:` footer.

The body says what changed and why. Footers come last, and every commit ends
with its sign-off.

```text
fix(etcd): accept only the etcd leaf on etcd's peer port

etcd's peer port trusted the same bundle as its client port, with no
check on who the certificate named, so any leaf that bundle's CAs sign
was a raft peer credential. etcd now runs with --peer-cert-allowed-cn.

Signed-off-by: Jane Doe <jane@example.com>
```

### Sign your commits off

Every commit carries a `Signed-off-by:` line certifying the
[Developer Certificate of Origin](https://developercertificate.org/) (DCO).
By signing off, you certify that you wrote the change, or otherwise have the
right to submit it under the project's open source license.

`git commit -s` adds the line from your `user.name` and `user.email`. The
sign-off must carry the commit author's email. The pre-commit `commit-msg` hook
refuses a commit without one, and CI's `signoff` check fails a pull request
with any commit that lacks one; merge commits are not checked. To add a missing
sign-off:

```bash
git commit --amend --signoff
git rebase --signoff origin/main
```

The first command fixes the last commit; the second fixes every commit on
the branch.

### Pull requests

- **One logical change per pull request.** A refactor that a fix needs goes in
  its own commit, or its own pull request.
- **The title follows the commit convention.** Pull requests are squash-merged,
  so the title becomes the commit subject on `main`.
- **The description** says what the problem was, what changed, how you tested
  it, and anything left out on purpose.
- **CI must pass.** If a check fails for a reason unrelated to your change,
  say so in the pull request rather than retrying until it passes.
- **Review.** A maintainer reviews and merges. Address each review comment, or
  reply saying why not, including comments from automated reviewers.

**Backports** to a `release-1.X` branch are a maintainer's call. A maintainer
adds a `backport/release-1.X` label, and a bot opens the backport pull request
once the original merges.

## Documentation

User-facing documentation lives in [`docs/docs/`](docs/docs/) and is published
as the documentation site. [docs/README.md](docs/README.md) has the writing
rules. The main one: every command, flag, path and version is checked against
the binary, and `make lint` enforces it for commands and flags. A change that
adds or alters a command updates its page in the same pull request.

## Releases

Releases are cut by maintainers, following [RELEASE.md](RELEASE.md).

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), as section 5 of the license provides, and you
certify the DCO with your sign-off. Third-party material in this repository
keeps its own license, listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
