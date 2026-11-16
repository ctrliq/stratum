# data — trace

Paths are relative to `dash-next/`. Written by implement-spec; statuses are `implemented`, `dropped` (tagged `@drop`) or `blocked` (with the reason). Every `utils/*.ts` named here has its `.test.ts` beside it.

## quantities-and-age.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A timestamp is shown as a rounded relative age in one unit | implemented | src/shared/utils/formatters.ts (toAge) |
| A missing timestamp has no age | implemented | src/shared/utils/formatters.ts (toAge) |
| A timestamp in the future reads as zero seconds | implemented | src/shared/utils/formatters.ts (toAge) |
| Ages advance only when the data is reloaded | implemented | src/domain/resources/utils/transforms.ts (toResourceRow takes the time of the last answer) |
| A CPU quantity is converted to millicores | implemented | src/shared/utils/parsers.ts |
| A memory quantity is converted to mebibytes | implemented | src/shared/utils/parsers.ts |
| Memory forms outside the short unit table are read as bytes or rejected | dropped | — |
| Memory accepts every apiserver quantity form and rejects unknown units | implemented | src/shared/utils/parsers.ts |
| Kubelet usage figures are converted to millicores and mebibytes | implemented | src/shared/utils/formatters.ts (toMillicores, toMebibytes) |
| A pod's requests are the sum over its regular containers | implemented | src/domain/workloads/utils/selectors.ts (requestsOf) |

## derived-pods.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A pod's status is the first rule that matches | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| The first waiting container with a recognised reason decides | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A crash-looping init container does not change the status | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod the scheduler cannot place is Unschedulable | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod the scheduler cannot place is Pending | dropped | — |
| Unschedulable applies only while the pod is Pending | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod's ready count is ready containers over total containers | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod's restarts are the sum over its containers | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod shows measured usage when its kubelet reported it | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod falls back to its requests when no usage was reported for it | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod the kubelet lists without figures falls back to its requests | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |
| A pod the kubelet lists without figures shows zero usage | dropped | — |
| Missing pod details are shown as a dash | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/predicates.ts, src/domain/workloads/utils/transforms.ts |

## derived-workloads.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A Deployment's ready count is ready replicas over desired replicas | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A Deployment's status follows its available replicas alone | dropped | — |
| Without hub facts a Deployment's status needs replicas both ready and available | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| With hub facts the hub's numbers and status are shown for a Deployment | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| With hub facts a Deployment the hub does not list has no verdict | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| StatefulSets and DaemonSets take their status from ready over desired | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A DaemonSet's desired count is the number of nodes it should run on | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A Job's status comes from its succeeded, failed and active counters | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A CronJob is always Scheduled and carries its schedule | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A workload shows the image of its first container | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A CronJob's image is read from its job template | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |
| A workload's usage is the sum over pods in its namespace whose name starts with its name | implemented | src/domain/workloads/utils/predicates.ts (isPodOf…), src/domain/workloads/utils/filters.ts, src/domain/workloads/utils/selectors.ts (measuredUsageOf) |
| A pod named exactly like the workload counts towards it | implemented | src/domain/workloads/utils/predicates.ts (isPodOf…), src/domain/workloads/utils/filters.ts, src/domain/workloads/utils/selectors.ts (measuredUsageOf) |
| Workloads whose names share a prefix also share usage | dropped | — |
| A pod's usage counts only towards the workload it belongs to | implemented | src/domain/workloads/utils/predicates.ts (isPodOf…), src/domain/workloads/utils/filters.ts, src/domain/workloads/utils/selectors.ts (measuredUsageOf) |
| Workload rows carry no restart count | dropped | — |
| A workload row never states zero restarts for pods that restarted | implemented | src/domain/workloads/utils/selectors.ts (restartsAcross), src/domain/workloads/utils/transforms.ts |
| Workload rows are ordered by kind | implemented | src/domain/workloads/utils/selectors.ts, src/domain/workloads/utils/transforms.ts |

