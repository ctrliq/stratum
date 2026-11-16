Feature: Pod exec session
  An interactive terminal attached to a command running in a pod's
  container, carried over a WebSocket to the cluster's exec endpoint.

  # ------------------------------------------------------------- connecting

  # source: oss:src/screens/terminal.jsx:100-108
  # source: pro:src/screens/terminal.jsx:79-89
  # source: pro:src/k8sclient.js:87-92
  @oss @pro
  Scenario: Opening a session connects to the pod's exec endpoint
    When an exec session is opened for pod "web-1" in namespace "shop", container "app"
    Then a WebSocket is opened to the cluster path "/api/v1/namespaces/shop/pods/web-1/exec"
    And the query carries "container=app", "stdin=true", "stdout=true", "stderr=true" and "tty=true"
    And the WebSocket requests the subprotocol "v4.channel.k8s.io" and no other
    And the terminal prints "connecting to shop/web-1 …" in a dim colour

  # source: oss:src/screens/terminal.jsx:102-107
  # source: oss:src/cluster-context.js:81
  # decision: D1
  @diverged @oss @drop
  Scenario: The socket address is absolute and uses the server's Kubernetes proxy prefix
    Given the page was loaded over HTTPS from "dash.example"
    When an exec session is opened
    Then the socket address starts with "wss://dash.example" followed by the proxy prefix from the server configuration, "/k8s" by default
    And over plain HTTP the address starts with "ws://"

  # source: pro:src/screens/terminal.jsx:84-88
  # source: pro:src/k8sclient.js:54,87-88
  # source: pro:src/cluster-context.js:78-81
  # decision: D1, D11
  @diverged @pro
  Scenario: The socket address is relative to the proxy of the cluster the session was opened on
    Given cluster "edge-7" is selected on a fleet hub
    When an exec session is opened
    Then the socket address is the same-origin path "/api/v1/clusters/edge-7/k8s" followed by the exec path
    And on a single-cluster server the prefix is the one its configuration names, "/k8s" by default

  # source: oss:src/screens/terminal.jsx:103-105,145
  # source: pro:src/screens/terminal.jsx:81-83,110
  @oss @pro
  Scenario: The default command is /bin/sh
    When an exec session is opened without a command
    Then the query carries the single parameter "command=/bin/sh"
    And the session header ends with "· /bin/sh"

  # source: oss:src/screens/terminal.jsx:105
  # source: pro:src/screens/terminal.jsx:83
  @oss @pro
  Scenario: A command is split on spaces into one argument per word
    When an exec session is opened with the command "bash  -c ls"
    Then the query carries "command=bash", "command=-c" and "command=ls" in that order
    And quoting inside the command is not interpreted

  # source: oss:src/screens/terminal.jsx:103
  # source: pro:src/screens/terminal.jsx:81
  @oss @pro
  Scenario: With no container named the cluster chooses the container
    When an exec session is opened without naming a container
    Then the query carries an empty "container=" parameter
    And the session header shows no container name

  # source: oss:src/screens/terminal.jsx:43-49,73-80,145
  # source: pro:src/screens/terminal.jsx:39-45,52-59,110
  @oss @pro
  Scenario Outline: The session header shows the connection state and the target
    Given an exec session for "shop/web-1", container "app", command "/bin/sh"
    When the session is <state>
    Then the header chip reads "<state>" in the <colour> colour
    And the header label reads "shop/web-1 · app · /bin/sh"

    Examples:
      | state         | colour  |
      | connecting    | warning |
      | connected     | success |
      | closed        | gray    |
      | error         | error   |

  # source: oss:src/screens/terminal.jsx:117
  # source: pro:src/screens/terminal.jsx:93
  @oss @pro
  Scenario: A connected session takes keyboard focus and reports the terminal size
    When the WebSocket opens
    Then the state becomes "connected" and the terminal has keyboard focus
    And the terminal's current size is sent to the container

  # ------------------------------------------------------------- data flow

  # source: oss:src/screens/terminal.jsx:115,132
  # source: pro:src/screens/terminal.jsx:91,102
  @oss @pro
  Scenario: Keystrokes are sent on the stdin channel
    Given a connected session
    When the user types "ls" and Enter
    Then each input is sent as a binary frame whose first byte is 0 followed by the UTF-8 text

  # source: oss:src/screens/terminal.jsx:118-122
  # source: pro:src/screens/terminal.jsx:94-97
  @oss @pro
  Scenario: Output on the stdout and stderr channels is written to the terminal
    Given a connected session
    When a frame arrives whose first byte is 1 or 2
    Then the rest of the frame is written to the terminal as UTF-8 text
    And stderr output is not distinguished from stdout

  # source: oss:src/screens/terminal.jsx:123
  # source: pro:src/screens/terminal.jsx:98
  @oss @pro
  Scenario: A failure on the status channel is printed in red
    Given a connected session
    When a frame arrives on channel 3 carrying a status of "Failure" with the message "container not found"
    Then the terminal prints "container not found" in red on its own line
    And a Failure with no message prints "exec error"

  # source: oss:src/screens/terminal.jsx:120-123
  # source: pro:src/screens/terminal.jsx:95-98
  @oss @pro
  Scenario: Other status frames and unknown channels are ignored
    Given a connected session
    When a channel-3 frame with a status other than "Failure" arrives, or one that is not valid JSON, or an empty frame, or a frame on any other channel
    Then nothing is printed

  # source: oss:src/screens/terminal.jsx:115
  # source: pro:src/screens/terminal.jsx:91
  @oss @pro
  Scenario: Input typed while the socket is not open is dropped
    Given a session that is still connecting or already closed
    When the user types
    Then nothing is sent and nothing is queued for later

  # ----------------------------------------------------------------- resize

  # source: oss:src/screens/terminal.jsx:96-99,117,133
  # source: pro:src/screens/terminal.jsx:75-78,93,103
  @oss @pro
  Scenario: The terminal refits its container and tells the pod its new size
    Given a connected session
    When the area holding the terminal changes size
    Then the terminal refits to the area
    And a frame whose first byte is 4 is sent carrying JSON with the new "Width" in columns and "Height" in rows

  # ------------------------------------------------------- close and errors

  # source: oss:src/screens/terminal.jsx:125-129
  # source: pro:src/screens/terminal.jsx:100
  @oss @pro
  Scenario: A closed connection is reported in the terminal and is not retried
    Given a connected session
    When the WebSocket closes, for example because the shell exited or the pod was deleted
    Then the state becomes "closed" and the terminal prints "[session closed]"
    And no reconnection is attempted
    And the terminal's earlier output stays readable

  # source: oss:src/screens/terminal.jsx:131
  # source: pro:src/screens/terminal.jsx:101
  # decision: D2
  @oss @pro @drop
  Scenario: A connection error is shown as an error state
    When the WebSocket reports an error, for example because the upgrade was refused or the user is forbidden
    Then the state becomes "error"
    And the reason for the refusal is not shown, because the browser does not expose it
    And when the socket then closes the state becomes "closed"

  # source: oss:src/screens/terminal.jsx:125-131
  # source: pro:src/screens/terminal.jsx:100-101
  # decision: D2
  @fixed @oss @pro
  Scenario: A connection that never opened stays in the error state
    When the WebSocket fails before it ever opened, for example because the upgrade was refused or the user is forbidden
    Then the state reads "error" and stays so after the socket closes
    And the terminal says the connection could not be established, instead of "[session closed]"

  # source: oss:src/screens/terminal.jsx:86,147
  # source: pro:src/screens/terminal.jsx:65,112
  # assumption: the branch tests a statically imported terminal library, so a built app cannot reach it; a build in which that import resolves to an empty value would confirm the message. Stated as the requirement the branch expresses.
  @oss @pro
  Scenario: A terminal that cannot be created says so
    Given the terminal cannot be started in the browser
    When an exec session is opened
    Then the pane shows an error state and says the terminal failed to load
    And no connection is attempted

  # source: oss:src/screens/terminal.jsx:135-141
  # source: pro:src/screens/terminal.jsx:105-106
  @oss @pro
  Scenario: Leaving a session closes its connection
    Given a connected session
    When its tab, dialog or pane is closed
    Then the WebSocket is closed and the terminal is discarded

  # source: oss:src/screens/terminal.jsx:141
  # source: pro:src/screens/terminal.jsx:106
  @oss @pro
  Scenario: Changing the target starts a fresh session
    Given a session to pod "web-1"
    When the pod, namespace, container or command of the session changes
    Then the old connection is closed and a new session is opened with an empty terminal

  # ---------------------------------------------------- session expiry (oss)

  # source: oss:src/screens/terminal.jsx:58-66,125-130
  # decision: D7
  @oss
  Scenario: An abnormal close caused by an expired sign-in routes to sign-in
    Given a session on a server that requires sign-in
    When the WebSocket closes abnormally
    And the server then answers that the current user is not signed in
    Then the user is taken to the sign-in screen

  # source: oss:src/screens/terminal.jsx:61-64
  @oss
  Scenario: A clean close or a still-valid sign-in does not route anywhere
    When the WebSocket closes cleanly, or closes abnormally while the user is still signed in
    Then the user stays where they are

  # source: oss:src/screens/terminal.jsx:87-88
  @oss
  Scenario: A session is not opened for a sign-in that has already ended
    Given the user's sign-in session ended or was replaced before the terminal appeared
    When an exec session would be opened
    Then the state is "closed" and no connection is attempted

  # source: oss:src/screens/terminal.jsx:110-122,127
  @oss
  Scenario: A session opened under an earlier sign-in is cut when the sign-in changes
    Given a connected session
    When the sign-in session ends or is replaced by another
    Then the next input or output closes the connection instead of being delivered
    And "[session closed]" is not printed

  # ------------------------------------------------ cluster pinning (pro)

  # source: pro:src/screens/terminal.jsx:32-36,66-67,88,106
  # source: pro:src/k8sclient.js:54,87-98
  @pro @fleet
  Scenario: A session stays on the cluster it was opened on
    Given an exec session opened while cluster A was selected
    When the selection moves to cluster B
    Then the session is never redirected to cluster B

  # source: pro:src/app.jsx:211-214,417
  # source: pro:src/k8sclient.js:93-98
  @pro @fleet
  Scenario: Switching cluster ends every open exec session
    Given exec sessions are open on cluster A
    When the user switches to cluster B
    Then every exec connection to cluster A is closed
