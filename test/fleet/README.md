# Vendored Fleet inventory CRDs

`crds/fleet.datumapis.com.yaml` is a verbatim copy of Fleet's
`config/components/inventory/crds/fleet.datumapis.com.yaml`. It is used only by
tests and the local dev cluster, never deployed by this repository. Fleet stays
the owner of the schema.

| Field | Value |
| --- | --- |
| Source repository | `datum-cloud/fleet` |
| Source | datum-cloud/fleet#106 (merged), the target inventory schema |
| Branch head when vendored | `5b60656759033b61674ee202a9ad94f4d49f4a64` (`feat/manual-inventory-records`) |
| Last commit touching the file | `11778ec901812747ad7cb5e915f6c7658836bc04` |
| sha256 | `21c081b63e8efb37d71d19a66616a972413b836ace24427de5d2b947f5ebbe9e` |

The envtest suite checks the sha256, so re-vendoring must update this table and
`vendoredFleetCRDDigest` in `internal/controller/admission_test.go` together.
To re-vendor, copy the file from the chosen Fleet commit without editing it, then
run the [test commands](../../README.md#development).
