Feature: Node host shell
  A root shell on a node's host, reached by starting a privileged helper pod
  pinned to the node and entering the host's namespaces from it. The two
  dashboards differ in how the helper is created, bounded and cleaned up.

  # ------------------------------------------------------------ availability

  # source: oss:src/screens/infra.jsx:74,130-135,203,216-219
  # source: oss:src/cluster-context.js:83-88
  # source: oss:src/session.js:144-156
  # decision: D1, D16
  @diverged @oss
  Scenario: The host shell is offered only when the server enables it, to a session that can write, on a ready node
    Given the server configuration reports the node shell feature as enabled
    And a signed-in user, or a server that requires no sign-in
    When the Nodes screen is shown
    Then every ready node offers "Open host shell" in its row and "Host shell" in its detail pane
    And a node that is not ready offers neither
    And nothing is offered when the feature is disabled or absent from the server configuration
    And nothing is offered to a session that cannot write

  # source: pro:src/screens/infra.jsx:379-384,522-525
  # source: pro:src/fleet.js:48-55
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: The host shell is offered to every admin on any node that is not NotReady
    Given a user with the "admin" role, or a standalone server that requires no sign-in
    When the Nodes screen is shown
    Then every node whose status is "Ready" or "Unknown" offers "Open host shell" in its row and "Host shell" in its detail pane
    And a node whose status is "NotReady" offers neither
    And no server setting can switch the host shell off
    And nothing is offered to a user without the "admin" role

  # source: oss:src/cluster-context.js:83-88
  @oss
  Scenario: The host shell is off when the server configuration cannot be read
    Given the server configuration could not be fetched
    When the Nodes screen is shown
    Then no host shell control is offered

  # source: oss:src/app.jsx:419-426,436
  # source: oss:src/ui/exec-logs-drawer.jsx:238-239,247-248,262-263
  # source: pro:src/app.jsx:407-414,426
  # source: pro:src/ui/exec-logs-drawer.jsx:159-160,168-169,183-184
  @oss @pro
  Scenario: A host shell opens as a tab in the bottom drawer
    When the user opens the host shell of node "worker-1"
    Then a tab named "worker-1" with a server icon is added to the bottom drawer and made active
    And the drawer shows "host shell · nsenter" in place of the Logs and Exec buttons

  # source: oss:src/app.jsx:421-425
  # source: pro:src/app.jsx:409-413
  @oss @pro
  Scenario: Opening the host shell of a node that already has a tab reuses it
    Given a host shell tab for "worker-1" is open
    When the user opens the host shell of "worker-1" again
    Then the existing tab is made active and no second helper is started

  # ---------------------------------------------------------- what is created

  # source: oss:src/node-shell.js:21-32,113-149
  # decision: D1, D16
  @diverged @oss
  Scenario: Opening a host shell creates a one-shot Job in the default namespace
    When the host shell of node "worker-1" is opened
    Then a Job is created in namespace "default" with a generated name starting "stratum-node-shell-"
    And the Job and its pod template are labelled "app=stratum-node-shell" and "stratum.ciq.com/node-shell=worker-1"
    And the pod template is pinned to node "worker-1", shares the host's process namespace, never restarts and tolerates every taint
    And its single container "shell" is privileged, has a terminal and standard input, and runs "sleep 3600"
    And the Job allows no retries

  # source: pro:src/screens/terminal.jsx:142,161-175
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: Opening a host shell creates a bare pod in the default namespace
    When the host shell of node "worker-1" is opened
    Then a Pod is created in namespace "default" with a generated name starting "stratum-node-shell-"
    And the pod is labelled "app=stratum-node-shell" and "stratum.ciq.com/node-shell=worker-1"
    And the pod is pinned to node "worker-1", shares the host's process namespace, never restarts and tolerates every taint
    And its single container "shell" is privileged, has a terminal and standard input, and runs "sleep 3600"

  # source: oss:src/node-shell.js:73-79,142
  # source: oss:src/cluster-context.js:90-95
  # source: oss:src/screens/terminal.jsx:183
  # decision: D1, D16
  @diverged @oss
  Scenario: The helper image is the one the server names
    Given the server configuration names a node shell image
    When a host shell is opened
    Then the helper container runs exactly that image

  # source: pro:src/screens/terminal.jsx:168
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: The helper image is a fixed public image
    When a host shell is opened
    Then the helper container runs the image "alpine:3.20"

  # source: oss:src/node-shell.js:82-90
  # source: oss:src/screens/terminal.jsx:189-196
  @oss
  Scenario: Without a server-provided image nothing is created
    Given the node shell feature is enabled but the server configuration names no image
    When a host shell is opened
    Then nothing is created in the cluster
    And the tab shows "Host shell failed: the server did not provide a node-shell image (features.nodeShellImage); this build cannot open a host shell"

  # source: oss:src/node-shell.js:35-46,117-139
  # decision: D1, D16
  @diverged @oss
  Scenario: The helper is bounded by the cluster even if the dashboard disappears
    When a host shell is opened
    Then the Job and its pod each have an active deadline of 3600 seconds
    And the finished Job is removed by the cluster 600 seconds after it ends, together with its pod

  # source: pro:src/screens/terminal.jsx:161-172
  # assumption: "remains as a completed pod" is Kubernetes' handling of a finished pod that never restarts, not dashboard code; leaving a helper for an hour and listing pods would confirm it
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: The helper has no cluster-side bound beyond its one-hour sleep
    When a host shell is opened
    Then the pod carries no deadline and no automatic removal
    And a pod that is never deleted by the dashboard stops after 3600 seconds and remains in the cluster as a completed pod

  # ------------------------------------------------------------------ phases

  # source: oss:src/screens/terminal.jsx:179,198-207
  # source: pro:src/screens/terminal.jsx:144,208-217
  @oss @pro
  Scenario: While the helper is being created the tab shows a placeholder
    When a host shell for node "worker-1" is starting
    Then the header chip reads "creating" and the label reads "node worker-1"
    And the pane shows "nsenter → host namespaces on worker-1…"

  # source: oss:src/node-shell.js:161-180
  # source: oss:src/screens/terminal.jsx:184,201
  # decision: D1, D16
  @diverged @oss
  Scenario: The helper pod is found by the Job's selector and then awaited
    Given the Job was created
    When the dashboard polls every 1.5 seconds
    Then it lists pods in "default" matching the Job's selector labels, or "job-name=<job>" when the Job states none
    And once a pod exists the header chip reads "waiting" and the label gains the pod's name

  # source: pro:src/screens/terminal.jsx:177-184,211
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: The helper pod is known from the create answer and then awaited
    Given the pod was created
    Then the header chip reads "waiting" at once and the label gains the pod's name
    And the dashboard reads that pod by name every 1.5 seconds

  # source: oss:src/node-shell.js:181-185
  # source: oss:src/screens/terminal.jsx:185,208
  # source: pro:src/screens/terminal.jsx:186-187,218
  @oss @pro
  Scenario: When the helper pod is running the shell enters the host
    When the helper pod reports the phase "Running"
    Then an exec session is opened to that pod in namespace "default", container "shell"
    And the command is "nsenter -t 1 -m -u -i -n -p -- sh"
    And the session behaves as any pod exec session

  # source: oss:src/node-shell.js:174-183
  # source: pro:src/screens/terminal.jsx:184-185
  @oss @pro
  Scenario: A failed status read during the wait is retried
    Given the helper pod is awaited
    When one read of its status fails
    Then the dashboard keeps waiting and tries again 1.5 seconds later

  # ---------------------------------------------------------------- failures

  # source: oss:src/node-shell.js:156
  # source: oss:src/screens/terminal.jsx:189-193
  # decision: D1, D16
  @diverged @oss
  Scenario: A refused create names the Job and the server's answer
    When the cluster or the server refuses the create with status 403
    Then the tab shows "Host shell failed: create debug job → HTTP 403 " followed by at most 180 characters of the error

  # source: pro:src/screens/terminal.jsx:175-176,199-203
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: A refused create names the pod and the cluster's answer
    When the cluster refuses the create with status 403
    Then the tab shows "Host shell failed: create debug pod → HTTP 403 " followed by at most 180 characters of the error

  # source: oss:src/node-shell.js:186
  # source: pro:src/screens/terminal.jsx:188
  @oss @pro
  Scenario Outline: A helper pod that ends before running is a failure
    When the helper pod reports the phase "<phase>" before it was ever seen running
    Then the tab shows "Host shell failed: debug pod <phase>"

    Examples:
      | phase     |
      | Failed    |
      | Succeeded |

  # source: oss:src/node-shell.js:33,170,188
  # decision: D1, D16
  @diverged @oss
  Scenario: A helper pod that does not start within two minutes times out
    When 80 polls 1.5 seconds apart pass without the helper pod running
    Then the tab shows "Host shell failed: timed out waiting for the debug pod to start (slow image pull on the node?)"

  # source: pro:src/screens/terminal.jsx:182-190
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: A helper pod that does not start within two minutes times out with a platform hint
    When 80 polls 1.5 seconds apart pass without the helper pod running
    Then the tab shows "Host shell failed: timed out waiting for the debug pod to start (slow image pull on substrate?)"

  # source: oss:src/screens/terminal.jsx:189-196
  # decision: D1, D16
  @diverged @oss
  Scenario: The failure panel lists what the host shell needs
    When a host shell has failed
    Then the header chip reads "error" and the label reads "node-shell · " followed by the node name
    And the panel explains that it needs permission to create and delete jobs in namespace "default", to get and list pods, to exec into pods, privileged host-PID pods, and the server started with "--enable-node-shell"

  # source: pro:src/screens/terminal.jsx:199-206
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: The failure panel names the dashboard's service account
    When a host shell has failed
    Then the header chip reads "error" and the label reads "node-shell · " followed by the node name
    And the panel explains that it needs "pods create/delete" on the dashboard service account and privileged host-PID pods, which a hardened profile may restrict

  # source: oss:src/node-shell.js:189-196
  # decision: D1, D16
  @diverged @oss
  Scenario: A failed start removes the helper at once
    Given the Job was created
    When the start then fails or times out
    Then the Job is deleted immediately, while the failure panel is still open

  # source: pro:src/screens/terminal.jsx:191-196
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: A failed start leaves the helper until the tab is closed
    Given the pod was created
    When the start then fails or times out
    Then the pod stays in the cluster for as long as the failure panel stays open
    And it is deleted when the tab is closed

  # ----------------------------------------------------------------- cleanup

  # source: oss:src/node-shell.js:30,103-108,198-201
  # source: oss:src/app.jsx:409-413
  # decision: D1, D16
  @diverged @oss
  Scenario: Closing the tab deletes the Job and its pod
    Given a host shell is open
    When the user closes its tab or closes the whole drawer
    Then the Job is deleted with a one-second grace period and background cascading, so its pod is removed with it

  # source: pro:src/screens/terminal.jsx:156-160,193-196
  # source: pro:src/app.jsx:397-401
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: Closing the tab deletes the pod
    Given a host shell is open
    When the user closes its tab or closes the whole drawer
    Then the pod is deleted with a one-second grace period

  # source: oss:src/node-shell.js:91-102,160
  # source: pro:src/screens/terminal.jsx:178-180
  @oss @pro
  Scenario: Closing the tab while the create is still in flight still cleans up
    Given the user opened a host shell and closed its tab before the create answered
    When the create then succeeds
    Then the helper that was just created is deleted
    And no status callbacks or errors are shown for the closed tab

  # source: oss:src/node-shell.js:97-108,112
  # decision: D1, D16
  @diverged @oss
  Scenario: A delete that fails is attempted again by the next cleanup
    Given closing the tab could not delete the Job because of a transient failure
    When another cleanup of the same shell runs, such as sign-out or the page closing
    Then the delete is sent again
    And a delete answered "not found" counts as done

  # source: pro:src/screens/terminal.jsx:156-160
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: A delete that fails is only logged
    Given closing the tab could not delete the pod
    Then a warning naming the pod, the cluster and the HTTP status is written to the browser console
    And no retry is made and nothing is shown to the user
    And a delete answered "not found" is not logged

  # source: oss:src/node-shell.js:109-111,204-225
  # decision: D1, D16
  @diverged @oss
  Scenario: Closing or leaving the page asks the cluster to delete every open helper
    Given host shells are open
    When the browser tab is closed, reloaded or navigated away
    Then a delete is sent for each open Job in a way that may outlive the page
    And delivery is best effort

  # source: pro:src/screens/terminal.jsx:193-196
  # assumption: pro registers no page-unload handler anywhere for node shells; none exists in terminal.jsx, exec-logs-drawer.jsx or app.jsx
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: Closing or leaving the page leaves open helpers behind
    Given host shells are open
    When the browser tab is closed, reloaded or navigated away
    Then no delete is sent and the privileged pods keep running until their sleep ends

  # source: oss:src/app.jsx:48-110,204-211
  # source: oss:src/node-shell.js:48-63
  # decision: D1, D16
  @diverged @oss
  Scenario: Signing out deletes open helpers before the session is cleared
    Given host shells are open
    When the user signs out
    Then a delete is sent for each open Job, each given at most 3 seconds
    And creates still in flight are awaited for at most 3 seconds first
    And only then is the sign-in session ended
    And a delete that fails is reported in the browser console naming the node and Job

  # source: pro:src/app.jsx:126-130
  # source: pro:src/fleet.js:146-151
  # assumption: that the hub then refuses the late delete is server behavior; signing out with a host shell open and listing pods in "default" afterwards would confirm the pod is left behind
  # decision: D1, D16
  @diverged @pro @drop
  Scenario: Signing out does not delete open helpers first
    Given host shells are open
    When the user signs out
    Then the sign-in session is ended first
    And the helpers' deletes are sent only afterwards, without a valid session

  # source: oss:src/node-shell.js:56-65,105,155,172-182,195
  # source: oss:src/app.jsx:183-187
  @oss
  Scenario: A helper started under an earlier sign-in is never touched with new credentials
    Given a host shell was opened by one sign-in session
    When that session ends, by expiry or by another user signing in
    Then no delete, status read or callback for that shell is performed under the new session
    And the cluster-side deadline and automatic removal are what clean it up

  # source: oss:src/app.jsx:87-110
  @oss
  Scenario: Sign-out is abandoned if another sign-in took over meanwhile
    Given sign-out is waiting on helper cleanup
    When a different sign-in session becomes current
    Then the new session is left signed in and its shells are left alone

  # ------------------------------------------------ cluster pinning (pro)

  # source: pro:src/screens/terminal.jsx:150-160
  # source: pro:src/app.jsx:211-214,417
  # source: pro:src/k8sclient.js:50-53,82
  # decision: D16
  @pro @fleet @drop
  Scenario: Switching cluster closes host shells and deletes their pods on the cluster that owns them
    Given a host shell is open on a node of cluster A
    When the user switches to cluster B
    Then the host shell tab is closed
    And the helper pod is deleted on cluster A, never on cluster B

  # --------------------------------------------------------- permission loss

  # source: oss:src/ui/exec-logs-drawer.jsx:212-216
  # source: pro:src/ui/exec-logs-drawer.jsx:133-137
  @oss @pro
  Scenario: Losing write permission closes open host shells
    Given a host shell tab is open
    When the user's session stops being allowed to write
    Then the host shell tab disappears from the drawer
    And its session is torn down by the same path as closing the tab
