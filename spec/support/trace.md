# support — trace

## evidence-archive.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Each cluster of a fleet offers the archive control to admins | implemented | src/domain/support/components/EvidenceArchiveControl/EvidenceArchiveControl.tsx (placed by the `clusters` domain) |
| The single cluster of a standalone hub offers the archive control to admins | dropped | D37: a standalone hub has no Clusters page, so nothing mounts the control there (the scenario carries no `@drop` tag) |
| A read-only user is not offered the archive control | implemented | src/domain/support/components/EvidenceArchiveControl/EvidenceArchiveControl.tsx, src/domain/support/utils/predicates.ts |
| The control is enabled only for a cluster whose evidence can be read | implemented | src/domain/support/components/EvidenceArchiveControl/EvidenceArchiveControl.tsx, src/domain/support/utils/predicates.ts, src/domain/support/utils/formatters.ts |
| One click starts a collection without confirmation | implemented | src/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows.tsx, src/domain/support/utils/reducers.ts |
| The start request carries an empty JSON object | dropped | D34 |
| A collection that cannot be started is reported | implemented | src/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows.tsx (90 s `deadlineMs`), src/domain/support/utils/selectors.ts, src/domain/support/utils/formatters.ts |
| A start that names no collection is reported | implemented | src/domain/support/utils/parsers.ts, src/domain/support/utils/selectors.ts |
| The collection is polled until it has an archive or an outcome | dropped | — |
| The collection is polled with the common request deadline | implemented | src/domain/support/components/EvidenceArchiveFlows/ArchiveFlowRunner.tsx, src/domain/support/utils/predicates.ts |
| A collection that never leaves pending keeps the control disabled indefinitely | dropped | — |
| A collection that never leaves pending is given up | implemented | src/domain/support/utils/predicates.ts, src/domain/support/utils/selectors.ts (2 minutes from the hub's acceptance) |
| A refused status read is retried like a dropped one | dropped | — |
| A refused status read ends the flow at once | implemented | src/domain/support/utils/predicates.ts, src/domain/support/utils/selectors.ts |
| Failed status reads are retried with growing delays and then given up | implemented | src/domain/support/utils/predicates.ts, src/domain/support/utils/selectors.ts, src/domain/support/components/EvidenceArchiveFlows/ArchiveFlowRunner.tsx |
| A collection that ends without an archive is reported as failed | implemented | src/domain/support/components/EvidenceArchiveFlows/ArchiveFlowRunner.tsx, src/domain/support/utils/selectors.ts |
| A finished collection is downloaded once and saved | implemented | src/domain/support/components/EvidenceArchiveFlows/ArchiveFlowRunner.tsx, src/domain/support/utils/selectors.ts |
| A partial archive names every section that is not complete | implemented | src/domain/support/utils/formatters.ts |
| The saved file takes the name the hub gives it | implemented | src/shared/fetchers/sameOriginFetch.ts (a named download resolves to a `File`), src/domain/support/utils/formatters.ts (toArchiveFilename), src/domain/support/utils/selectors.ts |
| An extended-form filename is ignored and the fallback name is used | implemented | src/shared/fetchers/requests.ts (`fileNameIn`, D40: the transport decides which form names the file), src/domain/support/utils/formatters.ts (`toArchiveFilename` falls back to the collection id) |
| A download that fails is reported and nothing is saved | implemented | src/domain/support/components/EvidenceArchiveFlows/ArchiveFlowRunner.tsx (60 s `deadlineMs`), src/domain/support/utils/selectors.ts |
| An expired session during the flow sends the user to sign-in | implemented | src/shared/fetchers/sameOriginFetch.ts |
| An unauthorized answer also follows the normal failure path | dropped | — |
| An unauthorized answer ends the flow silently | implemented | src/domain/support/utils/selectors.ts, src/domain/support/utils/predicates.ts |
| Only the start carries the anti-forgery token | implemented | src/shared/fetchers/requests.ts |
| An abandoned flow ends silently | implemented | src/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows.tsx (each start carries its own abort signal, aborted when its flow is gone or the page is left), src/domain/support/utils/selectors.ts (`abandonedKeysOf`), src/domain/support/utils/reducers.ts (answers are matched by flow key); status reads and the download are aborted with their queries |
| Notifications name the cluster as it was when the flow started | implemented | src/domain/support/utils/reducers.ts |
| Flows of different clusters are independent | implemented | src/domain/support/utils/reducers.ts, src/domain/support/utils/selectors.ts |
| Switching layout or filtering the cluster out abandons a running archive flow silently | dropped | — |
| A running archive flow survives layout and search changes | implemented | src/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows.tsx, mounted once around both layouts by src/domain/clusters/pages/ClustersList/index.tsx |
