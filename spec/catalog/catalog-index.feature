Feature: Catalog index sources
  The App Catalog lists the applications described by a catalog index. The
  index is read from the selected cluster first and from a hub-advertised URL
  second; when neither yields a usable index the catalog is empty and says so.

  # source: pro:src/catalog.js:36,47,209-213
  @pro
  Scenario: The index is read from the cluster's catalog ConfigMap
    Given the selected cluster has a ConfigMap "stratum-catalog" in "kube-system" whose "index.json" key holds a valid catalog index
    When the user opens the App Catalog
    Then the applications of that index are listed
    And the catalog reports its source as "ConfigMap"

  # source: pro:src/catalog.js:51,209-211,220-238
  # source: pro:src/cluster-context.js:54-55
  @pro
  Scenario: The hub-advertised catalog URL is used when the ConfigMap is missing
    Given the selected cluster has no readable "stratum-catalog" ConfigMap or it has no "index.json" key
    And the hub configuration advertises a catalog URL
    And that URL answers a valid catalog index
    When the user opens the App Catalog
    Then the applications of the remote index are listed
    And the catalog reports its source as "remote URL"

  # source: pro:src/catalog.js:211-218,220-238
  @pro
  Scenario: The catalog URL is used when the ConfigMap holds an unusable index
    Given the "stratum-catalog" ConfigMap holds an "index.json" that is not a usable catalog index
    And the hub configuration advertises a catalog URL that answers a valid catalog index
    When the user opens the App Catalog
    Then the applications of the remote index are listed
    And no error is shown

  # source: pro:src/catalog.js:209-211,220-221,248
  # source: pro:src/screens/catalog.jsx:550-551,585-596
  @pro
  Scenario: No catalog is available when the ConfigMap is missing and no URL is advertised
    Given the selected cluster has no readable "stratum-catalog" ConfigMap
    And the hub configuration advertises no catalog URL
    When the user opens the App Catalog
    Then the subtitle reads "no catalog index could be read"
    And a "No catalog available" message explains that the ConfigMap "kube-system/stratum-catalog" is missing or holds no usable "index.json" and that no reachable catalog URL is configured on the fleet manager
    And a "Retry" button is offered
    And no error detail line is shown
    And neither the essentials strip, the toolbar nor any application is shown

  # source: pro:src/catalog.js:162-166,211-218,248
  # source: pro:src/screens/catalog.jsx:594
  @pro
  Scenario Outline: An unusable ConfigMap index is reported with its reason when no other source succeeds
    Given the "stratum-catalog" ConfigMap's "index.json" is <content>
    And no catalog URL yields an index
    When the user opens the App Catalog
    Then the "No catalog available" message is shown
    And the error detail starts with "ConfigMap kube-system/stratum-catalog is not a usable catalog index:"
    And the error detail ends with <reason>

    Examples:
      | content                                              | reason                                                                      |
      | text that is not JSON                                | the JSON parser's own message                                               |
      | the JSON value null or a JSON scalar                 | "index is not an object"                                                    |
      | an object with no apiVersion                         | "unexpected apiVersion undefined (want catalog.stratum.ciq.com/v1)"         |
      | an object with apiVersion "other.example/v1"         | "unexpected apiVersion \"other.example/v1\" (want catalog.stratum.ciq.com/v1)" |

  # source: pro:src/catalog.js:164
  @pro
  Scenario: Any apiVersion that begins with the catalog version is accepted
    Given a catalog index whose apiVersion is "catalog.stratum.ciq.com/v1beta1"
    When the index is read
    Then it is accepted as a catalog index

  # source: pro:src/catalog.js:233-242,248
  @pro
  Scenario: A catalog URL that answers an HTTP error is reported
    Given the ConfigMap is missing
    And the advertised catalog URL answers HTTP 503
    When the user opens the App Catalog
    Then the "No catalog available" message is shown
    And the error detail reads "catalog URL <url> failed: HTTP 503"

  # source: pro:src/catalog.js:233-242
  @pro
  Scenario Outline: A catalog URL that answers an unusable index is reported with its reason
    Given the ConfigMap is missing
    And the advertised catalog URL answers successfully with <body>
    When the user opens the App Catalog
    Then the "No catalog available" message is shown
    And the error detail reads "catalog URL <url> failed: " followed by <reason>

    Examples:
      | body                                          | reason                                                                      |
      | a body that is not JSON                       | the JSON parser's own message                                               |
      | a JSON scalar or null                         | "index is not an object"                                                    |
      | an object with apiVersion "other.example/v1"  | "unexpected apiVersion \"other.example/v1\" (want catalog.stratum.ciq.com/v1)" |

  # source: pro:src/catalog.js:210-211,248
  @pro
  Scenario: An empty index.json is treated as a missing ConfigMap, with no error detail
    Given the "stratum-catalog" ConfigMap exists and its "index.json" key is an empty text
    When the user opens the App Catalog
    Then the catalog falls through to the advertised catalog URL, if any
    And when no source yields an index, "No catalog available" is shown with no error detail

  # source: pro:src/catalog.js:233-236
  # assumption: the request sets no credentials mode, so the browser's default applies; observing the request against a same-origin and a cross-origin catalog URL in the running legacy dashboard would confirm which carry the session cookie
  @pro
  Scenario: The catalog URL is fetched with the browser's default credentials
    When the catalog URL is requested
    Then the request asks for JSON
    And it carries the session cookie when the URL is on the dashboard's own origin and not when it is on another origin

  # source: pro:src/catalog.js:41,228-245
  # decision: D8 — legacy bound is 4 seconds
  @pro @drop
  Scenario: A catalog URL that does not finish within 4 seconds is abandoned
    Given the ConfigMap is missing
    And the advertised catalog URL has not delivered its complete body after 4 seconds
    When the user opens the App Catalog
    Then the request is aborted
    And the "No catalog available" message is shown
    And the error detail reads "catalog URL <url> failed: timed out after 4s"

  # source: pro:src/catalog.js:41,47,209,228-245
  # decision: D8
  @pro @fixed
  Scenario: Each catalog source is abandoned after 15 seconds
    Given the ConfigMap read, or the catalog URL including its body, has not completed after 15 seconds
    When the user opens the App Catalog
    Then that request is cancelled
    And a stalled ConfigMap read falls through to the catalog URL, and a stalled URL reads "catalog URL <url> failed: timed out after 15s"

  # source: pro:src/catalog.js:215-216,240-242
  @pro
  Scenario: The ConfigMap's error takes precedence over the URL's when both fail
    Given the ConfigMap holds an unusable index
    And the advertised catalog URL also fails
    When the user opens the App Catalog
    Then the error detail is the ConfigMap's error, not the URL's

  # source: pro:src/catalog.js:47,209
  # source: pro:src/k8sclient.js:75-78
  # decision: D8 — legacy bound is 10 seconds; see "Each catalog source is abandoned after 15 seconds"
  @pro @drop
  Scenario: A ConfigMap read that stalls for 10 seconds is treated as a missing ConfigMap
    Given the ConfigMap read has not completed after 10 seconds
    When the user opens the App Catalog
    Then the read is abandoned
    And the catalog falls through to the advertised catalog URL, if any

  # source: pro:src/catalog.js:167-168
  # source: pro:src/screens/catalog.jsx:552,597-607
  @pro
  Scenario: A valid index with no applications lists nothing
    Given the ConfigMap holds a valid catalog index whose "apps" is absent, empty or not a list
    When the user opens the App Catalog
    Then the subtitle starts with "0 applications · source: ConfigMap"
    And the toolbar is shown with the message "No apps match this filter." and a "Clear filters" link

  # source: pro:src/catalog.js:125-136,168
  @pro
  Scenario Outline: An index entry missing a mandatory field is left out of the catalog
    Given a catalog index with one complete entry and one entry whose <field> is missing, empty or not text
    When the index is read
    Then only the complete entry is listed
    And the rest of the catalog is unaffected

    Examples:
      | field         |
      | id            |
      | name          |
      | chart.repo    |
      | chart.name    |
      | chart.version |

  # source: pro:src/catalog.js:60,67-70,89-105,137-157
  @pro
  Scenario Outline: Optional entry fields fall back to safe defaults
    Given a catalog entry whose <field> is <given>
    When the index is read
    Then the application's <field> is <result>

    Examples:
      | field                   | given                                | result                               |
      | category                | absent                               | "Other"                              |
      | icon                    | absent                               | the generic box icon                 |
      | short description       | absent or not text                   | empty                                |
      | description             | absent or not text                   | empty                                |
      | notes                   | absent or not text                   | empty                                |
      | default values          | absent or not text                   | empty                                |
      | install timeout         | absent or not text                   | empty                                |
      | install target namespace | absent or not text                  | empty                                |
      | create namespace        | absent                               | on                                   |
      | create namespace        | true                                 | on                                   |
      | create namespace        | false                                | off                                  |
      | logo                    | "data:image/svg+xml;base64,AAAA"     | that inline image                    |
      | logo                    | "https://example.com/logo.png"       | none (the icon is shown instead)     |
      | logo                    | "/logos/app.png"                     | none (the icon is shown instead)     |
      | accent colour           | "#1A2B3C"                            | "#1a2b3c"                            |
      | accent colour           | "#ABC"                               | "#aabbcc"                            |
      | accent colour           | "red"                                | none                                 |
      | project home            | "https://example.com/app"            | "https://example.com/app"            |
      | project home            | " HTTP://Example.com "               | "http://example.com/"                |
      | project home            | "javascript:alert(1)"                | none (no link is shown)              |
      | project home            | "JaVaScRiPt:alert(1)"                | none (no link is shown)              |
      | project home            | "data:text/html,x"                   | none (no link is shown)              |
      | project home            | "/docs/app"                          | none (no link is shown)              |
      | parameters              | absent or not a list                 | none                                 |

  # source: pro:src/catalog.js:57,107-122
  @pro
  Scenario Outline: Index parameters are normalised or dropped
    Given a catalog entry with a parameter that has <given>
    When the index is read
    Then <result>

    Examples:
      | given                                         | result                                                   |
      | no key, an empty key or a non-text key        | the parameter is dropped and the application is kept     |
      | no type                                       | the parameter is a text parameter                        |
      | an unrecognised type such as "integer"        | the parameter is a text parameter                        |
      | type "enum" and no options                    | the parameter is dropped and the application is kept     |
      | type "enum" and options 1, 2 and 3            | the options are offered as the texts "1", "2" and "3"    |
      | no label                                      | the parameter is labelled with its key                   |
      | required set to true                          | the parameter is required                                |
      | required set to "yes" or 1                    | the parameter is optional                                |
      | no help or group                              | the parameter has no help text and belongs to no group   |

  # source: pro:src/catalog.js:174-189,200
  # source: pro:src/screens/catalog.jsx:187-196
  @pro
  Scenario: A cluster's catalog is read once per page load
    Given the catalog of the selected cluster has already been resolved during this page load
    When the user leaves the App Catalog and returns to it
    Then the previously resolved catalog is shown without reading the ConfigMap or the URL again

  # source: pro:src/catalog.js:198-202,248
  # decision: D2 — a transient failure sticks for the whole page load (NOTES (a) 2)
  @pro @drop
  Scenario: A failed catalog resolution is also remembered until the user retries
    Given the catalog of the selected cluster resolved to "No catalog available"
    When the user leaves the App Catalog and returns to it
    Then "No catalog available" is shown again without any new request
    And only "Retry" or a full page reload reads the sources again

  # source: pro:src/catalog.js:198-202,248
  # decision: D2
  @pro @fixed
  Scenario: A failed catalog resolution is not remembered
    Given the catalog of the selected cluster resolved to "No catalog available"
    When the user leaves the App Catalog and returns to it
    Then the sources are read again

  # source: pro:src/catalog.js:203-209,250-255
  # decision: D8 — requests are cancelled when their screen changes
  @pro @drop
  Scenario: Leaving the catalog mid-read neither cancels the read nor poisons the remembered catalog
    Given the selected cluster's catalog index is being read
    When the user leaves the App Catalog before the read finishes
    Then the read continues to completion
    And its result is what the next visit to that cluster's catalog shows

  # source: pro:src/catalog.js:203-209,250-255
  # decision: D8
  @pro @fixed
  Scenario: Leaving the catalog mid-read cancels the read without remembering an empty catalog
    Given the selected cluster's catalog index is being read
    When the user leaves the App Catalog or switches cluster before the read finishes
    Then the read is cancelled
    And nothing is remembered for it, so the next visit reads the sources again

  # source: pro:src/catalog.js:181-189,250-255
  @pro
  Scenario: Simultaneous first reads of one cluster's catalog share one request
    Given the catalog of the selected cluster is being read for the first time
    When a second read of the same cluster's catalog starts before the first finishes
    Then the ConfigMap is requested only once and both reads receive the same result

  # source: pro:src/catalog.js:28-31,174-188
  # source: pro:src/screens/catalog.jsx:187-196
  @pro @fleet
  Scenario: Switching cluster shows the new cluster's catalog, never the previous one's
    Given the App Catalog shows cluster A's catalog with a search, a category, the Installed filter and an open application
    When the user switches to cluster B
    Then the loading state is shown and the previous applications disappear at once
    And the search, the category and the Installed filter are cleared and the detail panel is closed
    And cluster B's own catalog is read and shown

  # source: pro:src/screens/catalog.jsx:187-206
  @pro @fleet
  Scenario: A catalog that resolves after the user moved on is discarded
    Given a catalog read for cluster A is still in flight
    When the user switches to cluster B and A's read then completes
    Then A's result is not shown

  # source: pro:src/screens/catalog.jsx:198-206,562-564,595
  # source: pro:src/catalog.js:183-202
  @pro
  Scenario: Refresh re-reads the index and the installed releases
    Given the App Catalog is showing a resolved catalog
    When the user presses "Refresh"
    Then the loading state is shown and "Refresh" is disabled until the read completes
    And the ConfigMap, and the URL if needed, are read again regardless of what was remembered
    And the list of installed releases is requested again immediately
    And the search, category, Installed filter and open application are kept

  # source: pro:src/catalog.js:190-202
  @pro
  Scenario: An older in-flight read never overwrites a newer refresh
    Given a catalog read is in flight
    When the user presses "Refresh" and the older read completes after the refresh
    Then the refresh's result is the one shown and remembered
