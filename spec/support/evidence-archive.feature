Feature: Evidence archive download
  From the Clusters page an admin can collect a cluster's existing evidence
  into a support archive and save it, in one click: the hub starts a
  collection, the dashboard waits for it to finish and then downloads the
  archive. The control is labelled "Download evidence archive"; the hub calls
  the resource a support archive.

  # source: pro:src/screens/clusters.jsx:235,274,379-386,398,426,472-473,535-545
  @pro @fleet
  Scenario: Each cluster of a fleet offers the archive control to admins
    Given an admin views the Clusters page of a fleet hub, as cards or as a table
    Then every cluster shows a "Download evidence archive" icon button beside its other cluster actions
    And it is offered whether or not the hub can provision clusters

  # source: pro:src/screens/clusters.jsx:228-235,271-274,472-473,535
  @pro
  Scenario: The single cluster of a standalone hub offers the archive control to admins
    Given an admin views the Clusters page of a hub in standalone mode
    Then the "Download evidence archive" icon button is the only control in the cluster's actions slot, beside its cluster-selection and "Engine state" controls

  # source: pro:src/screens/clusters.jsx:379-385,473,535
  # source: pro:src/fleet.js:48-59
  # decision: D14 — on a hub, write controls are offered only to the admin role; unchanged
  @pro
  Scenario: A read-only user is not offered the archive control
    Given the signed-in user does not have the admin role
    When the user views the Clusters page
    Then no evidence archive control is shown for any cluster

  # source: pro:src/screens/clusters.jsx:373-374,421,467-470,490,536-542
  # assumption: the disabled state and the tooltip text are read from the code; whether the tooltip is actually displayed on a disabled control is not (the tooltip sits on the disabled button itself, which the same file says swallows hover) — hovering a disconnected cluster's control in the running legacy dashboard would confirm it. The rewrite shows the tooltip in every row.
  @pro
  Scenario Outline: The control is enabled only for a cluster whose evidence can be read
    Given a cluster whose phase is <phase>
    Then its archive control is <state> with the tooltip "<tooltip>"

    Examples:
      | phase         | state    | tooltip                                                                    |
      | healthy       | enabled  | Download evidence archive                                                  |
      | degraded      | enabled  | Download evidence archive                                                  |
      | disconnected  | disabled | Cannot collect an evidence archive — the cluster is disconnected           |
      | pending       | disabled | Cannot collect an evidence archive — the cluster is pending                |
      | provisioning  | disabled | Cannot collect an evidence archive — the cluster is provisioning           |
      | deleting      | disabled | Cannot collect an evidence archive — the cluster is deleting               |
      | not reported  | disabled | Cannot collect an evidence archive — the cluster is in an unknown phase    |

  # source: pro:src/screens/clusters.jsx:431-441,489-503,538
  # source: pro:src/fleet.js:255,267-268
  @pro
  Scenario: One click starts a collection without confirmation
    When an admin presses a healthy cluster's archive control
    Then the hub is asked to start a support archive collection for that cluster, with no confirmation dialog
    And the control is disabled until the whole flow has ended
    And a second press while the flow runs starts nothing

  # source: pro:src/fleet.js:101,267-268
  @pro
  Scenario: The start request carries an empty JSON object
    When the collection is started
    Then the request is a POST whose body is an empty JSON object, sent as JSON, although the hub's route needs no body

  # source: pro:src/screens/clusters.jsx:503-505
  # source: pro:src/fleet.js:65-98,255
  # decision: D8 — the 90-second start deadline is kept as a proposed exception to the 15-second rule: the hub runs the whole collection inside this request (see NOTES, Fix pass)
  @pro
  Scenario Outline: A collection that cannot be started is reported
    When starting the collection <failure>
    Then a notification reads "Error: <detail>"
    And the control is enabled again

    Examples:
      | failure                                              | detail                                        |
      | is refused by the hub with a typed error             | the hub's error message                       |
      | is refused with a body that is not a typed error     | the response text, or "HTTP <status>" if empty |
      | fails at the network level                           | the network error message                     |
      | has not been answered after 90 seconds               | the abort's error message                     |

  # source: pro:src/screens/clusters.jsx:506-507
  @pro
  Scenario: A start that names no collection is reported
    When the hub accepts the start but its answer carries no collection identifier
    Then a notification reads "The hub started the collection but answered no collection id"
    And the control is enabled again

  # source: pro:src/screens/clusters.jsx:463-464,509-527
  # source: pro:src/fleet.js:276-277
  # decision: D8 — legacy status reads are bounded at 8 seconds
  @pro @drop
  Scenario: The collection is polled until it has an archive or an outcome
    Given the hub accepted the start and named a collection
    Then the collection's status is read immediately
    And while it reports no archive size and an outcome that is absent or "pending", it is read again every 2 seconds
    And each status read is abandoned after 8 seconds

  # source: pro:src/screens/clusters.jsx:463-464,509-527
  # source: pro:src/fleet.js:276-277
  # decision: D8
  @pro @fixed
  Scenario: The collection is polled with the common request deadline
    Given the hub accepted the start and named a collection
    Then the collection's status is read immediately
    And while it reports no archive size and an outcome that is absent or "pending", it is read again every 2 seconds
    And each status read is abandoned after 15 seconds

  # source: pro:src/screens/clusters.jsx:520-526
  # decision: D2 — the flow can hang forever (NOTES (a) 1)
  @pro @drop
  Scenario: A collection that never leaves pending keeps the control disabled indefinitely
    When every status read succeeds and keeps reporting no archive size and an outcome that is absent or "pending"
    Then the status is read every 2 seconds without end, each success resetting the failure count
    And the control stays disabled until the user leaves or the flow is abandoned

  # source: pro:src/screens/clusters.jsx:520-526
  # decision: D2
  # assumption: the legacy code has no overall bound to copy; 2 minutes is proposed (twice the hub's stated 60-second assembly budget) and needs confirming against the hub
  @pro @fixed
  Scenario: A collection that never leaves pending is given up
    When the collection still reports no archive and no terminal outcome 2 minutes after it was started
    Then polling stops and a notification says the evidence archive for the cluster did not finish in time
    And the control is enabled again

  # source: pro:src/screens/clusters.jsx:513-518
  # decision: D2 — a definitive refusal is hidden behind about 29 seconds of retries (NOTES (a) 6)
  @pro @drop
  Scenario: A refused status read is retried like a dropped one
    When a status read is refused by the hub, for example as forbidden or not found
    Then it is retried with the same delays and the same five-failure budget as a network failure before its message is reported

  # source: pro:src/screens/clusters.jsx:513-518
  # decision: D2
  @pro @fixed
  Scenario: A refused status read ends the flow at once
    When a status read is refused by the hub with a client error such as forbidden or not found
    Then a notification reads "Error: " followed by the hub's message, without retrying, and the control is enabled again
    But network failures, timeouts and server errors are still retried with growing delays

  # source: pro:src/screens/clusters.jsx:463-466,513-518
  @pro
  Scenario: Failed status reads are retried with growing delays and then given up
    When status reads fail consecutively
    Then the dashboard waits 2, 4, 8 and then 15 seconds between attempts
    And after the fifth consecutive failure a notification reads "Error: " followed by the last failure's message, and the control is enabled again
    But a successful read in between starts the count again

  # source: pro:src/screens/clusters.jsx:519-524
  @pro
  Scenario: A collection that ends without an archive is reported as failed
    When the collection reports no archive size and an outcome other than "pending", such as "failed"
    Then a notification reads "Evidence archive for <cluster name> ended in failure — the hub's outcome is failed"
    And no download is attempted
    And the control is enabled again

  # source: pro:src/screens/clusters.jsx:520,528-530,548-556
  # source: pro:src/fleet.js:319-374
  @pro
  Scenario: A finished collection is downloaded once and saved
    When the collection reports an archive size
    Then the archive is downloaded exactly once and handed to the browser as a file save
    And a notification reads "Evidence archive for <cluster name> saved — outcome <the hub's outcome word>"
    And polling stops and the control is enabled again

  # source: pro:src/screens/clusters.jsx:548-556
  @pro
  Scenario: A partial archive names every section that is not complete
    Given the finished collection's manifest lists the sections "logs" as "truncated", "events" as "stale" and "nodes" as "complete"
    When the archive has been saved
    Then the notification reads "Evidence archive for <cluster name> saved — outcome <outcome>; incomplete sections: logs truncated, events stale"

  # source: pro:src/fleet.js:292-301,360
  @pro
  Scenario Outline: The saved file takes the name the hub gives it
    Given the download answers with <header>
    Then the file is saved as "<filename>"

    Examples:
      | header                                                             | filename                          |
      | Content-Disposition: attachment; filename="lab-support.tar.gz"     | lab-support.tar.gz                |
      | Content-Disposition: attachment; filename=lab-support.tar.gz       | lab-support.tar.gz                |
      | Content-Disposition: attachment; filename=lab.tar.gz; size=10      | lab.tar.gz                        |
      | no Content-Disposition header                                      | <collection id>-support.tar.gz    |

  # source: pro:src/fleet.js:294-301
  @pro
  Scenario: An extended-form filename is ignored and the fallback name is used
    Given the download's Content-Disposition header carries only the extended form, such as filename*=UTF-8''lab-support.tar.gz
    Then the file is saved as "<collection id>-support.tar.gz"

  # source: pro:src/screens/clusters.jsx:528-530
  # source: pro:src/fleet.js:290,319-374
  # decision: D8 — the 60-second download deadline is kept as a proposed exception to the 15-second rule: the body is tens of MiB (see NOTES, Fix pass)
  @pro
  Scenario Outline: A download that fails is reported and nothing is saved
    When the download <failure>
    Then no file is saved
    And a notification reads "Error: <detail>"
    And the control is enabled again

    Examples:
      | failure                                              | detail                                         |
      | is refused by the hub with a typed error             | the hub's error message                        |
      | is refused with a body that is not a typed error     | the response text, or "HTTP <status>" if empty |
      | fails at the network level                           | the network error message                      |
      | has not completed after 60 seconds                   | the abort's error message                      |
      | arrives but the browser cannot hand it over as a file | the browser's error message                   |

  # source: pro:src/fleet.js:61-63,89,340
  @pro
  Scenario: An expired session during the flow sends the user to sign-in
    When the hub answers "unauthorized" to the start, a status read or the download
    Then the user is taken to the sign-in screen, as for any other hub request

  # source: pro:src/fleet.js:89,340
  # source: pro:src/screens/clusters.jsx:505,513-518,530
  # decision: D2 — the user is sent to sign-in and also shown an error, and status reads keep retrying behind the sign-in screen (NOTES (a) 5)
  @pro @drop
  Scenario: An unauthorized answer also follows the normal failure path
    When the hub answers "unauthorized" during the flow
    Then besides the hop to sign-in, an unauthorized start or download also raises an "Error: …" notification
    And an unauthorized status read is retried up to the five-failure budget

  # source: pro:src/fleet.js:89,340
  # source: pro:src/screens/clusters.jsx:505,513-518,530
  # decision: D2
  @pro @fixed
  Scenario: An unauthorized answer ends the flow silently
    When the hub answers "unauthorized" to the start, a status read or the download
    Then the user is taken to the sign-in screen
    And the flow ends without an error notification and without further requests

  # source: pro:src/fleet.js:68,267-268,276-277,330
  @pro
  Scenario: Only the start carries the anti-forgery token
    When the flow runs
    Then the start request carries the session's anti-forgery token
    And the status reads and the download, being reads, carry only the session cookie

  # source: pro:src/screens/clusters.jsx:476-487,495-497,504,512,529
  @pro
  Scenario Outline: An abandoned flow ends silently
    Given an archive flow is running for a cluster
    When <event>
    Then every request of the flow still in flight is aborted and no further status read or download happens
    And no notification is shown for that flow

    Examples:
      | event                                                                  |
      | the user leaves the Clusters page                                      |
      | the cluster disappears from the list                                   |
      | the cluster's identity or registration source changes                  |
      | the signed-in identity stops being, or becomes, an admin               |

  # source: pro:src/screens/clusters.jsx:491,522,530,554
  @pro
  Scenario: Notifications name the cluster as it was when the flow started
    Given a cluster is renamed while its archive flow is running
    Then the flow's notification uses the name the cluster had when the control was pressed

  # source: pro:src/screens/clusters.jsx:472-487
  @pro
  Scenario: Flows of different clusters are independent
    Given an archive flow is running for one cluster
    Then the archive controls of the other clusters stay enabled and can run their own flows at the same time

  # source: pro:src/screens/clusters.jsx:118,235,274,476-487
  # decision: D2 — a running collection is lost without a word when the user only changes how the list is displayed (NOTES (a) 11)
  @pro @drop
  Scenario: Switching layout or filtering the cluster out abandons a running archive flow silently
    Given an archive flow is running for a cluster
    When the user switches the Clusters page between cards and table, or types a search that no longer matches the cluster
    Then the flow is aborted with no notification and no archive is saved
    And the control is enabled again when the cluster is next shown

  # source: pro:src/screens/clusters.jsx:118,235,274,476-487
  # decision: D2
  @pro @fixed
  Scenario: A running archive flow survives layout and search changes
    Given an archive flow is running for a cluster
    When the user switches the Clusters page between cards and table, or filters the cluster out and back in
    Then the flow continues, the cluster's control stays disabled while it runs, and its outcome is notified as usual
