Feature: Ages and Kubernetes quantities
  The conversions every list, meter and total is built on: a timestamp to a
  relative age, a CPU quantity to millicores, a memory quantity to mebibytes
  (Mi), and kubelet usage figures to the same units.

  # ----------------------------------------------------------------------- age

  @oss @pro
  # source: oss:src/k8s-rules.js:62-70
  # source: pro:src/k8srules.js:8-16
  Scenario Outline: A timestamp is shown as a rounded relative age in one unit
    Given an object was created <elapsed> ago
    Then its age is shown as "<age>"

    Examples:
      | elapsed                 | age  |
      | 0 seconds               | 0s   |
      | 5 seconds               | 5s   |
      | 89 seconds              | 89s  |
      | 90 seconds              | 2m   |
      | 12 minutes              | 12m  |
      | 89 minutes 20 seconds   | 89m  |
      | 90 minutes              | 2h   |
      | 3 hours                 | 3h   |
      | 47 hours 20 minutes     | 47h  |
      | 48 hours                | 2d   |
      | 4 days                  | 4d   |
      | 400 days                | 400d |

  @oss @pro
  # source: oss:src/k8s-rules.js:64
  # source: pro:src/k8srules.js:10
  Scenario: A missing timestamp has no age
    Given an object has no creation timestamp
    Then its age is shown as "—"

  @oss @pro
  # source: oss:src/k8s-rules.js:65
  # source: pro:src/k8srules.js:11
  Scenario: A timestamp in the future reads as zero seconds
    Given an object's timestamp is 30 seconds ahead of the browser's clock
    Then its age is shown as "0s"

  @oss @pro
  # source: oss:src/k8s-rules.js:62-70
  # source: pro:src/k8srules.js:8-16
  # assumption: ages are computed when a load builds its rows and are not recomputed between loads
  Scenario: Ages advance only when the data is reloaded
    Given a row shows the age "5s"
    When 10 seconds pass without a load
    Then the row still shows "5s"

  # ----------------------------------------------------------------------- CPU

  @oss @pro
  # source: oss:src/k8s-rules.js:72-81
  # source: pro:src/k8srules.js:18-27
  Scenario Outline: A CPU quantity is converted to millicores
    Given a CPU quantity of <quantity>
    Then it counts as <millicores> millicores

    Examples:
      | quantity          | millicores |
      | "500m"            | 500        |
      | "2"               | 2000       |
      | "0.5"             | 500        |
      | "250000000n"      | 250        |
      | "1500u"           | 1.5        |
      | the number 2      | 2000       |
      | nothing           | 0          |

  # -------------------------------------------------------------------- memory

  @oss @pro
  # source: oss:src/k8s-rules.js:83-92
  # source: pro:src/k8srules.js:29-53
  Scenario Outline: A memory quantity is converted to mebibytes
    Given a memory quantity of <quantity>
    Then it counts as <mebibytes> Mi

    Examples:
      | quantity     | mebibytes          |
      | "512Mi"      | 512                |
      | "2Gi"        | 2048               |
      | "1.5Gi"      | 1536               |
      | "1048576Ki"  | 1024               |
      | "1Ti"        | 1048576            |
      | "1Pi"        | 1073741824         |
      | "1048576"    | 1                  |
      | "1M"         | about 0.954        |
      | "1G"         | about 953.67       |
      | "1T"         | about 953674.3     |
      | "1K"         | about 0.00095      |
      | nothing      | 0                  |
      | "abc"        | 0                  |
      | "-5Mi"       | 0                  |

  @diverged @oss @drop
  # decision: D1, D2 — the pro variant stands (NOTES O5)
  # source: oss:src/k8s-rules.js:86-91
  Scenario Outline: Memory forms outside the short unit table are read as bytes or rejected
    Given a memory quantity of "<quantity>"
    Then it counts as <mebibytes> Mi, because <reason>

    Examples:
      | quantity  | mebibytes          | reason                                        |
      | 100k      | about 0.0000954    | the unit is unknown, so the number is bytes   |
      | 1500m     | about 0.00143      | the unit is unknown, so the number is bytes   |
      | 1P        | about 0.00000095   | the unit is unknown, so the number is bytes   |
      | 1E        | about 0.00000095   | the unit is unknown, so the number is bytes   |
      | 1Ei       | about 0.00000095   | the unit is unknown, so the number is bytes   |
      | 5Zi       | about 0.0000048    | the unit is unknown, so the number is bytes   |
      | 129e6     | 0                  | an exponent is not accepted                   |
      | 1e3Mi     | 0                  | an exponent is not accepted                   |

  @diverged @pro
  # source: pro:src/k8srules.js:29-53
  Scenario Outline: Memory accepts every apiserver quantity form and rejects unknown units
    Given a memory quantity of "<quantity>"
    Then it counts as <mebibytes> Mi, because <reason>

    Examples:
      | quantity  | mebibytes          | reason                                        |
      | 100k      | about 0.0954       | lowercase k is 1000 bytes                     |
      | 1500m     | about 0.0000014    | m is thousandths of a byte                    |
      | 1P        | about 953674316    | P is 10^15 bytes                              |
      | 1E        | about 953674316406 | E is 10^18 bytes                              |
      | 1Ei       | 1099511627776      | Ei is 2^60 bytes                              |
      | 5Zi       | 0                  | an unknown unit is zero, never bytes          |
      | 129e6     | about 123.02       | a decimal exponent is accepted                |
      | 1e3Mi     | 1000               | a decimal exponent is accepted                |

  # ------------------------------------------------------------- kubelet usage

  @oss @pro
  # source: oss:src/live.js:188-199
  # source: pro:src/live.js:484-495
  Scenario Outline: Kubelet usage figures are converted to millicores and mebibytes
    Given a kubelet reports CPU usage of <nanocores> nanocores and a working set of <bytes> bytes
    Then the usage counts as <millicores> millicores and <mebibytes> Mi

    Examples:
      | nanocores   | bytes       | millicores | mebibytes |
      | 250000000   | 536870912   | 250        | 512       |
      | 1500000000  | 1073741824  | 1500       | 1024      |
      | 0           | 0           | 0          | 0         |

  @oss @pro
  # source: oss:src/live.js:98-102
  # source: pro:src/live.js:274-278
  Scenario: A pod's requests are the sum over its regular containers
    Given a pod has containers requesting "100m"/"128Mi" and "250m"/"256Mi"
    And an init container requesting "1"/"1Gi"
    Then the pod's requests count as 350 millicores and 384 Mi
    And a container with no requests adds nothing