## derived-nodes.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A node's role comes from its labels | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| The legacy master label counts and the Stratum label must say true | implemented | src/domain/nodes/utils/predicates.ts (the Stratum label must say "true"; the master-label row is not carried, see NOTES divergence 18) |
| The legacy master label is ignored and the Stratum label counts by presence | dropped | — |
| A node is ready or not, with cordon shown in its status | dropped | — |
| Without hub facts a node is Ready, NotReady or Unknown, and cordon is separate | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| With hub facts the hub's statement decides a node's readiness | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| A node's CPU and memory percentages are usage over allocatable, capped at 100 | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| Without a usage summary a node's percentages come from pod requests | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| Finished pods do not count towards a node | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| A node that reports no allocatable figure shows zero percent | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| A node's pod capacity is its allocatable pod count, defaulting to 110 | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| Node details are read off the node object and shown as a dash when absent | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| The instance type falls back to the beta label | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| Conditions, labels and taints are carried as the node states them | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| Extended resources are listed as accelerators | implemented | src/domain/nodes/utils/selectors.ts, src/domain/nodes/utils/predicates.ts, src/domain/nodes/utils/transforms.ts, src/domain/nodes/utils/filters.ts |
| Pressure conditions name the nodes on which they are true | implemented | src/domain/nodes/utils/transforms.ts (toPressureConditions) |
| A node that does not report a condition is counted as unknown, not as healthy | implemented | src/domain/nodes/utils/transforms.ts (toPressureConditions) |
| A node that does not report a condition is indistinguishable from a healthy one | dropped | — |
| A node drifts when its stamped version differs from its kubelet's upstream version | dropped | — |
| A node drifts when its stamped and kubelet versions differ after normalisation | implemented | src/domain/nodes/utils/predicates.ts (`isRowDrifted`), src/domain/nodes/utils/formatters.ts (`toNormalVersion`) |
| With hub facts the cluster-level drift verdict is the hub's | implemented | src/domain/nodes/utils/predicates.ts (`isDriftReported`, `isClusterDriftedInFacts`), src/domain/nodes/hooks/useClusterDrift.ts, src/domain/nodes/pages/NodesList/index.tsx ("Cluster version drift" badge under the title) |
| Without hub facts the cluster drifts on a stamp mismatch or a kubelet minor skew | implemented | src/domain/nodes/utils/predicates.ts (`isDriftReported`, `isClusterDrifted`), src/domain/nodes/hooks/useClusterDrift.ts, src/domain/nodes/pages/NodesList/index.tsx |

