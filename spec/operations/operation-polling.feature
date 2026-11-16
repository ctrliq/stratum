Feature: Operation progress polling
  While the Operations page is open it keeps re-reading every tracked
  operation: at a fixed pace while the hub answers, more slowly while it
  does not, and never for an operation the hub has said it no longer holds.

  Background:
    Given the Operations page tracks at least one operation

  @pro @fleet
  # source: pro:src/screens/operations.jsx:84-109
  Scenario: Tracked operations are read as soon as the page opens
    When the page opens
    Then each tracked operation is read at once, one after the other in the tracked order

  @pro @fleet
  # source: pro:src/screens/operations.jsx:104-105
  # source: pro:src/poll.js:9
  Scenario: While every read succeeds the operations are re-read every 15 seconds
    Given every tracked operation was read successfully in the last round
    When 15 seconds pass after that round finished
    Then the next round starts

  @pro @fleet
  # source: pro:src/screens/operations.jsx:95-96
  Scenario: A successful read updates the card
    Given a card shows an operation
    When a later read of it succeeds with new state, outcome, counts or targets
    Then the card shows the new values

  @drop @pro @fleet
  # decision: D8
  # source: pro:src/screens/operations.jsx:46,93
  # source: pro:src/fleet.js:71-87
  # source: pro:src/fleet.js:207-227
  Scenario: A read that takes longer than 8 seconds is given up
    Given the hub accepts a request of an operation read, the first page or any later page of its targets, but never answers
    When 8 seconds pass
    Then that request is cancelled and the read counts as failed
    And a read over several pages can therefore take 8 seconds per page

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/screens/operations.jsx:46,93
  # source: pro:src/fleet.js:207-227
  Scenario: An operation request that takes longer than 15 seconds is given up
    Given the hub accepts a request of an operation read but never answers
    When 15 seconds pass
    Then that request is cancelled and the read counts as failed

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:60-62,95-101
  Scenario: A failed read keeps the card as it was
    Given a card shows an operation read earlier
    When a later read of it fails
    Then the card keeps showing the earlier values
    And nothing on the page says the values are stale

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:60-62,95-101
  Scenario: A card kept through failed reads is marked as not current
    Given a card shows an operation read earlier
    When a later read of it fails
    Then the card keeps showing the earlier values
    And it states that the latest read failed, with the hub's message

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:95-101,155-163
  Scenario: An operation that has never been read successfully stays on its loading card
    Given the reads of "op-1" keep failing with something other than 404 or 410
    When rounds keep running
    Then the card keeps reading "Loading op-1…" and no error is shown

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:95-101,155-163
  Scenario: An operation that cannot be read shows why
    Given the reads of "op-1" fail with something other than 404 or 410
    When a round finishes
    Then the card for "op-1" shows the hub's message, or that the hub could not be reached
    And "op-1" keeps being retried

  @pro @fleet
  # source: pro:src/screens/operations.jsx:95-101
  Scenario Outline: What counts as a failed read
    When the read of a tracked operation ends with <answer>
    Then the round counts as <verdict>

    Examples:
      | answer                                              | verdict   |
      | a success carrying the operation's id               | healthy   |
      | a success whose body carries no operation id        | unhealthy |
      | a success with an empty body                        | unhealthy |
      | status 403                                          | unhealthy |
      | status 429                                          | unhealthy |
      | status 500                                          | unhealthy |
      | no response at all                                  | unhealthy |
      | no answer within 8 seconds                          | unhealthy |
      | status 404                                          | healthy, and the operation is marked gone |
      | status 410                                          | healthy, and the operation is marked gone |

  @drop @pro @fleet
  # decision: D9
  # source: pro:src/screens/operations.jsx:88-105
  Scenario: One failed read slows the whole round, not just that operation
    Given the page tracks "op-1" and "op-2"
    When the read of "op-1" fails and the read of "op-2" succeeds
    Then "op-2" is updated
    And the next round, for both, is scheduled with the back-off delay

  @fixed @pro @fleet
  # decision: D9
  # source: pro:src/screens/operations.jsx:88-105
  Scenario: A failing operation backs off on its own
    Given the page tracks "op-1" and "op-2"
    When the reads of "op-1" keep failing and the reads of "op-2" succeed
    Then "op-2" keeps being re-read every 15 seconds
    And only "op-1" is retried with the back-off delay

  @pro @fleet
  # source: pro:src/screens/operations.jsx:104-105
  # source: pro:src/poll.js:9-26
  Scenario Outline: Consecutive unhealthy rounds back off exponentially with jitter
    Given <rounds> rounds in a row were unhealthy
    When the next round is scheduled
    Then it starts between <min> and <max> seconds later

    Examples:
      | rounds | min  | max   |
      | 1      | 15   | 17.25 |
      | 2      | 25.5 | 34.5  |
      | 3      | 51   | 69    |
      | 4      | 102  | 120   |
      | 5      | 102  | 120   |
      | 20     | 102  | 120   |

  @pro @fleet
  # source: pro:src/poll.js:19-26
  Scenario: The back-off delay is never shorter than 15 seconds nor longer than 2 minutes
    Given any number of unhealthy rounds in a row
    When the next round is scheduled
    Then the delay is at least 15 seconds and at most 120 seconds
    And it varies by up to 15 percent either way so that many browsers do not retry in step

  @pro @fleet
  # source: pro:src/screens/operations.jsx:104-105
  Scenario: One healthy round restores the 15 second pace
    Given several rounds in a row were unhealthy
    When a round completes with every read healthy
    Then the next round starts 15 seconds later

  @pro @fleet
  # source: pro:src/screens/operations.jsx:63-73,92,97-98
  Scenario: An operation the hub no longer holds is not asked again
    Given the read of "op-9" answered 404 or 410
    When later rounds run
    Then "op-9" is not requested again
    And the other tracked operations keep being read

  @pro @fleet
  # source: pro:src/screens/operations.jsx:97-104
  Scenario: A gone answer does not slow the polling
    Given the only thing that went wrong in a round was a 404 or 410 for one operation
    When the next round is scheduled
    Then it starts 15 seconds later

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:48,84-109
  Scenario Outline: Polling continues after an operation has finished
    Given a tracked operation is in the state "<state>"
    When later rounds run
    Then the operation is still re-read every round

    Examples:
      | state     |
      | completed |
      | canceled  |
      | expired   |

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:48,84-109
  Scenario Outline: An operation that has finished is no longer polled
    Given a tracked operation was read in the state "<state>"
    When later rounds run
    Then the operation is not requested again and its card stays as last read

    Examples:
      | state     |
      | completed |
      | canceled  |
      | expired   |

  @pro @fleet
  # source: pro:src/screens/operations.jsx:84-109
  # source: pro:src/app.jsx:270-299
  Scenario: Pausing the dashboard's auto-refresh does not pause operation polling
    Given the user paused auto-refresh
    When the Operations page is open
    Then tracked operations keep being re-read

  @drop @pro @fleet
  # decision: D8
  # source: pro:src/screens/operations.jsx:84-85,108-109
  # source: pro:src/fleet.js:207-227
  Scenario: Changing the tracked set restarts the polling
    Given the page tracks "op-1"
    When the URL changes to track "op-1" and "op-2"
    Then a new round starts at once for the new set, at the 15 second pace
    And the pages of a target set still being read for the old set are requested to the end, and their answers are ignored

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/screens/operations.jsx:84-85,108-109
  Scenario: Changing the tracked set starts a new round at once
    Given the page tracks "op-1"
    When the URL changes to track "op-1" and "op-2"
    Then a new round starts at once for the new set, at the 15 second pace

  @drop @pro @fleet
  # decision: D8
  # source: pro:src/screens/operations.jsx:108
  # source: pro:src/fleet.js:207-227
  Scenario: Leaving the page stops the polling
    When the user navigates away from the Operations page
    Then no new round starts and no further operation is read
    And the pages of a target set still being read are requested to the end
    And every answer still pending is ignored

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/screens/operations.jsx:108
  # source: pro:src/fleet.js:207-227
  Scenario: Leaving the page or changing the tracked set cancels the reads in flight
    Given an operation read, including one part-way through its target pages, has not finished
    When the user navigates away from the Operations page or the tracked set changes
    Then the requests in flight are cancelled and no further page is requested for them

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:95-96,155-163
  Scenario: An answer is filed under the id the hub returns
    Given the page tracks "op-1"
    When the hub answers the read of "op-1" with an operation whose id differs from "op-1"
    Then the card for "op-1" keeps reading "Loading op-1…"

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/operations.jsx:95-96,155-163
  Scenario: An answer is shown on the card of the operation that was asked for
    Given the page tracks "op-1"
    When the hub answers the read of "op-1" successfully
    Then the card for "op-1" shows that answer

  @pro @fleet
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  # source: pro:src/auth-redirect.js:12-16
  Scenario: An expired session sends the user to sign-in and remembers the tracked operations
    Given the hub requires sign-in
    When a read of a tracked operation answers 401
    Then the sign-in screen is shown
    And the Operations page address, including its "op" parameters, is remembered for after sign-in

  @pro @fleet
  # source: pro:src/fleet.js:65-98,207
  Scenario: Operation reads are plain session reads
    When a tracked operation is read
    Then the request is a GET that accepts JSON and carries the session cookie
    And it carries no anti-forgery token

  @pro @fleet
  # source: pro:src/screens/operations.jsx:62,155-163
  Scenario: A re-tracked operation shows its last known card at once
    Given the page showed a card for "op-1" and "op-1" was then dropped from the URL
    When "op-1" is added to the URL again without leaving the page
    Then its last known card, data or gone, is shown at once instead of the loading card
