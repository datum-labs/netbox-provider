# Vendored Fleet inventory CRDs

`crds/fleet.datumapis.com.yaml` is a verbatim copy of Fleet's
`config/components/inventory/crds/fleet.datumapis.com.yaml`. It is used only by
tests and the local dev cluster, never deployed by this repository. Fleet stays
the owner of the schema.

| Field | Value |
| --- | --- |
| Source repository | `datum-cloud/fleet` |
| Source | datum-cloud/fleet#110 (merged), adding the unidentified-device marker on top of the #106 schema |
| Branch head when vendored | `f87c86b3f8f6af2a2f839ed61ece82de32a8b88a` (`feat/manual-inventory-records`, the #110 merge) |
| Last commit touching the file | `5341785b90777d3c64eae1345ea94fedda1e35c8` |
| sha256 | `023ae431bfd9ff566fa511a64b7bdc247c1bd0a0d4ae912e014a07bf21848f45` |

The envtest suite checks the sha256, so re-vendoring must update this table and
`vendoredFleetCRDDigest` in `internal/controller/admission_test.go` together.
To re-vendor, copy the file from the chosen Fleet commit without editing it, then
run the [test commands](../../README.md#development).
