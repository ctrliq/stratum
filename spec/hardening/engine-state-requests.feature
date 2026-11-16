Feature: Engine-state requests and their outcomes
  Every engine-state view (fleet summary, cluster summary, findings,
  evidence, capability record) reads the hub the same way. This feature
  fixes the rules those views share: deadlines, how each kind of answer is
  turned into the message the views quote, and which answers are dropped.

  @pro @fleet
  # source: pro:src/enginestate.js:105-132
  Scenario: Engine-state reads are plain session reads
    When any engine-state view reads the hub
    Then the request is a GET that accepts JSON and carries the session cookie
    And it carries no anti-forgery token

  @pro @fleet
  # decision: D8
  # source: pro:src/enginestate.js:55,105-128,151-152
  Scenario: A read that takes longer than 15 seconds is given up as retryable
    Given the hub accepts an engine-state request but never answers
    When 15 seconds pass
    Then the request is cancelled
    And the view reports "the engine-state request timed out" marked as retryable

  @pro @fleet
  # source: pro:src/enginestate.js:143-174
  Scenario Outline: Each kind of answer becomes one message the views quote
    When an engine-state read ends with <answer>
    Then the message the view quotes is <message>
    And the outcome is <retryable>

    Examples:
      | answer                                                             | message                                              | retryable     |
      | a network failure with a browser error message                     | the browser's error message                          | not retryable |
      | a network failure without any error message                        | "the engine-state request failed"                    | not retryable |
      | no answer within the deadline                                      | "the engine-state request timed out"                 | retryable     |
      | a success whose body is not a JSON object                          | "the engine-state response was not a JSON object"    | not retryable |
      | a refusal with a JSON body carrying a message                      | the body's message                                   | as the body's retryable flag says |
      | a refusal with a JSON body carrying only an error code             | the error code                                       | as the body's retryable flag says |
      | a refusal with a non-JSON body                                     | the body text, trimmed                               | retryable only for status 429 or 5xx |
      | a refusal with an empty body and status 503                        | "HTTP 503"                                           | retryable     |
      | a refusal with an empty body and status 403                        | "HTTP 403"                                           | not retryable |

  @pro @fleet
  # source: pro:src/enginestate.js:169
  Scenario Outline: A JSON refusal is retryable only when the hub says so
    When an engine-state read is refused with status <status> and a JSON body whose retryable flag is <flag>
    Then the outcome is <retryable>

    Examples:
      | status | flag    | retryable     |
      | 503    | true    | retryable     |
      | 503    | false   | not retryable |
      | 503    | absent  | not retryable |
      | 409    | true    | retryable     |
      | 409    | absent  | not retryable |

  @pro @fleet
  # source: pro:src/enginestate.js:159-173
  Scenario: A refusal keeps the facts the hub attached to it
    When an engine-state read is refused
    Then the outcome keeps the status, the error code, the request id and any report generation the body names
    And it keeps the retained-detail window from the "X-Stratum-Retained-From" and "X-Stratum-Retained-To" response headers
    And a fact the hub did not send stays absent rather than blank

  @pro @fleet
  # source: pro:src/enginestate.js:244-246,292-294,315-317
  Scenario Outline: A successful answer of the wrong shape is an error, never an empty result
    When the <route> read succeeds with a JSON object that lacks <field>
    Then the view reports "<message>"

    Examples:
      | route             | field              | message                                                         |
      | findings          | a list of findings | the findings route did not answer a ReportDetail page           |
      | evidence          | a list of items    | the evidence route did not answer an EvidencePage               |
      | capability record | a list of items    | the capabilities route did not answer a capability record       |

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/enginestate.js:105-132,148-174
  # source: pro:src/fleet.js:89
  Scenario: An expired session on an engine-state read does not open the sign-in screen
    Given the hub requires sign-in and the session has expired
    When an engine-state read answers 401
    Then the view quotes the hub's message like any other refusal
    And the user is not sent to the sign-in screen by that read

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/enginestate.js:105-132,148-174
  # source: pro:src/fleet.js:89
  Scenario: An expired session on an engine-state read opens the sign-in screen
    Given the hub requires sign-in and the session has expired
    When an engine-state read answers 401
    Then the sign-in screen is shown and the current page is remembered for after sign-in

  @pro @fleet
  # source: pro:src/enginestate.js:148-174
  Scenario: A forbidden engine-state read is quoted like any other refusal
    When an engine-state read answers 403 with a message
    Then the view quotes that message and shows no data it did not already have

  @pro @fleet
  # source: pro:src/enginestate.js:180-189,201-210,264-272,281-298,308-321
  # source: pro:src/k8sclient.js:29-46
  Scenario: Switching cluster drops every engine-state answer still in flight
    Given engine-state reads are pending
    When the selected cluster changes before they answer
    Then none of their answers, data or refusals, is shown

  @pro @fleet
  # source: pro:src/enginestate.js:181-184,202-205,223,241,282,289,309-312
  # source: pro:src/k8sclient.js:39-46
  Scenario: A newer read of the same thing wins over an older one
    Given two reads of the same engine-state surface of the same cluster overlap
    When the older one answers after the newer one started
    Then the older answer is dropped, whether it is data or a refusal

  @pro @fleet
  # source: pro:src/enginestate.js:186-188,207-209,265-272,295-297,318-320
  Scenario: A failed read never erases the last good value
    Given a view shows engine-state data read earlier
    When a later read of it fails, is dropped or is cancelled
    Then the earlier data is still available to the view

  @pro @fleet
  # source: pro:src/enginestate.js:94-100
  Scenario: Identifiers are escaped in engine-state request paths
    Given a cluster id, report generation or evidence reference containing characters such as "/" or spaces
    When an engine-state request is built for it
    Then each identifier is percent-encoded in the path

  # ---- the findings walk ----------------------------------------------------------

  @pro @fleet
  # source: pro:src/enginestate.js:220-256
  Scenario: Findings are read page by page until the hub mints no further cursor
    Given the hub serves a report's findings in 3 pages, each naming a cursor for the next except the last
    When the findings are read
    Then 3 requests are made, the second and third carrying the cursor the previous page named
    And the findings of all pages are shown in the order served

  @pro @fleet
  # source: pro:src/enginestate.js:224-238
  Scenario: Every page of a filtered walk repeats the filters
    Given the findings are read with a signal, a node and a minimum-severity filter
    When the walk follows a cursor
    Then each page request carries the same three filters beside the cursor

  @pro @fleet
  # source: pro:src/enginestate.js:225-229
  Scenario: Empty filters are not sent
    Given no filter is selected
    When the first findings page is requested
    Then the request carries no query parameters

  @pro @fleet
  # source: pro:src/enginestate.js:253-254
  Scenario Outline: A page without a usable cursor ends the walk
    When a findings page carries a next cursor that is <cursor>
    Then no further page is requested

    Examples:
      | cursor           |
      | absent           |
      | null             |
      | an empty string  |

  @pro @fleet
  # source: pro:src/enginestate.js:238-242
  Scenario: A page that fails mid-walk fails the whole walk
    Given the first findings page was served
    When the second page is refused
    Then no findings of this walk are shown
    And the refusal of the second page is what the view reports

  @pro @fleet
  # source: pro:src/enginestate.js:257-263
  Scenario: The walk's totals are the last page's
    Given a findings walk of several pages
    When the walk completes
    Then the total, the completeness and the state shown are those the last page stated

  @pro @fleet
  # source: pro:src/enginestate.js:259-261
  Scenario Outline: Missing totals on the last page fall back safely
    Given the last findings page <omission>
    When the walk completes
    Then <fallback>

    Examples:
      | omission                              | fallback                                              |
      | carries no numeric total              | the total is the number of findings actually walked   |
      | does not state completeness as true   | the walk is treated as incomplete                     |
      | carries no state text                 | the state is shown as "—"                             |

  @pro @fleet
  # source: pro:src/enginestate.js:90-92,232,249-251
  Scenario: An observation repeated on several pages is kept once
    Given the same observation (same signal and subject) is attached to two pages of a walk
    When the walk completes
    Then it is kept once, as the later page stated it

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/enginestate.js:233-256
  Scenario: A hub that keeps minting cursors keeps the walk going
    Given the hub answers every findings page with a new or repeated cursor
    When the findings are read
    Then requests continue until a page fails, the view is superseded or the caller cancels

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/enginestate.js:233-256
  # source: pro:src/live.js:880,930-953
  Scenario: A findings walk is bounded
    Given the hub answers every findings page with another cursor
    When the hub repeats a cursor it already issued, or 100 pages have been read
    Then the walk stops
    And the findings read so far are shown, labelled as not the whole set

  # ---- the evidence read -----------------------------------------------------------

  @pro @fleet
  # source: pro:src/enginestate.js:281-298
  Scenario: Evidence is read as one fragment and never paged further
    Given an evidence answer that names a cursor for more items
    When the evidence is read
    Then only that one request is made
    And the items it carried are what the view shows
