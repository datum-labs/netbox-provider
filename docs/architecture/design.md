# netbox-provider design

`netbox-provider` is a standalone Kubernetes controller that reads physical
inventory from a NetBox instance and creates the matching Fleet inventory
records. It is the only component that knows NetBox exists. Fleet sees
ordinary `Manual` operator records; correlation lives in this controller's own
labels and annotations, which Fleet ignores.

The contracts below are **implemented and tested against a synthetic NetBox and
the vendored Fleet CRD**. They have not run against a real NetBox, a Milo
control plane, or deployed Fleet. Anything described as proposed is not
implemented.

## Sources

| Input | Revision |
| --- | --- |
| Template | `milo-os/controller-template` at `a832499` (kubebuilder v4) |
| Fleet inventory schema | `datum-cloud/fleet` branch `feat/manual-inventory-records` at `919fbd78`, vendored in [test/fleet](../../test/fleet/README.md) |
| Import semantics | Fleet's offline importer `hack/netbox_inventory.py` and `docs/operations/netbox-physical-import.md` at the same revision |
| Earlier scaffold | The read-only Go CLI that previously occupied this directory |

## Boundaries

```text
              controller's own API server                      Milo provider project (Fleet's home)
 ┌────────────────────────────────────────────┐            ┌──────────────────────────────────────┐
 │ NetBoxSource ── tokenSecretRef ─► Secret    │   list     │ Facility (operator-owned, read only) │
 │      │                                      │ ─────────► │ Rack, DeviceClaim, Device            │
 │      └── mappingRef ─► NetBoxMapping        │   create   │   (created only, never changed)      │
 └────────────────────────────────────────────┘ ─────────► └──────────────────────────────────────┘
        │ GET only, bounded, same-origin
        ▼
     NetBox REST API
```

- The controller's own kinds and the NetBox token Secret live on the API server
  named by `kubeconfigPath` (in-cluster by default). Fleet records live on the
  API server named by `fleet.kubeconfigPath`, normally the Milo provider project
  control plane. The token never leaves the controller's own API server, and
  Fleet's project does not need to serve this controller's CRDs.
- The controller never reads or writes `MachineCapacity`, its `deviceRef`, any
  `metal.datumapis.com` resource, networking, IPAM, routing, or service policy.
  Local RBAC is namespaced to `netbox-provider-system` and covers only this
  controller's kinds and Secrets; Fleet access is Milo IAM in the provider
  project: `get` and `list` on Facility, Rack, PowerFeed, Manufacturer,
  DeviceType, DeviceClaim, Device and Cable, `create` and `patch` on all but
  Facility, and nothing else in
  Fleet's group. See [config/README.md](../../config/README.md).
- Fleet has no code that refers to this controller, and the controller needs
  no Fleet API change; see [how it uses Fleet](fleet-contract.md). Its only
  Fleet dependency is a role that grants `create` and `patch` on the kinds it
  writes.

## API: `netbox.miloapis.com/v1alpha1`

The group follows the template's `<name>.miloapis.com` convention. No existing
`milo-os` service uses it; the archived `inventory.miloapis.com` and Fleet's
`fleet.datumapis.com` were avoided. Kind names were chosen to say what they are
for and to avoid the template's `Resource` example and Milo's existing kinds.

### NetBoxSource

One NetBox instance connected to one Fleet namespace.

| Field | Meaning |
| --- | --- |
| `url` | HTTPS installation root, optional path prefix, no `/api`, credentials, query or fragment. CEL-validated and validated again by the reader. |
| `sourceID` | Operator-assigned stable identifier, written as the `netbox.miloapis.com/source` label. Immutable, because every generated name depends on it. Not a hostname. |
| `tokenSecretRef` | `{name, key}`; key defaults to `token`. Same namespace as the source. |
| `authScheme` | `Bearer` (NetBox v2 tokens, default) or `Token` (legacy v1). |
| `scope.siteIDs` | Optional site filter, sent to NetBox and required to survive pagination. |
| `mappingRef` | The `NetBoxMapping` in the same namespace. |
| `target` | `{namespace, writer}`. The namespace is immutable; `writer` accepts only `Operator`. |
| `mode` | `Observe` (default) or `Create`. |
| `interval` | Re-read period, 1m to 24h, default 30m. |
| `suspend` | Stops reads and writes. |

Status holds `ConfigurationResolved`, `SourceObserved`, `FleetObserved`,
`Applied` and `Ready` conditions; the last observation's snapshot digest,
observation time, mapping revision and collection counts; plan counts; and up
to 100 notable records, conflicts first. Status never contains serials, asset
tags, tokens, or response bodies.

### NetBoxMapping

The reviewed interpretation of a source. Nothing is imported without it.

| Field | Meaning |
| --- | --- |
| `revision` | Reviewed mapping identifier, written as the `netbox.miloapis.com/mapping-revision` annotation. |
| `facilities[]` | `{sourceType: Site\|Location, sourceID, facilityRef}`: maps NetBox premises to an **existing** Fleet Facility. A mapped location wins over its site. |
| `devices[]` | Per-device decisions keyed by NetBox ID: `classification` (`Physical` or `Excluded` with a reason), `serialMeaning` (`Chassis`, `ProviderHostID` or `Unknown`), and `placement` (`Facility`, `RackMembership` or `RackUnits`). |
| `cableMedia[]` | `{cableTypes, medium}`: the reviewed medium (`Copper`, `Fiber` or `Unknown`) of data cables by NetBox cable type. |
| `untypedCableMedium` | The medium of data cables with no NetBox cable type. Unset leaves them `Unresolved`. |
| `bmc.roleSlugs` | NetBox device roles whose out-of-band IP is a Redfish BMC. Devices with a chassis serial in these roles get `spec.bmc.endpoint`. Unset writes no BMC. |
| `bmc.vrfIDs` | NetBox VRFs whose out-of-band IPs may be used. The global table is always usable; any other VRF is not. |
| `exclusions[]` | Role-slug exclusions with a reason, for example hosted VMs recorded in DCIM. Exclusions win over device decisions. |

