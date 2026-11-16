# hardening — trace

Paths are relative to `dash-next/`. Short names used in the tables:

- `utils/…` = `src/domain/hardening/utils/…` (each with its `.test.ts`)
- `hooks/…` = `src/domain/hardening/hooks/…`
- `Posture/…` = `src/domain/hardening/pages/HardeningPosture/…`
- `Drilldown/…` = `src/domain/hardening/components/EngineStateDrilldown/…`
- `clusters:…` = `src/domain/clusters/…`, imported, not edited
- transport = `src/shared/fetchers/sameOriginFetch.ts`, `requests.ts`

Routes: `src/routes/c.$cluster.hardening.tsx` (posture) and
`src/routes/clusters_.$cluster.engine-state.tsx` (drill-down, path
`/clusters/$cluster/engine-state`). Both render not-found on the
single-cluster dashboard server (D25) through
`src/domain/hardening/components/HubOnly/HubOnly.tsx`.

## posture.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The screen says so when no posture has been observed | dropped | — |
| Loading and unreachable are told apart | implemented | `utils/transforms.ts` (`toPostureLoad`), `Posture/index.tsx` |
| The header names the benchmark and offers a refresh | implemented | `Posture/index.tsx`, `constants.ts` |
| Refresh reloads the cluster's data and reports the outcome | implemented | `~/domain/shell/hooks/useManualRefresh.ts` (shell's refresh message and stale-list rules) |
| Read-only users see the same Hardening screen as admins | implemented | `Posture/index.tsx` (no role check) |
| The CSR approver card states the operator's verdict | implemented | `utils/formatters.ts` (`toOperatorLabel`), `utils/transforms.ts` (`fromLeaseFact`, `toPostureCards`), `Posture/parts/PostureCards.tsx` |
| The PodSecurity card counts namespaces that enforce the restricted level | implemented | `utils/transforms.ts` (`toPostureCards`, `toRestrictedNamespaces`) |
| The kubelet serving certificates card counts approved and pending requests | implemented | `utils/transforms.ts` (`toPostureCards`), `utils/predicates.ts` (`isPendingKnown`) |
| An unreadable cluster request list reads as 0 approved | dropped | — |
| An unreadable cluster request list leaves the approved count unknown | implemented | `utils/transforms.ts` (`toPosture`, `toPostureCards`, `toPostureSignals`) |
| A hub that states the request availability but no pending count | dropped | — |
| A missing pending count reads as unknown whatever the availability | implemented | `utils/predicates.ts` (`isPendingKnown`), `utils/transforms.ts` |
| The control plane card counts control-plane nodes | implemented | dash-next/src/domain/nodes/utils/predicates.ts (`hasControlPlaneRole`, D30), `utils/transforms.ts` (`toPosture`) |
| The observed signals state when the hub observed them | implemented | `utils/formatters.ts` (`toObservedCaption`), `Posture/parts/ObservedSignals.tsx` |
| The operator signal explains its verdict | implemented | `utils/formatters.ts` (`toOperatorDetail`), `utils/transforms.ts` (`toPostureSignals`) |
| The certificate request signal explains what is pending or why it is unknown | implemented | `utils/transforms.ts` (`toPostureSignals`) |
| The PodSecurity signal names the enforcing namespaces | implemented | `utils/transforms.ts` (`toPostureSignals`) |
| The control-plane signal reports labelled nodes | implemented | `utils/transforms.ts` (`toPostureSignals`) |
| The hub's facts decide the operator verdict and the pending request count | implemented | `hooks/usePosture.ts`, `utils/transforms.ts` (`toPosture`, `fromLeaseFact`, `fromPendingRequestsFact`) |
| Without hub facts the operator verdict is derived from the operator's lease | implemented | `hooks/usePosture.ts` (lease read only when the facts read failed), `utils/transforms.ts` (`fromLease`, `fromLeaseError`) |
| Without hub facts, kubelet-serving requests are counted from the cluster's certificate signing requests | implemented | `utils/predicates.ts`, `utils/selectors.ts`, `utils/transforms.ts` (`toPendingRequests`) |
| Requests for other signers are ignored | implemented | `utils/filters.ts` |
| Without hub facts, a request list that could not be read leaves the pending count unknown | implemented | `utils/transforms.ts` (`fromRequestListError`, `toPosture`) |
| PodSecurity enforcement is read from namespace labels | implemented | `utils/predicates.ts` (`isRestrictedNamespace`), `utils/transforms.ts` (`toRestrictedNamespaces`) |

