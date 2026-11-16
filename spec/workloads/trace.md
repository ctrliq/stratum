# workloads — trace

Paths are relative to `dash-next/`. `W` stands for `src/domain/workloads`. The generic list,
detail panel, scale form, confirmations and notices are `src/domain/resources`
(`ResourceBrowser`, `ResourceDetail`, `useResourceBrowser`); rows below name the workloads
code that feeds them.

## kinds.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Each workload kind has its own list | implemented | W/constants.ts (kinds), W/pages/{Pods,Deployments,StatefulSets,DaemonSets,ReplicaSets,ReplicationControllers,Jobs,CronJobs}List/index.tsx, src/routes/c.$cluster.{pods,deployments,statefulsets,daemonsets,replicasets,replicationcontrollers,jobs,cronjobs}.tsx |
| Each workload kind shows its columns in order | implemented | W/constants.ts (columns) |
| A workload column shows a value read from the object | implemented | W/utils/selectors.ts, W/utils/transforms.ts (to…Cells), W/utils/formatters.ts (toSuspendText) |
| A pod with no reported containers counts its declared containers | implemented | W/utils/selectors.ts (readyCountOfPod), W/utils/transforms.ts (toPodCells) |
| Job completions default to one wanted completion | implemented | W/utils/selectors.ts (readyCountOfJob), W/utils/transforms.ts (toJobCells) |
| Job status is derived from the job's pod counters | implemented | W/utils/selectors.ts (statusForJob), src/shared/components/ToneBadge/StatusCell.tsx |
| Pod status ranks deletion, then stuck containers, then phase | implemented | W/utils/selectors.ts (statusForPod), src/shared/components/ToneBadge/StatusCell.tsx |
| A pod the scheduler cannot place reads as Pending | dropped | — |
| A pod the scheduler cannot place reads as Unschedulable | implemented | W/utils/predicates.ts (isUnschedulable), W/utils/selectors.ts (statusForPod), src/shared/components/ToneBadge/StatusCell.tsx |
| Deployment Ready is read from the deployment in the list | implemented | W/utils/selectors.ts (readyCountOfDeployment), W/utils/transforms.ts (toDeploymentCells) |
| Deployment Ready is read from the dashboard's cluster snapshot | dropped | — |
| A deployment the snapshot does not know shows dashes | dropped | — |
| Pausing auto-refresh freezes Ready and Available but not the rest of the row | dropped | — |
| Deployment Available is read from the deployment in the list | implemented | W/utils/selectors.ts (availableOf), W/utils/transforms.ts (toDeploymentCells) |
| Deployment Available is read from the dashboard's cluster snapshot | dropped | — |
| Kind-specific actions offered in the detail panel | implemented | W/pages/*List/index.tsx (`actions`, `writes.scale`, `writes.onRestart`) |
| Replication controllers can be created, edited, scaled and deleted | implemented | W/pages/ReplicationControllersList/index.tsx |
| Replication controllers are read-only | implemented | W/pages/ReplicationControllersList/index.tsx (`isReadOnly` on the hub) |

## overview.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The overview is the first row of the Workloads section | implemented | W/pages/WorkloadsOverview/index.tsx, src/routes/c.$cluster.workloads.tsx, src/domain/shell/hooks/useSidebarCounts.ts (the `workloads` count) |
| Pods, replica sets and replication controllers are not part of the overview | implemented | W/hooks/useWorkloadSnapshot.ts, W/constants.ts (overviewKinds) |
| The overview's columns | implemented | W/pages/WorkloadsOverview/index.tsx, parts/NameCell.tsx, src/shared/components/ToneBadge/StatusCell.tsx |
| CPU is shown in millicores or cores | implemented | W/utils/formatters.ts (toCpuText) |
| Memory is shown in Mi or GiB | implemented | W/utils/formatters.ts (toMemoryText) |
| Restarts default to zero | implemented | W/pages/WorkloadsOverview/index.tsx (restarts column) |
| A Ready figure knows when it was observed | implemented | W/pages/WorkloadsOverview/parts/ReadyCell.tsx, W/utils/formatters.ts (toObservedText), W/hooks/useWorkloadSnapshot.ts (hub facts) |
| The overview follows the selected namespace | implemented | W/utils/filters.ts (visibleUnder) |
| A kind filter narrows to one kind | implemented | W/pages/WorkloadsOverview/parts/KindFilter.tsx, W/utils/filters.ts (visibleUnder) |
| Each kind filter shows how many workloads of that kind exist cluster-wide | implemented | W/utils/selectors.ts (countsByKind), W/utils/formatters.ts (toKindLabel), parts/KindFilter.tsx |
| Search matches name, namespace, kind or image | implemented | W/utils/predicates.ts (matchesWorkloadSearch) |
| Namespace, kind and search narrow together | implemented | W/utils/filters.ts (visibleUnder) |
| The kind filter and search are forgotten on leaving | implemented | W/pages/WorkloadsOverview/index.tsx (component state) |
| The line under the title counts the rows | implemented | W/utils/formatters.ts (toWorkloadsSummary) |
| The line flags degraded workloads cluster-wide | implemented | W/utils/predicates.ts (isDegraded), W/utils/selectors.ts (degradedCountOf), W/utils/formatters.ts (toDegradedFlag), WorkloadsOverview.module.scss |
| An empty result says what was looked for | implemented | W/utils/formatters.ts (toEmptyWorkloadsMessage) |
| While the first snapshot is loading the overview looks empty | dropped | — |
| While workloads are first loading the overview says it is loading | implemented | W/utils/predicates.ts (isFirstLoad, isWhollyRead), W/pages/WorkloadsOverview/index.tsx |
| The overview cannot tell an unreadable cluster from an empty one | dropped | — |
| The overview says when workloads could not be read | implemented | W/utils/predicates.ts (isUnreadable, isWhollyRead), W/utils/selectors.ts (unreadableAmong), W/utils/formatters.ts (toUnreadableNotice), W/utils/transforms.ts (toKindRead) |
| The overview is as fresh as the snapshot | implemented | W/hooks/useWorkloadSnapshot.ts, W/hooks/usePodUsage.ts (`polledWhen(isLive)`) |
| Clicking a workload opens it in its kind's list | implemented | W/utils/transforms.ts (toWorkloadHref), W/pages/WorkloadsOverview/index.tsx; the list opens the panel through `linkedName` |
| A workload of an unrecognised kind opens the Deployments list | implemented | W/utils/selectors.ts (segmentOfKind) |
| Overview rows respond to the pointer only | implemented | W/pages/WorkloadsOverview/index.tsx (row click, no focusable control) |
| The overview itself offers no write action | implemented | W/pages/WorkloadsOverview/index.tsx |

## pod-actions.feature

The bottom drawer is `src/domain/terminal`; this domain hands a pod to it through
`useTerminal()`.

| Scenario | Status | Implemented in |
|---|---|---|
| Pods offer Logs and Exec on the row and in the detail panel | implemented | W/pages/PodsList/index.tsx (`actions`) |
| Without write controls a pod offers Logs but not Exec | implemented | W/pages/PodsList/index.tsx (`isWrite`) |
| Logs opens the pod's log view in the bottom drawer | implemented | W/pages/PodsList/index.tsx, W/utils/selectors.ts (targetOfPod); the tab is src/domain/terminal |
| Exec opens the pod's shell view in the bottom drawer | implemented | W/pages/PodsList/index.tsx; the tab is src/domain/terminal |
| A pod has one drawer tab however often it is opened | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withPodOpened`, `withModeChosen`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (mounted on the `/c/$cluster` layout route) |
| A pod object without a namespace or a name opens nothing | implemented | W/utils/selectors.ts (targetOfPod is absent), src/domain/terminal (`onLogsOpened(undefined)`) |
| Opening logs or a shell leaves the list as it was | implemented | W/pages/PodsList/index.tsx (actions change no list state) |
| Drawer tabs opened from the Pods list survive navigation | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withPodOpened`, `withModeChosen`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (mounted on the `/c/$cluster` layout route) |
| Switching cluster closes every pod tab | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withScope`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |

## restart.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Restart is offered for rolling workloads only | implemented | W/pages/{Deployments,StatefulSets,DaemonSets}List/index.tsx (`writes.onRestart`) |
| Restart asks for confirmation | implemented | src/domain/resources (ResourceBrowser) |
| Cancelling the confirmation restarts nothing | implemented | src/domain/resources (ResourceBrowser) |
| Confirming stamps the pod template so the workload rolls | implemented | W/utils/transforms.ts (toRestartPatch), W/constants.ts (restartedAtAnnotation), W/pages/{Deployments,StatefulSets,DaemonSets}List/index.tsx |
| A refused restart is reported | implemented | src/domain/resources (useResourceBrowser) |
| The dashboard does not track the rollout it triggered | implemented | nothing follows the patch |

## scale.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Scale is offered for the replica-count workloads, replication controllers included | implemented | W/pages/{Deployments,StatefulSets,ReplicaSets,ReplicationControllers}List/index.tsx (`writes.scale`) |
| Scale is offered for deployments, stateful sets and replica sets only | implemented | W/pages/ReplicationControllersList/index.tsx (`isReadOnly` on the hub) |
| Kinds without a replica count are never offered Scale | implemented | W/pages/{Pods,DaemonSets,Jobs,CronJobs}List/index.tsx (no `writes.scale`) |
| Scale swaps the manifest for a replica count | implemented | `replicasOf` in the four pages above; src/domain/resources (ResourceDetail) |
| The prefilled count is the one seen when the row was clicked | implemented | src/domain/resources (ResourceDetail) |
| The prefilled count is the first clicked workload's when the panel was left open | dropped | — |
| Cancel leaves the workload untouched | implemented | src/domain/resources (ResourceDetail) |
| A valid count scales the workload and closes the panel | implemented | W/utils/transforms.ts (toScalePatch), the four pages above; src/domain/resources (useResourceBrowser) |
| A refused scale is reported and the panel closes anyway | implemented | src/domain/resources (useResourceBrowser) |
| Only a whole count the cluster can hold may be submitted | implemented | src/domain/resources (scaleSchema, isReplicaCount) |
| Any entry is coerced to a count and submitted | dropped | — |
| A count that is not valid is refused with a notice if it is ever submitted | implemented | src/domain/resources/components/ResourceDetail.tsx (onSubmitInvalid), src/domain/resources/hooks/useResourceBrowser.ts (onScaled), src/domain/resources/utils/formatters.ts (toInvalidScaleNotice) |
| Scale cannot be pressed twice | implemented | src/domain/resources (ResourceDetail `isWriting`) |