CEL enforces the combinations: excluded devices need a reason; physical devices
need a serial meaning and placement; `providerSystem` is required for, and only
for, `ProviderHostID`. `RackUnits` asserts an occupied footprint only from
NetBox's own whole-unit position, face and device-type height; without all
three the device is `Unresolved`, never placed by inference.

## Reconcile flow

1. **Resolve configuration.** Read the mapping (cached, watched) and the token
   Secret (an uncached `get`). Secrets are watched as metadata only, in the
   manager's namespace, with annotations and labels dropped before caching, so
   no token is ever cached: kubectl's client-side apply copies a Secret's data
   into its `last-applied-configuration` annotation. A change to a Secret
   requeues the sources whose `tokenSecretRef` names it; a change to any other
   Secret requeues nothing. A missing mapping, Secret, or key, or an empty or
   whitespace token, sets `ConfigurationResolved=False`, makes no NetBox
   request, and is re-checked every minute as well as on the watch.
2. **Read NetBox.** One sequential, complete read of sites, locations, racks,
   device types, devices, their components, power panels and feeds, cables,
   and the devices' out-of-band IP records. Each collection read is bounded
   by the source's [read limits](#read-limits). Any failure discards the whole read and sets
   `SourceObserved=Unknown` and `Ready=Unknown`; nothing is planned or written.
   A `401` or `403` is different: retrying the same token cannot succeed, and
   NetBox installations commonly ban addresses after repeated rejections. It
   sets `ConfigurationResolved=False` and `Ready=False` with reason
   `TokenRejected` and is retried after the source's interval or one hour,
   whichever is longer, never through the error backoff. Rotating the token
   takes effect at once: the Secret watch requeues the source, and a token
   that differs from the rejected one is sent without waiting. The rejected
   token is not sent again before its retry time, whatever requeued the
   source, unless the source's spec changed. Only a sha256 of it is kept, in
   memory; it never reaches status or logs, and a restarted controller tries
   it once more.
3. **Read Fleet.** List Facility, Rack, PowerFeed, DeviceClaim, Device and Cable in the target
   namespace through an uncached client. Any failure sets `FleetObserved=Unknown`
   and `Ready=Unknown`.
