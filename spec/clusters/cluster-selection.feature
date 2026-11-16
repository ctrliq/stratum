Feature: Selecting a cluster
  How the dashboard decides which cluster it is looking at: "Use" on the
  fleet page, the cluster switcher, the command palette, the root
  redirect, and what happens when the selection is stale or unreachable.

  # ---- Use ---------------------------------------------------------------

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:231,268
  # source: pro:src/app.jsx:362-365
  Scenario: Use opens the chosen cluster's overview
    Given the Clusters page lists a cluster with id "staging" that is not the selected one
    When the user clicks "Use" on it
    Then the browser navigates to "/c/staging/overview"
    And "staging" becomes the selected cluster

  @pro @fleet
  # source: pro:src/app.jsx:362-365
  Scenario: Use drops the namespace filter and other query parameters
    Given the user was filtering by namespace before opening the Clusters page
    When the user clicks "Use" on a cluster
    Then the overview opens without a namespace filter

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:231
  Scenario: A disconnected cluster cannot be used from the card view
    Given the card view is active
    And the hub lists a cluster with status "disconnected"
    When the user looks at its card
    Then its "Use" button is disabled

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:268
  Scenario: A disconnected cluster can still be used from the table view
    Given the table view is active
    And the hub lists a cluster with status "disconnected"
    When the user looks at its row
    Then its "Use" button is enabled

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/screens/clusters.jsx:231,268
  Scenario: A disconnected cluster cannot be used from either view
    Given the hub lists a cluster with status "disconnected"
    When the user looks at it in the card view or the table view
    Then its "Use" button is disabled

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:231
  Scenario Outline: Any status other than disconnected leaves Use enabled
    Given the card view is active
    And the hub lists a cluster with status "<status>"
    When the user looks at its card
    Then its "Use" button is enabled

    Examples:
      | status       |
      | healthy      |
      | degraded     |
      | pending      |
      | provisioning |

  # ---- what a switch does ------------------------------------------------

  @pro @fleet
  # source: pro:src/app.jsx:217-232
  # source: pro:src/data.js:63-67
  Scenario: Switching cluster never shows the previous cluster's resources
    Given cluster "a" is selected and its resources are shown
    When the user switches to cluster "b"
    Then every per-cluster view is emptied before "b" is loaded
    And the cluster list itself is kept

  @pro @fleet
  # source: pro:src/app.jsx:477,527-529
  Scenario: The first load of a cluster shows a loading hint
    Given no cluster data has been loaded in this visit
    When a cluster screen for "prod" is opened
    Then "Loading prod…" is shown until the load finishes
    And the hint is not shown on the Clusters or Operations pages

  @pro @fleet
  # source: pro:src/app.jsx:417
  Scenario: Switching cluster closes logs, exec and terminal sessions
    Given pod log or exec tabs, or a terminal, are open for cluster "a"
    When the user switches to cluster "b"
    Then those tabs and the terminal are closed

  @pro @fleet
  # source: pro:src/k8sclient.js:29-46
  # source: pro:src/live.js:333,344,399,457,842
  Scenario: A load still in flight for the previous cluster is discarded
    Given a load of cluster "a" has not finished
    When the user switches to cluster "b"
    Then nothing the load of "a" returns later is shown
    And it changes neither the reconnecting banner nor the "refreshed" time

  @pro @fleet
  # source: pro:src/k8sclient.js:34-46
  Scenario: Switching away and back does not resurrect an old load
    Given a load of cluster "a" has not finished
    When the user switches to cluster "b" and back to cluster "a"
    Then the answer of the first load of "a" is discarded
    And only the load started by the return to "a" is shown

  @pro @fleet
  # source: pro:src/k8sclient.js:54-56,82
  Scenario: A change started on one cluster is sent to that cluster after a switch
    Given a write was issued against cluster "a"
    When the user switches to cluster "b" before the write is sent or answered
    Then the write still targets cluster "a"

  @pro @fleet
  # source: pro:src/k8sclient.js:93-98
  # source: pro:src/app.jsx:211-214
  Scenario: Switching cancels the previous cluster's pending reads and exec sessions
    Given reads and exec sessions are open against cluster "a"
    When the user switches to cluster "b"
    Then the pending reads against "a" are cancelled
    And the exec sessions against "a" are closed

  @pro @fleet
  # source: pro:src/cluster-context.js:25,31,68-73
  Scenario: The selection is remembered across reloads
    Given the user selected the cluster "staging"
    When the user reloads the dashboard on the Clusters page
    Then "staging" is still the selected cluster

  @pro @fleet
  # source: pro:src/cluster-context.js:78-81
  Scenario Outline: Kubernetes requests are addressed according to the hub's mode
    Given the hub runs in <mode> mode
    And the selected cluster is "prod"
    When the dashboard reads a Kubernetes resource
    Then the request path starts with "<prefix>"

    Examples:
      | mode       | prefix                     |
      | fleet      | /api/v1/clusters/prod/k8s  |
      | standalone | /k8s                       |

  @pro @fleet
  # source: pro:src/cluster-context.js:79
  Scenario: Standalone mode uses the Kubernetes path the hub configured
    Given the hub runs in standalone mode and configured a Kubernetes base path
    When the dashboard reads a Kubernetes resource
    Then the request path starts with the configured base path

  # ---- cluster switcher and palette ----------------------------------------

  @pro @fleet
  # source: pro:src/ui/shell.jsx:350-366
  Scenario: The cluster switcher names the selected cluster
    Given the cluster "prod" with context "Production" and provider "aws" is selected
    When the user looks at the sidebar
    Then the switcher shows "Production · aws" above "prod"
    And its tooltip reads "Switch cluster — current: prod"

  @pro @fleet
  # source: pro:src/ui/shell.jsx:351,355,359
  # source: pro:src/ui/shell.jsx:193-199
  Scenario: The cluster switcher says when no cluster is selected
    Given the selection matches no cluster in the list
    When the user looks at the sidebar
    Then the switcher shows "— · —" above "No cluster"
    And its tooltip reads "Choose a cluster"

  @pro @fleet
  # source: pro:src/ui/shell.jsx:277,354
  # source: pro:src/app.jsx:519
  Scenario: The cluster switcher opens the Clusters page
    Given any screen of the dashboard is open
    When the user clicks the cluster switcher
    Then the Clusters page opens
    And the selected cluster does not change

  @pro @fleet
  # source: pro:src/ui/shell.jsx:231-236
  Scenario: The breadcrumb starts with a link to the Clusters page
    Given the cluster "prod" is selected
    When the user looks at the breadcrumb
    Then it reads "Clusters › prod › <screen>"
    And clicking "Clusters" opens the Clusters page

  @pro @fleet
  # source: pro:src/ui/shell.jsx:233-236
  Scenario: The breadcrumb omits the cluster when none is selected
    Given the selection matches no cluster in the list
    When the user looks at the breadcrumb
    Then it reads "Clusters › <screen>"

  @pro @fleet
  # source: pro:src/ui/palette.jsx:57-61
  # source: pro:src/app.jsx:538-541
  Scenario: The command palette lists every cluster under Switch cluster
    Given the hub lists the clusters "prod" and "staging" and "prod" is selected
    When the user opens the command palette
    Then a "Switch cluster" group lists both clusters
    And each entry shows "<context> · <provider> · <ready>/<total> nodes · <status>"
    And the "prod" entry is marked as current

  @pro @fleet
  # source: pro:src/ui/palette.jsx:59
  Scenario: A palette cluster entry shows placeholders for missing facts
    Given a cluster reports no node counts and no status
    When the user opens the command palette
    Then its entry shows "0/0 nodes · —"

  @pro @fleet
  # source: pro:src/ui/palette.jsx:60
  # source: pro:src/app.jsx:362-365,541
  Scenario: Picking a cluster in the palette opens its overview
    Given the command palette is open
    When the user picks the cluster "staging"
    Then the browser navigates to "/c/staging/overview"

  # ---- root redirect ---------------------------------------------------------

  @pro @fleet
  # source: pro:src/app.jsx:49-52,461-464
  Scenario: The root path opens the fleet manager cluster's overview
    Given the cluster list contains a cluster flagged as the fleet manager, with id "hub"
    When the user opens "/"
    Then the browser is redirected to "/c/hub/overview"

  @pro @fleet
  # source: pro:src/app.jsx:49-52,461-464
  Scenario: Without a fleet manager cluster the root path opens the first cluster
    Given no listed cluster is flagged as the fleet manager
    And the first listed cluster has id "alpha"
    When the user opens "/"
    Then the browser is redirected to "/c/alpha/overview"

  @fixed @pro @fleet
  # decision: D13
  # source: pro:src/app.jsx:462
  Scenario Outline: With no cluster at all the root path depends on the mode
    Given the cluster list is empty
    And the hub runs in <mode> mode
    When the user opens "/"
    Then <result>

    Examples:
      | mode       | result                                         |
      | fleet      | the browser is redirected to the Clusters page |
      | standalone | only "No cluster available." is shown          |

  @pro @fleet
  # source: pro:src/app.jsx:54-64,461-464
  Scenario: Any path the dashboard does not know is treated like the root path
    Given the cluster list contains at least one cluster
    When the user opens a path that is neither the Clusters page, the Operations page nor a cluster screen
    Then the browser is redirected to the default cluster's overview

  @pro @fleet
  # source: pro:src/app.jsx:139-141
  Scenario: The sign-in path redirects to the root when no sign-in is needed
    Given the user is already signed in, or the hub does not require sign-in
    When the user opens "/login"
    Then the browser is redirected to "/" and from there to the default cluster's overview

  @pro @fleet
  # source: pro:src/app.jsx:204-205,216-230
  Scenario: A cluster screen URL selects the cluster it names
    Given the remembered selection is "a"
    When the user opens "/c/b/overview" directly
    Then "b" becomes the selected cluster and is loaded

  @pro @fleet
  # source: pro:src/app.jsx:204-206
  # source: pro:src/cluster-context.js:68
  Scenario: The Clusters page keeps the remembered selection
    Given the remembered selection is "a"
    When the user opens the Clusters page
    Then "a" stays selected and keeps being refreshed in the background

  @drop @pro @fleet
  # decision: D2
  # source: pro:src/cluster-context.js:68
  # source: pro:src/app.jsx:205,216-230
  Scenario: With nothing remembered the Clusters page selects a cluster called "local"
    Given this browser has no remembered selection
    When the user opens the Clusters page directly
    Then the dashboard selects and tries to load a cluster with id "local"

  @fixed @pro @fleet
  # decision: D2
  # source: pro:src/cluster-context.js:68
  # source: pro:src/app.jsx:205,216-230
  Scenario: With nothing remembered the Clusters page selects no cluster
    Given the backend is a fleet hub and this browser has no remembered selection
    When the user opens the Clusters page directly
    Then the fleet is shown with no cluster marked as current
    And no cluster is loaded in the background and no reconnecting banner is raised

  # ---- stale selection ---------------------------------------------------------

  @drop @pro @fleet
  # decision: D12
  # assumption: which requests fail is the hub's to settle; a runtime check of what the hub answers for the facts and Kubernetes version routes of an unknown cluster id would confirm whether the banner comes from the probe or from the facts read
  # source: pro:src/app.jsx:204-206,216-261
  # source: pro:src/live.js:153-174,386-391,465-467
  # source: pro:src/load-outcome.js:27-32
  # source: pro:src/poll.js:42-44
  Scenario: A URL that names a cluster the hub does not list shows empty screens and the reconnecting banner
    Given the hub runs in fleet mode and lists no cluster with id "ghost"
    When the user opens "/c/ghost/overview"
    Then the user is not redirected
    And the screen is shown with no data
    And the cluster switcher shows "No cluster"
    And the reconnecting banner is shown while auto-refresh is on
    And "ghost" is remembered as the selection
    And if the hub answers 404 for an unknown cluster, every refresh still sends the per-resource reads before giving up

  @fixed @pro @fleet
  # decision: D12
  # source: pro:src/app.jsx:204-206,216-261
  Scenario: A URL that names a cluster the hub does not list shows a not-found state
    Given the hub runs in fleet mode and lists no cluster with id "ghost"
    When the user opens "/c/ghost/overview"
    Then a not-found state names "ghost" and links to the Clusters page
    And no resource of "ghost" is requested and no reconnecting banner is raised
    And "ghost" is not remembered as the selection

  @drop @pro @fleet
  # decision: D12
  # source: pro:src/app.jsx:204-206,270-299
  # source: pro:src/live.js:297-298,325
  Scenario: The selected cluster disappearing from the list leaves the user where they are
    Given cluster "a" is selected and one of its screens is open
    When a later cluster list read no longer contains "a"
    Then the user is not redirected
    And the cluster switcher shows "No cluster" and the breadcrumb omits the cluster
    And the dashboard keeps trying to refresh "a"

  @fixed @pro @fleet
  # decision: D12
  # source: pro:src/app.jsx:204-206,270-299
  # source: pro:src/live.js:297-298,325
  Scenario: The selected cluster disappearing from the list shows the not-found state
    Given cluster "a" is selected and one of its screens is open
    When a later cluster list read no longer contains "a"
    Then the not-found state for "a" replaces the screen
    And the dashboard stops refreshing "a"

  @pro @fleet
  # source: pro:src/screens/clusters.jsx:122-134
  # source: pro:src/app.jsx:204-206
  Scenario: Unregistering the selected cluster does not pick another one
    Given cluster "a" is selected and the Clusters page is open
    When an admin unregisters "a"
    Then the user stays on the Clusters page
    And once the list is re-read the summary line ends with "context: —"

  # ---- unreachable cluster -------------------------------------------------------

  @pro @fleet
  # source: pro:src/app.jsx:232-249,544-550
  # source: pro:src/load-outcome.js:27-32
  Scenario: Opening an unreachable cluster shows the reconnecting banner at once
    Given auto-refresh is on
    When the user opens or switches to a cluster whose Kubernetes path does not answer
    Then a banner reads "Reconnecting… the selected cluster may be unreachable, or the fleet manager's serving certificate may have changed — reload to retry."
    And the banner offers "Reload", which reloads the whole page

  @pro @fleet
  # source: pro:src/app.jsx:277-296
  # source: pro:src/live.js:392-400,457-458
  Scenario: A cluster that becomes unreachable keeps its last good data
    Given cluster "a" is selected and its data is shown
    When a periodic refresh finds "a" unreachable
    Then the data already shown stays on screen
    And the reconnecting banner is shown
    And the "refreshed" time is not updated

  @drop @pro @fleet
  # decision: D9
  # source: pro:src/poll.js:42-44
  # source: pro:src/live.js:47-58
  Scenario Outline: The probe decides reachability from the answer's status
    Given a cluster is selected
    When the reachability probe of its Kubernetes path answers with <answer>
    Then the cluster is treated as <verdict>

    Examples:
      | answer                    | verdict     |
      | status 200                | reachable   |
      | status 401                | reachable   |
      | status 403                | reachable   |
      | status 404                | reachable   |
      | status 429                | unreachable |
      | status 500                | unreachable |
      | status 502                | unreachable |
      | status 503                | unreachable |
      | no response               | unreachable |
      | a body that is not JSON   | unreachable |

  @drop @pro @fleet
  # decision: D9
  # source: pro:src/live.js:392-400
  Scenario: An unreachable cluster costs one probe request per refresh
    Given the selected cluster's probe says unreachable
    When a refresh runs
    Then none of the per-resource reads are sent for that refresh

  @pro @fleet
  # source: pro:src/load-outcome.js:41-44
  # source: pro:src/poll.js:9-26
  Scenario Outline: Refreshes of an unreachable cluster back off exponentially
    Given the selected cluster has failed <failures> refreshes in a row
    When the next refresh is scheduled
    Then it waits between <min> and <max> seconds

    Examples:
      | failures | min   | max    |
      | 1        | 15    | 17.25  |
      | 2        | 25.5  | 34.5   |
      | 3        | 51    | 69     |
      | 4        | 102   | 120    |
      | 5        | 102   | 120    |
      | 10       | 102   | 120    |

  @pro @fleet
  # source: pro:src/load-outcome.js:41-44
  # source: pro:src/app.jsx:286-296
  Scenario: Recovery clears the banner and restores the 15 second cadence
    Given the reconnecting banner is shown
    When a refresh of the selected cluster succeeds
    Then the banner disappears
    And the next refresh is scheduled 15 seconds later

  @pro @fleet
  # source: pro:src/app.jsx:273
  Scenario: Pausing auto-refresh clears the reconnecting banner
    Given the reconnecting banner is shown
    When the user pauses auto-refresh
    Then the banner disappears and is not raised again while paused

  @pro @fleet
  # source: pro:src/app.jsx:366-383
  Scenario: A manual refresh of an unreachable cluster says it kept the last data
    Given the selected cluster is unreachable
    When the user triggers a manual refresh
    Then a toast reads "Refresh failed — keeping last data"
    And the reconnecting banner is raised only if auto-refresh is on

  @pro @fleet
  # source: pro:src/live.js:317-325,339-343
  Scenario: The cluster list keeps refreshing while the selected cluster is unreachable
    Given the selected cluster is unreachable and auto-refresh is on
    When periodic refreshes run
    Then each of them still requests the cluster list