## derived-cluster.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Cluster usage totals are measured usage over allocatable capacity | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| Cluster percentages are capped at 100 and absent without capacity | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| Cluster totals need a usage sample from every node | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| Cluster totals cover only the nodes whose kubelet answered | dropped | — |
| With no usage sample at all the totals keep their previous value | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| A cluster that answers with no nodes has empty totals and an empty history | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| The usage history keeps the last 24 samples | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| A sample with no percentage adds nothing to the history | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| The usage history lives only in the open page | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| The usage history starts empty on every cluster switch | implemented | src/domain/overview/utils/transforms.ts, src/domain/overview/utils/reducers.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/constants.ts |
| Events are the 50 most recently seen, newest first | implemented | src/domain/events/utils/transforms.ts, src/domain/events/utils/selectors.ts |
| An event's last-seen time is its best available timestamp | implemented | src/domain/events/utils/transforms.ts, src/domain/events/utils/selectors.ts |
| An event row carries type, reason, object, namespace, message, count and age | implemented | src/domain/events/utils/transforms.ts, src/domain/events/utils/selectors.ts |
| Missing event fields get defaults | implemented | src/domain/events/utils/transforms.ts, src/domain/events/utils/selectors.ts |
| An event's namespace falls back to the namespace of the object it is about | implemented | src/domain/events/utils/transforms.ts, src/domain/events/utils/selectors.ts |
| Storage class rows apply the Kubernetes defaults | implemented | src/domain/storage/utils/transforms.ts, src/domain/storage/utils/predicates.ts |
| The default storage class is the one annotated as default | implemented | src/domain/storage/utils/transforms.ts, src/domain/storage/utils/predicates.ts |
| One Helm release is shown per namespace and name, at its highest revision | implemented | src/domain/helm/utils/transforms.ts, src/domain/helm/utils/filters.ts, src/domain/helm/utils/selectors.ts, src/domain/helm/utils/parsers.ts |
| Helm release details the browser cannot read are shown as a dash | implemented | src/domain/helm/utils/transforms.ts, src/domain/helm/utils/filters.ts, src/domain/helm/utils/selectors.ts, src/domain/helm/utils/parsers.ts |
| The namespace choices are "all-namespaces" followed by the cluster's namespaces | implemented | src/domain/namespaces/utils/transforms.ts |
| The Kubernetes version drops its leading "v" | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| The CNI is named only when a Cilium DaemonSet exists | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| Static control-plane pods are the mirror pods in kube-system | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| A pod is a mirror pod by either annotation | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| Only pods in the Running phase count as running | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| kube-vip pods are any kube-system pods with kube-vip in their name | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| The default storage class name is recorded with the cluster facts | implemented | src/domain/overview/utils/selectors.ts |
| The operator is active while its Lease has not expired | dropped | — |
| Without hub facts the operator's state allows twice the Lease duration and needs a holder | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| Without hub facts a Lease that could not be read has its own state | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| With hub facts the operator's state is the hub's verdict | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| The cluster's identity and health are built from its own apiserver | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| The cluster's node counts can be fresher than its node rows | implemented | src/domain/overview/utils/selectors.ts, src/domain/overview/utils/predicates.ts, src/domain/overview/utils/filters.ts, src/domain/overview/utils/transforms.ts |
| The cluster's identity and health come from the hub's cluster list | implemented | src/domain/clusters/utils/selectors.ts |
| Without hub facts a kubelet-serving CSR is pending only when nothing decided it | implemented | src/domain/hardening/utils/predicates.ts, src/domain/hardening/utils/filters.ts, src/domain/hardening/utils/selectors.ts, src/domain/hardening/utils/transforms.ts |
| Requests for other signers are ignored | implemented | src/domain/hardening/utils/predicates.ts, src/domain/hardening/utils/filters.ts, src/domain/hardening/utils/selectors.ts, src/domain/hardening/utils/transforms.ts |
| An unread CSR list has no pending count | implemented | src/domain/hardening/utils/predicates.ts, src/domain/hardening/utils/filters.ts, src/domain/hardening/utils/selectors.ts, src/domain/hardening/utils/transforms.ts |
| With hub facts the pending count and availability are the hub's | implemented | src/domain/hardening/utils/predicates.ts, src/domain/hardening/utils/filters.ts, src/domain/hardening/utils/selectors.ts, src/domain/hardening/utils/transforms.ts |
| Restricted namespaces are those enforcing the restricted pod security level | implemented | src/domain/hardening/utils/predicates.ts, src/domain/hardening/utils/filters.ts, src/domain/hardening/utils/selectors.ts, src/domain/hardening/utils/transforms.ts |
| The time the facts were observed is the hub's, or absent | implemented | src/domain/hardening/utils/predicates.ts, src/domain/hardening/utils/filters.ts, src/domain/hardening/utils/selectors.ts, src/domain/hardening/utils/transforms.ts |
| Without hub facts the collection is complete only when six reads were whole | dropped | — |
| With hub facts completeness and the node total are the hub's statements | dropped | — |

