Feature: Logs and exec drawer
  A resizable panel docked at the bottom of the app with one tab per pod or
  node. Pod tabs switch between logs and an exec terminal; node tabs hold a
  host shell. The drawer survives navigation between screens.

  # ----------------------------------------------------------------- opening

  # source: oss:src/ui/exec-logs-drawer.jsx:216
  # source: oss:src/app.jsx:270-271,509-511
  # source: pro:src/ui/exec-logs-drawer.jsx:137
  # source: pro:src/app.jsx:174-175,532-534
  @oss @pro
  Scenario: The drawer is absent until something is opened in it
    Given no pod or node tab is open
    Then no drawer is shown

  # source: oss:src/resources.jsx:32-36
  # source: oss:src/app.jsx:399-408,434
  # source: pro:src/resources.jsx:48-52
  # source: pro:src/app.jsx:387-396,424
  @oss @pro
  Scenario: The Logs action on a pod opens the drawer on that pod's logs
    When the user presses Logs on pod "web-1" in namespace "shop"
    Then the drawer opens with an active tab named "web-1" with a pod icon
    And the tab shows the log viewer
    And the Logs button is shown as selected

  # source: oss:src/resources.jsx:32-36
  # source: oss:src/app.jsx:399-408,435
  # source: pro:src/resources.jsx:48-52
  # source: pro:src/app.jsx:387-396,425
  @oss @pro
  Scenario: The Exec action on a pod opens the drawer on a terminal in its first container
    Given pod "web-1" has containers "app" and "sidecar"
    When the user presses Exec on pod "web-1"
    Then the drawer opens with an active tab named "web-1" showing an exec terminal
    And the session runs "/bin/sh" in container "app"
    And the drawer offers no way to choose another container or command for exec

  # source: oss:src/app.jsx:402-407
  # source: pro:src/app.jsx:390-395
  @oss @pro
  Scenario: A pod has at most one tab
    Given a tab for pod "shop/web-1" is open on logs
    When the user presses Exec on the same pod
    Then no second tab is added
    And the existing tab becomes active and switches to the exec terminal

  # source: oss:src/app.jsx:402
  # source: pro:src/app.jsx:390
  @oss @pro
  Scenario: Pods with the same name in different namespaces get separate tabs
    When the user opens logs for "web-1" in "shop" and for "web-1" in "test"
    Then two tabs are open, both labelled "web-1"

  # source: oss:src/app.jsx:400-401
  # source: pro:src/app.jsx:388-389
  @oss @pro
  Scenario: An action on something without a namespace and name does nothing
    When a logs action is raised for an object that has no namespace or no name
    Then the drawer does not change

  # source: oss:src/app.jsx:403-406
  # source: pro:src/app.jsx:391-394
  @oss @pro
  Scenario: A tab keeps the container list it was opened with
    Given a tab for pod "web-1" was opened when the pod had one container
    When the pod is later reopened from the list after gaining a second container
    Then the tab still offers only the original container

  # ----------------------------------------------------------- tab behavior

  # source: oss:src/ui/exec-logs-drawer.jsx:217,229-244
  # source: pro:src/ui/exec-logs-drawer.jsx:138,150-165
  @oss @pro
  Scenario: Selecting a tab makes it the visible one
    Given tabs for "web-1" and "db-0" are open
    When the user selects the "db-0" tab
    Then "db-0" is highlighted and its pane is shown

  # source: oss:src/ui/exec-logs-drawer.jsx:258-268
  # source: pro:src/ui/exec-logs-drawer.jsx:179-189
  @oss @pro
  Scenario: Sessions in background tabs keep running
    Given an exec terminal is connected in the "web-1" tab
    When the user selects another tab and later returns to "web-1"
    Then the terminal is still connected and shows its earlier output
    And a log stream in a background tab keeps collecting lines

  # source: oss:src/ui/exec-logs-drawer.jsx:249-253,261
  # source: oss:src/app.jsx:427
  # source: pro:src/ui/exec-logs-drawer.jsx:170-174,182
  # source: pro:src/app.jsx:415
  # decision: D2
  @oss @pro @drop
  Scenario: Switching a pod tab between Logs and Exec ends the session it leaves
    Given an exec terminal is connected in the "web-1" tab
    When the user presses Logs and then Exec again
    Then the earlier exec connection was closed
    And a new exec session starts with an empty terminal
    And the log viewer likewise starts over each time it is returned to

  # source: oss:src/ui/exec-logs-drawer.jsx:1-8,258-268
  # source: pro:src/ui/exec-logs-drawer.jsx:1-8,179-189
  # decision: D2
  @fixed @oss @pro
  Scenario: Switching a pod tab between Logs and Exec keeps both sessions
    Given an exec terminal is connected in the "web-1" tab
    When the user presses Logs and then Exec again
    Then the terminal is still connected and shows its earlier output
    And the log viewer kept its lines while the terminal was shown

  # source: oss:src/ui/exec-logs-drawer.jsx:240-241
  # source: oss:src/app.jsx:409-413
  # source: pro:src/ui/exec-logs-drawer.jsx:161-162
  # source: pro:src/app.jsx:397-401
  @oss @pro
  Scenario: Closing the active tab activates the last remaining tab
    Given tabs "a", "b" and "c" are open in that order and "b" is active
    When the user closes "b"
    Then "c" becomes active
    And closing a tab that is not active leaves the active tab unchanged
    And closing a tab ends its session without selecting it

  # source: oss:src/ui/exec-logs-drawer.jsx:216
  # source: oss:src/app.jsx:409-413
  # source: pro:src/ui/exec-logs-drawer.jsx:137
  # source: pro:src/app.jsx:397-401
  @oss @pro
  Scenario: Closing the last tab removes the drawer
    Given one tab is open
    When the user closes it
    Then the drawer disappears

  # source: oss:src/ui/exec-logs-drawer.jsx:255
  # source: oss:src/app.jsx:511
  # source: pro:src/ui/exec-logs-drawer.jsx:176
  # source: pro:src/app.jsx:534
  @oss @pro
  Scenario: Close panel closes every tab at once
    Given several tabs are open
    When the user presses "Close panel"
    Then every tab is closed, every session ends and the drawer disappears
    And no confirmation is asked

  # source: oss:src/ui/exec-logs-drawer.jsx:217
  # source: pro:src/ui/exec-logs-drawer.jsx:138
  @oss @pro
  Scenario: If the active tab is no longer shown the first tab is shown instead
    Given the active tab has been hidden from the user
    Then the first remaining tab is shown as active

  # ------------------------------------------------------------------ layout

  # source: oss:src/ui/exec-logs-drawer.jsx:204-211,216,221-225
  # source: oss:src/app.jsx:509-511
  # source: pro:src/ui/exec-logs-drawer.jsx:125-132,137,142-146
  # source: pro:src/app.jsx:532-534
  @oss @pro
  Scenario: The drawer can be resized by dragging its top edge
    Given the drawer is open at its initial height of 300 pixels
    When the user drags the handle on its top edge
    Then the drawer grows or shrinks with the pointer
    And its height stays between 140 and 700 pixels
    And the height is kept when the drawer is closed and reopened
    And it returns to 300 pixels after a page reload or a new sign-in

  # source: oss:src/app.jsx:254-255,502-512
  # source: pro:src/app.jsx:155-156,525-535
  @oss @pro
  Scenario: The drawer and its sessions survive navigation
    Given a pod exec tab is connected
    When the user navigates to another screen of the same cluster
    Then the drawer stays open and the session stays connected

  # ------------------------------------------------------------- permissions

  # source: oss:src/ui/exec-logs-drawer.jsx:203,249
  # source: oss:src/ui/resource-list.jsx:83-93
  # source: oss:src/session.js:144-156
  # decision: D14
  @diverged @oss
  Scenario: Exec is offered to any session that can write
    Given a signed-in user, or a server that requires no sign-in
    Then pod rows offer both Logs and Exec
    And a pod tab offers both the Logs and the Exec button
    And a session with no signed-in identity on a server that requires sign-in is offered Logs only

  # source: pro:src/ui/exec-logs-drawer.jsx:124,170
  # source: pro:src/ui/resource-list.jsx:64-74
  # source: pro:src/fleet.js:48-55
  # decision: D14
  @diverged @pro
  Scenario: Exec is offered only to admins
    Given a user with the "admin" role, or a standalone server that requires no sign-in
    Then pod rows offer both Logs and Exec
    And a pod tab offers both the Logs and the Exec button
    And a signed-in user without the "admin" role is offered Logs only

  # source: oss:src/ui/exec-logs-drawer.jsx:212-215,261-266
  # source: pro:src/ui/exec-logs-drawer.jsx:133-136,182-187
  @oss @pro
  Scenario: An open exec tab falls back to logs when write permission is lost
    Given an exec terminal is connected in a pod tab
    When the user's session stops being allowed to write
    Then the exec connection is closed and the tab shows the pod's logs
    And only the Logs button is offered

  # source: oss:src/ui/exec-logs-drawer.jsx:214-216
  # source: pro:src/ui/exec-logs-drawer.jsx:135-137
  @oss @pro
  Scenario: A drawer holding only host shells disappears when write permission is lost
    Given only node host shell tabs are open
    When the user's session stops being allowed to write
    Then the drawer disappears

  # source: oss:src/ui/exec-logs-drawer.jsx:214-215
  # source: oss:src/app.jsx:270
  # source: pro:src/ui/exec-logs-drawer.jsx:135-136
  # source: pro:src/app.jsx:174
  @oss @pro
  Scenario: Regaining write permission restores the hidden tabs with new sessions
    Given a host shell tab and a pod exec tab were open when write permission was lost
    When the session is allowed to write again without the page being reloaded
    Then the host shell tab reappears and starts a new helper
    And the pod tab returns to exec with a new session

  # source: oss:src/app.jsx:183-188,209-210,222-228
  # source: pro:src/app.jsx:112-117,126-130,137-144
  @oss @pro
  Scenario: Signing out or losing the session closes the drawer
    Given the drawer has tabs open
    When the user signs out, or is sent to sign-in because the session ended
    Then every tab and session is closed
    And the drawer is empty after signing in again

  # ------------------------------------------------- cluster switch (pro)

  # source: pro:src/app.jsx:416-417
  @pro @fleet
  Scenario: Switching cluster closes the drawer and any shell dialog
    Given the drawer has tabs open and a shell dialog is open
    When the user switches to another cluster
    Then every tab is closed, the drawer disappears and the shell dialog closes
