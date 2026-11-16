Feature: Capability availability
  The cluster drill-down shows, read-only, whether the cluster can currently
  run the three actions its evidence relates to: the certificate check and
  the snapshot save and restore. It prints the hub's capability record and
  never offers to trigger anything. (What the hub is able to do at all —
  provisioning, kubeconfig registration — is gated separately from the hub
  configuration; see fleet-page.feature and register-cluster.feature.)

  Background:
    Given a cluster drill-down is open

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:432-450,872
  Scenario: The drill-down carries a capability section above the summary
    When the drill-down is shown
    Then a section labelled "Capability availability" is shown directly under the panel header

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:440
  Scenario: The section says it is reading until the record answers
    When the capability record has not answered yet
    Then the section reads "reading capability availability…"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:801-816,872-876
  Scenario: The section does not depend on the engine-state summary
    Given the engine-state summary read is refused or still pending
    When the capability record answers
    Then the capability section shows the record all the same

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:401,436,447
  Scenario: Only the three evidence-related actions are listed, in the record's order
    Given the record answers the actions "snapshot.restore", "packages.install", "certificates.check" and "snapshot.save" in that order
    When the section is shown
    Then it lists "snapshot.restore", "certificates.check" and "snapshot.save" in that order
    And "packages.install" is not listed

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:408-425
  Scenario Outline: An entry prints the record's verdict for one action
    Given the record states the action "certificates.check" as "<state>" with handler version <version> and denial reason <reason>
    When the section is shown
    Then the entry reads "certificates.check" and "<verdict>"
    And the handler version is <versionShown>
    And the denial reason is <reasonShown>
    And a link to the Operations page is <link>

    Examples:
      | state       | version | reason                                        | verdict     | versionShown        | reasonShown                                           | link        |
      | available   | "1.4.2" | none                                          | available   | shown as "handler 1.4.2" | not shown                                        | offered     |
      | available   | none    | none                                          | available   | not shown           | not shown                                             | offered     |
      | available   | "1.4.2" | "disconnected: no connected session"          | available   | shown as "handler 1.4.2" | not shown                                        | offered     |
      | unavailable | none    | "disconnected: the cluster holds no connected session" | unavailable | not shown  | shown as "disconnected: the cluster holds no connected session" | not offered |
      | unavailable | "1.4.2" | "no-grant: no covering grant"                 | unavailable | shown as "handler 1.4.2" | shown as "no-grant: no covering grant"           | not offered |
      | unavailable | none    | none                                          | unavailable | not shown           | not shown                                             | not offered |
      | degraded    | none    | none                                          | unavailable | not shown           | not shown                                             | not offered |
      | absent      | none    | none                                          | unavailable | not shown           | not shown                                             | not offered |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:409,415
  Scenario: Only the exact state "available" reads as available
    Given the record states an action with any state other than "available", or with no state
    When the section is shown
    Then the entry reads "unavailable" as a warning

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:410-421
  Scenario: An unavailable action is never hidden
    Given the record states "snapshot.restore" as unavailable
    When the section is shown
    Then "snapshot.restore" is still listed, with its denial reason as "<code>: <message>" when the record gives one

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/ui/enginestate.jsx:422
  Scenario: The Operations link opens the Operations page without tracking anything
    Given an action is listed as available
    When the user follows its "Operations" link
    Then the Operations page opens with no operation tracked

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/ui/enginestate.jsx:422
  Scenario: Following the Operations link reloads the application
    Given an action is listed as available
    When the user follows its "Operations" link
    Then the whole application is loaded again
    And the drill-down and everything held in memory are lost

  @fixed @pro @fleet
  # decision: D2, D18
  # source: pro:src/ui/enginestate.jsx:422
  Scenario: The Operations link navigates inside the application
    Given an action is listed as available
    When the user follows its "Operations" link
    Then the Operations page opens without reloading the application, with no operation tracked

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:434-446
  Scenario: A record without any of the three actions says so
    Given the record answers successfully but with none of the three actions
    When the section is shown
    Then it reads "the record answers none of the actions these evidence panels relate to"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:433,441-443
  Scenario Outline: A refused record read quotes the hub and invents no availability
    When the capability record read is refused with the message "<message>" and is <retryable>
    Then the section reads "<note>"
    And no action is listed as available or unavailable

    Examples:
      | message                 | retryable     | note                                                          |
      | no cluster edge         | not retryable | capability record unavailable — no cluster edge               |
      | hub overloaded          | retryable     | capability record unavailable — hub overloaded · retryable    |

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/ui/enginestate.jsx:807-816,433-436
  Scenario: A refused re-read replaces the entries that were shown
    Given the section lists entries from an earlier successful read
    When a later read of the record is refused
    Then the entries disappear and only the refusal note is shown

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/ui/enginestate.jsx:807-816,433-436
  Scenario: A refused re-read keeps the entries and says so
    Given the section lists entries from an earlier successful read
    When a later read of the record is refused
    Then the entries stay listed
    And the refusal note is shown beside them

  @pro @fleet
  # source: pro:src/enginestate.js:310-312
  # source: pro:src/ui/enginestate.jsx:432-436
  Scenario: A record answer that was overtaken shows nothing
    Given the capability record read was overtaken by a newer read or a cluster switch
    When the section is shown with that outcome
    Then it shows neither the reading text, nor entries, nor a refusal, nor the empty-record line

  @pro @fleet
  # assumption: holds once the page is redrawn with the new list; a runtime check with the selected cluster unreachable would confirm whether the re-read then lags one refresh behind
  # source: pro:src/ui/enginestate.jsx:807-816
  # source: pro:src/screens/clusters.jsx:361-363
  Scenario: The record is read once per opened cluster and again on every fleet refresh
    When the drill-down opens, and each time a new cluster list arrives while it is open
    Then the capability record of the drilled cluster is read

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:733,807-816
  Scenario: Drilling into another cluster clears the section first
    Given the section lists entries for cluster "prod"
    When the user opens the drill-down of "staging"
    Then the section reads "reading capability availability…" until the record of "staging" answers

  @pro @fleet
  # decision: D18
  # source: pro:src/ui/enginestate.jsx:408-425,432-450
  Scenario Outline: No role is offered a trigger control
    Given the signed-in user is <role>
    When the capability section is shown
    Then it offers no control that starts an action

    Examples:
      | role             |
      | an admin         |
      | a read-only user |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:782-799,801-816
  Scenario: The record never gates the evidence
    Given the record states every action as unavailable, or its read was refused
    When the user opens a finding's evidence
    Then the evidence is read and shown as usual
