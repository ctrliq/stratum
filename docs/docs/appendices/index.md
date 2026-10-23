# Appendices

This section holds the lookup material the other pages link to: terms, paths,
object names, defaults, and the full snapshot and upgrade rules. Use it when you need an
exact value rather than a procedure.

- **[Glossary](glossary.md)**: the terms and acronyms this documentation uses in a specific way
- **[Paths and Names](paths-and-names.md)**: where Stratum keeps things on a node, the Kubernetes objects it creates, and its default timings
- **[Snapshot Reference](snapshot-reference.md)**: snapshot files, retention, settings, restore rules, the controller backup and rejoin behaviour
- **[Upgrade Reference](upgrade-reference.md)**: the flags, refusals, objects and timings behind `stratum upgrade` and `stratum rollback`, and the checks for clusters on an earlier build

## Commands and flags

The binary documents its own commands. Run `stratum --help` for the list of
commands, and `stratum <command> --help` for a command's subcommands and flags:

```bash
stratum --help
stratum snapshot --help
stratum snapshot restore --help
```

The help comes from the same code that runs the commands, so it matches the
binary you have.
