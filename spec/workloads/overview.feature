Feature: Workloads overview
  One list across Deployments, StatefulSets, DaemonSets, Jobs and CronJobs,
  drawn from the dashboard's cluster snapshot (see spec/data for how the
  snapshot is built), with a kind filter, a search, and a jump into each
  kind's own list.

  @oss @pro
  # source: oss:src/app.jsx:485
  # source: oss:src/registry.js:37
  # source: oss:src/screens/workloads.jsx:19-20,42
  # source: pro:src/app.jsx:510
  # source: pro:src/nav.js:11
  # source: pro:src/screens/workloads.jsx:19-20,42
  Scenario: The overview is the first row of the Workloads section
    When the user opens the route "workloads" or the sidebar row "Overview" under "Workloads"
    Then a screen titled "Workloads" lists every workload in the cluster snapshot
    And the sidebar row carries the number of workloads in the snapshot

  @oss @pro
  # source: oss:src/screens/workloads.jsx:10,20
  # source: pro:src/screens/workloads.jsx:10,20
  Scenario: Pods, replica sets and replication controllers are not part of the overview
    Given the cluster has pods, replica sets and replication controllers
    Then none of them is a row of the overview
    And the overview's kinds are Deployment, StatefulSet, DaemonSet, Job and CronJob

  @oss @pro
  # source: oss:src/screens/workloads.jsx:67-90
  # source: pro:src/screens/workloads.jsx:68-91
  Scenario: The overview's columns
    Then the overview's columns are, in order: Name, Kind, Namespace, Ready, Status, CPU, Mem, Restarts, Age
    And each name is preceded by an icon for its kind
    And the Kind is shown as a neutral badge and the Status as a status badge

  @oss @pro
  # source: oss:src/screens/workloads.jsx:16
  # source: pro:src/screens/workloads.jsx:16
  Scenario Outline: CPU is shown in millicores or cores
    Given a workload's pods use <millicores> millicores
    Then its CPU column reads "<text>"

    Examples:
      | millicores | text       |
      | 0          | —          |
      | 250        | 250m       |
      | 999.6      | 1000m      |
      | 1000       | 1.00 cores |
      | 2500       | 2.50 cores |

  @oss @pro
  # source: oss:src/screens/workloads.jsx:17
  # source: pro:src/screens/workloads.jsx:17
  Scenario Outline: Memory is shown in Mi or GiB
    Given a workload's pods use <mebibytes> MiB
    Then its Mem column reads "<text>"

    Examples:
      | mebibytes | text    |
      | 0         | —       |
      | 512       | 512 Mi  |
      | 1024      | 1.0 GiB |
      | 1536      | 1.5 GiB |

  @oss @pro
  # source: oss:src/screens/workloads.jsx:88
  # source: pro:src/screens/workloads.jsx:89
  Scenario: Restarts default to zero
    Given the snapshot holds no restart count for a workload
    Then its Restarts column reads "0"

  @pro
  # source: pro:src/screens/workloads.jsx:85
  Scenario: A Ready figure knows when it was observed
    Given the snapshot records when a workload's readiness was observed
    When the user hovers that row's Ready value
    Then a tooltip reads "Observed <time>"

  # ---- narrowing -------------------------------------------------------------

  @oss @pro
  # source: oss:src/screens/workloads.jsx:25
  # source: pro:src/screens/workloads.jsx:25
  Scenario: The overview follows the selected namespace
    Given a namespace is selected in the top bar
    Then only workloads in that namespace are rows

  @oss @pro
  # source: oss:src/screens/workloads.jsx:10,21,26,50-57
  # source: pro:src/screens/workloads.jsx:10,21,26,50-57
  Scenario: A kind filter narrows to one kind
    Given the overview is shown with the filter on "All"
    Then the filter offers "All", "Deployment", "StatefulSet", "DaemonSet", "Job" and "CronJob"
    When the user chooses "StatefulSet"
    Then only stateful sets are rows and "StatefulSet" is marked as chosen

  @oss @pro
  # source: oss:src/screens/workloads.jsx:31-35,54
  # source: pro:src/screens/workloads.jsx:31-35,54
  Scenario: Each kind filter shows how many workloads of that kind exist cluster-wide
    Given the snapshot holds 7 workloads, 4 of them deployments
    And a namespace is selected that contains only 1 of those deployments
    Then the "All" filter is labelled with 7 and "Deployment" with 4
    And a kind with no workloads in the snapshot shows no number

  @oss @pro
  # source: oss:src/screens/workloads.jsx:27-28,59
  # source: pro:src/screens/workloads.jsx:27-28,59
  Scenario Outline: Search matches name, namespace, kind or image
    Given the overview holds Deployment "api" in namespace "payments" running image "registry.local/api:1.4"
    When the user types "<query>" in the box labelled "Search workloads…"
    Then that workload <outcome>

    Examples:
      | query        | outcome      |
      | API          | is a row     |
      | paym         | is a row     |
      | deploy       | is a row     |
      | registry.loc | is a row     |
      | "  api  "    | is a row     |
      | redis        | is not a row |

  @oss @pro
  # source: oss:src/screens/workloads.jsx:23-30
  # source: pro:src/screens/workloads.jsx:23-30
  Scenario: Namespace, kind and search narrow together
    When a namespace is selected, a kind is chosen and a search is typed
    Then only workloads satisfying all three are rows

  @oss @pro
  # source: oss:src/screens/workloads.jsx:21-22
  # source: pro:src/screens/workloads.jsx:21-22
  # assumption: the kind filter and search are not written to the address; inferred from the absence of any such write
  Scenario: The kind filter and search are forgotten on leaving
    Given a kind is chosen and a search is typed
    When the user leaves the overview and comes back, or reloads
    Then the filter is on "All" and the search is empty

  # ---- summary line ----------------------------------------------------------

  @oss @pro
  # source: oss:src/screens/workloads.jsx:43-45
  # source: pro:src/screens/workloads.jsx:43-45
  Scenario Outline: The line under the title counts the rows
    Given the snapshot holds <total> workloads and <shown> pass the filters
    And <namespace> is selected
    Then the line under the title starts "<text>"

    Examples:
      | total | shown | namespace       | text                           |
      | 7     | 7     | All namespaces  | 7 workloads                    |
      | 7     | 2     | All namespaces  | 2 of 7 workloads               |
      | 7     | 2     | payments        | 2 of 7 workloads · payments    |
      | 1     | 1     | All namespaces  | 1 workloads                    |

  @oss @pro
  # source: oss:src/screens/workloads.jsx:36,46
  # source: pro:src/screens/workloads.jsx:36,46
  Scenario: The line flags degraded workloads cluster-wide
    Given 2 workloads in the snapshot have status "Failed" or "Degraded"
    Then the line under the title ends with " · 2 degraded" in the error colour
    And the figure does not change with the namespace, kind filter or search
    And the flag is absent when no workload is failed or degraded

  # ---- empty, loading, stale -------------------------------------------------

  @oss @pro
  # source: oss:src/screens/workloads.jsx:92-96
  # source: pro:src/screens/workloads.jsx:93-97
  Scenario Outline: An empty result says what was looked for
    Given no workload passes the filters and the kind filter is on "<kind>"
    Then the table shows "<message>"

    Examples:
      | kind       | message                              |
      | All        | No workloads found.                  |
      | Deployment | No workloads of kind Deployment found. |

  @oss @pro @drop
  # decision: D2 — claiming nothing was found before anything was read is not reproduced
  # source: oss:src/screens/workloads.jsx:20,44,92-96
  # source: oss:src/app.jsx:470,504-506
  # source: pro:src/screens/workloads.jsx:20,44,93-97
  # source: pro:src/app.jsx:477,527-529
  Scenario: While the first snapshot is loading the overview looks empty
    Given the cluster snapshot has not loaded yet
    Then the overview reads "0 workloads" and "No workloads found."
    And the only loading cue is the shell's marker at the top right of the page, reading "Loading…" in the open dashboard and "Loading <cluster id>…" in the fleet dashboard

  @oss @pro @fixed
  # decision: D2 — replaces the scenario above
  # source: oss:src/screens/workloads.jsx:20,44,92-96
  # source: pro:src/screens/workloads.jsx:20,44,93-97
  Scenario: While workloads are first loading the overview says it is loading
    Given workloads have not been read yet
    Then the overview shows a loading state in place of the count and the rows
    And "No workloads found." is not shown until a read has answered

  @oss @pro @drop
  # decision: D2, D9 — a failed or forbidden read is not shown as an empty cluster
  # source: oss:src/screens/workloads.jsx:20
  # source: pro:src/screens/workloads.jsx:20
  # assumption: the overview has no error or forbidden state of its own; inferred from the absence of any such branch. Whether the snapshot holds workloads after a failed or forbidden read is decided by the loader (spec/data)
  Scenario: The overview cannot tell an unreadable cluster from an empty one
    Given the snapshot holds no workloads because they could not be read or the user may not list them
    Then the overview shows "No workloads found." and no error

  @oss @pro @fixed
  # decision: D2, D9 — replaces the scenario above
  # source: oss:src/screens/workloads.jsx:20,92-96
  # source: pro:src/screens/workloads.jsx:20,93-97
  Scenario: The overview says when workloads could not be read
    Given one of the overview's kinds could not be read or the user may not list it
    Then the overview names that kind as unreadable or not permitted
    And the kinds that were read are still listed
    And "No workloads found." is shown only when every kind was read and none has a workload

  @oss @pro
  # source: oss:src/screens/workloads.jsx:20
  # source: oss:src/app.jsx:314-323
  # source: pro:src/screens/workloads.jsx:20
  # source: pro:src/app.jsx:270-299
  Scenario: The overview is as fresh as the snapshot
    Given the overview is shown
    Then its rows change only when the cluster snapshot refreshes
    And while auto-refresh is paused or a refresh fails, the last rows stay with no marker on the overview itself

  # ---- jumping into a kind ---------------------------------------------------

  @oss @pro
  # source: oss:src/screens/workloads.jsx:11,74-75
  # source: oss:src/app.jsx:485,375-382
  # source: pro:src/screens/workloads.jsx:11,75-76
  # source: pro:src/app.jsx:510,345-354
  Scenario Outline: Clicking a workload opens it in its kind's list
    Given the overview shows <kind> "<name>" in namespace "<ns>"
    When the user clicks that row
    Then the "<route>" list opens with ?name=<ns>/<name>
    And the selected namespace is carried along
    And that list opens the workload's detail panel once loaded

    Examples:
      | kind        | name   | ns       | route        |
      | Deployment  | api    | payments | deployments  |
      | StatefulSet | db     | payments | statefulsets |
      | DaemonSet   | agent  | system   | daemonsets   |
      | Job         | import | batch    | jobs         |
      | CronJob     | nightly| batch    | cronjobs     |

  @oss @pro
  # source: oss:src/app.jsx:485
  # source: pro:src/app.jsx:510
  Scenario: A workload of an unrecognised kind opens the Deployments list
    Given the snapshot holds a workload whose kind is none of the five
    When the user clicks its row
    Then the Deployments list opens with ?name=<ns>/<name>

  @oss @pro
  # source: oss:src/screens/workloads.jsx:74-75
  # source: pro:src/screens/workloads.jsx:75-76
  # assumption: overview rows have no keyboard focus or key handling; inferred from the absence of either
  Scenario: Overview rows respond to the pointer only
    When the user tries to reach an overview row with the keyboard
    Then the row cannot be focused or opened without a pointer

  @oss @pro
  # source: oss:src/screens/workloads.jsx:19-101
  # source: pro:src/screens/workloads.jsx:19-102
  Scenario: The overview itself offers no write action
    Then the overview has no create, edit, scale, restart or delete control
    And every such action is reached through the kind's own list
