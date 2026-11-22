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
  project, `get`, `list` and `create` on four Fleet kinds and nothing else in
  Fleet's group. See [config/README.md](../../config/README.md).
- Fleet has no code that refers to this controller, and the controller needs
  no Fleet API change; see [how it uses Fleet](fleet-contract.md). Its only
  Fleet dependency is the create-only importer role.

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
| `devices[]` | Per-device decisions keyed by NetBox ID: `classification` (`Physical` or `Excluded` with a reason), `serialMeaning` (`Chassis`, `ProviderHostID` or `Unknown`), and `placement` (`Facility` or `RackMembership`). |
| `exclusions[]` | Role-slug exclusions with a reason, for example hosted VMs recorded in DCIM. Exclusions win over device decisions. |

CEL enforces the combinations: excluded devices need a reason; physical devices
need a serial meaning and placement; `providerSystem` is required for, and only
for, `ProviderHostID`. Exact rack units are not representable, so an import can
never assert an occupied footprint.

## Reconcile flow

1. **Resolve configuration.** Read the mapping (cached, watched) and the token
   Secret (uncached `get` only, polled every minute, so Secrets are never cached
   cluster-wide). A missing mapping, Secret, or key, or an empty or whitespace
   token, sets `ConfigurationResolved=False` and makes no NetBox request.
2. **Read NetBox.** One sequential, complete read of sites, locations, racks,
   device types and devices. Any failure discards the whole read and sets
   `SourceObserved=Unknown` and `Ready=Unknown`; nothing is planned or written.
3. **Read Fleet.** List Facility, Rack, DeviceClaim and Device in the target
   namespace through an uncached client. Any failure sets `FleetObserved=Unknown`
   and `Ready=Unknown`.
4. **Plan.** `internal/plan.Build` is a pure function of the snapshot, mapping
   and Fleet state. Every candidate is `Create`, `Unchanged`, `Quarantined` or
   `Unresolved`. There is no update, patch, adoption or delete action.
5. **Apply, only in Create mode.** First write status naming the snapshot digest
   and mapping revision that the creates come from. Then create racks, then
   claims, then devices. `AlreadyExists` re-reads and classifies the record,
   never overwrites it. A definite rejection quarantines the record. Any other
   error makes the outcome unknown: `Applied=Unknown`, `Ready=Unknown`, its
   dependents are skipped, and the next reconcile re-observes Fleet before
   retrying. In `Observe` mode, `Applied=False` with reason `ObserveOnly` and a
   count of what would be created.
6. **Summarize.** `Ready=True` only when configuration, NetBox and Fleet were
   all observed completely and nothing is quarantined or unresolved. `Ready`
   describes this import, never the readiness of a Fleet record or of hardware.

Following the template's reconcile-error rules, transient NetBox and Fleet
failures return an error (retried with backoff), configuration the user must fix
is a condition plus a poll or watch, and an invalid URL that passed admission is
a terminal error.

## Mapping NetBox to Fleet

| NetBox | Fleet | Rule |
| --- | --- | --- |
| site, location | existing `Facility` | Only through `facilities[]`. The controller never creates Facilities: premises identity is operator-owned and a site name is not proof of premises. A missing or deleting Facility leaves dependents `Unresolved`. |
| rack | `Rack` | `facilityRef` from the rack's mapped location, else its mapped site. `heightUnits` only when integral and 1 to 100. |
| device | `DeviceClaim` + `Device` | Only devices listed as `Physical`. The claim is `lifecycle: Installation` with `Facility` or `RackMembership` placement and never position, height or face. The device binds to the claim, carries `topologyAuthority: Cable`, the model shape, and the asset tag as `assetTag`. |
| device type | `identity.model`, `manufacturer`, `modelShape` | A device without a model is `Unresolved`; Fleet requires one. |
| serial | depends on `serialMeaning` | `Chassis` with a value: `identity.serial` and `expectedIdentity.serial`, and the Device is created. Otherwise (`ProviderHostID`, `Unknown`, or an empty serial) nothing is written for the serial, the DeviceClaim is created, and the Device is `Unresolved/IdentityUnavailable`: Fleet requires a serial or system UUID for a Device, and identity is never invented. |
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

Cables, power feeds, components, circuits, operational Sites and
`reportedPlacement` are **not imported yet**. Their NetBox records are simply
not read. The Fleet importer's handling of them is the reference for later work.

## Safety rules and how they are enforced

| Rule | Enforcement | Test |
| --- | --- | --- |
| Observe by default; writes need explicit opt-in | CRD default `mode: Observe`; apply only runs in `Create` | `observes only by default`, `defaults to observe-only` |
| Additive only | The reconciler only calls `Create`; tests wrap the Fleet client and fail on any update, patch, delete, or status write | every reconcile spec |
| Records not written by this controller are never adopted | Same name without matching correlation labels is `Quarantined/NameConflict`, even with an identical spec; Flux labels are named in the message | `never adopts an identical record that lacks this controller's labels`, `rejects an existing name from another writer`, `TestSameNameWithoutOurLabelsIsNeverAdopted` |
| Equal records are no-ops; different ones are not last-import-wins | Correlated by labels, then subset comparison of the spec; differences are `Quarantined/ExistingDiffers` | `then is a no-op`, `TestRecordsCorrelatedByLabelsAreUnchanged`, `TestDifferingRecordFromSameSourceIsQuarantined` |
| Identity collisions are quarantined | Serial, system UUID, asset tag, provider host ID, or provider host ID versus serial matching another Device | `quarantines an identity collision`, `TestIdentityCollisionsAreQuarantined` |
| No duplicate of a source already labelled under another name | `Quarantined/SourceAlreadyRecorded` | `TestSourceLabelledUnderAnotherNameIsNotDuplicated` |
| No invented identity | A Device without a chassis serial is `Unresolved/IdentityUnavailable`; its claim is still created | `plans the claim but not the Device`, `TestDeviceWithoutSerialIsUnresolved`, admission spec |
| Claim and device stay together; dependents of blocked records are blocked | Fixed-point propagation in the plan; skipped dependents at apply time | plan and reconcile specs |
| Never delete on omission | No delete code path; missing sources are reported `RetainedMissingSource` | `never deletes records whose source disappears`, `TestOmittedSourceRecordsAreRetained` |
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
- The controller's Milo identity is bound to Fleet's create-only
  `fleet.datumapis.com-inventory-importer` role, scoped to the provider
  project. See [the importer role](fleet-contract.md#the-one-fleet-dependency-the-importer-role).

## Open questions

- Is `go.miloapis.com/netbox-provider` resolvable for a `datum-labs`
  repository, or should the module path follow the repository?
- Should a NetBoxSource accept a custom CA bundle for internally signed NetBox
  certificates?
