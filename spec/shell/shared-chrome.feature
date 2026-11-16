Feature: Shared chrome behavior
  A few pieces every screen borrows from the shell behave in ways users rely
  on and a design system would not supply by default.

  # ---- header menus -----------------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:38-59
  # source: pro:src/ui/shell.jsx:26-47
  Scenario: A header menu toggles from its button
    When the user clicks a header menu's button
    Then the menu opens
    When the user clicks the button again
    Then the menu closes

  @oss @pro
  # source: oss:src/ui/shell.jsx:61-69
  # source: pro:src/ui/shell.jsx:49-57
  Scenario: A disabled menu entry does nothing
    When the user clicks a disabled entry of a header menu
    Then nothing is run

  # ---- detail pane ------------------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:134-145,156-160
  # source: pro:src/ui/shell.jsx:128-139,150-154
  Scenario: Escape or the close button closes a detail pane
    Given a detail pane is open beside a list
    When the user presses Escape or clicks the pane's close button
    Then the pane closes

  @oss @pro
  # source: oss:src/ui/shell.jsx:118-128,142
  # source: pro:src/ui/shell.jsx:110-122,136
  Scenario: Escape does not close a detail pane while typing in it
    Given a detail pane is open and the user is typing in one of its fields or its editor
    When the user presses Escape
    Then the pane stays open and the unsaved input is kept

  @oss @pro
  # source: oss:src/ui/shell.jsx:118-128
  # source: pro:src/ui/shell.jsx:110-122
  Scenario: Escape still closes the pane while typing elsewhere on the page
    Given a detail pane is open and the user is typing in a field outside it, such as the list's search box
    When the user presses Escape
    Then the pane closes

  # ---- status wording ---------------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:74-88,94-96
  # source: pro:src/ui/shell.jsx:62-76,78-80
  Scenario Outline: Some statuses are shown under a friendlier name
    When a status badge is shown for "<status>"
    Then it reads "<label>"

    Examples:
      | status             | label        |
      | SchedulingDisabled | Cordoned     |
      | CrashLoopBackOff   | CrashLoop    |
      | healthy            | Healthy      |
      | degraded           | Degraded     |
      | disconnected       | Disconnected |
      | pending            | Pending      |
      | provisioning       | Provisioning |
      | deployed           | Deployed     |

  @oss @pro
  # source: oss:src/ui/shell.jsx:87-88
  # source: pro:src/ui/shell.jsx:75-76
  Scenario: An unrecognised status is shown as it is, in a neutral colour
    When a status badge is shown for a status the dashboard has no entry for
    Then it reads that status unchanged and is coloured neutral

  @oss @pro
  # source: oss:src/ui/shell.jsx:74-86
  # source: pro:src/ui/shell.jsx:62-74
  Scenario Outline: Statuses are coloured by what they mean
    When a status badge is shown for <statuses>
    Then it is coloured as <meaning>

    Examples:
      | statuses                                                                                | meaning     |
      | Running, Ready, Healthy, Complete, Completed, Deployed, Active, Bound or Done           | success     |
      | Progressing, Validating, Upgrading, lowercase pending or provisioning                   | in progress |
      | Scheduled or capitalised Pending                                                        | neutral     |
      | Degraded, Paused, SchedulingDisabled or Terminating                                     | warning     |
      | Failed, NotReady, CrashLoopBackOff or disconnected                                      | error       |

  @diverged @oss @drop
  # decision: D1
  # source: oss:src/ui/shell.jsx:74-88
  Scenario: The open dashboard has no entry for Unschedulable
    When a status badge is shown for "Unschedulable"
    Then it is coloured neutral

  @diverged @pro
  # source: pro:src/ui/shell.jsx:70
  Scenario: The fleet dashboard colours Unschedulable as a warning
    When a status badge is shown for "Unschedulable"
    Then it is coloured as a warning

  @oss
  # source: oss:src/ui/shell.jsx:92
  Scenario: A node's dot shows readiness before cordon
    Then a node that is not ready has an error-coloured dot even when it is cordoned
    And a ready, cordoned node has a warning-coloured dot
    And a ready, schedulable node has a success-coloured dot

  # ---- usage meter ------------------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/shell.jsx:176-187
  # source: pro:src/ui/shell.jsx:170-181
  Scenario Outline: A usage meter is coloured by how full it is
    When a usage meter shows <percent>
    Then <appearance>

    Examples:
      | percent           | appearance                                          |
      | no value or 0     | a dash is shown instead of a bar                    |
      | 1 to 70 percent   | the bar is success-coloured with the percent beside it |
      | 71 to 85 percent  | the bar is warning-coloured with the percent beside it |
      | above 85 percent  | the bar is error-coloured with the percent beside it   |
