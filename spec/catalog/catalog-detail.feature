Feature: Catalog application detail
  The detail panel describes one application: what it is, which chart it
  installs, and — once a release exists — what the operator reports about it.

  # source: pro:src/screens/catalog.jsx:694-713
  @pro
  Scenario: The panel header identifies the application
    When the detail panel is open for an application
    Then the header shows its mark and name
    And a line with its chart name and version and its category

  # source: pro:src/screens/catalog.jsx:704-706
  # source: pro:src/catalog.js:89-96,147
  @pro
  Scenario: A project home link is offered only for a web address
    Given the application's index entry has an http or https project home
    Then the header shows a "Project home ↗" link that opens that address in a new tab
    But an application whose project home is missing or not an http or https address shows no link

  # source: pro:src/screens/catalog.jsx:699-700,677
  @pro
  Scenario Outline: The panel header shows the release state
    Given the application's release state is <state>
    Then the header shows <display>

    Examples:
      | state        | display                                                       |
      | none         | no state                                                      |
      | deployed     | a "Deployed" pill                                             |
      | installing   | "Installing" with a spinner                                   |
      | failed       | "Failed" with a red dot                                       |
      | removing     | "Removing <time since the deletion was accepted>" with a spinner |
      | stuck        | "Still removing <time since the deletion was accepted>" with a red dot |

  # source: pro:src/screens/catalog.jsx:717-719
  @pro
  Scenario Outline: The panel shows the application's description
    Given the application has <texts>
    Then the panel body starts with <shown>

    Examples:
      | texts                                              | shown                                             |
      | a long description with blank lines between parts  | one paragraph per part of the long description    |
      | only a short description                           | the short description                             |
      | neither                                            | "—"                                               |

  # source: pro:src/screens/catalog.jsx:721-728
  # source: pro:src/ui/fields.jsx:52-62
  @pro
  Scenario: The Chart section lists what will be installed
    When the detail panel is open for an application
    Then a "Chart" section shows "Repository", "Chart" and "Version" from the index
    And "Target namespace" shows the namespace currently entered in the install form, or "—" when it is empty
    And "Install timeout" is shown only when the index states one
    And each value is available in full as a tooltip when it is truncated

  # source: pro:src/screens/catalog.jsx:617,653,726
  # decision: D2 — the row can name a namespace the release is not in (NOTES (a) 4)
  @pro @drop
  Scenario: The Chart section's target namespace is the form's value even when the application is installed
    Given an application was installed into a namespace other than its catalog default
    When its detail panel is open
    Then the Chart section's "Target namespace" shows the install form's current value: the namespace the user entered if the panel has stayed open since installing from it, otherwise the catalog default
    And the "Release status" section's "Release namespace" shows the namespace the operator reports

  # source: pro:src/screens/catalog.jsx:640,726
  # decision: D2
  @pro @fixed
  Scenario: The Chart section of an installed application names the release's own target namespace
    Given an application was installed into a namespace other than its catalog default
    When its detail panel is open, whenever it was opened
    Then the Chart section's "Target namespace" shows the target namespace recorded in the release's HelmChart

  # source: pro:src/screens/catalog.jsx:730-739
  @pro
  Scenario: The application's notes are shown as an informational callout
    Given the application's index entry has notes
    Then the notes are shown below the Chart section, whether or not the application is installed

  # source: pro:src/screens/catalog.jsx:741,767-777,809
  @pro
  Scenario: An installed application shows the operator's release status instead of the install form
    Given the application has a release
    When its detail panel is open
    Then a "Release status" section is shown with "Phase", "Revision", "Chart version", "Release", "Release namespace" and "Installed"
    And no install form is shown

  # source: pro:src/screens/catalog.jsx:769-770
  @pro
  Scenario Outline: The Phase row reflects the release state
    Given the release state is <state> and the operator reports phase <phase>
    Then "Phase" reads <text> in <colour>

    Examples:
      | state      | phase        | text                                         | colour            |
      | deployed   | "Deployed"   | "Deployed"                                   | green             |
      | failed     | "Failed"     | "Failed"                                     | red               |
      | installing | "Upgrading"  | "Upgrading"                                  | the default colour |
      | installing | none         | "Pending"                                    | the default colour |
      | removing   | any          | "Removing (<time since deletion>)"           | the default colour |
      | stuck      | any          | "Removing (stuck <time since deletion>)"     | red               |

  # source: pro:src/screens/catalog.jsx:771-775
  # source: pro:src/ui/fields.jsx:59
  @pro
  Scenario: Status fields the operator has not reported read as a dash
    Given the operator has not yet written a revision, chart version, release name or release namespace
    Then each of those rows reads "—"
    And "Installed" reads "<age> ago" from the HelmChart's creation time, or "—" when it has none
    And a revision of 0 is shown as "0"

  # source: pro:src/screens/catalog.jsx:743-752,776
  @pro
  Scenario: A failed release shows the operator's message as an error callout
    Given the release state is failed and the operator reports a message
    Then the message is shown in a red callout above the "Release status" section
    And the section has no "Message" row

  # source: pro:src/screens/catalog.jsx:743,776
  @pro
  Scenario: A release that has not failed shows the operator's message as a status row
    Given the release state is not failed and the operator reports a message
    Then the "Release status" section has a "Message" row with that message
    And no error callout is shown for it

  # source: pro:src/screens/catalog.jsx:753-766
  @pro
  Scenario: A stuck removal explains that it will not clear on its own
    Given the release has been removing for 120 seconds or more
    Then a red callout reads "Still removing after <time> — check the stratum-operator logs. The HelmChart keeps its finalizer until helm uninstall succeeds, so this will not clear on its own."

  # source: pro:src/screens/catalog.jsx:678-680,778-786
  @pro
  Scenario: Additional status fields the operator reports are listed verbatim
    Given the HelmChart's status carries a non-empty text field other than phase, specHash, message, revision, chartVersion, releaseName, releaseNamespace, lastAttempt and observedGeneration
    Then it is shown below the status rows as "<field>: <value>"
    But fields that are empty or not text are not shown

  # source: pro:src/screens/catalog.jsx:681,787-798
  # source: pro:src/catalog.js:472-476
  @pro
  Scenario: The values an installed release was configured with are listed
    Given the release's HelmChart carries form values
    Then a "Configured values" section lists each key with its value
    And when the same key was set both as a typed value and as a text value, the text value is the one shown

  # source: pro:src/screens/catalog.jsx:686,791
  # source: pro:src/catalog.js:433-442
  @pro
  Scenario: Credential values stored in an older release are never displayed
    Given the release's HelmChart carries a value for a parameter the catalog declares as a password
    Then its row in "Configured values" shows "••••••••" instead of the value
    And the value is not available as a tooltip either

  # source: pro:src/screens/catalog.jsx:792
  @pro
  Scenario Outline: Free-form values of an installed release are acknowledged but not printed
    Given the release's HelmChart carries free-form YAML values
    And the application <credentials>
    Then "Configured values" has a "values.yaml" row reading "<text>"

    Examples:
      | credentials                   | text                                   |
      | declares password parameters  | provided (hidden)                      |
      | declares no password parameter | provided (see the HelmChart resource) |

  # source: pro:src/screens/catalog.jsx:793-795
  @pro
  Scenario: The credentials Secret an installed release names is shown by name only
    Given the release's HelmChart names a values Secret "my-creds"
    Then "Configured values" has a "credentials" row reading "Secret kube-system/my-creds"

  # source: pro:src/screens/catalog.jsx:787
  @pro
  Scenario: The Configured values section is omitted when the release carries none
    Given the release's HelmChart carries no form values, no free-form values and no values Secret
    Then no "Configured values" section is shown

  # source: pro:src/screens/catalog.jsx:799-805
  # source: pro:src/app.jsx:507
  @pro
  Scenario: The HelmChart resource can be opened from an installed application without credentials
    Given the application has a release and declares no password parameter
    When the user presses "View HelmChart resource"
    Then the HelmCharts resource list of the selected cluster is opened

  # source: pro:src/screens/catalog.jsx:799
  @pro
  Scenario: No shortcut to the raw HelmChart is offered for an application with credentials
    Given the application has a release and declares at least one password parameter
    Then no "View HelmChart resource" button is shown

  # source: pro:src/screens/catalog.jsx:922-926
  @pro
  Scenario Outline: The panel footer states how an install would run
    Given the application has no release
    And <known>
    Then the footer reads "<text>"

    Examples:
      | known                                                        | text                                              |
      | the installed releases are known and the index sets timeout 10m | Helm install · timeout 10m                     |
      | the installed releases are known and the index sets no timeout  | Helm install · timeout 5m                      |
      | the installed releases are unknown                            | Install state unknown — cannot install safely    |

  # source: pro:src/screens/catalog.jsx:147-148,397,462,625,652,928-951
  # source: pro:src/fleet.js:48-59
  # decision: D14 — on a hub, write controls are offered only to the admin role; unchanged
  @pro
  Scenario: A read-only user can browse everything but is offered no action
    Given the signed-in user does not have the admin role
    When the user browses the catalog and opens applications
    Then every application, status, release detail and the install form with its manifest preview are shown
    But no "Install", "Uninstall" or "Retry uninstall" button is shown anywhere
    And a release being removed still shows the disabled "Uninstalling… <time>" indicator

  # source: pro:src/fleet.js:48-55
  # decision: D14, D6 — the login-free fallback exists only in the development server; write gating follows the backend
  @pro @drop
  Scenario: Without a hub sign-in, actions are offered only in unauthenticated standalone mode
    Given no user identity is known
    Then install and uninstall actions are offered only when the hub runs in standalone mode with authentication not required

  # source: pro:src/fleet.js:48-55
  # decision: D14
  @pro @fixed
  Scenario: Install and uninstall are offered according to the backend's write gating
    Given the catalog is shown against a fleet hub
    Then install and uninstall actions are offered only to a signed-in identity with the admin role
    But against the single-cluster dashboard server they are offered to every signed-in identity, and a forbidden answer from the cluster is reported like any other failure

  # source: pro:src/screens/catalog.jsx:617,653-659,809
  @pro
  Scenario: The install form keeps the user's edits while the panel stays open across an install and a removal
    Given the user edited the install form and installed the application from the panel
    When the release later disappears while the panel has stayed open
    Then the form reappears with the user's earlier edits, not the seeded values
    And only closing and reopening the panel, selecting another application or pressing "Reset" restores the seeded values

  # source: pro:src/screens/catalog.jsx:254-260,625-645
  @pro
  Scenario: Escape closes the panel even when focus is in the install form or the confirmation is open
    Given an application's detail panel is open
    When the user presses Escape while typing in the install form
    Then the panel closes and the edits are gone when it is reopened
    And when the uninstall confirmation is open, Escape also closes the panel behind it while the confirmation's own state does not depend on the panel