## transport.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Requests go to the proxy prefix the server configuration names | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| The proxy prefix defaults to /k8s when the configuration names none | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| The proxy prefix is resolved again for every request | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A standalone hub serves the one cluster at the configured prefix | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A fleet hub serves each cluster under its own proxy path | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| Requests not tied to a screen's cluster follow the current selection | blocked | D11: every cluster screen is under /c/$cluster; the layout route provides ClusterContext |
| Requests started for one cluster stay on that cluster after a switch | implemented | src/api/k8s/k8sContext.ts (cluster captured in the hook variables) |
| Every request carries the session cookie and asks for JSON | implemented | dash-next/src/shared/fetchers/requests.ts (toHeaders, toInit) |
| A read may ask the apiserver for a different representation | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| Writes carry the CSRF token in the X-Stratum-CSRF header | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| Writes carry the CSRF token in the X-Fleet-CSRF header | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| Reads never carry the CSRF token | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A write without a known CSRF token is still sent, without the header | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A PATCH is sent under the content type of its patch kind | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A PATCH with no stated kind is a merge patch | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A create or replace sends its body as JSON | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A request without a body carries no content type | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A redirected write is refused, not followed | implemented | dash-next/src/shared/fetchers/requests.ts (redirectedWrite), dash-next/src/shared/fetchers/sameOriginFetch.ts |
| A redirected write is followed by the browser | dropped | — |
| A write that cannot reach the server reports a failure instead of crashing | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A refused request's error message comes from the best available source | implemented | src/shared/utils/formatters.ts (toErrorMessage) |
| A successful write with an empty or non-JSON body still succeeds | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A successful read that carries no JSON is a failed read | implemented | dash-next/src/api/k8s/k8sFetcher.ts, dash-next/src/shared/fetchers/requests.ts (isJsonAnswer, noJsonAnswer) |
| A successful read that cannot be decoded counts as an outage | dropped | — |
| A cleanup request may be marked to outlive the page | blocked | no keepalive option in the transport; needed only by the node-shell cleanup (nodes/terminal domain) |
| A response whose body cannot be read keeps its HTTP status | implemented | dash-next/src/shared/fetchers/requests.ts (toUnreadableError), dash-next/src/shared/fetchers/sameOriginFetch.ts |
| A list read that fails for any reason yields "no answer" and a console warning | implemented | dash-next/src/shared/query/loadState.ts, dash-next/src/domain/shell/hooks/useReadWarnings.ts |
| A failed list read yields the fallback and a warning that names the path only for an HTTP error | dropped | — |
| Every non-streaming request is abandoned after 15 seconds by default | blocked | owned by src/shared/streaming and the terminal domain; not built in this pass |
| A request has a deadline only when its caller states one | dropped | — |
| A caller's deadline replaces the default | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A caller's cancellation and the deadline bound the same request | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| A request whose caller already cancelled is aborted at once | implemented | src/shared/fetchers/requests.ts, src/shared/fetchers/sameOriginFetch.ts, src/shared/fetchers/backend.ts (existing transport, not changed or re-verified in this pass) |
| Leaving a cluster cancels its in-flight reads and closes its terminals | implemented | src/api/k8s/k8sContext.ts (cluster in the query key; TanStack Query aborts unobserved reads); terminals: streaming, not built |
| Leaving a cluster does not cancel writes already issued against it | implemented | generated mutation hooks (mutations are never cancelled by a key change) |
| A followed log stream is cancelled only by whoever opened it | blocked | owned by src/shared/streaming and the terminal domain; not built in this pass |
| A streaming body is handed over unparsed | blocked | owned by src/shared/streaming and the terminal domain; not built in this pass |
| A stream that cannot start reports why | blocked | owned by src/shared/streaming and the terminal domain; not built in this pass |
| A stream that answers without a body is a failed stream | implemented | dash-next/src/shared/streaming/logStream.ts |
| Terminal connections use the exec channel protocol and are forgotten when they close | blocked | owned by src/shared/streaming and the terminal domain; not built in this pass |
| A 401 from the proxy ends the session and sends the user to sign-in | dropped | — |
| A 401 from the proxy is ignored when the server requires no sign-in | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| Sign-out cleanup may see a 401 without triggering the sign-in redirect | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| A 401 from the proxy sends the user to sign-in only when the hub confirms its own session expired | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| A 401 that the hub does not confirm is shown as the cluster's own refusal | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| The hub identity check after a 401 is bounded and shared | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| No identity check is made when the hub requires no sign-in | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| No request is sent while the session is being replaced | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| An answer that arrives after the session changed is discarded | implemented | dash-next/src/shared/fetchers/sameOriginFetch.ts, dash-next/src/shared/fetchers/requests.ts (toDiscardedError), dash-next/src/shared/fetchers/backend.ts (withSessionChanged) |
| A discarded 401 answer still reports status 401 | implemented | dash-next/src/shared/fetchers/requests.ts (toDiscardedError) |
| A 401 for a request from a previous session does not sign the new session out | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |

## cluster-load.feature

