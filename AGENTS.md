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
- Never invent identity. A Device needs a reviewed chassis serial; otherwise
  only its DeviceClaim is created and the Device is reported Unresolved.
- A record without this controller's labels was not written by it: never adopt
  it, and quarantine it if it conflicts.
- Do not import Fleet's `internal` packages or invent a Fleet module path. Use
  unstructured objects, the `internal/fleet` mirror, and the vendored CRD in
  `test/fleet`. Re-vendor verbatim and update its recorded commit and sha256.
- Writes are additive. The reconciler may only `Create` Fleet records. Never
  update, patch, adopt, or delete one, never take over records another writer
  owns (such as Flux), and never touch `MachineCapacity` or `deviceRef`.
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
