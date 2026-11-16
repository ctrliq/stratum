Feature: Version comparison on the Nodes screen
  The fleet dashboard shows, per node and component, the expected, installed
  and serving versions and the hub's verdict on them, taken from the hub's
  engine-state evidence. The dashboard displays the hub's values and grades
  verbatim and computes none of its own.

  # assumption: tagged @fleet in the sense "the backend is the hub" (D4): the
  # evidence is loaded in the hub's standalone mode too (pro:src/live.js:233-239).
  # Running the hub with mode "standalone" and opening Nodes would confirm the
  # card appears there; if @fleet is meant as "mode is fleet", drop the tag here.

  # ------------------------------------------------------------ visibility

  # source: pro:src/screens/infra.jsx:322-325,397,456
  @pro @fleet
  Scenario: No comparison card is shown when no engine-state evidence was loaded
    Given the hub has returned no engine-state result for the cluster
    When the Nodes screen is shown
    Then neither the Version comparison card nor an unavailability notice is shown

  # source: pro:src/screens/infra.jsx:208-215,456-468
  @pro @fleet
  Scenario Outline: A refused evidence load states why no versions are shown
    Given the hub refused the engine-state load with the reason "<reason>"
    When the Nodes screen is shown
    Then a notice reads "Version comparison unavailable — <text>. No values are shown rather than guessed."
    And no comparison rows are shown

    Examples:
      | reason              | text                                                         |
      | detail-expired      | the report detail expired                                    |
      | generation-changed  | the report generation moved on before the view could read it |
      | summary-unavailable | the engine state summary is unavailable                      |
      | anything else       | the engine state could not be read                           |

  # source: pro:src/screens/infra.jsx:462-466
  @pro @fleet
  Scenario: The unavailability notice still lists nodes with kubelet drift
    Given the engine-state load was refused
    And node "worker-2" has a stamped version that differs from its kubelet version
    When the Nodes screen is shown
    Then the notice shows the same drift badge for "worker-2" that the node table shows

  # ----------------------------------------------------------------- header

  # source: pro:src/screens/infra.jsx:397-409
  @pro @fleet
  Scenario: The card header carries the hub's roll-up
    Given evidence observed at "2026-01-05T10:00:00Z" with roll-up severity "warning", freshness "fresh" and coverage 5 of 6 evaluated with 1 unknown
    When the Nodes screen is shown
    Then the card subtitle includes "observed 2026-01-05T10:00:00Z"
    And it includes "hub rollup warning · reporting fresh"
    And it includes "coverage 5/6, 1 unknown"

  # source: pro:src/screens/infra.jsx:404-408
  @pro @fleet
  Scenario: Missing roll-up values are shown as unknown, never guessed
    Given evidence with no observation time, no severity, no freshness and no coverage
    When the Nodes screen is shown
    Then the subtitle shows "hub rollup unknown · reporting unknown"
    And it shows no observation time and no coverage

  # source: pro:src/screens/infra.jsx:410-414
  @pro @fleet
  Scenario: A partial snapshot is called out
    Given the hub reports the snapshot behind the evidence as partial
    When the Nodes screen is shown
    Then a warning reads "Partial snapshot — the hub reports the snapshot behind this report as partial, so findings from some sources may be missing."

  # source: pro:src/screens/infra.jsx:415-419
  @pro @fleet
  Scenario: A truncated findings walk is called out
    Given the findings stopped at the hub's page bound
    When the Nodes screen is shown
    Then a warning reads "Partial detail — the hub's findings walk stopped at its page bound, so nodes beyond it are not listed."

  # source: pro:src/screens/infra.jsx:421-425
  @pro @fleet
  Scenario: Kubelet drift badges are repeated in the card header
    Given 2 nodes have a stamped version that differs from their kubelet version
    When the Nodes screen is shown
    Then the card header shows one drift badge per drifting node, identical to the node table's

  # ------------------------------------------------------------------- rows

  # source: pro:src/screens/infra.jsx:427-431
  @pro @fleet
  Scenario: Evidence with no findings shows an empty state
    Given the evidence load succeeded with no version findings
    When the Nodes screen is shown
    Then the card reads "No version evidence reported for this cluster yet."

  # source: pro:src/screens/infra.jsx:432-451
  # source: pro:src/version-view.js:199-209
  @pro @fleet
  Scenario: One row is shown per node and component in the hub's order
    Given the hub served findings for 2 components on each of 2 nodes
    When the Nodes screen is shown
    Then the comparison table has 4 rows with the columns Node, Component, Expected, Installed, Serving and Verdict
    And the rows appear in the order the hub served the findings
    And a missing node or component identifier shows "—"

  # source: pro:src/screens/infra.jsx:109-122
  @pro @fleet
  Scenario: Expected and Installed show the reported value verbatim or an explicit Unknown
    Given a row whose expected value is "v1.36.4" and whose installed value was not readable for the reason "source-timeout: member did not answer"
    When the Nodes screen is shown
    Then Expected shows "v1.36.4"
    And Installed shows "Unknown" with the tooltip "source-timeout: member did not answer"
    And an Unknown with no reason has the tooltip "no installed value was reported"

  # source: pro:src/screens/infra.jsx:129-147
  # source: pro:src/version-view.js:103-114
  @pro @fleet
  Scenario Outline: The Serving column distinguishes running, installed-only and unknown
    Given a row where <situation>
    When the Nodes screen is shown
    Then Serving shows "<shown>"

    Examples:
      | situation                                                              | shown     |
      | a serving value "v1.36.4" was read                                     | v1.36.4   |
      | the component runs from disk and has no serving process                | Installed |
      | a serving process is expected but its value could not be read          | Unknown   |

  # source: pro:src/screens/infra.jsx:165-176
  @pro @fleet
  Scenario: The verdict is the hub's own word
    Given a row whose comparison verdict is "drift" with the reason "version-mismatch: installed differs from expected"
    When the Nodes screen is shown
    Then Verdict shows "drift" with the tooltip "version-mismatch: installed differs from expected"
    And a row with no verdict shows "unknown"

  # source: pro:src/screens/infra.jsx:167-185
  # source: pro:src/version-view.js:52-53,145-146,163-164
  @pro @fleet
  Scenario Outline: A drift the hub grades as explained by a rollout shows the rollout
    Given a drift row the hub graded as explained, with no other finding for it
    And its rollout intent reports a phase
    And its rollout intent states the action "<action>", the target release "r-42" and the deadline "2026-01-05T12:00:00Z"
    When the Nodes screen is shown
    Then an info badge reads "<label> → r-42"
    And the deadline "2026-01-05T12:00:00Z" is shown beside it

    Examples:
      | action             | label                      |
      | release.upgrade    | Upgrading                  |
      | release.rollback   | Rolling back               |
      | not stated         | Rollout, direction unknown |

  # source: pro:src/version-view.js:145-146
  # source: pro:src/screens/infra.jsx:169,177
  @pro @fleet
  Scenario: A rollout does not explain a drift the hub also reports a loss for
    Given a drift row with a rollout intent and an additional reporting-loss finding from the hub
    When the Nodes screen is shown
    Then no rollout badge is shown
    And the reporting-loss finding is shown as its own badge

  # source: pro:src/screens/infra.jsx:177-182
  @pro @fleet
  Scenario: A rollout badge without a target release says the release is unknown
    Given an explained drift row whose rollout intent names no target release
    When the Nodes screen is shown
    Then the badge ends with "→ unknown release"
    And its tooltip states the phase, the operation, the action, the intended release as "an unstated release" and the deadline

  # source: pro:src/screens/infra.jsx:186-191
  @pro @fleet
  Scenario: A drift with no reported rollout says the intent is unknown
    Given a row whose verdict is "drift" and for which no rollout intent was reported
    When the Nodes screen is shown
    Then the text "intent unknown" is shown beside the verdict

  # source: pro:src/screens/infra.jsx:170,192-197
  @pro @fleet
  Scenario: Every other hub finding for the row is a badge of its own
    Given a row with a hub finding "source-missing" of severity "warning" and a finding "epoch-conflict" of severity "info"
    When the Nodes screen is shown
    Then "source-missing" is shown as a warning badge and "epoch-conflict" as a gray badge
    And each badge's tooltip gives the finding's severity and message
    And findings whose code begins with "outcome-" are not shown as badges

  # ------------------------------------------------------------ drill-down

  # source: pro:src/screens/infra.jsx:235-297,440-441,453
  @pro @fleet
  Scenario: Selecting a comparison row opens its full detail under the table
    When the user selects a comparison row
    Then a "Version comparison detail" card opens below the table
    And it lists the node, the component, every hub finding with code, severity and message, the comparison rule, the verdict and its reasons
    And it lists the member release baseline, or "none recorded"
    And it lists Expected, Installed and Serving, each with its digest, availability and reason when present
    And it lists every serving instance separately when the hub reported several
    And it lists the report generation and the sample time

  # source: pro:src/screens/infra.jsx:274-284
  @pro @fleet
  Scenario: The detail shows the raw rollout intent or says none was reported
    Given the detail of a row with a rollout intent is open
    Then it lists the operation, its target, action, phase, intended release, observation time and deadline
    And an intent without an action reads "not stated — direction unknown"
    And a row with no intent reads "unknown — no rollout operation was reported for this component"

  # source: pro:src/screens/infra.jsx:285-294
  @pro @fleet
  Scenario: The detail shows where the sample came from
    Given the detail of a row whose observation names its source is open
    Then it lists the producer and build, writer binding, publisher member and node, process instance and epoch, source sequence and process start time

  # source: pro:src/screens/infra.jsx:252,255-258,281
  @pro @fleet
  Scenario: The detail lists observation and baseline reasons and release digests
    Given the detail of a row whose evidence carries them is open
    Then it lists the observation reason and the baseline reason
    And it lists the member release digest and the intended release digest
    And each of these lines is absent when the evidence does not carry it

  # source: pro:src/screens/infra.jsx:240-242,263
  @pro @fleet
  Scenario: The detail names a from-disk component's serving state
    Given the detail of a row for a component that runs from disk is open
    Then Serving reads "Installed (no serving process)"

  # source: pro:src/screens/infra.jsx:247,453
  @pro @fleet
  Scenario: The detail card can be closed
    Given a version comparison detail is open
    When the user presses its close button
    Then the detail card is removed and the table stays

  # source: pro:src/screens/infra.jsx:326-328
  @pro @fleet
  Scenario: An open detail follows the latest evidence
    Given a version comparison detail is open
    When a refresh delivers new values for the same node and component
    Then the detail shows the new values

  # source: pro:src/screens/infra.jsx:326-333
  @pro @fleet
  Scenario: A detail whose row disappears closes and does not reopen
    Given a version comparison detail is open
    When a refresh no longer contains that node and component
    Then the detail card closes
    And it stays closed when a later refresh contains the row again
