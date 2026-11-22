# Working in netbox-provider

Read `README.md`, `docs/architecture/design.md` and
`docs/architecture/fleet-contract.md` before changing behavior. `CLAUDE.md`
holds the template's controller conventions.

## Rules

- Fleet must not know NetBox exists, and this controller needs no Fleet API
  change beyond its target inventory schema and importer role (see
  `docs/architecture/fleet-contract.md`). Write only physical facts in Fleet
  specs; no authorship or source fields. Correlate only through this
  controller's `netbox.miloapis.com/*` labels and annotations. `internal/fleet` stays source-agnostic. Do not edit Fleet from here.
- Never invent identity. A Device carries a reviewed chassis serial, or Fleet's
  explicit unidentified marker with a fixed reason when the source has no
  reviewed serial. Never write a serial or system UUID that was not reviewed,
  never mark a Device that has one, and never write `bmc` on a marked Device.
  A provider host ID is not missing identity: that Device is deferred.
- A record without this controller's labels was not written by it: never adopt
  it, and quarantine it if it conflicts.
- Do not import Fleet's `internal` packages or invent a Fleet module path. Use
  unstructured objects, the `internal/fleet` mirror, and the vendored CRD in
  `test/fleet`. Re-vendor verbatim and update its recorded commit and sha256.
- Every Fleet write is a server-side apply as field manager `netbox-provider`,
  carrying only the spec fields mapped from NetBox and this controller's
  labels and annotations. Never force ownership, never apply to a record this
  controller did not apply before (such as one Flux owns), never send status,
  never use a non-apply patch, update or delete, and never touch
  `MachineCapacity` or `deviceRef`. A change to a field Fleet keeps immutable
  is quarantined, never worked around by deleting and recreating.
- `Observe` stays the default. Writing needs the explicit `mode: Create` opt-in.
- Quarantine conflicts and identity collisions in status instead of
  overwriting. Records missing from NetBox are retained and reported, never
  deleted, retired or freed.
- Failed, partial or uncertain reads and writes are `Unknown`, never `Ready`.
  Retain the snapshot digest and mapping revision in status before any write.
- Never infer readiness, free capacity, rack-unit occupancy, or chassis identity
  from NetBox. Rack units are written only when the mapping asks for
  `RackUnits` and NetBox records a whole-unit position, face and height; cable
  lanes, media and component geometry are never guessed.
- Keep tokens, kubeconfigs, NetBox exports and real inventory out of Git,
  status, logs and errors. Do not keep custom fields, config contexts or
  response bodies. Use synthetic fixtures with reserved example names only: no
  real hostnames, IPs, tokens, sites or environment names.
- Add tests for every behavior change, including envtest coverage for anything
  that changes what reaches Fleet. Run the commands in `README.md#development`,
  including `task validate` for documentation changes.
- Record design decisions and their rationale in `docs/`, and keep implemented
  behavior distinct from proposals.
