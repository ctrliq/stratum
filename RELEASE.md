# Cutting a SKE release

This is the whole procedure. If you have never cut a release, follow it top to
bottom and do not improvise — every rule below is here because skipping it broke
something at least once, in this project or in the one it learned from.

The short version: you create a **signed tag**, and everything else is a
pipeline. You do not build artifacts, you do not upload files, you do not edit
version numbers. You write one paragraph of prose and press Publish.

---

## 1. Who may cut a release

Tagging is restricted to the release captains. Two, minimum, so a release is
never blocked on one person being asleep.

| Captain | GitHub | Signing key |
|---|---|---|
| JR Bing | `@jrbing` | `SHA256:OrIz8ueaZgKzWNWk/JLZzZzADGEiDpK/L9yWUEMmOx4` (ssh-ed25519) |
| Chris Wolford | `@WolfyChris` | `SHA256:2k9NDIXZ6pGNSu+G1c2LBF2Q+np/pqFbLICebV09Q/w` (ssh-ed25519) |

A captain needs:

- **Push access to tags** on `main` and every `release-1.X` branch.
- **A signing key git will use.** `tools/release.sh` refuses to run without one,
  because an unsigned tag proves nothing about who cut the release.
  - GPG: `git config --global user.signingkey <keyid>`
  - SSH (simpler, and enough): `git config --global gpg.format ssh` and
    `git config --global user.signingkey ~/.ssh/id_ed25519.pub`, then add the
    key to your GitHub account **as a signing key** (a separate list from
    authentication keys — adding it once does not add it twice).
- Their public key recorded in the table above and in
  `.github/allowed_signers`, so anyone can verify a tag they did not cut.

Nothing else. Registry credentials, signing keys for artifacts, and channel
publishing all live in the pipeline, not on your laptop. If a step asks you for
a credential, something is wrong — stop and ask.

---

## 2. The rules

### 2.1 Every final is preceded by a release candidate

A final release may only be cut from a commit that an **RC has already taken
through the full conformance run**. Not "an RC exists" — an RC that was built,
signed, and came back green from `conformance-test` on both architectures.

This is the whole point of having RCs. Conformance runs against real provisioned
infrastructure and takes hours; it is the one gate that cannot be moved earlier
in the pipeline. RCs are where it is allowed to fail.

Practically:

1. Cut `vX.Y.Z-stratum.N-rc.1`.
2. Wait for the release workflow to go green, conformance included.
3. If it fails, fix, and cut `-rc.2`. Repeat.
4. Cut the final **from the same commit as the last green RC**.

`tools/release.sh` warns when the final is not on the RC's commit, and tells you
how many commits have slipped in since. Any commit not in the RC has not been
through conformance. Treat that warning as a stop sign unless you can say
exactly why the extra commits cannot affect conformance.

**The one exception:** re-cutting a final that was published with a broken
artifact or wrong notes, from the identical commit. Same code, new counter.

### 2.2 SKE fixes ride the next upstream patch release

The default is: a fix lands on `main`, gets backported to the release branches,
and ships with the next upstream Kubernetes patch. The cadence is upstream's,
not ours.

### 2.3 Out-of-band hotfixes

`N > 0` in `vX.Y.Z-stratum.N` means a SKE release on an upstream base that has
already shipped. It exists for exactly two situations:

- **A security fix** that cannot wait for the next upstream patch — a CVE in a
  component SKE pins independently (containerd, runc, Cilium, kube-vip, etcd),
  or in SKE's own code.
- **A broken release** — the artifact does not install, an image is missing from
  the payload, a signature does not verify.

Anything that *can* wait, waits. "Customer asked" is not a criterion; "the
cluster does not come up" is.

A hotfix follows the same rules as any other release, with two additions:

- **It still gets an RC.** A hotfix is the release most likely to be rushed and
  therefore the one that most needs the gate. If the fix is urgent enough that
  hours of conformance are genuinely unaffordable, that is a decision two
  captains make together and write down in the release notes — not one a captain
  makes alone at 2am.
- **Security hotfixes stay private until the coordinated release date.** Build
  the fix with [`tools/embargo-build.sh`](tools/embargo-build.sh), which applies
  the patch in a private mirror and holds the artifacts unpublished. Do not push
  a tag before that date: tagging in public is a disclosure.

### 2.4 Release every maintained minor together

When upstream ships a patch set — and upstream ships all supported minors on the
same day — SKE releases **all maintained minors the same day**. Not "the one a
customer asked about first".