| Scenario | Status | Implemented in |
|---|---|---|
| One load reads twelve cluster-wide resources in parallel | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| One load reads twenty-one cluster-wide resources in parallel | dropped | — |
| Collections are prepared on every load that no screen displays | dropped | — |
| Usage is read from each node's kubelet after the node list answers | blocked | no generated hook for the Node proxy subresource (GET /api/v1/nodes/{name}/proxy/stats/summary) |
| The operator's Lease is read last, on every load | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| The operator's Lease is read only when no hub answers for the cluster | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| Certificate signing requests are read on every load | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| Cluster reads after the probe have no deadline, so one stalled read stops polling | dropped | — |
| Helm releases are found without ever downloading Secret contents | blocked | owned by the helm domain (D10); release derivation is in src/domain/helm/utils/ |
| Without server summaries, Helm releases come from a metadata-only table | blocked | owned by the helm domain (D10); release derivation is in src/domain/helm/utils/ |
| Helm releases are found by downloading every Secret in the cluster | dropped | — |
| Every load first refreshes the list of clusters from the hub | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A failed cluster-list refresh keeps the clusters already known | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| With no hub at all the dashboard assumes a single local cluster | dropped | — |
| An empty cluster list from a reachable hub is treated as a single local cluster | dropped | — |
| An empty fleet is shown as empty and an unreadable fleet as unreadable | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A liveness probe decides whether the full load runs | dropped | — |
| An unreachable cluster costs one request and keeps the last good data | dropped | — |
| A reachable probe that carried no version leaves the Kubernetes version blank | dropped | — |
| The hub's facts for the cluster are fetched alongside the reads | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A standalone hub's facts are addressed to the hub's own cluster record | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A standalone hub with no marked record is addressed as "local" | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A known hub that cannot give facts stops the load from committing | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| With no hub at all, verdicts fall back to rules applied in the browser | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| Missing hub facts are reported in the console once per cluster per session | implemented | dash-next/src/domain/shell/hooks/useReadWarnings.ts, dash-next/src/domain/shell/utils/predicates.ts (isFactsRead) |
| A slice of the snapshot is replaced only when every read behind it answered | implemented | src/shared/query/loadState.ts, src/shared/query/polling.ts (each kind is its own query and keeps its last data on a failed refresh) |
| An answered empty list replaces the previous rows | implemented | TanStack Query data replacement (no code) |
| A read that fails or answers empty keeps the previous rows for some collections | dropped | — |
| A read that fails blanks other collections | dropped | — |
| A failed read of one workload kind removes that kind's rows | dropped | — |
| An answer of the wrong shape is a failed read | blocked | needs shape validation in src/api/k8s/k8sFetcher.ts (not owned here); arbitrary kinds validate in src/shared/fetchers/arbitraryKinds.ts |
| An answer of the wrong shape is accepted as an empty list | dropped | — |
| A missing operator Lease is an answer, not a failure | implemented | src/domain/overview/utils/transforms.ts (fromOperatorLeaseError) |
| A failed operator Lease read keeps every cluster fact on its previous value | dropped | — |
| A failed operator Lease read leaves only the operator's state without a fresh verdict | implemented | src/domain/overview/utils/transforms.ts (fromOperatorLeaseError); the Lease is its own query |
| An unreachable kubelet leaves that node on request-based figures | implemented | src/domain/nodes/utils/selectors.ts (utilisationOf, requestsOn), src/domain/overview/utils/reducers.ts |
| Workload usage is replaced only by a complete sample | implemented | src/domain/workloads/hooks/usePodUsage.ts, src/domain/workloads/utils/predicates.ts (`isCompleteSample`), src/domain/workloads/utils/reducers.ts (`withSample`), src/domain/workloads/utils/transforms.ts (`toUsageSample`); the held sample is not named stale as "WORKLOADS" by the refresh toast |
| A workload first seen without a complete usage sample shows no usage | implemented | src/domain/workloads/hooks/usePodUsage.ts, src/domain/workloads/utils/reducers.ts (`withSample`) |
| Workload usage is recomputed whenever any kubelet answered | dropped | — |
| With no usage summary at all every workload shows zero usage | dropped | — |
| A failed node list also leaves workload usage on its last sample | blocked | the pure pieces are in src/domain/workloads/utils/transforms.ts and src/domain/workloads/utils/selectors.ts; keeping the previous sample is the workloads screen's wiring, and the kubelet read has no generated hook |
| Only outage-like failures count against the cluster's reachability | implemented | dash-next/src/shared/query/polling.ts (isRefusal: 429 is an outage), dash-next/src/api/k8s/k8sFetcher.ts |
| A round in which nothing succeeded and something was an outage keeps the last good data | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A partly failed round commits what loaded and says so in the console | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A round in which every read is forbidden still counts as reachable | dropped | — |
| A kind the user may not list is shown as forbidden without affecting other kinds | implemented | src/shared/query/loadState.ts (toLoadState), src/shared/query/polling.ts |
| A load reports whether anything answered and which slices are stale | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| A load that hit a 401 reports the session as ended | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| Every failed read leaves a console warning | implemented | dash-next/src/domain/shell/hooks/useReadWarnings.ts, dash-next/src/domain/shell/utils/formatters.ts (toReadWarning) |
| A committed load publishes every collection at once | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| The first load runs once when the dashboard shell appears | blocked | D9 replaces the snapshot load with one polled query per kind; the consuming domain wires its own reads with src/shared/query/polling.ts and src/shared/query/loadState.ts — not wired in this pass |
| A load runs when a cluster is opened and on every switch | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| Asking to load a cluster other than the selected one selects it | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A successful cordon or uncordon is followed by an immediate load | blocked | owned by the nodes domain |
| A refused cordon triggers no load | blocked | owned by the nodes domain |
| Changing the namespace filter issues no request | implemented | src/shared/context/NamespaceContext.ts, src/shared/utils/filters.ts (inNamespace) |

