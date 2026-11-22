# Vendored Fleet inventory CRDs

`crds/fleet.datumapis.com.yaml` is a verbatim copy of Fleet's
`config/components/inventory/crds/fleet.datumapis.com.yaml`. It is used only by
tests and the local dev cluster, never deployed by this repository. Fleet stays
the owner of the schema.

| Field | Value |
| --- | --- |
| Source repository | `datum-cloud/fleet` |
| Source | `feat/manual-inventory-records` with datum-cloud/fleet#106, #107 (hardware catalog), #110 (unidentified-device marker) and #134 (importer role) |
| Branch head when vendored | `8a7cd0533f05b7288eab339c6233dd824e83784b` (`feat/manual-inventory-records`) |
| Last commit touching the file | `5341785b90777d3c64eae1345ea94fedda1e35c8` |
| sha256 | `023ae431bfd9ff566fa511a64b7bdc247c1bd0a0d4ae912e014a07bf21848f45` |

The envtest suite checks the sha256, so re-vendoring must update this table and
`vendoredFleetCRDDigest` in `internal/controller/admission_test.go` together.
To re-vendor, copy the file from the chosen Fleet commit without editing it, then
run the [test commands](../../README.md#development).