The maintained minors are the lines in
[build/branches.txt](build/branches.txt), one `release-1.X` branch each.
`tools/release.sh` prints the other minors as a reminder when you cut.

A minor left a week behind is a minor whose users are a week exposed to whatever
that patch set fixed, and it is invisible — nobody files a bug saying "you did
not release". Do them together or the batch does not happen.

Each minor is its own tag, its own workflow run, its own conformance run, its own
draft. There is no combined release. Cut them in whatever order, but do not
publish any of them until all of them are green.

---

## 3. Cutting a release, step by step

### Before you start

```bash
git checkout main            # or release-1.35, etc.
git pull
```

Confirm the branch is releasable. It is releasable when CI is green on the
commit you intend to tag — not "green recently". Open the commit on GitHub and
look.

Confirm the pins are what you expect:

```bash
make print-version           # the version this commit would produce
grep '^kubernetes_version' build/versions.mk
```

If `kubernetes_version` is not the upstream patch you mean to ship, stop: the
Renovate bump PR has not merged yet. Merge it, let CI go green, start again.

### Cut the RC

```bash
tools/release.sh --rc
```

It asks nothing you have to think hard about: the version is derived from the
pins and the tag list, and the counter defaults to the right answer. It will
refuse to run on a dirty tree, on a branch that is not `main` or `release-1.X`,
on a commit that is not pushed, and without a signing key.

The tag is pushed explicitly (`git push origin <tag>`, never `--tags`), which
starts [.github/workflows/release.yml](.github/workflows/release.yml).

### Watch the pipeline

The run does, in order:

| Job | What it does | If it fails |
|---|---|---|
| `draft-release` | creates the draft (prerelease for `-rc.` tags) | check tag format |
| `build` (amd64, arm64) | native build, **smoke test**, importer assets (payload zip, engine ldflags), SBOM, cosign sign, upload | a real defect — fix and cut the next RC |
| `checksums` | `sha256sums.txt`, signed | rerun |
| `images` | multi-arch OCI to ghcr.io | check registry credentials |
| `mirror-images` | runtime images into the SKE namespace, by digest | check registry credentials |
| `conformance-test` (GCP × amd64, arm64) | hydrophone on provisioned infrastructure | **this is what RCs are for** |
| `conformance-upload` | attaches the results tarball to the release | rerun |
| `conformance-gate` | fails the run if a **final** has no green conformance | do not publish |

The build job smoke-tests the artifact **before** signing it. Nothing gets a
signature until it has been run.

Conformance takes hours. Nothing else in the run waits for it, so the artifacts
are attached to the draft long before the gate resolves.

### Depot delivery

Every published RC and final is also delivered to Depot once the conformance
guard succeeds: RCs to its `rc` channel, finals to `stable`. Depot is an
additional target, not a replacement for the GitHub release assets or ghcr.io,
and a failed Depot delivery holds back neither of them. Draft releases are never delivered.

The delivery workflows read bare hostnames from the GitHub Actions variables
`DEPOT_DEV_HOSTNAME` and `DEPOT_PROD_HOSTNAME`. They pass the selected value to
the publisher as `DEPOT_HOSTNAME`, which always uses HTTPS. Hostnames must not
include a protocol or path. Publication credentials remain Actions secrets.

A Depot release carries the same executables, build-info, signatures,
certificates, SBOMs and SBOM signatures as the GitHub release, plus a checksum
per executable and a `manifest.json`. Before anything is uploaded, the delivery
verifies the signed checksum index, the executables and the SBOMs against the
release workflow's identity at that exact tag.

### Cut the final

Once the RC's run is fully green — conformance included — cut the final **from
the same commit**:

```bash
git log --oneline -1          # confirm this is the RC's commit
tools/release.sh --final
```

The same pipeline runs again against the final tag.

### Write the notes

```bash
tools/release-notes.sh v1.36.4-stratum.0 > notes.md
```

This generates the skeleton: every merged PR since the previous tag, grouped
into upstream bump / components / features / fixes / backports, with the
upstream Kubernetes changelog linked at the top.

**Your job is the summary paragraph, and only that.** Two to four sentences at
the top of the release, answering: what is in this release, and does anyone have
to do anything? Everything below it is generated, and you should read it but
rarely need to edit it beyond deleting noise.

This should take under fifteen minutes. If it is taking longer, the skeleton is
wrong — fix `tools/release-notes.sh` rather than hand-editing every release.

Paste the notes into the draft release on GitHub.

### Publish

Before pressing Publish on a **final**, confirm:

