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
  project: `get` and `list` on Facility, Rack, PowerFeed, DeviceClaim, Device
  and Cable, `create` on all but Facility, and nothing else in
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
   Secret (uncached `get` only, polled every minute, so Secrets are never cached
   cluster-wide). A missing mapping, Secret, or key, or an empty or whitespace
   token, sets `ConfigurationResolved=False` and makes no NetBox request.
2. **Read NetBox.** One sequential, complete read of sites, locations, racks,
   device types, devices, their components, power panels and feeds, cables,
   and the devices' out-of-band IP records. Each collection read is bounded
   by the source's [read limits](#read-limits). Any failure discards the whole read and sets
   `SourceObserved=Unknown` and `Ready=Unknown`; nothing is planned or written.
   A `401` or `403` is different: retrying the same token cannot succeed, and
   NetBox installations commonly ban addresses after repeated rejections. It
   sets `ConfigurationResolved=False` and `Ready=False` with reason
   `TokenRejected` and is retried after the source's interval or one hour,
   whichever is longer, never through the error backoff.
3. **Read Fleet.** List Facility, Rack, PowerFeed, DeviceClaim, Device and Cable in the target
   namespace through an uncached client. Any failure sets `FleetObserved=Unknown`
   and `Ready=Unknown`.
4. **Plan.** `internal/plan.Build` is a pure function of the snapshot, mapping
   and Fleet state. Every candidate is `Create`, `Update`, `Unchanged`,
   `Quarantined` or `Unresolved`. There is no adoption or delete action.
