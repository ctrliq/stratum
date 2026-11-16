Feature: Engine-state evidence drawer
  In the cluster drill-down, "Evidence" on a finding opens the evidence the
  hub holds behind it. The drawer prints the evidence's own facts and
  nothing derived; missing evidence is never presented as an observed
  failure.

  Background:
    Given a cluster drill-down is open and lists findings

  # ---- opening ---------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:789-799,920-946,950-957
  # source: pro:src/enginestate.js:281-298
  Scenario: Evidence opens a drawer for the finding's reference
    When the user clicks "Evidence" on a finding with the reference "ev-1"
    Then a drawer headed "Evidence — ev-1" appears under the findings
    And the finding's row is marked as open
    And the evidence "ev-1" of the report the findings were walked for is read from the hub

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:654
  Scenario: The drawer says it is reading until the hub answers
    When the user opens a finding's evidence and the hub has not answered
    Then the drawer reads "reading evidence…"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:920
  Scenario: Every finding that shares the open reference is marked open
    Given two findings carry the same evidence reference
    When the evidence of one of them is open
    Then both rows are marked as open

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:928-939
  Scenario: Opening another finding's evidence starts the drawer clean
    Given the drawer shows the evidence "ev-1"
    When the user clicks "Evidence" on a finding with the reference "ev-2"
    Then the drawer is headed "Evidence — ev-2" and reads "reading evidence…"
    And nothing of "ev-1" is shown

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:696-699,928-944
  Scenario: Clicking Evidence again on the open reference re-reads it without clearing
    Given the drawer shows the evidence "ev-1"
    And the listed findings are still of the report the evidence was read for
    When the user clicks "Evidence" on the same finding again
    Then the evidence is read again
    And the content already shown stays until the new answer arrives

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:928-939
  Scenario: Re-opening the open reference after the report moved starts the drawer clean
    Given the drawer shows the evidence "ev-1" read for report "g-7"
    And the listed findings are now of report "g-8"
    When the user clicks "Evidence" on a finding with the reference "ev-1"
    Then the drawer is cleared and reads "reading evidence…"
    And "ev-1" is read for report "g-8"

  @pro @fleet
  # source: pro:src/enginestate.js:282,289
  Scenario: A slower answer for a reference the user moved away from is dropped
    Given the read of "ev-1" has not answered
    When the user opens "ev-2"
    Then the answer for "ev-1", when it arrives, is not shown

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:649-661,789-797
  # source: pro:src/enginestate.js:289
  Scenario: An evidence answer that was overtaken shows nothing
    Given the evidence read of the open reference was overtaken, for example by a cluster switch
    When the drawer is shown with that outcome
    Then it shows its heading only: no "reading evidence…", no refusal and no body

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:950
  Scenario: The drawer has no close control of its own
    Given the drawer is open
    When the user looks for a way to dismiss it
    Then it stays until another finding's evidence is opened, another cluster is drilled into, or the drill-down is closed

  # ---- refusals and partial fragments -----------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:649,655-657
  Scenario Outline: A refused evidence read quotes the hub
    When the evidence read is refused with the message "<message>" and is <retryable>
    Then the drawer reads "<note>"

    Examples:
      | message                 | retryable     | note                                                  |
      | detail expired          | not retryable | evidence unavailable — detail expired                 |
      | source not connected    | retryable     | evidence unavailable — source not connected · retryable |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:796,840,655-661
  Scenario: A refused retry keeps the last good evidence beside the refusal
    Given the drawer shows evidence read successfully for "ev-1"
    When the user retries and the hub refuses
    Then the earlier evidence stays shown
    And the refusal note is shown above it

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:658-660
  Scenario: A partial fragment is labelled with what was served
    Given the hub marks the evidence as incomplete, serving 2 of 5 items
    When the evidence is shown
    Then a note reads "partial — 2 of 5 items served"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:828-832,952-954
  Scenario Outline: Evidence from an older report is labelled
    Given the drawer shows evidence read for report "g-7"
    When <event>
    Then a note reads "this evidence is from the previous report — g-7."

    Examples:
      | event                                                                          |
      | the summary shown now describes report "g-8"                                   |
      | a findings read was refused because the report moved to "g-8"                  |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:705-707,932-936
  Scenario: A findings refresh does not rewrite an open drawer
    Given the drawer shows evidence
    When the findings are re-read
    Then the drawer keeps the evidence and the observation details it was opened with

  # ---- the body -----------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:538-545
  Scenario: The source of the evidence is printed as the hub recorded it
    Given the evidence names a source with a process instance, a build, a producer and a start time
    When the evidence is shown
    Then "Source" lists "Process", "Build", "Producer" and "Started" with those values
    And "Started" reads the local time followed by its age, such as "(3h ago)"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:541-544
  Scenario: Missing source facts are dashes
    Given the evidence names a source without a build and without a start time
    When the evidence is shown
    Then "Build" and "Started" read "—"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:546-548
  Scenario: Evidence without a source says the hub derived it
    Given the evidence names no source
    When the evidence is shown
    Then it reads "hub-derived evidence — no producer source observed"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:554-569
  Scenario: An item of a type without a dedicated view is shown raw
    Given the evidence carries an item of type "version"
    When the evidence is shown
    Then the item is shown as "evidence item · version" followed by its content as formatted JSON

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:565
  Scenario: An item without a type is shown raw with a dash
    Given the evidence carries an item without a type
    When the evidence is shown
    Then the item is shown as "evidence item · —" followed by its content as formatted JSON

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:650-661,554
  Scenario: Evidence with no items shows only its source
    Given the evidence carries no items
    When the evidence is shown
    Then only the source block, or the hub-derived line, is shown

  # ---- supervisor evidence ----------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:555-557,587-597
  Scenario: A supervisor item shows its process and build
    Given the evidence carries a supervisor item with a process id and a build
    When the evidence is shown
    Then "Supervisor" lists "Process" and "Build" with those values, "—" for any that is missing

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:589-600
  Scenario Outline: Uptime is shown only when the evidence states an actual start
    Given a supervisor item whose process start time is <start>
    When the evidence is shown
    Then <shown>

    Examples:
      | start                         | shown                                                        |
      | 2 hours before now            | a line reads "uptime 2h · started <local time>"              |
      | absent                        | no uptime line is shown                                      |
      | not a parseable time          | no uptime line is shown                                      |
      | later than now                | no uptime line is shown                                      |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:601-610
  Scenario Outline: Each supervised child states its desired and running state as recorded
    Given a supervisor item with a child "etcd" whose desired flag is <desired> and running flag is <running>
    When the evidence is shown
    Then the child line reads "<line>"

    Examples:
      | desired | running | line                              |
      | true    | true    | etcd · desired · running          |
      | true    | false   | etcd · desired · not running      |
      | false   | true    | etcd · not desired · running      |
      | false   | false   | etcd · not desired · not running  |
      | absent  | true    | etcd · — · running                |
      | true    | absent  | etcd · desired · —                |
      | absent  | absent  | etcd · — · —                      |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:603,608
  Scenario: A child's ownership and a missing name are shown as recorded
    Given a supervisor child with the ownership "foreign" and no component name
    When the evidence is shown
    Then the child line reads "— · <desired> · <running> · foreign"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:611-616
  Scenario: The runtime observation is labelled as such
    Given a supervisor item with a runtime observation of availability "available" sampled 40 seconds ago
    When the evidence is shown
    Then a line reads "runtime observation: available · sampled 40s ago"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:614
  Scenario: A runtime observation's reason is quoted
    Given a runtime observation with the reason code "timeout", the message "runtime did not answer" and the retryable flag set
    When the evidence is shown
    Then the line ends with " · timeout: runtime did not answer · retryable"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:617-621
  Scenario: The mirror-Pod observation is labelled as a Kubernetes observation
    Given a supervisor item with a mirror-Pod observation of availability "unavailable" sampled 2 minutes ago
    When the evidence is shown
    Then a line reads "Kubernetes observation — mirror Pod: unavailable · sampled 2m ago"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:613,619
  Scenario: Observations with missing facts use dashes
    Given a runtime or mirror-Pod observation without an availability or without a sample time
    When the evidence is shown
    Then the missing availability reads "—" and the missing age reads "sampled —"

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:611,617
  Scenario: Absent observations are simply not shown
    Given a supervisor item with neither a runtime nor a mirror-Pod observation
    When the evidence is shown
    Then no observation line is shown and nothing is said about the process being down

  # ---- typed panels -------------------------------------------------------------------------

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:515-531,549,561
  Scenario: A certificate finding's evidence is shown in the certificate panel
    Given the opened finding's signal is "certificate" and the evidence carries certificate items
    When the evidence is shown
    Then the certificate evidence panel is shown, fed with the certificate observations walked with the findings
    And the served fragment fills in the observation of the opened finding
    And the certificate items are not repeated as raw items

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:528-532
  Scenario: Extra certificate fragments are shown without invented metadata
    Given the evidence carries more certificate items than the one that fills the opened observation
    When the evidence is shown
    Then each extra item is shown in the certificate panel with its own facts only

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:516,533-535,550-553,562
  Scenario: A backup finding's evidence is shown in the backup panel
    Given the opened finding's signal is "backup" and the evidence carries backup items
    When the evidence is shown
    Then one backup evidence panel is shown per backup item
    And each is given the opened finding's severity and the matching observation's own outcome, "unknown" when either is missing
    And the backup items are not repeated as raw items

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:940-943
  Scenario: Another finding sharing the open evidence switches the severity shown
    Given two findings of different severity share the evidence reference that is open
    When the user clicks "Evidence" on the other one
    Then the evidence already shown stays
    And the typed panel uses the severity of the finding just clicked

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:570-575
  Scenario Outline: Typed evidence can always be inspected in full
    Given the opened finding's signal is "<signal>"
    When the evidence is shown
    Then a collapsed "Full evidence" section <offered>

    Examples:
      | signal      | offered                                           |
      | certificate | is offered and expands to the whole answer as JSON |
      | backup      | is offered and expands to the whole answer as JSON |
      | version     | is not offered                                    |

  @pro @fleet
  # source: pro:src/ui/enginestate.jsx:518,534,561-562
  Scenario: Items of another type inside typed evidence stay visible
    Given the opened finding's signal is "certificate" and the evidence also carries an item of another type
    When the evidence is shown
    Then that item is shown raw beneath the certificate panel
