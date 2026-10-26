# Public documentation

The public documentation for Stratum Kubernetes Engine, built with
[MkDocs Material](https://squidfunk.github.io/mkdocs-material/).

This is the **user-facing** site. Its counterpart is `docs/internal/`, holding the
engineering docs — `build/` and `security/` — which are read in the repository
rather than published.

## Build it

```bash
make docs          # build once into docs/site/
make docs-serve    # live preview on http://localhost:8000/
```

Both create a virtualenv at `docs/.venv` and install the versions pinned
in `build/versions.mk`. `site/` and `.venv/` are gitignored; do not commit either.

There is no separate development config. MkDocs computes internal links as
relative paths between source files, so `mkdocs serve` and a production build
produce the same hrefs — `site_url` only feeds `sitemap.xml` and the canonical
`<link>`.

## The build is strict, and that is the point

`mkdocs build --strict` turns a broken internal link, a nav entry pointing at a
file that does not exist, or an unrecognised config key into a **build failure**.
Without it MkDocs prints a warning and renders the page anyway.

`make docs` additionally asserts the page count. `--strict` catches a link to a
page that is gone; it says nothing about a page quietly dropping out of the nav,
which renders a perfectly valid site with less in it. If you add or remove a
page, update the count in the `docs` target deliberately.

CI runs the same `make docs`, so the target cannot drift away from what is
checked.

## Versions are pinned in `build/versions.mk`

`mkdocs_version` and `mkdocs_material_version`, with Renovate annotations, like
every other tool a job installs. Nothing here carries a version literal.

**mkdocs-material has an announced 2.0 rewrite with no migration path**, and
prints a notice about it on every build. That is a known, accepted risk, and the
pin is how it is contained: Renovate only automerges minor and patch, so the
major arrives as its own PR that has to pass `make docs` before anyone reads it.

## Writing

Content lives in `docs/`. One directory per section, `index.md` for the section
landing page — that layout is what makes the published URLs `/<section>/` and
`/<section>/<page>/`.

**Every page starts with an `# H1` matching its nav title.** MkDocs renders only
what is in the file; nothing synthesises a heading from front matter.

Front matter is optional and carries only `description:` where a page has one.
Page order is the `nav:` block in `mkdocs.yml`, not a per-page `weight`.

**Link to other pages by file path**, not by URL:

```markdown
[Hardening](../cluster-admin/hardening.md)
[Migrating](index.md#migrating-an-existing-cluster-onto-the-etcd-ca)
```

A directory-style link like `](../cluster-admin/hardening/)` renders and works in
a browser, but MkDocs cannot resolve it, so `--strict` will not check it — a typo
in one ships silently. Use the `.md` path and let the build verify it.

### Admonitions

```markdown
!!! warning "Body text follows, indented four spaces"
    A misspelled key is an error, not a silently ignored setting.
```

`note`, `tip`, `info`, `warning`, `danger`, `example` and the rest of
[Material's set](https://squidfunk.github.io/mkdocs-material/reference/admonitions/).
An unknown type renders unstyled rather than failing the build, so check the
output when adding one.

The title is **rendered as the admonition's heading**.

### Cards

The landing page's section grid is Material's `grid cards` recipe, which needs
`attr_list` and `md_in_html` — both enabled:

```markdown
<div class="grid cards" markdown>

-   ### [Quick Start](quick-start/index.md)

    One node to Ready, with no registry access.

</div>
```

These are hand-written. Adding a page to a section does not add a card to its
index; add both.

### Style guide: depth and voice

How deep a page goes and what it leaves out follows the upstream Kubernetes
contributor guides: the [style guide](https://kubernetes.io/docs/contribute/style/style-guide/),
the [content guide](https://kubernetes.io/docs/contribute/style/content-guide/) and
[page content types](https://kubernetes.io/docs/contribute/style/page-content-types/).
Where this site departs from them, this section says so and why.

#### Page types and how deep each goes

Each page is one type. A page that mixes types splits along the seam.

| Type | Answers | Holds | Depth limit |
|---|---|---|---|
| Section index | What is in this section, and do I need it? | Overview, one line per page, links | One screen. No procedures. |
| Concept | How does this work, and what does it mean for me? | Overview, explanation, up to 5 "next" links | No numbered steps; link to the task instead. |
| Task | How do I do one thing? | Overview, prerequisites as bullets, numbered steps, how to verify, what to do on failure | One goal. Explain only what you need to choose an option or recognise success. |
| Tutorial | How do I reach a larger goal end to end? | Overview, objectives, steps, cleanup | Surface-level explanation; link to concepts for depth. |
| Reference | What is the exact value, path, flag or default? | Tables and lists | Complete, with no narrative and no steps. |

A quick test for a task page: if the prose between two steps is longer than the
steps around it, that prose belongs on a concept page or in an appendix.

#### What to include and what to leave out

Include what the operator types, sees and decides, the errors they will meet
(quote the exact text) and how to recover from each.

Leave out, or move:

- **Internals the operator does not act on.** Which component renders which
  file, and why the code is built that way, go in the commit message. Keep an
  internal only when it changes what the reader does or how they read a symptom.
- **Design argument and history.** Do not explain what an earlier design or
  document got wrong.
- **Upstream behaviour.** Link to the canonical Kubernetes, Cilium or etcd page
  instead of restating it. Copied content goes stale.
- **Future plans.** Document what the binary does today.

Move, don't lose: detail an operator could still need goes to the page that
owns it, or to an appendix, and the task page links to it in one line. Paths,
object names and timings go in [Paths and Names](docs/appendices/paths-and-names.md),
terms go in the [Glossary](docs/appendices/glossary.md). Flags are not listed
by hand: the binary documents them, so point to `stratum <command> --help`.

#### Open every page with an overview

Directly under the page's H1 title, before any subsection heading, command or
table, write 2-4 sentences that say what the page covers, when you need it and,
if it is not obvious, what you need first.

```markdown
# Upgrades & Rollback

This page upgrades one node, or a whole cluster, to a newer Kubernetes release,
and rolls a node back to its previous binaries. Use it when a new `stratum`
release is available. You need the new binary and root access to the nodes.
```

#### Define every term at first use

Spell out each acronym the first time a page uses it, as in *virtual IP (VIP)*,
and link the glossary entry if one exists. Write a new term in italics where you
define it. A term used on more than one page also gets a glossary entry.

#### Voice

- **Second person.** Write "you", not "we", "the user" or "one".
- **Present tense.** "`stratum up` writes the unit", not "will write".
- **Active voice, with a named actor.** "Stratum rotates the certificate", not
  "the certificate is rotated".
- **Short sentences.** One idea each, roughly 25 words at most. Split a chain of
  semicolons, colons and dashes into separate sentences.
- **Imperative steps.** "Run", "Copy", "Check", one action per step.
- **Plain and neutral.** State what something does. Do not sell it.

Banned patterns:

- Marketing and minimising words: *seamless*, *powerful*, *robust*,
  *effortless*, *simply*, *just*, *easy*, *straightforward*. They sound
  condescending when the step fails.
- Words that date: *currently*, *planned*, *will* for future behaviour, and
  *now* as in "Stratum now supports".
- Rhetoric: "that is the point", "the thing itself", "would be a poor
  advertisement".
- Defining by negation: "It is not a fork and not a different distribution".
  Say what it is.
- Latin abbreviations: write "for example", "that is" and "and so on".
- Bold scattered through a paragraph. Bold at most one fact per section.

Before, from the upgrades page, narrating why the code behaves as it does:

> The upgrade always runs as the version it installs, because the control-plane
> manifests it renders come from that binary, not from its payload. So the
> installed binary, given a different `--from`, hands off to it: it prints
> `handing off to the upgrade binary …` and the upgrade binary carries on with
> the same arguments.

After, saying what the operator sees and whether they must act:

> When `--from` names a different binary, the installed `stratum` hands the
> upgrade to that binary and prints `handing off to the upgrade binary …`. The
> new binary continues with the same arguments. You do not need to run anything else.

#### Admonitions

- `warning` for data loss, an outage, a security exposure or a step that cannot
  be undone. Put it before the step it guards, and name the risk in the title.
- `note` for context the reader must not miss; `tip` for an optional shortcut.
- Do not put main-path instructions in an admonition. A page with more
  admonitions than sections is using them as body text.

#### Command blocks

A later pass converts every command block to the target form below. Until it
lands, leave existing blocks and fence languages as they are, and write new
blocks the way the page around them does.

The target form:

- `#` before a command run as root on a node, in place of `sudo`.
- `$` before a command run as an unprivileged user, such as `kubectl` from a
  workstation.
- A blank line between commands.
- Output directly under the command that produced it, with no prompt.
- Placeholders in angle brackets, such as `<dir>`.

```text
# stratum upgrade --target v1.36.4 --from /var/lib/stratum/upgrade/stratum-v1.36.4

# stratum version --components

$ kubectl get nodes
```

## No third-party requests

`theme.font` is **off** deliberately. Left unset, Material downloads Inter and a
code font from `fonts.googleapis.com` *during the build* and bundles them, which
makes the build itself require network access.

Inter is vendored at `docs/assets/fonts/` and declared by `docs/assets/extra.css`.
Shipping an external font request in the documentation of a product whose
headline claim is airgap operation would be a poor advertisement — and a build
that needs the network to succeed is worse than a font that falls back.

`extra.css` also maps the brand's green palette onto Material's
`--md-primary-*` / `--md-accent-*` variables. Material derives the rest from that
pair, which is why the file is short.

## The rule this site is written under

**Every command, flag, path and version here is checked against the binary, not
against prior documentation.**

That is not a style preference: documentation that drifts from the binary
describes a CLI the reader cannot run.

`tools/lint-docs-cli.sh` enforces it in CI: it extracts every documented
invocation and flag from all tracked Markdown and asserts each against the real
CLI surface, parsed from the cobra command tree in `pkg/cli/`, plus
`internal/dashboard/` and `internal/operator/`.

## Publishing

Where the built site is hosted is decided outside this repository. The site is
self-contained so that decision does not hold up the content: a host that needs
a different generator loses the generator, not the writing — these are plain
Markdown files. The `docs` target in the `Makefile` asserts the page count, so
`make docs` fails if it drifts.
