Feature: Generic resource list
  Every Kubernetes kind the dashboard knows is shown by one shared list:
  it loads the kind across all namespaces, keeps itself fresh while it is
  on screen, and reports loading, empty and failed states the same way for
  every kind.

  # ---- reaching a list -------------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:491
  # source: pro:src/app.jsx:514
  Scenario: A route id that names a known kind shows that kind's list
    Given the route id in the address is the id of a known kind
    When the page is shown
    Then the generic list for that kind is shown, titled with the kind's plural label

  @diverged @oss @drop
  # decision: D1, D11 — every cluster screen lives under /c/$cluster/…; the old address redirects there (spec/shell)
  # source: oss:src/app.jsx:131-137
  # source: oss:src/app.jsx:375-381
  Scenario: A kind's list is addressed directly by its route id
    When the user opens "/pods"
    Then the Pods list is shown

  @diverged @pro
  # decision: D11 — applies to both backends; a single-cluster backend uses the cluster id "local"
  # source: pro:src/app.jsx:54-64
  # source: pro:src/app.jsx:345-353
  Scenario: A kind's list is addressed under the cluster it belongs to
    When the user opens "/c/<cluster id>/pods"
    Then the Pods list of that cluster is shown

  # ---- loading ---------------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:58-60,102-110,156
  # source: pro:src/ui/resource-list.jsx:40-42,86-96,148
  Scenario: The list says it is loading until the first answer arrives
    Given a kind's list has just been opened
    When the cluster has not answered yet
    Then the line under the title reads "Loading…"
    And the table shows its column headers and no rows
    And no "nothing found" message is shown

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:105,116-120
  # source: pro:src/ui/resource-list.jsx:92,103-107
  Scenario: The list is read once for all namespaces
    Given a namespaced kind's list is open with one namespace selected
    When the list loads
    Then the kind is requested across all namespaces in a single read
    And the namespace selection is applied to the rows afterwards, without another request

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:156
  # source: pro:src/ui/resource-list.jsx:148
  Scenario Outline: The line under the title counts the rows shown
    Given the list has loaded <total> objects of kind "Pod"
    And <shown> of them pass the namespace selection and the search
    Then the line under the title reads "<text>"

    Examples:
      | total | shown | text         |
      | 3     | 3     | 3 Pods       |
      | 1     | 1     | 1 Pod        |
      | 0     | 0     | 0 Pods       |
      | 12    | 4     | 4 of 12 Pods |
      | 12    | 1     | 1 of 12 Pod  |

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:157
  # source: pro:src/ui/resource-list.jsx:149
  Scenario: The count names the selected namespace for a namespaced kind
    Given a namespaced kind's list is open
    And the namespace "kube-system" is selected
    Then the line under the title ends with " · kube-system"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:157
  # source: pro:src/ui/resource-list.jsx:149
  Scenario: The count never names a namespace for a cluster-scoped kind
    Given a cluster-scoped kind's list is open
    And the namespace "kube-system" is selected
    Then the line under the title does not mention a namespace

  # ---- columns and rows ------------------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:176-181,191
  # source: pro:src/ui/resource-list.jsx:168-173,179
  Scenario: The table shows the kind's own columns in a fixed order
    Given a kind's list has loaded
    Then the table has one column per column the kind defines, in the kind's order
    And the user cannot add, remove, reorder or resize columns

  @oss @pro
  # source: oss:src/resources.jsx:18-20
  # source: pro:src/resources.jsx:15-17
  Scenario: Every kind has a Name column and an Age column, and namespaced kinds a Namespace column
    Given a kind's list has loaded
    Then the first column is "Name" and shows each object's name
    And the last column is "Age"
    And a namespaced kind has a "Namespace" column directly after "Name"
    And a cluster-scoped kind has no "Namespace" column

  @oss @pro
  # source: oss:src/k8s-rules.js:63-70
  # source: pro:src/k8srules.js:9-16
  Scenario Outline: Age is the time since creation in one rounded unit
    Given an object was created <elapsed> ago
    Then its Age reads "<age>"

    Examples:
      | elapsed     | age |
      | 5 seconds   | 5s  |
      | 89 seconds  | 89s |
      | 90 seconds  | 2m  |
      | 12 minutes  | 12m |
      | 89 minutes  | 89m |
      | 90 minutes  | 2h  |
      | 47 hours    | 47h |
      | 48 hours    | 2d  |
      | 10 days     | 10d |

  @oss @pro
  # source: oss:src/k8s-rules.js:64
  # source: pro:src/k8srules.js:10
  Scenario: An object without a creation time has no age
    Given an object carries no creation time
    Then its Age reads "—"

  @oss @pro
  # source: oss:src/k8s-rules.js:65
  # source: pro:src/k8srules.js:11
  Scenario: A creation time in the future reads as zero seconds
    Given an object's creation time is later than the browser's clock
    Then its Age reads "0s"

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:183,116-124
  # source: pro:src/ui/resource-list.jsx:175,103-111
  # assumption: no sort control or comparator exists anywhere in the list; inferred from its absence
  Scenario: Rows keep the order the cluster returned and cannot be sorted
    Given a kind's list has loaded
    Then the rows appear in the order the cluster returned them
    And clicking a column header does nothing

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:105-108,183
  # source: pro:src/ui/resource-list.jsx:92-95,175
  # assumption: no paging, limit or continue handling exists; inferred from its absence
  Scenario: Every object is shown on one page
    Given a kind has more objects than fit on screen
    Then every object that passes the filters is a row in the one scrolling table
    And there are no pages or "load more" control

  @oss @pro
  # source: oss:src/ui/shell.jsx:74-96
  # source: pro:src/ui/shell.jsx:62-80
  Scenario Outline: A status value is shown as a badge with a fixed wording
    Given a status column holds "<status>"
    Then the badge reads "<label>"

    Examples:
      | status           | label       |
      | Running          | Running     |
      | Pending          | Pending     |
      | Failed           | Failed      |
      | CrashLoopBackOff | CrashLoop   |
      | Terminating      | Terminating |
      | Complete         | Complete    |
      | Scheduled        | Scheduled   |
      | Bound            | Bound       |
      | Active           | Active      |
      | Deployed         | Deployed    |

  @oss @pro
  # source: oss:src/ui/shell.jsx:87-88
  # source: pro:src/ui/shell.jsx:75-76
  Scenario: A status the dashboard does not know is shown verbatim
    Given a status column holds a value outside the known set, such as "Released"
    Then the badge reads "Released" in the neutral colour

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:179,192-203
  # source: pro:src/ui/resource-list.jsx:171,180-191
  Scenario: Kinds with contextual actions get a trailing action column
    Given a kind defines at least one contextual action the user may see
    Then each row ends with one icon button per visible action, named by the action's title
    And the action column has no header text

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:93,179,192
  # source: pro:src/ui/resource-list.jsx:74,171,180
  Scenario: The action column disappears when no action is visible
    Given a kind defines no contextual action, or every one it defines is hidden from this user
    Then the table has no action column

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:196-197
  # source: pro:src/ui/resource-list.jsx:184-185
  Scenario: Using a row's contextual action does not open the row's detail
    Given a kind's list has loaded
    When the user clicks a contextual action button on a row
    Then that action runs for the row's object
    And the detail panel does not open or change

  # ---- empty, error, forbidden ----------------------------------------------

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:206-212
  # source: pro:src/ui/resource-list.jsx:194-200
  Scenario: A kind with no objects says so
    Given the list has loaded and the cluster holds no object of the kind
    Then the table shows the single message "No <kind's plural label in lower case> found."

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:107,206-212
  # source: pro:src/ui/resource-list.jsx:94,194-200
  Scenario: A first load that fails shows an error in place of the rows
    Given a kind's list has just been opened
    When the cluster answers the read with an error, or does not answer
    Then the table shows "Unable to load — the apiserver returned an error (or the RBAC lacks access)."
    And the line under the title reads "0 <Kind>s"

  @oss @pro @drop
  # decision: D2, D9 — a forbidden kind gets its own state
  # source: oss:src/k8sutil.js:104-110
  # source: oss:src/ui/resource-list.jsx:107
  # source: pro:src/k8sclient.js:75-78
  # source: pro:src/ui/resource-list.jsx:94
  Scenario: A forbidden read looks the same as any other failed read
    Given the signed-in user is not allowed to list the kind
    When the kind's list is opened
    Then the table shows "Unable to load — the apiserver returned an error (or the RBAC lacks access)."
    And nothing distinguishes the refusal from a network or server failure

  @oss @pro @fixed
  # decision: D9 — a forbidden kind gets its own state; neither dashboard has one today
  # source: oss:src/k8sutil.js:104-110
  # source: oss:src/ui/resource-list.jsx:107
  # source: pro:src/k8sclient.js:75-78
  # source: pro:src/ui/resource-list.jsx:94
  Scenario: A forbidden read is shown as its own state
    Given the signed-in user is not allowed to list the kind
    When the kind's list is opened
    Then the table says the user is not permitted to list this kind, in words distinct from a failed load
    And the list keeps re-reading on its usual schedule and shows rows as soon as access is granted
    And no other kind's list is affected

  @oss @pro @drop
  # decision: D2 — silent staleness is not reproduced
  # source: oss:src/ui/resource-list.jsx:107-108,183,206
  # source: pro:src/ui/resource-list.jsx:94-95,175,194
  Scenario: A refresh that fails keeps the last rows and shows no warning
    Given a kind's list has loaded rows
    When a later refresh fails
    Then the rows from the last successful load stay on screen
    And nothing on the list indicates that they are stale

  @oss @pro @drop
  # decision: D2 — silent staleness is not reproduced; the corrected behavior is the @fixed scenario below
  # source: oss:src/ui/resource-list.jsx:206-210
  # source: pro:src/ui/resource-list.jsx:194-198
  Scenario: A failed refresh surfaces only when no row is visible
    Given a kind's list has loaded rows and the latest refresh failed
    When the namespace selection or the search hides every row
    Then the table shows the "Unable to load…" message instead of "No … found."

  @oss @pro @fixed
  # decision: D2 — replaces the two scenarios above
  # source: oss:src/ui/resource-list.jsx:107-108,183,206-210
  # source: pro:src/ui/resource-list.jsx:94-95,175,194-198
  Scenario: A refresh that fails keeps the last rows and marks them as stale
    Given a kind's list has loaded rows
    When a later refresh fails
    Then the rows from the last successful load stay on screen
    And the list shows that the latest refresh failed, whether or not any row is visible
    And "No … found." is shown only when the last successful load had nothing to show
    And the stale marker goes away with the next successful refresh

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:107-108
  # source: pro:src/ui/resource-list.jsx:94-95
  Scenario: The next successful refresh clears the error
    Given the list shows the "Unable to load…" message
    When a later refresh succeeds
    Then the rows are shown and the message is gone

  # ---- staying fresh ---------------------------------------------------------

  @diverged @oss @drop
  # decision: D1, D9 — pro's settle-then-wait cadence with backoff stands
  # source: oss:src/ui/resource-list.jsx:102-114
  Scenario: The open list re-reads itself every 15 seconds
    Given a kind's list is open
    Then it is read again every 15 seconds on a fixed clock
    And the cadence is the same whether the previous read succeeded, failed or is still pending

  @diverged @pro
  # decision: D9 — polling, one query per kind
  # source: pro:src/ui/resource-list.jsx:86-101
  # source: pro:src/poll.js:9
  Scenario: The open list re-reads itself 15 seconds after the previous read settled
    Given a kind's list is open and its last read succeeded
    Then the next read starts 15 seconds after that read finished
    And two reads of the same list never overlap

  @pro
  # source: pro:src/ui/resource-list.jsx:90-97
  # source: pro:src/poll.js:9-26
  Scenario Outline: Repeated failures slow the refresh down
    Given a kind's list is open
    When <fails> consecutive reads have failed
    Then the next read is attempted after about <delay> seconds, give or take 15 percent
    And never sooner than 15 seconds or later than 120 seconds

    Examples:
      | fails | delay |
      | 1     | 15    |
      | 2     | 30    |
      | 3     | 60    |
      | 4     | 120   |
      | 9     | 120   |

  @pro
  # source: pro:src/ui/resource-list.jsx:95-97
  Scenario: One successful read restores the normal refresh pace
    Given the list has been backing off after failed reads
    When a read succeeds
    Then the next read starts 15 seconds later

  @oss @pro
  # decision: D8 — every request has a 15 second deadline
  # source: oss:src/k8sutil.js:31,77,90
  # source: pro:src/ui/resource-list.jsx:92
  Scenario: A read that gets no answer within 15 seconds counts as failed
    Given a kind's list is open
    When a read has been pending for 15 seconds
    Then it is abandoned and treated as a failed read

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:102-114
  # source: pro:src/ui/resource-list.jsx:86-101
  # assumption: the list never consults the shell's live/paused toggle; inferred from the absence of any such input
  Scenario: Pausing auto-refresh in the shell does not pause an open list
    Given a kind's list is open
    When the user pauses auto-refresh from the top bar
    Then the list keeps re-reading itself on its own schedule

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:95-98,108
  # source: pro:src/ui/resource-list.jsx:76-81,95
  Scenario: A refresh does not disturb what the user is doing
    Given a kind's list is open with a search typed and a detail panel open
    When the list refreshes
    Then the search text and the open panel are kept
    And the table is not blanked while the read is in flight

  @oss @pro
  # decision: D8, D9 — a kind's query is cancelled when its screen changes and never touches another kind's
  # source: oss:src/ui/resource-list.jsx:103-106,113
  # source: pro:src/ui/resource-list.jsx:87-93,100
  Scenario: Leaving a list stops its refresh and discards late answers
    Given a kind's list is open with a read in flight
    When the user navigates to another kind
    Then the previous kind's refresh stops
    And an answer that arrives for the previous kind is not shown

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:98
  # source: pro:src/ui/resource-list.jsx:78-81
  Scenario: Switching kind starts from a clean list
    Given a kind's list is open with a search typed, a detail or create panel open, or a confirmation dialog up
    When the user navigates to a different kind
    Then the search is empty, no panel or dialog is open, the old rows are gone
    And the new list shows "Loading…" until it answers

  @pro @fleet
  # source: pro:src/ui/resource-list.jsx:78-81,101
  # source: pro:src/app.jsx:211-214
  Scenario: Switching cluster starts the same kind's list from clean
    Given a kind's list is open on one cluster
    When the address changes to the same kind on another cluster
    Then reads still pending for the first cluster are cancelled
    And the list resets and loads from the new cluster

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:127-128
  # source: pro:src/ui/resource-list.jsx:114-115
  Scenario: A successful write refreshes the list at once
    Given a kind's list is open
    When a create, edit, delete, scale or restart made from the list succeeds
    Then the list is re-read immediately rather than at the next scheduled refresh
    And the search text is kept

  @oss @pro
  # source: oss:src/ui/resource-list.jsx:128
  # source: pro:src/ui/resource-list.jsx:115
  Scenario: A failed write does not refresh the list
    Given a kind's list is open
    When a write made from the list is refused or fails
    Then a notice reading "Error: <the server's message>" is shown
    And the list is not re-read early

  @oss @pro
  # source: oss:src/app.jsx:372
  # source: pro:src/app.jsx:342
  Scenario: Notices from the list disappear on their own
    Given a write from the list produced a notice
    Then the notice is removed after 2.8 seconds
