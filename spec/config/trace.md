# config: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.

## kinds.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Each config kind has its own list | implemented | dash-next/src/domain/config/constants.ts, dash-next/src/domain/config/pages/*List/index.tsx, dash-next/src/routes/c.$cluster.<segment>.tsx for each of the eleven segments |
| Each config kind shows its columns in order | implemented | dash-next/src/domain/config/constants.ts |
| A config column shows a value read from the object | implemented | dash-next/src/domain/config/utils/transforms.ts, dash-next/src/domain/config/utils/selectors.ts |
| A config map with no data counts zero keys | implemented | dash-next/src/domain/config/utils/selectors.ts (keyCountOf) |
| An autoscaler's metrics appear only in its manifest | implemented | dash-next/src/domain/config/constants.ts (autoscalerColumns) |
| Config kinds have no actions of their own | implemented | dash-next/src/domain/config/pages/*List/index.tsx |

## secrets.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The Secrets list shows no secret values | implemented | dash-next/src/domain/config/constants.ts (secretColumns), dash-next/src/domain/config/utils/transforms.ts (toSecretRow) |
| Opening a secret shows every value in its stored encoding | implemented | dash-next/src/domain/config/pages/SecretsList/index.tsx (the detail read carries the selected cluster), dash-next/src/domain/resources/components/LoadedDetail.tsx (reads the one secret, then the generic detail) |
| Editing a secret edits its encoded values as JSON | implemented | dash-next/src/domain/config/pages/SecretsList/index.tsx, dash-next/src/domain/resources/components/LoadedDetail.tsx, dash-next/src/domain/resources/components/ResourceDetail.tsx |
| The open Secrets list repeatedly reads every secret in full | dropped | — |
| The Secrets list is read without secret values | implemented | dash-next/src/domain/config/pages/SecretsList/index.tsx (secret summaries when features.secretSummaries, otherwise the metadata-only Table), dash-next/src/domain/config/utils/parsers.ts (fromSecretTable), dash-next/src/domain/config/utils/transforms.ts (toSecretListings), dash-next/src/domain/config/types/config.ts (SecretTable). Not exercised against a live apiserver |
| A user who may not list secrets sees the generic failure | dropped | — |
| A user who may not list secrets sees the forbidden state | implemented | dash-next/src/domain/config/pages/SecretsList/index.tsx, dash-next/src/domain/resources/utils/formatters.ts (toListMessage) |
