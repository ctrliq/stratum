Feature: Track fleet operations
  The Operations page follows the operations named in its URL
  ("/operations?op=<id>", repeatable) and shows, for each, the hub's view of
  the parent operation and of every target it was frozen for. The page has
  no list of all operations and no way to start one: an operation is
  reached only by a link that carries its id.

  # ---- reaching the page ------------------------------------------------------

  @pro @fleet
  # source: pro:src/app.jsx:57-58,169,481-485
  Scenario: The Operations page tracks the operations its URL names
    When the user opens "/operations?op=op-1&op=op-2"
    Then the Operations page is shown tracking "op-1" and "op-2", in that order

  @pro @fleet
  # source: pro:src/app.jsx:193-201,481-485
  Scenario: The page shows a loading line while its code is fetched
    Given the Operations page has not been opened in this visit
    When the user opens it
    Then "Loading operations…" is shown until the page is ready

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/app.jsx:194-200,481-485
  Scenario: A page whose code cannot be fetched stays on the loading line
    Given the Operations page has not been opened in this visit
    When fetching the page's code fails
    Then "Loading operations…" stays, with no error and no retry, until the user navigates away and back

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/app.jsx:194-200,481-485
  Scenario: A page whose code cannot be fetched says so and offers a retry
    Given the Operations page has not been opened in this visit
    When fetching the page's code fails
    Then an error is shown in place of the page, with a way to try again

  @pro @fleet
  # source: pro:src/screens/operations.jsx:75
  Scenario Outline: Tracked ids are de-duplicated and blanks are ignored
    When the user opens "/operations<query>"
    Then the page tracks <tracked>

    Examples:
      | query                    | tracked               |
      | ?op=op-1&op=op-1         | "op-1" once           |
      | ?op=&op=op-1             | "op-1" only           |
      | ?op=                     | nothing               |
      |                          | nothing               |

  @pro @fleet
  # source: pro:src/screens/operations.jsx:138-143
  Scenario Outline: The header counts the tracked operations
    When the page tracks <count> operations
    Then the heading is "Operations"
    And the line under it reads "<count> tracked · remote actions across the fleet, polled live"

    Examples:
      | count |
      | 0     |
      | 1     |
      | 3     |

  @pro @fleet
  # decision: D18
  # source: pro:src/screens/operations.jsx:145-153
  Scenario: With nothing tracked the page explains how operations get here
    When the user opens "/operations" without any "op" parameter
    Then a card reads "No operations tracked. An operation opened from a submit flow carries a link of the form /operations?op=<id> — follow it, or track several with repeated op parameters."
    And no request for any operation is sent

  @pro @fleet
  # decision: D18
  # source: pro:src/app.jsx:345-347
  # source: pro:src/nav.js:5
  # source: pro:src/ui/enginestate.jsx:422
  Scenario: Every link inside the dashboard opens the page with nothing tracked
    When the user opens the Operations page from the sidebar or from a capability's "Operations" link
    Then the page opens without any "op" parameter and shows the nothing-tracked card

  @pro @fleet
  # source: pro:src/app.jsx:345-347
  Scenario: Leaving the page forgets what was tracked
    Given the page tracks "op-1"
    When the user navigates to another screen and returns through the sidebar
    Then the page tracks nothing

  @drop @pro @fleet
  # decision: D4
  # source: pro:src/app.jsx:57-58,207,481-485
  # source: pro:src/ui/shell.jsx:217
  Scenario: The page is not restricted to fleet mode
    Given the hub runs in standalone mode
    When the user opens "/operations?op=op-1"
    Then the Operations page is shown and tracks "op-1" like in fleet mode

  @fixed @pro @fleet
  # decision: D4
  # source: pro:src/app.jsx:57-58,207,481-485
  # source: pro:src/nav.js:5
  Scenario: The Operations page exists only on a fleet hub
    Given the backend is not a fleet hub
    When the user looks for the Operations page
    Then no navigation entry leads to it and its address is treated as an unknown screen

  @pro @fleet
  # source: pro:src/app.jsx:46,474
  Scenario: The breadcrumb names the page
    When the Operations page is open
    Then the breadcrumb ends with "Operations"

  @pro @fleet
  # source: pro:src/app.jsx:527-529
  Scenario: The page never shows the cluster loading hint
    Given the selected cluster is still loading
    When the Operations page is open
    Then no "Loading <cluster>…" hint is shown

  # ---- one operation ------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/operations.jsx:155-164
  Scenario: A tracked operation shows a loading card until its first good answer
    Given the page tracks "op-1"
    When the hub has not yet answered successfully for "op-1"
    Then a card reads "Loading op-1…"

  @pro @fleet
  # source: pro:src/screens/operations.jsx:195-229
  Scenario: An operation card shows the parent's identity, state, outcome and progress
    Given the hub reports operation "op-1" with action "certificates.check", state "running", outcome "unknown", 2 of 5 targets settled, 1 committed and outcomes "1 success" and "1 failed"
    When the card is shown
    Then it shows the id "op-1"
    And the line "certificates.check · running"
    And one outcome label reading "unknown"
    And "Settled" as "2/5", "Committed" as "1" and "Outcomes" as "1 success · 1 failed"

  @pro @fleet
  # source: pro:src/screens/operations.jsx:221-223
  # source: pro:src/screens/operations.jsx:55-56
  Scenario Outline: Missing progress numbers have fixed fallbacks
    Given the hub reports an operation without <field>
    When the card is shown
    Then "<label>" reads "<shown>"

    Examples:
      | field                    | label     | shown |
      | a settled count          | Settled   | 0/<targets> |
      | a target count           | Settled   | <settled>/0 |
      | a committed count        | Committed | 0     |
      | counts by outcome        | Outcomes  | —     |
      | any entry in its counts by outcome | Outcomes | — |

  @pro @fleet
  # source: pro:src/screens/operations.jsx:55-56,223
  Scenario: The outcome summary lists every outcome class the hub counted, in the hub's order
    Given the hub counts outcomes as "success" 3, "partial" 1 and "failed" 2, in that order
    When the card is shown
    Then "Outcomes" reads "3 success · 1 partial · 2 failed" as plain text

  @pro @fleet
  # source: pro:src/screens/operations.jsx:190-194,212,245
  Scenario: A partial result is never summarised as success
    Given the hub reports the parent outcome "partial" with some targets failed
    When the card is shown
    Then the parent's outcome label reads "partial"
    And each target shows its own outcome label

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/ui/shell.jsx:62-80
  # source: pro:src/screens/operations.jsx:212,245
  Scenario Outline: Outcome labels are printed with the tone the shared status table gives them
    Given the hub reports the outcome "<outcome>" for an operation or a target
    When its outcome label is shown
    Then it reads "<label>" in the <tone> tone

    Examples:
      | outcome   | label     | tone    |
      | unknown   | unknown   | neutral |
      | queued    | queued    | neutral |
      | success   | success   | neutral |
      | partial   | partial   | neutral |
      | failed    | failed    | neutral |
      | Failed    | Failed    | error   |
      | Completed | Completed | success |
      | pending   | Pending   | informational |

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/ui/shell.jsx:62-80
  # source: pro:src/screens/operations.jsx:212,245
  Scenario Outline: Each outcome class has its own tone
    Given the hub reports the outcome "<outcome>" for an operation or a target
    When its outcome label is shown
    Then it reads "<outcome>" in the <tone> tone

    Examples:
      | outcome | tone          |
      | success | success       |
      | failed  | error         |
      | partial | warning       |
      | queued  | informational |
      | unknown | neutral       |
      | other   | neutral       |

  @pro @fleet
  # source: pro:src/ui/shell.jsx:75-80
  # source: pro:src/screens/operations.jsx:212,245
  Scenario: A missing outcome leaves the label empty
    Given the hub reports an operation or a target without an outcome
    When its outcome label is shown
    Then the label is drawn in the neutral tone with no text

  # ---- targets --------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/operations.jsx:231-253
  Scenario: Every target of the operation is listed
    Given the hub reports an operation with targets
    When the card is shown
    Then a table with the columns "Target", "Node", "State", "Outcome", "Committed" and "Completed" lists one row per target
    And each row shows the target's cluster name, node identifier, state, outcome label, "yes" or "no" for committed, and the completion time in the browser's local format

  @pro @fleet
  # source: pro:src/screens/operations.jsx:242-247
  # source: pro:src/screens/operations.jsx:50
  Scenario Outline: Missing target facts are shown as placeholders
    Given a target without <field>
    When its row is shown
    Then "<column>" reads "<shown>"

    Examples:
      | field               | column    | shown |
      | a cluster name      | Target    | —     |
      | a node identifier   | Node      | —     |
      | a state             | State     | —     |
      | a committed flag    | Committed | no    |
      | a completion time   | Completed | —     |

  @pro @fleet
  # source: pro:src/screens/operations.jsx:50,247
  Scenario: An unparseable completion time is shown as received
    Given a target whose completion time is "soon"
    When its row is shown
    Then "Completed" reads "soon"

  @pro @fleet
  # source: pro:src/screens/operations.jsx:232-250
  Scenario: An operation without targets shows an empty table
    Given the hub reports an operation with no targets
    When the card is shown
    Then the targets table shows its header and no rows

  @pro @fleet
  # source: pro:src/fleet.js:207-227
  Scenario: The target table holds the whole target set, not one page
    Given the hub serves an operation's targets in 3 pages, each naming a cursor for the next except the last
    When the operation is read
    Then 3 requests are made, the second and third carrying the cursor the previous page named
    And the table lists the targets of all pages in the order served
    And the header numbers are those of the first page

  @pro @fleet
  # source: pro:src/fleet.js:215
  # source: pro:src/screens/operations.jsx:93-101
  Scenario: A page that fails mid-way keeps the last complete table
    Given an operation is shown from an earlier complete read
    When a later read gets its first page but a following page fails
    Then the table is not replaced with a shorter one
    And the read counts as a failed read

  @pro @fleet
  # source: pro:src/fleet.js:217-221
  Scenario Outline: Paging stops when the hub stops advancing
    Given an operation's page names a next cursor that is <cursor>
    When the operation is read
    Then no further page is requested and what was read is shown

    Examples:
      | cursor                               |
      | absent                               |
      | empty                                |
      | the same as the cursor just used     |

  @pro @fleet
  # source: pro:src/fleet.js:208,214
  Scenario: Operation ids and cursors are escaped in request paths
    Given an operation id or cursor containing characters such as "/" or "&"
    When the operation is read
    Then the id and the cursor are percent-encoded in the request

  # ---- operations the hub no longer holds ----------------------------------------------

  @pro @fleet
  # source: pro:src/screens/operations.jsx:63-73,97-98,155-156,174-188
  Scenario Outline: An operation the hub does not hold is shown with the hub's words
    Given the page tracks "op-9"
    When the hub answers the read of "op-9" with status <status> and the message "<message>"
    Then the card for "op-9" shows its id and "<message>"
    And it shows no state, outcome, numbers or targets

    Examples:
      | status | message                                              |
      | 404    | no such selection or operation is visible            |
      | 410    | the operation is past its retention window           |

  @drop @pro @fleet
  # decision: D3
  # source: pro:src/screens/operations.jsx:182
  # assumption: only reachable if a 404/410 answer yields no message at all; the request layer always falls back to "HTTP <status>"
  Scenario: A gone operation without any hub message uses a fixed line
    Given the hub's 404 or 410 answer yields no message
    When the card is shown
    Then it reads "the hub no longer holds this operation"

  @pro @fleet
  # source: pro:src/screens/operations.jsx:155-158
  Scenario: A gone answer replaces a card that was showing data
    Given the card for "op-1" shows an operation read earlier
    When a later read of "op-1" answers 410
    Then the card is replaced by the gone card with the hub's message

  @pro @fleet
  # source: pro:src/screens/operations.jsx:69-73
  Scenario: The first gone answer for an operation is the one shown
    Given the card for "op-9" shows a gone message
    When the tracked set changes and "op-9" stays tracked
    Then the card keeps the first gone message and "op-9" is not asked again

  # ---- roles -------------------------------------------------------------------------------

  @pro @fleet
  # decision: D14
  # source: pro:src/screens/operations.jsx:59,84-109
  Scenario: A read-only user can follow operations
    Given the signed-in user has no "admin" role
    When the user opens "/operations?op=op-1"
    Then the operation and its targets are shown and kept current like for an admin
