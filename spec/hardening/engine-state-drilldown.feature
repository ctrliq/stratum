Feature: Cluster engine-state drill-down
  From the Clusters page, "Engine state" on a cluster opens a read-only
  panel with the hub's verdict for that cluster and the findings behind it,
  filterable by signal, node and minimum severity. It never contacts the
  cluster itself and offers no action on it.

  # ---- opening and closing --------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:110,232,271,361-365
  # source: pro:src/ui/enginestate.jsx:861-871
  Scenario: Engine state opens the drill-down for that cluster
    Given the Clusters page lists the cluster "prod"
    When the user clicks "Engine state" on "prod" in the card or table view
    Then a panel titled "Engine state — prod" with the subtitle "the hub's findings behind its verdict" appears below the cluster list
    And it offers "Close"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:864
  Scenario: The panel title falls back to the cluster id
    Given a cluster without a name
    When its drill-down is open
    Then the title names the cluster by its id

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:364
  # source: pro:src/ui/enginestate.jsx:869
  Scenario: Close removes the drill-down
    Given a drill-down is open
    When the user clicks "Close"
    Then the panel disappears and the cluster list is unchanged

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:110,232,361-365
  # source: pro:src/ui/enginestate.jsx:713-734
  Scenario: Opening another cluster's drill-down replaces the first and starts clean
    Given the drill-down of "prod" is open with filters selected, findings listed and an evidence drawer open
    When the user clicks "Engine state" on "staging"
    Then the panel shows "staging" only
    And its filters are reset, and no finding or evidence of "prod" is shown

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:110,361
  Scenario: The drill-down stays open for a cluster that left the list
    Given the drill-down of "edge" is open
    When "edge" is unregistered or disappears from the cluster list
    Then the panel stays open for "edge" and keeps reading the hub's record for it

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:232,271
  # source: pro:src/ui/enginestate.jsx:675
  Scenario: The drill-down is available to read-only users and in standalone mode
    Given the user has no "admin" role, or the hub runs in standalone mode
    When the user opens the Clusters page
    Then "Engine state" is offered on every cluster and opens the same panel

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:231-232
  Scenario: The drill-down is available for a disconnected cluster
    Given a cluster with status "disconnected"
    When the user clicks its "Engine state"
    Then the panel opens and shows whatever the hub's record holds for it

  # ---- the summary --------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:873-876
  Scenario: The panel says it is reading until the summary answers
    Given no engine-state summary of the cluster is known yet
    When the drill-down opens and the hub has not answered
    Then the panel reads "reading engine state…"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:873-875
  Scenario: A refused first summary quotes the hub
    Given no engine-state summary of the cluster is known yet
    When the hub refuses the summary read with the message "the cluster's engine-state source is not connected"
    Then the panel reads "engine state unavailable — the cluster's engine-state source is not connected"
    And no filters and no findings are shown

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:824,868,878-887
  Scenario: The summary shows the hub's verdict, freshness, coverage and counts
    Given the hub's summary for the cluster has severity "warning", freshness "stale", an evaluation time 2 minutes ago, coverage 8 evaluated of 10 expected with 2 unknown, and 5 findings of which 3 required and 2 optional
    When the drill-down is shown
    Then the header shows the verdict "warning"
    And "Freshness" shows "stale" and "evaluated 2m ago"
    And "Coverage" shows "8/10 evaluated" and "2 unknown"
    And "Findings" shows "5 total · 3 required · 2 optional"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:63,171-174
  Scenario Outline: Each freshness band has a fixed tone
    Given the hub's summary states the freshness "<freshness>"
    When the freshness is shown
    Then it reads "<freshness>" in the <tone> tone

    Examples:
      | freshness | tone     |
      | fresh     | positive |
      | stale     | warning  |
      | warning   | warning  |
      | degraded  | critical |
      | unknown   | neutral  |
      | other     | neutral  |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:9-12,171-174,882
  Scenario: Severity and freshness are printed as the hub states them
    Given the hub's summary states a severity and a freshness
    When the drill-down is shown
    Then both words are shown verbatim
    And neither is recomputed from timestamps or findings in the browser

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:189-205
  Scenario Outline: Coverage always states what is unknown or missing
    Given the hub's summary has coverage <coverage> and an observation time that is <observed>
    When the coverage is shown
    Then it reads "<text>"
    And these extra labels are shown: <labels>

    Examples:
      | coverage                                              | observed | text            | labels                                         |
      | 10 evaluated, 10 expected, 0 unknown, complete        | present  | 10/10 evaluated | "0 unknown" without emphasis                   |
      | 8 evaluated, 10 expected, 2 unknown, complete         | present  | 8/10 evaluated  | "2 unknown" as a warning                       |
      | 8 evaluated, 10 expected, 2 unknown, not complete     | present  | 8/10 evaluated  | "2 unknown" as a warning, "incomplete"         |
      | 0 evaluated, 10 expected, 10 unknown, complete        | absent   | 0/10 evaluated  | "10 unknown" as a warning, "no source sample yet" |
      | 8 evaluated, 10 expected, no unknown count            | present  | 8/10 evaluated  | none                                           |
      | no coverage at all                                    | present  | —/— evaluated   | none                                           |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:209-225
  Scenario: Finding counts list only the severities that occur, in severity order
    Given the hub's summary counts findings by severity as critical 1, healthy 4, warning 0 and info 2
    When the counts are shown
    Then the chips read "healthy 4", "info 2" and "critical 1" in that order
    And no chip is shown for warning or unknown

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:216
  Scenario: Missing finding counts are shown as dashes
    Given the hub's summary carries no finding counts
    When the counts are shown
    Then the line reads "— total · — required · — optional"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:738,824,894
  Scenario: A refused summary re-read keeps the last summary and says so
    Given the drill-down shows a summary read earlier
    When a later summary read is refused with the message "hub overloaded"
    Then the earlier summary stays
    And the panel adds "the latest read was refused — hub overloaded"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:836-837,888-893
  Scenario: Certificate coverage by role is shown when the findings carry certificate observations
    Given the walked findings carry at least one certificate observation
    When the drill-down is shown
    Then a "Certificate coverage by role" line is shown beside the summary
    And it is absent when no certificate observation was walked

  # ---- findings -------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:747-778
  Scenario: The findings read follows the report the summary names
    Given the hub's summary names a report with retained detail
    When the drill-down loads
    Then the findings of exactly that report generation are read, with the selected filters

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:762-766
  Scenario: A summary without retained detail leads to no findings read
    Given the hub's summary names no retained detail
    When the drill-down loads for the first time
    Then no findings are requested
    And the filters are shown with no findings under them

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:266-268,762-766
  Scenario: Findings walked earlier stay listed when the summary stops naming retained detail
    Given the drill-down lists findings walked for an earlier report
    When a re-read summary names no retained detail
    Then no findings are requested
    And the earlier findings stay listed with the previous-report note

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:254-265
  Scenario: The findings header states the total and the state
    Given a walk that returned 12 findings in total with the state "complete"
    When the findings are shown
    Then the header reads "12 findings · complete"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:260
  Scenario: A walk without a state shows a dash
    Given a walk whose last page stated no state
    When the findings are shown
    Then the header reads "<total> findings · —"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:261-263
  Scenario: An incomplete walk is labelled partial
    Given a walk that the hub marked incomplete, serving 7 of 12 findings
    When the findings are shown
    Then a note reads "partial — 7 of 12 findings served"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:462-483
  Scenario: A finding row states the finding as the hub stated it
    Given a finding with severity "critical", code "cert-expiring", a message, signal "certificate", scope "node", node "uid-1", component "kubelet", key "serving" and a sample time
    When the finding is listed
    Then the row shows "critical", "cert-expiring" and the message
    And a detail line reading "certificate · node · node uid-1 · kubelet · serving · sampled <local time>"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:464-476
  Scenario Outline: Missing parts of a finding are shown as placeholders or left out
    Given a finding without <part>
    When the finding is listed
    Then <shown>

    Examples:
      | part            | shown                                              |
      | a severity      | its severity reads "unknown" in the neutral tone   |
      | a code          | its code reads "—"                                 |
      | a message       | no message text is shown                           |
      | a signal        | the detail line starts with "unknown signal"       |
      | a subject scope | the detail line reads "unknown scope"              |
      | a node          | the detail line has no node part                   |
      | a component     | the detail line has no component part              |
      | a key           | the detail line has no key part                    |
      | a sample time   | the detail line ends with "sampled —"              |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:477-479,921
  Scenario Outline: Only a finding with an evidence reference offers its evidence
    Given a finding whose evidence reference is <ref>
    When the finding is listed
    Then an "Evidence" button is <offered>

    Examples:
      | ref      | offered     |
      | present  | offered     |
      | null     | not offered |
      | absent   | not offered |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:480,485-500,857,919
  # source: pro:src/enginestate.js:90-92
  Scenario: A finding shows the collection observation that matches its signal and subject
    Given the walk carried an observation with the same signal and subject as a finding
    When the finding is listed
    Then under it "Collection: <availability>" is shown
    And the observation's reason as "<code>: <message>", with " · retryable" when it is retryable
    And its source as "Process <instance> · build <build> · started <local time>"
    And its "Sampled", "Attempted", "Completed" and "Last success" times, "—" for any that is missing

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:480,919
  Scenario: A finding without a matching observation shows no collection block
    Given no walked observation has a finding's signal and subject
    When the finding is listed
    Then no "Collection" block is shown under it

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:856,915
  Scenario: A walk with no findings lists nothing
    Given a walk that returned no findings
    When the drill-down is shown
    Then the findings header reads "0 findings · <state>"
    And no finding rows are shown

  # ---- filters ----------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:456,847-855,895-910
  Scenario: The filters offer what the walked findings contain
    Given the walked findings carry the signals "certificate" and "version" and the nodes "uid-1" and "uid-2"
    When the filters are shown
    Then "Signal" offers "All signals", "certificate" and "version"
    And "Node" offers "All nodes", "uid-1" and "uid-2"
    And "Minimum severity" offers "All severities", "healthy", "info", "unknown", "warning" and "critical" in that order
    And all three start on their "All" choice

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:854-855
  Scenario: A selected filter value stays offered when the narrowed findings no longer list it
    Given the user selected the signal "version" and the narrowed walk returned no findings
    When the filters are shown
    Then "Signal" still offers "version" as the selected value

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:767-771,780
  # source: pro:src/enginestate.js:224-228
  Scenario Outline: Changing a filter re-reads the findings from the first page with that filter
    Given the drill-down shows findings
    When the user sets "<filter>" to "<value>"
    Then the summary is read again and a new findings walk starts at the first page
    And every findings request of that walk carries "<parameter>=<value>"

    Examples:
      | filter           | value       | parameter       |
      | Signal           | certificate | signal          |
      | Node             | uid-1       | nodeUID         |
      | Minimum severity | warning     | minimumSeverity |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:771
  # source: pro:src/enginestate.js:223,241
  Scenario: A filter change abandons the walk still in flight
    Given a findings walk is still being read
    When the user changes a filter
    Then nothing of the abandoned walk is shown, neither findings nor a refusal

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/ui/enginestate.jsx:774-777,858,911-913
  Scenario: Until the newly selected filters have loaded, the previous findings stay with a note
    Given findings loaded under "certificate", all nodes and all severities are shown
    When the user selects the minimum severity "warning" and that walk has not succeeded
    Then the earlier findings stay listed
    And a note reads "retained findings for certificate · all nodes · all severities; the selected filters have not loaded successfully."

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/ui/enginestate.jsx:774-777,858,911-913
  Scenario Outline: The retained-filters note distinguishes loading from failure
    Given findings loaded under earlier filters are shown and the user selected other filters
    When the walk under the selected filters <state>
    Then the earlier findings stay listed, named by the filters they were loaded under
    And the note says <note>

    Examples:
      | state            | note                                          |
      | is still pending | that the selected filters are loading         |
      | was refused      | that the selected filters could not be loaded |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:912
  Scenario: The retained-filters note names a retained minimum severity
    Given findings loaded under all signals, node "uid-1" and the minimum severity "warning" are shown
    When the user changes a filter and that walk has not succeeded
    Then the note reads "retained findings for all signals · uid-1 · minimum severity warning; the selected filters have not loaded successfully."

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:774-777,858
  Scenario: The note disappears once the selected filters load
    Given the retained-filters note is shown
    When the walk under the selected filters succeeds
    Then the findings of that walk replace the retained ones and the note disappears

  # ---- refused findings -----------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:231-252,269,914
  Scenario Outline: A refused findings read is explained in the hub's terms
    Given the drill-down shows a summary
    When the findings read is refused with <refusal>
    Then a note reads "<note>"
    And findings this panel walked successfully earlier stay listed

    Examples:
      | refusal                                                                | note                                                                                                                      |
      | 409 naming the new generation "g-8"                                    | the report changed while reading — the generation moved to g-8; these findings are from the previous report.              |
      | 409 naming no generation                                               | the report changed while reading; these findings are from the previous report.                                            |
      | 410 with a retained window from "g-3" to "g-7"                         | report detail expired — retained g-3 to g-7; the summary above stays current.                                             |
      | 410 with only a retained-from bound "g-3"                              | report detail expired — retained g-3 to —; the summary above stays current.                                               |
      | 410 with no retained window                                            | report detail expired; the summary above stays current.                                                                   |
      | 503 with the message "report incomplete", retryable                    | findings unavailable — report incomplete · retryable                                                                      |
      | 403 with the message "insufficient role", not retryable                | findings unavailable — insufficient role                                                                                  |
      | no answer within the deadline                                          | findings unavailable — the engine-state request timed out · retryable                                                     |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:232-240,914
  Scenario: A changed report offers an explicit reload instead of a silent refetch
    Given the findings read was refused with 409
    When the note is shown
    Then it offers "Reload findings"
    And clicking it reads the summary again and walks the findings of the report it now names, under the selected filters

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:242-251
  Scenario: Other refusals offer no reload control
    Given the findings read was refused with 410, 503 or any status other than 409
    When the note is shown
    Then it offers no button

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:266-268
  Scenario Outline: Findings from an older report than the summary are labelled
    Given the listed findings were walked for report "g-7"
    And the summary now shown describes <summaryReport>
    When the findings are shown
    Then a note reads "<note>"

    Examples:
      | summaryReport          | note                                                                                                            |
      | report "g-8"           | these findings are from the previous report — g-7; the summary describes g-8.                                    |
      | no report generation   | these findings are from the previous report — g-7; the summary describes a report without retained detail.       |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:255
  Scenario: With neither findings nor a refusal the findings area is empty
    Given no findings have been walked and no findings read was refused
    When the drill-down is shown
    Then nothing is shown between the filters and the end of the panel

  # ---- staying current -------------------------------------------------------------------

  @pro @fleet
  # assumption: holds once the page is redrawn with the new list; a runtime check with the selected cluster unreachable would confirm whether the re-read then lags one refresh behind
  # source: pro:src/screens/clusters.jsx:352-365
  # source: pro:src/ui/enginestate.jsx:747-780,807-816
  Scenario: An open drill-down is re-read whenever a new cluster list arrives
    Given a drill-down is open with filters selected
    When a cluster list read succeeds with at least one cluster
    Then the summary, the findings under the selected filters and the capability record are read again
    And the evidence drawer, if open, is not read again

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:738,824
  # source: pro:src/enginestate.js:201-210
  Scenario: The summary shown is the freshest any view of that cluster has read
    Given another part of the dashboard read a newer engine-state summary of the same cluster
    When the drill-down is redrawn
    Then it shows that newer summary

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:747-766
  Scenario: A summary read overtaken by another view's read is retried, then the known summary is used
    Given the drill-down's summary read is overtaken by another view's read of the same cluster
    When this happens up to 3 times in a row
    Then the drill-down stops retrying and walks the findings of the report the latest known summary names

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:757-771
  Scenario: A refused summary re-read still re-walks the findings of the last known report
    Given the drill-down holds a summary read earlier that names retained detail
    When a summary re-read is refused
    Then the findings of the report that earlier summary names are walked again under the selected filters

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:37-38,675-962
  Scenario: The drill-down offers no action on the cluster
    When the drill-down is shown to any user
    Then its only controls are "Close", the three filters, "Evidence" per finding, "Reload findings" after a changed report, and navigation links
    And none of them changes anything on the hub or the cluster
