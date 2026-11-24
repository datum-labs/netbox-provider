# Vendored Fleet inventory CRDs

`crds/fleet.datumapis.com.yaml` is a verbatim copy of Fleet's
`config/components/inventory/crds/fleet.datumapis.com.yaml`. It is used only by
tests and the local dev cluster, never deployed by this repository. Fleet stays
the owner of the schema.

| Field | Value |
| --- | --- |
| Source repository | `datum-cloud/fleet` |
| Source | `feat/manual-inventory-records` with datum-cloud/fleet#106, #107 (hardware catalog), #110 (unidentified-device marker), #134 (importer role), and the Device lifecycle, template chassis face and DeviceType faceplate layout fields |
| Branch head when vendored | `6398b46ad2ad0d76b6ee2e55e3923186bd8ed04c` (`feat/manual-inventory-records`) |
| Last commit touching the file | `5e67e972c37ad3c28b5bf9365c4e873c5dee016f` |
| sha256 | `6609923b6df7743d75861eb62edffb9a96d47b2ddab2c6a548404f0c7be8d971` |

Fleet's Device CRD enables the status subresource but declares `status` as an
empty object, which prunes every status field. The envtest suite reads this file
and, in memory only, lets `Device.status` keep unknown fields, so its tests can
show that a status another writer filled survives this controller's applies.
The file itself stays verbatim.

The envtest suite checks the sha256, so re-vendoring must update this table and
`vendoredFleetCRDDigest` in `internal/controller/admission_test.go` together.
To re-vendor, copy the file from the chosen Fleet commit without editing it, then
run the [test commands](../../README.md#development).
