# How netbox-provider uses Fleet

This controller uses Fleet's inventory API exactly as an operator would. Fleet
has no concept of it or of NetBox, and nothing in a Fleet record's spec says
where the record came from. Who wrote a record comes from Milo audit logs, and
when from `metadata.creationTimestamp`.

The schema is Fleet's target inventory schema from
[datum-cloud/fleet#106](https://github.com/datum-cloud/fleet/pull/106) at
`5b60656759033b61674ee202a9ad94f4d49f4a64`, vendored in
[test/fleet](../../test/fleet/README.md).

## Fleet dependencies

| Change | Status | Why this controller needs it |
| --- | --- | --- |
| [datum-cloud/fleet#105](https://github.com/datum-cloud/fleet/pull/105) | Merged | Makes record-authorship metadata optional, so records without it are admitted. |
| [datum-cloud/fleet#106](https://github.com/datum-cloud/fleet/pull/106) | Merged | The target schema: removes record-authorship metadata, component source references and Fleet's NetBox import support. A Device's identity needs a serial or system UUID. |
| [datum-cloud/fleet#101](https://github.com/datum-cloud/fleet/pull/101) | Merged | The importer role below. |
| [datum-cloud/fleet#134](https://github.com/datum-cloud/fleet/pull/134) | Merged | Adds `patch` to the importer role on every inventory kind, for server-side apply. |
| [datum-cloud/fleet#110](https://github.com/datum-cloud/fleet/pull/110) | Merged | `identity.unidentified` and `identity.unidentifiedReason`: a Device with no recorded hardware identity. A Fleet deployment without it rejects such Devices, so they are quarantined and their cables blocked. |

A Fleet deployment older than #105 requires authorship metadata that this
controller does not send, so its creates are rejected. That is a definite
rejection: the record is quarantined with the API message and nothing is
written.

## What it writes

Racks, PowerFeeds, DeviceClaims, Devices with their components and internal
mappings, and Cables, with only the physical facts in the
[design](design.md#mapping-netbox-to-fleet). A Device in a role the mapping
lists also gets `spec.bmc.endpoint` from NetBox's out-of-band IP. That copy
is interim: Fleet assigns out-of-band BMC addresses to Networking/IPAM, and
datum-cloud/fleet#132 tracks replacing it with a reference to an IPAM-issued
address.

Every write is a server-side apply as field manager `netbox-provider`, never
forced, holding only those fields and this controller's labels and
annotations (see [Writes](design.md#writes-server-side-apply)). It creates
missing records and updates the fields it owns; other writers' fields are
kept, and a field another writer owns is a quarantined conflict. It never
writes Facilities, Sites, status, `reportedPlacement`, `MachineCapacity` or
`deviceRef`, and never deletes a record.

Fleet requires a Device to have a serial or system UUID, or to say explicitly
that it has no recorded hardware identity. The controller writes the serial
only when the mapping says it is a chassis serial and NetBox has one.
Otherwise it writes `identity.unidentified: true` with one of two fixed reasons
(no serial in the source, or a serial not reviewed as a chassis serial), never
a host name, and never a serial, system UUID or BMC beside it. Its claim
expects no serial. A marked Device can be cabled but never enrolled as
capacity. A serial reviewed as a provider host ID is the provider's record,
not missing identity, so that Device is deferred and only its claim is
created. envtest proves the vendored CRD admits the marker and rejects it with
a serial, without a reason, set false, or with a BMC.

## Correlation is this controller's metadata

Fleet ignores labels and annotations, so the link to NetBox lives there, under
this controller's own `netbox.miloapis.com` prefix:

| Key | Kind | Value |
| --- | --- | --- |
| `netbox.miloapis.com/source` | label | The NetBoxSource's `sourceID` |
| `netbox.miloapis.com/type` | label | The NetBox collection with `/` written as `.`, for example `dcim.devices` |
| `netbox.miloapis.com/id` | label | The NetBox record ID |
| `netbox.miloapis.com/snapshot-digest` | annotation | sha256 of the NetBox read the record came from |
| `netbox.miloapis.com/mapping-revision` | annotation | The NetBoxMapping revision |
| `netbox.miloapis.com/observed-at` | annotation | When that read started |

The three labels identify the NetBox record completely, so no separate
reference string is written. A record without matching labels was not written
by this controller: it is never adopted, and it is quarantined if it
conflicts. Duplicate detection also compares Fleet's own identity fields:
serial, system UUID, asset tag and provider host ID.

Names follow this controller's own rule, not a Fleet convention: `netbox-`
followed by the first 24 hex digits of the sha256 of
`netbox-provider-name/v1`, sourceID, collection, ID and Fleet kind, joined by
newlines. See `internal/plan/correlation.go`.

## The importer role

The controller's Milo identity needs, in the provider project, `get` and
`list` on the physical kinds and `create` and `patch` on the kinds it writes.
Server-side apply needs both: `create` for a record's first apply and `patch`
for later ones. #101 adds `config/components/inventory/roles/importer.yaml` to
`config/milo`:

```yaml
apiVersion: iam.miloapis.com/v1alpha1
kind: Role
metadata:
  name: fleet.datumapis.com-inventory-importer
  annotations:
    kubernetes.io/display-name: Fleet Inventory Importer
    kubernetes.io/description: Create physical inventory records from an external source without changing or deleting existing ones
spec:
  launchStage: Alpha
  inheritedRoles:
    - name: fleet.datumapis.com-inventory-viewer
  includedPermissions:
    - fleet.datumapis.com/sites.create
    - fleet.datumapis.com/deviceclaims.create
    - fleet.datumapis.com/devices.create
    - fleet.datumapis.com/facilities.create
    - fleet.datumapis.com/racks.create
    - fleet.datumapis.com/cables.create
    - fleet.datumapis.com/powerfeeds.create
```

The role inherits the viewer role and adds `create` on all seven physical
kinds. datum-cloud/fleet#134 adds `patch` on every inventory kind, for
server-side apply; the role still grants no `update`, `delete` or status
verbs, so it can add and adjust records but never remove them. The YAML above
is the role as #101 introduced it. This controller uses `create` and `patch`
on Racks, PowerFeeds, DeviceClaims, Devices and Cables only; it never writes
Facilities or Sites, even though the role would allow it.

This controller's own kinds stay on the cluster it runs on and are not
registered in Milo, so Fleet needs no ProtectedResources for them.