## cis-profile.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The control list is the one the hub serves in its configuration | implemented | `utils/selectors.ts` (`cisProfileOf`), `utils/formatters.ts` (`toProfileTitle`), `Posture/parts/ControlProfile.tsx` |
| Controls are grouped under their group in the order the hub lists them | implemented | `utils/transforms.ts` (`toControlGroups`), `utils/formatters.ts` (`toGroupHeading`) |
| Each control shows its identifier, description and how it is applied | implemented | `utils/formatters.ts` (`toControlBadge`), `Posture/parts/ControlProfile.tsx` |
| A hub that serves no control list is reported as such | implemented | `Posture/parts/ControlProfile.tsx`, `utils/selectors.ts` (`cisProfileOf`) |
| The card always explains which controls depend on the CIS profile | implemented | `Posture/parts/ControlProfile.tsx`, `constants.ts` |
| Controls carry no result | implemented | `Posture/parts/ControlProfile.tsx` (no result, filter, sort or search) |

## engine-state-drilldown.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Engine state opens the drill-down for that cluster | implemented | `Drilldown/EngineStateDrilldown.tsx`, `src/domain/hardening/pages/EngineStateDetail/index.tsx`, route; link helper `utils/formatters.ts` (`toEngineStatePath`). D38: the drill-down is its own page, `/clusters/$cluster/engine-state`, not a panel below the list; the "Engine state" link is `clusters:pages/ClustersList/parts/ClusterEntry.tsx`. |
| The panel title falls back to the cluster id | implemented | `utils/formatters.ts` (`toDrilldownTitle`) |
| Close removes the drill-down | implemented | `Drilldown/EngineStateDrilldown.tsx` (`onClosed`); D38: `pages/EngineStateDetail/index.tsx` returns to `/clusters` with the list's search text, which the link carried as `?q=` (`utils/formatters.ts` `toEngineStatePath`, `toClusterListPath`; `src/routes/clusters.tsx`, `src/routes/clusters_.$cluster.engine-state.tsx`) |
| Opening another cluster's drill-down replaces the first and starts clean | implemented | `Drilldown/EngineStateDrilldown.tsx` (remounted per cluster id) |
| The drill-down stays open for a cluster that left the list | implemented | `pages/EngineStateDetail/index.tsx` (reads by id; the name is optional) |
| The drill-down is available to read-only users and in standalone mode | implemented | route (no role or mode check beyond D25). D37: a standalone hub has no Clusters page, so there the drill-down opens by its address only. |
| The drill-down is available for a disconnected cluster | implemented | route (no status check) |
| The panel says it is reading until the summary answers | implemented | `Drilldown/EngineStateDrilldown.tsx` |
| A refused first summary quotes the hub | implemented | `hooks/useEngineState.ts`, `utils/formatters.ts` (`toSummaryRefusal`) |
| The summary shows the hub's verdict, freshness, coverage and counts | implemented | `Drilldown/StateSummary.tsx`, `clusters:utils/formatters.ts` |
| Each freshness band has a fixed tone | implemented | `clusters:utils/formatters.ts` (`toFreshnessTone`) |
| Severity and freshness are printed as the hub states them | implemented | `Drilldown/StateSummary.tsx`, `Drilldown/EngineStateDrilldown.tsx` |
| Coverage always states what is unknown or missing | implemented | `clusters:utils/selectors.ts` (`coverageLabelsOf`), `clusters:utils/formatters.ts` (`toCoverageText`) |
| Finding counts list only the severities that occur, in severity order | implemented | `clusters:utils/selectors.ts` (`occurringSeveritiesOf`) |
| Missing finding counts are shown as dashes | implemented | `clusters:utils/formatters.ts` (`toFindingCountsText`) |
| A refused summary re-read keeps the last summary and says so | implemented | `hooks/useEngineState.ts`, `utils/formatters.ts` (`toSummaryRefusal`) |
| Certificate coverage by role is shown when the findings carry certificate observations | implemented | `utils/transforms.ts` (`toWalkedRoleCoverage`: present once a certificate observation was walked, even when none names a role), `Drilldown/StateSummary.tsx`, `Drilldown/RoleCoverageList.tsx` |
| The findings read follows the report the summary names | implemented | `hooks/useEngineState.ts`, `utils/selectors.ts` (`detailGenerationOf`) |
| A summary without retained detail leads to no findings read | implemented | `hooks/useEngineState.ts`, `hooks/useFindingsWalk.ts` |
| Findings walked earlier stay listed when the summary stops naming retained detail | implemented | `hooks/useRetained.ts`, `utils/formatters.ts` (`toPreviousReportNote`) |
| The findings header states the total and the state | implemented | `utils/formatters.ts` (`toFindingsHeader`) |
| A walk without a state shows a dash | implemented | `utils/formatters.ts` (`toFindingsHeader`) |
| An incomplete walk is labelled partial | implemented | `utils/formatters.ts` (`toPartialFindingsNote`) |
| A finding row states the finding as the hub stated it | implemented | `Drilldown/FindingRow.tsx`, `utils/formatters.ts` (`toFindingDetail`) |
| Missing parts of a finding are shown as placeholders or left out | implemented | `utils/formatters.ts` (`toFindingDetail`, `toSeverityText`) |
| Only a finding with an evidence reference offers its evidence | implemented | `utils/predicates.ts` (`hasEvidence`) |
| A finding shows the collection observation that matches its signal and subject | implemented | `utils/selectors.ts` (`observationFor`), `Drilldown/FindingRow.tsx`, `utils/formatters.ts` (`toReasonText`, `toProcessLine`, `toCollectionTimes`) |
| A finding without a matching observation shows no collection block | implemented | `Drilldown/FindingRow.tsx` |
| A walk with no findings lists nothing | implemented | `Drilldown/EngineStateDrilldown.tsx` |
| The filters offer what the walked findings contain | implemented | `Drilldown/FindingFilters.tsx`, `utils/selectors.ts` (`signalsOf`, `nodesOf`), `utils/transforms.ts` (`toOptions`) |
| A selected filter value stays offered when the narrowed findings no longer list it | implemented | `utils/selectors.ts` (`signalsOf`, `nodesOf`) |
| Changing a filter re-reads the findings from the first page with that filter | implemented | `Drilldown/EngineStateDrilldown.tsx`, `hooks/useFindingsWalk.ts`, `utils/transforms.ts` (`toFindingsQuery`) |
| A filter change abandons the walk still in flight | implemented | `hooks/useFindingsWalk.ts` (the filters are part of the query key; the request signal is cancelled) |
| Until the newly selected filters have loaded, the previous findings stay with a note | dropped | — |
| The retained-filters note distinguishes loading from failure | implemented | `utils/formatters.ts` (`toRetainedFiltersNote`), `utils/predicates.ts` (`isSameFilters`) |
| The retained-filters note names a retained minimum severity | implemented | `utils/formatters.ts` (`toRetainedFiltersNote`); the ending follows the `@fixed` scenario, not this scenario's literal "have not loaded successfully" |
| The note disappears once the selected filters load | implemented | `Drilldown/EngineStateDrilldown.tsx` |
| A refused findings read is explained in the hub's terms | implemented | `utils/formatters.ts` (`toFindingsRefusal`), `utils/transforms.ts` (`toRefusal`, which reads the retained window from the `x-stratum-retained-from` / `-to` headers of `ApiError`) |
| A changed report offers an explicit reload instead of a silent refetch | implemented | `Drilldown/EngineStateDrilldown.tsx`, `hooks/useEngineState.ts` (`onReread`) |
| Other refusals offer no reload control | implemented | `Drilldown/EngineStateDrilldown.tsx` |
| Findings from an older report than the summary are labelled | implemented | `utils/formatters.ts` (`toPreviousReportNote`) |
| With neither findings nor a refusal the findings area is empty | implemented | `Drilldown/EngineStateDrilldown.tsx` |
| An open drill-down is re-read whenever a new cluster list arrives | implemented | `hooks/useEngineState.ts`: per D9 the summary and the walk poll on the live cadence instead of being keyed to the cluster list; the evidence read never polls (`hooks/useEvidence.ts`). The capability record is `clusters`'. |
| The summary shown is the freshest any view of that cluster has read | implemented | `hooks/useEngineState.ts` (same generated hook and variables as the clusters card, one query cache entry) |
| A summary read overtaken by another view's read is retried, then the known summary is used | implemented | no code of its own: TanStack Query de-duplicates reads of one key, so a read is never overtaken and no retry loop exists; only the outcome holds |
| A refused summary re-read still re-walks the findings of the last known report | implemented | `hooks/useEngineState.ts`, `hooks/useFindingsWalk.ts` |
| The drill-down offers no action on the cluster | implemented | `Drilldown/*` (read-only hooks only) |

