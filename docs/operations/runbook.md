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