## polling-and-refresh.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The next poll is scheduled 15 seconds after the previous load settles | implemented | src/shared/query/polling.ts (refetchInterval, re-armed by TanStack Query when a read settles) |
| Pausing live refresh stops the polling | implemented | src/shared/query/polling.ts (polledWhen) |
| Resuming live refresh waits a full interval before the first poll | implemented | src/shared/query/polling.ts (polledWhen) |
| Polling does not start until the first load has finished | implemented | src/shared/query/polling.ts (refetchInterval, re-armed by TanStack Query when a read settles) |
| The first poll is scheduled 15 seconds after a cluster is opened even if its first load has not finished | dropped | — |
| Polling restarts when the selected cluster changes | implemented | src/shared/query/polling.ts, src/api/k8s/k8sContext.ts (a new cluster is a new query) |
| A failed poll is retried at the same 15 second cadence | dropped | — |
| Consecutive failed polls back off exponentially with jitter | implemented | src/shared/query/polling.ts (toPollDelay) |
| One successful poll returns to the healthy cadence | implemented | src/shared/query/polling.ts (toPollDelay) |
| A poll superseded by a newer load is neither a success nor a failure | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| Every load ends in exactly one verdict | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| Every load ends in one refresh verdict | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A long list of stale slices is shortened in the refresh message | implemented | dash-next/src/domain/shell/utils/formatters.ts (toRefreshMessage) |
| Up to four stale slices are all named | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| Stale slices from the latest load are reported until a later load clears them | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| No refresh is claimed before a load has answered | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| The refreshed time is set when the page opens, before any load | dropped | — |
| A manual refresh shows its verdict as a transient message | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A manual refresh that crashes says the last data is kept | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A manual refresh that finds the session ended shows no message | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A 401 on one read while sign-in is not required still suppresses the refreshed time and message | dropped | — |
| A 401 that is not an ended session is reported like any other failed read | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A successful manual refresh confirms and clears the banner | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A failed manual refresh says the last data is kept | implemented | dash-next/src/domain/shell/utils/predicates.ts (isRefreshFailed), dash-next/src/domain/shell/utils/formatters.ts (toRefreshMessage) |
| A failed manual refresh while paused raises no banner | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| The reload after a cordon updates the refreshed time and banner without a message | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A manual refresh overtaken by a cluster switch reports nothing | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| Refreshing the list of clusters updates the "refreshed" time | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| An unreachable cluster raises a reconnecting banner | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| The banner goes away on the next successful poll | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A cluster that is down when opened raises the banner immediately | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A cluster opened while paused raises no banner | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| Pausing live refresh clears the banner | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| An unreachable cluster raises no banner on the open dashboard | implemented | dash-next/src/domain/shell/utils/predicates.ts (isReconnecting) |