## engine-state-requests.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Engine-state reads are plain session reads | implemented | transport, generated `src/api/fleet` hooks |
| A read that takes longer than 15 seconds is given up as retryable | implemented | transport (15 s deadline, `ApiError.reason` `timeout`), `utils/transforms.ts` (`toRefusal`, `hasReason(error, 'timeout')`). The findings walk and the evidence read do not retry (`hooks/useFindingsWalk.ts`, `hooks/useEvidence.ts`: `retry: false`), so the refusal shows when the request is given up. |
| Each kind of answer becomes one message the views quote | implemented | `utils/transforms.ts` (`toRefusal`), `utils/selectors.ts` (`shapeFaultOf`: a success that is not a JSON object, for the findings walk and the evidence read), `hooks/useEngineState.ts` (the same for the summary). A refusal without a body and a success that cannot be parsed depend on what the transport hands over. |
| A JSON refusal is retryable only when the hub says so | implemented | `utils/transforms.ts` (`toRefusal`) |
| A refusal keeps the facts the hub attached to it | implemented | `utils/transforms.ts` (`toRefusal`) keeps status, code, request id, generation and the retained-window headers |
| A successful answer of the wrong shape is an error, never an empty result | implemented | `hooks/useFindingsWalk.ts`, `hooks/useEvidence.ts`, `utils/selectors.ts` (`shapeFaultOf`), `utils/predicates.ts` (`isReportDetail`, `isEvidencePage`). The capability-record row: `clusters:utils/selectors.ts` (`evidenceCapabilitiesOf`, `recordFailureOf`), `clusters:components/CapabilityAvailability/CapabilityAvailability.tsx`. |
| An expired session on an engine-state read does not open the sign-in screen | dropped | — |
| An expired session on an engine-state read opens the sign-in screen | implemented | transport (`onUnauthorized`) |
| A forbidden engine-state read is quoted like any other refusal | implemented | `utils/transforms.ts` (`toRefusal`) |
| Switching cluster drops every engine-state answer still in flight | implemented | query keys carry the cluster id; `Drilldown/EngineStateDrilldown.tsx` remounts per cluster; the retained walk is held per cluster (`hooks/useRetained.ts`, `utils/reducers.ts` `withRetained`), so `components/FindingsSurface/FindingsSurface.tsx` shows nothing of the previous cluster |
| A newer read of the same thing wins over an older one | implemented | TanStack Query (one fetch per key) |
| A failed read never erases the last good value | implemented | TanStack Query keeps data on a failed refetch; `hooks/useRetained.ts` across walk keys |
| Identifiers are escaped in engine-state request paths | implemented | transport (`toUrl`) |
| Findings are read page by page until the hub mints no further cursor | implemented | `hooks/useFindingsWalk.ts`, `utils/reducers.ts` (`withPage`) |
| Every page of a filtered walk repeats the filters | implemented | `utils/transforms.ts` (`toFindingsQuery`) |
| Empty filters are not sent | implemented | `utils/transforms.ts` (`toFindingsQuery`) |
| A page without a usable cursor ends the walk | implemented | `utils/reducers.ts` (`withPage`) |
| A page that fails mid-walk fails the whole walk | implemented | `hooks/useFindingsWalk.ts` (one query per walk) |
| The walk's totals are the last page's | implemented | `utils/reducers.ts` (`withPage`) |
| Missing totals on the last page fall back safely | implemented | `utils/reducers.ts` (`withPage`), `utils/formatters.ts` (`toFindingsHeader`) |
| An observation repeated on several pages is kept once | implemented | `utils/reducers.ts` (`withPage`), `utils/selectors.ts` (`observationKeyOf`) |
| A hub that keeps minting cursors keeps the walk going | dropped | — |
| A findings walk is bounded | implemented | `utils/reducers.ts` (`withPage`), `constants.ts` (`findingsPageLimit`) |
| Evidence is read as one fragment and never paged further | implemented | `hooks/useEvidence.ts` |