4. **Retry pending releases, only in Create mode.** Each record in
   `status.pendingReleases` is read and its release is applied again, as
   described in [Writes](#writes-server-side-apply). Those still pending, and
   those released now, are held as `Quarantined/NameConflict` in this plan.
   In `Observe` mode they are held without a retry.
5. **Plan.** `internal/plan.Build` is a pure function of the snapshot, mapping
   and Fleet state. Every candidate is `Create`, `Update`, `Unchanged`,
   `Quarantined` or `Unresolved`. There is no adoption or delete action.
6. **Apply, only in Create mode.** First write status naming the snapshot digest
   and mapping revision the writes come from. Then apply racks, power feeds,
   claims, devices and cables, each after what it references, as described in
   [Writes](#writes-server-side-apply). A definite rejection quarantines the
   record. Any other error makes the outcome unknown: `Applied=Unknown`,
   `Ready=Unknown`, its dependents are skipped, and the next reconcile
   re-observes Fleet before retrying. In `Observe` mode, `Applied=False` with
   reason `ObserveOnly` and counts of what would be created and updated, and
   no write call is made.
7. **Summarize.** `Ready=True` only when configuration, NetBox and Fleet were
   all observed completely and nothing is quarantined or unresolved. `Ready`
   describes this import, never the readiness of a Fleet record or of hardware.

Following the template's reconcile-error rules, transient NetBox and Fleet
failures return an error (retried with backoff from 30 seconds up to 30
minutes), configuration the user must fix
is a condition plus a poll or watch, and an invalid URL that passed admission is
a terminal error.

## Mapping NetBox to Fleet

| NetBox | Fleet | Rule |
| --- | --- | --- |
| site, location | existing `Facility` | Only through `facilities[]`. The controller never creates Facilities: premises identity is operator-owned and a site name is not proof of premises. A missing or deleting Facility leaves dependents `Unresolved`. |
| rack | `Rack` | `facilityRef` from the rack's mapped location, else its mapped site. `heightUnits` only when integral and 1 to 100. |
| device | `DeviceClaim` + `Device` | Only devices listed as `Physical`. The claim is `lifecycle: Installation` with `Facility`, `RackMembership` or `RackUnits` placement; only `RackUnits` writes position, height and face. The device binds to the claim, carries `topologyAuthority: Cable`, the model shape, the asset tag as `assetTag`, and its components. |
| component | `Device.spec.components[]`, `internalMappings[]` | Interfaces, power ports and outlets, console and console server ports, front and rear ports, modules, inventory items and device bays, with the ID of the component template it was made from, or `<collection>-<NetBox ID>` when there is none (see [Component IDs](#component-ids)), and the NetBox port type as `connector`. Virtual, LAG and bridge interfaces are networking, not components. A rear port needs a position count. Each front port position maps to its rear port position (NetBox 4.5 and later list one mapping per position; older NetBox records one), and outlets map to the inlet that feeds them. If any component or path cannot be recorded exactly, or there are more than 256, the Device is `Unresolved/ComponentsInvalid`: a Device's components are written in one apply, so a partial list is never recorded. |
| interface MAC | `Device.spec.interfaces[].macAddress` | The MAC NetBox expects on each physical interface (not virtual, LAG or bridge), as `{name, macAddress}` with the interface's component name. Read from the interface's `primary_mac_address` (NetBox 4.2 and later), else its legacy `mac_address`; no other request is made. Written in Fleet's form, a lowercase colon-separated EUI-48. A MAC is expected evidence for discovery to compare, never identity, so one that cannot be written exactly is left empty, the Device is still written, and its plan message names the interfaces: a value that is not an EUI-48 unicast address (including all zeros and multicast), an interface name over Fleet's 63 characters or shared with another interface, and a MAC NetBox records on more than one interface anywhere in the snapshot, which is left empty on every one. Fleet rejects a MAC recorded twice on a Device, and discovery could not tell which port a shared MAC names. A device with more than Fleet's 32 interfaces with a MAC writes none. No MAC is invented, derived or written when NetBox records none. A MAC another writer recorded in `unmappedMACAddresses`, or on another interface name, makes Fleet reject the apply, which is quarantined with Fleet's message. |
| power feed | `PowerFeed` | `facilityRef` from its power panel's mapped location or site; `rackRef` when NetBox names a rack; supply type, voltage, current and phase; one `output` power outlet termination. A planned feed is excluded. The panel is not written: it has no Fleet kind, and an external reference would be a source field. |
| cable | `Cable` | One lane between two planned endpoints: a component of a created Device or a feed output. An endpoint on a device without a created Device, outside the scope, logical, or a circuit leaves the cable `Unresolved`. Power cables must join an outlet and an inlet and console cables two console components; their medium follows. Data cables take the reviewed medium of their NetBox type. Planned cables are excluded; cables with several terminations per side and multi-position endpoints are not imported, because lanes are never inferred. |
| out-of-band IP | `Device.spec.bmc.endpoint` | **Interim.** Fleet's design assigns out-of-band BMC addresses to Networking/IPAM; datum-cloud/fleet#132 tracks replacing this copied endpoint with a reference to an IPAM-issued address, and datum-cloud/fleet#130 tracks discovering hardware details from the BMC. Only for roles listed in `bmc.roleSlugs` and Devices with a chassis serial, never on an unidentified Device. The address comes from the `ipam/ip-addresses` record, read for its VRF: an address in a VRF the mapping does not list in `bmc.vrfIDs` is never treated as global. Written as `https://<IPv4>:443`, the literal form Fleet's Redfish executor accepts, with NetBox's prefix length dropped. IPv6, `0.0.0.0/8`, `240.0.0.0/4` (including `255.255.255.255`), loopback, link-local and multicast addresses write no BMC, and the Device is still created with the reason in its plan message. A Device whose endpoint matches another Device's, planned or existing from any writer, is `Quarantined/BMCEndpointCollision`, naming both. Endpoints are compared by host and port, with the scheme's default port filled in, IP literals canonicalized (including IPv4-mapped IPv6) and the path ignored. A hostname endpoint on another writer's Device is compared only with the same hostname, never resolved: the planner is a pure function, DNS answers vary between resolvers and over time, and a lookup would make the plan neither reproducible nor authoritative. Quarantining every Device whenever any other writer recorded a hostname would block the namespace's BMC imports on records unrelated to them. This controller itself writes only IP literals. A switch's or PDU's management address is not a BMC, so roles are listed explicitly. |
| device type | `identity.model`, `manufacturer`, `modelShape` | A device without a model is `Unresolved`; Fleet requires one. |
| serial | depends on `serialMeaning` | `Chassis` with a value: `identity.serial` and `expectedIdentity.serial`. `Unknown`, or `Chassis` with an empty serial: no serial is written, and the Device carries Fleet's `identity.unidentified: true` with a fixed reason. `ProviderHostID`: the DeviceClaim is created and the Device is `Unresolved/IdentityUnavailable`. Identity is never invented. |
| record identity | labels `netbox.miloapis.com/{source,type,id}` | Correlation is this controller's metadata, not a Fleet field. See [how it uses Fleet](fleet-contract.md#correlation-is-this-controllers-metadata). |

Fleet records carry no authorship or source fields: who wrote a record comes
from Milo audit logs, and when from `metadata.creationTimestamp`. The link to
NetBox is only this controller's metadata. The three labels identify the
NetBox record; annotations record the snapshot digest, mapping revision and
observation time. No separate reference string is written, because nothing
would read it. Example:

```yaml
metadata:
  name: netbox-d6e24bd6be606a8873be3ef8
  labels:
    netbox.miloapis.com/source: example-netbox
    netbox.miloapis.com/type: dcim.devices
    netbox.miloapis.com/id: "1000"
  annotations:
    netbox.miloapis.com/snapshot-digest: sha256:...
    netbox.miloapis.com/mapping-revision: example-reviewed-1
    netbox.miloapis.com/observed-at: "2026-01-02T03:04:05Z"
spec:
  claimRef: {name: netbox-...}
  identity: {manufacturer: Example Vendor, model: Example 1U Server, serial: SYNTH-CHASSIS-0001}
  assetTag: SYNTH-ASSET-0001
  displayName: example-server-a
  modelShape: {heightUnits: 1, fullDepth: true}
  topologyAuthority: Cable
```

Names follow this controller's own rule: `netbox-` and the first 24 hex digits
of the sha256 of the versioned tuple (sourceID, collection, ID, Fleet kind).
NetBox renames never rename Fleet records.

Circuits, power panels, operational Sites, service associations and
`reportedPlacement` are **not imported**. Fleet's earlier offline importer
(`hack/netbox_inventory.py`, removed by datum-cloud/fleet#106) is the reference
for component IDs, internal mappings and cable endpoints.

### Component IDs

Fleet joins a Device's components to its DeviceType's component templates by
`id`. A component made from a template therefore carries that template's ID,
such as `interface-templates-12`, and the DeviceType's template IDs never
change for it. NetBox records no link from a component to its template; it
makes components from templates by name. So a component takes a template's ID
only when:

- exactly one template on its NetBox device's type has the same NetBox name
  and the same Fleet component type (an interface template for a network port,
  a power port template for a power inlet, and so on);
- no other component of the device matches that same template; and
- no module installed it, since a device type's templates never describe a
  module's components.

Every other component keeps its instance ID, `<collection>-<NetBox ID>`, such
as `interfaces-34`: a component added by hand, one whose name no longer
matches, one a module installed, one matched by more than one template, and
every component that matches a template another component also matches.

IDs stay unique within a Device. A template ID is given to at most one
component. Instance IDs are unique because NetBox IDs are unique within a
collection. A template ID never equals an instance ID, because no template
collection has the name of a component collection, so their
`<collection>-` prefixes differ. Internal mappings and cable endpoints name
the final IDs. Feed endpoints stay `output`.

Matching uses the device type's templates as the snapshot records them, whether
or not the DeviceType is written in the same plan, so a component's ID does not
change when its DeviceType is quarantined. Virtual, LAG and bridge interface
templates are not templates in Fleet and match nothing.

**Moving existing records to template IDs.** Devices applied while every
component had its instance ID are updated to the new IDs, and so are the Cables
that name them. Fleet accepts this: a Device's `components` is a list keyed by
`id`, so applying the new list as the same field manager removes the old
entries, nothing makes components or cable endpoints immutable, and Fleet does
not check a Cable's endpoints against its Devices at admission. Devices are
applied before Cables, so applying the new Device alone would leave its Cables
naming components it no longer has until they are applied. To avoid that, the
Device is applied in two steps within one reconcile:

1. An interim Device with the new components and the old components that
   existing Cables name, with the new internal mappings.
2. The Cables, which move to the new IDs.
3. The planned Device, which removes the old components. It is applied only
   once every Cable it waits for is written.

So no Cable names a component its Device lacks at any point. During the
reconcile, a Device being moved briefly lists both the old and new entries of
its cabled components, so the staff UI can show those ports twice until
step 3. If a Cable it waits for is not written, for example because its outcome
is unknown or it is quarantined, the Device keeps the interim components and
says which Cable it waits for. The next reconcile that writes the Cable
removes them. Only Cable Updates in the same plan are waited for: an existing
Cable this plan does not write, such as one retained after it left NetBox,
cannot move, so the Device's message names it and it is left naming the old
ID. A Device whose interim would exceed Fleet's 256 components is applied in
one step; its Cables then name missing components until they are applied
later in the same reconcile.

After the move, the next reconcile writes nothing: the planned Device and
Cables match what was applied.

**Template edits move component IDs.** Matching is by name, at snapshot time,
and NetBox does not rename a device's components when their template is
renamed. So an edit to a device type's templates changes the IDs of components
that already exist, on the next reconcile:

- Renaming a template, deleting it, or adding a second template with the same
  name and Fleet type makes its components fall back to their instance IDs.
- Undoing that, or adding a template that matches a component that had none,
  moves them to the template ID.
- A component renamed in NetBox to match, or stop matching, a template moves
  the same way.

Each affected Device gets one Update in the plan, with only the moved
components changed. The DeviceType's templates and the Cables that name a
moved component are Updates in the same plan. The move uses the same staged
cutover as the move to template IDs: the Device's interim keeps the old ID
while its Cables move, and the planned Device drops it once they are written.
So within one reconcile the Device is applied twice, the Cable and the
DeviceType once each, and no Cable names a component its Device lacks after
any write. The next reconcile writes nothing. Nothing flips back on later
reconciles: the IDs change again only when the templates or component names
change again in NetBox. The plan test
`TestTemplateChangesMoveComponentIDsInOneDeviceUpdate` and the envtest
`moves one component between its template and instance IDs with one Device
update each way` count these writes in both directions.

A moved component is a new Fleet component entry, because its ID is the
entry's key. Fields another writer set on the old entry, such as an
operator's `assetTag`, are not carried to the new ID. Server-side apply keeps
the old entry with only that writer's fields, and the controller leaves it
alone, as it does any entry only another manager owns.

## Hardware catalog

Fleet's hardware catalog (datum-cloud/fleet#107) records Manufacturers and
DeviceTypes. The controller imports the catalog entries of the devices it
imports, never the whole NetBox catalog: a device type is imported only when
a device in the source's scope uses it and the mapping classifies that device
as `Physical`, and a manufacturer only when one of those types names it. Their
manufacturers and component templates are read by ID for those types, so
the read stays bounded by the source's scope. There is no whole-catalog
option.

| NetBox | Fleet | Rule |
| --- | --- | --- |
| manufacturer | `Manufacturer` | `canonicalName` and `displayName` are NetBox's name. A name Fleet would reject (not printable ASCII, longer than 253 characters, starting with something other than a letter or digit, or ending in a space) is `Unresolved/CanonicalNameInvalid`; it is never altered to fit. `aliases` are not written: NetBox has no reviewed source for them. Two NetBox manufacturers with the same name ignoring case are both `Quarantined/CanonicalNameCollision`. |
| device type | `DeviceType` | `manufacturerRef` names the controller's Manufacturer. `model` is the model key below. `displayName` is NetBox's model. `modelShape` comes from `u_height` and `is_full_depth`, each left out when NetBox records none. Two types of one manufacturer with the same key are both `Quarantined/ModelKeyCollision`, naming them. A type whose manufacturer or model changes in NetBox is `Quarantined/ImmutableChanged`: Fleet keeps both immutable. |
| component templates | `DeviceType.spec.componentTemplates[]` | Interface, power port and outlet, console and console server port, front and rear port, module bay, device bay and inventory item templates, with the same type and connector vocabulary as installed components and IDs `<collection>-<NetBox ID>`, such as `interface-templates-12`. Virtual, LAG and bridge interface templates are not components. A rear port template needs a position count. If any template cannot be recorded exactly, or there are more than 256, the type is `Unresolved/TemplatesInvalid`. |
| device's type | `Device.spec.deviceTypeRef` | Set on every created Device, identified or unidentified, whose type is written in the same plan. See below. |

**Model key.** The key is the type's NetBox slug, or its model when it has no
slug, lowercased, with every run of characters other than ASCII letters and
digits replaced by one hyphen, and hyphens trimmed. `example_1u` and
`Example 1U` both become `example-1u`. Non-ASCII letters are separators, so
they never produce characters Fleet rejects. A type whose key is empty or
longer than 253 characters is `Unresolved/ModelKeyInvalid`.

**`deviceTypeRef`.** Fleet lets a Device gain a reference but never lose or
change one.
- A Device whose type is not written (unresolved, quarantined, or its
  manufacturer is) is still created, without a reference. That is safe:
  the reference can be added by a later `Update` once the type is written,
  and it never blocks the Device's own record.
- A Device that already has a reference keeps it in every apply, even when its
  type stops being written, since removing it would be rejected.
- A Device whose NetBox type changes keeps its reference and is
  `Quarantined/ImmutableChanged`: the controller never rebinds it. A type
  change that also changes the Device's manufacturer or model is quarantined
  for the identity change first.
- Records are applied Manufacturers first, then DeviceTypes, then Devices.

**Other writers.** Fleet cannot enforce uniqueness across objects, so the
controller checks, on every reconcile, whether or not its own record already
exists. A Manufacturer from another writer whose canonical name or one of
whose aliases equals the controller's canonical name ignoring case, or a
DeviceType from another writer for the same manufacturer (matched the same
way) and model key, with both models normalized by the same model-key rule,
quarantines the controller's record as `Quarantined/CatalogDuplicate`, naming
both records, so a human decides which to keep. Nothing more is applied to the
controller's record while it is quarantined, a Device keeps a reference it
already has, and neither record is changed or deleted. Two NetBox
manufacturers whose names differ only in case are both
`Quarantined/CanonicalNameCollision`.

## Writes: server-side apply

Every Fleet write is a server-side apply as field manager `netbox-provider`,
never forced. The applied configuration holds only the spec fields mapped from
NetBox and this controller's `netbox.miloapis.com/*` labels and annotations,
so `managedFields` records exactly those as owned by `netbox-provider`. Status
is never sent, and nothing else is: other writers, such as an operator or a
future collector writing device status (datum-cloud/fleet#130), keep their
fields.

A Device's spec and status have different owners. `netbox-provider` owns the
spec fields it maps: what NetBox expects, such as identity, components, the
BMC endpoint and expected MACs. Fleet discovery owns `Device.status`: what the
BMC reports, compared against that spec. Applies go to the main resource and
never carry `status`, so with Fleet's status subresource the API server keeps
status, and its managed fields, exactly as its writer left them. Drift
compares only `spec` and labels, so a status written by anyone is never a
reason to apply. System UUIDs and TLS pins are discovery's to report in
status; this controller never writes them to spec. envtest shows a status
another manager applied is unchanged byte for byte by a create that lands on
its Device, by a spec update, and by a no-op reconcile.

| Existing record | Outcome |
| --- | --- |
| None | `Create`: the first apply creates it. Server-side apply has no create-only precondition, so if another writer creates the name between observation and apply, the apply merges into that record. The controller detects this from the other manager in the response, immediately applies an empty configuration preconditioned on that record's UID, which removes every field only it owned (its labels, annotations and any spec field the other writer did not set), and quarantines it as `NameConflict`. Fields the other writer set identically stay theirs. If that release fails, the record still carries this controller's labels and ownership, so the controller records it in `status.pendingReleases` with its UID before any further write, and every later Create-mode reconcile retries the release and holds the record as `Quarantined/NameConflict`, never `Unchanged` or `Update`, until it succeeds. Meanwhile `Applied` and `Ready` are `Unknown` with reason `ReleasePending`; see the [runbook](../operations/runbook.md#a-release-is-pending). A conflicting field fails the apply before anything is written. |
| Same name without this source's labels | `Quarantined/NameConflict`; never written. |
| This source's labels, but not applied by `netbox-provider` | `Quarantined/NotApplyOwned`; never adopted. |
| Applied by `netbox-provider`, and an immutable field would change | `Quarantined/ImmutableChanged`; no call is made. A Device's `identity` and `claimRef`, and a claim's `placement` and `lifecycle`, are immutable in Fleet; for example, "hardware identity changed in NetBox; ... a hardware swap is a new Device". A record is never deleted and recreated. Other rejections Fleet makes are quarantined with its message. |
| Applied by `netbox-provider`, last applied digest and values equal | `Unchanged`; nothing is sent. |
| Applied by `netbox-provider`, otherwise | `Update`: the plan is re-applied, preconditioned on the observed UID. A record deleted or replaced since observation fails that precondition, nothing is written, and the outcome is `Unknown` until the next observation. A field another manager now owns is an apply conflict: `Quarantined/ApplyConflict`, naming the manager and field paths. |

A claim and its Device move together: if either is blocked, the other is not
written either.

**Steady state makes no apply calls.** Each written record carries the
`netbox.miloapis.com/applied-digest` annotation, the sha256 of the labels and
spec last applied. A record is `Unchanged`, and is not sent, when that digest
matches the plan's and every planned spec value is present and equal. The
digest catches fields this controller stopped mapping, whose removal needs an
apply; the value check catches drift. Applying anyway would be a no-op, but it
would cost one request per record each interval and refresh the observation
annotations, which would then no longer say which observation the owned
fields came from. A field another writer forced away is seen as drift, and its
re-apply conflicts and is quarantined.

**Other managers' fields and items are not drift.** The value check ignores
what other managers added, because server-side apply keeps it through every
apply this controller makes. Objects compare field by field. Lists that Fleet's
CRD declares as map-lists (`x-kubernetes-list-type: map`, listed with their
keys in `fleet.MapLists`) compare item by item by their keys, in any order.
Each item this controller plans must exist with the same key, and its planned
fields must be present and equal. Extra fields on that item, such as a layout
writer's `face` and `layout` on a DeviceType's component templates or an
operator's `assetTag` on a Device component, are other managers' fields.
Atomic lists, sets and scalars must still be equal.

A live item the plan does not name is drift only if this controller owns it,
read from the `k:{...}` entries of its server-side apply in the record's
managed fields. An item only other managers own is theirs and is never
compared. An item this controller owns is drift, because applying without it
is how a removal in NetBox reaches Fleet. The applied digest alone would not
catch every such item: a Device's interim apply during the move to template
IDs (see **Migration** below) carries the planned digest while it keeps old
components, which must still go once their Cables move. When this
controller's managed fields are missing or unreadable, ownership is unknown
and every extra item is drift, since re-applying is always safe. A test checks
`fleet.MapLists` against the vendored CRD, so a new map-list in Fleet's schema
fails the suite until it is listed.

**Migration.** Because owned fields follow NetBox, a record this controller
applied gains what later versions map:

- Devices applied before components, internal mappings, BMC endpoints or
  expected MACs were imported get them through an `Update`, and the cables to
  their new components are then created.
- Devices applied while every component had its instance ID move to template
  IDs, with their Cables, as described in
  [Component IDs](#component-ids).
- Turning on `bmc` for a mapping whose Devices already exist, or fixing an
  address later, updates those Devices' `spec.bmc`, which Fleet does not keep
  immutable. This applies only to Devices `netbox-provider` applied: a Device
  another writer applied stays `Quarantined/NotApplyOwned` and never gets a
  BMC endpoint from this controller.
- Records written before writes were server-side applies (by `Create`) are not
  owned by `netbox-provider`, and stay `Quarantined/NotApplyOwned` until an
  operator removes them or transfers their ownership.

## Read limits

A NetBoxSource's `spec.readLimits` bounds each collection read. A read that
exceeds a bound fails as a whole, and nothing is planned from it.

| Field | Default | Bounds |
| --- | --- | --- |
| `maxRecordsPerCollection` | 20000 | 1 to 1000000. The page bound scales with it at 200 records per page, never below 1000 pages. |
| `maxMiBPerCollection` | 64 | 1 to 4096. Each page is still bounded at 4 MiB. |
| `collectionTimeout` | 2m | 10s to 30m, for each collection. Each request is also bounded at 30 seconds. |

The timeout bounds each collection, not the whole snapshot, so a NetBox with
many large collections can complete a snapshot that would not fit in one
fixed budget.

## Safety rules and how they are enforced

| Rule | Enforcement | Test |
| --- | --- | --- |
| Observe by default; writes need explicit opt-in | CRD default `mode: Observe`; apply only runs in `Create` | `observes only by default`, `defaults to observe-only` |
| Writes are non-forced server-side applies | The reconciler only calls `Apply` with field manager `netbox-provider`; tests wrap the Fleet client and fail on create, update, non-apply patch, delete, any status write, a forced apply, or another field manager | every reconcile spec |
| Another writer's fields are kept, never taken | An apply conflict is `Quarantined/ApplyConflict`, naming the manager and field paths; other managers' fields survive applies | `keeps another manager's field`, `quarantines a conflict with another manager` |
| Other managers' fields and items on map-lists are not drift | Map-list items compare by key; an unplanned item is drift only when this controller's managed fields own it | `TestOtherWritersFieldsOnMapListItemsAreUnchanged`, `TestOwnedFieldOnAMapListItemStillUpdatesBesideOtherWritersFields`, `TestUnwantedItemThisControllerOwnsUpdatesButAnotherWritersItemDoesNot`, `TestExtraItemsCountAsDriftWhenOwnershipIsUnknown`, `TestAtomicListsStayStrict`, `keeps the old components while a Cable that names them is not written`, `TestMapListsMatchTheVendoredCRD`, `are not drift: a reconcile after another manager adds layout fields writes nothing and keeps them`, `still removes an item NetBox dropped, and keeps an item only another manager owns` |
| Immutable fields stay immutable | A Device's identity or claim, or a claim's placement or lifecycle, that changes in NetBox is `Quarantined/ImmutableChanged` before any call | `quarantines a change to an immutable field`, `TestImmutableChangesAreQuarantined` |
| Catalog records are never duplicated or rebound | A Manufacturer or DeviceType that another writer's record already describes is `Quarantined/CatalogDuplicate`; colliding model keys and canonical names are quarantined; a Device keeps its `deviceTypeRef` and is quarantined if NetBox moves it to another type | `TestOperatorWrittenCatalogRecordsAreNeverAdopted`, `TestDuplicatesAppearingAfterOursQuarantineOurs`, `TestManufacturersDifferingOnlyInCaseAreQuarantined`, `TestModelKeyCollisionsAreQuarantined`, `TestImmutableCatalogChangesAreQuarantined`, `TestDeviceTypeRefsAreAddedLaterAndNeverRemoved`, `adds deviceTypeRef to Devices it applied` |
| A create that lands on another writer's record is undone | The controller releases its fields at once; a failed release is recorded in `status.pendingReleases`, retried on every Create-mode reconcile, and its record stays `Quarantined/NameConflict` with `Ready=Unknown` until the release succeeds | `releases every field it applied to a record another writer created, and adopts nothing`, `retries a failed release on later reconciles and holds the record until it succeeds` |
| Status is never written and never drift | Applies never carry `status` and go only to the main resource; drift compares only spec and labels | `is kept byte for byte, and is not drift, on a no-op reconcile`, `is kept byte for byte when the Device's spec is updated`, `is kept byte for byte when a create lands on a Device another writer made`, `TestStatusAnotherWriterOwnsIsNeverDrift` |
| Expected MACs are never ambiguous or invented | Only NetBox's MAC on a physical interface, normalized; an invalid, oversized or repeated MAC is left empty and noted | `TestExpectedMACs`, `TestMoreMACsThanFleetAllowsWritesNone`, `TestExpectedMACsFollowNetBox`, `TestInterfaceMACPrefersThePrimaryMACAddress`, `are written in Fleet's form, and a MAC NetBox records twice is removed from both interfaces` |
| Credential custom fields never cross | Custom fields are not decoded; nothing logs raw records | `never reach the snapshot, its digest input, the plan, Fleet, status or logs`, `TestSnapshotAllowlistsFieldsAndIsStable` |
| No two Devices share a BMC endpoint | A planned endpoint equal to another planned or existing Device's, from any writer, quarantines it | `TestDevicesNeverShareABMCEndpoint` |
| Records not written by this controller are never adopted | Same name without matching correlation labels is `Quarantined/NameConflict`, even with an identical spec; Flux labels are named in the message | `never adopts an identical record that lacks this controller's labels`, `rejects an existing name from another writer`, `TestSameNameWithoutOurLabelsIsNeverAdopted` |
| Equal records are no-ops; owned fields follow NetBox | Correlated by labels and this controller's apply ownership; unchanged digest and values send nothing, differences are `Update`. Records it did not apply are `Quarantined/NotApplyOwned` | `then is a no-op`, `TestOwnedFieldChangeIsUpdated`, `TestRecordNotAppliedByThisControllerIsNeverAdopted` |
| A template edit moves component IDs once, with no dangling Cable | One Device Update per affected Device, staged through its interim; its Cables and DeviceType are updated in the same plan; the next plan writes nothing | `TestTemplateChangesMoveComponentIDsInOneDeviceUpdate`, `moves one component between its template and instance IDs with one Device update each way` |
| Identity collisions are quarantined | Serial, system UUID, asset tag, provider host ID, or provider host ID versus serial matching another Device | `quarantines an identity collision`, `TestIdentityCollisionsAreQuarantined` |
| No duplicate of a source already labelled under another name | `Quarantined/SourceAlreadyRecorded` | `TestSourceLabelledUnderAnotherNameIsNotDuplicated` |
| No invented identity | A Device without a reviewed chassis serial carries the unidentified marker, never a serial; a provider-hosted server is `Unresolved/IdentityUnavailable` with its claim still created | `records a device without a chassis serial as unidentified`, `TestDeviceWithoutSerialIsRecordedAsUnidentified`, `admits the unidentified marker only without serial, system UUID or BMC` |
| Claim and device stay together; dependents of blocked records are blocked | Fixed-point propagation in the plan; skipped dependents at apply time | plan and reconcile specs |
| Never delete on omission | No delete code path; missing sources are reported `RetainedMissingSource` | `never deletes records whose source disappears`, `TestOmittedSourceRecordsAreRetained` |
| A rejected token is not retried quickly | `401`/`403` is `TokenRejected` with a requeue of at least an hour; status writes do not trigger reconciles; a Secret event that leaves the token unchanged does not resend it | `does not retry a rejected token for at least an hour`, `is not sent again for the same generation until its retry time, and a new token is`, `does not resend a rejected token when the Secret changes but its token does not`, `TestErrorsNeverReturnPartialRecordsOrBodies` |
| A rotated token takes effect at once, and tokens are never cached | Secrets are watched as metadata only, without annotations, and requeue only the sources that name them | `picks up a rotated token without a spec edit or waiting for the interval`, `enqueues nothing for a Secret no source references, and the source for its own Secret`, `caches Secret metadata only, without annotations or data` |
| Uncertainty is Unknown | NetBox, Fleet and ambiguous create failures set Unknown conditions | `reports NetBox failures as Unknown`, `reports Fleet read failures as Unknown` |
| Generated records pass Fleet admission | envtest installs the vendored Fleet CRD and creates every fixture candidate | `admits every candidate the fixture plan creates` |

Deleting a NetBoxSource leaves every Fleet record in place; there is no
finalizer. Two sources must not share a `sourceID` unless they read the same
NetBox instance: same names with different content are quarantined, not merged.

## NetBox reader

Salvaged from the earlier scaffold and generalized to scoped collections:

- GET only, Authorization header only, `Accept: application/json`; redirects
  refused; no cookie jar; 30 second request timeout; 2 minute read timeout;
  TLS verification never disabled.
- Installation roots may carry a path prefix; `/api` suffixes, credentials,
  queries, fragments, encoded paths and `..` are refused.
- Offset pagination with `limit=200&ordering=id`. Each `next` link must keep
  the scheme, host and exact collection path; may only carry `limit`, `offset`,
  `ordering=id`, and the scope filter; and must keep the scope filter unchanged.
- Per collection: 4 MiB per page, 64 MiB total, 1,000 pages, 20,000 records.
  Pagination cycles, duplicate or non-positive IDs, an empty non-final page, a
  changing `count`, or a total that differs from `count` fail the collection.
- Only allowlisted fields are decoded; custom fields, config contexts,
  comments, URLs and every other field are dropped at decode time. Errors carry
  no response bodies or tokens.
- Custom fields are never requested, decoded, kept, logged or put in the
  snapshot digest. NetBox returns them on every device whether asked or not,
  and they can hold credentials, such as a BMC password in an `oob_password`
  field. The typed decode has no field for them, so they are discarded with
  the raw record, and nothing logs a raw record or response body. A test
  serves a synthetic `oob_password` and checks that it reaches none of the
  decoded snapshot, the digest input, the plan, the Fleet records, the
  NetBoxSource status or the controller's logs.
- No automatic retry inside a read; the controller's backoff retries the whole
  read.

A snapshot is a sequential read, not a transactional NetBox snapshot. Its
digest is the sha256 of the canonical JSON of the retained fields, so a re-read
of unchanged data yields the same digest.

## Consuming Fleet types

Fleet's Go types are in `internal/api/inventory/v1alpha1` and cannot be
imported, and Fleet publishes no API module. Inventing a module path for Fleet
was ruled out. The controller instead:

- uses `unstructured` objects for every Fleet API call;
- keeps a minimal mirror of only the fields it writes or compares in
  `internal/fleet`, with no NetBox knowledge; and
- vendors Fleet's CRD verbatim in `test/fleet/crds`, with its source commit and
  sha256 recorded and checked by a test, so envtest proves admission.

Drift is caught in two ways: a mirror field that Fleet's schema rejects fails
the admission spec, and a field the server would prune fails the
stored-equals-candidate comparison in the same spec. If Fleet later publishes a
separate API module, `internal/fleet` can be replaced by it without changing
the plan logic.

## Configuration

`NetBoxSourceOperator` keeps the template's metrics and webhook server settings
and adds:

```yaml
fleet:
  kubeconfigPath: ""         # Milo provider project control plane
```

## Template decisions

Generated with the template's `hack/rename.sh --service-name netbox-provider
--api-group netbox.miloapis.com --kind NetBoxSource`, then:

- Removed the Remix UI, its tasks and image publishing; the example `Resource`
  kind and controller; webhooks, cert-manager, and the webhook Service (CEL
  covers validation); the one-shot `hack/rename.sh` and the `.claude`
  initialization hook, whose marker the template's `.gitignore` excludes so
  every clone would re-prompt; and `CODEOWNERS`, whose `@milo-os/platform`
  team does not apply to a `datum-labs` repository.
- Repaired rename fallout: the script's lowercase `resource` substitution
  rewrote every kustomize `resources:` key and RBAC resource list.
- Applied the Fleet defaults from `SetDefaults_NetBoxSourceOperator`, because the
  template's `task generate` never regenerates `zz_generated.defaults.go` (its
  package lacks a `+k8s:defaulter-gen` tag) and only the checked-in file runs.
- Kept the template's module path rule (`go.miloapis.com/netbox-provider`),
  Taskfile, lint configuration, instrumentation adoption test, and `ci-basic`
  workflow. Images and bundles publish to `ghcr.io/datum-labs`.
- Replaced the template's deployment tree (PDB, metrics auth roles, webhook
  Service, controller-gen ClusterRole, dev overlay, samples) with Fleet's
  layout: `config/base/{crd,manager}`, `config/overlays/management-plane`,
  `config/standalone`, `build/manager.Dockerfile`, and Fleet's bundle
  publishing. Differences are recorded in [config/README.md](../../config/README.md).
- `task standalone:deploy` installs the vendored Fleet CRDs into the local kind
  cluster before the standalone root.

## Decisions

- One deployment writes to one Milo provider project. `fleet.kubeconfigPath`
  is set per deployment, not per NetBoxSource. Serving another provider project
  means another deployment. Revisit only when one deployment needs to serve
  several projects.
- The controller's Milo identity is bound to a role with `get` and `list` on
  the physical kinds and `create` and `patch` on the kinds it writes, scoped to
  the provider project: Fleet's `fleet.datumapis.com-inventory-importer` role,
  which grants `create` and `patch` since datum-cloud/fleet#134; see
  [the importer role](fleet-contract.md#the-importer-role).

## Open questions

- Is `go.miloapis.com/netbox-provider` resolvable for a `datum-labs`
  repository, or should the module path follow the repository?
- Should a NetBoxSource accept a custom CA bundle for internally signed NetBox
  certificates?
