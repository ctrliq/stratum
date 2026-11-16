Feature: Cancel a fleet operation
  An admin can ask the hub to cancel an operation that is still live. The
  request records an intent; it never rolls back targets that already
  committed. The control disappears once a cancellation is recorded or the
  operation has reached a terminal state.

  Background:
    Given the Operations page shows the operation "op-1"

  # ---- when the control is offered ---------------------------------------------

  @pro @fleet
  # decision: D14
  # source: pro:src/screens/operations.jsx:48,195-198,213-218
  Scenario Outline: Cancel is offered to admins for operations that are not terminal
    Given the signed-in user is an admin
    And "op-1" is in the state "<state>" with no cancellation requested
    When the card is shown
    Then a "Cancel operation" control with the tooltip "Request cancellation" is <offered>

    Examples:
      | state       | offered     |
      | accepted    | offered     |
      | queued      | offered     |
      | running     | offered     |
      | reconciling | offered     |
      | completed   | not offered |
      | canceled    | not offered |
      | expired     | not offered |

  @pro @fleet
  # source: pro:src/screens/operations.jsx:48,196-197
  Scenario: A state the dashboard does not know counts as live
    Given the signed-in user is an admin
    And "op-1" is in a state other than "completed", "canceled" or "expired", or has no state
    When the card is shown
    Then the cancel control is offered

  @pro @fleet
  # decision: D14
  # source: pro:src/screens/operations.jsx:59,197
  # source: pro:src/fleet.js:48-59
  Scenario: A read-only user is never offered Cancel
    Given the signed-in user has no "admin" role
    And "op-1" is running
    When the card is shown
    Then no cancel control is shown

  @pro @fleet
  # source: pro:src/screens/operations.jsx:197,206
  Scenario: A recorded cancellation is labelled and removes the control
    Given the hub reports that a cancellation was requested for "op-1"
    When the card is shown
    Then a "Cancel requested" label is shown next to the id
    And no cancel control is shown, for any role

  @pro @fleet
  # source: pro:src/screens/operations.jsx:206
  Scenario: The label stays after the operation has finished
    Given the hub reports "op-1" as "canceled" with a cancellation requested
    When the card is shown
    Then the "Cancel requested" label is still shown

  @pro @fleet
  # source: pro:src/screens/operations.jsx:155-158,174-188
  Scenario: Gone and loading cards offer no cancel
    Given "op-1" is shown as a loading card or as a gone card
    When the card is shown to an admin
    Then no cancel control is shown

  # ---- cancelling -------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/operations.jsx:111-115,214
  # source: pro:src/fleet.js:235-236
  # source: pro:src/fleet.js:68
  Scenario: Cancel sends the request at once, without confirmation
    Given the signed-in user is an admin and "op-1" is running
    When the user clicks the cancel control
    Then the hub is asked to cancel "op-1" with an empty JSON object as the body and, when the session has one, the anti-forgery token
    And no confirmation dialog is shown

  @pro @fleet
  # source: pro:src/screens/operations.jsx:74,112-113,131-133,214
  Scenario: While a cancellation is in flight every cancel control is disabled
    Given the page shows several live operations to an admin
    When the user clicks the cancel control of one of them and the hub has not answered
    Then the cancel controls of all shown operations are disabled until the request and its follow-up read have finished

  @pro @fleet
  # source: pro:src/screens/operations.jsx:115-130
  Scenario: An accepted cancellation is shown immediately and confirmed by a fresh read
    Given the signed-in user is an admin and "op-1" is running
    When the hub accepts the cancellation and returns the time it was requested
    Then the card shows "Cancel requested" and loses its cancel control at once
    And the target table keeps the full target set it had
    And the operation is read again in full straight away, replacing the card with that answer
    And once that read has answered, a toast reads "Cancellation requested for op-1 — targets already committed stay committed"

  @pro @fleet
  # source: pro:src/screens/operations.jsx:123-126
  Scenario: A failed follow-up read keeps the immediate result
    Given the hub accepted the cancellation of "op-1"
    When the read that follows it fails
    Then the card keeps showing "Cancel requested" with its earlier values
    And the success toast is still shown

  @pro @fleet
  # source: pro:src/screens/operations.jsx:116,128-130
  Scenario: An accepted cancellation without an operation in the answer still reports success
    Given the hub accepts the cancellation of "op-1" with a body that carries no operation id
    When the answer arrives
    Then the toast reads "Cancellation requested for op-1 — targets already committed stay committed"
    And the card is not changed until the next periodic read

  @pro @fleet
  # source: pro:src/screens/operations.jsx:116-122,197
  Scenario: An accepted cancellation without a request time keeps the control until the next read
    Given the hub accepts the cancellation of "op-1" with a body that carries the operation id but no request time
    When the answer arrives
    Then the card still offers the cancel control and shows no "Cancel requested" label
    And both change only when the follow-up read or a later round reports the request

  @pro @fleet
  # source: pro:src/screens/operations.jsx:128-130
  Scenario Outline: A refused cancellation is reported in a toast and changes nothing
    Given the signed-in user is an admin and "op-1" is shown as running
    When the hub refuses the cancellation with <refusal>
    Then a toast reads "<toast>"
    And the card is unchanged and still offers the cancel control

    Examples:
      | refusal                                                    | toast                                               |
      | 409 with the message "the operation has already settled"   | Error: the operation has already settled            |
      | 403 with the message "admin role required"                 | Error: admin role required                          |
      | 404 with the message "no such operation is visible"        | Error: no such operation is visible                 |
      | 500 with the message "internal error"                      | Error: internal error                               |
      | no response at all                                         | Error: followed by the browser's error message      |

  @pro @fleet
  # source: pro:src/screens/operations.jsx:111-134
  Scenario: A 404 or 410 on cancel does not turn the card into a gone card
    Given "op-1" is shown as running
    When the hub answers the cancellation with 404 or 410
    Then only the error toast is shown
    And the card becomes a gone card only when a later periodic read answers 404 or 410

  @pro @fleet
  # source: pro:src/fleet.js:89
  # source: pro:src/app.jsx:111-119
  Scenario: An expired session during cancel sends the user to sign-in
    Given the hub requires sign-in
    When the cancellation request answers 401
    Then the sign-in screen is shown and the Operations page address is remembered for after sign-in

  @drop @pro @fleet
  # decision: D8
  # source: pro:src/fleet.js:65-98,235-236
  Scenario: The cancellation request has no deadline of its own
    Given the hub accepts the cancellation request but never answers
    When time passes
    Then the cancel controls stay disabled for as long as the browser keeps the request open

  @fixed @pro @fleet
  # decision: D8
  # source: pro:src/fleet.js:65-98,235-236
  Scenario: A cancellation request that takes longer than 15 seconds is given up
    Given the hub accepts the cancellation request but never answers
    When 15 seconds pass
    Then the request is cancelled, an error toast is shown and the cancel controls are enabled again

  @pro @fleet
  # decision: D18
  # source: pro:src/screens/operations.jsx:1-32,58-167
  Scenario: Cancel is the only action the page offers
    When the Operations page is shown to an admin
    Then it offers no way to start, retry, pause or delete an operation
