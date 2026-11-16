Feature: Open shell in pod dialog
  A dialog, raised from the command palette, that lets the user pick a
  running pod and a command and opens an exec terminal inside the dialog.

  # source: oss:src/ui/palette.jsx:28,42
  # source: oss:src/app.jsx:435,517
  # source: pro:src/ui/palette.jsx:42,56
  # source: pro:src/app.jsx:425,542
  @oss @pro
  Scenario: The command palette offers "Open shell in pod…" to users who can write
    Given a user allowed to write
    When the user runs "Open shell in pod…" from the command palette
    Then a dialog titled "Open shell in pod" opens with the subtitle "kubectl exec · WebSocket → containerd"
    And the command is not listed in the palette for a user who cannot write

  # source: oss:src/screens/terminal.jsx:153-155,162-165,220-222
  # source: pro:src/screens/terminal.jsx:118-120,127-130,230-232
  @oss @pro
  Scenario: The picker lists running pods from every namespace
    Given the loaded pods include running, pending and completed pods in several namespaces
    When the dialog opens
    Then the pod selector lists only pods whose status is "Running", as "<namespace> / <name>"
    And the selected namespace of the app does not narrow the list
    And the first running pod is preselected

  # source: oss:src/screens/terminal.jsx:163,167
  # source: pro:src/screens/terminal.jsx:128,132
  @oss @pro
  Scenario: With no running pods nothing can be connected
    Given no loaded pod is running
    When the dialog opens
    Then the selector shows "no running pods" and Connect is disabled

  # source: oss:src/screens/terminal.jsx:171
  # source: pro:src/screens/terminal.jsx:136
  @oss @pro
  Scenario: Before connecting the dialog explains what to do
    When the dialog opens
    Then it shows "Select a pod and connect to open a live kubectl exec shell." and no terminal

  # source: oss:src/screens/terminal.jsx:156,158,166-170
  # source: pro:src/screens/terminal.jsx:121,123,131-135
  @oss @pro
  Scenario: Connect opens an exec session with the chosen command
    Given pod "shop / web-1" is selected and the command field reads "/bin/sh"
    When the user presses Connect
    Then an exec terminal for "shop/web-1" opens inside the dialog running "/bin/sh"
    And no container is named, so the cluster chooses the container

  # source: oss:src/screens/terminal.jsx:156,166
  # source: pro:src/screens/terminal.jsx:121,131
  @oss @pro
  Scenario: The command can be edited before connecting
    Given the user replaced the command with "bash -l"
    When the user presses Connect
    Then the session runs "bash" with the argument "-l"
    And editing the command while a session is open does not affect that session

  # source: oss:src/screens/terminal.jsx:158,170
  # source: pro:src/screens/terminal.jsx:123,135
  @oss @pro
  Scenario: Connect again starts a fresh session
    Given a session in the dialog has closed or is still open
    When the user presses Connect again
    Then the previous connection is closed and a new session opens with an empty terminal

  # source: oss:src/screens/terminal.jsx:162
  # source: pro:src/screens/terminal.jsx:127
  @oss @pro
  Scenario: Choosing another pod ends the current session
    Given a session to "shop / web-1" is open in the dialog
    When the user selects "shop / web-2"
    Then the session is closed and the explanatory text returns until Connect is pressed

  # source: oss:src/screens/terminal.jsx:224-228
  # source: oss:src/app.jsx:517
  # source: pro:src/screens/terminal.jsx:234-238
  # source: pro:src/app.jsx:542
  @oss @pro
  Scenario: Closing the dialog ends its session
    Given a session is open in the dialog
    When the user closes the dialog
    Then the exec connection is closed

  # source: oss:src/screens/terminal.jsx:155
  # source: pro:src/screens/terminal.jsx:120
  @oss @pro
  Scenario: The preselected pod is fixed when the dialog opens
    Given the dialog opened while no pod was running
    When a pod starts running and appears in the selector
    Then no pod is selected and Connect stays disabled until the user picks one

  # ------------------------------------------- extension-seam paths (dropped)

  # source: oss:src/app.jsx:435,489-490
  # source: oss:src/screens/terminal.jsx:216-227
  # decision: D19
  # assumption: nothing in the open dashboard raises this shape itself; only a consumer-registered screen can. The same code exists in pro, where nothing can reach it (D3).
  @oss @drop
  Scenario: An exec action naming a pod opens the shell dialog directly on that pod
    Given a registered extension screen raises an exec action naming namespace "shop", pod "web-1" and container "app"
    Then a dialog titled "Shell · web-1" opens straight on an exec terminal in container "app"
    And no pod picker is shown

  # source: oss:src/app.jsx:435,489-490
  # source: oss:src/screens/terminal.jsx:220-222
  # decision: D19
  # assumption: reachable only through a consumer-registered screen, as above
  @oss @drop
  Scenario: An exec action naming a workload scopes the pod picker to its pods
    Given a registered extension screen raises an exec action carrying the name "web"
    Then the dialog's pod picker lists only running pods whose name starts with "web"
    And it lists every running pod when no pod name starts with "web"
