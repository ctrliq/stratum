#!/usr/bin/env bash
# pkg/cli is stratum's one deliberate public API surface: an
# importer (a downstream build) adds its own cobra subcommands to the root it
# returns. That is only safe to import from outside this module if its
# exported surface never hands back a type from this module's own
# internal/... packages — an importer cannot even name such a type, but a
# struct field or method built around one would still leak its behavior and
# its churn. Fail loudly rather than rely on someone noticing in review.
set -euo pipefail
cd "$(dirname "$0")/.."

go run ./tools/apiboundary ./pkg/cli
