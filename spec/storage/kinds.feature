Feature: Storage kinds and their list columns
  Persistent Volume Claims, Persistent Volumes and Storage Classes each have
  a list built on the generic resource list (see spec/resources). This
  feature records their routes, columns and how each column value is
  derived.

  @oss @pro
  # source: oss:src/resources.jsx:168,174,182
  # source: oss:src/registry.js:68-72
  # source: oss:src/app.jsx:491
  # source: pro:src/resources.jsx:187,193,201
  # source: pro:src/nav.js:42-46
  # source: pro:src/app.jsx:514
  Scenario Outline: Each storage kind has its own list
    When the user opens the route "<route>" or its row in the sidebar's "Storage" section
    Then the generic list for kind "<kind>" is shown, titled "<title>"
    And the kind is <scope>

    Examples:
      | route                  | kind                  | title                    | scope          |
      | persistentvolumeclaims | PersistentVolumeClaim | Persistent Volume Claims | namespaced     |
      | persistentvolumes      | PersistentVolume      | Persistent Volumes       | cluster-scoped |
      | storageclasses         | StorageClass          | Storage Classes          | cluster-scoped |

  @oss @pro
  # source: oss:src/resources.jsx:169-187
  # source: pro:src/resources.jsx:188-206
  Scenario Outline: Each storage kind shows its columns in order
    When the "<route>" list is shown
    Then its columns are, in order: <columns>

    Examples:
      | route                  | columns                                                     |
      | persistentvolumeclaims | Name, Namespace, Status, Capacity, Storage Class, Age       |
      | persistentvolumes      | Name, Capacity, Reclaim, Status, Claim, Storage Class, Age  |
      | storageclasses         | Name, Provisioner, Reclaim, Binding, Age                    |

  @oss @pro
  # source: oss:src/resources.jsx:170-186
  # source: pro:src/resources.jsx:189-205
  Scenario Outline: A storage column shows a value read from the object
    Given the "<route>" list is shown
    Then the "<column>" column shows <value>

    Examples:
      | route                  | column        | value                                                                          |
      | persistentvolumeclaims | Status        | the claim's phase as a status badge                                            |
      | persistentvolumeclaims | Capacity      | the storage size actually provisioned (from status), or "—" until bound        |
      | persistentvolumeclaims | Storage Class | the storage class named in spec, or "—" when unset                             |
      | persistentvolumes      | Capacity      | the storage size declared in spec, or "—" when absent                          |
      | persistentvolumes      | Reclaim       | the volume's reclaim policy, or "—" when absent                                |
      | persistentvolumes      | Status        | the volume's phase as a status badge                                           |
      | persistentvolumes      | Claim         | "<namespace>/<name>" of the claim bound to it, or "—" when unclaimed           |
      | persistentvolumes      | Storage Class | the storage class named in spec, or "—" when unset                             |
      | storageclasses         | Provisioner   | the class's provisioner                                                        |
      | storageclasses         | Reclaim       | the class's reclaim policy, or "Delete" when unset                             |
      | storageclasses         | Binding       | the class's volume binding mode, or "Immediate" when unset                     |

  @oss @pro
  # source: oss:src/resources.jsx:170,178
  # source: oss:src/ui/shell.jsx:74-88
  # source: pro:src/resources.jsx:189,197
  # source: pro:src/ui/shell.jsx:62-76
  Scenario Outline: Claim and volume phases as badges
    Given a claim or volume in phase "<phase>"
    Then its Status badge reads "<badge>" in the <colour> colour

    Examples:
      | phase     | badge     | colour  |
      | Bound     | Bound     | success |
      | Pending   | Pending   | neutral |
      | Failed    | Failed    | error   |
      | Available | Available | neutral |
      | Released  | Released  | neutral |
      | Lost      | Lost      | neutral |

  @oss @pro
  # source: oss:src/resources.jsx:170,178
  # source: oss:src/ui/shell.jsx:87-88
  # source: pro:src/resources.jsx:189,197
  # source: pro:src/ui/shell.jsx:75-76
  Scenario: A claim or volume with no phase shows a blank badge
    Given a claim or volume that reports no phase
    Then its Status badge is neutral and has no text

  @oss @pro
  # source: oss:src/resources.jsx:182-187
  # source: pro:src/resources.jsx:201-206
  # assumption: no column marks the default storage class; inferred from its absence
  Scenario: The default storage class is not marked in the list
    Given one storage class is annotated as the cluster default
    Then no list column distinguishes it from the others

  @oss @pro
  # source: oss:src/resources.jsx:240-241,254-257
  # source: pro:src/resources.jsx:257-258,270-274
  Scenario: Storage kinds have no actions of their own
    Given the session is offered write controls
    When a storage kind's list is open
    Then Create is offered and a row's detail panel offers Edit and Delete
    And no Scale, Restart or contextual action is offered