- [ ] The workflow run is green, `conformance-gate` included.
- [ ] `conformance-results-gcp-amd64-<tag>.tar.gz` and the arm64 one are
      attached to the release. Each holds `e2e.log`, `junit_01.xml` and
      `summary.txt`. Until the hydrophone change is backported to every
      release branch, a tag cut from an unported branch carries the old
      `sonobuoy-conformance-results-…` name instead; `release-publish.yml`
      accepts either.
- [ ] `sha256sums.txt` and the `.sig`/`.pem` files are attached.
- [ ] `stratum-payload-linux-{amd64,arm64}.zip` and `stratum-ldflags-linux-{amd64,arm64}.txt` are attached, each with its `.sig`/`.pem`, and `ghcr.io/ctrliq/stratum` has the `$TAG-{ldflags,payload}-linux-{amd64,arm64}` images.
- [ ] The summary paragraph is written and the generated sections read sensibly.
- [ ] Every other maintained minor in this batch is also ready to publish
      (§2.4).

Then press Publish. That fires
[.github/workflows/release-publish.yml](.github/workflows/release-publish.yml),
which:

1. **Re-checks the conformance evidence.** A final published without it is put
   back into draft and an issue is opened. The gate is not advisory.
2. Delivers the release to Depot (see [Depot delivery](#depot-delivery)).

### Verify, as an outsider would

Do this once, on the first release you cut, so you know the published artifacts
are what they claim:

```bash
TAG=v1.36.4-stratum.0
gh release download "$TAG" -p 'stratum-linux-amd64*' -p 'sha256sums.txt'
sha256sum -c sha256sums.txt --ignore-missing

cosign verify-blob stratum-linux-amd64 \
  --signature stratum-linux-amd64.sig \
  --certificate stratum-linux-amd64.pem \
  --certificate-identity-regexp 'https://github.com/ctrliq/stratum/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Artifacts are signed **keyless, via GitHub OIDC** — no long-lived private key
exists to steal or rotate. The identity that signed is the workflow file at the
tag, which is why the `--certificate-identity-regexp` above names it.

The importer assets (`stratum-payload-linux-<arch>.zip` and
`stratum-ldflags-linux-<arch>.txt`, which a build that imports `pkg/cli` appends
and links) are **not** in `sha256sums.txt`: release-publish checks every name in
it against its own download, which leaves them out. Their signature is their only
check, so pin it to the exact tag:

```bash
gh release download "$TAG" -p 'stratum-payload-linux-amd64.zip*'
cosign verify-blob stratum-payload-linux-amd64.zip \
  --signature stratum-payload-linux-amd64.zip.sig \
  --certificate stratum-payload-linux-amd64.zip.pem \
  --certificate-identity "https://github.com/ctrliq/stratum/.github/workflows/release.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

While the release is a draft, reading its assets needs write on this repository.
The build job therefore also pushes each asset with its `.sig`/`.pem` to ghcr as
a single-layer image, `ghcr.io/ctrliq/stratum:$TAG-ldflags-linux-<arch>` and
`…:$TAG-payload-linux-<arch>`, which `read:packages` is enough to pull. The
same `cosign verify-blob` applies to what comes out:

```bash
crane auth login ghcr.io -u <user> --password-stdin   # a classic token with read:packages
crane export "ghcr.io/ctrliq/stratum:$TAG-payload-linux-amd64" - | tar -x
```

---

## 4. When it goes wrong

**A tag is never moved and never deleted.** A published tag is something people
have already fetched; changing what it points at is worse than any mistake it
would fix. Cut the next counter instead.

| Situation | What to do |
|---|---|
| Tagged the wrong commit, **not yet pushed** | `git tag -d <tag>` and start again |
| Tagged the wrong commit, **pushed** | cut the next counter (`.N+1`), leave the bad tag |
| Build failed on an RC | fix, cut `-rc.M+1` |
| Build failed on a final | fix, cut `.N+1` — do not retry the same tag |
| Conformance failed on an RC | that is the system working; fix and cut `-rc.M+1` |
| Conformance failed on a final | do **not** publish; fix and cut `.N+1` |
| Published a final without conformance | `release-publish` already un-published it; read the issue it opened |
| Channel verification failed | the release is published but not being served — treat as an outage, see the job log for whether the write or the read failed |
| A signature does not verify | do not publish; this is a broken pipeline, not a release problem |

**Never** hand-upload an artifact to a release. An asset that did not come from
the pipeline has no signature anyone can check, and there is no way for a
downstream user to tell it apart from one that does.
