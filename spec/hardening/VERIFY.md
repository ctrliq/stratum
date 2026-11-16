# hardening — verification report

Verified 2026-11-13 against `pro` = `~/Code/stratum-pro/stratum-dash`
(`src/screens/hardening.jsx`, `src/evidence-backup.jsx`,
`src/evidence-certificate.jsx`, plus the cited lines of `src/live.js`,
`src/app.jsx`, `src/cluster-context.js`, `src/fleet.js`, `src/data.js`,
`src/k8sident.js` and `src/ui/enginestate.jsx`).

## Summary

**71 scenarios checked (all of them, no sampling): 69 VERIFIED, 2 DRIFTED,
0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 5 UNCOVERED behaviors.
Not ready to implement** (DRIFTED and UNCOVERED outstanding). Five further
VERIFIED scenarios are flagged below because they describe a component
contract the application never reaches as written.

How it was checked:

- Completeness pass first: the three source files were read in full and
  inventoried before any `.feature` file was opened.
- Every `# source:` citation (4 files) was resolved mechanically: file exists
  and every line range is inside the file. All resolve.
- Every scenario and every Outline row was compared with the code by reading
  (the two evidence panels are JSX modules whose rules are not exported, so
  they were traced by hand rather than run): the verdict order, the coverage
  and staleness arithmetic, `duration`, `copyAge`, `attemptLater` /
  `successLater` / `reconcileAttempt`, `expiryBand`, `remainingText`,
  `ageText`, `causesOf`, `roleCoverage`, and in `live.js` the lease verdict,
  the CSR counting and the PodSecurity counting.
- All 3 `# assumption:` scenarios were re-derived and hold (no role check in
  `hardening.jsx`; no result field in the control rows; an empty required
  location list yields "satisfactory").

## UNCOVERED behaviors

| # | Location | Behavior with no scenario | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/screens/hardening.jsx:96,120-121`; `pro:src/live.js:783-784` | The hub's pending count and its availability are independent fields. When the hub states availability "available" but no count, the card prints "null pending" (or "undefined pending") and the signal is a warning reading "N approved · null pending (kubelet-serving signer)". (NOTES (a) 2 records it; no scenario does.) | A hub that states the request availability but no pending count |
| 2 | `pro:src/live.js:749,760,783-784`; `pro:src/screens/hardening.jsx:95-96,120-121` | With hub facts, the approved count still comes from the cluster's own request list. If that list is refused or fails while the hub answers "available", the screen shows "0 approved" beside the hub's pending count, as if zero had been observed. (NOTES (a) 3 records it; no scenario does.) | With hub facts, an unreadable cluster request list reads as 0 approved beside the hub's pending count |
| 3 | `pro:src/ui/enginestate.jsx:517-532,549` | The certificate panel is mounted only when there is at least one row. A certificate finding opened with no loaded certificate observation and no certificate fragment shows no certificate panel at all — only the source line, any other items and "Full evidence". | A certificate finding with no observation and no certificate fragment shows no certificate panel |
| 4 | `pro:src/evidence-backup.jsx:309`; `pro:src/ui/enginestate.jsx:552` | A published outcome word outside the six listed in the Outline (for example "unconfigured", or a word a newer hub adds) is shown verbatim as the verdict word; only "unknown" is renamed, to "uncertain". | A published outcome the dashboard does not know is shown verbatim |
| 5 | `pro:src/evidence-backup.jsx:87-99` | Ordering of two observations when attempt times are missing: an absent attempt time counts as the earliest, two absent ones tie and fall through to completion, then failure, and finally to the alphabetically smaller outcome word. The Outline covers only the cases where both attempt times are present. | Observations with no attempt time are ordered by completion, then failure, then outcome word |

Also noted, owned by another domain: `pro:src/app.jsx:375-377` — a Refresh
that is overtaken by a cluster switch (or yields no connection verdict) ends
with no notification at all. The Refresh Outline in `posture.feature` lists
only "Refreshed" and "Refresh failed"; the silent third outcome belongs to
`shell`, check that it is specified there.