5. **Apply, only in Create mode.** First write status naming the snapshot digest
   and mapping revision the writes come from. Then apply racks, power feeds,
   claims, devices and cables, each after what it references, as described in
   [Writes](#writes-server-side-apply). A definite rejection quarantines the
   record. Any other error makes the outcome unknown: `Applied=Unknown`,
   `Ready=Unknown`, its dependents are skipped, and the next reconcile
   re-observes Fleet before retrying. In `Observe` mode, `Applied=False` with
   reason `ObserveOnly` and counts of what would be created and updated, and
   no write call is made.
6. **Summarize.** `Ready=True` only when configuration, NetBox and Fleet were
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
| component | `Device.spec.components[]`, `internalMappings[]` | Interfaces, power ports and outlets, console and console server ports, front and rear ports, modules, inventory items and device bays, with ID `<collection>-<NetBox ID>` and the NetBox port type as `connector`. Virtual, LAG and bridge interfaces are networking, not components. A rear port needs a position count. Each front port position maps to its rear port position (NetBox 4.5 and later list one mapping per position; older NetBox records one), and outlets map to the inlet that feeds them. If any component or path cannot be recorded exactly, or there are more than 256, the Device is `Unresolved/ComponentsInvalid`: a Device's components are written in one apply, so a partial list is never recorded. |
| power feed | `PowerFeed` | `facilityRef` from its power panel's mapped location or site; `rackRef` when NetBox names a rack; supply type, voltage, current and phase; one `output` power outlet termination. A planned feed is excluded. The panel is not written: it has no Fleet kind, and an external reference would be a source field. |
| cable | `Cable` | One lane between two planned endpoints: a component of a created Device or a feed output. An endpoint on a device without a created Device, outside the scope, logical, or a circuit leaves the cable `Unresolved`. Power cables must join an outlet and an inlet and console cables two console components; their medium follows. Data cables take the reviewed medium of their NetBox type. Planned cables are excluded; cables with several terminations per side and multi-position endpoints are not imported, because lanes are never inferred. |
| out-of-band IP | `Device.spec.bmc.endpoint` | **Interim.** Fleet's design assigns out-of-band BMC addresses to Networking/IPAM; datum-cloud/fleet#132 tracks replacing this copied endpoint with a reference to an IPAM-issued address, and datum-cloud/fleet#130 tracks discovering hardware details from the BMC. Only for roles listed in `bmc.roleSlugs` and Devices with a chassis serial, never on an unidentified Device. The address comes from the `ipam/ip-addresses` record, read for its VRF: an address in a VRF the mapping does not list in `bmc.vrfIDs` is never treated as global. Written as `https://<IPv4>:443`, the literal form Fleet's Redfish executor accepts, with NetBox's prefix length dropped. IPv6, `0.0.0.0/8`, `240.0.0.0/4` (including `255.255.255.255`), loopback, link-local and multicast addresses write no BMC, and the Device is still created with the reason in its plan message. A Device whose endpoint matches another Device's, planned or existing from any writer, is `Quarantined/BMCEndpointCollision`, naming both. A switch's or PDU's management address is not a BMC, so roles are listed explicitly. |
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

## Writes: server-side apply

Every Fleet write is a server-side apply as field manager `netbox-provider`,
never forced. The applied configuration holds only the spec fields mapped from
NetBox and this controller's `netbox.miloapis.com/*` labels and annotations,
so `managedFields` records exactly those as owned by `netbox-provider`. Status
is never sent, and nothing else is: other writers, such as an operator or a
future collector writing device status (datum-cloud/fleet#130), keep their
fields.

| Existing record | Outcome |
| --- | --- |
| None | `Create`: the first apply creates it. If another manager turns out to have created the name between observation and apply, the record is quarantined as `NameConflict`, never treated as this controller's. |
| Same name without this source's labels | `Quarantined/NameConflict`; never written. |
| This source's labels, but not applied by `netbox-provider` | `Quarantined/NotApplyOwned`; never adopted. |
| Applied by `netbox-provider`, and an immutable field would change | `Quarantined/ImmutableChanged`; no call is made. A Device's `identity` and `claimRef`, and a claim's `placement` and `lifecycle`, are immutable in Fleet; for example, "hardware identity changed in NetBox; ... a hardware swap is a new Device". A record is never deleted and recreated. Other rejections Fleet makes are quarantined with its message. |
| Applied by `netbox-provider`, last applied digest and values equal | `Unchanged`; nothing is sent. |
| Applied by `netbox-provider`, otherwise | `Update`: the plan is re-applied, preconditioned on the observed UID. A field another manager now owns is an apply conflict: `Quarantined/ApplyConflict`, naming the manager and field paths. |

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

**Migration.** Because owned fields follow NetBox, a record this controller
applied gains what later versions map:

- Devices applied before components, internal mappings or BMC endpoints were
  imported get them through an `Update`, and the cables to their new
  components are then created.
- Turning on `bmc` for a mapping whose Devices already exist, or fixing an
  address later, updates those Devices' `spec.bmc`, which Fleet does not keep
  immutable.
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
| Immutable fields stay immutable | A Device's identity or claim, or a claim's placement or lifecycle, that changes in NetBox is `Quarantined/ImmutableChanged` before any call | `quarantines a change to an immutable field`, `TestImmutableChangesAreQuarantined` |
| No two Devices share a BMC endpoint | A planned endpoint equal to another planned or existing Device's, from any writer, quarantines it | `TestDevicesNeverShareABMCEndpoint` |
| Records not written by this controller are never adopted | Same name without matching correlation labels is `Quarantined/NameConflict`, even with an identical spec; Flux labels are named in the message | `never adopts an identical record that lacks this controller's labels`, `rejects an existing name from another writer`, `TestSameNameWithoutOurLabelsIsNeverAdopted` |
| Equal records are no-ops; owned fields follow NetBox | Correlated by labels and this controller's apply ownership; unchanged digest and values send nothing, differences are `Update`. Records it did not apply are `Quarantined/NotApplyOwned` | `then is a no-op`, `TestOwnedFieldChangeIsUpdated`, `TestRecordNotAppliedByThisControllerIsNeverAdopted` |
| Identity collisions are quarantined | Serial, system UUID, asset tag, provider host ID, or provider host ID versus serial matching another Device | `quarantines an identity collision`, `TestIdentityCollisionsAreQuarantined` |
| No duplicate of a source already labelled under another name | `Quarantined/SourceAlreadyRecorded` | `TestSourceLabelledUnderAnotherNameIsNotDuplicated` |
| No invented identity | A Device without a reviewed chassis serial carries the unidentified marker, never a serial; a provider-hosted server is `Unresolved/IdentityUnavailable` with its claim still created | `records a device without a chassis serial as unidentified`, `TestDeviceWithoutSerialIsRecordedAsUnidentified`, `admits the unidentified marker only without serial, system UUID or BMC` |
| Claim and device stay together; dependents of blocked records are blocked | Fixed-point propagation in the plan; skipped dependents at apply time | plan and reconcile specs |
| Never delete on omission | No delete code path; missing sources are reported `RetainedMissingSource` | `never deletes records whose source disappears`, `TestOmittedSourceRecordsAreRetained` |
| A rejected token is not retried quickly | `401`/`403` is `TokenRejected` with a requeue of at least an hour; status writes do not trigger reconciles | `does not retry a rejected token for at least an hour`, `TestErrorsNeverReturnPartialRecordsOrBodies` |
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
  the provider project. Fleet's `fleet.datumapis.com-inventory-importer` role
  grants only `create`; see [the importer role](fleet-contract.md#the-importer-role).

## Open questions

- Is `go.miloapis.com/netbox-provider` resolvable for a `datum-labs`
  repository, or should the module path follow the repository?
- Should a NetBoxSource accept a custom CA bundle for internally signed NetBox
  certificates?