## stale-load-guards.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Only one load runs at a time | implemented | TanStack Query de-duplication per query key (no code) |
| Requests that arrive during a load share one follow-up load | implemented | TanStack Query de-duplication per query key (no code) |
| A load requested after a write always starts after the write | implemented | src/domain/resources/hooks/useResourceBrowser.ts (refetch after an accepted write cancels the read in flight) |
| A crashed load does not block the one queued behind it | implemented | TanStack Query de-duplication per query key (no code) |
| Overlapping loads of the same cluster may run, and only the newest commits | dropped | — |
| Switching cluster empties the snapshot before the new cluster loads | implemented | src/api/k8s/k8sContext.ts (cluster in every query key and in the hook variables) |
| Reopening the same cluster does not empty a snapshot that already loaded | implemented | src/api/k8s/k8sContext.ts (cluster in every query key and in the hook variables) |
| A load overtaken by a cluster switch commits nothing | implemented | src/api/k8s/k8sContext.ts (cluster in every query key and in the hook variables) |
| Switching away and back does not let the earlier load commit | implemented | src/api/k8s/k8sContext.ts (cluster in every query key and in the hook variables) |
| Every read of one load targets the cluster the load began with | implemented | src/api/k8s/k8sContext.ts (cluster in every query key and in the hook variables) |
| Open logs and terminals are closed on a cluster switch | blocked | owned by src/shared/streaming and the terminal domain; not built in this pass |
| A cordon that completes after a cluster switch reports nothing | blocked | owned by the nodes domain |
| The outcome of a switched-away first load does not touch the new cluster's indicators | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| A load overtaken by a sign-out or a new session writes nothing | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| A load requested for a session that already ended does not start | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| Ending or replacing the session empties the snapshot | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| Signing out does not empty the snapshot | dropped | — |

## caching-and-persistence.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Cluster data is held in memory only | implemented | TanStack Query cache with no persister (no code); src/shared/query/loadState.ts |
| Before any load answers, every collection is empty rather than fabricated | implemented | src/shared/query/loadState.ts, src/domain/overview/constants.ts (emptyUsage) |
| The snapshot survives a failed poll | implemented | TanStack Query cache with no persister (no code); src/shared/query/loadState.ts |
| The server configuration is fetched once and reused | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| The selected cluster is remembered across page loads | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A browser that blocks local storage still works, without remembering the cluster | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| With nothing remembered the selection is "local" | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| The list of clusters outlives cluster switches and cluster loads | blocked | owned by the clusters domain (hub cluster list, facts, selection) |
| A hub configuration once loaded is kept when a later fetch fails | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| Only a non-secret session marker is shared between tabs | blocked | owned by the auth domain (session, 401 confirmation, sign-out) |
| The page remembers whether a load ever succeeded and the last Kubernetes version | blocked | owned by the shell (live indicator, Refresh, banner); it consumes src/shared/query/polling.ts and src/shared/query/loadState.ts |
| The previous snapshot is the fallback for values a load could not refresh | implemented | src/shared/query/loadState.ts, src/domain/overview/utils/reducers.ts |
| Console warnings about a missing hub are remembered for the life of the page | implemented | dash-next/src/domain/shell/hooks/useReadWarnings.ts |

## engine-state-load.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Engine-state evidence is requested before the cluster is probed | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| Engine-state evidence is still published when the cluster itself is unreachable | implemented | dash-next/src/domain/hardening/hooks/useEngineState.ts, dash-next/src/domain/clusters/components/ClusterEngineState/ClusterEngineState.tsx (its own polled queries, D33) |
| Engine-state evidence is published on every early stop of a load | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A standalone hub's engine state is addressed to the hub's own cluster record | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| Each engine-state request is same-origin, asks for JSON and is abandoned after 10 seconds | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| One engine-state load is abandoned as a whole after 15 seconds | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A refused summary makes the engine state unavailable with a stated reason | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A refused findings or evidence page has its own reason | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A detail-expired or detail-unavailable refusal makes the engine state unavailable | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A page of the wrong shape makes the engine state unavailable | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A report generation that moved mid-read is re-read once | dropped | D31 |
| A report generation that moves twice ends the load | dropped | D31 |
| Findings are read page by page, following the hub's cursor, for the version signal only | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| At most 100 findings pages are read | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A cursor the hub already issued ends the read as truncated | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A page that says its snapshot is incomplete marks the result as partial | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| Evidence delivered with the findings is not requested again | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| Evidence with no matching observation is read from the hub, with the same bounds | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| An observation repeated across pages is kept once | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| An unavailable engine state leaves one console note per cluster and reason | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| An engine-state refusal never turns a reachable cluster into an unreachable one | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| The hub's engine-state refresh runs only for a reachable cluster | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| The per-poll engine-state refresh re-pins a moved generation at most twice | dropped | D31 |
| A per-poll engine-state refresh that exceeds 15 seconds is published as a timed-out refusal | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| A summary without a report generation is shown without findings | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| The two engine-state reads of one load share one summary request | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
| Engine state from a cluster the user left is never shown | blocked | owned by the clusters and hardening domains (D23); not built in this pass |
