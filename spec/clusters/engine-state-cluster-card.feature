Feature: Engine-state card of the selected cluster
  The selected cluster's overview carries a compact card with the hub's
  engine-state verdict for that cluster and the size of the findings
  behind it. It shares its wording with the drill-down on the Clusters
  page and is kept current by the selected cluster's periodic refresh.

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:276-289
  # source: pro:src/screens/overview.jsx:158
  Scenario: The overview shows the engine-state card of the selected cluster
    Given a cluster is selected
    When its overview is shown
    Then a card titled "Engine State" with the subtitle "the hub's supervisor verdict" is shown

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:278
  Scenario: No card without a known cluster
    Given the selection matches no cluster in the list
    When the overview is shown
    Then no engine-state card is shown

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:290-294
  Scenario Outline: Before any summary is known the card says what it is waiting for
    Given no engine-state summary of the cluster is known
    When the summary read is <state>
    Then the card reads "<text>"

    Examples:
      | state                                              | text                                                    |
      | still pending                                      | reading engine state…                                   |
      | refused with the message "source not connected"    | engine state unavailable — source not connected         |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:288,295-308
  Scenario: The card shows verdict, freshness, coverage, counts and times
    Given the hub's summary for the cluster is known
    When the card is shown
    Then the header shows the severity as the hub states it
    And "Freshness" shows the hub's freshness and "evaluated <age> ago"
    And "Coverage" and "Findings" read as in the drill-down
    And "Evaluated" shows the evaluation time in the browser's local format
    And "Observed" shows the observation time followed by its age in brackets

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:307
  Scenario: A missing observation time is a dash
    Given the hub's summary carries no observation time
    When the card is shown
    Then "Observed" reads "—"
    And the coverage adds "no source sample yet"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:309
  Scenario: A refused re-read keeps the last summary and says so
    Given the card shows a summary read earlier
    When a later summary read is refused with the message "hub overloaded"
    Then the earlier summary stays
    And the card adds "the latest read was refused — hub overloaded"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:254-272,310
  Scenario: The card states how many findings stand behind the verdict
    Given the findings of the summary's report were walked without filters
    When the card is shown
    Then it reads "<total> findings · <state>"
    And it lists no individual finding

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:231-252,261-269,310
  Scenario: The card explains refused, partial and outdated findings like the drill-down
    When the unfiltered findings read is refused, incomplete or belongs to an older report than the summary
    Then the card shows the same notes as the drill-down for that case
    And a changed report offers "Reload findings"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:116-143,150
  Scenario: Reload findings re-reads summary and findings on demand
    Given the card offers "Reload findings"
    When the user clicks it
    Then the summary is read again and the findings of the report it names are walked again

  @pro @fleet
  # source: pro:src/live.js:355-385
  # source: pro:src/enginestate.js:69-83
  # source: pro:src/ui/enginestate.jsx:109-125
  Scenario: A periodic refresh and the card's own reload supersede each other, newest wins
    Given the card's own read or "Reload findings" is still in flight
    When a periodic refresh of the selected cluster starts
    Then the answer of the card's own read is dropped in favour of the refresh
    And a "Reload findings" clicked after that refresh started wins over the refresh in turn
    And when the cluster is unreachable the refresh puts nothing in the dropped read's place

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:130-135
  Scenario: A summary that names no retained detail leads to no findings read
    Given the hub's summary names no retained detail
    When the card loads on its own
    Then no findings are requested

  # ---- kept current by the periodic refresh -----------------------------------

  @pro @fleet
  # source: pro:src/live.js:223-272,401-408
  Scenario: Each successful refresh of the selected cluster re-reads its engine state
    Given the selected cluster is reachable and auto-refresh is on
    When a periodic refresh runs
    Then the cluster's engine-state summary is read
    And the findings of the report generation that summary names are walked without filters
    And the card shows the result

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:116-121
  # source: pro:src/enginestate.js:69-83
  Scenario: The card does not read on its own once the periodic refresh has supplied data
    Given a periodic refresh has already delivered the cluster's engine state in this visit
    When the overview is opened or redrawn
    Then the card shows that data without issuing its own summary and findings requests

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:116-141
  Scenario: The card reads on its own when no refresh has delivered data yet
    Given no periodic refresh has delivered the cluster's engine state in this visit
    When the overview is opened
    Then the card reads the summary and walks the findings itself

  @pro @fleet
  # source: pro:src/live.js:392-408
  # source: pro:src/live.js:361-385,1064-1074
  Scenario: An unreachable cluster gets no engine-state refresh
    Given the selected cluster's reachability probe fails
    When a periodic refresh runs
    Then the unfiltered findings behind this card are not requested by that refresh
    And the cluster's engine-state summary is still requested, but its answer is not shown on the card
    And the card keeps what it showed

  @pro @fleet
  # source: pro:src/live.js:247-250,256-259
  Scenario: A refused engine-state read does not make the cluster unreachable
    Given the selected cluster is reachable
    When the engine-state summary or findings read of a refresh is refused
    Then the refresh still counts as successful and no reconnecting banner is raised
    And the card keeps its last good data and shows the refusal

  @pro @fleet
  # source: pro:src/live.js:211,243-266
  Scenario: A report that moves during the refresh is re-pinned a bounded number of times
    Given the findings read of a refresh is refused with the error code "generation-changed"
    When the refresh handles the refusal
    Then it reads the summary again and walks the findings of the generation the new summary names
    And it does this at most 2 more times within one refresh
    And if the report still moved, the card shows the changed-report note

  @pro @fleet
  # source: pro:src/live.js:256-259
  # source: pro:src/ui/enginestate.jsx:232
  Scenario: A 409 with another error code is shown as a changed report but not re-pinned
    Given the findings read of a refresh is refused with status 409 and an error code other than "generation-changed"
    When the refresh handles the refusal
    Then it does not read the summary again within that refresh
    And the card shows the changed-report note

  @pro @fleet
  # decision: D8
  # source: pro:src/live.js:225-231,879
  Scenario: The engine-state part of a refresh has a 15 second budget
    Given the engine-state reads of a refresh take longer than 15 seconds in total
    When the budget runs out
    Then the card keeps its last good data
    And the refusal shown is "the engine-state refresh timed out", marked retryable

  @pro @fleet
  # source: pro:src/live.js:233-240
  Scenario: Standalone mode reads the engine state of the hub's own cluster record
    Given the hub runs in standalone mode
    When a periodic refresh reads the engine state
    Then it addresses the cluster flagged as the fleet manager in the cluster list
    And it addresses a cluster called "local" when no such record is listed

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:112-115
  # source: pro:src/enginestate.js:69-83
  Scenario: Switching cluster never shows the previous cluster's engine state
    Given the card shows the engine state of cluster "a"
    When the user switches to cluster "b"
    Then answers still pending for "a" are dropped
    And the card shows only what is known about "b"