Source files under the domain's paths that no scenario cites: none.
Source files that belong to no domain: none found.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `evidence-backup.feature` | With no published outcome the panel derives the verdict from the fragment | DRIFTED | The Given cannot occur in the application and the Then contradicts the scenario above it. The only mount passes `outcome={context?.observation?.nativeOutcome ?? 'unknown'}` (`enginestate.jsx:551-552`), so a finding that publishes nothing reads "uncertain" (as the row "nothing → uncertain" of "The verdict shown is the report's published outcome" says), never "satisfactory", "critical" or any other derived word. The derived reading is only ever visible as the *explanation* that follows the published word when the two words agree (`evidence-backup.jsx:309-310`). The `outcome === undefined` branch is a component default with no caller. Minor, same Outline: the row "the policy state is any word other than 'configured' → uncertain" omits that `enabled: false` is tested first and gives "disabled" whatever the state word (`:181`). | Retitle "The explanation is the panel's own reading of the fragment" and reword: Given "a backup fragment whose own reading is `<condition>`"; Then "the panel's own verdict is `<word>`, and when the published word equals it the explanation reads `<explanation>`". Row: "the policy state is any word other than 'configured', 'unconfigured' or 'disabled' and the policy is not explicitly switched off". Add `pro:src/ui/enginestate.jsx:551-552` to the citation. |
| `posture.feature` | The kubelet serving certificates card counts approved and pending requests — row "the request list could not be read at all → 0 approved / CSR pending count unknown" | DRIFTED | True only without hub facts. With hub facts the pending count and availability are the hub's (`live.js:783-784`), so an unreadable cluster list gives "0 approved" with "`<hub count>` pending", not "CSR pending count unknown". | Row: "no hub facts are available and the request list could not be read at all → 0 approved / CSR pending count unknown"; add the hub-facts case as UNCOVERED #2. Add `pro:src/live.js:749,760,783-784`. |

## Scenarios that describe a component contract rather than reachable behavior

VERIFIED against the cited code, but as written they will mislead an
implementer or cannot be exercised through the application.

| File | Scenario | Why |
|---|---|---|
| `evidence-backup.feature` | The derived verdict asks its questions in a fixed order | "The derived verdict" is never displayed as a verdict (see the DRIFTED row above); it only decides whether an explanation is shown. Reword in terms of the explanation. |
| `evidence-backup.feature` | Coverage problems decide between critical and partial | Same. |
| `evidence-backup.feature` | Every coverage problem is listed | Same. |
| `evidence-backup.feature` | A policy that requires no location is satisfied by an empty set of backups (`# assumption:`) | Same; the assumption itself holds (`evidence-backup.jsx:228,240,249`). |
| `evidence-certificate.feature` | A panel with no observations says so | The message exists (`evidence-certificate.jsx:256`) but the panel is never mounted with an empty list (`enginestate.jsx:549`); see UNCOVERED #3 for what the user actually sees. |

## Citation corrections on VERIFIED scenarios

| File | Scenario | Correction |
|---|---|---|
| `evidence-backup.feature` | A backup finding's evidence opens as backup panels | The "Full evidence" step comes from `570-575`, which the citation omits. Cite `pro:src/ui/enginestate.jsx:512-516,533-535,550-553,562,570-575`. |
| `evidence-backup.feature` | The verdict is styled by the finding's severity | The "unknown" default is applied by the caller; `evidence-backup.jsx:319` has its own unreachable default (`partial → warning`, else the word). Keep both citations, behavior as specified. |
| `evidence-certificate.feature` | A certificate finding's evidence opens as the certificate panel | The observations are a snapshot taken when the evidence is opened (`enginestate.jsx:932-936`), not a live view of later findings pages. Add `pro:src/ui/enginestate.jsx:836-837,932-936`. |
