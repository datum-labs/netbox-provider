# Runbook

This runbook covers NetBoxSource conditions that need an operator. The
[design](../architecture/design.md) explains the rules behind them. Examples
use placeholder names; substitute your own namespace and source.

Every condition and record outcome is in the source's status:

```sh
kubectl -n <namespace> get netboxsource <source> -o yaml
```

`status.records` lists at most 100 records, conflicts first. The controller
logs every planned record with the same fields, so a whole plan can be read
from its logs.

## A release is pending

**Signs.** `Applied` and `Ready` are `Unknown` with reason `ReleasePending`,
and `status.pendingReleases` lists one or more records. Each listed record's
row in `status.records` is `Quarantined/NameConflict` with a message saying
its fields are not yet released.

**What happened.** In Create mode, the controller applied a new record, and
another writer created a record with the same name between the controller's
observation and its apply. Server-side apply has no create-only precondition,
so the apply merged into the other writer's record. The controller then tried
to release its fields by applying an empty configuration, and that apply
failed. Until a release succeeds, the record carries the controller's labels,
annotations and any fields only the controller set.

**What the controller does.** It retries the release on every Create-mode
reconcile, about once a minute while a release is pending. Each retry is
preconditioned on the UID recorded in `status.pendingReleases`, so it never
touches a different record that later takes the name. While the entry is
listed, the record is held as `Quarantined/NameConflict`: it is never planned
as `Unchanged` or `Update`, and nothing else is applied to it. The entry is
removed when the release succeeds, when the record is deleted or replaced by
one with another UID, or when the controller no longer owns any field on it.
After that the record is the other writer's and stays
`Quarantined/NameConflict` because it lacks the controller's labels.

In Observe mode the controller makes no writes, so entries wait, still held,
until the source is back in Create mode.

While 100 releases are pending, nothing is created.

**What to do.**

1. Read the last failure for each entry in
   `status.pendingReleases[].message`. Failures that clear on their own, such
   as an unavailable or overloaded API server, need nothing more.
2. If the failure persists, for example a denied `patch`, fix its cause. The
   controller's identity needs `patch` on the record's kind to release it.
3. Do not delete the NetBoxSource's status or edit
   `status.pendingReleases` by hand. Removing an entry before its release
   succeeds lets the next reconcile plan the other writer's record as the
   controller's own.
4. Once the list is empty, decide which record to keep, as for any
   `NameConflict`. The controller never adopts the other writer's record.

The controller records an entry in status immediately after the failed
release. If that status write also fails, the controller logs an error naming
the record's kind, name and UID, and writes nothing more in that reconcile.
The entry is written again with the reconcile's final status update. If both
writes fail, search the logs for `recording a pending release failed` and
release the named record by hand: remove the controller's labels and
annotations prefixed `netbox.miloapis.com/` and any field only the controller
set, using a server-side apply with field manager `netbox-provider` and an
empty configuration preconditioned on the logged UID.

## Cable references are undelivered or Devices stay interim

**Signs.** `CableReferencesDelivered` is `False`, with one of two reasons.

- `UndeliveredReferences`: `status.undeliveredCableReferences` is above zero,
  and the message names the first Cables, Devices and component IDs.
- `InterimDevices`: `status.interimDeviceCount` is above zero, and
  `status.interimDevices` lists the Devices with the Cables they wait for.

**What happened.** Component IDs moved, for example after a template rename
in NetBox (see the
[design](../architecture/design.md#component-ids)). A Device whose Cables must
move with it is applied with an interim that keeps the old IDs, then its
Cables, then the planned Device.

- An interim Device's Cable rewrite did not land: its outcome is unknown, or
  Fleet rejected it and it is quarantined. Until it lands, the Device lists
  each moved, cabled component twice, under the old and the new ID.
- An undelivered reference is a Cable that this plan does not write. It still
  names an ID the Device dropped: the Cable is retained after it left NetBox,
  its medium is no longer reviewed, or another writer owns it.

**What the controller does.** It retries an interim Device's Cable on every
reconcile, and drops the old IDs in the same reconcile that writes the Cable.
It never rewrites a Cable it does not plan, so an undelivered reference stays
until the Cable is fixed.

**What to do.**

1. For an interim Device, read the `status.records` row of each Cable in
   `waitingFor`. Fix what blocks it, such as an apply conflict or a missing
   `patch` permission. Outcomes that are only unknown clear on a later
   reconcile.
2. An interim Device with an empty `waitingFor` had its planned apply fail.
   If its `status.records` row is `Quarantined/Rejected`, and Fleet's message
   says a component's `name` and `type` are required, another writer owns a
   field on the component entry the move drops, such as an `assetTag`. Ask
   that writer to move the field to the new component ID, or release it. The
   next reconcile then removes the old entry. Until then the Device stays
   quarantined and `Ready` is `False`. After the first reconcile it is no
   longer listed as interim, so the interim alert below does not fire for
   it.
3. For an undelivered reference, decide what the Cable should be. Restore it
   in NetBox, or review its medium, so the controller writes it again. If
   another writer owns it, update it there to the component ID the Device
   now has.
4. Do not edit the Device's components by hand. The next Create-mode apply
   restores what NetBox records.

**Suggested alerts.** This repository ships no alert rules. These expressions
use the controller's gauges. Make each window longer than the source's
`spec.interval` (default 30 minutes), so a Device that the next reconcile
completes does not fire:

```promql
# A Device stayed interim across more than one reconcile interval.
min_over_time(netbox_provider_interim_devices[45m]) > 0

# A Cable names a component its Device no longer has.
min_over_time(netbox_provider_undelivered_cable_references[45m]) > 0
```
