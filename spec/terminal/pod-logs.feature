Feature: Pod log viewer
  A streaming view of one container's logs with follow, previous-container,
  timestamps, a text filter and auto-scroll.

  # ---------------------------------------------------------------- request

  # source: oss:src/ui/exec-logs-drawer.jsx:71-75,82-85,135-137
  # source: pro:src/ui/exec-logs-drawer.jsx:29-31,51-59
  # source: pro:src/k8sproxy.js:91-93
  @oss @pro
  Scenario: Opening the log viewer follows the last 1000 lines of the first container
    Given a pod "web-1" in namespace "shop" with containers "app" and "sidecar"
    When its log viewer is opened
    Then logs are requested from the cluster path "/api/v1/namespaces/shop/pods/web-1/log"
    And the query carries "timestamps=false", "tailLines=1000", "container=app" and "follow=true"
    And the query carries no "previous" parameter
    And Follow and Auto-scroll are ticked, Timestamps and Previous are not

  # source: oss:src/ui/exec-logs-drawer.jsx:71
  # source: pro:src/ui/exec-logs-drawer.jsx:51
  @oss @pro
  Scenario: The tail size is fixed
    When the log viewer is open
    Then no control changes how many lines are requested
    And every request asks for the last 1000 lines

  # source: oss:src/ui/exec-logs-drawer.jsx:130-131,72
  # source: pro:src/ui/exec-logs-drawer.jsx:24-25,52
  @oss @pro
  Scenario: A pod whose containers are unknown lets the cluster choose
    Given a pod for which no container names are known
    When its log viewer is opened
    Then the request carries no "container" parameter

  # ------------------------------------------------------------- containers

  # source: oss:src/ui/exec-logs-drawer.jsx:172-176
  # source: pro:src/ui/exec-logs-drawer.jsx:93-97
  @oss @pro
  Scenario: A container selector appears only for multi-container pods
    Given a pod with containers "app" and "sidecar"
    When its log viewer is opened
    Then a selector offers "app" and "sidecar" with "app" selected
    And a pod with a single container shows no selector

  # source: oss:src/app.jsx:403
  # source: pro:src/app.jsx:391
  @oss @pro
  Scenario: Init and ephemeral containers are not offered
    Given a pod with an init container "migrate" and a container "app"
    When its log viewer is opened
    Then "migrate" cannot be selected

  # source: oss:src/ui/exec-logs-drawer.jsx:143-157,173
  # source: pro:src/ui/exec-logs-drawer.jsx:37-78,94
  @oss @pro
  Scenario: Selecting another container restarts the stream
    Given the log viewer shows lines from container "app"
    When the user selects "sidecar"
    Then the shown lines are cleared
    And a new request is made with "container=sidecar"

  # ---------------------------------------------------------------- options

  # source: oss:src/ui/exec-logs-drawer.jsx:71-74,143-157,181-183
  # source: pro:src/ui/exec-logs-drawer.jsx:51-54,37-78,102-104
  @oss @pro
  Scenario Outline: Changing a stream option restarts the stream with it
    Given the log viewer is streaming
    When the user <action>
    Then the shown lines are cleared and the running request is cancelled
    And a new request is made with <query>

    Examples:
      | action            | query                       |
      | ticks Timestamps  | "timestamps=true"           |
      | ticks Previous    | "previous=true"             |
      | unticks Follow    | no "follow" parameter       |

  # source: oss:src/ui/exec-logs-drawer.jsx:115-121,154
  # source: pro:src/ui/exec-logs-drawer.jsx:68-75
  @oss @pro
  Scenario: Without Follow the viewer loads the tail once and stops
    Given Follow is not ticked
    When the logs have been read to the end
    Then the viewer stops streaming and offers "Stream" instead of "Stop"

  # source: oss:src/ui/exec-logs-drawer.jsx:74,95
  # source: pro:src/ui/exec-logs-drawer.jsx:54,64
  @oss @pro
  Scenario: Previous logs of a container that never restarted show the cluster's refusal
    Given Previous is ticked for a container with no previous instance
    When the cluster answers the request with an error
    Then an "[error]" line is shown and streaming stops

  # ------------------------------------------------------------- stop/start

  # source: oss:src/ui/exec-logs-drawer.jsx:161,187-189
  # source: pro:src/ui/exec-logs-drawer.jsx:82,108-110
  @oss @pro
  Scenario: Stop ends the stream and keeps what was read
    Given the log viewer is streaming
    When the user presses Stop
    Then the request is cancelled and no error line is added
    And the lines already shown stay
    And the button becomes "Stream"

  # source: oss:src/ui/exec-logs-drawer.jsx:147,189
  # source: pro:src/ui/exec-logs-drawer.jsx:49,110
  @oss @pro
  Scenario: Stream starts over from the tail
    Given a stopped log viewer showing earlier lines
    When the user presses Stream
    Then the shown lines are cleared and a new request is made with the current options

  # source: oss:src/ui/exec-logs-drawer.jsx:115,154
  # source: pro:src/ui/exec-logs-drawer.jsx:68,75
  @oss @pro
  Scenario: A followed stream that the cluster ends is not resumed
    Given a followed stream
    When the cluster ends the response, for example because the container exited
    Then the viewer stops streaming and does not reconnect on its own

  # ---------------------------------------------------------------- display

  # source: oss:src/ui/exec-logs-drawer.jsx:116-121,152
  # source: pro:src/ui/exec-logs-drawer.jsx:69-73
  @oss @pro
  Scenario: Lines appear as they arrive and the buffer keeps the newest 5000
    Given a followed stream
    When more than 5000 lines have arrived
    Then only the newest 5000 are kept
    And a final line without a trailing newline is shown when the stream ends

  # source: oss:src/ui/exec-logs-drawer.jsx:162,178-179,186
  # source: pro:src/ui/exec-logs-drawer.jsx:83,99-100,107
  @oss @pro
  Scenario: The filter keeps lines containing the text, ignoring case
    Given lines "GET /health 200" and "ERROR timeout" are shown
    When the user types "error" in the filter
    Then only "ERROR timeout" is shown and the counter reads "1 lines"
    And clearing the filter shows both lines again without a new request

  # source: oss:src/ui/exec-logs-drawer.jsx:186
  # source: pro:src/ui/exec-logs-drawer.jsx:107
  @oss @pro
  Scenario: The counter shows the number of lines currently displayed
    Given 250 lines are held and no filter is set
    Then the counter reads "250 lines"

  # source: oss:src/ui/exec-logs-drawer.jsx:159,184
  # source: pro:src/ui/exec-logs-drawer.jsx:80,105
  @oss @pro
  Scenario: Auto-scroll keeps the newest line in view until it is unticked
    Given Auto-scroll is ticked
    When new lines arrive
    Then the view scrolls to the bottom
    And with Auto-scroll unticked the view stays where the user left it
    And scrolling up by hand does not untick Auto-scroll

  # source: oss:src/ui/exec-logs-drawer.jsx:191-193
  # source: pro:src/ui/exec-logs-drawer.jsx:112-114
  @oss @pro
  Scenario: Long lines always wrap
    When a line is wider than the viewer
    Then it wraps, breaking inside words if needed
    And there is no control to turn wrapping off

  # source: oss:src/ui/exec-logs-drawer.jsx:194-195
  # source: pro:src/ui/exec-logs-drawer.jsx:115-116
  @oss @pro
  Scenario: A stopped viewer with nothing to show says so
    Given the stream ended, or was stopped, without any line, or the filter matches nothing
    Then the viewer shows "No log output."
    And while streaming with no lines yet the viewer is simply empty

  # source: oss:src/ui/exec-logs-drawer.jsx:169-198
  # source: pro:src/ui/exec-logs-drawer.jsx:90-119
  @oss @pro
  Scenario: Logs cannot be downloaded or copied with a dedicated control
    When the log viewer is open
    Then no download button and no copy button is offered
    And terminal colour codes in log lines are shown as raw text

  # ----------------------------------------------------------------- errors

  # source: oss:src/ui/exec-logs-drawer.jsx:95
  # decision: D1
  @diverged @oss @drop
  Scenario: A refused log request shows the HTTP status
    When the cluster answers the log request with status 403
    Then the viewer shows the single line "[error] HTTP 403" and stops streaming
    And the cluster's explanation is not shown

  # source: pro:src/ui/exec-logs-drawer.jsx:64
  # source: pro:src/k8sproxy.js:94-99
  # decision: D1
  @diverged @pro
  Scenario: A refused log request shows the cluster's own answer
    When the cluster answers the log request with status 403 and a response body
    Then the viewer shows the single line "[error] " followed by the response body, and stops streaming
    And a refusal with an empty body shows "[error] HTTP 403"

  # source: oss:src/ui/exec-logs-drawer.jsx:88-91
  # source: pro:src/ui/exec-logs-drawer.jsx:64
  # source: pro:src/k8sproxy.js:101
  @oss @pro
  Scenario: A request that cannot be sent shows the network error
    When the log request fails before any answer, for example because the network is down
    Then the viewer shows "[error] " followed by the browser's error message and stops streaming

  # source: oss:src/ui/exec-logs-drawer.jsx:122-124
  # source: pro:src/ui/exec-logs-drawer.jsx:74
  @oss @pro
  Scenario: A stream that breaks midway appends an error after the lines read so far
    Given a stream that has delivered lines
    When reading the response fails
    Then "[error] " followed by the failure message is appended below the existing lines
    And streaming stops

  # source: oss:src/ui/exec-logs-drawer.jsx:94-95
  # source: oss:src/app.jsx:182-189
  # decision: D1, D7
  @diverged @oss @drop
  Scenario: An unauthenticated log request routes to sign-in
    Given a server that requires sign-in
    When the log request is answered with status 401
    Then the user is taken to the sign-in screen, which remembers where they were
    And the viewer also appends the line "[error] HTTP 401"

  # source: pro:src/k8sproxy.js:14-21,94-99
  # source: pro:src/ui/exec-logs-drawer.jsx:64
  # decision: D1, D7
  @diverged @pro
  Scenario: A 401 on a log request routes to sign-in only if the hub session really expired
    When the log request is answered with status 401
    Then the hub is asked whether the user's own session has expired
    And if it has, the user is taken to the sign-in screen
    And in both cases the 401 is shown as an "[error]" line like any other refusal

  # source: oss:src/ui/exec-logs-drawer.jsx:28,78-92
  # decision: D1, D8
  @diverged @oss
  Scenario: A log request that gets no answer is given up after 15 seconds
    When the cluster accepts the log request but sends no response for 15 seconds
    Then the request is cancelled
    And the viewer shows "[error] the request passed its deadline" and stops streaming

  # source: pro:src/ui/exec-logs-drawer.jsx:56-59
  # source: pro:src/k8sproxy.js:91-93
  # decision: D1, D8
  @diverged @pro @drop
  Scenario: A log request that gets no answer waits until the user stops it
    When the cluster accepts the log request but sends no response
    Then the viewer keeps showing the streaming state with no lines
    And it ends only when the user presses Stop, changes an option or closes the viewer

  # source: oss:src/ui/exec-logs-drawer.jsx:100-112
  # decision: D8
  @oss
  Scenario: Only non-followed reads are bounded once the response has started
    Given the cluster has started answering
    When Follow is ticked and the pod writes nothing for a long time
    Then the stream stays open with no error
    And when Follow is not ticked and no data arrives for 15 seconds the viewer shows "[error] the request passed its deadline"

  # source: oss:src/ui/exec-logs-drawer.jsx:69-70,93,113
  @oss
  Scenario: A stream opened under an earlier sign-in ends silently when the sign-in changes
    Given a log stream is open
    When the sign-in session ends or is replaced by another
    Then the stream is cancelled without an error line
    And no log request is started for a sign-in that has already ended

  # ----------------------------------------------------------- permissions

  # source: oss:src/ui/exec-logs-drawer.jsx:214-215,249
  # source: oss:src/resources.jsx:32-36
  # source: pro:src/ui/exec-logs-drawer.jsx:135-136,170
  # source: pro:src/resources.jsx:48-52
  @oss @pro
  Scenario: Logs are available to users who cannot write
    Given a user who is not allowed to write
    When they open a pod's logs
    Then the log viewer works as for any other user

  # ------------------------------------------------ cluster pinning (pro)

  # source: pro:src/ui/exec-logs-drawer.jsx:16-22,40-47
  # source: pro:src/app.jsx:417
  @pro @fleet
  Scenario: A log stream stays on the cluster it was opened on and ends on a cluster switch
    Given a log stream opened while cluster A was selected
    When the user switches to cluster B
    Then the stream is never redirected to cluster B
    And the log viewer is closed, which cancels the stream
