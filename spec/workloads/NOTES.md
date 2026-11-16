# workloads — notes

Extracted from the workload descriptors in `src/resources.jsx`, the rules
they call (`oss:src/k8s-rules.js`, `pro:src/k8srules.js`),
`src/screens/workloads.jsx`, the scale/restart paths of
`src/ui/resource-list.jsx` and the `logs`/`exec` branches of `onAction` in
`src/app.jsx`, in both roots.

## Scope decisions

- `domains.md` lists pods, deployments, statefulsets, daemonsets, jobs and
  cronjobs. ReplicaSets and Replication Controllers have descriptors and
  sidebar rows in the Workloads section in both roots and no other owner, so
  they are specified here.
- The Workloads overview reads the cluster snapshot. How each snapshot row's
  ready/status/cpu/mem is computed is `data` domain behavior; `overview.feature`
  specifies only what the screen does with the rows.
- The bottom drawer itself is `terminal`; `pod-actions.feature` stops at "a tab
  opens on the log/shell view".
- The brief mentioned cordon and triggering a cron job. Cordon/uncordon exists
  but acts on nodes (`app.jsx` `onAction`), so it belongs to `nodes`. Neither
  root can trigger, suspend or resume a cron job, nor delete or evict a pod
  other than through the generic Delete; there are no scenarios for them.

## Divergences and recommendation

Six `@diverged` pairs.

| # | Behavior | oss | pro | Keep | Why |
|---|---|---|---|---|---|
| 1 | Unschedulable pod status | "Pending" | "Unschedulable" (warning) | pro | Distinguishes a pod that will never start from one that is starting. |
| 2 | Deployment **Ready** on the Deployments list | from the listed object (`readyReplicas/spec.replicas`) | from the shell's cluster snapshot (hub verdict), with "Observed <time>" tooltip | oss for the list; offer the hub's observation as an extra where a hub exists | pro's row mixes two refresh clocks, shows "—" for deployments the snapshot has not seen, and freezes when auto-refresh is paused. |
| 3 | Deployment **Available** on the Deployments list | from the listed object | from the snapshot | oss | Same reason as 2. |
| 4 | Replication Controllers | create/edit/scale/delete | read-only | needs a decision; lean oss | pro's reason is its agent's RBAC grant, not a product rule; a unified app should hide by what the caller may do. |
| 5 | Kinds offered Scale | deployments, statefulsets, replicasets, replicationcontrollers | first three only | follows 4 | Same root cause. |
| 6 | Scale input validation | whole number 0…2147483647 or the button is disabled | anything is coerced: empty/garbage/negative → 0, decimals floored | oss | pro scales a workload to zero when the field is emptied. |

One-sided: pro's "Observed <time>" tooltip on the overview's Ready cell
(`@pro`).

## Bugs and oddities in the legacy code

1. *(pro)* **Emptying the replica field and pressing Scale scales to 0**
   (`Math.max(0, Math.floor(Number(n) || 0))`). Highest-impact bug found in
   these domains.
2. *(pro)* Deployment Ready/Available come from the shell snapshot while
   Up-to-date and Age come from the list's own poll: one row, two clocks. With
   auto-refresh paused the two columns freeze while the list keeps polling. A
   deployment created since the last snapshot shows "—/—".
3. A refused Scale closes the panel (the close runs whether or not the write
   succeeded); a refused Edit keeps it open. Inconsistent in both roots.
4. The Scale field is prefilled from the object as of the row click, not the
   current replica count.
5. Restart only reports that the patch was accepted; nothing follows the
   rollout.
6. Job status logic is written twice in pro (inline in the descriptor and in
   the loader) and once in oss (shared rule). Results are the same today.
7. *(oss)* The comment above the scale set (`resources.jsx:237-239`) omits
   replication controllers, which the set includes.
8. Workloads overview: "1 workloads" (no singular); the kind-filter counts and
   the "N degraded" flag ignore the namespace, kind filter and search, so they
   can disagree with the rows; rows are not keyboard-reachable; there is no
   loading or error state of its own, so a loading, unreadable or forbidden
   cluster all read "No workloads found."; an unknown kind's row opens the
   Deployments list.
9. Pod status treats any waiting reason containing `Err`, `ImagePull` or
   `Invalid` as Failed (substring match), e.g. `CreateContainerConfigError`.
10. Exec is the only write-flagged contextual action; Logs is offered to every
    session that can load the list.

## pro code that may not belong in public

- `pro:src/resources.jsx:22-40` and `pro:src/live.js:622-638` — the hub's
  per-deployment verdict ("DeploymentFact": replicas, ready, available,
  minReadySeconds-aware status, observation time). It is a fleet-hub feature;
  whether its contract is public is a product decision. No secret material.
- Otherwise none found.

## Fix pass

Counts: 3 added, 2 corrected, 9 `@drop`, 2 `@fixed` (included in the 3 added),
0 moved, 0 renamed. 71 scenarios.

Verifier items:

- DRIFTED `scale.feature` "The prefilled count is the one seen when the row was
  clicked" — now `@diverged @oss` with oss citations only (title unchanged),
  paired with new `@diverged @pro @drop` "The prefilled count is the first
  clicked workload's when the panel was left open". This is a higher-impact
  defect than bug 1 above: no typing is needed to scale workload B to A's count.
- DRIFTED `overview.feature` "While the first snapshot is loading the overview
  looks empty" — the loading marker's wording now names both roots' texts.
- No verifier finding was rejected.

Decisions:

- **D1.** Dropped: oss "Pending" for an unschedulable pod; pro snapshot-sourced
  Ready and Available, with the two pro-only consequences ("shows dashes",
  "Pausing auto-refresh freezes…", also D2); pro scale coercion (also D2); pro
  carried-over prefill (also D2).
- **D15.** The Replication Controllers pair and the "kinds offered Scale" pair
  both stand, one per backend; the pro variants gained `@fleet`. This supersedes
  rows 4 and 5 of the table above.
- **D2.** Overview: "…looks empty" while loading and "…cannot tell an unreadable
  cluster from an empty one" are `@drop`, with `@fixed` "While workloads are
  first loading the overview says it is loading" and "The overview says when
  workloads could not be read" (also D9).
- **D22.** ReplicaSet and ReplicationController scenarios were already in this
  domain; nothing moved.

Left as they are (surprising, not defective under D2): a refused Scale closing
the panel, "1 workloads", filter counts ignoring the namespace, pointer-only
overview rows.
