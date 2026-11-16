Feature: Fleet engine-state summary
  The Clusters page opens with the hub's fleet-wide engine-state roll-up:
  how many clusters sit at each severity. The numbers are the hub's own;
  the dashboard never averages or re-derives them.

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:184
  # source: pro:src/ui/enginestate.jsx:323-332
  Scenario: The fleet page carries the fleet engine-state card
    When the user opens the Clusters page
    Then a card titled "Fleet Engine State" with the subtitle "the hub's per-cluster severity roll-up" is shown above the search box

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:156-167,337-340
  Scenario: The card says it is reading until the first answer
    Given the fleet engine-state has never been read in this visit
    When the Clusters page opens and the hub has not answered
    Then the card reads "reading fleet engine state…"

  @drop @pro @fleet
  # decision: D2
  # assumption: derived from the order in which the page's first reads and the initial cluster selection run; a runtime check after a fresh page load would confirm that the first read is discarded
  # source: pro:src/app.jsx:218-230
  # source: pro:src/k8sclient.js:29-46
  # source: pro:src/ui/enginestate.jsx:116-141,156-166
  Scenario: The first engine-state read after a page load is discarded by the initial cluster selection
    Given the dashboard was just loaded, or the selected cluster just changed
    When the fleet engine-state card or the selected cluster's engine-state card makes its first read
    Then the answer of that read is dropped
    And the card keeps its "reading…" text until a later cluster list or refresh triggers another read

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/app.jsx:218-230
  # source: pro:src/ui/enginestate.jsx:116-141,156-166
  Scenario: The first engine-state read after a page load is shown
    Given the dashboard was just loaded, or the selected cluster just changed
    When the fleet engine-state card or the selected cluster's engine-state card makes its first read
    Then the card shows the answer of that read

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:341-368
  Scenario: The card shows the hub's roll-up
    Given the hub's fleet summary reports 4 clusters, counts healthy 2, warning 1, critical 1, 0 clusters with unknown required coverage and an evaluation time 30 seconds ago
    When the card is shown
    Then it reads "4 clusters" and "evaluated 30s ago"
    And under "Clusters by severity" it shows "healthy 2", "info 0", "unknown 0", "warning 1" and "critical 1" in that order
    And under "Required coverage" it reads "0 clusters with unknown required coverage"
    And it shows "Evaluated" with the evaluation time in the browser's local date-time format

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:61,352-354
  Scenario: Every severity is listed even when its count is zero or missing
    Given the hub's fleet summary carries counts for only some severities
    When the card is shown
    Then all five severities are listed in the order healthy, info, unknown, warning, critical
    And a severity without a count reads 0

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:346,360
  Scenario Outline: Counts of one use the singular
    Given the hub's fleet summary reports <clusters> clusters and <unknown> with unknown required coverage
    When the card is shown
    Then it reads "<clusterText>" and "<coverageText>"

    Examples:
      | clusters | unknown | clusterText | coverageText                                |
      | 1        | 1       | 1 cluster   | 1 cluster with unknown required coverage    |
      | 0        | 0       | 0 clusters  | 0 clusters with unknown required coverage   |
      | 7        | 3       | 7 clusters  | 3 clusters with unknown required coverage   |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:346,359-360
  Scenario: Missing counts are shown as placeholders
    Given the hub's fleet summary carries neither a cluster count nor an unknown-coverage count
    When the card is shown
    Then it reads "— clusters" and "0 clusters with unknown required coverage"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:359
  Scenario Outline: Unknown required coverage is emphasised only when present
    Given the hub's fleet summary reports <unknown> clusters with unknown required coverage
    When the card is shown
    Then the required-coverage line is <emphasis>

    Examples:
      | unknown | emphasis                 |
      | 0       | shown without emphasis   |
      | 1       | shown as a warning       |
      | 5       | shown as a warning       |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:333-335
  Scenario Outline: A partial snapshot is labelled
    Given the hub's fleet summary states its completeness as <complete>
    When the card is shown
    Then a "partial snapshot" label is <shown>

    Examples:
      | complete | shown     |
      | false    | shown     |
      | true     | not shown |
      | absent   | not shown |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:61-62
  Scenario Outline: Each severity has a fixed tone
    When a count or verdict for the severity "<severity>" is shown
    Then it uses the <tone> tone

    Examples:
      | severity | tone          |
      | healthy  | positive      |
      | info     | informational |
      | unknown  | neutral       |
      | warning  | warning       |
      | critical | critical      |
      | other    | neutral       |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:70-88
  Scenario Outline: Ages are shown in one coarse unit
    Given a time that lies <elapsed> before the moment the card was drawn
    When its age is shown
    Then it reads "<age>"

    Examples:
      | elapsed              | age      |
      | 0 seconds            | 0s ago   |
      | 59 seconds           | 59s ago  |
      | 59.6 seconds         | 1m ago   |
      | 60 seconds           | 1m ago   |
      | 119 seconds          | 1m ago   |
      | 59 minutes 59 seconds | 59m ago |
      | 1 hour               | 1h ago   |
      | 23 hours 59 minutes  | 23h ago  |
      | 24 hours             | 1d ago   |
      | 10 days              | 10d ago  |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:70-88,347
  Scenario Outline: An age that cannot be computed is shown as a dash
    Given an evaluation time that is <problem>
    When its age is shown
    Then the card reads "evaluated —"

    Examples:
      | problem                              |
      | missing                              |
      | not a parseable time                 |
      | later than the moment the card drew  |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:65-69,365
  Scenario Outline: Timestamps are shown in the browser's local format
    Given an evaluation time of "<value>"
    When the "Evaluated" line is shown
    Then it reads <shown>

    Examples:
      | value                | shown                                         |
      | 2026-11-13T12:00:00Z | that instant in the browser's locale and zone |
      | garbage              | "garbage"                                     |
      |                      | "—"                                           |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:276,323
  Scenario: Ages do not tick between refreshes
    Given the card shows "evaluated 30s ago"
    When time passes without the page being redrawn
    Then the text does not change until the next redraw

  # ---- refusals -----------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:92,337-340
  Scenario: A refused first read quotes the hub
    Given the fleet engine-state has never been read successfully in this visit
    When the hub refuses the read with the message "insufficient role"
    Then the card reads "fleet engine state unavailable — insufficient role"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:166,367
  # source: pro:src/enginestate.js:180-189
  Scenario: A refused later read keeps the last good roll-up
    Given the card shows a roll-up read earlier
    When a later read is refused with the message "hub overloaded"
    Then the earlier roll-up stays on the card
    And the card adds "the latest read was refused — hub overloaded"

  @pro @fleet
  # source: pro:src/enginestate.js:59,187
  # source: pro:src/ui/enginestate.jsx:166
  Scenario: The last good roll-up survives leaving and reopening the page
    Given the fleet engine-state was read successfully earlier in this visit
    When the user leaves the Clusters page and comes back
    Then the earlier roll-up is shown at once while the new read is pending

  @pro @fleet
  # assumption: holds once the page is redrawn with the new list; a runtime check with the selected cluster unreachable would confirm whether the re-read then lags one refresh behind
  # source: pro:src/screens/clusters.jsx:176-184
  # source: pro:src/ui/enginestate.jsx:158-165
  # source: pro:src/live.js:297-298
  Scenario: The roll-up is re-read whenever a new cluster list arrives
    Given the Clusters page is open
    When a cluster list read succeeds with at least one cluster
    Then the fleet engine-state is read again

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:184
  # source: pro:src/live.js:297-306
  Scenario: The roll-up is not re-read when the cluster list read fails
    Given the Clusters page is open
    When a cluster list read fails or returns no clusters
    Then the fleet engine-state is not read again

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:184
  # source: pro:src/ui/enginestate.jsx:158-165
  Scenario: The roll-up is re-read on every refresh interval
    Given the Clusters page is open and auto-refresh is on
    When a refresh interval passes
    Then the fleet engine-state is read again, whatever the cluster list read answered

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:159-164
  Scenario: An answer that arrives after a newer read started is not shown as the latest outcome
    Given a fleet engine-state read is pending
    When a new read starts before it answers
    Then only the newer read decides what refusal, if any, the card shows

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:323-372
  # source: pro:src/screens/clusters.jsx:184
  Scenario: The fleet card is the same for admins and read-only users
    Given the signed-in user has no "admin" role
    When the user opens the Clusters page
    Then the fleet engine-state card is shown with the same content and offers no action
