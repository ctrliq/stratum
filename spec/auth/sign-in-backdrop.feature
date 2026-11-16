Feature: Sign-in backdrop
  Beside the fleet sign-in form sits a decorative animated network. It shows
  nothing about the fleet.

  @pro @drop
  # decision: D20
  # source: pro:src/ui/login-network.jsx:3,14-31
  Scenario: The backdrop is decorative and fetches nothing
    Given a visitor without a session
    When the sign-in screen is shown
    Then the backdrop is drawn from a fixed generated pattern
    And no fleet or cluster data is requested to draw it
    And it is hidden from assistive technology

  @pro @drop
  # decision: D20
  # source: pro:src/ui/login-network.jsx:12,38,77,92
  Scenario: The backdrop does not animate for users who prefer reduced motion
    Given the visitor's system asks for reduced motion
    When the sign-in screen is shown
    Then the backdrop is drawn once as a still image
    And it starts or stops animating when that preference changes

  @pro @drop
  # decision: D20
  # source: pro:src/ui/login-network.jsx:13,37,82,93
  # source: pro:src/screens/login.jsx:104-110
  Scenario: The backdrop is not drawn on narrow viewports
    Given a viewport narrower than 768 pixels
    When the sign-in screen is shown
    Then the backdrop is not drawn
    And it is drawn again when the viewport becomes wider

  @pro @drop
  # decision: D20
  # source: pro:src/ui/login-network.jsx:37,82,94
  Scenario: The backdrop pauses while the tab is hidden
    Given the sign-in screen is shown
    When the browser tab is hidden
    Then the animation stops
    And it resumes when the tab is visible again

  @pro @drop
  # decision: D20
  # source: pro:src/ui/login-network.jsx:79-91
  Scenario: The backdrop follows the size of its area
    When the area beside the form changes size
    Then the backdrop is redrawn to fill it