## engine-state-evidence.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Evidence opens a drawer for the finding's reference | implemented | `utils/reducers.ts` (`withOpenedFinding`), `hooks/useEvidence.ts`, `Drilldown/EvidenceDrawer.tsx` |
| The drawer says it is reading until the hub answers | implemented | `Drilldown/EvidenceDrawer.tsx` |
| Every finding that shares the open reference is marked open | implemented | `utils/predicates.ts` (`isEvidenceOpen`), `Drilldown/FindingRow.tsx` |
| Opening another finding's evidence starts the drawer clean | implemented | `utils/reducers.ts` (`withOpenedFinding`), `hooks/useEvidence.ts` (no cache kept per reference) |
| Clicking Evidence again on the open reference re-reads it without clearing | implemented | `Drilldown/EngineStateDrilldown.tsx`, `utils/predicates.ts` (`isSameEvidence`) |
| Re-opening the open reference after the report moved starts the drawer clean | implemented | `utils/reducers.ts` (`withOpenedFinding`) |
| A slower answer for a reference the user moved away from is dropped | implemented | `hooks/useEvidence.ts` (the reference is part of the query key) |
| An evidence answer that was overtaken shows nothing | blocked | no code: a read is never overtaken (a cluster switch remounts the drill-down and cancels the read), so the state cannot occur; candidate for `@drop` under D3 |
| The drawer has no close control of its own | implemented | `Drilldown/EvidenceDrawer.tsx` |
| A refused evidence read quotes the hub | implemented | `utils/formatters.ts` (`toEvidenceRefusal`) |
| A refused retry keeps the last good evidence beside the refusal | implemented | `hooks/useEvidence.ts`, `Drilldown/EvidenceDrawer.tsx` |
| A partial fragment is labelled with what was served | implemented | `utils/formatters.ts` (`toPartialEvidenceNote`) |
| Evidence from an older report is labelled | implemented | `utils/selectors.ts` (`currentGenerationOf`), `utils/formatters.ts` (`toPreviousEvidenceNote`) |
| A findings refresh does not rewrite an open drawer | implemented | `utils/reducers.ts` (snapshot in `OpenedEvidence`), `hooks/useEvidence.ts` |
| The source of the evidence is printed as the hub recorded it | implemented | `utils/formatters.ts` (`toSourceFacts`), `Drilldown/FactList.tsx` |
| Missing source facts are dashes | implemented | `utils/formatters.ts` (`toSourceFacts`) |
| Evidence without a source says the hub derived it | implemented | `Drilldown/EvidenceDrawer.tsx` |
| An item of a type without a dedicated view is shown raw | implemented | `utils/predicates.ts` (`isShownRaw`), `utils/formatters.ts` (`toRawItemTitle`) |
| An item without a type is shown raw with a dash | implemented | `utils/formatters.ts` (`toRawItemTitle`) |
| Evidence with no items shows only its source | implemented | `Drilldown/EvidenceDrawer.tsx` |
| A supervisor item shows its process and build | implemented | `Drilldown/SupervisorEvidence.tsx`, `utils/formatters.ts` (`toSupervisorFacts`) |
| Uptime is shown only when the evidence states an actual start | implemented | `utils/formatters.ts` (`toUptimeLine`) |
| Each supervised child states its desired and running state as recorded | implemented | `utils/formatters.ts` (`toChildLine`) |
| A child's ownership and a missing name are shown as recorded | implemented | `utils/formatters.ts` (`toChildLine`) |
| The runtime observation is labelled as such | implemented | `utils/formatters.ts` (`toRuntimeLine`) |
| A runtime observation's reason is quoted | implemented | `utils/formatters.ts` (`toRuntimeLine`, `toReasonText`) |
| The mirror-Pod observation is labelled as a Kubernetes observation | implemented | `utils/formatters.ts` (`toMirrorPodLine`) |
| Observations with missing facts use dashes | implemented | `utils/formatters.ts` (`toRuntimeLine`, `toMirrorPodLine`) |
| Absent observations are simply not shown | implemented | `Drilldown/SupervisorEvidence.tsx` |
| A certificate finding's evidence is shown in the certificate panel | implemented | duplicate of evidence-certificate "A certificate finding's evidence opens as the certificate panel": `utils/transforms.ts` (`toCertificateRows`) |
| Extra certificate fragments are shown without invented metadata | implemented | duplicate of the same scenario: `utils/transforms.ts` (`toCertificateRows`), `Drilldown/CertificateEvidencePanel.tsx` (no id, availability, outcome or sample line without an observation) |
| A backup finding's evidence is shown in the backup panel | implemented | duplicate of evidence-backup "A backup finding's evidence opens as backup panels": `Drilldown/EvidenceDrawer.tsx` |
| Another finding sharing the open evidence switches the severity shown | implemented | `utils/reducers.ts` (`withOpenedFinding`) |
| Typed evidence can always be inspected in full | implemented | duplicate of the "Full evidence" steps of the two evidence-* opening scenarios: `utils/predicates.ts` (`isTypedSignal`), `Drilldown/EvidenceDrawer.tsx` |
| Items of another type inside typed evidence stay visible | implemented | duplicate of the two "stay raw" scenarios: `utils/predicates.ts` (`isShownRaw`) |

