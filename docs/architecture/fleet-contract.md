# How netbox-provider uses Fleet

This controller uses Fleet's inventory API exactly as an operator would. Fleet
has no concept of it or of NetBox, and nothing in a Fleet record's spec says
where the record came from. Who wrote a record comes from Milo audit logs, and
when from `metadata.creationTimestamp`.

The schema is Fleet's target inventory schema from
[datum-cloud/fleet#106](https://github.com/datum-cloud/fleet/pull/106) at
`4ceb658cb2849b369fb5eaae07a7acdf85ae73ee`, vendored in
[test/fleet](../../test/fleet/README.md).

## Fleet dependencies

| Change | Status | Why this controller needs it |
| --- | --- | --- |
| [datum-cloud/fleet#105](https://github.com/datum-cloud/fleet/pull/105) | Open | Makes record-authorship metadata optional, so records without it are admitted. |
| [datum-cloud/fleet#106](https://github.com/datum-cloud/fleet/pull/106) | Open | The target schema: removes record-authorship metadata and Fleet's NetBox import support. A Device's identity then needs a serial or system UUID. |
| [datum-cloud/fleet#101](https://github.com/datum-cloud/fleet/pull/101) | Approved, not merged | The create-only importer role below. |

Until #105 is deployed, deployed Fleet requires authorship metadata that this
controller does not send, so creates are rejected. That is a definite
rejection: the record is quarantined with the API message and nothing is
written. Keep sources in `mode: Observe` until then.

## What it writes

Racks, DeviceClaims and Devices with only the physical facts in the
[design](design.md#mapping-netbox-to-fleet). It never writes Facilities,
`reportedPlacement`, `MachineCapacity` or `deviceRef`, and never updates,
patches or deletes a record.

A Device is created only when the mapping says the NetBox serial is a chassis
serial and NetBox has one, because Fleet requires a serial or system UUID.
Otherwise the DeviceClaim, which needs no identity, is still created and the
Device is reported Unresolved. This follows Fleet's guidance to keep the plan
and defer registration when identity is unknown. envtest proves the vendored
CRD rejects such a Device.

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

The controller's Milo identity needs a create-only role in the provider
project. #101 adds `config/components/inventory/roles/importer.yaml` to
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

The role inherits the viewer role, adds `create` on all seven physical kinds,
and grants no update, patch, delete or status verbs. This controller uses only
`create` on Racks, DeviceClaims and Devices; it never creates Facilities, even
though the role would allow it.

This controller's own kinds stay on the cluster it runs on and are not
registered in Milo, so Fleet needs no ProtectedResources for them.
