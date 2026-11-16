Feature: Toast messages
  Screens and shell actions report outcomes through one short-lived message
  at the edge of the page.

  @oss @pro
  # source: oss:src/app.jsx:372,519
  # source: pro:src/app.jsx:342,552
  Scenario: A message is shown briefly and then removed
    When a screen or a shell action reports a message
    Then the message is shown and announced as a status to assistive technology
    And it disappears after 2.8 seconds

  @oss @pro
  # source: oss:src/app.jsx:372
  # source: pro:src/app.jsx:342
  Scenario: A new message replaces the one showing
    Given a message is showing
    When another message is reported
    Then only the newer message is shown

  @oss @pro @drop
  # decision: D2
  # source: oss:src/app.jsx:372
  # source: pro:src/app.jsx:342
  Scenario: A replaced message's timer still clears the screen
    Given a message was reported and another one less than 2.8 seconds later
    When 2.8 seconds have passed since the first message
    Then the second message disappears at that moment, earlier than its own 2.8 seconds

  @oss @pro @fixed
  # decision: D2
  # source: oss:src/app.jsx:372
  # source: pro:src/app.jsx:342
  Scenario: A replacing message gets its full time
    Given a message was reported and another one less than 2.8 seconds later
    Then the second message stays for 2.8 seconds counted from when it was reported

  @oss @pro
  # source: oss:src/app.jsx:256,519
  # source: pro:src/app.jsx:155-157,552
  Scenario: A message outlives the screen that raised it
    Given a screen reported a message
    When the user navigates to another screen before it disappears
    Then the message is still shown for the rest of its time