## evidence-backup.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A backup finding's evidence opens as backup panels | implemented | `Drilldown/EvidenceDrawer.tsx`, `utils/selectors.ts` (`itemsOfType`), `utils/predicates.ts` (`isShownRaw`, `isTypedSignal`) |
| Backup fragments under any other finding stay raw | implemented | `utils/predicates.ts` (`isShownRaw`) |
| The verdict shown is the report's published outcome | implemented | `utils/formatters.ts` (`toVerdictWord`) |
| A published outcome the dashboard does not know is shown verbatim | implemented | `utils/formatters.ts` (`toVerdictWord`) |
| The verdict is explained only when the evidence itself supports the same word | implemented | `Drilldown/BackupEvidencePanel.tsx` |
| The verdict is styled by the finding's severity | implemented | `Drilldown/BackupEvidencePanel.tsx`, `clusters:utils/formatters.ts` (`toSeverityTone`) |
| The explanation is the panel's own reading of the fragment | implemented | `utils/transforms.ts` (`toBackupReading`) |
| The panel's own reading asks its questions in a fixed order | implemented | `utils/transforms.ts` (`toBackupReading`) |
| Coverage problems decide between critical and partial | implemented | `utils/transforms.ts` (`toBackupReading`) |
| Every coverage problem is listed | implemented | `utils/transforms.ts` (`toBackupReading`) |
| What counts as a copy's success, failure and age | implemented | `utils/selectors.ts` (`copySuccessTimeOf`), `utils/predicates.ts` (`isAttemptFailed`, `isWithinSkew`), `utils/transforms.ts` |
| A policy that requires no location is satisfied by an empty set of backups | implemented | `utils/transforms.ts` (`toBackupReading`) |
| A panel given no published outcome would show its own verdict | dropped | — |
| The policy is printed as published, with unknowns named | implemented | `utils/formatters.ts` (`toPolicyLines`) |
| An absent policy prints a single line | implemented | `utils/formatters.ts` (`toPolicyLines`) |
| Durations are shown floored in one coarse unit | implemented | `utils/formatters.ts` (`toCoarseDuration`) |
| The listing line states how complete the storage listing was | implemented | `utils/formatters.ts` (`toListingLine`) |
| A fragment without a listing shows no listing line | implemented | `Drilldown/BackupEvidencePanel.tsx` |
| Every warning the evidence carries is shown | implemented | `utils/formatters.ts` (`toWarningText`), `Drilldown/BackupEvidencePanel.tsx` |
| Each distinct backup is shown once | implemented | `utils/transforms.ts` (`toDistinctBackups`), `utils/formatters.ts` (`toBackupCaption`) |
| Each copy of a backup is shown once with its location, kind and age | implemented | `utils/transforms.ts` (`toDistinctBackups`), `Drilldown/BackupEvidencePanel.tsx` |
| A copy's age is measured from its own last success | implemented | `utils/formatters.ts` (`toCopyAge`) |
| A copy's save and upload attempts are shown as recorded | implemented | `utils/formatters.ts` (`toAttemptLine`) |
| A copy without an attempt record shows no line for it | implemented | `Drilldown/BackupEvidencePanel.tsx` |
| Duplicate observations of one copy are reconciled into one | implemented | `utils/transforms.ts` (`toReconciledAttempt`), `utils/predicates.ts` (`isLaterAttempt`, `isLaterSuccess`) |
| Observations with no attempt time are ordered by completion, then failure, then outcome word | implemented | `utils/predicates.ts` (`isLaterAttempt`) |
| The prune attempt is shown when the evidence carries one | implemented | `utils/formatters.ts` (`toAttemptLine`), `Drilldown/BackupEvidencePanel.tsx` |
| An empty fragment still renders | implemented | `Drilldown/BackupEvidencePanel.tsx` |
| The backup panel is read-only for every user | implemented | `Drilldown/BackupEvidencePanel.tsx` (no control, no request) |

