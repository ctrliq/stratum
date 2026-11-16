Feature: What the data layer caches and persists
  Everything the data layer keeps between requests, and where. Cluster data
  itself is never written to browser storage; only a few small markers are.

  @oss @pro
  # source: oss:src/data.js:13-41
  # source: pro:src/data.js:10-61
  # assumption: no code in either root writes cluster objects to localStorage, sessionStorage, IndexedDB or a service-worker cache
  Scenario: Cluster data is held in memory only
    Given a load has filled the snapshot
    When the page is reloaded
    Then every list starts empty and is filled again from the cluster
    And nothing about pods, nodes, workloads, events or secrets was written to browser storage

  @oss @pro
  # source: oss:src/data.js:16-39
  # source: pro:src/data.js:16-56
  Scenario: Before any load answers, every collection is empty rather than fabricated
    Given no load has answered yet
    Then the node, pod, workload, event, Helm release and storage class lists are empty
    And the usage totals read zero with no percentages and an empty history
    And the cluster facts are absent

  @oss @pro
  # source: oss:src/live.js:107-108
  # source: pro:src/live.js:312-316
  Scenario: The snapshot survives a failed poll
    Given the snapshot holds data from a successful load
    When the next load fails entirely
    Then the screens keep showing the data from the successful load

  @oss @pro
  # source: oss:src/cluster-context.js:44-75
  # source: pro:src/cluster-context.js:33-59
  Scenario: The server configuration is fetched once and reused
    Given the server configuration was fetched when the page started
    When screens need the proxy prefix, the mode or the feature flags
    Then no further configuration request is made unless a refresh is explicitly forced

  @pro @fleet
  # source: pro:src/cluster-context.js:25,31,68-73
  Scenario: The selected cluster is remembered across page loads
    Given the user selected cluster "edge-01"
    When the page is reloaded
    Then "edge-01" is the remembered selection, read from the browser's local storage key "stratum.currentCluster"

  @pro
  # source: pro:src/cluster-context.js:31,72
  Scenario: A browser that blocks local storage still works, without remembering the cluster
    Given the browser refuses access to local storage
    When the user selects a cluster
    Then the selection works for the open page and no error is raised

  @pro
  # source: pro:src/cluster-context.js:68
  Scenario: With nothing remembered the selection is "local"
    Given no cluster was ever selected in this browser
    Then the current cluster id is "local"

  @pro @fleet
  # source: pro:src/data.js:13-14,58-67
  # source: pro:src/live.js:469-473,848
  Scenario: The list of clusters outlives cluster switches and cluster loads
    Given the hub's cluster list was loaded
    When the user switches cluster, or a cluster load commits
    Then the list of clusters is neither emptied nor overwritten by per-cluster data

  @pro
  # source: pro:src/cluster-context.js:50-53,64-66
  Scenario: A hub configuration once loaded is kept when a later fetch fails
    Given the hub configuration loaded successfully earlier in this page
    When a forced re-fetch of the configuration fails
    Then the earlier configuration stays in force
    And the dashboard does not fall back to treating the hub as absent

  @oss
  # source: oss:src/session.js:40-44,55-66,83-96
  Scenario: Only a non-secret session marker is shared between tabs
    When the user signs in or out in one tab
    Then a marker with a random generation and a "changing" flag is written to local storage under "stratum-dashboard-session-generation"
    And it contains no token, cookie or cluster data

  @oss @pro
  # source: oss:src/app.jsx:299,302
  # source: oss:src/live.js:429-431
  # source: oss:src/data.js:47-50
  # source: pro:src/app.jsx:231,250
  # source: pro:src/live.js:849
  Scenario: The page remembers whether a load ever succeeded and the last Kubernetes version
    Given a load has committed data
    Then the page records that live data is present, so a later re-entry does not blank the lists first
    And the Kubernetes version found when the cluster was first opened is kept for the footer, as a fallback when the cluster's own record states none

  @oss @pro
  # source: oss:src/live.js:338-340
  # source: pro:src/live.js:469-473
  Scenario: The previous snapshot is the fallback for values a load could not refresh
    Given the previous load recorded a value that the current load could not recompute
    Then the previous value is what stays visible
    And it is replaced as soon as a load can recompute it

  @pro
  # source: pro:src/live.js:146-152,166-172,211-222
  Scenario: Console warnings about a missing hub are remembered for the life of the page
    Given the hub facts for a cluster were reported missing once
    When they are missing again on later polls
    Then no further warning is written for that cluster until the page is reloaded
