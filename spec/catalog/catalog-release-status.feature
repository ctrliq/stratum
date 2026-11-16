Feature: Installed release status in the catalog
  Whether an application is installed is never a local flag: it is read from
  the cluster's HelmChart resources that carry the catalog label, and the
  state shown is derived from each resource and the clock.

  # source: pro:src/catalog.js:35,37
  # source: pro:src/screens/catalog.jsx:234-252,262-269
  @pro
  Scenario: An application is installed when a HelmChart anywhere in the cluster carries its catalog label
    Given a HelmChart in any namespace is labelled "catalog.stratum.ciq.com/app" with an application's id
    When the installed releases are read
    Then that application is shown as having a release
    And HelmCharts without that label are ignored by the catalog

  # source: pro:src/screens/catalog.jsx:262-269
  # assumption: when several HelmCharts carry the same application id the code keeps whichever the list returns last; nothing documents this as intended
  @pro
  Scenario: When several HelmCharts carry one application's label the last one listed represents it
    Given two HelmCharts are labelled with the same application id
    When the installed releases are read
    Then the application shows the state of the one the cluster listed last

  # source: pro:src/screens/catalog.jsx:277-279
  @pro
  Scenario: A labelled HelmChart for an application the catalog does not list is not counted
    Given a HelmChart is labelled with an application id that is not in the catalog index
    Then it adds nothing to the installed count and appears nowhere on the screen
    But in the legacy dashboard it still counts towards the fast re-read cadence while it is installing or removing

  # source: pro:src/catalog-rules.js:19,25-36
  @pro
  Scenario Outline: A release's state is derived from its HelmChart and the clock
    Given an application's HelmChart has <deletion> and reports phase <phase>
    Then the application's release state is <state>

    Examples:
      | deletion                                         | phase        | state      |
      | no deletion timestamp                            | "Deployed"   | deployed   |
      | no deletion timestamp                            | "Failed"     | failed     |
      | no deletion timestamp                            | "Pending"    | installing |
      | no deletion timestamp                            | "Upgrading"  | installing |
      | no deletion timestamp                            | no phase     | installing |
      | no deletion timestamp                            | "deployed"   | installing |
      | a deletion timestamp 30 seconds old              | "Deployed"   | removing   |
      | a deletion timestamp 119 seconds old             | "Failed"     | removing   |
      | a deletion timestamp exactly 120 seconds old     | "Deployed"   | stuck      |
      | a deletion timestamp 10 minutes old              | no phase     | stuck      |
      | a deletion timestamp that cannot be parsed       | "Deployed"   | removing   |

  # source: pro:src/catalog-rules.js:25-26
  # source: pro:src/screens/catalog.jsx:361,531
  @pro
  Scenario: An application without a labelled HelmChart has no release state
    Given no HelmChart carries an application's label
    Then the application shows no status and is offered for install

  # source: pro:src/screens/catalog.jsx:78-113,364-369,397,531
  # source: pro:src/k8srules.js:9-16
  @pro
  Scenario Outline: The table and the essentials strip show a release's status
    Given an application's release state is <state>
    Then its status reads <text> with <mark>

    Examples:
      | state      | text                                                        | mark                    |
      | deployed   | "Deployed <age since creation> ago"                         | a green dot             |
      | failed     | "Failed"                                                    | a red dot               |
      | installing | "Installing"                                                | a spinner               |
      | removing   | "Removing <time since the deletion was accepted>"           | a spinner               |
      | stuck      | "Still removing <time since the deletion was accepted>"     | a red dot, no spinner   |

  # source: pro:src/screens/catalog.jsx:483,492-494
  @pro
  Scenario Outline: A card shows a release's status
    Given the applications are shown as cards
    And an application's release state is <state>
    Then its card shows <display>

    Examples:
      | state      | display                                                                  |
      | deployed   | a "Deployed" pill beside the name and "<age since creation> ago" below    |
      | failed     | "Failed" with a red dot                                                   |
      | installing | "Installing" with a spinner                                               |
      | removing   | "Removing <time since the deletion was accepted>" with a spinner          |
      | stuck      | "Still removing <time since the deletion was accepted>" with a red dot    |

  # source: pro:src/k8srules.js:9-16
  @pro
  Scenario Outline: Ages are shown in one coarse unit
    Given a timestamp <elapsed> in the past
    Then its age reads "<age>"

    Examples:
      | elapsed       | age  |
      | 5 seconds     | 5s   |
      | 89 seconds    | 89s  |
      | 90 seconds    | 2m   |
      | 89 minutes    | 89m  |
      | 90 minutes    | 2h   |
      | 47 hours      | 47h  |
      | 48 hours      | 2d   |
      | 10 days       | 10d  |

  # source: pro:src/screens/catalog.jsx:364-369
  @pro
  Scenario: A release without a creation timestamp shows no age
    Given a deployed release's HelmChart carries no creation timestamp
    Then its status reads "Deployed" with no age

  # source: pro:src/screens/catalog.jsx:218-221,234-252
  # source: pro:src/poll.js:9
  # decision: D2 — the cadence follows every HelmChart in the cluster, not only catalog releases (NOTES (a) 20)
  @pro @drop
  Scenario: Installed releases are re-read every 15 seconds while the catalog is open
    Given the App Catalog is open and no HelmChart in the cluster, labelled or not, is installing or being removed
    Then the installed releases are read immediately and again 15 seconds after each answer
    And reading stops when the user leaves the App Catalog

  # source: pro:src/screens/catalog.jsx:218-221,239
  # decision: D2 — a HelmChart unrelated to the catalog, or one that never reports a phase, holds the fast cadence indefinitely (NOTES (a) 20)
  @pro @drop
  Scenario: Installed releases are re-read every 3 seconds while a release is in flight
    Given at least one HelmChart in the cluster, with or without the catalog label, is installing or removing
    Then the installed releases are re-read 3 seconds after each answer
    And the 15-second cadence resumes once every HelmChart in the cluster is deployed, failed, stuck or gone

  # source: pro:src/screens/catalog.jsx:218-221,234-252,262-269
  # decision: D2
  @pro @fixed
  Scenario: Only catalog releases decide how often installed releases are re-read
    Given the App Catalog is open
    Then the installed releases are read immediately and again 15 seconds after each answer
    And they are re-read 3 seconds after each answer only while a HelmChart carrying the catalog label of a listed application is installing or removing
    And HelmCharts without that label never change the cadence
    And reading stops when the user leaves the App Catalog

  # source: pro:src/screens/catalog.jsx:208-221
  @pro
  Scenario: A removal that is stuck does not keep the fast cadence
    Given the only release in flight has been removing for 120 seconds or more
    Then the installed releases are re-read on the 15-second cadence

  # source: pro:src/screens/catalog.jsx:241
  # source: pro:src/poll.js:9
  @pro
  Scenario: A read of installed releases is abandoned after 15 seconds
    Given a read of the installed releases has not answered after 15 seconds
    Then it is treated as a failed read

  # source: pro:src/screens/catalog.jsx:49,243-248,555-560
  # source: pro:src/poll.js:9-26
  @pro
  Scenario: A failed read keeps the last known releases and says the state is unverified
    Given the installed releases were read successfully
    When a later read fails for any reason, including a forbidden answer
    Then the releases from the last successful read stay on screen
    And a red notice reads "Unable to list installed apps — the apiserver returned an error (or the RBAC lacks access). Retrying — Install is held back until a list succeeds."
    And the subtitle reads "install state unknown" and the Installed count reads "—"
    And the next read waits longer after each consecutive failure, doubling from the current cadence up to 2 minutes with about 15% jitter

  # source: pro:src/screens/catalog.jsx:246,555
  @pro
  Scenario: A successful read clears the notice
    Given the unverified-state notice is shown
    When a read of the installed releases succeeds
    Then the notice disappears, the installed count is shown again and the normal cadence resumes

  # source: pro:src/screens/catalog.jsx:555
  @pro
  Scenario: The notice is not shown while the index is loading
    Given the read of installed releases failed
    And the catalog index is still loading or being refreshed
    Then the unverified-state notice is not shown until the index has resolved

  # source: pro:src/screens/catalog.jsx:171-176,397,462-467,533
  @pro
  Scenario: Install is withheld everywhere while the installed releases are unknown
    Given the installed releases have not been read yet or their latest read failed
    Then no quick "Install" button is shown on any row, card or essentials card
    And applications already known to have a release keep showing their status

  # source: pro:src/screens/catalog.jsx:223-232
  @pro @fleet
  Scenario: Switching cluster forgets the previous cluster's releases
    Given the App Catalog shows cluster A's installed releases
    When the user switches to cluster B
    Then every application shows no status and the install state is unknown
    And Install stays withheld until cluster B's own releases have been read

  # source: pro:src/screens/catalog.jsx:184,205,331,354
  @pro
  Scenario: Installed releases are re-read right after any install, uninstall or refresh
    When an install or uninstall request completes, successfully or not, or the user presses "Refresh"
    Then the installed releases are read again immediately without waiting for the cadence