## evidence-certificate.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A certificate finding's evidence opens as the certificate panel | implemented | `utils/reducers.ts` (`withOpenedFinding` snapshot), `utils/transforms.ts` (`toCertificateRows`), `Drilldown/EvidenceDrawer.tsx` |
| Certificate fragments under any other finding stay raw | implemented | `utils/predicates.ts` (`isShownRaw`), `utils/transforms.ts` (`toCertificateRows`) |
| The panel is headed as a read-only record | implemented | `Drilldown/CertificateEvidencePanel.tsx`, `constants.ts` |
| A panel with no observations says so | dropped | — |
| A certificate finding with no observation and no certificate fragment shows no certificate panel | implemented | `utils/transforms.ts` (`toCertificateRows`), `Drilldown/EvidenceDrawer.tsx` |
| Coverage is summed per certificate role | implemented | `utils/transforms.ts` (`toRoleCoverage`), `utils/formatters.ts` (`toRoleCoverageText`), `Drilldown/RoleCoverageList.tsx` |
| Coverage tolerates incomplete observations | implemented | `utils/transforms.ts` (`toRoleCoverage`) |
| Each observation is shown as its own certificate block | implemented | `Drilldown/CertificateEvidencePanel.tsx`, `utils/formatters.ts` (`toCertificateFacts`, `toObservationFacts`) |
| An observation without certificate evidence shows dashes | implemented | `utils/formatters.ts` (`toCertificateFacts`) |
| Facts a read learned are shown only when the read stood | implemented | `utils/predicates.ts` (`isReadStanding`), `utils/formatters.ts` (`toCertificateFacts`) |
| The expiry line is banded by the exact time remaining | implemented | `utils/formatters.ts` (`toExpiryBand`, `toExpiryLine`, `toExpiryTone`, `toRemaining`) |
| Times are shown in the browser's locale | implemented | `src/shared/utils/formatters.ts` (`toLocalTime`) |
| No expiry line is invented | implemented | `utils/formatters.ts` (`toExpiryLine`) |
| Each block states how old its sample is | implemented | `utils/formatters.ts` (`toSampleLine`), `src/shared/utils/formatters.ts` (`toAgeAgo`) |
| A renewal record is shown as recorded | implemented | `utils/formatters.ts` (`toRenewalFacts`) |
| A missing renewal record is unknown, never success | implemented | `Drilldown/CertificateEvidencePanel.tsx`, `constants.ts` |
| Every degrading cause is shown as its own marked line | implemented | `utils/transforms.ts` (`toCertificateCauses`) |
| A cause quotes the record's own reason | implemented | `utils/formatters.ts` (`toCauseLine`) |
| Read and renewal causes quote different reasons | implemented | `utils/transforms.ts` (`toCertificateCauses`) |
| Causes are never collapsed into a single severity | implemented | `Drilldown/CertificateEvidencePanel.tsx` (no overall badge) |
| The certificate panel is read-only for every user | implemented | `Drilldown/CertificateEvidencePanel.tsx` (no control, no request) |

## Overlap with `data/engine-state-load.feature`

The loader scenarios listed under "Overlap with hardening" in
`spec/data/NOTES.md` describe the same requests. The sequencing exists once,
here: the page-by-page walk, the 100-page bound, the repeated-cursor stop,
the partial label, the once-kept observation and the wrong-shape error are
`hooks/useFindingsWalk.ts` + `utils/reducers.ts`; refusals are
`utils/transforms.ts` (`toRefusal`). Not carried over from the loader: the
10-second per-request and 15-second per-load budgets (D8's single 15-second
deadline per request applies), the automatic re-read of a moved generation
(the drill-down offers "Reload findings" and follows the summary on the next
poll), the liveness-probe ordering (D9), the console note and the
version-signal-only walk.
