Feature: Version comparison view
  The fleet dashboard lists, per node and component, the version that was
  expected, the one installed and the one serving, with the hub's verdict.
  The browser shows what the hub reported and compares nothing itself.

  # assumption: every scenario is tagged @pro only; the hub serves this evidence
  # and nothing in the browser code limits it to fleet mode.

  # ---- which rows exist -----------------------------------------------------------------

  @pro
  # source: pro:src/version-view.js:185-187
  Scenario: Without a hub summary nothing is derived
    Given the hub's version evidence for the cluster carries no summary
    Then the view has no rows and reports the evidence as unavailable

  @pro
  # source: pro:src/version-view.js:36-40,199-209
  Scenario: One row per node, component and signal the hub reported a finding for
    Given the hub reported findings for several subjects
    Then the view has one row for each distinct combination of scope, node, component, key and signal
    And the rows follow the order in which the hub first reported each combination

  @pro
  # source: pro:src/version-view.js:117-123,166,199-205
  Scenario: Several findings about the same subject share one row
    Given the hub reported more than one finding for the same subject and signal
    Then the view has a single row for it
    And the row lists every finding as a cause with its code, severity, message and sample time
    And each cause says whether it is the hub's policy finding or the comparison's own outcome

  @pro
  # source: pro:src/version-view.js:36-37,200-202
  Scenario: A finding that names no subject is left out
    Given the hub reported a finding without a subject
    Then no row is shown for it

  @pro
  # source: pro:src/version-view.js:132-138,208
  Scenario: A subject with findings but no held observation still has a row
    Given the hub reported a finding for a subject and delivered no observation for it
    Then the view still has a row for that subject, named from the finding
    And its evidence is whatever the hub served behind the finding's evidence reference, if anything

  @pro
  # source: pro:src/version-view.js:194-198
  Scenario: A repeated observation is used once
    Given the hub delivered the same subject's observation on several pages
    Then the row uses the first one delivered

  # ---- what a row shows -------------------------------------------------------------------

  @pro
  # source: pro:src/version-view.js:147-170
  Scenario: A row carries the hub's evidence verbatim
    Given a subject with an observation carrying version evidence
    Then its row shows the expected, installed and serving values, the comparison's verdict, rule and reason
    And the member release, the observation's sample time and its source
    And none of these values is recomputed in the browser

  @pro
  # source: pro:src/version-view.js:154-155,169
  Scenario: A row carries the observation's and the baseline's reasons and its evidence reference
    Given a subject with an observation carrying version evidence
    Then its row also shows the observation's own reason and the baseline's reason
    And the hub's reference to the evidence behind the row

  @pro
  # source: pro:src/version-view.js:133-134,165
  Scenario: A row's severity is the hub's grade of the comparison
    Given a subject with an outcome finding and a policy finding
    Then the row's severity is the outcome finding's severity

  @pro
  # source: pro:src/version-view.js:133-134,165
  Scenario: A row with only policy findings takes the first one's severity
    Given a subject whose findings are all hub policy findings
    Then the row's severity is the first finding's severity
    And a finding without a severity makes the row's severity "unknown"

  @pro
  # source: pro:src/version-view.js:103-114,160
  Scenario Outline: The serving column is labelled from the evidence
    Given evidence where serving is <applicable> and <value>
    Then the serving column is labelled <label>

    Examples:
      | applicable              | value                           | label                       |
      | marked not applicable   | no serving value is present     | "Installed"                 |
      | applicable or unstated  | a readable serving value exists | "Running"                   |
      | applicable or unstated  | the serving value is unreadable | unknown, with no label      |
      | marked not applicable   | a serving value is present      | unknown, with no label      |

  # ---- rollouts in progress ---------------------------------------------------------------

  @pro
  # source: pro:src/version-view.js:27-30,52-53,145-146,163-164
  Scenario Outline: A mismatch explained by a rollout is labelled with the rollout
    Given the hub graded a subject's mismatch as explained by an operation in progress
    And the hub served no policy finding for that subject
    And the operation states the action <action>
    Then the row is labelled with the operation's phase and reads "<label>"

    Examples:
      | action                    | label                      |
      | "release.upgrade"         | Upgrading                  |
      | "release.rollback"        | Rolling back               |
      | anything else, or nothing | Rollout, direction unknown |

  @pro
  # source: pro:src/version-view.js:125-131,145-146
  Scenario: A reporting loss hides the rollout label
    Given the hub graded a subject's mismatch as explained by an operation in progress
    And the hub also served a policy finding for that subject, such as a reporting loss
    Then the row carries no rollout label and shows the loss among its causes

  @pro
  # source: pro:src/version-view.js:140-146
  Scenario: No rollout label without a phase
    Given the hub graded a subject's mismatch as explained
    And the evidence carries no operation, or an operation without a phase
    Then the row carries no rollout label

  @pro
  # source: pro:src/version-view.js:47-53
  Scenario: A rollout's direction is never guessed from the versions
    Given an explained mismatch whose operation does not state a known action
    Then the row does not say whether it is an upgrade or a rollback

  # ---- malformed evidence ------------------------------------------------------------------

  @pro
  # source: pro:src/version-view.js:55-76,93-101
  Scenario: A malformed value is shown as unknown, never as raw structure
    Given evidence in which a value that should be text is something else
    Then that value is shown as unknown
    And the well-formed values beside it are still shown

  @pro
  # source: pro:src/version-view.js:137-139
  Scenario: Evidence that is not an object leaves every column unknown
    Given a subject whose evidence is missing or is not an object
    Then its row still exists with every evidence column unknown

  @pro
  # source: pro:src/version-view.js:77-86
  Scenario: Replica instances are checked one by one
    Given a value that lists one identity per replica
    Then each well-formed replica identity is shown
    And an entry that is not an object is dropped

  @pro
  # source: pro:src/version-view.js:57-59,74,84,90,97
  Scenario: Well-formed evidence keeps fields the dashboard does not know
    Given well-formed evidence carrying additional fields from a newer hub
    Then those fields are passed through untouched

  # ---- the report as a whole ---------------------------------------------------------------

  @pro
  # source: pro:src/version-view.js:210-217
  Scenario: The report's header comes straight from the hub's summary
    Then the view shows the summary's cluster, rollup severity, reporting freshness, observation time and coverage unchanged
    And none of them is derived from the rows

  @pro
  # source: pro:src/version-view.js:212-213
  Scenario: The report generation of the load wins over the summary's
    Given the load states a report generation of its own
    Then the view shows that generation instead of the summary's

  @pro
  # source: pro:src/version-view.js:218-219
  # source: pro:src/screens/infra.jsx:410-419
  Scenario: Incomplete reports are flagged
    When the hub's findings walk stopped at its bound
    Then the view is flagged as truncated and says nodes beyond the bound are not listed
    When the hub reported the snapshot behind the report as partial
    Then the view is flagged as partial and says findings from some sources may be missing
