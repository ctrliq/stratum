Feature: Split pane sizing
  The open dashboard carries its own arithmetic for the list / detail split
  used by the Nodes screen and the resource lists. The fleet dashboard
  declares the same split to the design system's splitter.

  # assumption: whether the design system's splitter honours a declared split
  # is not settled by the code. oss:src/ui/controls.jsx:217-221 says it rendered
  # every split 50/50 under React, which is why oss re-implemented it. Opening
  # the fleet dashboard's Nodes screen with a node selected and measuring the
  # panes (declared 64% / 36%, pro:src/screens/infra.jsx:337) would confirm
  # which is true of the version in use. The first and fifth scenarios are the
  # acceptance criteria; the rest pin the helper's arithmetic.

  # source: oss:src/ui/split.js:13-33
  # source: oss:src/screens/infra.jsx:88
  @oss
  Scenario: A split declared in percentages gives each pane that share
    Given two panes declared as "64% 36%"
    Then the first pane takes 64% of the width and the second 36%

  # source: oss:src/ui/split.js:16-30
  @oss
  Scenario: Panes without a percentage share what the percentages leave
    Given three panes declared as "50% 2 *"
    Then the first pane takes 50%
    And the remaining 50% is shared two to one between the second and third panes
    And a pane with no declaration at all counts as one share

  # source: oss:src/ui/split.js:31-32
  @oss
  Scenario: Over-subscribed and unusable declarations still fill exactly the width
    Given two panes declared as "80% 60%"
    Then the panes are scaled to fill exactly 100% in the same proportion
    And panes whose declarations add up to nothing share the width equally

  # source: oss:src/ui/split.js:19-24
  @oss
  Scenario: Invalid sizes are ignored
    Given a pane declared with a negative or non-numeric percentage
    Then it is treated as 0%
    And a pane declared with a zero, negative or non-numeric weight is treated as one share

  # source: oss:src/ui/split.js:42-50
  # source: oss:src/ui/controls.jsx:235-246,272-277
  # source: oss:src/screens/infra.jsx:190
  @oss
  Scenario: A collapsed pane gives its space to the others and gets it back
    Given a list and a detail pane split 64% and 36%
    When the detail pane is collapsed
    Then the list takes the full width
    And when the detail pane is shown again the split returns to 64% and 36%, or to wherever the user last dragged it

  # source: oss:src/ui/split.js:35-40,52-60
  # source: oss:src/ui/controls.jsx:262-277,293-301
  # source: oss:src/screens/infra.jsx:90,190
  @oss
  Scenario: Dragging the divider respects both panes' limits
    Given a list pane with a minimum of 30% and a detail pane limited to between 26% and 66%
    When the user drags the divider
    Then neither pane goes beyond its limits
    And the two panes together keep the same combined width

  # source: oss:src/ui/split.js:36-39
  @oss
  Scenario: Panes without declared limits can be dragged between 10% and 90%
    Given a pane with no minimum or maximum size
    Then dragging keeps it between 10% and 90% of the group
