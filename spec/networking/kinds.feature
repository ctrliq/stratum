Feature: Networking kinds and their list columns
  Services, Endpoints, Endpoint Slices, Ingresses, Ingress Classes and
  Network Policies each have a list built on the generic resource list (see
  spec/resources). This feature records their routes, columns and how each
  column value is derived.

  @oss @pro
  # source: oss:src/resources.jsx:82,88,92,99,104,108
  # source: oss:src/registry.js:60-67
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:101,107,111,118,123,127
  # source: pro:src/nav.js:34-41
  # source: pro:src/app.jsx:514
  Scenario Outline: Each networking kind has its own list
    When the user opens the route "<route>"
    Then the generic list for kind "<kind>" is shown, titled "<title>"
    And the kind is <scope>

    Examples:
      | route           | kind          | title            | scope          |
      | services        | Service       | Services         | namespaced     |
      | endpoints       | Endpoints     | Endpoints        | namespaced     |
      | endpointslices  | EndpointSlice | Endpoint Slices  | namespaced     |
      | ingresses       | Ingress       | Ingresses        | namespaced     |
      | ingressclasses  | IngressClass  | Ingress Classes  | cluster-scoped |
      | networkpolicies | NetworkPolicy | Network Policies | namespaced     |

  @oss @pro
  # source: oss:src/registry.js:60-67
  # source: pro:src/nav.js:34-41
  Scenario: The sidebar's Network section lists the six kinds
    Then the sidebar section "Network" has the rows Services, Endpoints, Endpoint Slices, Ingresses, Ingress Classes and Network Policies, in that order

  @oss @pro
  # source: oss:src/resources.jsx:83-111
  # source: pro:src/resources.jsx:102-130
  Scenario Outline: Each networking kind shows its columns in order
    When the "<route>" list is shown
    Then its columns are, in order: <columns>

    Examples:
      | route           | columns                                                       |
      | services        | Name, Namespace, Type, Cluster IP, Ports, Age                 |
      | endpoints       | Name, Namespace, Addresses, Age                               |
      | endpointslices  | Name, Namespace, Service, Address Type, Endpoints, Ports, Age |
      | ingresses       | Name, Namespace, Class, Hosts, Age                            |
      | ingressclasses  | Name, Controller, Age                                         |
      | networkpolicies | Name, Namespace, Policy Types, Age                            |

  @oss @pro
  # source: oss:src/resources.jsx:84-110
  # source: pro:src/resources.jsx:103-129
  Scenario Outline: A networking column shows a value read from the object
    Given the "<route>" list is shown
    Then the "<column>" column shows <value>

    Examples:
      | route           | column       | value                                                                              |
      | services        | Cluster IP   | the service's cluster IP, or "None" when it has none                               |
      | endpointslices  | Service      | the value of the label "kubernetes.io/service-name", or "—" when unlabelled        |
      | endpointslices  | Address Type | the slice's address type, or "—" when absent                                       |
      | endpointslices  | Endpoints    | the number of endpoints in the slice                                               |
      | ingresses       | Class        | the ingress class name from spec, or "—" when unset                                |
      | ingressclasses  | Controller   | the controller named in spec, or "—" when absent                                   |
      | networkpolicies | Policy Types | the policy types from spec joined by ", ", or "—" when none                        |

  @oss @pro
  # source: oss:src/resources.jsx:84
  # source: pro:src/resources.jsx:103
  Scenario Outline: Service Type calls out headless services
    Given a service with cluster IP <cluster ip> and type <type>
    Then its Type column reads "<shown>"

    Examples:
      | cluster ip | type         | shown        |
      | 10.0.0.12  | ClusterIP    | ClusterIP    |
      | 10.0.0.12  | NodePort     | NodePort     |
      | 10.0.0.12  | LoadBalancer | LoadBalancer |
      | 10.0.0.12  | unset        | ClusterIP    |
      | None       | ClusterIP    | Headless     |
      | unset      | ExternalName | ExternalName |

  @oss @pro
  # source: oss:src/resources.jsx:25,86
  # source: pro:src/resources.jsx:41,105
  Scenario Outline: Service Ports list port, node port and protocol
    Given a service whose ports are <ports>
    Then its Ports column reads "<shown>"

    Examples:
      | ports                                        | shown               |
      | 80 over TCP                                  | 80/TCP              |
      | 80 with no protocol stated                   | 80/TCP              |
      | 80 with node port 30080                      | 80:30080/TCP        |
      | 53 over UDP and 53 over TCP                  | 53/UDP, 53/TCP      |
      | none                                         | —                   |

  @oss @pro
  # source: oss:src/resources.jsx:25,84-86
  # source: pro:src/resources.jsx:41,103-105
  # assumption: no column reads target ports, external IPs, load-balancer addresses or selectors; inferred from their absence
  Scenario: A service's external address and target ports appear only in its manifest
    Given a LoadBalancer service with an external address
    Then no list column shows the external address or the target ports
    And they can be read in the detail panel's manifest

  @oss @pro
  # source: oss:src/resources.jsx:90
  # source: pro:src/resources.jsx:109
  Scenario Outline: Endpoints Addresses shows at most four ready addresses
    Given an Endpoints object with <ready> ready and <not ready> not-ready addresses across its subsets
    Then its Addresses column shows <shown>

    Examples:
      | ready | not ready | shown                                                   |
      | 2     | 0         | both ready IPs joined by ", "                           |
      | 6     | 0         | the first four ready IPs, with no sign that more exist  |
      | 0     | 3         | "—"                                                     |
      | 0     | 0         | "—"                                                     |

  @oss @pro
  # source: oss:src/resources.jsx:97
  # source: pro:src/resources.jsx:116
  Scenario Outline: Endpoint slice Ports list port and protocol
    Given an endpoint slice whose ports are <ports>
    Then its Ports column reads "<shown>"

    Examples:
      | ports                      | shown        |
      | 8080 over TCP              | 8080/TCP     |
      | 8080 with no protocol      | 8080/TCP     |
      | a port entry with no number| */TCP        |
      | 53 over UDP and 53 over TCP| 53/UDP, 53/TCP |
      | none                       | —            |

  @oss @pro
  # source: oss:src/resources.jsx:102
  # source: pro:src/resources.jsx:121
  Scenario Outline: Ingress Hosts lists the rule hosts
    Given an ingress whose rules name <hosts>
    Then its Hosts column reads "<shown>"

    Examples:
      | hosts                              | shown                    |
      | a.example.com and b.example.com    | a.example.com, b.example.com |
      | a.example.com and one host-less rule | a.example.com          |
      | no host on any rule                | *                        |
      | no rules at all                    | *                        |

  # ---- the ingress alias -----------------------------------------------------

  @oss @pro
  # source: oss:src/app.jsx:488
  # source: oss:src/registry.js:64
  # source: pro:src/app.jsx:513
  # source: pro:src/nav.js:38
  Scenario: The sidebar's Ingresses row uses the route "ingress"
    When the user clicks "Ingresses" in the sidebar
    Then the address ends in the route "ingress"
    And the Ingresses list is shown, the same list the route "ingresses" shows

  @oss @pro
  # source: oss:src/app.jsx:124-129,496
  # source: oss:src/registry.js:64
  # source: pro:src/app.jsx:66-71,519
  # source: pro:src/nav.js:38
  Scenario: Only the route "ingress" lights the sidebar row
    When the Ingresses list is reached through the route "ingresses"
    Then no sidebar row is marked as current
    And when it is reached through the route "ingress" the "Ingresses" row is marked as current

  @diverged @oss
  # source: oss:src/app.jsx:114-119,462
  Scenario: The breadcrumb for the ingress route reads Ingresses
    When the user opens the route "ingress"
    Then the breadcrumb's last entry reads "Ingresses"

  @diverged @pro @drop
  # decision: D1 — "Ingresses" matches the list title and the sidebar row
  # source: pro:src/app.jsx:41-46,474
  Scenario: The breadcrumb for the ingress route reads Ingress
    When the user opens the route "ingress"
    Then the breadcrumb's last entry reads "Ingress"
    And the list below it is still titled "Ingresses"

  # ---- what each kind lets the user do ---------------------------------------

  @oss @pro
  # source: oss:src/resources.jsx:240-241,254-257
  # source: pro:src/resources.jsx:257-258,271-274
  Scenario: Networking kinds have no actions of their own
    Given the session is offered write controls
    When a detail panel is open for a Service, Endpoints, Ingress, Ingress Class or Network Policy
    Then it offers Edit and Delete and no Scale, Restart or contextual action

  @diverged @oss
  # decision: D15 — both variants stand: this one on the dashboard server backend
  # source: oss:src/resources.jsx:92,254-257
  Scenario: Endpoint slices can be created, edited and deleted
    Given the session is offered write controls
    When the Endpoint Slices list is open
    Then Create is offered
    And a row's detail panel offers Edit and Delete

  @diverged @pro @fleet
  # decision: D15 — both variants stand: this one on the hub backend
  # source: pro:src/resources.jsx:270-274
  # source: pro:src/ui/resource-list.jsx:158
  Scenario: Endpoint slices are read-only
    Given the signed-in user has the admin role
    When the Endpoint Slices list is open
    Then no Create button is shown
    And a row's detail panel offers no Edit or Delete
